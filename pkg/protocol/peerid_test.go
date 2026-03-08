package protocol

import (
	"strings"
	"testing"
)

// ── GeneratePeerID ────────────────────────────────────────────────────────────

func TestGeneratePeerIDFormat(t *testing.T) {
	id, err := GeneratePeerID("BT", "0001")
	if err != nil {
		t.Fatalf("GeneratePeerID: %v", err)
	}
	s := string(id[:])
	// Azureus style: -BT0001-<12 random bytes>
	if !strings.HasPrefix(s, "-BT0001-") {
		t.Errorf("peer ID should start with '-BT0001-', got prefix %q", s[:8])
	}
}

func TestGeneratePeerIDLength(t *testing.T) {
	id, err := GeneratePeerID("BT", "0001")
	if err != nil {
		t.Fatalf("GeneratePeerID: %v", err)
	}
	if len(id) != 20 {
		t.Errorf("peer ID length: got %d, want 20", len(id))
	}
}

func TestGeneratePeerIDIsUnique(t *testing.T) {
	id1, _ := GeneratePeerID("BT", "0001")
	id2, _ := GeneratePeerID("BT", "0001")
	if id1 == id2 {
		t.Error("two generated peer IDs should not be identical")
	}
}

func TestGeneratePeerIDCustomClient(t *testing.T) {
	id, err := GeneratePeerID("qB", "4.0.")
	if err != nil {
		t.Fatalf("GeneratePeerID: %v", err)
	}
	s := string(id[:])
	if !strings.HasPrefix(s, "-qB4.0.-") {
		t.Errorf("peer ID should start with '-qB4.0.-', got prefix %q", s[:8])
	}
}

func TestGeneratePeerIDTooLong(t *testing.T) {
	// clientID + version = 9 chars → prefix "-ABCDE12345-" = 12 chars > 8 limit.
	_, err := GeneratePeerID("ABCDE", "12345")
	if err == nil {
		t.Error("GeneratePeerID should fail when clientID+version exceeds 6 chars")
	}
}

// ── DefaultPeerID ─────────────────────────────────────────────────────────────

func TestDefaultPeerIDFormat(t *testing.T) {
	id, err := DefaultPeerID()
	if err != nil {
		t.Fatalf("DefaultPeerID: %v", err)
	}
	s := string(id[:])
	if !strings.HasPrefix(s, "-BT0001-") {
		t.Errorf("DefaultPeerID should start with '-BT0001-', got %q", s[:8])
	}
}

func TestDefaultPeerIDNotEmpty(t *testing.T) {
	id, err := DefaultPeerID()
	if err != nil {
		t.Fatalf("DefaultPeerID: %v", err)
	}
	if id.Empty() {
		t.Error("DefaultPeerID should not produce an all-zero key")
	}
}
