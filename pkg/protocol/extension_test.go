package protocol

import (
	"testing"
)

// ── NewExtensionHandshake ─────────────────────────────────────────────────────

func TestNewExtensionHandshakeHasMetadata(t *testing.T) {
	h := NewExtensionHandshake(6881)
	if !h.HasMetadataSupport() {
		t.Error("NewExtensionHandshake should declare ut_metadata support")
	}
	id, ok := h.GetMetadataExtID()
	if !ok {
		t.Error("GetMetadataExtID should return ok=true")
	}
	if id != 1 {
		t.Errorf("ut_metadata local ID: got %d, want 1", id)
	}
}

// ── Serialize / ParseExtensionHandshake round-trip ───────────────────────────

func TestExtensionHandshakeRoundTrip(t *testing.T) {
	original := &ExtensionHandshake{
		M:            map[string]int{"ut_metadata": 2, "ut_pex": 1},
		Reqq:         250,
		MetadataSize: 16384,
	}

	data, err := original.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	got, err := ParseExtensionHandshake(data)
	if err != nil {
		t.Fatalf("ParseExtensionHandshake: %v", err)
	}

	if got.M["ut_metadata"] != 2 {
		t.Errorf("ut_metadata ID: got %d, want 2", got.M["ut_metadata"])
	}
	if got.M["ut_pex"] != 1 {
		t.Errorf("ut_pex ID: got %d, want 1", got.M["ut_pex"])
	}
	if got.Reqq != original.Reqq {
		t.Errorf("Reqq: got %d, want %d", got.Reqq, original.Reqq)
	}
	if got.MetadataSize != original.MetadataSize {
		t.Errorf("MetadataSize: got %d, want %d", got.MetadataSize, original.MetadataSize)
	}
}

func TestExtensionHandshakeSerializeIsNonEmpty(t *testing.T) {
	h := NewExtensionHandshake(0)
	data, err := h.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if len(data) == 0 {
		t.Error("Serialize should produce non-empty bytes")
	}
}

// ── HasMetadataSupport / GetMetadataExtID edge cases ─────────────────────────

func TestHasMetadataSupportFalse(t *testing.T) {
	h := &ExtensionHandshake{M: map[string]int{"ut_pex": 1}}
	if h.HasMetadataSupport() {
		t.Error("HasMetadataSupport should be false when ut_metadata not in M")
	}
	_, ok := h.GetMetadataExtID()
	if ok {
		t.Error("GetMetadataExtID should return ok=false when ut_metadata absent")
	}
}

func TestHasMetadataSupportNilMap(t *testing.T) {
	h := &ExtensionHandshake{}
	if h.HasMetadataSupport() {
		t.Error("HasMetadataSupport should be false with nil M map")
	}
}

// ── ParseExtensionHandshake error cases ──────────────────────────────────────

func TestParseExtensionHandshakeInvalidBencode(t *testing.T) {
	_, err := ParseExtensionHandshake([]byte("not-valid-bencode"))
	if err == nil {
		t.Error("ParseExtensionHandshake should fail for invalid bencode")
	}
}

func TestParseExtensionHandshakeEmpty(t *testing.T) {
	// An empty bencode dict "de" is technically valid.
	h, err := ParseExtensionHandshake([]byte("de"))
	if err != nil {
		t.Fatalf("ParseExtensionHandshake empty dict: %v", err)
	}
	if h.HasMetadataSupport() {
		t.Error("empty dict should not have metadata support")
	}
}
