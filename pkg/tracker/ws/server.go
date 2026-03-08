package ws

// WebSocket Tracker — WebTorrent-compatible signaling relay (BEP 55 / WebTorrent spec).
//
// Design notes:
//   - nhooyr.io/websocket serializes writes internally; no per-peer write mutex needed.
//   - Each peer gets its own context; cancel() evicts the peer and closes the connection.
//   - distributeOffers/routeAnswer snapshot peer pointers under the swarm read lock,
//     then write outside the lock to avoid blocking on slow network I/O.
//   - A background goroutine evicts stale peers and removes empty swarms.

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	maxOffersPerAnnounce = 10 // cap on offers forwarded per announce
	staleThreshold       = 15 * time.Minute
	evictInterval        = 30 * time.Second
)

// Tracker is a WebSocket BitTorrent tracker (WebTorrent signaling relay).
type Tracker struct {
	swarms   map[string]*Swarm
	mu       sync.RWMutex
	interval time.Duration
	stop     chan struct{}
	log      *slog.Logger
}

// Swarm holds all peers for one info hash.
type Swarm struct {
	InfoHash  string
	Peers     map[string]*Peer
	Seeders   int
	Leechers  int
	Completed int64
	mu        sync.RWMutex
}

// Peer is a connected WebSocket client.
type Peer struct {
	PeerID     string
	conn       *websocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	InfoHashes []string
	IP         string
	Left       int64
	Uploaded   int64
	Downloaded int64
	LastSeen   time.Time
	mu         sync.Mutex
}

// writeJSON sends v as JSON. nhooyr.io/websocket serializes writes with an
// internal mutex, so concurrent calls from different goroutines are safe.
func (p *Peer) writeJSON(v any) error {
	return wsjson.Write(p.ctx, p.conn, v)
}

// Config holds WebSocket tracker configuration.
type Config struct {
	Interval time.Duration
	// Logger is the structured logger. If nil, all output is discarded.
	Logger *slog.Logger
}

// NewTracker creates a Tracker and starts the background eviction goroutine.
func NewTracker(config *Config) *Tracker {
	if config == nil {
		config = &Config{Interval: 2 * time.Minute}
	}
	lg := config.Logger
	if lg == nil {
		lg = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	t := &Tracker{
		swarms:   make(map[string]*Swarm),
		interval: config.Interval,
		stop:     make(chan struct{}),
		log:      lg,
	}
	go t.runEviction()
	return t
}

// Shutdown stops the background eviction goroutine.
func (t *Tracker) Shutdown() {
	close(t.stop)
}

// Handler returns an http.HandlerFunc that upgrades HTTP to WebSocket.
func (t *Tracker) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.handleConnection(w, r)
	}
}

func (t *Tracker) handleConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // WebTorrent: allow all origins
	})
	if err != nil {
		t.log.Warn("websocket accept failed", "err", err)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = fwd
	}

	peer := &Peer{
		conn:       conn,
		ctx:        ctx,
		cancel:     cancel,
		InfoHashes: make([]string, 0),
		IP:         ip,
		LastSeen:   time.Now(),
	}
	t.log.Debug("websocket connection", "ip", ip)
	t.handleMessages(peer)
}

func (t *Tracker) handleMessages(peer *Peer) {
	defer func() {
		t.cleanupPeer(peer)
		peer.conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		var req AnnounceRequest
		if err := wsjson.Read(peer.ctx, peer.conn, &req); err != nil {
			if peer.ctx.Err() == nil {
				t.log.Warn("websocket read error", "ip", peer.IP, "err", err)
			}
			return
		}
		peer.mu.Lock()
		peer.LastSeen = time.Now()
		peer.mu.Unlock()

		switch req.Action {
		case "announce":
			t.handleAnnounce(peer, &req)
		case "scrape":
			t.handleScrape(peer, &req)
		default:
			_ = peer.writeJSON(ErrorResponse{FailureReason: "unknown action: " + req.Action})
		}
	}
}

func (t *Tracker) handleAnnounce(peer *Peer, req *AnnounceRequest) {
	if len(req.InfoHash) != 20 {
		_ = peer.writeJSON(ErrorResponse{FailureReason: "invalid info_hash"})
		return
	}
	if len(req.PeerID) != 20 {
		_ = peer.writeJSON(ErrorResponse{FailureReason: "invalid peer_id"})
		return
	}

	infoHashHex := hex.EncodeToString(req.InfoHash)
	peerIDHex := hex.EncodeToString(req.PeerID)

	peer.mu.Lock()
	if peer.PeerID == "" {
		peer.PeerID = peerIDHex
	}
	peer.mu.Unlock()

	swarm := t.getOrCreateSwarm(infoHashHex)

	if req.Event == "stopped" {
		t.removePeer(swarm, peer)
		return
	}

	// Record this info hash on the peer for cleanup on disconnect.
	peer.mu.Lock()
	hasIH := false
	for _, ih := range peer.InfoHashes {
		if ih == infoHashHex {
			hasIH = true
			break
		}
	}
	if !hasIH {
		peer.InfoHashes = append(peer.InfoHashes, infoHashHex)
	}
	peer.mu.Unlock()

	t.addPeer(swarm, peer, req)

	// Send tracker response only for non-answer messages.
	if req.Answer == nil {
		swarm.mu.RLock()
		resp := AnnounceResponse{
			Action:     "announce",
			Interval:   int(t.interval.Seconds()),
			InfoHash:   req.InfoHash,
			Complete:   swarm.Seeders,
			Incomplete: swarm.Leechers,
		}
		swarm.mu.RUnlock()
		if err := peer.writeJSON(resp); err != nil {
			t.log.Warn("announce response failed", "peer", shortID(peerIDHex), "err", err)
			return
		}
	}

	if len(req.Offers) > 0 {
		t.distributeOffers(peer, req, swarm)
	}
	if req.Answer != nil {
		t.routeAnswer(peer, req, swarm)
	}
}

func (t *Tracker) distributeOffers(from *Peer, req *AnnounceRequest, swarm *Swarm) {
	n := len(req.Offers)
	if n > maxOffersPerAnnounce {
		n = maxOffersPerAnnounce
	}

	// Snapshot target peers under read lock — release before network I/O.
	swarm.mu.RLock()
	targets := make([]*Peer, 0, n)
	for peerID, p := range swarm.Peers {
		if len(targets) >= n {
			break
		}
		if peerID == from.PeerID {
			continue
		}
		targets = append(targets, p)
	}
	swarm.mu.RUnlock()

	for i, target := range targets {
		msg := OfferMessage{
			Action:   "announce",
			Offer:    req.Offers[i].Offer,
			OfferID:  req.Offers[i].OfferID,
			PeerID:   req.PeerID,
			InfoHash: req.InfoHash,
		}
		if err := target.writeJSON(msg); err != nil {
			t.log.Warn("forward offer failed", "peer", shortID(target.PeerID), "err", err)
		}
	}
}

func (t *Tracker) routeAnswer(from *Peer, req *AnnounceRequest, swarm *Swarm) {
	if len(req.ToPeerID) != 20 {
		_ = from.writeJSON(ErrorResponse{FailureReason: "invalid to_peer_id"})
		return
	}
	toPeerIDHex := hex.EncodeToString(req.ToPeerID)

	// Snapshot target pointer before releasing lock.
	swarm.mu.RLock()
	target, ok := swarm.Peers[toPeerIDHex]
	swarm.mu.RUnlock()

	if !ok {
		_ = from.writeJSON(ErrorResponse{FailureReason: "peer not found"})
		return
	}

	msg := AnswerMessage{
		Action:   "announce",
		Answer:   *req.Answer,
		OfferID:  req.OfferID,
		PeerID:   req.PeerID,
		InfoHash: req.InfoHash,
	}
	if err := target.writeJSON(msg); err != nil {
		t.log.Warn("route answer failed", "peer", shortID(toPeerIDHex), "err", err)
	}
}

func (t *Tracker) handleScrape(peer *Peer, req *AnnounceRequest) {
	resp := ScrapeResponse{
		Action: "scrape",
		Files:  make(map[string]ScrapeStats),
	}

	if len(req.InfoHash) == 20 {
		// Single-torrent scrape.
		ih := hex.EncodeToString(req.InfoHash)
		t.mu.RLock()
		swarm, ok := t.swarms[ih]
		t.mu.RUnlock()
		if ok {
			swarm.mu.RLock()
			resp.Files[ih] = ScrapeStats{
				Complete:   swarm.Seeders,
				Incomplete: swarm.Leechers,
				Downloaded: int(swarm.Completed),
			}
			swarm.mu.RUnlock()
		}
	} else {
		// No specific hash: return all swarms (capped at 100).
		t.mu.RLock()
		count := 0
		for ih, swarm := range t.swarms {
			if count >= 100 {
				break
			}
			swarm.mu.RLock()
			resp.Files[ih] = ScrapeStats{
				Complete:   swarm.Seeders,
				Incomplete: swarm.Leechers,
				Downloaded: int(swarm.Completed),
			}
			swarm.mu.RUnlock()
			count++
		}
		t.mu.RUnlock()
	}

	if err := peer.writeJSON(resp); err != nil {
		t.log.Warn("scrape response failed", "err", err)
	}
}

func (t *Tracker) getOrCreateSwarm(infoHash string) *Swarm {
	t.mu.Lock()
	defer t.mu.Unlock()
	swarm, ok := t.swarms[infoHash]
	if !ok {
		swarm = &Swarm{
			InfoHash: infoHash,
			Peers:    make(map[string]*Peer),
		}
		t.swarms[infoHash] = swarm
	}
	return swarm
}

func (t *Tracker) addPeer(swarm *Swarm, peer *Peer, req *AnnounceRequest) {
	swarm.mu.Lock()
	defer swarm.mu.Unlock()

	if existing, ok := swarm.Peers[peer.PeerID]; ok {
		wasSeeder := existing.Left == 0
		existing.Left = req.Left
		existing.Uploaded = req.Uploaded
		existing.Downloaded = req.Downloaded
		existing.LastSeen = time.Now()
		if !wasSeeder && existing.Left == 0 {
			swarm.Seeders++
			swarm.Leechers--
			swarm.Completed++
		}
	} else {
		peer.Left = req.Left
		peer.Uploaded = req.Uploaded
		peer.Downloaded = req.Downloaded
		swarm.Peers[peer.PeerID] = peer
		if req.Left == 0 {
			swarm.Seeders++
		} else {
			swarm.Leechers++
		}
		if req.Event == "completed" {
			swarm.Completed++
		}
	}
}

func (t *Tracker) removePeer(swarm *Swarm, peer *Peer) {
	swarm.mu.Lock()
	defer swarm.mu.Unlock()
	if p, ok := swarm.Peers[peer.PeerID]; ok {
		if p.Left == 0 {
			swarm.Seeders--
		} else {
			swarm.Leechers--
		}
		delete(swarm.Peers, peer.PeerID)
	}
}

func (t *Tracker) cleanupPeer(peer *Peer) {
	peer.mu.Lock()
	infoHashes := append([]string{}, peer.InfoHashes...)
	peerID := peer.PeerID
	peer.mu.Unlock()

	// Snapshot swarm pointers, then remove outside the tracker lock.
	t.mu.RLock()
	swarms := make([]*Swarm, 0, len(infoHashes))
	for _, ih := range infoHashes {
		if s, ok := t.swarms[ih]; ok {
			swarms = append(swarms, s)
		}
	}
	t.mu.RUnlock()

	for _, s := range swarms {
		t.removePeer(s, peer)
	}

	if peerID != "" {
		t.log.Debug("peer disconnected", "peer", shortID(peerID))
	} else {
		t.log.Debug("peer disconnected", "peer", "unknown")
	}
}

// runEviction periodically evicts stale peers and removes empty swarms.
func (t *Tracker) runEviction() {
	ticker := time.NewTicker(evictInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.evictStale()
		case <-t.stop:
			return
		}
	}
}

func (t *Tracker) evictStale() {
	threshold := time.Now().Add(-staleThreshold)

	t.mu.Lock()
	defer t.mu.Unlock()

	for ih, swarm := range t.swarms {
		swarm.mu.Lock()
		for peerID, peer := range swarm.Peers {
			peer.mu.Lock()
			stale := peer.LastSeen.Before(threshold)
			peer.mu.Unlock()
			if stale {
				if peer.Left == 0 {
					swarm.Seeders--
				} else {
					swarm.Leechers--
				}
				peer.cancel() // signals handleMessages to exit
				delete(swarm.Peers, peerID)
				t.log.Debug("evicted stale peer", "peer", shortID(peerID), "swarm", shortID(ih))
			}
		}
		if len(swarm.Peers) == 0 {
			delete(t.swarms, ih)
			t.log.Debug("removed empty swarm", "swarm", shortID(ih))
		}
		swarm.mu.Unlock()
	}
}

// GetStats returns tracker-wide statistics.
func (t *Tracker) GetStats() map[string]any {
	t.mu.RLock()
	defer t.mu.RUnlock()
	peers, seeders, leechers := 0, 0, 0
	for _, swarm := range t.swarms {
		swarm.mu.RLock()
		peers += len(swarm.Peers)
		seeders += swarm.Seeders
		leechers += swarm.Leechers
		swarm.mu.RUnlock()
	}
	return map[string]any{
		"torrents": len(t.swarms),
		"peers":    peers,
		"seeders":  seeders,
		"leechers": leechers,
	}
}

// shortID returns the first 8 characters of an ID string, safe for empty strings.
func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}
