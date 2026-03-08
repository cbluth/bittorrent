package protocol

import (
	"net"
	"testing"
)

// BEP 6 test vectors:
//   IP: 80.4.4.200
//   infohash: 0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA (all 0xAA)
//   numPieces: 1313
//   k=7 → [1059, 431, 808, 1217, 287, 376, 1188]
//   k=9 → above + [353, 508]

func TestAllowedFastSet_BEP6Vectors(t *testing.T) {
	ip := net.ParseIP("80.4.4.200")
	var infoHash [20]byte
	for i := range infoHash {
		infoHash[i] = 0xAA
	}

	t.Run("k=7", func(t *testing.T) {
		expected := []uint32{1059, 431, 808, 1217, 287, 376, 1188}
		got := AllowedFastSet(7, 1313, infoHash, ip)
		if len(got) != len(expected) {
			t.Fatalf("expected %d pieces, got %d: %v", len(expected), len(got), got)
		}
		for i, v := range expected {
			if got[i] != v {
				t.Errorf("index %d: expected %d, got %d (full: %v)", i, v, got[i], got)
			}
		}
	})

	t.Run("k=9", func(t *testing.T) {
		expected := []uint32{1059, 431, 808, 1217, 287, 376, 1188, 353, 508}
		got := AllowedFastSet(9, 1313, infoHash, ip)
		if len(got) != len(expected) {
			t.Fatalf("expected %d pieces, got %d: %v", len(expected), len(got), got)
		}
		for i, v := range expected {
			if got[i] != v {
				t.Errorf("index %d: expected %d, got %d (full: %v)", i, v, got[i], got)
			}
		}
	})
}

func TestAllowedFastSet_EdgeCases(t *testing.T) {
	var infoHash [20]byte
	ip := net.ParseIP("127.0.0.1")

	t.Run("zero_pieces", func(t *testing.T) {
		got := AllowedFastSet(10, 0, infoHash, ip)
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("zero_k", func(t *testing.T) {
		got := AllowedFastSet(0, 100, infoHash, ip)
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("ipv6_returns_nil", func(t *testing.T) {
		got := AllowedFastSet(10, 100, infoHash, net.ParseIP("::1"))
		if got != nil {
			t.Errorf("expected nil for IPv6, got %v", got)
		}
	})

	t.Run("k_larger_than_pieces", func(t *testing.T) {
		// When k > numPieces, can only return numPieces unique indices.
		got := AllowedFastSet(20, 5, infoHash, ip)
		if len(got) != 5 {
			t.Errorf("expected 5 pieces, got %d: %v", len(got), got)
		}
		// Verify all unique
		seen := make(map[uint32]bool)
		for _, idx := range got {
			if seen[idx] {
				t.Errorf("duplicate index %d in %v", idx, got)
			}
			seen[idx] = true
		}
	})
}
