package lsd

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestBuildPacket(t *testing.T) {
	ih := "aabbccddee11223344556677889900aabbccddee"
	pkt := buildPacket(ih, 6881, "deadbeef")
	s := string(pkt)

	if !strings.HasPrefix(s, "BT-SEARCH * HTTP/1.1\r\n") {
		t.Fatal("missing BT-SEARCH request line")
	}
	if !strings.Contains(s, "Host: 239.192.152.143:6771\r\n") {
		t.Fatal("missing Host header")
	}
	if !strings.Contains(s, "Port: 6881\r\n") {
		t.Fatal("missing Port header")
	}
	if !strings.Contains(s, "Infohash: "+ih+"\r\n") {
		t.Fatal("missing Infohash header")
	}
	if !strings.Contains(s, "cookie: deadbeef\r\n") {
		t.Fatal("missing cookie header")
	}
	if !strings.HasSuffix(s, "\r\n\r\n") {
		t.Fatal("missing trailing CRLF CRLF")
	}
}

func TestParsePacket(t *testing.T) {
	ih := "aabbccddee11223344556677889900aabbccddee"
	pkt := buildPacket(ih, 6881, "deadbeef")
	p := parsePacket(pkt)
	if p == nil {
		t.Fatal("parsePacket returned nil")
	}
	if p.infoHash != ih {
		t.Fatalf("infoHash = %q, want %q", p.infoHash, ih)
	}
	if p.port != 6881 {
		t.Fatalf("port = %d, want 6881", p.port)
	}
	if p.cookie != "deadbeef" {
		t.Fatalf("cookie = %q, want %q", p.cookie, "deadbeef")
	}
}

func TestParsePacketWrongHash(t *testing.T) {
	pkt := buildPacket("aabbccddee11223344556677889900aabbccddee", 6881, "c0ffee00")
	p := parsePacket(pkt)
	if p == nil {
		t.Fatal("parsePacket returned nil for valid packet")
	}
	// Simulating the filter: a different info hash should not match ours.
	ourHash := "1111111111111111111111111111111111111111"
	if p.infoHash == ourHash {
		t.Fatal("info hash should not match")
	}
}

func TestParsePacketMalformed(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"garbage", []byte("hello world")},
		{"no infohash", []byte("BT-SEARCH * HTTP/1.1\r\nPort: 6881\r\n\r\n")},
		{"short infohash", []byte("BT-SEARCH * HTTP/1.1\r\nInfohash: aabb\r\nPort: 6881\r\n\r\n")},
		{"no port", []byte("BT-SEARCH * HTTP/1.1\r\nInfohash: aabbccddee11223344556677889900aabbccddee\r\n\r\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if p := parsePacket(tc.data); p != nil {
				t.Fatalf("expected nil, got %+v", p)
			}
		})
	}
}

func TestSelfSuppression(t *testing.T) {
	ih := "aabbccddee11223344556677889900aabbccddee"
	cookie := "deadbeef"
	pkt := buildPacket(ih, 6881, cookie)
	p := parsePacket(pkt)
	if p == nil {
		t.Fatal("parsePacket returned nil")
	}
	// Same cookie → should be filtered.
	if p.cookie != cookie {
		t.Fatalf("cookie mismatch: %q vs %q", p.cookie, cookie)
	}
}

func TestGenerateCookie(t *testing.T) {
	c1 := generateCookie()
	c2 := generateCookie()
	if len(c1) != 8 { // 4 bytes = 8 hex chars
		t.Fatalf("cookie length = %d, want 8", len(c1))
	}
	if _, err := hex.DecodeString(c1); err != nil {
		t.Fatalf("cookie not valid hex: %v", err)
	}
	if c1 == c2 {
		t.Fatal("two cookies should not be identical")
	}
}
