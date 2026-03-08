// Package pex implements Peer Exchange (PEX) as specified in BEP 11.
// https://www.bittorrent.org/beps/bep_0011.html
//
// PEX is a BEP 10 extension negotiated via the key "ut_pex" in the extension
// handshake.  Once negotiated, peers periodically send each other lists of
// peers they know about for the same torrent, giving the client a third peer
// source (alongside trackers and DHT).
//
// # Wire format
//
// Each ut_pex message payload is a bencoded dict with up to six byte-string fields:
//
//	d
//	  6:added    <n*6  bytes>  compact IPv4: 4-byte BE IP + 2-byte BE port
//	  7:added.f  <n    bytes>  flags, one byte per IPv4 peer in "added"
//	  6:added6   <n*18 bytes>  compact IPv6: 16-byte IP + 2-byte BE port
//	  8:added6.f <n    bytes>  flags, one byte per IPv6 peer in "added6"
//	  7:dropped  <m*6  bytes>  compact IPv4 peers no longer seen
//	  8:dropped6 <m*18 bytes>  compact IPv6 peers no longer seen
//	e
//
// Keys absent means no peers of that type.  "dropped" peers have no flags.
//
// # Peer flags (1 byte per peer in "added[6].f")
//
//	0x01  prefers encrypted connections (MSE/BEP 8)
//	0x02  seeder (has all pieces)
//	0x04  supports uTP (BEP 29)
//	0x08  supports holepunch (BEP 55)
//	0x10  outgoing connection (we dialled them)
//
// # Message cadence
//
// The first message sent to a new peer is a full snapshot of all currently
// connected peers.  Subsequent messages are diffs: only newly-connected peers
// appear in "added", only disconnected peers appear in "dropped".
// Clients MUST NOT send a PEX message more than once per 60 seconds.
package pex

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cbluth/bittorrent/pkg/bencode"
)

// ── Flags ─────────────────────────────────────────────────────────────────────

const (
	FlagEncryption byte = 0x01 // Peer prefers encrypted connections (MSE)
	FlagSeed       byte = 0x02 // Peer is a seeder
	FlagUTP        byte = 0x04 // Peer supports uTP (BEP 29)
	FlagHolepunch  byte = 0x08 // Peer supports holepunch (BEP 55)
	FlagOutgoing   byte = 0x10 // We initiated the connection to this peer
)

// ── Tunables ──────────────────────────────────────────────────────────────────

const (
	// MinInterval is the minimum time between PEX messages per peer connection.
	MinInterval = 60 * time.Second

	// MaxPeersPerMessage is the maximum number of peers in a single PEX message.
	// Split evenly between IPv4 and IPv6 when both are present.
	MaxPeersPerMessage = 50
)

// ── Core types ────────────────────────────────────────────────────────────────

// PeerInfo is a single peer entry as carried by PEX.
type PeerInfo struct {
	Addr  *net.TCPAddr
	Flags byte
}

// Message is a decoded ut_pex payload.
type Message struct {
	Added   []PeerInfo // peers seen since last message (or full snapshot on first send)
	Dropped []PeerInfo // peers no longer seen (empty in snapshot)
}

// wireMsg is the bencoded representation of a PEX message.
// The bencode encoder sorts struct fields by their tag name, which produces the
// correct lexicographic key order required by the bencode spec:
// added < added.f < added6 < added6.f < dropped < dropped6
type wireMsg struct {
	Added    []byte `bencode:"added,omitempty"`
	AddedF   []byte `bencode:"added.f,omitempty"`
	Added6   []byte `bencode:"added6,omitempty"`
	Added6F  []byte `bencode:"added6.f,omitempty"`
	Dropped  []byte `bencode:"dropped,omitempty"`
	Dropped6 []byte `bencode:"dropped6,omitempty"`
}

// ── Encode / Decode ───────────────────────────────────────────────────────────

// Encode serialises a Message into the bencoded wire format expected for a
// ut_pex extension message payload.
func Encode(msg *Message) ([]byte, error) {
	if msg == nil {
		return bencode.EncodeBytes(wireMsg{})
	}

	v4added, v4flags, v6added, v6flags := splitByFamily(msg.Added)
	v4dropped, _, v6dropped, _ := splitByFamily(msg.Dropped)

	w := wireMsg{
		Added:    packIPv4(v4added, v4flags),
		AddedF:   flagBytes(v4flags, v4added),
		Added6:   packIPv6(v6added, v6flags),
		Added6F:  flagBytes(v6flags, v6added),
		Dropped:  packIPv4(v4dropped, nil),
		Dropped6: packIPv6(v6dropped, nil),
	}
	return bencode.EncodeBytes(w)
}

// Decode parses a bencoded ut_pex payload into a Message.
// Unknown fields and malformed compact entries are silently skipped.
func Decode(data []byte) (*Message, error) {
	var w wireMsg
	if err := bencode.DecodeBytes(data, &w); err != nil {
		return nil, fmt.Errorf("pex: decode: %w", err)
	}

	added4, err := unpackIPv4(w.Added, w.AddedF)
	if err != nil {
		return nil, fmt.Errorf("pex: decode added: %w", err)
	}
	added6, err := unpackIPv6(w.Added6, w.Added6F)
	if err != nil {
		return nil, fmt.Errorf("pex: decode added6: %w", err)
	}
	dropped4, err := unpackIPv4(w.Dropped, nil)
	if err != nil {
		return nil, fmt.Errorf("pex: decode dropped: %w", err)
	}
	dropped6, err := unpackIPv6(w.Dropped6, nil)
	if err != nil {
		return nil, fmt.Errorf("pex: decode dropped6: %w", err)
	}

	return &Message{
		Added:   append(added4, added6...),
		Dropped: append(dropped4, dropped6...),
	}, nil
}

// ── State — per-peer-connection PEX tracking ──────────────────────────────────

// State tracks what we have already advertised to one specific peer connection
// so we can compute correct diffs for subsequent PEX messages.
//
// One State should be created per peer connection and discarded when the
// connection closes.  It is safe for concurrent use.
type State struct {
	mu          sync.Mutex
	announced   map[string]PeerInfo // addr.String() → last-advertised PeerInfo
	lastSent    time.Time
	initialized bool // false until the first (snapshot) message is sent
}

// NewState creates a fresh PEX State for a new peer connection.
func NewState() *State {
	return &State{
		announced: make(map[string]PeerInfo),
	}
}

// BuildMessage computes the next PEX Message to send to the remote peer, given
// the caller's current view of all connected peers (current).
//
// The first call returns a full snapshot (all current peers in Added, nothing in
// Dropped).  Subsequent calls return diffs relative to what was last announced.
//
// Returns (nil, nil) if the MinInterval has not elapsed since the last send.
// The caller should encode the returned Message with Encode and send it as a
// BEP 10 extension message with the negotiated ut_pex extension ID.
func (s *State) BuildMessage(current []PeerInfo) (*Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.initialized && time.Since(s.lastSent) < MinInterval {
		return nil, nil // rate-limited
	}

	// Build a fast lookup of current peers.
	currentMap := make(map[string]PeerInfo, len(current))
	for _, p := range current {
		if p.Addr == nil {
			continue
		}
		currentMap[p.Addr.String()] = p
	}

	var added, dropped []PeerInfo

	if !s.initialized {
		// Snapshot: everything in current goes into Added.
		for _, p := range current {
			if len(added) >= MaxPeersPerMessage {
				break
			}
			added = append(added, p)
		}
	} else {
		// Diff: added = current − announced, dropped = announced − current.
		for key, p := range currentMap {
			if _, exists := s.announced[key]; !exists {
				added = append(added, p)
				if len(added) >= MaxPeersPerMessage {
					break
				}
			}
		}
		for key, p := range s.announced {
			if _, exists := currentMap[key]; !exists {
				dropped = append(dropped, p)
				if len(dropped) >= MaxPeersPerMessage {
					break
				}
			}
		}
	}

	// Update s.announced to reflect what we are about to send.
	for _, p := range added {
		s.announced[p.Addr.String()] = p
	}
	for _, p := range dropped {
		delete(s.announced, p.Addr.String())
	}

	s.initialized = true
	s.lastSent = time.Now()

	if len(added) == 0 && len(dropped) == 0 {
		return nil, nil // nothing to send
	}
	return &Message{Added: added, Dropped: dropped}, nil
}

// ── Compact peer encoding ─────────────────────────────────────────────────────

// splitByFamily partitions peers into IPv4 and IPv6 slices.
// The returned flag slices parallel their peer slices.
func splitByFamily(peers []PeerInfo) (v4 []PeerInfo, v4f []byte, v6 []PeerInfo, v6f []byte) {
	for _, p := range peers {
		if p.Addr == nil {
			continue
		}
		if p.Addr.IP.To4() != nil {
			v4 = append(v4, p)
			v4f = append(v4f, p.Flags)
		} else {
			v6 = append(v6, p)
			v6f = append(v6f, p.Flags)
		}
	}
	return
}

// packIPv4 encodes IPv4 peers as a compact 6-byte-per-peer binary string.
// flags is parallel to peers; pass nil when encoding dropped peers (no flags needed).
func packIPv4(peers []PeerInfo, flags []byte) []byte {
	if len(peers) == 0 {
		return nil
	}
	out := make([]byte, 0, len(peers)*6)
	for _, p := range peers {
		ip4 := p.Addr.IP.To4()
		if ip4 == nil {
			continue
		}
		out = append(out, ip4...)
		out = appendPort(out, p.Addr.Port)
	}
	return out
}

// packIPv6 encodes IPv6 peers as a compact 18-byte-per-peer binary string.
func packIPv6(peers []PeerInfo, flags []byte) []byte {
	if len(peers) == 0 {
		return nil
	}
	out := make([]byte, 0, len(peers)*18)
	for _, p := range peers {
		if p.Addr.IP.To4() != nil {
			continue // skip IPv4-mapped addresses
		}
		ip6 := p.Addr.IP.To16()
		if ip6 == nil {
			continue
		}
		out = append(out, ip6...)
		out = appendPort(out, p.Addr.Port)
	}
	return out
}

// flagBytes returns flags only if peers is non-empty (omit otherwise).
func flagBytes(flags []byte, peers []PeerInfo) []byte {
	if len(peers) == 0 {
		return nil
	}
	return flags
}

// appendPort appends a big-endian 2-byte port to buf.
func appendPort(buf []byte, port int) []byte {
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	return append(buf, p[:]...)
}

// unpackIPv4 decodes a compact IPv4 binary string into []PeerInfo.
// flags is the parallel flags string (may be nil or shorter than peers).
func unpackIPv4(data, flags []byte) ([]PeerInfo, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if len(data)%6 != 0 {
		return nil, fmt.Errorf("compact IPv4 length %d not a multiple of 6", len(data))
	}
	peers := make([]PeerInfo, 0, len(data)/6)
	for i := 0; i+6 <= len(data); i += 6 {
		ip := make(net.IP, 4)
		copy(ip, data[i:i+4])
		port := binary.BigEndian.Uint16(data[i+4 : i+6])
		var flag byte
		if idx := i / 6; idx < len(flags) {
			flag = flags[idx]
		}
		peers = append(peers, PeerInfo{
			Addr:  &net.TCPAddr{IP: ip, Port: int(port)},
			Flags: flag,
		})
	}
	return peers, nil
}

// unpackIPv6 decodes a compact IPv6 binary string into []PeerInfo.
func unpackIPv6(data, flags []byte) ([]PeerInfo, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if len(data)%18 != 0 {
		return nil, fmt.Errorf("compact IPv6 length %d not a multiple of 18", len(data))
	}
	peers := make([]PeerInfo, 0, len(data)/18)
	for i := 0; i+18 <= len(data); i += 18 {
		ip := make(net.IP, 16)
		copy(ip, data[i:i+16])
		port := binary.BigEndian.Uint16(data[i+16 : i+18])
		var flag byte
		if idx := i / 18; idx < len(flags) {
			flag = flags[idx]
		}
		peers = append(peers, PeerInfo{
			Addr:  &net.TCPAddr{IP: ip, Port: int(port)},
			Flags: flag,
		})
	}
	return peers, nil
}
