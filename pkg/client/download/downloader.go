// Package download provides the BitTorrent download engine.
//
// Implements the following BEPs:
//   - BEP 3:  Core peer wire protocol (handshake, messages, piece transfer, SHA-1 verification)
//   - BEP 5:  DHT — MsgPort for advertising DHT listener port
//   - BEP 6:  Fast Extension (HaveAll/HaveNone, Reject, Suggest, AllowedFast)
//   - BEP 7:  IPv6 tracker compact (peers6) — via tracker clients
//   - BEP 8:  Message Stream Encryption (MSE) — parallel encrypted+plaintext dial
//   - BEP 9:  Metadata exchange (ut_metadata) — serving info dict blocks to peers
//   - BEP 10: Extension protocol — handshake negotiation for ut_pex, ut_metadata, ut_holepunch
//   - BEP 11: Peer Exchange (PEX) — periodic diff-based peer sharing via ut_pex
//   - BEP 12: Multi-tracker (announce-list) — via metainfo
//   - BEP 14: LSD — local peer discovery
//   - BEP 23: Compact peer list — 6-byte IPv4 peer format from trackers
//   - BEP 29: uTP — micro transport protocol for NAT-friendly connections
//   - BEP 21: Partial seeds — upload_only extension handshake field
//   - BEP 27: Private torrents — disables DHT port, PEX, LSD when private=1
//   - BEP 54: lt_donthave — handle peer reneging on previously advertised pieces
//   - BEP 55: Holepunch — NAT traversal via relay (Rendezvous/Connect/Error)
//
// CSP-based architecture with per-peer reader/writer goroutines:
// 1. Parallel encryption attempts (BEP 8 MSE + plaintext + BEP 29 uTP simultaneously)
// 2. Aggressive connection parallelism (100+ concurrent dials)
// 3. Pausable channels for backpressure
// 4. Optimistic piece requesting (request next before current completes)
// 5. Enhanced piece picker with rarest-first + endgame (BEP 3 §Algorithms)
// 6. Separate reader/writer goroutines per peer
// 7. Request pipelining (250+ blocks in-flight)
// 8. Resume support with SHA-1 hash verification (BEP 3 §Pieces)
// 9. First/last piece prioritization for streaming
// 10. Clean completion detection (stops at 100%)
//
// Logging: uses pkg/log (slog wrapper) with structured "sub" attributes for topic filtering.
package download

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"

	"github.com/cbluth/bittorrent/pkg/bencode"
	"github.com/cbluth/bittorrent/pkg/choker"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/lsd"
	"github.com/cbluth/bittorrent/pkg/mse"
	"github.com/cbluth/bittorrent/pkg/pex"
	"github.com/cbluth/bittorrent/pkg/protocol"
	"github.com/cbluth/bittorrent/pkg/punch"
	"github.com/cbluth/bittorrent/pkg/upnp"
)

const (
	// Connection settings
	MaxParallelDials = 100 // Dial 100 peers simultaneously
	DialTimeout      = 10 * time.Second
	HandshakeTimeout = 10 * time.Second

	// Protocol settings
	BlockSize            = 16384 // BEP 3: 2^14 (16 KiB) — standard block size
	RequestQueueDepth    = 250   // Pipeline 250 block requests per peer
	MaxDuplicateDownload = 3     // BEP 3 §Endgame: request same piece from N peers

	// Performance settings (BEP 3 §Choking)
	MaxActivePeers  = 40               // Maintain up to 40 active connections
	UnchokeInterval = 10 * time.Second // BEP 3: rechoke every 10 seconds
	NumUnchoked     = 4                // BEP 3: unchoke top 4 peers (tit-for-tat)
	NumOptimistic   = 1                // BEP 3: 1 optimistic unchoke every 30s

	// Snub detection — BEP 3 §Choking: reclaim unchoke slot from stalled peers
	SnubTimeout = 15 * time.Second // Disconnect peer if no block received within this window

	// Memory limits
	MaxConcurrentWrites = 1               // Only 1 piece write at a time (disk I/O)
	PieceBufferSize     = 4 * 1024 * 1024 // 4MB max piece size
)

// Downloader implements the optimized BitTorrent download engine
type Downloader struct {
	// Configuration
	infoHash      dht.Key
	peerID        dht.Key
	port          uint16
	pieceSize     uint32
	numPieces     uint32
	totalSize     uint64
	outputPath    string
	strategy      PieceSelectionStrategy
	files         []FileInfo // Multi-file torrent support
	shareRatio    float64    // 0=stop immediately, >0=seed until ratio met, Inf=forever
	rawInfoDict   []byte     // raw bencoded info dict for BEP 9 ut_metadata serving (may be nil)
	bookendPieces int        // first/last N pieces of a file range always marked urgent (default 2)
	private       bool       // BEP 27: private torrent — disables DHT port, PEX, LSD
	externalIP    atomic.Pointer[net.IP] // BEP 40: our external IP as reported by peers/trackers

	// State (accessed atomically or via channels)
	bitfield   *Bitfield
	downloaded atomic.Uint64
	uploaded   atomic.Uint64
	completed  atomic.Bool // Set to true when all pieces downloaded

	// Peer management
	peers       sync.Map // map[string]*Peer
	activePeers atomic.Int32

	// BEP 3 §Choking: tit-for-tat unchoke algorithm (see pkg/choker)
	chkr *choker.Choker

	// Piece management
	piecePicker *PiecePicker
	pieces      []*Piece

	// Channels for coordination (CSP pattern)
	newPeerAddrs  chan []*net.TCPAddr
	peerMessages  chan PeerMessage
	pieceComplete chan *Piece
	closeC        chan struct{}
	doneC         chan struct{}

	// Pausable channels for backpressure
	pieceDataPausable *PausableChannel

	// Metrics
	startTime      time.Time
	downloadSpeed  atomic.Uint64 // bytes/sec
	lastDownloaded uint64        // previous tick's downloaded value (non-atomic, only used in updateSpeed)

	// Waiter registry: tracks which pieces HTTP streaming goroutines are waiting
	// for. The downloader focuses on the minimum-needed piece so a VLC end-of-file
	// probe (registering piece N-1) never starves the main stream (piece 11).
	waitersMu sync.Mutex
	waiters   map[uint32]int // pieceIndex → count of goroutines waiting

	// Seek debouncer: absorbs rapid seeks (VLC scrubbing) and only fires
	// JumpToOffset after the position stabilizes.
	seekMu     sync.Mutex
	seekTarget uint32      // latest requested piece (updated on every seek)
	seekTimer  *time.Timer // fires settleSeek after settleDelay

	// Logging
	statsTicks uint32 // counter for throttling post-complete [Stats] output
}

// Bitfield tracks which pieces we have
type Bitfield struct {
	mu   sync.RWMutex
	bits []byte
	len  uint32
}

// NewBitfield creates a new bitfield
func NewBitfield(numPieces uint32) *Bitfield {
	numBytes := (numPieces + 7) / 8
	return &Bitfield{
		bits: make([]byte, numBytes),
		len:  numPieces,
	}
}

// Set marks a piece as complete
func (b *Bitfield) Set(index uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if index/8 >= uint32(len(b.bits)) {
		return
	}
	b.bits[index/8] |= 1 << (7 - index%8)
}

// Test checks if we have a piece
func (b *Bitfield) Test(index uint32) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if index/8 >= uint32(len(b.bits)) {
		return false
	}
	return (b.bits[index/8] & (1 << (7 - index%8))) != 0
}

// Clear marks a piece as not present (BEP 54: lt_donthave).
func (b *Bitfield) Clear(index uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if index/8 >= uint32(len(b.bits)) {
		return
	}
	b.bits[index/8] &^= 1 << (7 - index%8)
}

// AllSet reports whether all n pieces are marked complete.
func (b *Bitfield) AllSet(n uint32) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for i := uint32(0); i < n; i++ {
		if (b.bits[i/8] & (1 << (7 - i%8))) == 0 {
			return false
		}
	}
	return true
}

// Piece represents a piece of the torrent
type Piece struct {
	Index   uint32
	Length  uint32
	Hash    dht.Key
	Done    atomic.Bool // set to true when the piece is verified and written to disk
	Writing atomic.Bool // set while the piece is being hashed/written (prevents re-requests)
	Buffer  []byte
	Blocks  map[uint32]bool // block offset -> downloaded; protected by Mu
	Mu      sync.Mutex      // Exported: guards Blocks
}

// noPriority is the sentinel value for priorityPiece / maxPiece that means "not set".
// We use MaxUint32 so that piece index 0 can be a valid priority.
const noPriority = math.MaxUint32

// PiecePicker implements rarest-first + endgame strategy
type PiecePicker struct {
	Pieces        []*PieceAvailability          // Exported for streaming support
	byIndex       map[uint32]*PieceAvailability // fast O(1) lookup by Piece.Index; read-only after init
	endgame       atomic.Bool
	maxDuplicate  int
	strategy      PieceSelectionStrategy
	priorityPiece atomic.Uint32 // sequential: first piece to fetch; noPriority = pause
	maxPiece      atomic.Uint32 // sequential: last piece to fetch (inclusive); noPriority = unlimited
	urgentPieces  sync.Map      // map[uint32]bool — tail pieces (MKV Cues) fetched alongside main window
	Mu            sync.RWMutex  // Exported for streaming support
}

// PieceAvailability tracks piece rarity and download status
type PieceAvailability struct {
	Piece        *Piece
	Availability int32    // Number of peers having this piece
	Requesting   sync.Map // map[*Peer]bool
	Snubbed      sync.Map // map[*Peer]bool
	Choked       sync.Map // map[*Peer]bool
}

// PausableChannel implements a channel that can be suspended
type PausableChannel struct {
	buffer chan PieceData
	paused atomic.Bool
	mu     sync.Mutex
}

// NewPausableChannel creates a new pausable channel
func NewPausableChannel(bufferSize int) *PausableChannel {
	return &PausableChannel{
		buffer: make(chan PieceData, bufferSize),
	}
}

// Send attempts to send data if not paused
func (p *PausableChannel) Send(data PieceData) bool {
	if p.paused.Load() {
		return false
	}
	select {
	case p.buffer <- data:
		return true
	default:
		return false
	}
}

// Receive gets data from channel
func (p *PausableChannel) Receive() <-chan PieceData {
	return p.buffer
}

// Suspend pauses the channel
func (p *PausableChannel) Suspend() {
	p.paused.Store(true)
}

// Resume resumes the channel
func (p *PausableChannel) Resume() {
	p.paused.Store(false)
}

// ── BEP 40: Canonical Peer Priority ──────────────────────────────────────────

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// peerPriority computes the BEP 40 canonical peer priority.
// Lower values indicate higher priority peers.
//
// For different IPs:
//
//	priority = crc32c(sort(masked_a, masked_b))
//
// For same IPs (different ports):
//
//	priority = crc32c(sort(port_a, port_b))
//
// IPv4 masks: same /24 → FF.FF.FF.55, same /16 → FF.FF.FF.FF, else → FF.FF.55.55
// IPv6 masks: similar progressive unmasking based on shared prefix length.
func peerPriority(a, b net.IP, portA, portB uint16) uint32 {
	a4 := a.To4()
	b4 := b.To4()
	if a4 != nil && b4 != nil {
		return peerPriorityIPv4(a4, b4, portA, portB)
	}
	// IPv6 or mixed — use 16-byte form
	a16 := a.To16()
	b16 := b.To16()
	if a16 != nil && b16 != nil {
		return peerPriorityIPv6(a16, b16, portA, portB)
	}
	return 0
}

func peerPriorityIPv4(a, b net.IP, portA, portB uint16) uint32 {
	// Same IP → use ports
	if a.Equal(b) {
		var buf [4]byte
		lo, hi := portA, portB
		if lo > hi {
			lo, hi = hi, lo
		}
		binary.BigEndian.PutUint16(buf[0:2], lo)
		binary.BigEndian.PutUint16(buf[2:4], hi)
		return crc32.Checksum(buf[:], crc32cTable)
	}

	// Determine mask based on shared prefix
	var mask [4]byte
	switch {
	case a[0] == b[0] && a[1] == b[1] && a[2] == b[2]: // same /24
		mask = [4]byte{0xFF, 0xFF, 0xFF, 0x55}
	case a[0] == b[0] && a[1] == b[1]: // same /16
		mask = [4]byte{0xFF, 0xFF, 0xFF, 0xFF}
	default:
		mask = [4]byte{0xFF, 0xFF, 0x55, 0x55}
	}

	var ma, mb [4]byte
	for i := range 4 {
		ma[i] = a[i] & mask[i]
		mb[i] = b[i] & mask[i]
	}

	// Sort the two masked IPs lexicographically
	var buf [8]byte
	if compareFourBytes(ma, mb) <= 0 {
		copy(buf[0:4], ma[:])
		copy(buf[4:8], mb[:])
	} else {
		copy(buf[0:4], mb[:])
		copy(buf[4:8], ma[:])
	}
	return crc32.Checksum(buf[:], crc32cTable)
}

func peerPriorityIPv6(a, b net.IP, portA, portB uint16) uint32 {
	if a.Equal(b) {
		var buf [4]byte
		lo, hi := portA, portB
		if lo > hi {
			lo, hi = hi, lo
		}
		binary.BigEndian.PutUint16(buf[0:2], lo)
		binary.BigEndian.PutUint16(buf[2:4], hi)
		return crc32.Checksum(buf[:], crc32cTable)
	}

	// BEP 40 IPv6: start mask FFFF:FFFF:FFFF:5555:5555:5555:5555:5555
	// Progressively unmask as shared prefix gets longer (each 8-bit step).
	baseMask := [16]byte{
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x55, 0x55,
		0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55,
	}
	// Find shared prefix length (in bytes) beyond the base /48
	shared := 6 // base: first 6 bytes always fully matched in mask
	for shared < 16 && a[shared] == b[shared] {
		baseMask[shared] = 0xFF
		shared++
	}

	var ma, mb [16]byte
	for i := 0; i < 16; i++ {
		ma[i] = a[i] & baseMask[i]
		mb[i] = b[i] & baseMask[i]
	}

	var buf [32]byte
	if compareBytes(ma[:], mb[:]) <= 0 {
		copy(buf[0:16], ma[:])
		copy(buf[16:32], mb[:])
	} else {
		copy(buf[0:16], mb[:])
		copy(buf[16:32], ma[:])
	}
	return crc32.Checksum(buf[:], crc32cTable)
}

func compareFourBytes(a, b [4]byte) int {
	for i := range 4 {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

func compareBytes(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return len(a) - len(b)
}

// Peer represents a connected peer
type Peer struct {
	Addr     *net.TCPAddr
	Conn     net.Conn
	PeerID   dht.Key
	Bitfield *Bitfield
	UseUTP   bool   // BEP 29: connected via uTP (micro transport protocol)
	Priority uint32 // BEP 40: canonical peer priority (lower = higher priority)

	// State
	AmChoking      atomic.Bool
	AmInterested   atomic.Bool
	PeerChoking    atomic.Bool
	PeerInterested atomic.Bool

	// Download state
	Downloading  atomic.Bool
	CurrentPiece atomic.Pointer[Piece]

	// Channels
	messages chan interface{}
	requests chan BlockRequest
	cancels  chan BlockRequest      // cancel messages; drained before requests in peerWriter
	outgoing chan *protocol.Message // non-request outgoing msgs (have, piece, ext)
	closeC   chan struct{}
	doneC    chan struct{}

	// BEP 6: Fast Extension state
	fastEnabled    bool            // both sides set reserved[7] & 0x04
	allowedFastOut map[uint32]bool // pieces WE allow them to request while choked
	allowedFastIn  map[uint32]bool // pieces THEY allow us to request while choked

	// BEP 10: extension protocol state — IDs from peer's extension handshake
	extSupported    bool          // peer advertised BEP 10 in reserved bytes
	peerPEXID       atomic.Uint32 // BEP 11: peer's ut_pex extension ID (0 = not advertised)
	peerMetadataID  atomic.Uint32 // BEP 9: peer's ut_metadata extension ID (0 = not advertised)
	peerHolepunchID atomic.Uint32 // BEP 55: peer's ut_holepunch extension ID (0 = not advertised)
	peerDontHaveID  atomic.Uint32 // BEP 54: peer's lt_donthave extension ID (0 = not advertised)
	uploadOnly      atomic.Bool   // BEP 21: peer is a partial seed (not downloading)
	pexState        *pex.State    // BEP 11: per-connection PEX diff state

	// Choker stats — read by GetPeers callback, written by reader goroutine.
	// bytesFromPeer is swapped to 0 each rechoke cycle so it represents a window.
	bytesFromPeer     atomic.Int64
	bytesToPeer       atomic.Int64
	lastPieceNano     atomic.Int64 // UnixNano of most recent piece block received
	downloadStartNano atomic.Int64 // UnixNano when current piece download was assigned
	connectedAt       time.Time    // set once on creation; used for 3× optimistic weight

	// Metrics
	downloadSpeed atomic.Uint64
	uploadSpeed   atomic.Uint64
	lastActivity  atomic.Int64 // Unix timestamp
}

// PeerMessage wraps a message from a peer
type PeerMessage struct {
	Peer    *Peer
	Message interface{}
}

// PieceData contains downloaded piece data
type PieceData struct {
	Peer  *Peer // the peer that sent this block; used to free the peer on early-exit
	Piece *Piece
	Index uint32
	Begin uint32
	Data  []byte
}

// BlockRequest is a request for a block
type BlockRequest struct {
	Index  uint32
	Begin  uint32
	Length uint32
}

// NewDownloader creates a new optimized downloader
func NewDownloader(config DownloaderConfig) (*Downloader, error) {
	numPieces := uint32(len(config.PieceHashes))

	// Create pieces
	pieces := make([]*Piece, numPieces)
	for i := range numPieces {
		pieceLen := config.PieceSize
		if i == numPieces-1 && config.TotalSize%uint64(config.PieceSize) != 0 {
			// Last piece may be smaller
			pieceLen = uint32(config.TotalSize % uint64(config.PieceSize))
		}

		pieces[i] = &Piece{
			Index:  i,
			Length: pieceLen,
			Hash:   config.PieceHashes[i],
			Buffer: make([]byte, pieceLen),
			Blocks: make(map[uint32]bool),
		}
	}

	// ShareRatio: 0 (zero-value) and callers that don't set it seed forever.
	// Callers must explicitly pass math.Inf(1) for "seed forever" or a positive
	// value for a target ratio. 0.0 means stop seeding immediately after 100%.
	// The CLI/config layer reads the user's setting and defaults to math.Inf(1).
	shareRatio := config.ShareRatio
	if shareRatio == 0 {
		// Zero-value — treat as "not configured", default to seeding forever.
		shareRatio = math.Inf(1)
	}

	bookend := config.BookendPieces
	if bookend <= 0 {
		bookend = 2 // default: first 2 + last 2 pieces always urgent
	}

	d := &Downloader{
		infoHash:          config.InfoHash,
		peerID:            config.PeerID,
		port:              config.Port,
		pieceSize:         config.PieceSize,
		numPieces:         numPieces,
		totalSize:         config.TotalSize,
		outputPath:        config.OutputPath,
		strategy:          config.Strategy,
		files:             config.Files,
		shareRatio:        shareRatio,
		rawInfoDict:       config.RawInfoDict,
		bookendPieces:     bookend,
		private:           config.Private,
		bitfield:          NewBitfield(numPieces),
		pieces:            pieces,
		newPeerAddrs:      make(chan []*net.TCPAddr, 10),
		peerMessages:      make(chan PeerMessage, 100),
		pieceComplete:     make(chan *Piece, 10),
		closeC:            make(chan struct{}),
		doneC:             make(chan struct{}),
		pieceDataPausable: NewPausableChannel(50),
		startTime:         time.Now(),
		waiters:           make(map[uint32]int),
	}

	// Initialize piece picker
	d.piecePicker = NewPiecePicker(pieces, MaxDuplicateDownload, config.Strategy)

	// BEP 3 §Choking: initialise choker with tit-for-tat unchoke algorithm.
	d.chkr = choker.New(choker.Config{
		Slots:    NumUnchoked + NumOptimistic,
		IsSeeder: false,
		GetPeers: func() []choker.PeerStats {
			var stats []choker.PeerStats
			d.peers.Range(func(_, v any) bool {
				p := v.(*Peer)
				var lastRcv time.Time
				if nano := p.lastPieceNano.Load(); nano != 0 {
					lastRcv = time.Unix(0, nano)
				}
				stats = append(stats, choker.PeerStats{
					ID:                  choker.PeerID(p.Addr.String()),
					Interested:          p.PeerInterested.Load(),
					BytesDownloadedFrom: p.bytesFromPeer.Swap(0), // sliding window
					BytesUploadedTo:     p.bytesToPeer.Load(),
					LastReceived:        lastRcv,
					ConnectedAt:         p.connectedAt,
				})
				return true
			})
			return stats
		},
		SetChoked: func(id choker.PeerID, choked bool) {
			v, ok := d.peers.Load(string(id))
			if !ok {
				return
			}
			p := v.(*Peer)
			p.AmChoking.Store(choked)
			if err := d.sendChokeMsg(p, choked); err != nil {
				log.Warn("failed to send choke message", "sub", "choker", "choked", choked, "peer", string(id), "err", err)
			}
			if !choked {
				// Newly unchoked — give them something to download.
				go d.startPeerDownload(p)
			}
		},
	})

	return d, nil
}

// PieceSelectionStrategy determines how pieces are selected for download.
type PieceSelectionStrategy int

const (
	// StrategyRarestFirst prioritizes the rarest pieces first (best for swarm health).
	// BEP 3 §Algorithms: rarest-first maximises piece diversity across the swarm.
	StrategyRarestFirst PieceSelectionStrategy = iota
	// StrategySequential downloads pieces in order (best for streaming/preview).
	// Weakens BEP 3 incentive mechanisms but necessary for real-time playback.
	StrategySequential
)

func (s PieceSelectionStrategy) String() string {
	switch s {
	case StrategyRarestFirst:
		return "rarest-first"
	case StrategySequential:
		return "sequential"
	default:
		return "unknown"
	}
}

// DownloaderConfig contains configuration for the downloader
// FileInfo describes a file in a multi-file torrent
type FileInfo struct {
	Path   []string // Path components (e.g., ["folder", "subfolder", "file.txt"])
	Length int64    // File size in bytes
	Offset int64    // Byte offset from start of torrent
}

type DownloaderConfig struct {
	InfoHash    dht.Key
	PeerID      dht.Key
	Port        uint16
	PieceSize   uint32
	TotalSize   uint64
	PieceHashes []dht.Key
	OutputPath  string                 // For single-file: file path. For multi-file: directory path
	Strategy    PieceSelectionStrategy // Piece selection strategy (default: StrategyRarestFirst)

	// Multi-file torrent support
	Files []FileInfo // If non-empty, this is a multi-file torrent

	// ShareRatio controls when to stop seeding after download completes.
	// 0.0  = stop immediately (no seeding).
	// >0   = seed until uploaded/downloaded >= ShareRatio.
	// Inf  = seed forever (default when zero-value).
	ShareRatio float64

	// RawInfoDict is the raw bencoded info dictionary.
	// When non-nil, we advertise ut_metadata support and serve blocks to peers (BEP 9).
	RawInfoDict []byte

	// BookendPieces is the number of pieces at the start and end of a file
	// range that are always marked urgent in sequential mode.  This ensures
	// container headers (MP4 moov, MKV EBML) and seek tables (MKV Cues) are
	// fetched immediately.  Default: 2.
	BookendPieces int

	// Private indicates a private torrent (BEP 27).
	// When true, DHT port advertisement, PEX, and LSD are disabled.
	// Peers MUST only come from trackers listed in the metainfo.
	Private bool
}

// NewPiecePicker creates a new piece picker
func NewPiecePicker(pieces []*Piece, maxDuplicate int, strategy PieceSelectionStrategy) *PiecePicker {
	avail := make([]*PieceAvailability, len(pieces))
	byIndex := make(map[uint32]*PieceAvailability, len(pieces))
	for i, p := range pieces {
		pa := &PieceAvailability{Piece: p}
		avail[i] = pa
		byIndex[p.Index] = pa
	}

	pp := &PiecePicker{
		Pieces:       avail,
		byIndex:      byIndex,
		maxDuplicate: maxDuplicate,
		strategy:     strategy,
	}
	pp.priorityPiece.Store(noPriority) // no file requested yet
	pp.maxPiece.Store(noPriority)      // no upper bound
	return pp
}

// PickPiece selects the next piece to download for this peer.
// BEP 3 §Algorithms: implements rarest-first with endgame mode.
func (pp *PiecePicker) PickPiece(peer *Peer) *Piece {
	pp.Mu.Lock()
	defer pp.Mu.Unlock()

	// Don't pick if peer is already downloading
	if peer.Downloading.Load() {
		return nil
	}

	// ── Sequential strategy ───────────────────────────────────────────────────
	//
	// In sequential mode we only download pieces within the requested file's
	// byte range.  Until SetPieceRange is called (i.e. no HTTP request has
	// come in yet), priorityPiece == noPriority and we return nil — nothing is
	// downloaded proactively.
	//
	// Once a file is requested, the readahead window [priorityPiece,
	// priorityPiece+readaheadWindow) is served in ascending index order so the
	// player always has the next N pieces queued.  Pieces beyond the window but
	// still within the file range fall back to rarest-first.  Pieces outside the
	// file range (other files in a multi-file torrent) are never picked.
	if pp.strategy == StrategySequential {
		priorityIdx := pp.priorityPiece.Load()
		if priorityIdx == noPriority {
			// No file has been requested yet — don't download anything.
			return nil
		}
		maxIdx := pp.maxPiece.Load() // noPriority = no upper bound
		const readaheadWindow = uint32(30)
		windowEnd := priorityIdx + readaheadWindow

		// Pass 1: sequential window [priorityIdx, windowEnd) — lowest index first.
		// Also collect the best urgent piece (MKV Cues / tail reads) as a fallback.
		var bestInWindow *PieceAvailability
		var bestUrgent *PieceAvailability
		for _, pa := range pp.Pieces {
			idx := pa.Piece.Index
			if pa.Piece.Done.Load() || pa.Piece.Writing.Load() {
				continue
			}
			if !peer.Bitfield.Test(idx) {
				continue
			}
			cnt := 0
			pa.Requesting.Range(func(_, _ interface{}) bool { cnt++; return true })
			if cnt != 0 {
				continue
			}
			// Window piece?
			inWindow := idx >= priorityIdx && idx < windowEnd && (maxIdx == noPriority || idx <= maxIdx)
			if inWindow && (bestInWindow == nil || idx < bestInWindow.Piece.Index) {
				bestInWindow = pa
				continue
			}
			// Urgent piece? (tail reads, MKV Cues)
			if _, ok := pp.urgentPieces.Load(idx); ok {
				if bestUrgent == nil || idx < bestUrgent.Piece.Index {
					bestUrgent = pa
				}
			}
		}
		if bestInWindow != nil {
			bestInWindow.Requesting.Store(peer, true)
			return bestInWindow.Piece
		}
		if bestUrgent != nil {
			bestUrgent.Requesting.Store(peer, true)
			return bestUrgent.Piece
		}

		// Pass 2: forward-from-priority for pieces beyond the window but within file range.
		// After a seek, pieces AHEAD of the window are more useful than pieces behind it
		// (VLC plays forward). Sort circularly: [windowEnd..max, 0..priorityIdx).
		sort.Slice(pp.Pieces, func(i, j int) bool {
			ai, aj := pp.Pieces[i].Piece.Index, pp.Pieces[j].Piece.Index
			aAhead := ai >= priorityIdx
			bAhead := aj >= priorityIdx
			if aAhead != bAhead {
				return aAhead // pieces at/after priority come first
			}
			return ai < aj
		})
		for _, pa := range pp.Pieces {
			idx := pa.Piece.Index
			if pa.Piece.Done.Load() || pa.Piece.Writing.Load() {
				continue
			}
			if idx >= priorityIdx && idx < windowEnd {
				continue // already handled in pass 1
			}
			if maxIdx != noPriority && idx > maxIdx {
				continue
			}
			if !peer.Bitfield.Test(idx) {
				continue
			}
			cnt := 0
			pa.Requesting.Range(func(_, _ interface{}) bool { cnt++; return true })
			if cnt == 0 {
				pa.Requesting.Store(peer, true)
				return pa.Piece
			}
		}

		// All file pieces are in-flight — endgame within file range.
		wasEndgame := pp.endgame.Swap(true)
		if !wasEndgame {
			log.Info("entering endgame mode", "sub", "endgame")
		}
		// First pass: endgame within file range.
		// Second pass (if first yields nothing): allow out-of-range pieces
		// so the torrent completes fully for seeding.
		for pass := range 2 {
			for _, pa := range pp.Pieces {
				idx := pa.Piece.Index
				if pa.Piece.Done.Load() || pa.Piece.Writing.Load() {
					continue
				}
				if pass == 0 && maxIdx != noPriority && idx > maxIdx {
					continue
				}
				if !peer.Bitfield.Test(idx) {
					continue
				}
				cnt := 0
				pa.Requesting.Range(func(_, _ interface{}) bool { cnt++; return true })
				if cnt < pp.maxDuplicate {
					pa.Requesting.Store(peer, true)
					return pa.Piece
				}
			}
		}
		return nil
	}

	// ── Rarest-first strategy (and fallback) ──────────────────────────────────

	// PRIORITY 1: First and last pieces (preview/header + index data).
	// Find them by Index, not by array position (array may be reordered).
	lastPieceIdx := uint32(len(pp.Pieces) - 1)
	for _, pa := range pp.Pieces {
		idx := pa.Piece.Index
		if idx != 0 && idx != lastPieceIdx {
			continue
		}
		if pa.Piece.Done.Load() || pa.Piece.Writing.Load() || !peer.Bitfield.Test(idx) {
			continue
		}
		cnt := 0
		pa.Requesting.Range(func(_, _ interface{}) bool { cnt++; return true })
		if cnt == 0 {
			log.Debug("piece selected (first/last priority)", "sub", "piece", "addr", peer.Addr, "piece", idx)
			pa.Requesting.Store(peer, true)
			return pa.Piece
		}
	}

	// Sort by rarest-first.
	sort.Slice(pp.Pieces, func(i, j int) bool {
		return atomic.LoadInt32(&pp.Pieces[i].Availability) <
			atomic.LoadInt32(&pp.Pieces[j].Availability)
	})

	// Find unrequested piece that peer has.
	candidatesChecked := 0
	for _, pa := range pp.Pieces {
		if pa.Piece.Done.Load() || pa.Piece.Writing.Load() {
			continue
		}
		candidatesChecked++
		if !peer.Bitfield.Test(pa.Piece.Index) {
			continue
		}
		cnt := 0
		pa.Requesting.Range(func(_, _ interface{}) bool { cnt++; return true })
		if cnt == 0 {
			log.Debug("piece selected (rarest-first)", "sub", "piece", "addr", peer.Addr, "piece", pa.Piece.Index, "checked", candidatesChecked)
			pa.Requesting.Store(peer, true)
			return pa.Piece
		}
	}

	log.Debug("no unrequested pieces found", "sub", "piece", "addr", peer.Addr, "checked", candidatesChecked)

	// No unrequested pieces — enter endgame mode.
	wasEndgame := pp.endgame.Swap(true)
	if !wasEndgame {
		log.Info("entering endgame mode", "sub", "endgame")
	}

	endgameCandidates := 0
	for _, pa := range pp.Pieces {
		if pa.Piece.Done.Load() || pa.Piece.Writing.Load() {
			continue
		}
		endgameCandidates++
		if !peer.Bitfield.Test(pa.Piece.Index) {
			continue
		}
		cnt := 0
		pa.Requesting.Range(func(_, _ interface{}) bool { cnt++; return true })
		if cnt < pp.maxDuplicate {
			log.Debug("endgame piece selected", "sub", "endgame", "addr", peer.Addr, "piece", pa.Piece.Index, "requestors", cnt)
			pa.Requesting.Store(peer, true)
			return pa.Piece
		}
	}

	log.Debug("endgame no pieces available", "sub", "endgame", "addr", peer.Addr, "checked", endgameCandidates, "maxDup", pp.maxDuplicate)
	return nil
}

// HandleHave updates availability when peer announces they have a piece.
// Uses byIndex so the lookup is correct even after sort.Slice reorders Pieces.
func (pp *PiecePicker) HandleHave(peer *Peer, index uint32) {
	peer.Bitfield.Set(index)
	if pa, ok := pp.byIndex[index]; ok {
		atomic.AddInt32(&pa.Availability, 1)
	}
}

// HandleDontHave updates availability when a peer no longer has a piece (BEP 54).
// Clears the piece from the peer's bitfield and decrements availability.
func (pp *PiecePicker) HandleDontHave(peer *Peer, index uint32) {
	peer.Bitfield.Clear(index)
	if pa, ok := pp.byIndex[index]; ok {
		if v := atomic.AddInt32(&pa.Availability, -1); v < 0 {
			atomic.StoreInt32(&pa.Availability, 0)
		}
	}
}

// AbortPiece removes a peer from requesting a piece (e.g. when choked).
// Uses the byIndex map so the result is correct even after sort.Slice reorders Pieces.
func (pp *PiecePicker) AbortPiece(peer *Peer, pieceIndex uint32) {
	pp.Mu.Lock()
	defer pp.Mu.Unlock()

	if pa, ok := pp.byIndex[pieceIndex]; ok {
		pa.Requesting.Delete(peer)
	}
}

// AbortAllPieces removes a peer from the Requesting map of every piece.
// Called on peer disconnect to prevent stale entries from blocking endgame.
func (pp *PiecePicker) AbortAllPieces(peer *Peer) {
	pp.Mu.Lock()
	defer pp.Mu.Unlock()

	for _, pa := range pp.Pieces {
		pa.Requesting.Delete(peer)
	}
}

// PieceByIndex returns the PieceAvailability for the given piece index, or nil.
// byIndex is read-only after NewPiecePicker, so this is safe without holding Mu.
func (pp *PiecePicker) PieceByIndex(index uint32) *PieceAvailability {
	return pp.byIndex[index]
}

// SetPriorityPiece sets the first piece to fetch in sequential mode.
// Also used by JumpToOffset to service seek requests.
func (pp *PiecePicker) SetPriorityPiece(pieceIndex uint32) {
	pp.priorityPiece.Store(pieceIndex)
}

// SetPieceRange restricts sequential downloading to pieces in [minPiece, maxPiece].
// Call this when an HTTP request comes in for a specific file so we don't
// download pieces that belong to other files in the torrent.
// Calling with maxPiece == noPriority removes the upper bound.
func (pp *PiecePicker) SetPieceRange(minPiece, maxPiece uint32) {
	pp.priorityPiece.Store(minPiece)
	pp.maxPiece.Store(maxPiece)
}

// verifyExistingPieces checks if output file/directory exists and verifies piece
// hashes. Supports both single-file and multi-file torrents for resume.
func (d *Downloader) verifyExistingPieces() error {
	if len(d.files) > 0 {
		return d.verifyExistingPiecesMultiFile()
	}

	f, err := os.Open(d.outputPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}
	if uint64(info.Size()) > d.totalSize {
		log.Warn("file larger than expected", "sub", "verifier", "fileSize", info.Size(), "expected", d.totalSize)
	}

	buf := make([]byte, d.pieceSize)
	for i, piece := range d.pieces {
		offset := int64(i) * int64(d.pieceSize)
		n, err := f.ReadAt(buf[:piece.Length], offset)
		if err != nil && err != io.EOF {
			log.Warn("piece read error during verification", "sub", "verifier", "piece", i, "offset", offset, "err", err)
			continue
		}
		if uint32(n) != piece.Length {
			continue
		}
		hash := sha1.Sum(buf[:piece.Length])
		if hash == piece.Hash {
			piece.Done.Store(true)
			d.bitfield.Set(uint32(i))
		}
	}
	return nil
}

// verifyExistingPiecesMultiFile verifies pieces for a multi-file torrent by
// reading across individual files in the torrent directory.
func (d *Downloader) verifyExistingPiecesMultiFile() error {
	// Check if the output directory exists at all.
	if _, err := os.Stat(d.outputPath); os.IsNotExist(err) {
		return nil
	}

	buf := make([]byte, d.pieceSize)
	for i, piece := range d.pieces {
		globalOffset := int64(i) * int64(d.pieceSize)
		n, err := d.readMultiFileAt(buf[:piece.Length], globalOffset)
		if err != nil {
			log.Warn("piece read error during verification", "sub", "verifier", "piece", i, "offset", globalOffset, "err", err)
			continue
		}
		if uint32(n) != piece.Length {
			continue
		}
		hash := sha1.Sum(buf[:piece.Length])
		if hash == piece.Hash {
			piece.Done.Store(true)
			d.bitfield.Set(uint32(i))
		}
	}
	return nil
}

// readMultiFileAt reads len(buf) bytes starting at globalOffset (a byte offset
// into the logical concatenation of all torrent files) across however many
// individual files are needed to satisfy the read.
func (d *Downloader) readMultiFileAt(buf []byte, globalOffset int64) (int, error) {
	totalRead := 0
	dataLen := len(buf)
	for totalRead < dataLen {
		cur := globalOffset + int64(totalRead)

		// Find which file contains cur.
		fileIdx := -1
		var fileOffset int64
		for i, file := range d.files {
			if cur >= file.Offset && cur < file.Offset+file.Length {
				fileIdx = i
				fileOffset = cur - file.Offset
				break
			}
		}
		if fileIdx == -1 {
			break // beyond end of torrent data
		}

		file := d.files[fileIdx]
		remaining := file.Length - fileOffset
		want := min(remaining, int64(dataLen - totalRead))

		filePath := filepath.Join(append([]string{d.outputPath}, file.Path...)...)
		f, err := os.Open(filePath)
		if os.IsNotExist(err) {
			// File not written yet; treat as missing data.
			return totalRead, nil
		}
		if err != nil {
			return totalRead, err
		}
		n, err := f.ReadAt(buf[totalRead:totalRead+int(want)], fileOffset)
		f.Close()
		totalRead += n
		if n == 0 {
			// File on disk is shorter than torrent metadata says (partial download).
			// No progress possible — return what we have.
			return totalRead, nil
		}
		if err != nil && err != io.EOF {
			return totalRead, err
		}
	}
	return totalRead, nil
}

// Run starts the downloader event loop
func (d *Downloader) Run(ctx context.Context) error {
	defer close(d.doneC)

	log.Info("downloader started", "sub", "download", "port", d.port, "pieces", d.numPieces, "totalSize", d.totalSize)

	// Start uTP listener for incoming peers (BEP 29).
	utpListener, err := protocol.Listen(ctx, fmt.Sprintf(":%d", d.port))
	if err != nil {
		log.Error("uTP listener failed", "sub", "utp", "port", d.port, "err", err)
	} else {
		log.Info("uTP listening", "sub", "utp", "port", d.port)
		defer utpListener.Close()
		go d.acceptUTPPeers(ctx, utpListener)
	}

	// UPnP port mapping for incoming peer connections (fire-and-forget).
	go func() {
		extIP, extPort, cleanup, err := upnp.MapPort(ctx, "TCP", int(d.port), "BitTorrent")
		if err != nil {
			log.Debug("UPnP TCP mapping failed", "sub", "upnp", "err", err)
			return
		}
		defer cleanup()
		log.Info("UPnP mapped", "sub", "upnp", "proto", "TCP", "ext", fmt.Sprintf("%s:%d", extIP, extPort))
		<-ctx.Done()
	}()
	go func() {
		_, _, cleanup, err := upnp.MapPort(ctx, "UDP", int(d.port), "BitTorrent uTP")
		if err != nil {
			log.Debug("UPnP UDP mapping failed", "sub", "upnp", "err", err)
			return
		}
		defer cleanup()
		<-ctx.Done()
	}()

	// BEP 14: Local Service Discovery — announce on LAN and listen for local peers.
	// BEP 27: disabled for private torrents.
	if !d.private {
		go lsd.Run(ctx, d.infoHash, d.port, d.AddPeers)
	}

	// Check for existing file and verify pieces
	log.Info("starting piece verification", "sub", "download")
	verifyStart := time.Now()
	if err := d.verifyExistingPieces(); err != nil {
		log.Warn("failed to verify existing pieces", "sub", "download", "err", err)
	}
	log.Info("piece verification done", "sub", "download", "elapsed", time.Since(verifyStart).Round(time.Millisecond))

	existingPieces := d.countCompletedPieces()
	if existingPieces > 0 {
		log.Info("resuming download", "sub", "download", "verified", existingPieces, "total", d.numPieces, "progress", fmt.Sprintf("%.1f%%", float64(existingPieces)/float64(d.numPieces)*100))
	} else {
		log.Info("starting fresh download", "sub", "download", "pieces", d.numPieces, "totalSize", d.totalSize)
	}

	// Check if already complete
	if existingPieces == d.numPieces {
		d.completed.Store(true)
		log.Info("download already complete", "sub", "download",
			"file", d.outputPath,
			"size", d.totalSize,
			"size_mb", fmt.Sprintf("%.2f", float64(d.totalSize)/(1024*1024)))
		// Continue running to seed (upload) to peers
	}

	// Check if context already cancelled before entering the loop.
	if ctx.Err() != nil {
		log.Info("context already cancelled", "sub", "download", "err", ctx.Err())
		return ctx.Err()
	}

	log.Debug("entering select loop", "sub", "download", "backlog", len(d.newPeerAddrs))

	// Reset speed tracking baseline so verification bytes don't inflate speed.
	d.startTime = time.Now()
	d.lastDownloaded = d.downloaded.Load()

	speedTicker := time.NewTicker(time.Second)
	defer speedTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("context done", "sub", "download", "err", ctx.Err())
			return ctx.Err()

		case <-d.closeC:
			return nil

		case addrs := <-d.newPeerAddrs:
			// New peers from tracker/DHT
			log.Debug("received peer addresses", "sub", "download", "count", len(addrs))
			go d.connectToPeers(addrs)

		case pm := <-d.peerMessages:
			// Message from peer
			d.handlePeerMessage(pm)

		case pd := <-d.pieceDataPausable.Receive():
			// Piece data received
			d.handlePieceData(pd)

		case piece := <-d.pieceComplete:
			// Piece download complete - verify and write
			go d.verifyAndWritePiece(piece)

		case <-speedTicker.C:
			d.updateSpeed()
			total, unchoked, idle := d.PeerStats()
			completed := d.countCompletedPieces()
			log.Debug("tick", "sub", "download", "peers", total, "unchoked", unchoked, "idle", idle, "completed", completed, "total", d.numPieces)
			// Once download is complete, check share ratio.
			if d.completed.Load() {
				if d.ratioMet() {
					log.Info("share ratio reached", "sub", "seeding", "ratio", fmt.Sprintf("%.2f", d.shareRatio), "uploaded", d.uploaded.Load(), "downloaded", d.downloaded.Load())
					return nil
				}
			} else {
				// BEP 3 §Choking: disconnect peers that stalled on their piece.
				d.disconnectSnubbedPeers()
				// Safety net: reschedule any idle unchoked peers that slipped through.
				d.startIdlePeerDownloads()
			}
		}
	}
}

// connectToPeers dials multiple peers in parallel
// Optimized: 100 concurrent dials, simultaneous encrypted+plaintext attempts
func (d *Downloader) connectToPeers(addrs []*net.TCPAddr) {
	log.Debug("connecting to peers", "sub", "download", "count", len(addrs))

	sem := make(chan struct{}, MaxParallelDials)
	var wg sync.WaitGroup

	for _, addr := range addrs {
		if d.activePeers.Load() >= MaxActivePeers {
			break
		}

		// Skip our own listen address to avoid self-connections.
		if addr.Port == int(d.port) && (addr.IP.IsLoopback() || addr.IP.IsUnspecified()) {
			continue
		}

		wg.Add(1)
		go func(addr *net.TCPAddr) {
			defer wg.Done()

			sem <- struct{}{}        // Acquire semaphore
			defer func() { <-sem }() // Release semaphore

			// OPTIMIZATION: Try encrypted and plaintext simultaneously
			peer, err := d.dialPeerOptimized(addr)
			if err != nil {
				log.Debug("dial failed", "sub", "dial", "addr", addr, "err", err)
				// Direct connection failed; ask a relay peer to help (BEP 55).
				go d.tryHolepunchViaRelay(addr)
				return
			}

			log.Debug("dial connected", "sub", "dial", "addr", addr, "utp", peer.UseUTP)
			d.addPeer(peer)
			go d.runPeer(peer)
		}(addr)
	}

	wg.Wait()
	log.Debug("connection attempts complete", "sub", "download", "active", d.activePeers.Load(), "attempted", len(addrs))
}

// dialPeerOptimized races three transports and returns the first to succeed:
//  1. MSE-encrypted TCP (immediately)
//  2. Plaintext TCP (100 ms delay to prefer encrypted)
//  3. uTP / BEP 29 (200 ms delay to prefer TCP, UDP-friendly for firewalled peers)
func (d *Downloader) dialPeerOptimized(addr *net.TCPAddr) (*Peer, error) {
	type result struct {
		conn          net.Conn
		extSupported  bool
		fastSupported bool
		useUTP        bool
		err           error
	}

	resultC := make(chan result, 3)
	ctx, cancel := context.WithTimeout(context.Background(), DialTimeout)
	defer cancel()

	go func() {
		conn, ext, fast, err := d.dialEncrypted(ctx, addr)
		resultC <- result{conn: conn, extSupported: ext, fastSupported: fast, err: err}
	}()
	go func() {
		time.Sleep(100 * time.Millisecond)
		conn, ext, fast, err := d.dialPlaintext(ctx, addr)
		resultC <- result{conn: conn, extSupported: ext, fastSupported: fast, err: err}
	}()
	go func() {
		time.Sleep(200 * time.Millisecond)
		conn, ext, fast, err := d.dialUTP(ctx, addr)
		resultC <- result{conn: conn, extSupported: ext, fastSupported: fast, useUTP: true, err: err}
	}()

	for range 3 {
		select {
		case res := <-resultC:
			if res.err == nil {
				cancel()
				peer := d.makePeer(addr, res.conn, res.extSupported, res.fastSupported)
				peer.UseUTP = res.useUTP
				return peer, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("all connection attempts failed")
}

// makePeer constructs a Peer from an already-handshaked connection.
func (d *Downloader) makePeer(addr *net.TCPAddr, conn net.Conn, extSupported, fastEnabled bool) *Peer {
	p := &Peer{
		Addr:         addr,
		Conn:         conn,
		Bitfield:     NewBitfield(d.numPieces),
		messages:     make(chan interface{}, 50),
		requests:     make(chan BlockRequest, RequestQueueDepth),
		cancels:      make(chan BlockRequest, RequestQueueDepth),
		outgoing:     make(chan *protocol.Message, 64),
		closeC:       make(chan struct{}),
		doneC:        make(chan struct{}),
		connectedAt:  time.Now(),
		extSupported: extSupported,
		fastEnabled:  fastEnabled,
		pexState:     pex.NewState(),
	}
	p.AmChoking.Store(true)
	p.PeerChoking.Store(true)

	// BEP 6: compute allowed fast set for this peer.
	if fastEnabled {
		set := protocol.AllowedFastSet(10, d.numPieces, d.infoHash, addr.IP)
		p.allowedFastOut = make(map[uint32]bool, len(set))
		for _, idx := range set {
			p.allowedFastOut[idx] = true
		}
		p.allowedFastIn = make(map[uint32]bool)
	}

	return p
}

// dialEncrypted attempts an MSE-encrypted connection (BEP 8).
// On success the BEP 3 handshake runs on top of the MSE-wrapped conn so all
// subsequent peer wire traffic is transparently encrypted/decrypted.
// Failures here are expected when the remote peer doesn't support MSE; the
// plaintext dial attempt running in parallel will succeed in that case.
func (d *Downloader) dialEncrypted(ctx context.Context, addr *net.TCPAddr) (net.Conn, bool, bool, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, false, false, err
	}

	// MSE handshake: DH key exchange + RC4 (or plaintext obfuscation).
	// Prefer RC4 but accept plaintext obfuscation from peers that don't do RC4.
	mseConn, err := mse.InitiatorHandshake(conn, d.infoHash, mse.CryptoRC4|mse.CryptoPlaintext)
	if err != nil {
		conn.Close()
		return nil, false, false, err
	}

	// BEP 3 handshake runs on the MSE-wrapped conn; Read/Write are now RC4.
	return d.doHandshake(mseConn)
}

// dialPlaintext attempts plaintext TCP connection.
func (d *Downloader) dialPlaintext(ctx context.Context, addr *net.TCPAddr) (net.Conn, bool, bool, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, false, false, err
	}
	return d.doHandshake(conn)
}

// dialUTP attempts a uTP (BEP 29) connection and runs the BEP 3 handshake on top.
func (d *Downloader) dialUTP(ctx context.Context, addr *net.TCPAddr) (net.Conn, bool, bool, error) {
	utpAddr := fmt.Sprintf("%s:%d", addr.IP, addr.Port)
	conn, err := protocol.DialContext(ctx, "udp", utpAddr)
	if err != nil {
		return nil, false, false, err
	}
	return d.doHandshake(conn)
}

// doHandshake performs the BEP 3 peer wire handshake.
// Returns (conn, supportsExtensions, supportsFast, err).
// Sets the BEP 10 extension bit, BEP 5 DHT bit, and BEP 6 fast bit in the outgoing handshake.
func (d *Downloader) doHandshake(conn net.Conn) (net.Conn, bool, bool, error) {
	conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	// BEP 3: send handshake with BEP 10 extension bit, BEP 5 DHT bit, and BEP 6 fast bit set.
	hs := protocol.NewHandshake(d.infoHash, d.peerID)
	if _, err := conn.Write(hs.Serialize()); err != nil {
		conn.Close()
		return nil, false, false, err
	}

	// Receive peer's handshake.
	peerHS, err := protocol.ReadHandshake(conn)
	if err != nil {
		conn.Close()
		return nil, false, false, err
	}

	// BEP 3: verify info hash matches — reject peers on different torrents.
	if peerHS.InfoHash != d.infoHash {
		conn.Close()
		return nil, false, false, fmt.Errorf("info hash mismatch")
	}

	// BEP 6: fast extension is enabled only when BOTH sides set the bit.
	// We always set it (in NewHandshake), so it's enabled iff the peer sets it too.
	return conn, peerHS.SupportsExtensions(), peerHS.SupportsFast(), nil
}

// addPeer adds a peer to active peers
func (d *Downloader) addPeer(peer *Peer) {
	d.peers.Store(peer.Addr.String(), peer)
	d.activePeers.Add(1)
	// Notify choker so it can consider the new peer immediately.
	d.chkr.Trigger()
}

// runPeer runs the peer goroutine
// OPTIMIZATION: Separate reader and writer goroutines for full-duplex I/O
func (d *Downloader) runPeer(peer *Peer) {
	defer close(peer.doneC)
	defer d.removePeer(peer)

	// Start reader goroutine
	go d.peerReader(peer)

	// Start writer goroutine
	go d.peerWriter(peer)

	pexTicker := time.NewTicker(60 * time.Second)
	defer pexTicker.Stop()

	// Main peer loop - coordinates reader/writer and handles messages
	for {
		select {
		case <-peer.closeC:
			return

		case <-pexTicker.C:
			// BEP 27: never send PEX for private torrents.
			if !d.private && peer.peerPEXID.Load() != 0 {
				d.sendPEX(peer)
			}

		case msgInterface, ok := <-peer.messages:
			if !ok {
				return
			}

			// Type assert to *protocol.Message
			msg, ok := msgInterface.(*protocol.Message)
			if !ok {
				log.Warn("invalid message type", "sub", "peer", "addr", peer.Addr)
				continue
			}

			// Send to main event loop for processing
			select {
			case d.peerMessages <- PeerMessage{Peer: peer, Message: msg}:
			case <-peer.closeC:
				return
			}
		}
	}
}

// handleMessage processes a single BEP 3 peer wire message.
func (d *Downloader) handleMessage(peer *Peer, msg *protocol.Message) {
	switch msg.ID {
	case protocol.MsgChoke: // BEP 3: peer will not serve our requests
		peer.PeerChoking.Store(true)

		// If peer was downloading, abort that piece
		if peer.Downloading.Load() {
			currentPiece := peer.CurrentPiece.Load()
			if currentPiece != nil {
				log.Debug("choked while downloading", "sub", "peer", "addr", peer.Addr, "piece", currentPiece.Index)
				d.piecePicker.AbortPiece(peer, currentPiece.Index)
				peer.CurrentPiece.Store(nil)
			} else {
				log.Debug("choked (no current piece)", "sub", "peer", "addr", peer.Addr)
			}
			peer.Downloading.Store(false)
		} else {
			log.Debug("choked", "sub", "peer", "addr", peer.Addr)
		}
		// Reassign the freed piece to another idle peer immediately.
		d.startIdlePeerDownloads()

	case protocol.MsgUnchoke: // BEP 3: peer will now serve our requests
		peer.PeerChoking.Store(false)

		// Try to pick a piece and start downloading
		go d.startPeerDownload(peer)

	case protocol.MsgInterested: // BEP 3: peer wants pieces we have
		peer.PeerInterested.Store(true)
		// BEP 3 §Choking: trigger immediate rechoke evaluation for new interest
		d.chkr.Trigger()

	case protocol.MsgNotInterested: // BEP 3: peer no longer wants our pieces
		peer.PeerInterested.Store(false)

	case protocol.MsgHave: // BEP 3: peer completed and verified a piece
		have, err := protocol.ParseHaveMessage(msg)
		if err != nil {
			log.Error("invalid have message", "sub", "peer", "addr", peer.Addr, "err", err)
			return
		}

		peer.Bitfield.Set(have.PieceIndex)
		d.piecePicker.HandleHave(peer, have.PieceIndex)

	case protocol.MsgBitfield: // BEP 3: full piece availability bitmap (first msg after handshake)
		bitfield, err := protocol.ParseBitfieldMessage(msg)
		if err != nil {
			log.Error("invalid bitfield", "sub", "peer", "addr", peer.Addr, "err", err)
			return
		}

		// Copy peer's bitfield; update len so bounds checks stay correct.
		peer.Bitfield.mu.Lock()
		peer.Bitfield.bits = make([]byte, len(bitfield.Bitfield))
		copy(peer.Bitfield.bits, bitfield.Bitfield)
		peer.Bitfield.len = uint32(len(bitfield.Bitfield)) * 8
		peer.Bitfield.mu.Unlock()

		// Update piece availability — only iterate pieces covered by the
		// received bitfield; the bounds-checked Test() guards the rest.
		peerBits := uint32(len(bitfield.Bitfield)) * 8
		limit := d.numPieces
		if peerBits < limit {
			limit = peerBits
		}
		for i := uint32(0); i < limit; i++ {
			if peer.Bitfield.Test(i) {
				if pa, ok := d.piecePicker.byIndex[i]; ok {
					atomic.AddInt32(&pa.Availability, 1)
				}
			}
		}

		pieceCount := protocol.CountPieces(bitfield.Bitfield)
		log.Debug("peer connected", "sub", "peer", "addr", peer.Addr, "pieces", pieceCount, "total", d.numPieces, "active", d.activePeers.Load())

	case protocol.MsgRequest: // BEP 3: peer requests a block — serve if unchoked and we have it
		if peer.AmChoking.Load() {
			if peer.fastEnabled {
				// BEP 6: reject unless piece is in the allowed fast set.
				req, err := protocol.ParseRequestMessage(msg)
				if err != nil || req.Index >= uint32(len(d.pieces)) {
					return
				}
				if !peer.allowedFastOut[req.Index] {
					select {
					case peer.outgoing <- protocol.NewRejectMessage(req.Index, req.Begin, req.Length):
					default:
					}
					return
				}
				// Allowed fast piece — fall through to serve it.
				if !d.pieces[req.Index].Done.Load() {
					select {
					case peer.outgoing <- protocol.NewRejectMessage(req.Index, req.Begin, req.Length):
					default:
					}
					return
				}
				if req.Length > BlockSize*2 {
					return
				}
				data, err := d.readBlock(req.Index, req.Begin, req.Length)
				if err != nil {
					log.Warn("failed to read block for upload", "sub", "peer", "addr", peer.Addr, "piece", req.Index, "offset", req.Begin, "err", err)
					return
				}
				select {
				case peer.outgoing <- protocol.NewPieceMessage(req.Index, req.Begin, data):
				default:
				}
				return
			}
			return // BEP 3: silently ignore requests from choked peers
		}
		req, err := protocol.ParseRequestMessage(msg)
		if err != nil || req.Index >= uint32(len(d.pieces)) {
			return
		}
		if !d.pieces[req.Index].Done.Load() {
			return
		}
		if req.Length > BlockSize*2 {
			return // sanity check
		}
		data, err := d.readBlock(req.Index, req.Begin, req.Length)
		if err != nil {
			log.Warn("failed to read block for upload", "sub", "peer", "addr", peer.Addr, "piece", req.Index, "offset", req.Begin, "err", err)
			return
		}
		select {
		case peer.outgoing <- protocol.NewPieceMessage(req.Index, req.Begin, data):
		default:
			// peer too slow, drop
		}

	case protocol.MsgPiece: // BEP 3: received a block of piece data
		piece, err := protocol.ParsePieceMessage(msg)
		if err != nil {
			log.Error("invalid piece message", "sub", "peer", "addr", peer.Addr, "err", err)
			return
		}

		// Send piece data to main loop via pausable channel
		pd := PieceData{
			Peer:  peer,
			Piece: d.pieces[piece.Index],
			Index: piece.Index,
			Begin: piece.Begin,
			Data:  piece.Block,
		}

		if !d.pieceDataPausable.Send(pd) {
			// Channel paused during disk write — drop this block.
			// The piece will be re-requested after the write completes
			// and startIdlePeerDownloads() reschedules idle peers.
			log.Debug("piece data dropped (disk write in progress)", "sub", "peer", "addr", peer.Addr, "piece", piece.Index, "offset", piece.Begin)
		}

		// Update speed and choker stats.
		blockLen := uint64(len(piece.Block))
		peer.downloadSpeed.Add(blockLen)
		peer.bytesFromPeer.Add(int64(blockLen))
		peer.lastPieceNano.Store(time.Now().UnixNano())

	case protocol.MsgCancel: // BEP 3: cancel a pending request (endgame mode)
		// best-effort: block was already sent or not, nothing to do

	case protocol.MsgExtended: // BEP 10: extension protocol message
		extID, payload, err := protocol.ParseExtendedMessage(msg)
		if err != nil {
			return
		}
		if extID == protocol.ExtHandshake {
			// BEP 10: extension handshake — peer declares local extension IDs
			hs, err := protocol.ParseExtensionHandshake(payload)
			if err != nil {
				return
			}
			if id, ok := hs.M["ut_pex"]; ok && id > 0 {
				peer.peerPEXID.Store(uint32(id)) // BEP 11
			}
			if id, ok := hs.M["ut_metadata"]; ok && id > 0 {
				peer.peerMetadataID.Store(uint32(id)) // BEP 9
			}
			if id, ok := hs.M["ut_holepunch"]; ok && id > 0 {
				peer.peerHolepunchID.Store(uint32(id)) // BEP 55
			}
			if id, ok := hs.M["lt_donthave"]; ok && id > 0 {
				peer.peerDontHaveID.Store(uint32(id)) // BEP 54
			}
			// BEP 21: track partial seed status
			if hs.UploadOnly == 1 {
				peer.uploadOnly.Store(true)
			}
			return
		}
		// Incoming ut_metadata request: peer sends using their declared ID for
		// ut_metadata (stored in peerMetadataID), not our local ID.
		if peerMetaID := peer.peerMetadataID.Load(); peerMetaID != 0 && extID == uint8(peerMetaID) && len(d.rawInfoDict) > 0 {
			var metaMsg protocol.MetadataMessage
			if err := bencode.DecodeBytes(payload, &metaMsg); err != nil {
				return
			}
			if metaMsg.MsgType == protocol.MetadataRequest {
				d.serveMetadataBlock(peer, metaMsg.Piece)
			}
			return
		}
		// Incoming PEX message: the peer uses peerPEXID as their local ut_pex ID.
		// BEP 27: ignore PEX for private torrents.
		if peerPEXID := peer.peerPEXID.Load(); !d.private && peerPEXID != 0 && extID == uint8(peerPEXID) {
			pexMsg, err := pex.Decode(payload)
			if err != nil {
				return
			}
			addrs := make([]*net.TCPAddr, 0, len(pexMsg.Added))
			for _, pi := range pexMsg.Added {
				if pi.Addr != nil {
					addrs = append(addrs, pi.Addr)
				}
			}
			if len(addrs) > 0 {
				log.Debug("PEX received", "sub", "pex", "count", len(addrs), "from", peer.Addr)
				d.AddPeers(addrs)
			}
		}
		// Incoming ut_holepunch message: peer uses their declared ID.
		if holepunchID := peer.peerHolepunchID.Load(); holepunchID != 0 && extID == uint8(holepunchID) {
			d.handleHolepunch(peer, payload)
		}
		// BEP 54: lt_donthave — peer no longer has a piece.
		// Note: we always handle incoming lt_donthave regardless of whether
		// the peer advertised support (per BEP 54 spec).
		if dontHaveID := peer.peerDontHaveID.Load(); dontHaveID != 0 && extID == uint8(dontHaveID) {
			if len(payload) >= 4 {
				pieceIndex := binary.BigEndian.Uint32(payload[:4])
				if pieceIndex < d.numPieces {
					d.piecePicker.HandleDontHave(peer, pieceIndex)
					log.Debug("lt_donthave received", "sub", "peer", "addr", peer.Addr, "piece", pieceIndex)
				}
			}
		}

	// ── BEP 6 Fast Extension Messages ──────────────────────────────────────

	case protocol.MsgHaveAll: // BEP 6: peer has every piece (seed)
		if !peer.fastEnabled {
			return
		}
		peer.Bitfield.mu.Lock()
		for i := uint32(0); i < d.numPieces; i++ {
			protocol.SetPiece(peer.Bitfield.bits, int(i))
		}
		peer.Bitfield.mu.Unlock()
		for i := uint32(0); i < d.numPieces; i++ {
			if pa, ok := d.piecePicker.byIndex[i]; ok {
				atomic.AddInt32(&pa.Availability, 1)
			}
		}
		log.Debug("peer has all pieces (BEP 6)", "sub", "peer", "addr", peer.Addr, "total", d.numPieces, "active", d.activePeers.Load())

	case protocol.MsgHaveNone: // BEP 6: peer has no pieces
		if !peer.fastEnabled {
			return
		}
		log.Debug("peer has no pieces (BEP 6)", "sub", "peer", "addr", peer.Addr)

	case protocol.MsgSuggest: // BEP 6: advisory — peer suggests we download this piece
		if !peer.fastEnabled {
			return
		}
		suggest, err := protocol.ParseSuggestMessage(msg)
		if err != nil {
			return
		}
		log.Debug("suggest piece (BEP 6)", "sub", "peer", "addr", peer.Addr, "piece", suggest.PieceIndex)

	case protocol.MsgReject: // BEP 6: peer explicitly rejects a request
		if !peer.fastEnabled {
			return
		}
		rej, err := protocol.ParseRejectMessage(msg)
		if err != nil {
			return
		}
		log.Debug("request rejected (BEP 6)", "sub", "peer", "addr", peer.Addr, "piece", rej.Index, "offset", rej.Begin)
		// If we were downloading this piece from this peer, abort and reassign.
		if peer.Downloading.Load() {
			currentPiece := peer.CurrentPiece.Load()
			if currentPiece != nil && currentPiece.Index == rej.Index {
				d.piecePicker.AbortPiece(peer, currentPiece.Index)
				peer.CurrentPiece.Store(nil)
				peer.Downloading.Store(false)
				d.startIdlePeerDownloads()
			}
		}

	case protocol.MsgAllowedFast: // BEP 6: piece we may request while choked
		if !peer.fastEnabled {
			return
		}
		af, err := protocol.ParseAllowedFastMessage(msg)
		if err != nil || af.PieceIndex >= d.numPieces {
			return
		}
		peer.allowedFastIn[af.PieceIndex] = true
		log.Debug("allowed fast (BEP 6)", "sub", "peer", "addr", peer.Addr, "piece", af.PieceIndex)
		// If peer is choking us and we need this piece, try downloading it.
		if peer.PeerChoking.Load() && !peer.Downloading.Load() && !d.completed.Load() {
			go d.startPeerDownload(peer)
		}

	default:
		log.Debug("unknown message type", "sub", "peer", "addr", peer.Addr, "msgID", msg.ID)
	}
}

// startPeerDownload picks a piece and starts downloading it
func (d *Downloader) startPeerDownload(peer *Peer) {
	// Don't start if download is complete
	if d.completed.Load() {
		return
	}

	// Don't start if peer is choking us (unless BEP 6 allowed fast pieces available)
	if peer.PeerChoking.Load() {
		if !peer.fastEnabled || len(peer.allowedFastIn) == 0 {
			return
		}
		// BEP 6: we can still request allowed fast pieces while choked.
		// Check if any allowed fast piece is still needed.
		hasNeeded := false
		for idx := range peer.allowedFastIn {
			if idx < uint32(len(d.pieces)) && !d.pieces[idx].Done.Load() && peer.Bitfield.Test(idx) {
				hasNeeded = true
				break
			}
		}
		if !hasNeeded {
			return
		}
	}

	// Check if already downloading (but don't set flag yet - PickPiece needs it to be false)
	if peer.Downloading.Load() {
		return
	}

	// Pick a piece - this must happen BEFORE setting Downloading flag
	// because PickPiece checks the flag internally
	piece := d.piecePicker.PickPiece(peer)
	if piece == nil {
		return
	}

	// Now set the downloading flag atomically to prevent races
	// If another goroutine set it between our check and here, abort
	if peer.Downloading.Swap(true) {
		// Race: another goroutine won the swap between our Load check and here.
		// Return the piece we picked so it isn't stranded in Requesting forever.
		d.piecePicker.AbortPiece(peer, piece.Index)
		return
	}

	log.Debug("downloading piece", "sub", "peer", "addr", peer.Addr, "piece", piece.Index)

	peer.downloadStartNano.Store(time.Now().UnixNano())
	peer.CurrentPiece.Store(piece)

	// Request all blocks in the piece (pipelined).
	// Non-blocking send: if the channel is full the peer's writer is
	// overwhelmed; abort and let startIdlePeerDownloads reschedule later.
	numBlocks := (piece.Length + BlockSize - 1) / BlockSize
	for i := uint32(0); i < numBlocks; i++ {
		blockSize := uint32(BlockSize)
		if i == numBlocks-1 {
			// Last block may be smaller
			blockSize = piece.Length - (i * BlockSize)
		}

		req := BlockRequest{
			Index:  piece.Index,
			Begin:  i * BlockSize,
			Length: blockSize,
		}

		select {
		case peer.requests <- req:
		default:
			log.Warn("request queue full", "sub", "peer", "addr", peer.Addr, "piece", piece.Index)
			d.piecePicker.AbortPiece(peer, piece.Index)
			peer.Downloading.Store(false)
			peer.CurrentPiece.Store(nil)
			return
		}
	}
}

// startIdlePeerDownloads assigns new pieces to idle, unchoked peers
// This is called after a piece completes to keep peers busy
func (d *Downloader) startIdlePeerDownloads() {
	d.peers.Range(func(key, value interface{}) bool {
		peer := value.(*Peer)

		// Check if peer is idle (unchoked but not downloading)
		// Call directly (not in goroutine) to avoid race conditions
		if !peer.PeerChoking.Load() && !peer.Downloading.Load() {
			d.startPeerDownload(peer)
		}

		return true
	})
}

// peerReader reads messages from peer connection
func (d *Downloader) peerReader(peer *Peer) {
	defer close(peer.messages)

	// BEP 6 / BEP 3: send bitfield (or HaveAll/HaveNone for fast peers) as first message after handshake.
	if peer.fastEnabled && d.completed.Load() {
		// BEP 6: HaveAll replaces bitfield when we are a seed.
		if err := protocol.WriteMessage(peer.Conn, protocol.NewHaveAllMessage()); err != nil {
			log.Warn("failed to send have-all", "sub", "peer", "addr", peer.Addr, "err", err)
			close(peer.closeC)
			return
		}
	} else if peer.fastEnabled && d.countCompletedPieces() == 0 {
		// BEP 6: HaveNone replaces bitfield when we have nothing.
		if err := protocol.WriteMessage(peer.Conn, protocol.NewHaveNoneMessage()); err != nil {
			log.Warn("failed to send have-none", "sub", "peer", "addr", peer.Addr, "err", err)
			close(peer.closeC)
			return
		}
	} else {
		// BEP 3: send full bitfield.
		if err := protocol.WriteMessage(peer.Conn, protocol.NewBitfieldMessage(d.bitfield.bits)); err != nil {
			log.Warn("failed to send bitfield", "sub", "peer", "addr", peer.Addr, "err", err)
			close(peer.closeC)
			return
		}
	}

	// BEP 6: send allowed fast set so choked peer can bootstrap.
	if peer.fastEnabled {
		for idx := range peer.allowedFastOut {
			protocol.WriteMessage(peer.Conn, protocol.NewAllowedFastMessage(uint32(idx)))
		}
	}

	// BEP 3: send interested to indicate we want to download
	interestedMsg := protocol.NewInterestedMessage()
	if err := protocol.WriteMessage(peer.Conn, interestedMsg); err != nil {
		log.Warn("failed to send interested", "sub", "peer", "addr", peer.Addr, "err", err)
		close(peer.closeC)
		return
	}
	peer.AmInterested.Store(true)

	// BEP 10: send extension handshake declaring our local extension IDs.
	// BEP 11: ut_pex=1, BEP 9: ut_metadata=2, BEP 55: ut_holepunch=3.
	// BEP 27: do not advertise ut_pex for private torrents.
	if peer.extSupported {
		m := map[string]int{"ut_metadata": 2, "ut_holepunch": 3, "lt_donthave": 4}
		if !d.private {
			m["ut_pex"] = 1
		}
		extHS := &protocol.ExtensionHandshake{
			M: m,
		}
		if len(d.rawInfoDict) > 0 {
			extHS.MetadataSize = len(d.rawInfoDict)
		}
		payload, err := extHS.Serialize()
		if err == nil {
			extMsg := protocol.NewExtendedMessage(protocol.ExtHandshake, payload)
			if err := protocol.WriteMessage(peer.Conn, extMsg); err != nil {
				log.Warn("failed to send extension handshake", "sub", "peer", "addr", peer.Addr, "err", err)
			}
		}
	}

	// Read messages in loop
	for {
		msg, err := protocol.ReadMessage(peer.Conn, 120*time.Second)
		if err != nil {
			log.Debug("read error", "sub", "peer", "addr", peer.Addr, "err", err)
			close(peer.closeC)
			return
		}

		if msg == nil {
			// Keep-alive
			continue
		}

		// Send to main peer loop
		select {
		case peer.messages <- msg:
		case <-peer.closeC:
			return
		}
	}
}

// peerWriter writes messages to peer connection
func (d *Downloader) peerWriter(peer *Peer) {
	keepAliveTicker := time.NewTicker(60 * time.Second)
	defer keepAliveTicker.Stop()

	for {
		// Drain all pending outgoing messages (have, piece, ext) before new requests.
		drained := true
		for drained {
			select {
			case msg := <-peer.outgoing:
				if err := protocol.WriteMessage(peer.Conn, msg); err != nil {
					log.Warn("failed to send outgoing message", "sub", "peer", "addr", peer.Addr, "err", err)
					close(peer.closeC)
					return
				}
				if msg.ID == protocol.MsgPiece && len(msg.Payload) > 8 {
					n := int64(len(msg.Payload) - 8)
					d.uploaded.Add(uint64(n))
					peer.bytesToPeer.Add(n)
				}
			default:
				drained = false
			}
		}

		// Drain all pending cancels before sending any new requests.
		// This ensures the remote peer stops sending stale data after a seek.
		drained = true
		for drained {
			select {
			case cancel := <-peer.cancels:
				msg := protocol.NewCancelMessage(cancel.Index, cancel.Begin, cancel.Length)
				if err := protocol.WriteMessage(peer.Conn, msg); err != nil {
					log.Warn("failed to send cancel", "sub", "peer", "addr", peer.Addr, "err", err)
					close(peer.closeC)
					return
				}
			default:
				drained = false
			}
		}

		select {
		case <-peer.closeC:
			return

		case <-keepAliveTicker.C:
			// Send keep-alive
			if err := protocol.WriteMessage(peer.Conn, nil); err != nil {
				log.Warn("failed to send keep-alive", "sub", "peer", "addr", peer.Addr, "err", err)
				close(peer.closeC)
				return
			}

		case msg := <-peer.outgoing:
			if err := protocol.WriteMessage(peer.Conn, msg); err != nil {
				log.Warn("failed to send outgoing message", "sub", "peer", "addr", peer.Addr, "err", err)
				close(peer.closeC)
				return
			}
			if msg.ID == protocol.MsgPiece && len(msg.Payload) > 8 {
				n := int64(len(msg.Payload) - 8)
				d.uploaded.Add(uint64(n))
				peer.bytesToPeer.Add(n)
			}

		case cancel := <-peer.cancels:
			msg := protocol.NewCancelMessage(cancel.Index, cancel.Begin, cancel.Length)
			if err := protocol.WriteMessage(peer.Conn, msg); err != nil {
				log.Warn("failed to send cancel", "sub", "peer", "addr", peer.Addr, "err", err)
				close(peer.closeC)
				return
			}

		case req := <-peer.requests:
			// Send block request
			msg := protocol.NewRequestMessage(req.Index, req.Begin, req.Length)
			if err := protocol.WriteMessage(peer.Conn, msg); err != nil {
				log.Warn("failed to send request", "sub", "peer", "addr", peer.Addr, "err", err)
				close(peer.closeC)
				return
			}
		}
	}
}

// removePeer removes a peer from active peers
func (d *Downloader) removePeer(peer *Peer) {
	if peer.Downloading.Load() {
		currentPiece := peer.CurrentPiece.Load()
		if currentPiece != nil {
			log.Debug("disconnecting while downloading", "sub", "peer", "addr", peer.Addr, "piece", currentPiece.Index)
		}
	}

	// Always scrub all Requesting maps regardless of Downloading flag.
	// Endgame assigns peers to Requesting without setting Downloading=true,
	// and request-queue timeouts clear Downloading without calling AbortPiece,
	// so a conditional check misses stale entries that block endgame.
	d.piecePicker.AbortAllPieces(peer)
	peer.Downloading.Store(false)
	peer.CurrentPiece.Store(nil)

	d.peers.Delete(peer.Addr.String())
	d.activePeers.Add(-1)
	peer.Conn.Close()
	d.chkr.Trigger()
	// Wake idle peers — one of them will now pick up the freed piece.
	d.startIdlePeerDownloads()
}

// handlePeerMessage processes messages from peers (called from main event loop)
func (d *Downloader) handlePeerMessage(pm PeerMessage) {
	peer := pm.Peer
	msg, ok := pm.Message.(*protocol.Message)
	if !ok {
		log.Warn("invalid message type", "sub", "peer", "addr", peer.Addr)
		return
	}

	d.handleMessage(peer, msg)
}

// handlePieceData processes received piece data
func (d *Downloader) handlePieceData(pd PieceData) {
	piece := pd.Piece

	piece.Mu.Lock()

	// Skip if piece is already done or being written.
	// Critically, also free the sending peer so it doesn't stay stuck
	// with Downloading=true on a piece that will never complete here.
	if piece.Done.Load() || piece.Writing.Load() {
		piece.Mu.Unlock()
		if p := pd.Peer; p != nil && p.CurrentPiece.Load() == piece {
			d.piecePicker.AbortPiece(p, piece.Index)
			p.Downloading.Store(false)
			p.CurrentPiece.Store(nil)
			d.startIdlePeerDownloads()
		}
		return
	}

	// Copy data into piece buffer
	copy(piece.Buffer[pd.Begin:], pd.Data)
	piece.Blocks[pd.Begin] = true

	// Check if piece is complete
	numBlocks := (piece.Length + BlockSize - 1) / BlockSize
	if uint32(len(piece.Blocks)) == numBlocks {
		// Mark as writing to prevent duplicate processing
		piece.Writing.Store(true)
		piece.Mu.Unlock()

		// All blocks received - mark peers as idle and send for verification
		log.Debug("piece complete, verifying", "sub", "piece", "piece", piece.Index, "blocks", numBlocks)

		// Mark all peers downloading this piece as idle
		d.peers.Range(func(key, value interface{}) bool {
			peer := value.(*Peer)
			currentPiece := peer.CurrentPiece.Load()
			if currentPiece != nil && currentPiece.Index == piece.Index {
				peer.Downloading.Store(false)
				peer.CurrentPiece.Store(nil)
			}
			return true
		})

		select {
		case d.pieceComplete <- piece:
		default:
			log.Warn("completion channel full", "sub", "piece", "piece", piece.Index)
		}
	} else {
		piece.Mu.Unlock()
	}
}

// verifyAndWritePiece verifies hash and writes piece to disk
// OPTIMIZATION: Pause piece downloads during disk write to prevent memory bloat
func (d *Downloader) verifyAndWritePiece(piece *Piece) {
	// Suspend piece downloads
	d.pieceDataPausable.Suspend()
	defer d.pieceDataPausable.Resume()

	// BEP 3 §Pieces: verify SHA-1 hash of reassembled piece data
	hash := sha1.Sum(piece.Buffer)
	if hash != piece.Hash {
		log.Warn("piece hash mismatch", "sub", "piece", "piece", piece.Index, "expected", fmt.Sprintf("%x", piece.Hash), "got", fmt.Sprintf("%x", hash))

		// Reset piece for re-download
		piece.Mu.Lock()
		piece.Blocks = make(map[uint32]bool)
		piece.Writing.Store(false) // CRITICAL: Reset Writing flag so piece can be re-requested
		piece.Mu.Unlock()

		// Trigger reassignment since we have idle peers
		d.startIdlePeerDownloads()
		return
	}

	// Check if already marked as done (shouldn't happen but safety check).
	alreadyDone := piece.Done.Load()
	if alreadyDone {
		piece.Writing.Store(false)
		log.Debug("piece already done, skipping", "sub", "piece", "piece", piece.Index)
		return
	}

	// Write to disk
	if err := d.writePieceToDisk(piece); err != nil {
		log.Error("failed to write piece to disk", "sub", "piece", "piece", piece.Index, "err", err)
		// Reset piece for re-download
		piece.Mu.Lock()
		piece.Blocks = make(map[uint32]bool)
		piece.Writing.Store(false)
		piece.Mu.Unlock()
		return
	}

	// Mark as complete and update progress
	piece.Done.Store(true)
	piece.Writing.Store(false)

	d.bitfield.Set(piece.Index)
	d.downloaded.Add(uint64(piece.Length))
	d.broadcastHave(piece.Index)
	d.piecePicker.urgentPieces.Delete(piece.Index)

	log.Debug("piece verified and saved", "sub", "piece", "piece", piece.Index, "completed", d.countCompletedPieces(), "total", d.numPieces)

	// Check if download is complete
	if d.countCompletedPieces() == d.numPieces {
		// Set completed flag atomically (only first completion matters)
		if !d.completed.Swap(true) {
			elapsed := time.Since(d.startTime)
			avgSpeed := float64(d.downloaded.Load()) / elapsed.Seconds() / (1024 * 1024)
			log.Info("download complete", "sub", "download",
				"bytes", d.downloaded.Load(),
				"MB", fmt.Sprintf("%.2f", float64(d.downloaded.Load())/(1024*1024)),
				"speed", fmt.Sprintf("%.2f MB/s", avgSpeed),
				"elapsed", elapsed.Round(time.Second))
		}
		// Don't assign new pieces after completion
		return
	}

	// CRITICAL: Assign new pieces to idle peers after piece completion
	d.startIdlePeerDownloads()
}

// writePieceToDisk writes a completed piece to the output file
func (d *Downloader) writePieceToDisk(piece *Piece) error {
	// Calculate global offset for this piece
	pieceOffset := int64(piece.Index) * int64(d.pieceSize)
	pieceData := piece.Buffer[:piece.Length]

	// Single-file torrent: write directly to output path
	if len(d.files) == 0 {
		return d.writeSingleFile(pieceOffset, pieceData)
	}

	// Multi-file torrent: split piece across file boundaries
	return d.writeMultiFile(pieceOffset, pieceData)
}

// writeSingleFile writes piece data to a single output file
func (d *Downloader) writeSingleFile(offset int64, data []byte) error {
	f, err := os.OpenFile(d.outputPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open output file: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(offset, 0); err != nil {
		return fmt.Errorf("failed to seek: %w", err)
	}

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("failed to write: %w", err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("failed to sync: %w", err)
	}

	return nil
}

// writeMultiFile writes piece data across multiple files respecting file boundaries
func (d *Downloader) writeMultiFile(pieceOffset int64, pieceData []byte) error {
	bytesWritten := 0
	dataLen := len(pieceData)

	for bytesWritten < dataLen {
		// Find which file this offset belongs to
		fileIdx := -1
		var fileOffset int64

		for i, file := range d.files {
			fileEnd := file.Offset + file.Length
			if pieceOffset+int64(bytesWritten) >= file.Offset && pieceOffset+int64(bytesWritten) < fileEnd {
				fileIdx = i
				fileOffset = pieceOffset + int64(bytesWritten) - file.Offset
				break
			}
		}

		if fileIdx == -1 {
			return fmt.Errorf("piece offset %d not found in any file", pieceOffset+int64(bytesWritten))
		}

		file := d.files[fileIdx]

		// Calculate how many bytes to write to this file
		remainingInFile := file.Length - fileOffset
		remainingInPiece := int64(dataLen - bytesWritten)
		writeSize := remainingInFile
		if remainingInPiece < writeSize {
			writeSize = remainingInPiece
		}

		// Construct file path
		filePath := filepath.Join(append([]string{d.outputPath}, file.Path...)...)

		// Create directory if needed
		dir := filepath.Dir(filePath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}

		// Open/create file
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", filePath, err)
		}

		// Seek to position
		if _, err := f.Seek(fileOffset, 0); err != nil {
			f.Close()
			return fmt.Errorf("failed to seek in %s: %w", filePath, err)
		}

		// Write data
		chunk := pieceData[bytesWritten : bytesWritten+int(writeSize)]
		if _, err := f.Write(chunk); err != nil {
			f.Close()
			return fmt.Errorf("failed to write to %s: %w", filePath, err)
		}

		// Sync
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("failed to sync %s: %w", filePath, err)
		}

		f.Close()

		bytesWritten += int(writeSize)
	}

	return nil
}

// readBlock reads a block from the completed torrent data (used for seeding).
func (d *Downloader) readBlock(index, begin, length uint32) ([]byte, error) {
	globalOffset := int64(index)*int64(d.pieceSize) + int64(begin)
	buf := make([]byte, length)

	if len(d.files) == 0 {
		// Single-file torrent.
		f, err := os.Open(d.outputPath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		_, err = f.ReadAt(buf, globalOffset)
		return buf, err
	}

	// Multi-file torrent: walk file list, same offset math as writeMultiFile.
	bytesRead := 0
	for bytesRead < int(length) {
		cur := globalOffset + int64(bytesRead)
		found := false
		for _, fi := range d.files {
			if cur < fi.Offset || cur >= fi.Offset+fi.Length {
				continue
			}
			fileOff := cur - fi.Offset
			toRead := int(fi.Length - fileOff)
			if toRead > int(length)-bytesRead {
				toRead = int(length) - bytesRead
			}
			path := filepath.Join(append([]string{d.outputPath}, fi.Path...)...)
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			_, err = f.ReadAt(buf[bytesRead:bytesRead+toRead], fileOff)
			f.Close()
			if err != nil {
				return nil, err
			}
			bytesRead += toRead
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("readBlock: offset %d not found in any file", cur)
		}
	}
	return buf, nil
}

// countCompletedPieces counts completed pieces
func (d *Downloader) countCompletedPieces() uint32 {
	var count uint32
	for _, p := range d.pieces {
		if p.Done.Load() {
			count++
		}
	}
	return count
}

// CountCompletedPieces returns the number of completed pieces (public accessor)
func (d *Downloader) CountCompletedPieces() uint32 {
	return d.countCompletedPieces()
}

// NumPieces returns the total number of pieces
func (d *Downloader) NumPieces() uint32 {
	return d.numPieces
}

// PiecePicker returns the piece picker (for streaming support)
func (d *Downloader) PiecePicker() *PiecePicker {
	return d.piecePicker
}

// PeerStats returns (total, unchoked, idle) peer counts for diagnostics.
func (d *Downloader) PeerStats() (total, unchoked, idle int) {
	d.peers.Range(func(_, v any) bool {
		p := v.(*Peer)
		total++
		if !p.PeerChoking.Load() {
			unchoked++
			if !p.Downloading.Load() {
				idle++
			}
		}
		return true
	})
	return
}

// broadcastHave sends a BEP 3 MsgHave for pieceIndex to all connected peers.
// Called AFTER bitfield.Set() and downloaded.Add() in verifyAndWritePiece.
func (d *Downloader) broadcastHave(pieceIndex uint32) {
	msg := protocol.NewHaveMessage(pieceIndex)
	d.peers.Range(func(_, v any) bool {
		p := v.(*Peer)
		select {
		case p.outgoing <- msg:
		default:
		}
		return true
	})
}

// serveMetadataBlock sends a single BEP 9 ut_metadata block (or Reject) to peer.
// BEP 9: piece is 0-indexed; each block is MetadataBlockSize (16384) bytes.
func (d *Downloader) serveMetadataBlock(peer *Peer, piece int) {
	peerMetaID := uint8(peer.peerMetadataID.Load())
	if peerMetaID == 0 {
		return // peer hasn't declared ut_metadata — nowhere to send the reply
	}
	raw := d.rawInfoDict
	const blockSize = protocol.MetadataBlockSize
	numBlocks := (len(raw) + blockSize - 1) / blockSize
	if piece < 0 || piece >= numBlocks {
		header, err := bencode.EncodeBytes(protocol.MetadataMessage{
			MsgType: protocol.MetadataReject,
			Piece:   piece,
		})
		if err != nil {
			return
		}
		select {
		case peer.outgoing <- protocol.NewExtendedMessage(peerMetaID, header):
		default:
		}
		return
	}
	start := piece * blockSize
	end := start + blockSize
	if end > len(raw) {
		end = len(raw)
	}
	header, err := bencode.EncodeBytes(protocol.MetadataMessage{
		MsgType:   protocol.MetadataData,
		Piece:     piece,
		TotalSize: len(raw),
	})
	if err != nil {
		return
	}
	payload := append(header, raw[start:end]...)
	select {
	case peer.outgoing <- protocol.NewExtendedMessage(peerMetaID, payload):
	default:
	}
}

// sendPEX builds and sends a BEP 11 PEX message to peer (rate-limited by pexState).
func (d *Downloader) sendPEX(peer *Peer) {
	current := d.connectedPeerInfos(peer)
	msg, _ := peer.pexState.BuildMessage(current)
	if msg == nil {
		return
	}
	data, err := pex.Encode(msg)
	if err != nil {
		return
	}
	// We advertised ut_pex as local ID 1 in our extension handshake.
	extMsg := protocol.NewExtendedMessage(1, data)
	select {
	case peer.outgoing <- extMsg:
	default:
	}
}

// connectedPeerInfos returns PeerInfo for all peers except exclude.
func (d *Downloader) connectedPeerInfos(exclude *Peer) []pex.PeerInfo {
	var out []pex.PeerInfo
	d.peers.Range(func(_, v any) bool {
		p := v.(*Peer)
		if p == exclude {
			return true
		}
		var flags byte
		if p.Bitfield.AllSet(d.numPieces) {
			flags |= pex.FlagSeed
		}
		if p.peerHolepunchID.Load() != 0 {
			flags |= pex.FlagHolepunch
		}
		out = append(out, pex.PeerInfo{Addr: p.Addr, Flags: flags})
		return true
	})
	return out
}

// sendChokeMsg sends a MsgChoke or MsgUnchoke wire message to peer.
// Called by the choker's SetChoked callback outside the choker's mutex.
func (d *Downloader) sendChokeMsg(peer *Peer, choked bool) error {
	var msg *protocol.Message
	if choked {
		msg = protocol.NewChokeMessage()
	} else {
		msg = protocol.NewUnchokeMessage()
	}
	peer.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := protocol.WriteMessage(peer.Conn, msg)
	peer.Conn.SetWriteDeadline(time.Time{})
	return err
}

// ratioMet reports whether the share ratio target has been reached.
// Always false until download is complete. Returns true immediately for ratio=0.
func (d *Downloader) ratioMet() bool {
	if math.IsInf(d.shareRatio, 1) {
		return false // seed forever
	}
	dl := float64(d.downloaded.Load())
	if dl == 0 {
		return false
	}
	return float64(d.uploaded.Load())/dl >= d.shareRatio
}

// updateSpeed calculates current download speed
func (d *Downloader) updateSpeed() {
	// Speed = bytes received from peers since last tick (1 second interval).
	current := d.downloaded.Load()
	speed := current - d.lastDownloaded
	d.lastDownloaded = current
	d.downloadSpeed.Store(speed)

	// Count peers by state
	var unchoked, downloading int32
	d.peers.Range(func(key, value interface{}) bool {
		peer := value.(*Peer)
		if !peer.PeerChoking.Load() {
			unchoked++
		}
		if peer.Downloading.Load() {
			downloading++
		}
		return true
	})

	completed := d.countCompletedPieces()
	progress := float64(completed) / float64(d.numPieces) * 100

	// Throttle stats output: every tick while downloading, every 30s once complete.
	if d.completed.Load() {
		d.statsTicks++
		if d.statsTicks%30 != 1 {
			return
		}
	}

	if speed > 0 {
		remaining := (d.numPieces - completed) * d.pieceSize
		eta := time.Duration(uint64(remaining)/speed) * time.Second
		log.Info("stats", "sub", "stats",
			"strategy", d.strategy.String(),
			"peers", d.activePeers.Load(),
			"unchoked", unchoked,
			"downloading", downloading,
			"speed_mbps", fmt.Sprintf("%.2f", float64(speed)/(1024*1024)),
			"completed", completed,
			"total", d.numPieces,
			"progress", fmt.Sprintf("%.1f%%", progress),
			"eta", eta.Round(time.Second))
	} else {
		log.Info("stats", "sub", "stats",
			"strategy", d.strategy.String(),
			"peers", d.activePeers.Load(),
			"unchoked", unchoked,
			"downloading", downloading,
			"completed", completed,
			"total", d.numPieces,
			"progress", fmt.Sprintf("%.1f%%", progress))
	}
}

// AddPeers adds new peer addresses from tracker/DHT
func (d *Downloader) AddPeers(addrs []*net.TCPAddr) {
	select {
	case d.newPeerAddrs <- addrs:
		log.Debug("AddPeers queued", "sub", "download", "count", len(addrs), "backlog", len(d.newPeerAddrs))
	default:
		log.Warn("AddPeers channel full, dropping", "sub", "download", "cap", cap(d.newPeerAddrs), "dropped", len(addrs))
	}
}

// settleDelay is how long RequestSeek waits after the last seek before
// committing via JumpToOffset.  800ms absorbs VLC scrubbing (200-500ms
// between seeks) while still feeling responsive for single clicks.
const settleDelay = 800 * time.Millisecond

// RequestSeek is the debounced entry point for seek operations.
// It immediately updates priorityPiece (so PickPiece targets the new position)
// but defers the expensive JumpToOffset (peer aborts) until the seek position
// stabilises.  Safe to call rapidly from VLC scrubbing.
func (d *Downloader) RequestSeek(pieceIndex uint32) {
	d.seekMu.Lock()
	d.seekTarget = pieceIndex
	if d.seekTimer != nil {
		d.seekTimer.Reset(settleDelay)
	} else {
		d.seekTimer = time.AfterFunc(settleDelay, d.settleSeek)
	}
	d.seekMu.Unlock()

	// Immediately steer new PickPiece calls to the latest target.
	// No peer aborts — in-flight downloads continue undisturbed.
	d.piecePicker.SetPriorityPiece(pieceIndex)
	log.Info("RequestSeek", "sub", "seek", "piece", pieceIndex)
}

// settleSeek is the timer callback that fires after settleDelay with no new
// seeks.  It commits the debounced position via JumpToOffset.
func (d *Downloader) settleSeek() {
	d.seekMu.Lock()
	target := d.seekTarget
	d.seekTimer = nil
	d.seekMu.Unlock()

	// Guard against firing after Close().
	select {
	case <-d.closeC:
		return
	default:
	}

	log.Info("settleSeek", "sub", "seek", "piece", target)
	d.JumpToOffset(int64(target) * int64(d.pieceSize))
}

// JumpToOffset aborts any peer downloading outside the new readahead window
// and redirects all freed peers to start fetching from targetPiece.
// Handles both forward seeks (abort stale/behind peers) and backward seeks
// (abort ahead peers so they can immediately serve the new position).
func (d *Downloader) JumpToOffset(byteOffset int64) {
	targetPiece := uint32(byteOffset / int64(d.pieceSize))
	if targetPiece >= d.numPieces {
		targetPiece = d.numPieces - 1
	}

	log.Info("JumpToOffset", "sub", "seek", "byte", byteOffset, "piece", targetPiece, "total", d.numPieces)

	// Update picker so the next PickPiece call uses the new window.
	d.piecePicker.SetPriorityPiece(targetPiece)

	// Abort peers downloading pieces outside the new readahead window
	// [targetPiece, targetPiece+readaheadWindow).  This covers:
	//   • Forward seek: abort peers that are behind the new start.
	//   • Backward seek: abort peers that are ahead of the window
	//     so they immediately get reassigned to the sought position.
	const readaheadWindow = uint32(30)
	windowEnd := targetPiece + readaheadWindow

	d.peers.Range(func(_, v any) bool {
		peer := v.(*Peer)
		current := peer.CurrentPiece.Load()
		if current == nil {
			return true // already idle
		}
		if current.Index >= targetPiece && current.Index < windowEnd {
			return true // within the new window — keep going
		}
		log.Info("aborting peer outside window", "sub", "seek", "addr", peer.Addr, "piece", current.Index, "window_start", targetPiece, "window_end", windowEnd)
		d.piecePicker.AbortPiece(peer, current.Index)
		peer.Downloading.Store(false)
		peer.CurrentPiece.Store(nil)
		// Drain queued block requests so peerWriter stops sending stale requests.
		// Collect drained requests so we can cancel them on the remote peer.
		var staleRequests []BlockRequest
		for len(peer.requests) > 0 {
			select {
			case req := <-peer.requests:
				staleRequests = append(staleRequests, req)
			default:
			}
		}
		// Also enqueue cancels for all blocks of the aborted piece that the
		// remote peer already received (they're in-flight on the wire).
		numBlocks := (current.Length + BlockSize - 1) / BlockSize
		for i := uint32(0); i < numBlocks; i++ {
			blockSize := uint32(BlockSize)
			if i == numBlocks-1 {
				blockSize = current.Length - (i * BlockSize)
			}
			cancel := BlockRequest{
				Index:  current.Index,
				Begin:  i * BlockSize,
				Length: blockSize,
			}
			// Skip if already in staleRequests (not yet sent to remote)
			alreadyDrained := false
			for _, s := range staleRequests {
				if s == cancel {
					alreadyDrained = true
					break
				}
			}
			if !alreadyDrained {
				select {
				case peer.cancels <- cancel:
				default:
					// cancels channel full — best-effort
				}
			}
		}
		return true
	})

	// Wake all idle (and newly freed) peers so they pick up the target piece.
	d.startIdlePeerDownloads()
}

// disconnectSnubbedPeers walks all active peers and disconnects any that have
// been assigned a piece but haven't received a block within SnubTimeout.
// This prevents slow/uncooperative peers from hogging pieces indefinitely.
func (d *Downloader) disconnectSnubbedPeers() {
	now := time.Now().UnixNano()
	d.peers.Range(func(_, v any) bool {
		peer := v.(*Peer)
		if !peer.Downloading.Load() {
			return true
		}
		// Use the most recent of downloadStart and lastPiece as the activity baseline.
		startNano := peer.downloadStartNano.Load()
		if startNano == 0 {
			return true
		}
		lastBlock := peer.lastPieceNano.Load()
		baseline := startNano
		if lastBlock > baseline {
			baseline = lastBlock
		}
		idle := time.Duration(now - baseline)
		if idle > SnubTimeout {
			cp := peer.CurrentPiece.Load()
			pieceIdx := uint32(0)
			if cp != nil {
				pieceIdx = cp.Index
			}
			log.Debug("snub disconnecting stalled peer", "sub", "peer", "addr", peer.Addr, "piece", pieceIdx, "idle", idle.Round(time.Second))
			peer.Conn.Close()
		}
		return true
	})
}

// ForceReassignPiece disconnects all peers currently stuck on the given piece.
// Used by the streaming layer when a piece has been stuck for too long (e.g.
// a peer stalled on the priority piece while other pieces keep completing).
//
// We close the network connection rather than just abort+reassign because an
// aborted-but-idle peer would immediately be reassigned to the same priority
// piece by startIdlePeerDownloads.  Closing the connection triggers the normal
// peerReader→closeC→runPeer→removePeer cleanup path, which scrubs the
// Requesting map and wakes idle peers to take over.
func (d *Downloader) ForceReassignPiece(pieceIndex uint32) {
	picker := d.piecePicker
	if picker == nil {
		return
	}
	pa := picker.PieceByIndex(pieceIndex)
	if pa == nil || pa.Piece.Done.Load() {
		return
	}

	// Collect peers to disconnect first — closing inside Range is safe but
	// collecting is clearer about intent.
	var toDisconnect []*Peer
	pa.Requesting.Range(func(key, _ interface{}) bool {
		peer, ok := key.(*Peer)
		if !ok {
			return true
		}
		current := peer.CurrentPiece.Load()
		if current == nil || current.Index != pieceIndex {
			pa.Requesting.Delete(peer)
			return true
		}
		toDisconnect = append(toDisconnect, peer)
		return true
	})

	for _, peer := range toDisconnect {
		log.Debug("disconnecting peer stuck on piece", "sub", "stream", "addr", peer.Addr, "piece", pieceIndex)
		// Close the network connection.  This triggers peerReader to exit with
		// a read error, which closes peer.closeC, which causes runPeer to return
		// and its deferred removePeer to clean up Requesting maps + wake idle peers.
		peer.Conn.Close()
	}
}

// SetFileRange restricts the piece picker to only fetch pieces that cover
// [startByte, endByte) within the torrent.  This prevents downloading pieces
// that belong to other files in a multi-file torrent.
// Call this as soon as an HTTP request arrives for a specific file; it also
// sets the priority piece to the file's first piece so downloading begins
// immediately (no need to call JumpToOffset separately for the initial seek).
func (d *Downloader) SetFileRange(startByte, endByte int64) {
	if startByte < 0 {
		startByte = 0
	}
	pieceSize := int64(d.pieceSize)
	startPiece := uint32(startByte / pieceSize)
	endPiece := uint32((endByte - 1) / pieceSize)
	if endPiece >= d.numPieces {
		endPiece = d.numPieces - 1
	}
	log.Debug("SetFileRange", "sub", "stream", "startByte", startByte, "endByte", endByte, "startPiece", startPiece, "endPiece", endPiece)
	d.piecePicker.SetPieceRange(startPiece, endPiece)

	// Mark bookend pieces (first N + last N of the file range) as urgent.
	// This ensures container headers (MP4 moov, MKV EBML) and seek tables
	// (MKV Cues) are fetched immediately alongside the main readahead window.
	n := uint32(d.bookendPieces)
	for i := uint32(0); i < n && startPiece+i <= endPiece; i++ {
		d.piecePicker.urgentPieces.Store(startPiece+i, true)
	}
	for i := uint32(0); i < n && endPiece-i >= startPiece; i++ {
		d.piecePicker.urgentPieces.Store(endPiece-i, true)
	}
	log.Info("bookend urgent", "sub", "seek", "first", startPiece, "last", endPiece, "n", n)

	// Wake idle peers so they start fetching the first piece immediately.
	d.startIdlePeerDownloads()
}

// RegisterWaiter records that an HTTP streaming goroutine needs pieceIndex.
// The downloader focuses on min(all registered pieces) so a VLC end-of-file
// probe never starves the main stream.  Call with defer UnregisterWaiter.
func (d *Downloader) RegisterWaiter(pieceIndex uint32) {
	d.waitersMu.Lock()
	d.waiters[pieceIndex]++
	d.waitersMu.Unlock()
	d.updatePriorityFromWaiters()
}

// UnregisterWaiter removes one goroutine's claim on pieceIndex.
func (d *Downloader) UnregisterWaiter(pieceIndex uint32) {
	d.waitersMu.Lock()
	d.waiters[pieceIndex]--
	if d.waiters[pieceIndex] <= 0 {
		delete(d.waiters, pieceIndex)
	}
	d.waitersMu.Unlock()
	d.updatePriorityFromWaiters()
}

// tailPieceThreshold is the number of pieces at the end of the file that
// are treated as "tail" reads (e.g. MKV Cues).  Tail waiters are marked
// as urgent instead of changing the main priority, preventing ping-pong.
const tailPieceThreshold = uint32(3)

// updatePriorityFromWaiters recomputes the minimum-needed piece across all
// active waiters.  Tail pieces (last 3 of the torrent) are added to
// urgentPieces instead of redirecting priority.  Primary waiters go through
// the seek debouncer (RequestSeek) instead of calling JumpToOffset directly.
func (d *Downloader) updatePriorityFromWaiters() {
	d.waitersMu.Lock()
	primary := uint32(math.MaxUint32)
	tailStart := uint32(0)
	if d.numPieces > tailPieceThreshold {
		tailStart = d.numPieces - tailPieceThreshold
	}
	for piece := range d.waiters {
		if piece >= tailStart {
			// Tail piece (e.g. MKV Cues) — mark as urgent, don't shift priority.
			d.piecePicker.urgentPieces.Store(piece, true)
			log.Debug("tail waiter → urgent", "sub", "seek", "piece", piece)
		} else if piece < primary {
			primary = piece
		}
	}
	d.waitersMu.Unlock()

	if primary == math.MaxUint32 {
		return // only tail waiters (or none), no primary change needed
	}

	currentPriority := d.piecePicker.priorityPiece.Load()
	if primary == currentPriority {
		return // already focused on the right piece
	}

	log.Info("waiter priority update", "sub", "seek", "piece", primary, "was", currentPriority)
	d.RequestSeek(primary)
}

// Close shuts down the downloader and its background goroutines.
func (d *Downloader) Close() {
	// Stop seek debounce timer to prevent goroutine leak.
	d.seekMu.Lock()
	if d.seekTimer != nil {
		d.seekTimer.Stop()
		d.seekTimer = nil
	}
	d.seekMu.Unlock()

	d.chkr.Stop()
	close(d.closeC)
	<-d.doneC
}

// ===================================================================
// acceptUTPPeers accepts incoming uTP connections and registers them as peers.
func (d *Downloader) acceptUTPPeers(ctx context.Context, l *protocol.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return // listener closed
		}
		go func(c net.Conn) {
			handshaked, extSupported, fastSupported, err := d.doHandshake(c)
			if err != nil {
				c.Close()
				return
			}
			// Remote UDP addr → pseudo-TCP addr for peer map key.
			udpAddr, ok := c.RemoteAddr().(*net.UDPAddr)
			if !ok {
				handshaked.Close()
				return
			}
			tcpAddr := &net.TCPAddr{IP: udpAddr.IP, Port: udpAddr.Port}
			peer := d.makePeer(tcpAddr, handshaked, extSupported, fastSupported)
			peer.UseUTP = true
			d.addPeer(peer)
			go d.runPeer(peer)
		}(conn)
	}
}

// BEP 55 — Holepunch Extension
// ===================================================================

// handleHolepunch dispatches an incoming ut_holepunch message.
// We may be acting as relay C (Rendezvous), target B (Connect), or receive an Error.
func (d *Downloader) handleHolepunch(from *Peer, payload []byte) {
	msg, err := punch.Decode(payload)
	if err != nil {
		return
	}
	switch msg.Type {
	case punch.MsgRendezvous:
		d.relayRendezvous(from, msg.Addr)
	case punch.MsgConnect:
		go d.attemptHolepunch(msg.Addr)
	case punch.MsgError:
		log.Warn("holepunch error from relay", "sub", "peer", "addr", from.Addr, "code", msg.ErrCode)
	}
}

// relayRendezvous handles a Rendezvous from initiator A: we are relay C and
// must forward Connect messages to both A and target B so they punch simultaneously.
func (d *Downloader) relayRendezvous(initiator *Peer, targetAddr *net.UDPAddr) {
	if targetAddr == nil {
		d.sendHolepunchMsg(initiator, &punch.Message{
			Type:    punch.MsgError,
			Addr:    &net.UDPAddr{IP: initiator.Addr.IP, Port: initiator.Addr.Port},
			ErrCode: punch.ErrInvalidAddr,
		})
		return
	}

	// Initiator == target?
	if initiator.Addr.IP.Equal(targetAddr.IP) && initiator.Addr.Port == targetAddr.Port {
		d.sendHolepunchMsg(initiator, &punch.Message{
			Type:    punch.MsgError,
			Addr:    targetAddr,
			ErrCode: punch.ErrNoSelf,
		})
		return
	}

	// Find target among connected peers.
	tcpTarget := &net.TCPAddr{IP: targetAddr.IP, Port: targetAddr.Port}
	v, ok := d.peers.Load(tcpTarget.String())
	if !ok {
		d.sendHolepunchMsg(initiator, &punch.Message{
			Type:    punch.MsgError,
			Addr:    targetAddr,
			ErrCode: punch.ErrNotConnected,
		})
		return
	}
	target := v.(*Peer)
	if target.peerHolepunchID.Load() == 0 {
		d.sendHolepunchMsg(initiator, &punch.Message{
			Type:    punch.MsgError,
			Addr:    targetAddr,
			ErrCode: punch.ErrNoSupport,
		})
		return
	}

	initiatorUDP := &net.UDPAddr{IP: initiator.Addr.IP, Port: initiator.Addr.Port}
	// Forward Connect{Addr: target} to initiator.
	d.sendHolepunchMsg(initiator, &punch.Message{Type: punch.MsgConnect, Addr: targetAddr})
	// Forward Connect{Addr: initiator} to target.
	d.sendHolepunchMsg(target, &punch.Message{Type: punch.MsgConnect, Addr: initiatorUDP})
}

// sendHolepunchMsg encodes and enqueues a ut_holepunch message to peer.
// Uses the peer's declared extension ID (drop-on-full, never blocks).
func (d *Downloader) sendHolepunchMsg(peer *Peer, msg *punch.Message) {
	peerHolepunchID := uint8(peer.peerHolepunchID.Load())
	if peerHolepunchID == 0 {
		return
	}
	data, err := punch.Encode(msg)
	if err != nil {
		return
	}
	select {
	case peer.outgoing <- protocol.NewExtendedMessage(peerHolepunchID, data):
	default:
	}
}

// attemptHolepunch performs a simultaneous connect after receiving a BEP 55
// Connect message.  We try uTP first (UDP doesn't need a 3-way handshake,
// making it more reliable for NAT traversal), then fall back to TCP.
func (d *Downloader) attemptHolepunch(addr *net.UDPAddr) {
	tcpAddr := &net.TCPAddr{IP: addr.IP, Port: addr.Port}
	if _, ok := d.peers.Load(tcpAddr.String()); ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), DialTimeout)
	defer cancel()

	// Try uTP first — UDP punches through most NATs more reliably than TCP SYN.
	utpAddr := fmt.Sprintf("%s:%d", addr.IP, addr.Port)
	if utpConn, err := protocol.DialContext(ctx, "udp", utpAddr); err == nil {
		if conn, extSupported, fastSupported, err := d.doHandshake(utpConn); err == nil {
			peer := d.makePeer(tcpAddr, conn, extSupported, fastSupported)
			peer.UseUTP = true
			d.addPeer(peer)
			go d.runPeer(peer)
			return
		} else {
			utpConn.Close()
		}
	}

	// Fall back to TCP.
	peer, err := d.dialPeerOptimized(tcpAddr)
	if err != nil {
		log.Debug("holepunch connect failed", "sub", "peer", "addr", tcpAddr, "err", err)
		return
	}
	d.addPeer(peer)
	go d.runPeer(peer)
}

// tryHolepunchViaRelay sends a Rendezvous to a connected relay-capable peer,
// asking it to forward a Connect to target so both sides can punch through NAT.
func (d *Downloader) tryHolepunchViaRelay(target *net.TCPAddr) {
	var relay *Peer
	d.peers.Range(func(_, v any) bool {
		p := v.(*Peer)
		if p.peerHolepunchID.Load() != 0 {
			relay = p
			return false
		}
		return true
	})
	if relay == nil {
		return
	}
	targetUDP := &net.UDPAddr{IP: target.IP, Port: target.Port}
	d.sendHolepunchMsg(relay, &punch.Message{Type: punch.MsgRendezvous, Addr: targetUDP})
}

// Helper to convert peer ID to binary
func decodePeerID(peerID string) (dht.Key, error) {
	var id dht.Key
	if len(peerID) != 20 {
		return id, fmt.Errorf("invalid peer ID length: %d", len(peerID))
	}
	copy(id[:], peerID)
	return id, nil
}

// decodeCompactPeers decodes BEP 23 compact peer format (6 bytes per peer: 4 IP + 2 port).
func decodeCompactPeers(data []byte) []*net.TCPAddr {
	if len(data)%6 != 0 {
		return nil
	}

	numPeers := len(data) / 6
	addrs := make([]*net.TCPAddr, 0, numPeers)

	for i := 0; i < numPeers; i++ {
		offset := i * 6
		ip := net.IPv4(data[offset], data[offset+1], data[offset+2], data[offset+3])
		port := binary.BigEndian.Uint16(data[offset+4 : offset+6])

		addrs = append(addrs, &net.TCPAddr{
			IP:   ip,
			Port: int(port),
		})
	}

	return addrs
}

// ===================================================================
// PUBLIC API
// ===================================================================

// Download downloads a complete torrent to the specified output path
// This is the main entry point for using bittorrent
//
// Features:
// - Resume support: automatically verifies and resumes partial downloads
// - First/last piece priority: downloads headers/footers first for streaming
// - Clean completion: stops at 100%, no corruption
// - Progress reporting: logs stats every second
//
// Example:
//
//	config := bittorrent.DownloaderConfig{
//	    InfoHash:    infoHash,
//	    PeerID:      peerID,
//	    PieceHashes: pieceHashes,
//	    OutputPath:  "/path/to/output.mp4",
//	}
//	peers := []*net.TCPAddr{...} // from tracker
//	err := bittorrent.Download(ctx, config, peers)
func Download(ctx context.Context, config DownloaderConfig, peers []*net.TCPAddr) error {
	downloader, err := NewDownloader(config)
	if err != nil {
		return fmt.Errorf("failed to create downloader: %w", err)
	}

	// Start downloader in background
	go func() {
		if err := downloader.Run(ctx); err != nil && err != context.Canceled {
			log.Error("download error", "sub", "download", "err", err)
		}
	}()

	// Add peers (triggers connection attempts)
	downloader.AddPeers(peers)

	// Wait for cancellation or the downloader stopping itself (e.g. ratio met).
	select {
	case <-ctx.Done():
	case <-downloader.doneC:
	}
	downloader.Close()

	// Check if completed successfully
	if downloader.completed.Load() {
		return nil
	}

	return ctx.Err()
}
