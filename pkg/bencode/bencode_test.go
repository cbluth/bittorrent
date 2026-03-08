package bencode

import (
	"bytes"
	"strings"
	"testing"
)

// ── Integer ───────────────────────────────────────────────────────────────────

func TestEncodeDecodeInt(t *testing.T) {
	cases := []struct {
		v    int64
		want string
	}{
		{0, "i0e"},
		{42, "i42e"},
		{-1, "i-1e"},
		{1000000, "i1000000e"},
	}
	for _, tc := range cases {
		enc, err := EncodeString(tc.v)
		if err != nil {
			t.Fatalf("EncodeString(%d): %v", tc.v, err)
		}
		if enc != tc.want {
			t.Errorf("EncodeString(%d): got %q, want %q", tc.v, enc, tc.want)
		}
		var got int64
		if err := DecodeString(enc, &got); err != nil {
			t.Fatalf("DecodeString %q into int64: %v", enc, err)
		}
		if got != tc.v {
			t.Errorf("int64 round-trip %d: got %d", tc.v, got)
		}
	}
}

func TestEncodeDecodeUint(t *testing.T) {
	var got uint64
	enc, err := EncodeString(uint64(255))
	if err != nil {
		t.Fatalf("EncodeString(uint64): %v", err)
	}
	if enc != "i255e" {
		t.Errorf("EncodeString(uint64(255)): got %q, want %q", enc, "i255e")
	}
	if err := DecodeString(enc, &got); err != nil {
		t.Fatalf("DecodeString uint64: %v", err)
	}
	if got != 255 {
		t.Errorf("uint64 round-trip: got %d, want 255", got)
	}
}

func TestEncodeDecodeBool(t *testing.T) {
	cases := []struct {
		in   bool
		want string
	}{
		{true, "i1e"},
		{false, "i0e"},
	}
	for _, tc := range cases {
		enc, err := EncodeString(tc.in)
		if err != nil {
			t.Fatalf("EncodeString(%v): %v", tc.in, err)
		}
		if enc != tc.want {
			t.Errorf("EncodeString(%v): got %q, want %q", tc.in, enc, tc.want)
		}
		var got bool
		if err := DecodeString(enc, &got); err != nil {
			t.Fatalf("DecodeString bool: %v", err)
		}
		if got != tc.in {
			t.Errorf("bool round-trip %v: got %v", tc.in, got)
		}
	}
}

// ── String ────────────────────────────────────────────────────────────────────

func TestEncodeDecodeString(t *testing.T) {
	cases := []struct {
		v    string
		want string
	}{
		{"", "0:"},
		{"spam", "4:spam"},
		{"hello world", "11:hello world"},
	}
	for _, tc := range cases {
		enc, err := EncodeString(tc.v)
		if err != nil {
			t.Fatalf("EncodeString(%q): %v", tc.v, err)
		}
		if enc != tc.want {
			t.Errorf("EncodeString(%q): got %q, want %q", tc.v, enc, tc.want)
		}
		var got string
		if err := DecodeString(enc, &got); err != nil {
			t.Fatalf("DecodeString %q: %v", enc, err)
		}
		if got != tc.v {
			t.Errorf("string round-trip %q: got %q", tc.v, got)
		}
	}
}

// ── Bytes ─────────────────────────────────────────────────────────────────────

func TestEncodeDecodeBytes(t *testing.T) {
	input := []byte{0x01, 0x00, 0xFF}
	enc, err := EncodeBytes(input)
	if err != nil {
		t.Fatalf("EncodeBytes: %v", err)
	}
	var got []byte
	if err := DecodeBytes(enc, &got); err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Errorf("bytes round-trip: got %v, want %v", got, input)
	}
}

// ── List ──────────────────────────────────────────────────────────────────────

func TestEncodeDecodeList(t *testing.T) {
	input := []string{"spam", "eggs"}
	enc, err := EncodeString(input)
	if err != nil {
		t.Fatalf("EncodeString(list): %v", err)
	}
	if enc != "l4:spam4:eggse" {
		t.Errorf("encoded list: got %q, want %q", enc, "l4:spam4:eggse")
	}

	var got []string
	if err := DecodeString(enc, &got); err != nil {
		t.Fatalf("DecodeString(list): %v", err)
	}
	if len(got) != 2 || got[0] != "spam" || got[1] != "eggs" {
		t.Errorf("decoded list: got %v", got)
	}
}

func TestEncodeDecodeIntList(t *testing.T) {
	input := []int64{1, 2, 3}
	enc, err := EncodeString(input)
	if err != nil {
		t.Fatalf("EncodeString(int list): %v", err)
	}
	if enc != "li1ei2ei3ee" {
		t.Errorf("encoded int list: got %q, want %q", enc, "li1ei2ei3ee")
	}
}

// ── Dict ──────────────────────────────────────────────────────────────────────

func TestEncodeDictKeysAreSorted(t *testing.T) {
	m := map[string]int64{"zoo": 1, "abc": 2, "mid": 3}
	enc, err := EncodeString(m)
	if err != nil {
		t.Fatalf("EncodeString(map): %v", err)
	}
	// Keys must appear in lexicographic order: abc < mid < zoo.
	abcPos := strings.Index(enc, "abc")
	midPos := strings.Index(enc, "mid")
	zooPos := strings.Index(enc, "zoo")
	if !(abcPos < midPos && midPos < zooPos) {
		t.Errorf("map keys not sorted in %q (abc=%d, mid=%d, zoo=%d)",
			enc, abcPos, midPos, zooPos)
	}
}

func TestDecodeMapStringString(t *testing.T) {
	enc := "d3:foo3:bar3:baz3:quxe"
	got := make(map[string]string)
	if err := DecodeString(enc, &got); err != nil {
		t.Fatalf("DecodeString(map): %v", err)
	}
	if got["foo"] != "bar" || got["baz"] != "qux" {
		t.Errorf("decoded map: %v", got)
	}
}

// ── Struct ────────────────────────────────────────────────────────────────────

type simpleStruct struct {
	Name   string `bencode:"name"`
	Count  int64  `bencode:"count"`
	Hidden string `bencode:"-"`
}

func TestEncodeDecodeStruct(t *testing.T) {
	original := simpleStruct{Name: "hello", Count: 42, Hidden: "secret"}
	enc, err := EncodeBytes(original)
	if err != nil {
		t.Fatalf("EncodeBytes(struct): %v", err)
	}

	var got simpleStruct
	if err := DecodeBytes(enc, &got); err != nil {
		t.Fatalf("DecodeBytes(struct): %v", err)
	}
	if got.Name != original.Name {
		t.Errorf("Name: got %q, want %q", got.Name, original.Name)
	}
	if got.Count != original.Count {
		t.Errorf("Count: got %d, want %d", got.Count, original.Count)
	}
	// "-" tagged field is excluded from encoding, so it decodes to zero value.
	if got.Hidden != "" {
		t.Errorf("Hidden should be empty after decode (bencode:\"-\"), got %q", got.Hidden)
	}
}

type omitemptyStruct struct {
	Name  string `bencode:"name"`
	Extra string `bencode:"extra,omitempty"`
}

func TestEncodeOmitemptyZero(t *testing.T) {
	s := omitemptyStruct{Name: "test", Extra: ""}
	enc, err := EncodeString(s)
	if err != nil {
		t.Fatalf("EncodeString: %v", err)
	}
	if strings.Contains(enc, "extra") {
		t.Errorf("omitempty field should not be encoded when zero, got %q", enc)
	}
}

func TestEncodeOmitemptyNonZero(t *testing.T) {
	s := omitemptyStruct{Name: "test", Extra: "present"}
	enc, err := EncodeString(s)
	if err != nil {
		t.Fatalf("EncodeString: %v", err)
	}
	if !strings.Contains(enc, "extra") {
		t.Errorf("omitempty field should be encoded when non-zero, got %q", enc)
	}
}

// ── RawMessage ────────────────────────────────────────────────────────────────

func TestRawMessagePassthrough(t *testing.T) {
	// Encoding a RawMessage writes it verbatim.
	raw := RawMessage("i99e")
	enc, err := EncodeBytes(raw)
	if err != nil {
		t.Fatalf("EncodeBytes(RawMessage): %v", err)
	}
	if string(enc) != "i99e" {
		t.Errorf("RawMessage: got %q, want %q", enc, "i99e")
	}
}

func TestDecodeIntoRawMessage(t *testing.T) {
	// Decoding into a RawMessage captures the raw bytes of the value.
	enc := "d4:name4:teste"
	var raw RawMessage
	if err := DecodeString(enc, &raw); err != nil {
		t.Fatalf("DecodeString(RawMessage): %v", err)
	}
	if string(raw) != enc {
		t.Errorf("RawMessage capture: got %q, want %q", raw, enc)
	}
}

// ── BytesParsed ───────────────────────────────────────────────────────────────

func TestDecoderBytesParsed(t *testing.T) {
	// "i42e" is 4 bytes; a following "4:spam" starts at byte offset 4.
	data := "i42e4:spam"
	d := NewDecoder(strings.NewReader(data))

	var n int64
	if err := d.Decode(&n); err != nil {
		t.Fatalf("Decode int: %v", err)
	}
	if d.BytesParsed() != 4 {
		t.Errorf("BytesParsed after 'i42e': got %d, want 4", d.BytesParsed())
	}
}

// ── Error cases ───────────────────────────────────────────────────────────────

func TestDecodeNonPointer(t *testing.T) {
	var x int64
	err := NewDecoder(strings.NewReader("i0e")).Decode(x) // value, not pointer
	if err == nil {
		t.Error("Decode with a non-pointer value should return an error")
	}
}

func TestDecodeNilPointer(t *testing.T) {
	var p *int64 // nil pointer
	err := NewDecoder(strings.NewReader("i0e")).Decode(p)
	if err == nil {
		t.Error("Decode with a nil pointer should return an error")
	}
}

func TestDecodeInvalidToken(t *testing.T) {
	var x int64
	err := DecodeString("x42e", &x)
	if err == nil {
		t.Error("invalid bencode token 'x' should return an error")
	}
}

func TestDecodeUnknownFieldsIgnored(t *testing.T) {
	// Decoding a dict with a key not in the target struct should not error.
	enc := "d7:unknown5:value4:name5:helloe"
	var s simpleStruct
	if err := DecodeString(enc, &s); err != nil {
		t.Fatalf("DecodeString with unknown field: %v", err)
	}
	if s.Name != "hello" {
		t.Errorf("Name: got %q, want %q", s.Name, "hello")
	}
}
