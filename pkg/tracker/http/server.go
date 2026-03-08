package http

// BEP 3: HTTP Tracker Protocol
// https://www.bittorrent.org/beps/bep_0003.html
//

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/cbluth/bittorrent/pkg/bencode"
	"github.com/cbluth/bittorrent/pkg/log"
	"github.com/cbluth/bittorrent/pkg/tracker/ws"
)

// ServerConfig holds tracker server configuration
type ServerConfig struct {
	Port             int           `json:"port"`
	Storage          string        `json:"storage"` // memory, sqlite, postgres
	DatabasePath     string        `json:"databasePath"`
	AnnounceInterval time.Duration `json:"announceInterval"`
	MaxPeers         int           `json:"maxPeers"`
	EnableIPv6       bool          `json:"enableIPv6"`
	ExternalURL      string        `json:"externalUrl"` // For reverse proxy setups
	SSLCert          string        `json:"sslCert"`
	SSLKey           string        `json:"sslKey"`
	AllowedIPs       []string      `json:"allowedIPs"` // Whitelist for security
	EnableStats      bool          `json:"enableStats"`
}

// Server represents a BitTorrent tracker server
type Server struct {
	config     *ServerConfig
	stats      *ServerStats
	mu         sync.RWMutex
	shutdown   chan struct{}
	httpServer *http.Server
	wsTracker  *ws.Tracker // WebSocket tracker
}

// ServerStats tracks tracker statistics
type ServerStats struct {
	TotalAnnounces int64     `json:"totalAnnounces"`
	TotalScrapes   int64     `json:"totalScrapes"`
	ActiveTorrents int       `json:"activeTorrents"`
	TotalPeers     int64     `json:"totalPeers"`
	StartTime      time.Time `json:"startTime"`
	mu             sync.RWMutex
}

// TorrentInfo represents a tracked torrent (in-memory storage)
type TorrentInfo struct {
	InfoHash    string        `json:"infoHash"`
	Name        string        `json:"name"`
	Completed   int64         `json:"completed"`
	Downloaded  int64         `json:"downloaded"`
	Peers       []TrackerPeer `json:"peers"`
	LastUpdated time.Time     `json:"lastUpdated"`
	CreatedAt   time.Time     `json:"createdAt"`
	mu          sync.RWMutex
}

// TrackerPeer represents a BitTorrent peer (different from client Peer)
type TrackerPeer struct {
	PeerID     string    `json:"peerId"`
	IP         string    `json:"ip"`
	Port       int       `json:"port"`
	Uploaded   int64     `json:"uploaded"`
	Downloaded int64     `json:"downloaded"`
	Left       int64     `json:"left"`
	Event      string    `json:"event"` // started, stopped, completed
	LastSeen   time.Time `json:"lastSeen"`
}

// AnnounceRequest represents a tracker announce request
type AnnounceRequest struct {
	InfoHash   string `json:"infoHash"`
	PeerID     string `json:"peerId"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	Uploaded   int64  `json:"uploaded"`
	Downloaded int64  `json:"downloaded"`
	Left       int64  `json:"left"`
	Event      string `json:"event"`
	NumWant    int    `json:"numWant"` // Number of peers wanted
	Compact    bool   `json:"compact"`
	NoPeerID   bool   `json:"noPeerId"`
}

// AnnounceResponse represents a tracker announce response
type AnnounceResponse struct {
	FailureReason string        `json:"failureReason"`
	Interval      int           `json:"interval"`
	MinInterval   int           `json:"minInterval"`
	TrackerID     string        `json:"trackerId"`
	Complete      int           `json:"complete"`
	Incomplete    int           `json:"incomplete"`
	Peers         []TrackerPeer `json:"peers"`
}

// In-memory storage for torrents and peers
var (
	torrents  = make(map[string]*TorrentInfo)
	storageMu sync.RWMutex
)

// NewServer creates a new tracker server
func NewServer(config *ServerConfig) (*Server, error) {
	if config == nil {
		config = &ServerConfig{
			Port:             6969,
			Storage:          "memory",
			AnnounceInterval: 30 * time.Minute,
			MaxPeers:         200,
			EnableIPv6:       false,
			EnableStats:      true,
		}
	}

	// Create WebSocket tracker with shorter interval
	wsTracker := ws.NewTracker(&ws.Config{
		Interval: config.AnnounceInterval / 5, // WebSocket interval is shorter
	})

	server := &Server{
		config:    config,
		stats:     &ServerStats{StartTime: time.Now()},
		shutdown:  make(chan struct{}),
		wsTracker: wsTracker,
	}

	return server, nil
}

// Start starts the tracker server
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Register HTTP endpoints
	mux.HandleFunc("/announce", s.handleAnnounceOrWebSocket)
	mux.HandleFunc("/scrape", s.handleHTTPScrape)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/", s.handleIndex)

	// Configure server
	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.config.Port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Start server
	go func() {
		log.Info("starting HTTP tracker server", "sub", "tracker", "port", s.config.Port)
		if s.config.SSLCert != "" && s.config.SSLKey != "" {
			log.Info("starting HTTPS server", "sub", "tracker")
			if err := s.httpServer.ListenAndServeTLS(s.config.SSLCert, s.config.SSLKey); err != nil && err != http.ErrServerClosed {
				log.Error("HTTPS server error", "sub", "tracker", "err", err)
			}
		} else {
			log.Info("starting HTTP server", "sub", "tracker")
			if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("HTTP server error", "sub", "tracker", "err", err)
			}
		}
	}()

	// Wait for shutdown signal
	s.waitForShutdown()

	return nil
}

// handleAnnounceOrWebSocket routes to either HTTP or WebSocket handler
func (s *Server) handleAnnounceOrWebSocket(w http.ResponseWriter, r *http.Request) {
	// Check if this is a WebSocket upgrade request
	if r.Header.Get("Upgrade") == "websocket" {
		// Use the WebSocket tracker handler
		s.wsTracker.Handler()(w, r)
		return
	}

	// Otherwise handle as HTTP announce
	s.handleHTTPAnnounce(w, r)
}

// handleHTTPAnnounce handles HTTP announce requests
func (s *Server) handleHTTPAnnounce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse announce request
	req, err := s.parseAnnounceRequest(r.URL.Query())
	if err != nil {
		s.writeErrorResponse(w, "Failed to parse request", http.StatusBadRequest)
		return
	}

	// Check IP whitelist
	if len(s.config.AllowedIPs) > 0 && !s.isIPAllowed(req.IP) {
		s.writeErrorResponse(w, "IP not allowed", http.StatusForbidden)
		return
	}

	// Process announce
	response := s.processAnnounce(req)
	s.writeAnnounceResponse(w, response)

	// Update stats
	s.stats.mu.Lock()
	s.stats.TotalAnnounces++
	s.stats.mu.Unlock()

	log.Debug("HTTP announce", "sub", "tracker", "ip", req.IP, "port", req.Port, "hash", req.InfoHash[:8])
}

// processAnnounce processes an announce request and returns response
func (s *Server) processAnnounce(req *AnnounceRequest) *AnnounceResponse {
	// Get or create torrent info
	torrent := s.getOrCreateTorrent(req.InfoHash)

	// Update peer information
	peer := TrackerPeer{
		PeerID:     req.PeerID,
		IP:         req.IP,
		Port:       req.Port,
		Uploaded:   req.Uploaded,
		Downloaded: req.Downloaded,
		Left:       req.Left,
		Event:      req.Event,
		LastSeen:   time.Now(),
	}

	// Handle different events
	switch req.Event {
	case "started":
		// New peer starting download
	case "stopped":
		// Peer stopped downloading - remove from active peers
		s.removePeer(torrent, peer.PeerID)
	case "completed":
		// Peer completed download
		torrent.Completed++
	case "":
		// Regular update - just update peer info
	}

	// Add or update peer
	s.addOrUpdatePeer(torrent, peer)

	// Prepare response
	response := &AnnounceResponse{
		Interval:    int(s.config.AnnounceInterval.Seconds()),
		MinInterval: int(s.config.AnnounceInterval.Seconds()),
		Complete:    int(torrent.Completed),
		Incomplete:  len(torrent.Peers),
	}

	// Return peers based on request format
	if req.Compact {
		// For now, don't implement compact format
		response.Peers = []TrackerPeer{}
	} else {
		response.Peers = s.getRandomPeers(torrent.Peers, req.NumWant)
	}

	return response
}

// getOrCreateTorrent gets or creates a torrent info
func (s *Server) getOrCreateTorrent(infoHash string) *TorrentInfo {
	storageMu.RLock()
	torrent, exists := torrents[infoHash]
	storageMu.RUnlock()

	if !exists {
		torrent = &TorrentInfo{
			InfoHash:    infoHash,
			Peers:       []TrackerPeer{},
			CreatedAt:   time.Now(),
			LastUpdated: time.Now(),
		}
		storageMu.Lock()
		torrents[infoHash] = torrent
		storageMu.Unlock()
	}

	return torrent
}

// addOrUpdatePeer adds or updates a peer in the torrent
func (s *Server) addOrUpdatePeer(torrent *TorrentInfo, peer TrackerPeer) {
	torrent.mu.Lock()
	defer torrent.mu.Unlock()

	for i, p := range torrent.Peers {
		if p.PeerID == peer.PeerID {
			// Update existing peer
			torrent.Peers[i] = peer
			return
		}
	}
	// Add new peer
	torrent.Peers = append(torrent.Peers, peer)

	// Limit number of peers
	if len(torrent.Peers) > s.config.MaxPeers {
		torrent.Peers = torrent.Peers[len(torrent.Peers)-s.config.MaxPeers:]
	}
}

// removePeer removes a peer from the torrent
func (s *Server) removePeer(torrent *TorrentInfo, peerID string) {
	torrent.mu.Lock()
	defer torrent.mu.Unlock()

	for i, p := range torrent.Peers {
		if p.PeerID == peerID {
			torrent.Peers = append(torrent.Peers[:i], torrent.Peers[i+1:]...)
			return
		}
	}
}

// getRandomPeers returns random peers from the peer list
func (s *Server) getRandomPeers(peers []TrackerPeer, numWant int) []TrackerPeer {
	if len(peers) == 0 {
		return []TrackerPeer{}
	}

	if numWant <= 0 || numWant > len(peers) {
		numWant = len(peers)
	}

	// Simple random selection (could be improved with better algorithms)
	result := make([]TrackerPeer, numWant)
	for i := 0; i < numWant; i++ {
		result[i] = peers[i]
	}

	return result
}

// parseAnnounceRequest parses HTTP announce request parameters
func (s *Server) parseAnnounceRequest(values url.Values) (*AnnounceRequest, error) {
	req := &AnnounceRequest{}

	// Parse info_hash
	if infoHash := values.Get("info_hash"); infoHash != "" {
		if len(infoHash) != 20 {
			return nil, fmt.Errorf("invalid info_hash length")
		}
		req.InfoHash = hex.EncodeToString([]byte(infoHash))
	}

	// Parse other parameters
	req.PeerID = values.Get("peer_id")
	req.IP = values.Get("ip")
	if req.IP == "" {
		req.IP = values.Get("ipaddress") // Alternative parameter name
	}
	if portStr := values.Get("port"); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid port: %w", err)
		}
		req.Port = port
	}
	if s := values.Get("uploaded"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err != nil {
			return nil, fmt.Errorf("invalid uploaded: %w", err)
		} else {
			req.Uploaded = v
		}
	}
	if s := values.Get("downloaded"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err != nil {
			return nil, fmt.Errorf("invalid downloaded: %w", err)
		} else {
			req.Downloaded = v
		}
	}
	if s := values.Get("left"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err != nil {
			return nil, fmt.Errorf("invalid left: %w", err)
		} else {
			req.Left = v
		}
	}
	if s := values.Get("numwant"); s != "" {
		if v, err := strconv.Atoi(s); err != nil {
			return nil, fmt.Errorf("invalid numwant: %w", err)
		} else {
			req.NumWant = v
		}
	}
	req.Event = values.Get("event")
	req.Compact = values.Get("compact") == "1"
	req.NoPeerID = values.Get("no_peer_id") == "1"

	return req, nil
}

// writeAnnounceResponse writes HTTP announce response
func (s *Server) writeAnnounceResponse(w http.ResponseWriter, response *AnnounceResponse) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-cache")

	if response.FailureReason != "" {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "d14:failure reason%d:%s", len(response.FailureReason), response.FailureReason)
	} else {
		w.WriteHeader(http.StatusOK)
		// Bencode response
		responseData := map[string]interface{}{
			"interval":     response.Interval,
			"min interval": response.MinInterval,
			"complete":     response.Complete,
			"incomplete":   response.Incomplete,
		}

		if response.Peers != nil {
			responseData["peers"] = response.Peers
		}

		encoded, err := bencode.EncodeBytes(responseData)
		if err != nil {
			http.Error(w, "Failed to encode response", http.StatusInternalServerError)
			return
		}

		w.Write(encoded)
	}
}

// writeErrorResponse writes an error response
func (s *Server) writeErrorResponse(w http.ResponseWriter, reason string, code int) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(code)
	fmt.Fprintf(w, "d14:failure reason%d:%s", len(reason), reason)
}

// handleHTTPScrape handles HTTP scrape requests
func (s *Server) handleHTTPScrape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	infoHashes := r.URL.Query()["info_hash"]
	if len(infoHashes) == 0 {
		s.writeErrorResponse(w, "No info_hash parameter", http.StatusBadRequest)
		return
	}

	// Process scrape
	scrapeResponse := make(map[string]interface{})
	for _, infoHash := range infoHashes {
		if len(infoHash) != 20 {
			continue
		}

		torrent := s.getOrCreateTorrent(hex.EncodeToString([]byte(infoHash)))
		scrapeResponse[hex.EncodeToString([]byte(infoHash))] = map[string]interface{}{
			"complete":   torrent.Completed,
			"incomplete": len(torrent.Peers),
			"downloaded": torrent.Downloaded,
			"name":       torrent.Name,
		}
	}

	// Encode scrape response
	response := map[string]interface{}{
		"files": scrapeResponse,
	}

	encoded, err := bencode.EncodeBytes(response)
	if err != nil {
		http.Error(w, "Failed to encode scrape response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write(encoded)

	log.Debug("scrape request", "sub", "tracker", "count", len(infoHashes))
}

// handleStats displays server statistics
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.stats.mu.RLock()
	totalAnnounces := s.stats.TotalAnnounces
	totalScrapes := s.stats.TotalScrapes
	totalPeers := s.stats.TotalPeers
	startTime := s.stats.StartTime
	s.stats.mu.RUnlock()

	// Count active torrents
	storageMu.RLock()
	activeTorrents := len(torrents)
	storageMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")

	statsData := map[string]interface{}{
		"totalAnnounces": totalAnnounces,
		"totalScrapes":   totalScrapes,
		"activeTorrents": activeTorrents,
		"totalPeers":     totalPeers,
		"uptime":         time.Since(startTime).String(),
		"version":        "bt-tracker-1.0.0",
	}

	encoded, _ := json.Marshal(statsData)
	w.Write(encoded)
}

// handleIndex serves a basic index page
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `
<html>
<head><title>BitTorrent Tracker</title></head>
<body>
<h1>BitTorrent Tracker</h1>
<p>Tracker is running on port %d</p>
<ul>
<li><a href="/announce">/announce</a> - Announce endpoint</li>
<li><a href="/scrape">/scrape</a> - Scrape endpoint</li>
<li><a href="/stats">/stats</a> - Statistics</li>
</ul>
</body>
</html>`, s.config.Port)
}

// isIPAllowed checks if an IP is in the whitelist
func (s *Server) isIPAllowed(ip string) bool {
	if len(s.config.AllowedIPs) == 0 {
		return true
	}

	for _, allowedIP := range s.config.AllowedIPs {
		if ip == allowedIP {
			return true
		}
	}

	return false
}

// waitForShutdown waits for shutdown signals
func (s *Server) waitForShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigChan:
		log.Info("shutting down tracker server", "sub", "tracker")
	case <-s.shutdown:
	}

	// Graceful shutdown
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.httpServer.Shutdown(ctx)
		cancel()
	}

	log.Info("tracker server stopped", "sub", "tracker")
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown() {
	close(s.shutdown)
}
