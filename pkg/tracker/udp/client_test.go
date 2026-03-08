package udp

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/cbluth/bittorrent/pkg/dht"
)

// ── parseCompact4 ─────────────────────────────────────────────────────────────

func TestParseCompact4Single(t *testing.T) {
	b := []byte{1, 2, 3, 4, 0x1A, 0xE1} // 1.2.3.4:6881
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
	b := []byte{
		10, 0, 0, 1, 0x00, 0x50, // 10.0.0.1:80
		172, 16, 0, 1, 0x1F, 0x90, // 172.16.0.1:8080
	}
	peers, err := parseCompact4(b)
	if err != nil {
		t.Fatalf("parseCompact4: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("len(peers): got %d, want 2", len(peers))
	}
	if peers[1].Port != 8080 {
		t.Errorf("peer[1].Port: got %d, want 8080", peers[1].Port)
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
	_, err := parseCompact4(make([]byte, 7))
	if err == nil {
		t.Error("parseCompact4 should fail when length is not a multiple of 6")
	}
}

// ── parseCompact6 ─────────────────────────────────────────────────────────────

func TestParseCompact6Single(t *testing.T) {
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
	_, err := parseCompact6(make([]byte, 17))
	if err == nil {
		t.Error("parseCompact6 should fail when length is not a multiple of 18")
	}
}

// ── parseAnnounceResponse ─────────────────────────────────────────────────────

func buildAnnounceResponse(txID uint32, interval, leechers, seeders int32, peers []byte) []byte {
	buf := make([]byte, 20+len(peers))
	binary.BigEndian.PutUint32(buf[0:], actionAnnounce)
	binary.BigEndian.PutUint32(buf[4:], txID)
	binary.BigEndian.PutUint32(buf[8:], uint32(interval))
	binary.BigEndian.PutUint32(buf[12:], uint32(leechers))
	binary.BigEndian.PutUint32(buf[16:], uint32(seeders))
	copy(buf[20:], peers)
	return buf
}

func TestParseAnnounceResponseBasic(t *testing.T) {
	peerBytes := []byte{1, 2, 3, 4, 0x1A, 0xE1}
	data := buildAnnounceResponse(0xDEAD, 1800, 5, 10, peerBytes)

	resp, err := parseAnnounceResponse(data, 0xDEAD)
	if err != nil {
		t.Fatalf("parseAnnounceResponse: %v", err)
	}
	if resp.Interval != 1800 {
		t.Errorf("Interval: got %d, want 1800", resp.Interval)
	}
	if resp.Incomplete != 5 {
		t.Errorf("Leechers: got %d, want 5", resp.Incomplete)
	}
	if resp.Complete != 10 {
		t.Errorf("Seeders: got %d, want 10", resp.Complete)
	}
	if len(resp.Peers) != 1 {
		t.Fatalf("len(Peers): got %d, want 1", len(resp.Peers))
	}
}

func TestParseAnnounceResponseTxIDMismatch(t *testing.T) {
	data := buildAnnounceResponse(0xAAAA, 1800, 0, 0, nil)
	_, err := parseAnnounceResponse(data, 0xBBBB)
	if err == nil {
		t.Error("parseAnnounceResponse should fail on transaction ID mismatch")
	}
}

func TestParseAnnounceResponseTooShort(t *testing.T) {
	_, err := parseAnnounceResponse(make([]byte, 10), 0)
	if err == nil {
		t.Error("parseAnnounceResponse should fail for data shorter than 20 bytes")
	}
}

func TestParseAnnounceResponseNoPeers(t *testing.T) {
	data := buildAnnounceResponse(1, 300, 0, 3, nil)
	resp, err := parseAnnounceResponse(data, 1)
	if err != nil {
		t.Fatalf("parseAnnounceResponse: %v", err)
	}
	if len(resp.Peers) != 0 {
		t.Errorf("expected 0 peers, got %d", len(resp.Peers))
	}
}

// ── parseScrapeResponse ───────────────────────────────────────────────────────

func buildScrapeResponse(txID uint32, entries ...struct{ seeders, completed, leechers int32 }) []byte {
	buf := make([]byte, 8+len(entries)*12)
	binary.BigEndian.PutUint32(buf[0:], actionScrape)
	binary.BigEndian.PutUint32(buf[4:], txID)
	for i, e := range entries {
		off := 8 + i*12
		binary.BigEndian.PutUint32(buf[off:], uint32(e.seeders))
		binary.BigEndian.PutUint32(buf[off+4:], uint32(e.completed))
		binary.BigEndian.PutUint32(buf[off+8:], uint32(e.leechers))
	}
	return buf
}

func TestParseScrapeResponseBasic(t *testing.T) {
	hash := dht.RandomKey()
	data := buildScrapeResponse(42, struct{ seeders, completed, leechers int32 }{10, 500, 3})

	resp, err := parseScrapeResponse(data, 42, []dht.Key{hash})
	if err != nil {
		t.Fatalf("parseScrapeResponse: %v", err)
	}
	if len(resp.Files) != 1 {
		t.Fatalf("Files count: got %d, want 1", len(resp.Files))
	}
	stats := resp.Files[hash.Hex()]
	if stats.Complete != 10 {
		t.Errorf("Complete: got %d, want 10", stats.Complete)
	}
	if stats.Downloaded != 500 {
		t.Errorf("Downloaded: got %d, want 500", stats.Downloaded)
	}
	if stats.Incomplete != 3 {
		t.Errorf("Incomplete: got %d, want 3", stats.Incomplete)
	}
}

func TestParseScrapeResponseTxIDMismatch(t *testing.T) {
	data := buildScrapeResponse(0xAAAA)
	_, err := parseScrapeResponse(data, 0xBBBB, nil)
	if err == nil {
		t.Error("parseScrapeResponse should fail on transaction ID mismatch")
	}
}

func TestParseScrapeResponseTooShort(t *testing.T) {
	_, err := parseScrapeResponse(make([]byte, 4), 0, nil)
	if err == nil {
		t.Error("parseScrapeResponse should fail for data shorter than 8 bytes")
	}
}
