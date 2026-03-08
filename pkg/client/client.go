package client

// BitTorrent Client Library
// Implements multiple BitTorrent Enhancement Proposals (BEPs):
//
// BEP 3: The BitTorrent Protocol Specification
// https://www.bittorrent.org/beps/bep_0003.html
//
// BEP 9: Extension for Peers to Send Metadata Files
// https://www.bittorrent.org/beps/bep_0009.html
//
// This package provides a high-level BitTorrent client library:
// - .torrent file loading and parsing
// - Magnet link parsing and metadata resolution
// - Tracker communication (HTTP/UDP)
// - Peer connection and metadata exchange (BEP 9)
// - Info hash resolution from trackers and DHT
// - Concurrent peer querying with rolling concurrency

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
	"github.com/cbluth/bittorrent/pkg/magnet"
	"github.com/cbluth/bittorrent/pkg/protocol"
	"github.com/cbluth/bittorrent/pkg/torrent"
	"github.com/cbluth/bittorrent/pkg/tracker"
)

type (
	// Client is the main BitTorrent client library
	Client struct {
		peerID          dht.Key
		port            uint16
		trackerClient   *tracker.Client
		defaultTrackers []string
		log             *slog.Logger
		mu              sync.RWMutex
	}

	// Config holds configuration for the BitTorrent client
	Config struct {
		PeerID   dht.Key
		Port     uint16
		Timeout  time.Duration
		Trackers []string
		Logger   *slog.Logger // Optional: defaults to slog.Default()
	}

	// TorrentInfo holds information about a torrent
	TorrentInfo struct {
		InfoHash     dht.Key
		Name         string
		TotalLength  int64
		PieceLength  int64
		NumPieces    int
		Trackers     []string
		IsSingleFile bool
		Files        []FileInfo
		meta         *torrent.MetaInfo
	}

	// FileInfo represents a file in the torrent
	FileInfo struct {
		Path   string
		Length int64
	}

	// MagnetInfo holds information about a magnet link
	MagnetInfo struct {
		InfoHash    dht.Key
		DisplayName string
		Trackers    []string
		ExactLength int64
		magnet      *magnet.Magnet
	}

	// TrackerResults holds results from querying multiple trackers
	TrackerResults struct {
		Successful []TrackerResponse
		Failed     []TrackerError
	}

	// TrackerResponse pairs a tracker URL with its response
	TrackerResponse struct {
		URL      string
		Response *tracker.AnnounceResponse
	}

	// TrackerError pairs a tracker URL with its error
	TrackerError struct {
		URL   string
		Error error
	}
)

// NewClient creates a new BitTorrent client with the given configuration
func NewClient(cfg *Config) (*Client, error) {
	if cfg == nil {
		cfg = &Config{}
	}

	// Generate peer ID if not provided
	peerID := cfg.PeerID
	if peerID == [20]byte{} {
		var err error
		peerID, err = protocol.DefaultPeerID()
		if err != nil {
			return nil, fmt.Errorf("failed to generate peer ID: %w", err)
		}
	}

	// Set default port if not provided
	port := cfg.Port
	if port == 0 {
		port = 6881
	}

	// Set default timeout if not provided
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}

	// Set default logger if not provided
	logger := cfg.Logger
	if logger == nil {
		logger = log.Logger()
	}

	return &Client{
		peerID:          peerID,
		port:            port,
		trackerClient:   tracker.NewClient(timeout),
		defaultTrackers: cfg.Trackers,
		log:             logger,
	}, nil
}

// NewDefaultClient creates a client with default configuration
func NewDefaultClient() (*Client, error) {
	return NewClient(nil)
}

// SetTrackers sets the default tracker list
func (c *Client) SetTrackers(trackers []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultTrackers = trackers
}

// AddTrackers adds trackers to the default list
func (c *Client) AddTrackers(trackers []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultTrackers = append(c.defaultTrackers, trackers...)
}

// GetTrackers returns a copy of the default tracker list
func (c *Client) GetTrackers() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	trackers := make([]string, len(c.defaultTrackers))
	copy(trackers, c.defaultTrackers)
	return trackers
}

// PeerID returns the client's peer ID
func (c *Client) PeerID() dht.Key {
	return c.peerID
}

// Port returns the client's port
func (c *Client) Port() uint16 {
	return c.port
}

// Logger returns the client's logger
func (c *Client) Logger() *slog.Logger {
	return c.log
}

// LoadTorrent loads and parses a torrent file from a reader
func (c *Client) LoadTorrent(r io.Reader) (*TorrentInfo, error) {
	meta, err := torrent.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("failed to parse torrent: %w", err)
	}

	infoHash, _ := dht.KeyFromBytes(meta.InfoHash[:])

	return &TorrentInfo{
		InfoHash:     infoHash,
		Name:         meta.Info.Name,
		TotalLength:  meta.Info.TotalLength(),
		PieceLength:  meta.Info.PieceLength,
		NumPieces:    len(meta.Info.Pieces) / 20,
		Trackers:     meta.GetTrackers(),
		IsSingleFile: meta.Info.IsSingleFile(),
		Files:        convertFiles(meta.Info.Files),
		meta:         meta,
	}, nil
}

// ParseMagnet parses a magnet link
func (c *Client) ParseMagnet(magnetURI string) (*MagnetInfo, error) {
	mag, err := magnet.Parse(magnetURI)
	if err != nil {
		return nil, fmt.Errorf("failed to parse magnet: %w", err)
	}

	infoHash, _ := dht.KeyFromBytes(mag.InfoHash[:])

	return &MagnetInfo{
		InfoHash:    infoHash,
		DisplayName: mag.DisplayName,
		Trackers:    mag.Trackers,
		ExactLength: mag.ExactLength,
		magnet:      mag,
	}, nil
}

// ResolveMetadata resolves metadata for a magnet link or bare info hash
// This queries trackers to find peers and attempts metadata exchange
func (c *Client) ResolveMetadata(ctx context.Context, infoHashHex string) (*TorrentInfo, error) {
	// Parse info hash
	infoHash, err := dht.KeyFromHex(infoHashHex)
	if err != nil {
		return nil, fmt.Errorf("invalid info hash: %w", err)
	}

	return c.ResolveMetadataFromHash(ctx, infoHash)
}

// ResolveMetadataFromPeers resolves metadata from a list of peer addresses
// This is useful when you have peers from DHT or other sources (not trackers)
func (c *Client) ResolveMetadataFromPeers(ctx context.Context, infoHash dht.Key, peerAddrs []net.Addr) (*TorrentInfo, error) {
	if len(peerAddrs) == 0 {
		return nil, fmt.Errorf("no peers provided")
	}

	// Convert net.Addr to tracker.Peer format
	peers := make([]tracker.Peer, 0, len(peerAddrs))
	for _, addr := range peerAddrs {
		switch a := addr.(type) {
		case *net.TCPAddr:
			peers = append(peers, tracker.Peer{IP: a.IP, Port: uint16(a.Port)})
		case *net.UDPAddr:
			peers = append(peers, tracker.Peer{IP: a.IP, Port: uint16(a.Port)})
		default:
			// Try to parse string representation
			host, portStr, err := net.SplitHostPort(addr.String())
			if err != nil {
				continue
			}
			ip := net.ParseIP(host)
			if ip == nil {
				continue
			}
			portNum, err := strconv.Atoi(portStr)
			if err != nil || portNum <= 0 || portNum > 65535 {
				continue
			}
			peers = append(peers, tracker.Peer{IP: ip, Port: uint16(portNum)})
		}
	}

	if len(peers) == 0 {
		return nil, fmt.Errorf("no valid peers after conversion")
	}

	c.log.Info("resolving metadata from provided peers", "total", len(peerAddrs), "valid", len(peers))
	return c.fetchMetadataFromPeers(ctx, infoHash, peers)
}

// ResolveMetadataFromHash resolves metadata from a DHT Key (info hash)
func (c *Client) ResolveMetadataFromHash(ctx context.Context, infoHash dht.Key) (*TorrentInfo, error) {
	c.mu.RLock()
	trackers := make([]string, len(c.defaultTrackers))
	copy(trackers, c.defaultTrackers)
	c.mu.RUnlock()

	if len(trackers) == 0 {
		return nil, fmt.Errorf("no trackers configured")
	}

	c.log.Info("querying trackers for peers", "count", len(trackers))

	// Query trackers to get peers
	peers, err := c.queryTrackersForHash(ctx, infoHash, trackers)
	if err != nil {
		return nil, fmt.Errorf("failed to query trackers: %w", err)
	}

	if len(peers) == 0 {
		return nil, fmt.Errorf("no peers found for info hash")
	}

	c.log.Info("tracker queries complete")

	// Try metadata exchange with peers (BEP 9)
	return c.fetchMetadataFromPeers(ctx, infoHash, peers)
}

// QueryTrackers queries all configured trackers for a torrent
func (c *Client) QueryTrackers(ctx context.Context, info interface{}) (*TrackerResults, error) {
	var infoHash dht.Key
	var trackers []string

	switch v := info.(type) {
	case *TorrentInfo:
		infoHash = v.InfoHash
		trackers = v.Trackers
	case *MagnetInfo:
		infoHash = v.InfoHash
		trackers = v.Trackers
	default:
		return nil, fmt.Errorf("unsupported info type")
	}

	// Add default trackers
	c.mu.RLock()
	trackers = append(trackers, c.defaultTrackers...)
	c.mu.RUnlock()

	return c.queryTrackers(ctx, infoHash, trackers, 0)
}

// queryTrackersForHash is an internal method to query trackers for a bare hash
func (c *Client) queryTrackersForHash(ctx context.Context, infoHash dht.Key, trackers []string) ([]tracker.Peer, error) {
	results, err := c.queryTrackers(ctx, infoHash, trackers, 0)
	if err != nil {
		return nil, err
	}
	return results.AllPeers(), nil
}

// queryTrackers performs the actual tracker queries
func (c *Client) queryTrackers(ctx context.Context, infoHash dht.Key, trackers []string, left int64) (*TrackerResults, error) {
	if len(trackers) == 0 {
		return nil, fmt.Errorf("no trackers available")
	}

	// Deduplicate trackers
	seen := make(map[string]bool)
	uniqueTrackers := make([]string, 0, len(trackers))
	for _, t := range trackers {
		if !seen[t] && t != "" {
			seen[t] = true
			uniqueTrackers = append(uniqueTrackers, t)
		}
	}

	results := &TrackerResults{
		Successful: make([]TrackerResponse, 0),
		Failed:     make([]TrackerError, 0),
	}

	request := &tracker.AnnounceRequest{
		Uploaded:   0,
		Downloaded: 0,
		NumWant:    50,
		Left:       left,
		Compact:    true,
		Port:       c.port,
		PeerID:     c.peerID,
		InfoHash:   infoHash,
		Event:      tracker.EventStarted,
	}

	// Query all trackers concurrently with context support
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, trackerURL := range uniqueTrackers {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()

			// Check context cancellation
			select {
			case <-ctx.Done():
				return
			default:
			}

			resp, err := c.trackerClient.Announce(url, request)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				results.Failed = append(results.Failed, TrackerError{
					URL:   url,
					Error: err,
				})
				return
			}

			results.Successful = append(results.Successful, TrackerResponse{
				URL:      url,
				Response: resp,
			})
		}(trackerURL)
	}

	wg.Wait()

	return results, nil
}

// AllPeers returns all unique peers from successful tracker responses
func (r *TrackerResults) AllPeers() []tracker.Peer {
	seen := make(map[string]bool)
	allPeers := make([]tracker.Peer, 0)

	for _, tr := range r.Successful {
		for _, peer := range tr.Response.Peers {
			key := fmt.Sprintf("%s:%d", peer.IP.String(), peer.Port)
			if !seen[key] {
				seen[key] = true
				allPeers = append(allPeers, peer)
			}
		}
	}

	return allPeers
}

// TotalSeeders returns the total number of seeders across all trackers
func (r *TrackerResults) TotalSeeders() int64 {
	var total int64
	for _, tr := range r.Successful {
		total += tr.Response.Complete
	}
	return total
}

// TotalLeechers returns the total number of leechers across all trackers
func (r *TrackerResults) TotalLeechers() int64 {
	var total int64
	for _, tr := range r.Successful {
		total += tr.Response.Incomplete
	}
	return total
}

// convertFiles converts internal file info to public format
func convertFiles(files []torrent.FileInfo) []FileInfo {
	if len(files) == 0 {
		return nil
	}

	result := make([]FileInfo, len(files))
	for i, f := range files {
		path := ""
		for j, part := range f.Path {
			if j > 0 {
				path += "/"
			}
			path += part
		}
		result[i] = FileInfo{
			Path:   path,
			Length: f.Length,
		}
	}
	return result
}

// GetMetaInfo returns the MetaInfo for this torrent, or nil if not available
func (t *TorrentInfo) GetMetaInfo() *torrent.MetaInfo {
	return t.meta
}

type peerResult struct {
	info     *torrent.MetaInfo
	err      error
	peerAddr string
	peerIdx  int
}

// fetchMetadataFromPeers tries to fetch metadata from a list of peers using BEP 9
func (c *Client) fetchMetadataFromPeers(ctx context.Context, infoHash dht.Key, peers []tracker.Peer) (*TorrentInfo, error) {
	// Filter out bogus peers (localhost, private IPs, etc)
	validPeers := filterValidPeers(peers)
	c.log.Info("found peers", "total", len(peers), "valid", len(validPeers))

	if len(validPeers) == 0 {
		return nil, fmt.Errorf("no valid peers found")
	}

	// Try all valid peers with rolling concurrency
	maxPeers := len(validPeers)
	concurrency := 30 // Increased from 20

	c.log.Info("trying peers", "count", maxPeers, "concurrency", concurrency)

	// Channel to receive results
	results := make(chan peerResult, concurrency)

	// Context for overall operation
	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var activePeers int
	peerIdx := 0
	failCount := 0

	// Launch initial batch
	for i := 0; i < concurrency && peerIdx < maxPeers; i++ {
		go c.tryPeer(fetchCtx, validPeers[peerIdx], peerIdx, maxPeers, infoHash, results)
		activePeers++
		peerIdx++
	}

	// Process results and launch more peers as needed
	for activePeers > 0 {
		select {
		case res := <-results:
			activePeers--

			if res.err == nil {
				c.log.Info("metadata received", "peer", res.peerAddr)
				// Success! Convert to TorrentInfo
				cancel() // Stop other peers
				return &TorrentInfo{
					InfoHash:     res.info.InfoHash,
					Name:         res.info.Info.Name,
					TotalLength:  res.info.Info.TotalLength(),
					PieceLength:  res.info.Info.PieceLength,
					NumPieces:    len(res.info.Info.Pieces) / 20,
					Trackers:     res.info.GetTrackers(),
					IsSingleFile: res.info.Info.IsSingleFile(),
					Files:        convertFiles(res.info.Info.Files),
					meta:         res.info,
				}, nil
			}

			failCount++

			// Launch another peer if we have more to try
			if peerIdx < maxPeers {
				go c.tryPeer(fetchCtx, validPeers[peerIdx], peerIdx, maxPeers, infoHash, results)
				activePeers++
				peerIdx++
			}

		case <-fetchCtx.Done():
			c.log.Info("context cancelled", "peers_tried", peerIdx)
			return nil, fmt.Errorf("metadata fetch cancelled after trying %d peers", peerIdx)
		}
	}

	c.log.Info("all peers failed", "count", failCount)
	return nil, fmt.Errorf("all %d peers failed", failCount)
}

func (c *Client) tryPeer(ctx context.Context, peer tracker.Peer, idx int, total int, infoHash dht.Key, results chan<- peerResult) {
	// Create peer address
	peerAddr := &tracker.PeerAddr{IP: peer.IP, Port: peer.Port}
	peerAddrStr := peerAddr.String()

	c.log.Debug("Connecting to...", "peer", peerAddrStr, "round", idx+1)

	// Try to fetch metadata from this peer with reduced timeout (5s connection, 10s total)
	metaInfo, err := protocol.FetchMetadata(peerAddr, infoHash, c.peerID, 10*time.Second, c.log)

	if err != nil {
		c.log.Debug("failed to connect", "peer", peerAddrStr, "round", idx+1, "error", err.Error())
	} else {
		c.log.Info("Connection success", "peer", peerAddrStr, "round", idx+1)
		// log.Printf("[Peer %d/%d] %s - SUCCESS! Got metadata", idx+1, total, peerAddrStr)
	}

	select {
	case results <- peerResult{info: metaInfo, err: err, peerAddr: peerAddrStr, peerIdx: idx}:
	case <-ctx.Done():
	}
}

// filterValidPeers removes bogus peers (localhost, private IPs, invalid addresses)
func filterValidPeers(peers []tracker.Peer) []tracker.Peer {
	valid := make([]tracker.Peer, 0, len(peers))

	for _, peer := range peers {
		// Skip nil or empty IPs
		if len(peer.IP) == 0 {
			continue
		}

		// Skip localhost, private, unspecified, multicast
		if peer.IP.IsLoopback() || peer.IP.IsPrivate() || peer.IP.IsUnspecified() || peer.IP.IsMulticast() {
			continue
		}

		// Skip broadcast (255.255.255.255)
		if peer.IP.Equal(net.IPv4(255, 255, 255, 255)) {
			continue
		}

		// Skip IPs starting with 0 (e.g., 0.x.x.x) — not routable
		if ip4 := peer.IP.To4(); ip4 != nil && ip4[0] == 0 {
			continue
		}

		// Skip invalid ports
		if peer.Port == 0 || peer.Port == 1 {
			continue
		}

		valid = append(valid, peer)
	}

	return valid
}
