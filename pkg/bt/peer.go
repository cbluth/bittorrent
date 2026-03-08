package bt

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/cbluth/bittorrent/pkg/dht"
)

// Peer represents a BitTorrent peer
type Peer struct {
	// Identification
	ID   dht.Key // 20-byte peer ID
	IP   net.IP  // IP address
	Port int     // UDP port (0-65535)

	// Connection state
	Connected bool      // Whether connection is established
	LastSeen  time.Time // Last activity timestamp

	// BitTorrent protocol state
	AmChoking      bool // Whether we're choking the peer
	PeerChoking    bool // Whether peer is choking us
	Interested     bool // Whether we're interested in peer's data
	PeerInterested bool // Whether peer is interested in our data

	// Transfer state
	BytesUploaded    uint64 // Total bytes uploaded to peer
	BytesDownloaded  uint64 // Total bytes downloaded from peer
	PiecesUploaded   uint64 // Number of pieces uploaded
	PiecesDownloaded uint64 // Number of pieces downloaded

	// Torrent association
	Torrents map[string]*TorrentAssociation // Infohash -> torrent info

	// Performance metrics
	ConnectionTime time.Duration // How long connection has been active
}

// TorrentAssociation represents a peer's association with a torrent
type TorrentAssociation struct {
	InfoHash     dht.Key   // 20-byte SHA1 infohash
	Name         string    // Torrent name (if available)
	Size         uint64    // Total size in bytes
	PieceCount   uint32    // Number of pieces
	LastActivity time.Time // Last time we exchanged data
}

// ConnectionState represents the current state of a peer connection
type ConnectionState int

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
	StateHandshaking
	StateActive
)

// String returns a string representation of the connection state
func (s ConnectionState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateHandshaking:
		return "handshaking"
	case StateActive:
		return "active"
	default:
		return "unknown"
	}
}

// IsGood returns whether the peer is considered "good" based on recent activity
func (p *Peer) IsGood() bool {
	// A peer is considered good if we've seen activity within the last 15 minutes
	return time.Since(p.LastSeen) <= 15*time.Minute
}

// IsQuestionable returns whether the peer is "questionable"
func (p *Peer) IsQuestionable() bool {
	// A peer is questionable if we haven't seen activity in 15-60 minutes
	since := time.Since(p.LastSeen)
	return since > 15*time.Minute && since <= 60*time.Minute
}

// IsBad returns whether the peer is "bad"
func (p *Peer) IsBad() bool {
	// A peer is bad if we haven't seen activity in over 60 minutes
	return time.Since(p.LastSeen) > 60*time.Minute
}

// GetConnectionState returns the current connection state based on flags
func (p *Peer) GetConnectionState() ConnectionState {
	if !p.Connected {
		return StateDisconnected
	}

	if p.AmChoking || p.PeerChoking {
		return StateConnected // Connected but not transferring
	}

	if !p.Interested || !p.PeerInterested {
		return StateConnected // Connected but not transferring
	}

	return StateActive // Ready for data transfer
}

// UpdateActivity updates the peer's last seen timestamp
func (p *Peer) UpdateActivity() {
	p.LastSeen = time.Now()
}

// AddTorrent associates a peer with a torrent
func (p *Peer) AddTorrent(infoHash dht.Key, name string, size uint64, pieceCount uint32) {
	if p.Torrents == nil {
		p.Torrents = make(map[string]*TorrentAssociation)
	}

	p.Torrents[string(infoHash.String())] = &TorrentAssociation{
		InfoHash:     infoHash,
		Name:         name,
		Size:         size,
		PieceCount:   pieceCount,
		LastActivity: time.Now(),
	}
}

// RemoveTorrent removes a torrent association from a peer
func (p *Peer) RemoveTorrent(infoHash dht.Key) {
	delete(p.Torrents, infoHash.String())
}

// GetTorrent returns a torrent association if it exists
func (p *Peer) GetTorrent(infoHash dht.Key) (*TorrentAssociation, bool) {
	torrent, exists := p.Torrents[string(infoHash.String())]
	return torrent, exists
}

// ListTorrents returns all torrent associations
func (p *Peer) ListTorrents() map[string]*TorrentAssociation {
	if p.Torrents == nil {
		return make(map[string]*TorrentAssociation)
	}

	// Return a copy to prevent external modification
	result := make(map[string]*TorrentAssociation)
	for k, v := range p.Torrents {
		result[k] = v
	}
	return result
}

// GetTransferStats returns transfer statistics for a specific torrent
func (p *Peer) GetTransferStats(infoHash dht.Key) (uploaded, downloaded uint64, piecesUploaded, piecesDownloaded uint64) {
	if _, exists := p.GetTorrent(infoHash); exists {
		return uploaded, downloaded, piecesUploaded, piecesDownloaded
	}
	return 0, 0, 0, 0
}

// String returns a string representation of the peer
func (p *Peer) String() string {
	return net.JoinHostPort(p.IP.String(), strconv.Itoa(p.Port))
}

// IDString returns the peer ID as a hex string
func (p *Peer) IDString() string {
	id := make([]byte, len(p.ID))
	copy(id, p.ID[:])

	// Convert to hex string
	hex := ""
	for _, b := range id {
		hex += fmt.Sprintf("%02x", b)
	}
	return hex
}
