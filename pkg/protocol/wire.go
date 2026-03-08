package protocol

// BEP 3: The BitTorrent Protocol Specification
// BEP 10: Extension Protocol
// https://www.bittorrent.org/beps/bep_0003.html
// https://www.bittorrent.org/beps/bep_0010.html
//
// This file implements the BitTorrent wire protocol:
// - Handshake message with extension protocol support
// - Message types: choke, unchoke, interested, have, bitfield, request, piece, cancel
// - Extension protocol message (BEP 10) for metadata exchange
// - Message encoding/decoding for peer communication

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/cbluth/bittorrent/pkg/dht"
)

// BEP 3 §Peer Messages — message IDs for the peer wire protocol.
// https://www.bittorrent.org/beps/bep_0003.html
const (
	MsgChoke         uint8 = 0  // BEP 3: choke — peer will not serve requests
	MsgUnchoke       uint8 = 1  // BEP 3: unchoke — peer will serve requests
	MsgInterested    uint8 = 2  // BEP 3: interested — wants pieces the peer has
	MsgNotInterested uint8 = 3  // BEP 3: not interested — no longer wants pieces
	MsgHave          uint8 = 4  // BEP 3: have <piece index> — completed & verified a piece
	MsgBitfield      uint8 = 5  // BEP 3: bitfield — sent as first message after handshake
	MsgRequest       uint8 = 6  // BEP 3: request <index><begin><length> — ask for a block
	MsgPiece         uint8 = 7  // BEP 3: piece <index><begin><block> — deliver a block
	MsgCancel        uint8 = 8  // BEP 3: cancel <index><begin><length> — cancel a pending request
	MsgPort          uint8 = 9  // BEP 5: port — advertise DHT listener port
	MsgSuggest       uint8 = 13 // BEP 6: suggest piece
	MsgHaveAll       uint8 = 14 // BEP 6: have all pieces
	MsgHaveNone      uint8 = 15 // BEP 6: have no pieces
	MsgReject        uint8 = 16 // BEP 6: reject request
	MsgAllowedFast   uint8 = 17 // BEP 6: allowed fast piece
	MsgExtended      uint8 = 20 // BEP 10: extension protocol message
)

// BEP 10 §Extension Messages — extension message sub-IDs.
const (
	ExtHandshake uint8 = 0 // BEP 10: extension handshake (ext_id=0, always)
)

// BEP 3 §Handshake — protocol constants.
const (
	ProtocolString = "BitTorrent protocol" // BEP 3: pstr = "BitTorrent protocol"
	HandshakeLen   = 68                    // BEP 3: 1 (pstrlen) + 19 (pstr) + 8 (reserved) + 20 (info_hash) + 20 (peer_id)
)

// BEP 10 — extension bits in the 8-byte handshake reserved field.
const (
	ExtensionBit = 0x100000 // BEP 10: bit 20 from right → reserved[5] & 0x10
)

// Message represents a BitTorrent protocol message
type Message struct {
	ID      uint8
	Payload []byte
}

// Handshake represents the BitTorrent handshake
type Handshake struct {
	Pstr     string
	Reserved [8]byte
	InfoHash dht.Key
	PeerID   dht.Key
}

// NewHandshake creates a handshake with extension protocol support.
// BEP 3 §Handshake: pstrlen(1) + pstr(19) + reserved(8) + info_hash(20) + peer_id(20) = 68 bytes.
func NewHandshake(infoHash, peerID dht.Key) *Handshake {
	h := &Handshake{
		Pstr:     ProtocolString,
		InfoHash: infoHash,
		PeerID:   peerID,
	}
	// BEP 10: set reserved bit 20 from right (byte 5, bit 4) to advertise extension protocol
	h.Reserved[5] |= 0x10
	// BEP 5 §DHT: set reserved bit 0 (byte 7, bit 0) to advertise DHT support
	h.Reserved[7] |= 0x01
	// BEP 6: set reserved bit 2 (byte 7, bit 2) to advertise Fast Extension
	h.Reserved[7] |= 0x04
	return h
}

// Serialize converts the handshake to wire format
func (h *Handshake) Serialize() []byte {
	buf := make([]byte, HandshakeLen)
	buf[0] = byte(len(h.Pstr))
	copy(buf[1:], h.Pstr)
	copy(buf[20:], h.Reserved[:])
	copy(buf[28:], h.InfoHash[:])
	copy(buf[48:], h.PeerID[:])
	return buf
}

// SupportsExtensions checks if the peer supports extension protocol (BEP 10).
func (h *Handshake) SupportsExtensions() bool {
	// BEP 10: check bit 20 from right (byte 5, bit 4)
	return (h.Reserved[5] & 0x10) != 0
}

// SupportsFast checks if the peer supports BEP 6 Fast Extension.
func (h *Handshake) SupportsFast() bool {
	return (h.Reserved[7] & 0x04) != 0
}

// ReadHandshake reads a handshake from a connection
func ReadHandshake(r io.Reader) (*Handshake, error) {
	buf := make([]byte, HandshakeLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("failed to read handshake: %w", err)
	}

	pstrLen := int(buf[0])
	if pstrLen != 19 {
		return nil, fmt.Errorf("invalid pstr length: %d", pstrLen)
	}

	h := &Handshake{
		Pstr: string(buf[1:20]),
	}
	copy(h.Reserved[:], buf[20:28])
	copy(h.InfoHash[:], buf[28:48])
	copy(h.PeerID[:], buf[48:68])

	if h.Pstr != ProtocolString {
		return nil, fmt.Errorf("invalid protocol string: %s", h.Pstr)
	}

	return h, nil
}

// ReadMessage reads a message from a connection
func ReadMessage(r io.Reader, timeout time.Duration) (*Message, error) {
	// Set read deadline
	if timeout > 0 {
		if conn, ok := r.(interface{ SetReadDeadline(time.Time) error }); ok {
			conn.SetReadDeadline(time.Now().Add(timeout))
		}
	}

	// Read 4-byte length prefix
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}

	length := binary.BigEndian.Uint32(lenBuf)
	if length == 0 {
		// BEP 3: keep-alive — length prefix of zero with no message ID or payload
		return nil, nil
	}

	if length > 1<<20 { // 1MB limit (increased from 128KB)
		return nil, fmt.Errorf("message too large: %d bytes", length)
	}

	// Read entire message (ID + payload) at once
	msgBuf := make([]byte, length)
	if _, err := io.ReadFull(r, msgBuf); err != nil {
		return nil, fmt.Errorf("failed to read message body: %w", err)
	}

	msg := &Message{
		ID: msgBuf[0],
	}

	// Payload is everything after the ID
	if length > 1 {
		msg.Payload = msgBuf[1:]
	}

	return msg, nil
}

// WriteMessage writes a message to a connection
func WriteMessage(w io.Writer, msg *Message) error {
	if msg == nil {
		// BEP 3: keep-alive — 4-byte zero length, no ID, no payload
		return binary.Write(w, binary.BigEndian, uint32(0))
	}

	length := uint32(1 + len(msg.Payload))
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, length)
	buf.WriteByte(msg.ID)
	buf.Write(msg.Payload)

	_, err := w.Write(buf.Bytes())
	return err
}

// NewExtendedMessage creates an extended protocol message
func NewExtendedMessage(extID uint8, payload []byte) *Message {
	buf := make([]byte, 1+len(payload))
	buf[0] = extID
	copy(buf[1:], payload)
	return &Message{
		ID:      MsgExtended,
		Payload: buf,
	}
}

// ParseExtendedMessage parses an extended message
func ParseExtendedMessage(msg *Message) (extID uint8, payload []byte, err error) {
	if msg.ID != MsgExtended {
		return 0, nil, fmt.Errorf("not an extended message: %d", msg.ID)
	}
	if len(msg.Payload) < 1 {
		return 0, nil, fmt.Errorf("extended message too short")
	}
	return msg.Payload[0], msg.Payload[1:], nil
}

// ── BEP 6 Fast Extension Message Types ───────────────────────────────────────

// SuggestMessage suggests a piece for the peer to download (BEP 6).
type SuggestMessage struct {
	PieceIndex uint32
}

// RejectMessage rejects a previously received request (BEP 6).
type RejectMessage struct {
	Index  uint32
	Begin  uint32
	Length uint32
}

// AllowedFastMessage declares a piece the choked peer may still request (BEP 6).
type AllowedFastMessage struct {
	PieceIndex uint32
}

// ── BEP 3 Peer Wire Messages for Piece Transfer ─────────────────────────────

// BitfieldMessage represents which pieces the peer has.
// BEP 3: sent as the very first message after handshake (if peer has any pieces).
// Each bit represents a piece index (1 = have, 0 = don't have).
// Bit ordering: high bit of first byte = piece 0, next bit = piece 1, etc.
type BitfieldMessage struct {
	Bitfield []byte // Length must be (num_pieces + 7) / 8 bytes
}

// HaveMessage announces that the peer completed and verified a piece.
// BEP 3: have <piece index> — payload is a single 4-byte big-endian uint32.
type HaveMessage struct {
	PieceIndex uint32 // BEP 3: zero-based piece index
}

// RequestMessage requests a block of a piece.
// BEP 3: request <index><begin><length> — all fields are 4-byte big-endian uint32.
// Length is typically 2^14 (16384 = 16 KiB) except for the last block in a piece.
type RequestMessage struct {
	Index  uint32 // BEP 3: zero-based piece index
	Begin  uint32 // BEP 3: byte offset within the piece
	Length uint32 // BEP 3: block size (typically 16384 bytes)
}

// PieceMessage delivers a block of a piece.
// BEP 3: piece <index><begin><block> — index and begin are 4-byte big-endian uint32.
type PieceMessage struct {
	Index uint32 // BEP 3: zero-based piece index
	Begin uint32 // BEP 3: byte offset within the piece
	Block []byte // BEP 3: raw piece data for this block
}

// CancelMessage cancels a pending request.
// BEP 3: cancel <index><begin><length> — identical payload format to request.
// Used during endgame mode when a block arrives from another peer.
type CancelMessage struct {
	Index  uint32 // BEP 3: piece index (matches original request)
	Begin  uint32 // BEP 3: byte offset (matches original request)
	Length uint32 // BEP 3: block size (matches original request)
}

// ----------------------
// Message Encoders (Create wire protocol messages)
// ----------------------

// NewChokeMessage creates a choke message
func NewChokeMessage() *Message {
	return &Message{ID: MsgChoke, Payload: nil}
}

// NewUnchokeMessage creates an unchoke message
func NewUnchokeMessage() *Message {
	return &Message{ID: MsgUnchoke, Payload: nil}
}

// NewInterestedMessage creates an interested message
func NewInterestedMessage() *Message {
	return &Message{ID: MsgInterested, Payload: nil}
}

// NewNotInterestedMessage creates a not interested message
func NewNotInterestedMessage() *Message {
	return &Message{ID: MsgNotInterested, Payload: nil}
}

// NewBitfieldMessage creates a bitfield message
func NewBitfieldMessage(bitfield []byte) *Message {
	return &Message{
		ID:      MsgBitfield,
		Payload: bitfield,
	}
}

// NewHaveMessage creates a have message
func NewHaveMessage(pieceIndex uint32) *Message {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, pieceIndex)
	return &Message{
		ID:      MsgHave,
		Payload: payload,
	}
}

// NewRequestMessage creates a request message
func NewRequestMessage(index, begin, length uint32) *Message {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], index)
	binary.BigEndian.PutUint32(payload[4:8], begin)
	binary.BigEndian.PutUint32(payload[8:12], length)
	return &Message{
		ID:      MsgRequest,
		Payload: payload,
	}
}

// NewPieceMessage creates a piece message
func NewPieceMessage(index, begin uint32, block []byte) *Message {
	payload := make([]byte, 8+len(block))
	binary.BigEndian.PutUint32(payload[0:4], index)
	binary.BigEndian.PutUint32(payload[4:8], begin)
	copy(payload[8:], block)
	return &Message{
		ID:      MsgPiece,
		Payload: payload,
	}
}

// NewCancelMessage creates a cancel message
func NewCancelMessage(index, begin, length uint32) *Message {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], index)
	binary.BigEndian.PutUint32(payload[4:8], begin)
	binary.BigEndian.PutUint32(payload[8:12], length)
	return &Message{
		ID:      MsgCancel,
		Payload: payload,
	}
}

// ── BEP 6 Fast Extension Encoders ────────────────────────────────────────────

// NewHaveAllMessage creates a HaveAll message (BEP 6: msg ID 14, no payload).
func NewHaveAllMessage() *Message {
	return &Message{ID: MsgHaveAll}
}

// NewHaveNoneMessage creates a HaveNone message (BEP 6: msg ID 15, no payload).
func NewHaveNoneMessage() *Message {
	return &Message{ID: MsgHaveNone}
}

// NewSuggestMessage creates a Suggest Piece message (BEP 6: msg ID 13, 4-byte index).
func NewSuggestMessage(pieceIndex uint32) *Message {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, pieceIndex)
	return &Message{ID: MsgSuggest, Payload: payload}
}

// NewRejectMessage creates a Reject Request message (BEP 6: msg ID 16, 12-byte payload).
func NewRejectMessage(index, begin, length uint32) *Message {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], index)
	binary.BigEndian.PutUint32(payload[4:8], begin)
	binary.BigEndian.PutUint32(payload[8:12], length)
	return &Message{ID: MsgReject, Payload: payload}
}

// NewAllowedFastMessage creates an Allowed Fast message (BEP 6: msg ID 17, 4-byte index).
func NewAllowedFastMessage(pieceIndex uint32) *Message {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, pieceIndex)
	return &Message{ID: MsgAllowedFast, Payload: payload}
}

// ----------------------
// Message Decoders (Parse wire protocol messages)
// ----------------------

// ParseBitfieldMessage parses a bitfield message
func ParseBitfieldMessage(msg *Message) (*BitfieldMessage, error) {
	if msg.ID != MsgBitfield {
		return nil, fmt.Errorf("not a bitfield message: %d", msg.ID)
	}
	return &BitfieldMessage{
		Bitfield: msg.Payload,
	}, nil
}

// ParseHaveMessage parses a have message
func ParseHaveMessage(msg *Message) (*HaveMessage, error) {
	if msg.ID != MsgHave {
		return nil, fmt.Errorf("not a have message: %d", msg.ID)
	}
	if len(msg.Payload) != 4 {
		return nil, fmt.Errorf("invalid have message length: %d", len(msg.Payload))
	}
	return &HaveMessage{
		PieceIndex: binary.BigEndian.Uint32(msg.Payload),
	}, nil
}

// ParseRequestMessage parses a request message
func ParseRequestMessage(msg *Message) (*RequestMessage, error) {
	if msg.ID != MsgRequest {
		return nil, fmt.Errorf("not a request message: %d", msg.ID)
	}
	if len(msg.Payload) != 12 {
		return nil, fmt.Errorf("invalid request message length: %d", len(msg.Payload))
	}
	return &RequestMessage{
		Index:  binary.BigEndian.Uint32(msg.Payload[0:4]),
		Begin:  binary.BigEndian.Uint32(msg.Payload[4:8]),
		Length: binary.BigEndian.Uint32(msg.Payload[8:12]),
	}, nil
}

// ParsePieceMessage parses a piece message
func ParsePieceMessage(msg *Message) (*PieceMessage, error) {
	if msg.ID != MsgPiece {
		return nil, fmt.Errorf("not a piece message: %d", msg.ID)
	}
	if len(msg.Payload) < 8 {
		return nil, fmt.Errorf("piece message too short: %d", len(msg.Payload))
	}
	return &PieceMessage{
		Index: binary.BigEndian.Uint32(msg.Payload[0:4]),
		Begin: binary.BigEndian.Uint32(msg.Payload[4:8]),
		Block: msg.Payload[8:],
	}, nil
}

// ParseCancelMessage parses a cancel message
func ParseCancelMessage(msg *Message) (*CancelMessage, error) {
	if msg.ID != MsgCancel {
		return nil, fmt.Errorf("not a cancel message: %d", msg.ID)
	}
	if len(msg.Payload) != 12 {
		return nil, fmt.Errorf("invalid cancel message length: %d", len(msg.Payload))
	}
	return &CancelMessage{
		Index:  binary.BigEndian.Uint32(msg.Payload[0:4]),
		Begin:  binary.BigEndian.Uint32(msg.Payload[4:8]),
		Length: binary.BigEndian.Uint32(msg.Payload[8:12]),
	}, nil
}

// ── BEP 6 Fast Extension Decoders ────────────────────────────────────────────

// ParseSuggestMessage parses a BEP 6 Suggest Piece message.
func ParseSuggestMessage(msg *Message) (*SuggestMessage, error) {
	if msg.ID != MsgSuggest {
		return nil, fmt.Errorf("not a suggest message: %d", msg.ID)
	}
	if len(msg.Payload) != 4 {
		return nil, fmt.Errorf("invalid suggest message length: %d", len(msg.Payload))
	}
	return &SuggestMessage{
		PieceIndex: binary.BigEndian.Uint32(msg.Payload),
	}, nil
}

// ParseRejectMessage parses a BEP 6 Reject Request message.
func ParseRejectMessage(msg *Message) (*RejectMessage, error) {
	if msg.ID != MsgReject {
		return nil, fmt.Errorf("not a reject message: %d", msg.ID)
	}
	if len(msg.Payload) != 12 {
		return nil, fmt.Errorf("invalid reject message length: %d", len(msg.Payload))
	}
	return &RejectMessage{
		Index:  binary.BigEndian.Uint32(msg.Payload[0:4]),
		Begin:  binary.BigEndian.Uint32(msg.Payload[4:8]),
		Length: binary.BigEndian.Uint32(msg.Payload[8:12]),
	}, nil
}

// ParseAllowedFastMessage parses a BEP 6 Allowed Fast message.
func ParseAllowedFastMessage(msg *Message) (*AllowedFastMessage, error) {
	if msg.ID != MsgAllowedFast {
		return nil, fmt.Errorf("not an allowed fast message: %d", msg.ID)
	}
	if len(msg.Payload) != 4 {
		return nil, fmt.Errorf("invalid allowed fast message length: %d", len(msg.Payload))
	}
	return &AllowedFastMessage{
		PieceIndex: binary.BigEndian.Uint32(msg.Payload),
	}, nil
}

// ── BEP 3 Bitfield Utilities ─────────────────────────────────────────────────
//
// BEP 3: The bitfield message payload has ceil(numPieces/8) bytes.
// Bit at index 0 is the high bit of the first byte. Spare bits at
// the end must be zero.

// NewBitfield creates a bitfield for the given number of pieces (BEP 3).
func NewBitfield(numPieces int) []byte {
	numBytes := (numPieces + 7) / 8
	return make([]byte, numBytes)
}

// SetPiece sets a piece as present in the bitfield.
// BEP 3: high bit of first byte = piece 0 (bit ordering).
func SetPiece(bitfield []byte, pieceIndex int) {
	byteIndex := pieceIndex / 8
	bitIndex := uint(7 - (pieceIndex % 8)) // BEP 3: high bit to low bit
	if byteIndex < len(bitfield) {
		bitfield[byteIndex] |= (1 << bitIndex)
	}
}

// HasPiece checks if a piece is present in the bitfield (BEP 3).
func HasPiece(bitfield []byte, pieceIndex int) bool {
	byteIndex := pieceIndex / 8
	bitIndex := uint(7 - (pieceIndex % 8)) // BEP 3: high bit to low bit
	if byteIndex >= len(bitfield) {
		return false
	}
	return (bitfield[byteIndex] & (1 << bitIndex)) != 0
}

// ClearPiece clears a piece from the bitfield (BEP 3).
func ClearPiece(bitfield []byte, pieceIndex int) {
	byteIndex := pieceIndex / 8
	bitIndex := uint(7 - (pieceIndex % 8)) // BEP 3: high bit to low bit
	if byteIndex < len(bitfield) {
		bitfield[byteIndex] &= ^(1 << bitIndex)
	}
}

// CountPieces counts the number of pieces set in the bitfield
func CountPieces(bitfield []byte) int {
	count := 0
	for _, b := range bitfield {
		// Count set bits in each byte
		for i := 0; i < 8; i++ {
			if (b & (1 << uint(i))) != 0 {
				count++
			}
		}
	}
	return count
}
