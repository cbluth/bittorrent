package protocol

import (
	"bytes"
	"testing"

	"github.com/cbluth/bittorrent/pkg/dht"
)

// ── Handshake ─────────────────────────────────────────────────────────────────

func TestHandshakeSerializeRoundTrip(t *testing.T) {
	infoHash := dht.RandomKey()
	peerID := dht.RandomKey()

	h := NewHandshake(infoHash, peerID)
	data := h.Serialize()

	if len(data) != HandshakeLen {
		t.Fatalf("Serialize returned %d bytes, want %d", len(data), HandshakeLen)
	}

	got, err := ReadHandshake(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadHandshake: %v", err)
	}
	if got.Pstr != ProtocolString {
		t.Errorf("Pstr = %q, want %q", got.Pstr, ProtocolString)
	}
	if got.InfoHash != infoHash {
		t.Errorf("InfoHash mismatch: got %x, want %x", got.InfoHash, infoHash)
	}
	if got.PeerID != peerID {
		t.Errorf("PeerID mismatch: got %x, want %x", got.PeerID, peerID)
	}
}

func TestNewHandshakeSetsExtensionBits(t *testing.T) {
	h := NewHandshake(dht.RandomKey(), dht.RandomKey())
	if !h.SupportsExtensions() {
		t.Error("NewHandshake should set extension bit (BEP 10)")
	}
	// DHT bit: byte 7, bit 0
	if h.Reserved[7]&0x01 == 0 {
		t.Error("NewHandshake should set DHT bit (byte 7)")
	}
}

func TestSupportsExtensionsFalse(t *testing.T) {
	h := &Handshake{Pstr: ProtocolString}
	if h.SupportsExtensions() {
		t.Error("empty reserved bytes should not report extension support")
	}
}

func TestReadHandshakeInvalidProtocol(t *testing.T) {
	data := make([]byte, HandshakeLen)
	data[0] = 19
	copy(data[1:], "NotBitTorrent!!!!!!")

	_, err := ReadHandshake(bytes.NewReader(data))
	if err == nil {
		t.Error("ReadHandshake should fail for invalid protocol string")
	}
}

func TestReadHandshakeTooShort(t *testing.T) {
	_, err := ReadHandshake(bytes.NewReader([]byte{0x13, 'B', 'i'}))
	if err == nil {
		t.Error("ReadHandshake should fail on truncated input")
	}
}

// ── WriteMessage / ReadMessage ────────────────────────────────────────────────

func TestWriteReadMessageRoundTrip(t *testing.T) {
	msg := &Message{ID: MsgHave, Payload: []byte{0, 0, 0, 7}}

	var buf bytes.Buffer
	if err := WriteMessage(&buf, msg); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	got, err := ReadMessage(&buf, 0)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got == nil {
		t.Fatal("ReadMessage returned nil for non-keepalive message")
	}
	if got.ID != msg.ID {
		t.Errorf("ID: got %d, want %d", got.ID, msg.ID)
	}
	if !bytes.Equal(got.Payload, msg.Payload) {
		t.Errorf("Payload: got %v, want %v", got.Payload, msg.Payload)
	}
}

func TestWriteReadKeepAlive(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, nil); err != nil {
		t.Fatalf("WriteMessage keep-alive: %v", err)
	}

	got, err := ReadMessage(&buf, 0)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got != nil {
		t.Errorf("ReadMessage keep-alive: got %+v, want nil", got)
	}
}

func TestReadMessageTooLarge(t *testing.T) {
	var buf bytes.Buffer
	// 2 MB length prefix — beyond the 1 MB limit.
	buf.Write([]byte{0x00, 0x20, 0x00, 0x01})
	_, err := ReadMessage(&buf, 0)
	if err == nil {
		t.Error("ReadMessage should return error for oversized message")
	}
}

// ── Extended messages ─────────────────────────────────────────────────────────

func TestNewExtendedMessage(t *testing.T) {
	payload := []byte("test-payload")
	msg := NewExtendedMessage(42, payload)

	if msg.ID != MsgExtended {
		t.Errorf("ID: got %d, want %d", msg.ID, MsgExtended)
	}
	if len(msg.Payload) != 1+len(payload) {
		t.Errorf("Payload length: got %d, want %d", len(msg.Payload), 1+len(payload))
	}
	if msg.Payload[0] != 42 {
		t.Errorf("ext ID: got %d, want 42", msg.Payload[0])
	}
}

func TestParseExtendedMessage(t *testing.T) {
	payload := []byte("hello")
	msg := NewExtendedMessage(7, payload)

	extID, got, err := ParseExtendedMessage(msg)
	if err != nil {
		t.Fatalf("ParseExtendedMessage: %v", err)
	}
	if extID != 7 {
		t.Errorf("extID: got %d, want 7", extID)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload: got %v, want %v", got, payload)
	}
}

func TestParseExtendedMessageWrongID(t *testing.T) {
	msg := &Message{ID: MsgHave, Payload: []byte{0}}
	_, _, err := ParseExtendedMessage(msg)
	if err == nil {
		t.Error("ParseExtendedMessage should fail for non-extended message")
	}
}

func TestParseExtendedMessageTooShort(t *testing.T) {
	msg := &Message{ID: MsgExtended, Payload: []byte{}}
	_, _, err := ParseExtendedMessage(msg)
	if err == nil {
		t.Error("ParseExtendedMessage should fail for empty payload")
	}
}

// ── Have ──────────────────────────────────────────────────────────────────────

func TestHaveMessageRoundTrip(t *testing.T) {
	const index uint32 = 12345
	msg := NewHaveMessage(index)

	have, err := ParseHaveMessage(msg)
	if err != nil {
		t.Fatalf("ParseHaveMessage: %v", err)
	}
	if have.PieceIndex != index {
		t.Errorf("PieceIndex: got %d, want %d", have.PieceIndex, index)
	}
}

func TestParseHaveMessageWrongID(t *testing.T) {
	_, err := ParseHaveMessage(&Message{ID: MsgChoke, Payload: make([]byte, 4)})
	if err == nil {
		t.Error("ParseHaveMessage should fail for wrong ID")
	}
}

func TestParseHaveMessageShortPayload(t *testing.T) {
	_, err := ParseHaveMessage(&Message{ID: MsgHave, Payload: []byte{1, 2}})
	if err == nil {
		t.Error("ParseHaveMessage should fail for short payload")
	}
}

// ── Request ───────────────────────────────────────────────────────────────────

func TestRequestMessageRoundTrip(t *testing.T) {
	msg := NewRequestMessage(3, 16384, 16384)

	req, err := ParseRequestMessage(msg)
	if err != nil {
		t.Fatalf("ParseRequestMessage: %v", err)
	}
	if req.Index != 3 || req.Begin != 16384 || req.Length != 16384 {
		t.Errorf("Request fields: got (%d, %d, %d), want (3, 16384, 16384)",
			req.Index, req.Begin, req.Length)
	}
}

func TestParseRequestMessageWrongID(t *testing.T) {
	_, err := ParseRequestMessage(&Message{ID: MsgHave, Payload: make([]byte, 12)})
	if err == nil {
		t.Error("ParseRequestMessage should fail for wrong ID")
	}
}

func TestParseRequestMessageShortPayload(t *testing.T) {
	_, err := ParseRequestMessage(&Message{ID: MsgRequest, Payload: make([]byte, 8)})
	if err == nil {
		t.Error("ParseRequestMessage should fail for payload shorter than 12 bytes")
	}
}

// ── Piece ─────────────────────────────────────────────────────────────────────

func TestPieceMessageRoundTrip(t *testing.T) {
	block := []byte("some block data here")
	msg := NewPieceMessage(5, 0, block)

	piece, err := ParsePieceMessage(msg)
	if err != nil {
		t.Fatalf("ParsePieceMessage: %v", err)
	}
	if piece.Index != 5 || piece.Begin != 0 {
		t.Errorf("Index/Begin: got (%d,%d), want (5,0)", piece.Index, piece.Begin)
	}
	if !bytes.Equal(piece.Block, block) {
		t.Errorf("Block: got %v, want %v", piece.Block, block)
	}
}

func TestParsePieceMessageTooShort(t *testing.T) {
	_, err := ParsePieceMessage(&Message{ID: MsgPiece, Payload: make([]byte, 7)})
	if err == nil {
		t.Error("ParsePieceMessage should fail for payload shorter than 8 bytes")
	}
}

// ── Cancel ────────────────────────────────────────────────────────────────────

func TestCancelMessageRoundTrip(t *testing.T) {
	msg := NewCancelMessage(7, 32768, 16384)

	cancel, err := ParseCancelMessage(msg)
	if err != nil {
		t.Fatalf("ParseCancelMessage: %v", err)
	}
	if cancel.Index != 7 || cancel.Begin != 32768 || cancel.Length != 16384 {
		t.Errorf("Cancel fields: got (%d,%d,%d), want (7,32768,16384)",
			cancel.Index, cancel.Begin, cancel.Length)
	}
}

// ── Bitfield ──────────────────────────────────────────────────────────────────

func TestBitfieldMessageRoundTrip(t *testing.T) {
	bf := NewBitfield(10)
	SetPiece(bf, 0)
	SetPiece(bf, 5)
	SetPiece(bf, 9)

	msg := NewBitfieldMessage(bf)
	bfm, err := ParseBitfieldMessage(msg)
	if err != nil {
		t.Fatalf("ParseBitfieldMessage: %v", err)
	}
	if !bytes.Equal(bfm.Bitfield, bf) {
		t.Errorf("Bitfield mismatch: got %v, want %v", bfm.Bitfield, bf)
	}
}

func TestParseBitfieldMessageWrongID(t *testing.T) {
	_, err := ParseBitfieldMessage(&Message{ID: MsgHave, Payload: []byte{0xFF}})
	if err == nil {
		t.Error("ParseBitfieldMessage should fail for wrong ID")
	}
}

func TestBitfieldSetHasClear(t *testing.T) {
	bf := NewBitfield(24)
	for _, i := range []int{0, 7, 8, 15, 16, 23} {
		SetPiece(bf, i)
		if !HasPiece(bf, i) {
			t.Errorf("piece %d: Set then Has returned false", i)
		}
		ClearPiece(bf, i)
		if HasPiece(bf, i) {
			t.Errorf("piece %d: Clear then Has returned true", i)
		}
	}
}

func TestBitfieldCountPieces(t *testing.T) {
	bf := NewBitfield(8)
	if CountPieces(bf) != 0 {
		t.Error("empty bitfield should have count 0")
	}
	SetPiece(bf, 0)
	SetPiece(bf, 3)
	SetPiece(bf, 7)
	if got := CountPieces(bf); got != 3 {
		t.Errorf("CountPieces: got %d, want 3", got)
	}
}

func TestBitfieldOutOfBounds(t *testing.T) {
	bf := NewBitfield(8)
	// Out-of-bounds index should not panic; HasPiece should return false.
	SetPiece(bf, 100)
	if HasPiece(bf, 100) {
		t.Error("HasPiece for out-of-range index should return false")
	}
}

func TestNewBitfieldSize(t *testing.T) {
	cases := []struct {
		numPieces int
		wantBytes int
	}{
		{0, 0},
		{1, 1},
		{8, 1},
		{9, 2},
		{16, 2},
		{17, 3},
	}
	for _, tc := range cases {
		bf := NewBitfield(tc.numPieces)
		if len(bf) != tc.wantBytes {
			t.Errorf("NewBitfield(%d): got %d bytes, want %d", tc.numPieces, len(bf), tc.wantBytes)
		}
	}
}

// ── Simple message constructors ───────────────────────────────────────────────

func TestSimpleMessageIDs(t *testing.T) {
	cases := []struct {
		msg  *Message
		want uint8
	}{
		{NewChokeMessage(), MsgChoke},
		{NewUnchokeMessage(), MsgUnchoke},
		{NewInterestedMessage(), MsgInterested},
		{NewNotInterestedMessage(), MsgNotInterested},
	}
	for _, tc := range cases {
		if tc.msg.ID != tc.want {
			t.Errorf("message ID: got %d, want %d", tc.msg.ID, tc.want)
		}
		if tc.msg.Payload != nil {
			t.Errorf("simple message should have nil payload, got %v", tc.msg.Payload)
		}
	}
}

// ── Message constants ─────────────────────────────────────────────────────────

func TestMessageIDConstants(t *testing.T) {
	if MsgChoke != 0 || MsgUnchoke != 1 || MsgInterested != 2 ||
		MsgNotInterested != 3 || MsgHave != 4 || MsgBitfield != 5 ||
		MsgRequest != 6 || MsgPiece != 7 || MsgCancel != 8 || MsgExtended != 20 {
		t.Error("message ID constants have unexpected values")
	}
}
