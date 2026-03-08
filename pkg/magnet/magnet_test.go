package magnet

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cbluth/bittorrent/pkg/dht"
)

func TestParseMagnet(t *testing.T) {
	tests := []struct {
		name        string
		magnetURI   string
		expectError bool
		checkFunc   func(*testing.T, *Magnet)
	}{
		{
			name:        "Valid magnet with hex hash",
			magnetURI:   "magnet:?xt=urn:btih:ABCDEF1234567890ABCDEF1234567890ABCDEF12",
			expectError: false,
			checkFunc: func(t *testing.T, m *Magnet) {
				expected := "abcdef1234567890abcdef1234567890abcdef12"
				actual := fmt.Sprintf("%x", m.InfoHash)
				if actual != expected {
					t.Errorf("InfoHash mismatch: got %s, want %s", actual, expected)
				}
			},
		},
		{
			name:        "Magnet with display name",
			magnetURI:   "magnet:?xt=urn:btih:ABCDEF1234567890ABCDEF1234567890ABCDEF12&dn=Test+File",
			expectError: false,
			checkFunc: func(t *testing.T, m *Magnet) {
				if m.DisplayName != "Test File" {
					t.Errorf("DisplayName: got %s, want 'Test File'", m.DisplayName)
				}
			},
		},
		{
			name: "Magnet with trackers",
			magnetURI: "magnet:?xt=urn:btih:ABCDEF1234567890ABCDEF1234567890ABCDEF12" +
				"&tr=udp://tracker1.com:1337&tr=http://tracker2.com",
			expectError: false,
			checkFunc: func(t *testing.T, m *Magnet) {
				if len(m.Trackers) != 2 {
					t.Errorf("Trackers count: got %d, want 2", len(m.Trackers))
				}
			},
		},
		{
			name:        "Invalid URI - no xt parameter",
			magnetURI:   "magnet:?dn=Test",
			expectError: true,
		},
		{
			name:        "Invalid URI - not a magnet",
			magnetURI:   "http://example.com",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Parse(tt.magnetURI)
			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if tt.checkFunc != nil {
				tt.checkFunc(t, m)
			}
		})
	}
}

func TestFromInfoHash(t *testing.T) {
	var infoHash dht.Key
	for i := 0; i < 20; i++ {
		infoHash[i] = byte(i)
	}

	m := FromInfoHash(infoHash)
	if m.InfoHash != infoHash {
		t.Error("InfoHash mismatch")
	}
}

func TestAddTrackers(t *testing.T) {
	m := FromInfoHash(dht.Key{})

	m.AddTracker("udp://tracker1.com")
	if len(m.Trackers) != 1 {
		t.Errorf("Expected 1 tracker, got %d", len(m.Trackers))
	}

	m.AddTrackers([]string{"udp://tracker2.com", "http://tracker3.com"})
	if len(m.Trackers) != 3 {
		t.Errorf("Expected 3 trackers, got %d", len(m.Trackers))
	}
}

func TestMagnetString(t *testing.T) {
	var infoHash dht.Key
	for i := range 20 {
		infoHash[i] = byte(i)
	}

	m := &Magnet{
		InfoHash:    infoHash,
		DisplayName: "Test",
		Trackers:    []string{"udp://tracker1.com"},
	}

	magnetStr := m.String()
	if !strings.Contains(magnetStr, "magnet:?") {
		t.Error("Magnet string should start with 'magnet:?'")
	}
	if !strings.Contains(magnetStr, "xt=urn") || !strings.Contains(magnetStr, "btih") {
		t.Errorf("Magnet string should contain info hash. Got: %s", magnetStr)
	}
}

func TestDecodeBase32(t *testing.T) {
	// Test case: "MFRGG" in base32 = "abc" in hex
	result, err := decodeBase32("MFRGG")
	if err != nil {
		t.Fatalf("Failed to decode base32: %v", err)
	}

	expected := []byte{0x61, 0x62, 0x63}
	if len(result) != len(expected) {
		t.Errorf("Length mismatch: got %d, want %d", len(result), len(expected))
	}

	for i, b := range expected {
		if result[i] != b {
			t.Errorf("Byte %d: got %x, want %x", i, result[i], b)
		}
	}
}
