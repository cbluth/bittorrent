package tracker

// BEP 3: The BitTorrent Protocol Specification - Tracker Protocol
// BEP 7: IPv6 Tracker Extension
// BEP 15: UDP Tracker Protocol
// https://www.bittorrent.org/beps/bep_0003.html
// https://www.bittorrent.org/beps/bep_0007.html
// https://www.bittorrent.org/beps/bep_0015.html
//
// This file implements BitTorrent tracker communication:
// - HTTP/HTTPS tracker announces and scrapes
// - UDP tracker protocol for faster announces
// - Peer list retrieval from trackers
// - Compact and non-compact peer formats (IPv4 and IPv6)
// - Event reporting (started, stopped, completed)
// - IPv6 peer support via peers6 parameter (BEP 7)

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cbluth/bittorrent/pkg/bencode"
	"github.com/cbluth/bittorrent/pkg/dht"
)

// Event represents the event type for tracker announces.
// BEP 3 §Tracker Request: event is optional; if not present, it's a regular re-announce.
type Event string

const (
	EventNone      Event = ""          // BEP 3: regular re-announce (no event key sent)
	EventStarted   Event = "started"   // BEP 3: first announce for this download
	EventStopped   Event = "stopped"   // BEP 3: client is shutting down gracefully
	EventCompleted Event = "completed" // BEP 3: download completed (leecher → seeder)
)

// AnnounceRequest contains parameters for tracker announce.
// BEP 3 §Tracker Request: all fields map to tracker query parameters.
type AnnounceRequest struct {
	InfoHash   dht.Key // BEP 3: 20-byte SHA-1 info hash (urlencoded)
	PeerID     dht.Key // BEP 3: 20-byte peer ID (urlencoded)
	Port       uint16  // BEP 3: port this client is listening on
	Uploaded   int64   // BEP 3: total bytes uploaded since "started" event
	Downloaded int64   // BEP 3: total bytes downloaded since "started" event
	Left       int64   // BEP 3: bytes remaining to download
	Compact    bool    // BEP 23: request compact peer list (6 bytes per peer)
	NoPeerID   bool    // BEP 3: omit peer_id in responses (compact implies this)
	Event      Event   // BEP 3: started/stopped/completed or empty
	NumWant    int     // BEP 3: number of peers requested (tracker may ignore)
}

// AnnounceResponse represents the tracker's response.
// BEP 3 §Tracker Response: bencoded dictionary with peer list and timing.
type AnnounceResponse struct {
	Interval    int64  // BEP 3: seconds client should wait before next announce
	MinInterval int64  // BEP 3: optional minimum re-announce interval
	TrackerID   string // BEP 3: optional tracker identifier for future announces
	Complete    int64  // BEP 3: number of seeders
	Incomplete  int64  // BEP 3: number of leechers
	Peers       []Peer // BEP 3/23: peer list (compact or dict format)
	ExternalIP  net.IP // BEP 24: client's public IP as seen by the tracker (may be nil)
}

// ScrapeRequest contains parameters for tracker scrape
type ScrapeRequest struct {
	InfoHashes []dht.Key // Can scrape multiple torrents (up to ~74 for UDP)
}

// ScrapeResponse represents the tracker's scrape response
type ScrapeResponse struct {
	Files map[string]ScrapeStats // Key is hex-encoded info hash
}

// ScrapeStats contains statistics for a single torrent
type ScrapeStats struct {
	Complete   int64 // Number of seeders
	Downloaded int64 // Number of complete downloads (peers that completed)
	Incomplete int64 // Number of leechers
}

// Peer represents a peer returned by the tracker
type Peer struct {
	IP   net.IP
	Port uint16
	ID   []byte
}

// PeerAddr implements net.Addr for a peer
type PeerAddr struct {
	IP   net.IP
	Port uint16
}

// Network returns the network type
func (p *PeerAddr) Network() string {
	return "tcp"
}

// String returns the peer address as a string
// IPv6 addresses are wrapped in brackets per RFC 3986
func (p *PeerAddr) String() string {
	// Check if it's IPv6 (not IPv4-mapped)
	if p.IP.To4() == nil {
		// IPv6 - wrap in brackets
		return fmt.Sprintf("[%s]:%d", p.IP.String(), p.Port)
	}
	// IPv4 - no brackets needed
	return fmt.Sprintf("%s:%d", p.IP.String(), p.Port)
}

// Client handles communication with BitTorrent trackers
type Client struct {
	httpClient *http.Client
	timeout    time.Duration
}

// NewClient creates a new tracker client
func NewClient(timeout time.Duration) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: timeout,
		},
		timeout: timeout,
	}
}

// Announce sends an announce request to a tracker
func (c *Client) Announce(trackerURL string, req *AnnounceRequest) (*AnnounceResponse, error) {
	parsed, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid tracker URL: %w", err)
	}

	switch parsed.Scheme {
	case "http", "https":
		return c.announceHTTP(trackerURL, req)
	case "udp":
		return c.announceUDP(parsed.Host, req)
	case "ws", "wss":
		// WebSocket trackers use the WebTorrent signaling protocol over WebRTC.
		// Peer connections are established via data channels, not IP addresses.
		// Use ws.Client (pkg/tracker/ws) with an OnConn callback instead.
		return nil, fmt.Errorf("ws tracker %q requires ws.Client for WebRTC peer discovery", trackerURL)
	default:
		return nil, fmt.Errorf("unsupported tracker protocol: %s", parsed.Scheme)
	}
}

// Scrape sends a scrape request to a tracker
func (c *Client) Scrape(trackerURL string, req *ScrapeRequest) (*ScrapeResponse, error) {
	parsed, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid tracker URL: %w", err)
	}

	switch parsed.Scheme {
	case "http", "https":
		return c.scrapeHTTP(trackerURL, req)
	case "udp":
		return c.scrapeUDP(parsed.Host, req)
	default:
		return nil, fmt.Errorf("unsupported tracker protocol: %s", parsed.Scheme)
	}
}

// announceHTTP handles HTTP/HTTPS tracker announces
func (c *Client) announceHTTP(trackerURL string, req *AnnounceRequest) (*AnnounceResponse, error) {
	params := url.Values{}
	params.Add("info_hash", string(req.InfoHash[:]))
	params.Add("peer_id", string(req.PeerID[:]))
	params.Add("port", strconv.Itoa(int(req.Port)))
	params.Add("uploaded", strconv.FormatInt(req.Uploaded, 10))
	params.Add("downloaded", strconv.FormatInt(req.Downloaded, 10))
	params.Add("left", strconv.FormatInt(req.Left, 10))

	if req.Compact {
		params.Add("compact", "1")
	}
	if req.NoPeerID {
		params.Add("no_peer_id", "1")
	}
	if req.Event != EventNone {
		params.Add("event", string(req.Event))
	}
	if req.NumWant > 0 {
		params.Add("numwant", strconv.Itoa(req.NumWant))
	}

	fullURL := trackerURL + "?" + params.Encode()

	resp, err := c.httpClient.Get(fullURL)
	if err != nil {
		return nil, fmt.Errorf("tracker request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker returned status %d", resp.StatusCode)
	}

	return parseHTTPResponse(resp.Body)
}

// parseHTTPResponse parses the bencoded tracker response
func parseHTTPResponse(r io.Reader) (*AnnounceResponse, error) {
	var response struct {
		FailureReason string             `bencode:"failure reason,omitempty"`
		Interval      int64              `bencode:"interval"`
		MinInterval   int64              `bencode:"min interval,omitempty"`
		TrackerID     string             `bencode:"tracker id,omitempty"`
		Complete      int64              `bencode:"complete"`
		Incomplete    int64              `bencode:"incomplete"`
		Peers         bencode.RawMessage `bencode:"peers"`
		Peers6        bencode.RawMessage `bencode:"peers6,omitempty"`    // BEP 7: IPv6 peers
		ExternalIP    string             `bencode:"external ip,omitempty"` // BEP 24: client's external IP
	}

	if err := bencode.NewDecoder(r).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if response.FailureReason != "" {
		return nil, fmt.Errorf("tracker error: %s", response.FailureReason)
	}

	// Parse IPv4 peers
	peers, err := parsePeers(response.Peers)
	if err != nil {
		return nil, fmt.Errorf("failed to parse peers: %w", err)
	}

	// Parse IPv6 peers if present (BEP 7)
	if len(response.Peers6) > 0 {
		peers6, err := parsePeers6(response.Peers6)
		if err != nil {
			// Don't fail the whole request if IPv6 parsing fails
			// Just log and continue with IPv4 peers
			// log.Printf("Warning: failed to parse peers6: %v", err)
		} else {
			// Append IPv6 peers to the list
			peers = append(peers, peers6...)
		}
	}

	resp := &AnnounceResponse{
		Interval:    response.Interval,
		MinInterval: response.MinInterval,
		TrackerID:   response.TrackerID,
		Complete:    response.Complete,
		Incomplete:  response.Incomplete,
		Peers:       peers,
	}
	// BEP 24: parse external IP (4-byte IPv4 or 16-byte IPv6 packed binary)
	if len(response.ExternalIP) == 4 || len(response.ExternalIP) == 16 {
		resp.ExternalIP = net.IP([]byte(response.ExternalIP))
	}
	return resp, nil
}

// parsePeers handles both compact and non-compact peer formats
func parsePeers(raw bencode.RawMessage) ([]Peer, error) {
	// Try compact format first (binary string)
	var compactPeers string
	if err := bencode.DecodeBytes(raw, &compactPeers); err == nil {
		return parseCompactPeers([]byte(compactPeers))
	}

	// Try dictionary format
	var dictPeers []struct {
		IP     string `bencode:"ip"`
		Port   int64  `bencode:"port"`
		PeerID string `bencode:"peer id,omitempty"`
	}

	if err := bencode.DecodeBytes(raw, &dictPeers); err != nil {
		return nil, fmt.Errorf("failed to parse peers in any format: %w", err)
	}

	peers := make([]Peer, len(dictPeers))
	for i, p := range dictPeers {
		peers[i] = Peer{
			IP:   net.ParseIP(p.IP),
			Port: uint16(p.Port),
			ID:   []byte(p.PeerID),
		}
	}

	return peers, nil
}

// parseCompactPeers parses compact peer format (BEP 23).
// 6 bytes per peer: 4-byte IPv4 address + 2-byte big-endian port.
func parseCompactPeers(data []byte) ([]Peer, error) {
	if len(data)%6 != 0 {
		return nil, errors.New("invalid compact peers length")
	}

	numPeers := len(data) / 6
	peers := make([]Peer, numPeers)

	for i := 0; i < numPeers; i++ {
		offset := i * 6
		peers[i] = Peer{
			IP:   net.IP(data[offset : offset+4]),
			Port: binary.BigEndian.Uint16(data[offset+4 : offset+6]),
		}
	}

	return peers, nil
}

// parsePeers6 handles both compact and non-compact IPv6 peer formats (BEP 7)
func parsePeers6(raw bencode.RawMessage) ([]Peer, error) {
	// Try compact format first (binary string)
	var compactPeers string
	if err := bencode.DecodeBytes(raw, &compactPeers); err == nil {
		return parseCompactPeersIPv6([]byte(compactPeers))
	}

	// Try dictionary format (same as IPv4 but with IPv6 addresses)
	var dictPeers []struct {
		IP     string `bencode:"ip"`
		Port   int64  `bencode:"port"`
		PeerID string `bencode:"peer id,omitempty"`
	}

	if err := bencode.DecodeBytes(raw, &dictPeers); err != nil {
		return nil, fmt.Errorf("failed to parse peers6 in any format: %w", err)
	}

	peers := make([]Peer, len(dictPeers))
	for i, p := range dictPeers {
		peers[i] = Peer{
			IP:   net.ParseIP(p.IP),
			Port: uint16(p.Port),
			ID:   []byte(p.PeerID),
		}
	}

	return peers, nil
}

// parseCompactPeersIPv6 parses compact IPv6 peer format.
// BEP 7: 18 bytes per peer (16-byte IPv6 address + 2-byte big-endian port).
func parseCompactPeersIPv6(data []byte) ([]Peer, error) {
	if len(data)%18 != 0 {
		return nil, errors.New("invalid compact IPv6 peers length")
	}

	numPeers := len(data) / 18
	peers := make([]Peer, numPeers)

	for i := 0; i < numPeers; i++ {
		offset := i * 18
		peers[i] = Peer{
			IP:   net.IP(data[offset : offset+16]),
			Port: binary.BigEndian.Uint16(data[offset+16 : offset+18]),
		}
	}

	return peers, nil
}

// scrapeHTTP handles HTTP/HTTPS tracker scrapes
func (c *Client) scrapeHTTP(trackerURL string, req *ScrapeRequest) (*ScrapeResponse, error) {
	// Convert announce URL to scrape URL
	// Replace "/announce" with "/scrape" if present
	scrapeURL := trackerURL
	if len(trackerURL) >= 9 && trackerURL[len(trackerURL)-9:] == "/announce" {
		scrapeURL = trackerURL[:len(trackerURL)-9] + "/scrape"
	}

	// Add info_hash parameters
	params := url.Values{}
	for _, hash := range req.InfoHashes {
		params.Add("info_hash", string(hash[:]))
	}

	fullURL := scrapeURL + "?" + params.Encode()

	resp, err := c.httpClient.Get(fullURL)
	if err != nil {
		return nil, fmt.Errorf("scrape request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker returned status %d", resp.StatusCode)
	}

	return parseHTTPScrapeResponse(resp.Body)
}

// parseHTTPScrapeResponse parses the bencoded tracker scrape response
func parseHTTPScrapeResponse(r io.Reader) (*ScrapeResponse, error) {
	var response struct {
		FailureReason string                      `bencode:"failure reason,omitempty"`
		Files         map[string]map[string]int64 `bencode:"files"`
	}

	if err := bencode.NewDecoder(r).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode scrape response: %w", err)
	}

	if response.FailureReason != "" {
		return nil, fmt.Errorf("tracker error: %s", response.FailureReason)
	}

	scrapeResp := &ScrapeResponse{
		Files: make(map[string]ScrapeStats),
	}

	// Convert to our format
	for infoHash, stats := range response.Files {
		scrapeResp.Files[infoHash] = ScrapeStats{
			Complete:   stats["complete"],
			Downloaded: stats["downloaded"],
			Incomplete: stats["incomplete"],
		}
	}

	return scrapeResp, nil
}

// announceUDP handles UDP tracker announces
func (c *Client) announceUDP(host string, req *AnnounceRequest) (*AnnounceResponse, error) {
	conn, err := net.DialTimeout("udp", host, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to UDP tracker: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(c.timeout))

	// Step 1: Connect
	connectionID, err := c.udpConnect(conn)
	if err != nil {
		return nil, fmt.Errorf("UDP connect failed: %w", err)
	}

	// Step 2: Announce
	return c.udpAnnounce(conn, connectionID, req)
}

// udpConnect performs the UDP tracker connect handshake.
// BEP 15 §Connect: client sends protocol_id + action(0) + transaction_id;
// tracker responds with action(0) + transaction_id + connection_id.
func (c *Client) udpConnect(conn net.Conn) (uint64, error) {
	const protocolID uint64 = 0x41727101980 // BEP 15: magic constant for connect request

	transactionID := rand.Uint32()

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, protocolID)    // BEP 15: protocol_id (8 bytes)
	binary.Write(buf, binary.BigEndian, uint32(0))     // BEP 15: action = 0 (connect)
	binary.Write(buf, binary.BigEndian, transactionID) // BEP 15: transaction_id (4 bytes)

	if _, err := conn.Write(buf.Bytes()); err != nil {
		return 0, err
	}

	response := make([]byte, 16)
	if _, err := io.ReadFull(conn, response); err != nil {
		return 0, err
	}

	respBuf := bytes.NewReader(response)
	var action, respTransactionID uint32
	var connectionID uint64

	binary.Read(respBuf, binary.BigEndian, &action)
	binary.Read(respBuf, binary.BigEndian, &respTransactionID)
	binary.Read(respBuf, binary.BigEndian, &connectionID)

	if respTransactionID != transactionID {
		return 0, errors.New("transaction ID mismatch")
	}

	return connectionID, nil
}

// udpAnnounce sends an announce request over UDP.
// BEP 15 §Announce: connection_id + action(1) + transaction_id + info_hash + peer_id + ...
func (c *Client) udpAnnounce(conn net.Conn, connectionID uint64, req *AnnounceRequest) (*AnnounceResponse, error) {
	transactionID := rand.Uint32()

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, connectionID)  // BEP 15: from connect response
	binary.Write(buf, binary.BigEndian, uint32(1))     // BEP 15: action = 1 (announce)
	binary.Write(buf, binary.BigEndian, transactionID) // BEP 15: transaction_id
	buf.Write(req.InfoHash[:])
	buf.Write(req.PeerID[:])
	binary.Write(buf, binary.BigEndian, req.Downloaded)
	binary.Write(buf, binary.BigEndian, req.Left)
	binary.Write(buf, binary.BigEndian, req.Uploaded)
	binary.Write(buf, binary.BigEndian, uint32(eventToInt(req.Event)))
	binary.Write(buf, binary.BigEndian, uint32(0))     // IP address (default)
	binary.Write(buf, binary.BigEndian, rand.Uint32()) // key
	binary.Write(buf, binary.BigEndian, int32(req.NumWant))
	binary.Write(buf, binary.BigEndian, req.Port)

	if _, err := conn.Write(buf.Bytes()); err != nil {
		return nil, err
	}

	response := make([]byte, 1024)
	n, err := conn.Read(response)
	if err != nil {
		return nil, err
	}

	return parseUDPAnnounceResponse(response[:n], transactionID)
}

// parseUDPAnnounceResponse parses the UDP announce response.
// BEP 15 §Announce Response: action(1) + transaction_id + interval + leechers + seeders + peers.
// Supports both IPv4 (6-byte compact, BEP 23) and IPv6 (18-byte compact, BEP 7).
func parseUDPAnnounceResponse(data []byte, expectedTxID uint32) (*AnnounceResponse, error) {
	if len(data) < 20 {
		return nil, errors.New("response too short")
	}

	buf := bytes.NewReader(data)
	var action, transactionID uint32
	var interval, leechers, seeders int32

	binary.Read(buf, binary.BigEndian, &action)
	binary.Read(buf, binary.BigEndian, &transactionID)

	if transactionID != expectedTxID {
		return nil, errors.New("transaction ID mismatch")
	}

	// BEP 15: action = 3 means error response
	if action == 3 {
		errorMsg := string(data[8:])
		return nil, fmt.Errorf("tracker error: %s", errorMsg)
	}

	// BEP 15: action = 1 means announce response
	if action != 1 {
		return nil, fmt.Errorf("unexpected action: %d", action)
	}

	binary.Read(buf, binary.BigEndian, &interval) // BEP 15: announce interval
	binary.Read(buf, binary.BigEndian, &leechers) // BEP 15: number of leechers
	binary.Read(buf, binary.BigEndian, &seeders)  // BEP 15: number of seeders

	peerData := data[20:] // BEP 15: compact peer data follows header

	// Determine if this is IPv4 or IPv6 based on peer data size
	var peers []Peer
	var err error

	if len(peerData)%18 == 0 {
		// IPv6 format (18 bytes per peer)
		peers, err = parseCompactPeersIPv6(peerData)
	} else if len(peerData)%6 == 0 {
		// IPv4 format (6 bytes per peer)
		peers, err = parseCompactPeers(peerData)
	} else {
		return nil, fmt.Errorf("invalid peer data length: %d", len(peerData))
	}

	if err != nil {
		return nil, err
	}

	return &AnnounceResponse{
		Interval:   int64(interval),
		Complete:   int64(seeders),
		Incomplete: int64(leechers),
		Peers:      peers,
	}, nil
}

// scrapeUDP handles UDP tracker scrapes
func (c *Client) scrapeUDP(host string, req *ScrapeRequest) (*ScrapeResponse, error) {
	if len(req.InfoHashes) == 0 {
		return nil, errors.New("no info hashes provided")
	}

	// BEP 15: Can scrape up to about 74 torrents at once
	if len(req.InfoHashes) > 74 {
		return nil, errors.New("too many info hashes (max 74 for UDP)")
	}

	conn, err := net.DialTimeout("udp", host, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to UDP tracker: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(c.timeout))

	// Step 1: Connect
	connectionID, err := c.udpConnect(conn)
	if err != nil {
		return nil, fmt.Errorf("UDP connect failed: %w", err)
	}

	// Step 2: Scrape
	return c.udpScrape(conn, connectionID, req)
}

// udpScrape sends a scrape request over UDP.
// BEP 15 §Scrape: connection_id + action(2) + transaction_id + info_hashes.
func (c *Client) udpScrape(conn net.Conn, connectionID uint64, req *ScrapeRequest) (*ScrapeResponse, error) {
	transactionID := rand.Uint32()

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, connectionID)  // BEP 15: from connect response
	binary.Write(buf, binary.BigEndian, uint32(2))     // BEP 15: action = 2 (scrape)
	binary.Write(buf, binary.BigEndian, transactionID) // BEP 15: transaction_id

	// Add info hashes
	for _, hash := range req.InfoHashes {
		buf.Write(hash[:])
	}

	if _, err := conn.Write(buf.Bytes()); err != nil {
		return nil, err
	}

	response := make([]byte, 8+12*len(req.InfoHashes)+100) // Response size: 8 bytes header + 12 bytes per torrent
	n, err := conn.Read(response)
	if err != nil {
		return nil, err
	}

	return parseUDPScrapeResponse(response[:n], transactionID, req.InfoHashes)
}

// parseUDPScrapeResponse parses the UDP scrape response.
// BEP 15 §Scrape Response: action(2) + transaction_id + 12 bytes per torrent (seeders + completed + leechers).
func parseUDPScrapeResponse(data []byte, expectedTxID uint32, infoHashes []dht.Key) (*ScrapeResponse, error) {
	if len(data) < 8 {
		return nil, errors.New("response too short")
	}

	buf := bytes.NewReader(data)
	var action, transactionID uint32

	binary.Read(buf, binary.BigEndian, &action)
	binary.Read(buf, binary.BigEndian, &transactionID)

	if transactionID != expectedTxID {
		return nil, errors.New("transaction ID mismatch")
	}

	// Check for error response (action == 3)
	if action == 3 {
		errorMsg := string(data[8:])
		return nil, fmt.Errorf("tracker error: %s", errorMsg)
	}

	// Check for scrape response (action == 2)
	if action != 2 {
		return nil, fmt.Errorf("unexpected action: %d", action)
	}

	// Parse stats: 12 bytes per torrent (seeders + completed + leechers)
	scrapeResp := &ScrapeResponse{
		Files: make(map[string]ScrapeStats),
	}

	statsData := data[8:]
	numTorrents := len(statsData) / 12

	if numTorrents > len(infoHashes) {
		numTorrents = len(infoHashes)
	}

	for i := 0; i < numTorrents; i++ {
		offset := i * 12
		if offset+12 > len(statsData) {
			break
		}

		var seeders, completed, leechers int32
		statsReader := bytes.NewReader(statsData[offset : offset+12])
		binary.Read(statsReader, binary.BigEndian, &seeders)
		binary.Read(statsReader, binary.BigEndian, &completed)
		binary.Read(statsReader, binary.BigEndian, &leechers)

		// Use hex-encoded info hash as key
		hashHex := fmt.Sprintf("%x", infoHashes[i][:])
		scrapeResp.Files[hashHex] = ScrapeStats{
			Complete:   int64(seeders),
			Downloaded: int64(completed),
			Incomplete: int64(leechers),
		}
	}

	return scrapeResp, nil
}

// eventToInt converts Event to integer for UDP protocol.
// BEP 15: event encoding: 0=none, 1=completed, 2=started, 3=stopped.
func eventToInt(e Event) int {
	switch e {
	case EventNone:
		return 0 // BEP 15: no event
	case EventCompleted:
		return 1 // BEP 15: completed
	case EventStarted:
		return 2 // BEP 15: started
	case EventStopped:
		return 3 // BEP 15: stopped
	default:
		return 0
	}
}
