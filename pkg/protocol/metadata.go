package protocol

// BEP 9: Extension for Peers to Send Metadata Files
// https://www.bittorrent.org/beps/bep_0009.html
//
// This file implements metadata exchange via the extension protocol:
// - ut_metadata extension for requesting torrent metadata
// - Metadata request/data/reject messages
// - Block-based metadata transfer (16KB blocks)
// - Metadata reassembly and verification
// - Used to resolve magnet links and info hashes to full .torrent metadata

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"github.com/cbluth/bittorrent/pkg/log"
	"io"
	"log/slog"
	"math"
	"net"
	"time"

	"github.com/cbluth/bittorrent/pkg/bencode"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/torrent"
)

// Metadata message types (BEP 9)
const (
	MetadataRequest uint8 = 0
	MetadataData    uint8 = 1
	MetadataReject  uint8 = 2
)

// MetadataBlockSize is the standard block size for metadata exchange (16KB)
const MetadataBlockSize = 16384

// MetadataMessage represents a ut_metadata message
type MetadataMessage struct {
	MsgType   uint8 `bencode:"msg_type"`
	Piece     int   `bencode:"piece"`
	TotalSize int   `bencode:"total_size,omitempty"`
}

// OurMetadataExtID is the extension ID we announce for ut_metadata
// Peers send responses TO US using this ID
const OurMetadataExtID uint8 = 1

// MetadataExchanger handles metadata exchange with a peer
type MetadataExchanger struct {
	conn         net.Conn
	infoHash     dht.Key
	peerID       dht.Key
	peerExtID    uint8 // Peer's ut_metadata ID - use this when sending TO peer
	metadataSize int
	blocks       map[int][]byte
	numBlocks    int
	log          *slog.Logger
}

// NewMetadataExchanger creates a new metadata exchanger
func NewMetadataExchanger(conn net.Conn, infoHash, peerID dht.Key, logger *slog.Logger) *MetadataExchanger {
	if logger == nil {
		logger = log.Logger()
	}
	return &MetadataExchanger{
		conn:     conn,
		infoHash: infoHash,
		peerID:   peerID,
		blocks:   make(map[int][]byte),
		log:      logger,
	}
}

// PerformHandshake performs the BitTorrent and extension handshakes
func (m *MetadataExchanger) PerformHandshake(port int) error {
	// Set TCP socket options (critical for BitTorrent protocol)
	if tcpConn, ok := m.conn.(*net.TCPConn); ok {
		// Discard unsent data immediately on close
		tcpConn.SetLinger(0)
		// Disable Nagle's algorithm for low latency
		tcpConn.SetNoDelay(true)
	}

	// Set shorter deadlines for handshake
	m.conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Send BitTorrent handshake
	handshake := NewHandshake(m.infoHash, m.peerID)
	if _, err := m.conn.Write(handshake.Serialize()); err != nil {
		return fmt.Errorf("failed to send handshake: %w", err)
	}

	// Receive peer handshake (must be exactly 68 bytes)
	peerHandshake, err := ReadHandshake(m.conn)
	if err != nil {
		return fmt.Errorf("failed to receive handshake: %w", err)
	}

	// Verify info hash matches (some peers may not match exactly, log but continue)
	if peerHandshake.InfoHash != m.infoHash {
		// Some implementations might have different info hash handling
		// but we should still try - this is sometimes OK for DHT peers
	}

	// Check if peer supports extensions
	if !peerHandshake.SupportsExtensions() {
		return fmt.Errorf("peer does not support extensions")
	}

	// Send extension handshake
	extHandshake := NewExtensionHandshake(port)
	extData, err := extHandshake.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize extension handshake: %w", err)
	}

	extMsg := NewExtendedMessage(ExtHandshake, extData)
	if err := WriteMessage(m.conn, extMsg); err != nil {
		return fmt.Errorf("failed to send extension handshake: %w", err)
	}

	// Receive peer extension handshake - loop until we get an extension message
	// Peers may send bitfield or other messages first
	var msg *Message
	for {
		msg, err = ReadMessage(m.conn, 30*time.Second)
		if err != nil {
			return fmt.Errorf("failed to receive extension handshake: %w", err)
		}

		if msg == nil {
			// Keep-alive, keep waiting
			continue
		}

		if msg.ID == MsgExtended {
			// Got it!
			break
		}

		// Ignore other messages (bitfield, have, etc.) and keep waiting
	}

	extID, payload, err := ParseExtendedMessage(msg)
	if err != nil {
		return fmt.Errorf("failed to parse extended message: %w", err)
	}

	if extID != ExtHandshake {
		return fmt.Errorf("expected extension handshake, got ID: %d", extID)
	}

	peerExtHandshake, err := ParseExtensionHandshake(payload)
	if err != nil {
		return fmt.Errorf("failed to parse extension handshake: %w", err)
	}

	// Check if peer supports metadata exchange
	if !peerExtHandshake.HasMetadataSupport() {
		return fmt.Errorf("peer does not support ut_metadata extension")
	}

	metadataExtID, _ := peerExtHandshake.GetMetadataExtID()
	m.peerExtID = uint8(metadataExtID)
	m.metadataSize = peerExtHandshake.MetadataSize

	if m.metadataSize <= 0 {
		return fmt.Errorf("peer does not have metadata (metadata_size=0), likely a leecher")
	}

	// Calculate number of blocks
	m.numBlocks = int(math.Ceil(float64(m.metadataSize) / float64(MetadataBlockSize)))

	return nil
}

// RequestMetadata requests all metadata blocks from the peer
func (m *MetadataExchanger) RequestMetadata(timeout time.Duration) (*torrent.MetaInfo, error) {
	deadline := time.Now().Add(timeout)

	// Request all blocks
	for i := 0; i < m.numBlocks; i++ {
		if err := m.requestBlock(i); err != nil {
			return nil, fmt.Errorf("failed to request block %d: %w", i, err)
		}
	}

	// Receive all blocks
	// Use overall deadline instead of per-message timeout
	for len(m.blocks) < m.numBlocks {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("peer not sending metadata blocks (got %d/%d) - may be choking or doesn't have metadata", len(m.blocks), m.numBlocks)
		}

		// Use 30 second timeout per message to avoid getting stuck
		// But overall deadline will still apply
		if err := m.receiveBlock(30 * time.Second); err != nil {
			return nil, fmt.Errorf("failed to receive block %d/%d: %w", len(m.blocks)+1, m.numBlocks, err)
		}
	}

	// Reassemble metadata
	metadata := m.assembleMetadata()

	// Verify info hash
	if err := m.verifyInfoHash(metadata); err != nil {
		return nil, fmt.Errorf("metadata verification failed: %w", err)
	}

	// Parse metadata
	metaInfo, err := torrent.ParseMetadata(metadata, m.infoHash)
	if err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	return metaInfo, nil
}

// requestBlock requests a specific metadata block
func (m *MetadataExchanger) requestBlock(piece int) error {
	msg := MetadataMessage{
		MsgType: MetadataRequest,
		Piece:   piece,
	}

	msgData, err := bencode.EncodeBytes(msg)
	if err != nil {
		return err
	}

	extMsg := NewExtendedMessage(m.peerExtID, msgData)
	return WriteMessage(m.conn, extMsg)
}

// receiveBlock receives a metadata block from the peer
// Like magnetico, we loop and ignore all non-extension messages
func (m *MetadataExchanger) receiveBlock(timeout time.Duration) error {
	for {
		msg, err := ReadMessage(m.conn, timeout)
		if err != nil {
			return err
		}

		if msg == nil {
			// Keep-alive, ignore and continue
			continue
		}

		// Debug: log what message type we got
		m.log.Debug("received message", "id", msg.ID, "payload_len", len(msg.Payload))

		// We only care about extension messages (ID 20)
		// Ignore bitfield, have, unchoke, interested, etc.
		if msg.ID == MsgExtended {
			err := m.handleExtendedMessage(msg)
			if err == io.EOF {
				// Successfully received a metadata block
				return nil
			}
			if err != nil {
				return err
			}
			// handleExtendedMessage returns nil if it's not a metadata message (e.g., PEX)
			// In that case, continue looping to wait for metadata
			continue
		}

		// Special case: if peer chokes us, metadata exchange won't work
		if msg.ID == MsgChoke {
			return fmt.Errorf("peer choked us")
		}

		// If peer unchokes us, that's good but keep waiting for extension message
		if msg.ID == MsgUnchoke {
			// Peer unchoked us, good! Keep waiting for metadata
			continue
		}

		// Ignore all other messages (bitfield, have, etc.) and keep reading
		continue
	}
}

// handleExtendedMessage handles an extended protocol message
func (m *MetadataExchanger) handleExtendedMessage(msg *Message) error {
	extID, payload, err := ParseExtendedMessage(msg)
	if err != nil {
		return err
	}

	// Peer sends TO US using OUR announced extension ID (1)
	if extID != OurMetadataExtID {
		// Not a metadata message (probably PEX), ignore
		m.log.Debug("ignoring non-metadata extension message", "ext_id", extID, "our_id", OurMetadataExtID)
		return nil
	}

	m.log.Debug("received metadata extension message", "ext_id", extID)

	// Find the bencode dictionary end to separate metadata from data
	var metaMsg MetadataMessage
	decoder := bencode.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&metaMsg); err != nil {
		return fmt.Errorf("failed to decode metadata message: %w", err)
	}

	switch metaMsg.MsgType {
	case MetadataData:
		// Find where bencode ends and data begins
		// Use BytesParsed() to find exact boundary
		dataStart := decoder.BytesParsed()
		if dataStart >= len(payload) {
			return fmt.Errorf("no data in metadata message")
		}

		blockData := payload[dataStart:]
		m.blocks[metaMsg.Piece] = blockData
		m.log.Debug("received metadata block", "piece", metaMsg.Piece, "size", len(blockData))
		// Return special sentinel to indicate we got a block
		return io.EOF

	case MetadataReject:
		return fmt.Errorf("peer rejected metadata request for piece %d", metaMsg.Piece)

	default:
		return fmt.Errorf("unexpected metadata message type: %d", metaMsg.MsgType)
	}
}

// assembleMetadata combines all blocks into complete metadata
func (m *MetadataExchanger) assembleMetadata() []byte {
	metadata := make([]byte, 0, m.metadataSize)
	for i := 0; i < m.numBlocks; i++ {
		metadata = append(metadata, m.blocks[i]...)
	}
	return metadata[:m.metadataSize] // Trim to exact size
}

// verifyInfoHash verifies the info hash of the assembled metadata
func (m *MetadataExchanger) verifyInfoHash(metadata []byte) error {
	hash := sha1.Sum(metadata)
	if hash != m.infoHash {
		return fmt.Errorf("info hash mismatch: expected %x, got %x", m.infoHash, hash)
	}
	return nil
}

// FetchMetadata connects to a peer and fetches metadata for a torrent
func FetchMetadata(peer net.Addr, infoHash, peerID dht.Key, timeout time.Duration, logger *slog.Logger) (*torrent.MetaInfo, error) {
	// Connect to peer with shorter timeout (half of total timeout, min 3s, max 5s)
	connTimeout := timeout / 2
	if connTimeout < 3*time.Second {
		connTimeout = 3 * time.Second
	}
	if connTimeout > 5*time.Second {
		connTimeout = 5 * time.Second
	}

	conn, err := net.DialTimeout("tcp", peer.String(), connTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to peer: %w", err)
	}
	defer conn.Close()

	// Set overall deadline
	conn.SetDeadline(time.Now().Add(timeout))

	// Create exchanger
	exchanger := NewMetadataExchanger(conn, infoHash, peerID, logger)

	// Perform handshake (use port 0 since we're not accepting connections)
	if err := exchanger.PerformHandshake(0); err != nil {
		return nil, fmt.Errorf("handshake failed: %w", err)
	}

	// Request and receive metadata
	// Use remaining time, but ensure at least 5 seconds for metadata exchange
	metadataTimeout := timeout - 5*time.Second
	if metadataTimeout < 5*time.Second {
		metadataTimeout = 5 * time.Second
	}
	metaInfo, err := exchanger.RequestMetadata(metadataTimeout)
	if err != nil {
		return nil, fmt.Errorf("metadata exchange failed: %w", err)
	}

	return metaInfo, nil
}
