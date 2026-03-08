package http

import (
	"net"
	"strings"
	"testing"
)

// ── scrapeURLFrom ─────────────────────────────────────────────────────────────

func TestScrapeURLFromAnnounce(t *testing.T) {
	cases := []struct {
		announce string
		want     string
	}{
		{
			"http://tracker.example.com/announce",
			"http://tracker.example.com/scrape",
		},
		{
			"http://tracker.example.com/announce?passkey=abc",
			// Does not end with "/announce" — returned as-is.
			"http://tracker.example.com/announce?passkey=abc",
		},
		{
			"udp://tracker.example.com:1337/announce",
			"udp://tracker.example.com:1337/scrape",
		},
		{
			"http://tracker.example.com/scrape",
			// Already a scrape URL, no substitution.
			"http://tracker.example.com/scrape",
		},
		{
			"",
			"",
		},
	}
	for _, tc := range cases {
		got := scrapeURLFrom(tc.announce)
		if got != tc.want {
			t.Errorf("scrapeURLFrom(%q): got %q, want %q", tc.announce, got, tc.want)
		}
	}
}

// ── parseCompact4 ─────────────────────────────────────────────────────────────

func TestParseCompact4Single(t *testing.T) {
	// 1.2.3.4:6881 packed as 4-byte IP + 2-byte big-endian port.
	b := []byte{1, 2, 3, 4, 0x1A, 0xE1} // port = 0x1AE1 = 6881
	peers, err := parseCompact4(b)
	if err != nil {
		t.Fatalf("parseCompact4: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("len(peers): got %d, want 1", len(peers))
	}
	if !peers[0].IP.Equal(net.IPv4(1, 2, 3, 4)) {
		t.Errorf("IP: got %v, want 1.2.3.4", peers[0].IP)
	}
	if peers[0].Port != 6881 {
		t.Errorf("Port: got %d, want 6881", peers[0].Port)
	}
}

func TestParseCompact4Multiple(t *testing.T) {
	// Two peers.
	b := []byte{
		10, 0, 0, 1, 0x1A, 0xE1,
		192, 168, 1, 1, 0x1B, 0x9B, // port 7067
	}
	peers, err := parseCompact4(b)
	if err != nil {
		t.Fatalf("parseCompact4: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("len(peers): got %d, want 2", len(peers))
	}
}

func TestParseCompact4Empty(t *testing.T) {
	peers, err := parseCompact4([]byte{})
	if err != nil {
		t.Fatalf("parseCompact4 empty: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("expected 0 peers, got %d", len(peers))
	}
}

func TestParseCompact4InvalidLength(t *testing.T) {
	// 7 bytes — not a multiple of 6.
	_, err := parseCompact4(make([]byte, 7))
	if err == nil {
		t.Error("parseCompact4 should fail for length not a multiple of 6")
	}
}

// ── parseCompact6 ─────────────────────────────────────────────────────────────

func TestParseCompact6Single(t *testing.T) {
	// ::1 (loopback) on port 6881.
	b := make([]byte, 18)
	b[15] = 1 // ::1
	b[16] = 0x1A
	b[17] = 0xE1 // port 6881
	peers, err := parseCompact6(b)
	if err != nil {
		t.Fatalf("parseCompact6: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("len(peers): got %d, want 1", len(peers))
	}
	if peers[0].Port != 6881 {
		t.Errorf("Port: got %d, want 6881", peers[0].Port)
	}
}

func TestParseCompact6InvalidLength(t *testing.T) {
	// 10 bytes — not a multiple of 18.
	_, err := parseCompact6(make([]byte, 10))
	if err == nil {
		t.Error("parseCompact6 should fail for length not a multiple of 18")
	}
}

// ── parseAnnounceResponse ─────────────────────────────────────────────────────

func TestParseAnnounceResponseCompact(t *testing.T) {
	// Minimal valid bencoded announce response with one compact IPv4 peer.
	// d8:intervali1800e5:peers6:<6 bytes>e
	peer := []byte{1, 2, 3, 4, 0x1A, 0xE1}
	body := "d8:intervali1800e5:peers6:\x01\x02\x03\x04\x1A\xE1e"
	resp, err := parseAnnounceResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseAnnounceResponse: %v", err)
	}
	if resp.Interval != 1800 {
		t.Errorf("Interval: got %d, want 1800", resp.Interval)
	}
	if len(resp.Peers) != 1 {
		t.Fatalf("len(Peers): got %d, want 1", len(resp.Peers))
	}
	if !resp.Peers[0].IP.Equal(net.IPv4(peer[0], peer[1], peer[2], peer[3])) {
		t.Errorf("peer IP: got %v", resp.Peers[0].IP)
	}
}

func TestParseAnnounceResponseFailure(t *testing.T) {
	body := "d14:failure reason12:tracker downe"
	_, err := parseAnnounceResponse(strings.NewReader(body))
	if err == nil {
		t.Error("parseAnnounceResponse should return error on failure reason")
	}
}

func TestParseAnnounceResponseNoPeers(t *testing.T) {
	body := "d8:intervali300e5:peers0:e"
	resp, err := parseAnnounceResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseAnnounceResponse: %v", err)
	}
	if len(resp.Peers) != 0 {
		t.Errorf("expected 0 peers, got %d", len(resp.Peers))
	}
}
