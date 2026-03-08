package udp

// BEP 15: UDP Tracker Protocol
// https://www.bittorrent.org/beps/bep_0015.html
//
// This file implements a UDP-based BitTorrent tracker according to BEP 15:
// - UDP protocol for 50% traffic reduction over HTTP
// - Connection management with transaction IDs
// - Compact binary format for efficiency
// - IPv4 and IPv6 support
// - Proper announce and scrape operations

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
)

// ServerConfig holds configuration for UDP tracker server
type ServerConfig struct {
	Port             int
	Storage          string
	AnnounceInterval int
	MaxPeers         int
	EnableStats      bool
}

// Server represents a UDP BitTorrent tracker server (BEP 15)
type Server struct {
	config   *ServerConfig
	conn     *net.UDPConn
	stats    *ServerStats
	shutdown chan struct{}
	mu       sync.RWMutex

	// Connection management - maps connection ID to expiry time
	connections map[uint64]time.Time

	// Torrent storage
	torrents   map[string]*TorrentInfo
	muTorrents sync.RWMutex
}

// TorrentInfo stores information about a torrent for UDP tracker
type TorrentInfo struct {
	InfoHash    string
	Seeders     int
	Leechers    int
	Completed   int64
	LastUpdated time.Time
	Peers       []*PeerInfo
	mu          sync.RWMutex
}

// PeerInfo stores information about a peer for UDP tracker
type PeerInfo struct {
	IP       net.IP
	Port     uint16
	Left     uint64 // Bytes left to download (0 = seeder)
	LastSeen time.Time
}

// IsSeeder returns true if the peer has completed the download
func (p *PeerInfo) IsSeeder() bool {
	return p.Left == 0
}

// ServerStats tracks UDP tracker statistics
type ServerStats struct {
	TotalConnections  int64
	ActiveConnections int
	TotalAnnounces    int64
	TotalScrapes      int64
	ActiveTorrents    int
	TotalPeers        int64
	StartTime         time.Time
}

// BEP 15 protocol constants
const (
	ProtocolID     = 0x41727101980
	ConnectAction  = 0x00
	AnnounceAction = 0x01
	ScrapeAction   = 0x02
	ErrorAction    = 0x03

	// Connection ID timeout (2 minutes as per BEP 15)
	ConnectionTimeout = 2 * time.Minute

	// Default announce interval
	DefaultInterval = 1800 // 30 minutes
)

// NewServer creates a new UDP tracker server
func NewServer(config *ServerConfig) *Server {
	return &Server{
		config:      config,
		connections: make(map[uint64]time.Time),
		torrents:    make(map[string]*TorrentInfo),
		stats: &ServerStats{
			StartTime: time.Now(),
		},
		shutdown: make(chan struct{}),
	}
}

// Start starts the UDP tracker server
func (s *Server) Start(ctx context.Context) error {
	// Create UDP listener
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", s.config.Port))
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	s.conn, err = net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on UDP port %d: %w", s.config.Port, err)
	}

	log.Info("UDP tracker server starting", "sub", "tracker", "port", s.config.Port)
	log.Info("protocol: UDP (BEP 15)", "sub", "tracker")
	log.Info("storage backend", "sub", "tracker", "storage", s.config.Storage)

	// Start statistics reporting
	if s.config.EnableStats {
		go s.statsReporter()
	}

	// Handle incoming packets
	go s.packetHandler(ctx)

	// Handle graceful shutdown
	go func() {
		<-s.shutdown
		log.Info("UDP tracker server shutting down", "sub", "tracker")
		s.conn.Close()
	}()

	log.Info("UDP tracker server started", "sub", "tracker", "addr", s.conn.LocalAddr())
	return nil
}

// packetHandler processes incoming UDP packets
func (s *Server) packetHandler(ctx context.Context) {
	buf := make([]byte, 2048)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// Set read deadline
			s.conn.SetReadDeadline(time.Now().Add(1 * time.Second))

			n, addr, err := s.conn.ReadFromUDP(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				log.Warn("error reading UDP packet", "sub", "tracker", "addr", addr, "err", err)
				continue
			}

			// Process packet
			s.processPacket(buf[:n], addr)
		}
	}
}

// processPacket handles incoming UDP tracker packets
func (s *Server) processPacket(data []byte, addr *net.UDPAddr) {
	if len(data) < 16 {
		log.Warn("malformed packet: too short", "sub", "tracker", "addr", addr)
		return
	}

	action := binary.BigEndian.Uint32(data[8:12])
	transactionID := binary.BigEndian.Uint32(data[12:16])

	// Only connect requests have protocol_id at offset 0
	// Announce and scrape have connection_id at offset 0
	if action == ConnectAction {
		if binary.BigEndian.Uint64(data[0:8]) != ProtocolID {
			log.Warn("invalid protocol ID in connect request", "sub", "tracker", "addr", addr)
			s.sendError(addr, transactionID, "Invalid protocol ID")
			return
		}
	}

	switch action {
	case ConnectAction:
		s.handleConnect(data, transactionID, addr)
	case AnnounceAction:
		s.handleAnnounce(data, transactionID, addr)
	case ScrapeAction:
		s.handleScrape(data, transactionID, addr)
	default:
		log.Warn("received unknown action", "sub", "tracker", "action", action, "addr", addr)
		s.sendError(addr, transactionID, "Unknown action")
	}
}

// handleConnect handles connection requests
func (s *Server) handleConnect(data []byte, transactionID uint32, addr *net.UDPAddr) {
	if len(data) < 16 {
		s.sendError(addr, transactionID, "Connect request too short")
		return
	}

	// Generate connection ID (using timestamp + random)
	connID := s.generateConnectionID()

	// Store connection with expiry
	s.mu.Lock()
	s.connections[connID] = time.Now().Add(ConnectionTimeout)
	s.mu.Unlock()

	// Build connect response (16 bytes)
	response := make([]byte, 16)
	binary.BigEndian.PutUint32(response[0:4], ConnectAction) // action
	binary.BigEndian.PutUint32(response[4:8], transactionID) // transaction_id
	binary.BigEndian.PutUint64(response[8:16], connID)       // connection_id

	s.sendResponse(addr, response)

	// Update statistics
	s.mu.Lock()
	s.stats.TotalConnections++
	s.mu.Unlock()

	log.Debug("connect: sent connection ID", "sub", "tracker", "connID", connID, "addr", addr)
}

// handleAnnounce handles announce requests
func (s *Server) handleAnnounce(data []byte, transactionID uint32, addr *net.UDPAddr) {
	if len(data) < 98 {
		s.sendError(addr, transactionID, "Announce request too short")
		return
	}

	// Parse announce request according to BEP 15
	req, err := s.parseAnnounceRequest(data)
	if err != nil {
		log.Warn("invalid announce request", "sub", "tracker", "addr", addr, "err", err)
		s.sendError(addr, transactionID, "Malformed announce request")
		return
	}

	// Validate connection ID
	s.mu.Lock()
	connExpiry, exists := s.connections[req.ConnectionID]
	if !exists || time.Now().After(connExpiry) {
		s.mu.Unlock()
		s.sendError(addr, transactionID, "Invalid or expired connection ID")
		return
	}
	s.mu.Unlock()

	// Get or create torrent info
	infoHashStr := hex.EncodeToString(req.InfoHash[:])
	s.muTorrents.Lock()
	torrent, exists := s.torrents[infoHashStr]
	if !exists {
		torrent = &TorrentInfo{
			InfoHash:    infoHashStr,
			Peers:       make([]*PeerInfo, 0),
			LastUpdated: time.Now(),
		}
		s.torrents[infoHashStr] = torrent
	}
	s.muTorrents.Unlock()

	// Add or update peer
	torrent.mu.Lock()
	defer torrent.mu.Unlock()

	// Find existing peer
	peerIndex := -1
	var existingPeer *PeerInfo
	for i, peer := range torrent.Peers {
		if peer.IP.Equal(addr.IP) && peer.Port == req.Port {
			peerIndex = i
			existingPeer = peer
			break
		}
	}

	// Handle stopped event - remove peer
	if req.Event == 3 {
		if peerIndex >= 0 {
			// Remove peer from list
			torrent.Peers = append(torrent.Peers[:peerIndex], torrent.Peers[peerIndex+1:]...)

			// Update counters based on peer's last known state
			if existingPeer.IsSeeder() {
				if torrent.Seeders > 0 {
					torrent.Seeders--
				}
			} else {
				if torrent.Leechers > 0 {
					torrent.Leechers--
				}
			}

			log.Debug("peer stopped", "sub", "tracker", "ip", addr.IP, "port", req.Port)
		}
		return // Don't send peer list for stopped event
	}

	// Handle existing peer update
	if peerIndex >= 0 {
		wasSeeder := existingPeer.IsSeeder()
		isSeeder := req.Left == 0

		// Update peer state
		existingPeer.Left = req.Left
		existingPeer.LastSeen = time.Now()

		// Handle transition from leecher to seeder
		if !wasSeeder && isSeeder {
			if torrent.Leechers > 0 {
				torrent.Leechers--
			}
			torrent.Seeders++
			torrent.Completed++
			log.Debug("peer completed", "sub", "tracker", "ip", addr.IP, "port", req.Port)
		} else if wasSeeder && !isSeeder {
			// Handle transition from seeder to leecher (rare but possible)
			if torrent.Seeders > 0 {
				torrent.Seeders--
			}
			torrent.Leechers++
		}
	} else {
		// Add new peer
		newPeer := &PeerInfo{
			IP:       addr.IP,
			Port:     req.Port,
			Left:     req.Left,
			LastSeen: time.Now(),
		}
		torrent.Peers = append(torrent.Peers, newPeer)

		// Update counters
		if newPeer.IsSeeder() {
			torrent.Seeders++
		} else {
			torrent.Leechers++
		}

		log.Debug("new peer", "sub", "tracker", "ip", addr.IP, "port", req.Port, "seeder", newPeer.IsSeeder())
	}

	// Log announce
	log.Debug("announce", "sub", "tracker", "hash", infoHashStr[:16], "addr", addr, "event", req.Event, "peers", len(torrent.Peers))

	// Send response
	resp := s.buildAnnounceResponse(transactionID, torrent, addr.IP.To4() != nil, req.NumWant)
	s.sendResponse(addr, resp)

	// Update statistics
	s.mu.Lock()
	s.stats.TotalAnnounces++
	s.mu.Unlock()
}

// handleScrape handles scrape requests
func (s *Server) handleScrape(data []byte, transactionID uint32, addr *net.UDPAddr) {
	if len(data) < 16 {
		s.sendError(addr, transactionID, "Scrape request too short")
		return
	}

	// Parse scrape request
	req, err := s.parseScrapeRequest(data)
	if err != nil {
		log.Warn("invalid scrape request", "sub", "tracker", "addr", addr, "err", err)
		s.sendError(addr, transactionID, "Malformed scrape request")
		return
	}

	// Validate connection ID
	s.mu.Lock()
	connExpiry, exists := s.connections[req.ConnectionID]
	if !exists || time.Now().After(connExpiry) {
		s.mu.Unlock()
		s.sendError(addr, transactionID, "Invalid or expired connection ID")
		return
	}
	s.mu.Unlock()

	// Build scrape response
	var files []*ScrapeFileInfo
	for _, infoHash := range req.InfoHashes {
		s.muTorrents.RLock()
		torrent, exists := s.torrents[infoHash]
		s.muTorrents.RUnlock()

		if exists {
			torrent.mu.RLock()
			files = append(files, &ScrapeFileInfo{
				Seeders:   int32(torrent.Seeders),
				Completed: int32(torrent.Completed),
				Leechers:  int32(torrent.Leechers),
			})
			torrent.mu.RUnlock()
		} else {
			files = append(files, &ScrapeFileInfo{
				Seeders:   0,
				Completed: 0,
				Leechers:  0,
			})
		}
	}

	resp := s.buildScrapeResponse(transactionID, files)
	s.sendResponse(addr, resp)

	// Update statistics
	s.mu.Lock()
	s.stats.TotalScrapes++
	s.mu.Unlock()

	log.Debug("scrape", "sub", "tracker", "count", len(req.InfoHashes), "addr", addr)
}

// AnnounceRequest represents a UDP announce request according to BEP 15
type AnnounceRequest struct {
	ConnectionID  uint64
	Action        uint32
	TransactionID uint32
	InfoHash      dht.Key
	PeerID        dht.Key
	Downloaded    uint64
	Left          uint64
	Uploaded      uint64
	Event         uint32
	IP            uint32
	Key           uint32
	NumWant       int32
	Port          uint16
}

// ScrapeRequest represents a UDP scrape request according to BEP 15
type ScrapeRequest struct {
	ConnectionID  uint64
	Action        uint32
	TransactionID uint32
	InfoHashes    []string
}

// ScrapeFileInfo represents file information in scrape response
type ScrapeFileInfo struct {
	Seeders   int32
	Completed int32
	Leechers  int32
}

// parseAnnounceRequest parses an announce request from binary data
func (s *Server) parseAnnounceRequest(data []byte) (*AnnounceRequest, error) {
	if len(data) < 98 {
		return nil, fmt.Errorf("announce request too short: got %d, want 98", len(data))
	}

	req := &AnnounceRequest{
		ConnectionID:  binary.BigEndian.Uint64(data[0:8]),
		Action:        binary.BigEndian.Uint32(data[8:12]),
		TransactionID: binary.BigEndian.Uint32(data[12:16]),
		Downloaded:    binary.BigEndian.Uint64(data[56:64]),
		Left:          binary.BigEndian.Uint64(data[64:72]),
		Uploaded:      binary.BigEndian.Uint64(data[72:80]),
		Event:         binary.BigEndian.Uint32(data[80:84]),
		IP:            binary.BigEndian.Uint32(data[84:88]),
		Key:           binary.BigEndian.Uint32(data[88:92]),
		NumWant:       int32(binary.BigEndian.Uint32(data[92:96])),
		Port:          binary.BigEndian.Uint16(data[96:98]),
	}

	copy(req.InfoHash[:], data[16:36])
	copy(req.PeerID[:], data[36:56])

	return req, nil
}

// parseScrapeRequest parses a scrape request from binary data
func (s *Server) parseScrapeRequest(data []byte) (*ScrapeRequest, error) {
	if len(data) < 16 {
		return nil, fmt.Errorf("scrape request too short")
	}

	// Count infohashes (each 20 bytes)
	infoHashCount := (len(data) - 16) / 20
	req := &ScrapeRequest{
		ConnectionID:  binary.BigEndian.Uint64(data[0:8]),
		Action:        binary.BigEndian.Uint32(data[8:12]),
		TransactionID: binary.BigEndian.Uint32(data[12:16]),
		InfoHashes:    make([]string, infoHashCount),
	}

	// Extract infohashes
	for i := 0; i < infoHashCount; i++ {
		offset := 16 + i*20
		if offset+20 > len(data) {
			break
		}
		req.InfoHashes[i] = hex.EncodeToString(data[offset : offset+20])
	}

	return req, nil
}

// buildAnnounceResponse builds an announce response according to BEP 15
func (s *Server) buildAnnounceResponse(transactionID uint32, torrent *TorrentInfo, isIPv4 bool, numWant int32) []byte {
	torrent.mu.RLock()
	defer torrent.mu.RUnlock()

	// Determine interval
	interval := DefaultInterval
	if s.config.AnnounceInterval > 0 {
		interval = s.config.AnnounceInterval
	}

	// Determine how many peers to return
	maxPeers := len(torrent.Peers)

	// Apply num_want limit (-1 means all)
	if numWant > 0 && int(numWant) < maxPeers {
		maxPeers = int(numWant)
	}

	// Apply config MaxPeers limit
	if s.config.MaxPeers > 0 && maxPeers > s.config.MaxPeers {
		maxPeers = s.config.MaxPeers
	}

	// Default maximum if not configured (prevent DoS)
	const defaultMaxPeers = 200
	if s.config.MaxPeers == 0 && maxPeers > defaultMaxPeers {
		maxPeers = defaultMaxPeers
	}

	// Calculate response size
	// Header: 20 bytes + peers
	var peerSize int
	if isIPv4 {
		peerSize = 6 // 4 bytes IP + 2 bytes port
	} else {
		peerSize = 18 // 16 bytes IP + 2 bytes port
	}

	// Allocate buffer for maximum possible size
	responseSize := 20 + maxPeers*peerSize
	buf := make([]byte, responseSize)

	// Header
	binary.BigEndian.PutUint32(buf[0:4], AnnounceAction)             // action
	binary.BigEndian.PutUint32(buf[4:8], transactionID)              // transaction_id
	binary.BigEndian.PutUint32(buf[8:12], uint32(interval))          // interval
	binary.BigEndian.PutUint32(buf[12:16], uint32(torrent.Leechers)) // leechers
	binary.BigEndian.PutUint32(buf[16:20], uint32(torrent.Seeders))  // seeders

	// Add peers (up to maxPeers)
	offset := 20
	peersAdded := 0
	for _, peer := range torrent.Peers {
		if peersAdded >= maxPeers {
			break
		}

		if isIPv4 {
			ipv4 := peer.IP.To4()
			if ipv4 == nil {
				continue // Skip IPv6 peers for IPv4 response
			}
			copy(buf[offset:offset+4], ipv4)
			binary.BigEndian.PutUint16(buf[offset+4:offset+6], peer.Port)
			offset += 6
			peersAdded++
		} else {
			// IPv6 format
			ipv6 := peer.IP.To16()
			if ipv6 == nil {
				continue // Skip IPv4 peers for IPv6 response
			}
			copy(buf[offset:offset+16], ipv6)
			binary.BigEndian.PutUint16(buf[offset+16:offset+18], peer.Port)
			offset += 18
			peersAdded++
		}
	}

	return buf[:offset]
}

// buildScrapeResponse builds a scrape response according to BEP 15
func (s *Server) buildScrapeResponse(transactionID uint32, files []*ScrapeFileInfo) []byte {
	// Calculate response size
	// Header: 8 bytes + 12 bytes per file
	responseSize := 8 + 12*len(files)
	buf := make([]byte, responseSize)

	// Header
	binary.BigEndian.PutUint32(buf[0:4], ScrapeAction)  // action
	binary.BigEndian.PutUint32(buf[4:8], transactionID) // transaction_id

	// File data
	offset := 8
	for _, file := range files {
		binary.BigEndian.PutUint32(buf[offset:offset+4], uint32(file.Seeders))     // seeders
		binary.BigEndian.PutUint32(buf[offset+4:offset+8], uint32(file.Completed)) // completed
		binary.BigEndian.PutUint32(buf[offset+8:offset+12], uint32(file.Leechers)) // leechers
		offset += 12
	}

	return buf
}

// sendError sends an error response according to BEP 15
func (s *Server) sendError(addr *net.UDPAddr, transactionID uint32, message string) {
	response := make([]byte, 8+len(message))
	binary.BigEndian.PutUint32(response[0:4], ErrorAction)   // action
	binary.BigEndian.PutUint32(response[4:8], transactionID) // transaction_id
	copy(response[8:], message)                              // error message

	s.sendResponse(addr, response)
}

// sendResponse sends a response packet
func (s *Server) sendResponse(addr *net.UDPAddr, data []byte) {
	_, err := s.conn.WriteToUDP(data, addr)
	if err != nil {
		log.Warn("failed to send response", "sub", "tracker", "addr", addr, "err", err)
	}
}

// generateConnectionID generates a cryptographically secure connection ID
// Per BEP 15: "Connection IDs should not be guessable by the client"
func (s *Server) generateConnectionID() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback to timestamp-based ID if crypto/rand fails
		// This should never happen in practice
		log.Warn("failed to generate secure connection ID, using fallback", "sub", "tracker", "err", err)
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(b[:])
}

// cleanupExpiredConnections removes expired connection IDs
func (s *Server) cleanupExpiredConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for connID, expiry := range s.connections {
		if now.After(expiry) {
			delete(s.connections, connID)
		}
	}
}

// cleanupExpiredPeers removes peers that haven't announced in a while
func (s *Server) cleanupExpiredPeers() {
	// Peers should re-announce within the interval (default 30 min)
	// Allow 2x the interval before considering them expired
	peerTimeout := 2 * time.Duration(DefaultInterval) * time.Second
	if s.config.AnnounceInterval > 0 {
		peerTimeout = 2 * time.Duration(s.config.AnnounceInterval) * time.Second
	}

	s.muTorrents.Lock()
	defer s.muTorrents.Unlock()

	now := time.Now()
	totalExpired := 0

	for _, torrent := range s.torrents {
		torrent.mu.Lock()

		var activePeers []*PeerInfo
		removedSeeders := 0
		removedLeechers := 0

		for _, peer := range torrent.Peers {
			if now.Sub(peer.LastSeen) < peerTimeout {
				activePeers = append(activePeers, peer)
			} else {
				// Peer has expired
				if peer.IsSeeder() {
					removedSeeders++
				} else {
					removedLeechers++
				}
				totalExpired++
			}
		}

		// Update torrent state
		torrent.Peers = activePeers
		if torrent.Seeders > removedSeeders {
			torrent.Seeders -= removedSeeders
		} else {
			torrent.Seeders = 0
		}

		if torrent.Leechers > removedLeechers {
			torrent.Leechers -= removedLeechers
		} else {
			torrent.Leechers = 0
		}

		torrent.mu.Unlock()
	}

	if totalExpired > 0 {
		log.Debug("cleaned up expired peers", "sub", "tracker", "count", totalExpired)
	}
}

// statsReporter periodically reports server statistics
func (s *Server) statsReporter() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
			// Cleanup expired connections and peers
			s.cleanupExpiredConnections()
			s.cleanupExpiredPeers()

			s.mu.Lock()
			s.muTorrents.RLock()
			s.stats.ActiveTorrents = len(s.torrents)

			// Count total peers
			totalPeers := 0
			for _, torrent := range s.torrents {
				torrent.mu.RLock()
				totalPeers += len(torrent.Peers)
				torrent.mu.RUnlock()
			}
			s.stats.TotalPeers = int64(totalPeers)
			s.stats.ActiveConnections = len(s.connections)

			log.Info("UDP tracker stats", "sub", "tracker",
				"connections", s.stats.ActiveConnections,
				"torrents", s.stats.ActiveTorrents,
				"peers", s.stats.TotalPeers,
				"announces", s.stats.TotalAnnounces,
				"scrapes", s.stats.TotalScrapes)
			s.muTorrents.RUnlock()
			s.mu.Unlock()
		}
	}
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown() error {
	close(s.shutdown)
	return nil
}

// GetStats returns current server statistics
func (s *Server) GetStats() *ServerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}
