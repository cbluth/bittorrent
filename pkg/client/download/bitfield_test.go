package download

import (
	"sync"
	"testing"
)

// ── NewBitfield ───────────────────────────────────────────────────────────────

func TestNewBitfieldSize(t *testing.T) {
	cases := []struct {
		numPieces uint32
		wantBytes int
	}{
		{0, 0},
		{1, 1},
		{8, 1},
		{9, 2},
		{16, 2},
		{17, 3},
		{100, 13},
	}
	for _, tc := range cases {
		bf := NewBitfield(tc.numPieces)
		if len(bf.bits) != tc.wantBytes {
			t.Errorf("NewBitfield(%d): got %d bytes, want %d", tc.numPieces, len(bf.bits), tc.wantBytes)
		}
		if bf.len != tc.numPieces {
			t.Errorf("NewBitfield(%d): len field = %d, want %d", tc.numPieces, bf.len, tc.numPieces)
		}
	}
}

// ── Set / Test ────────────────────────────────────────────────────────────────

func TestBitfieldSetTest(t *testing.T) {
	bf := NewBitfield(64)
	for _, i := range []uint32{0, 1, 7, 8, 15, 32, 63} {
		if bf.Test(i) {
			t.Errorf("piece %d: should not be set before Set()", i)
		}
		bf.Set(i)
		if !bf.Test(i) {
			t.Errorf("piece %d: should be set after Set()", i)
		}
	}
}

func TestBitfieldSetIdempotent(t *testing.T) {
	bf := NewBitfield(8)
	bf.Set(3)
	bf.Set(3) // calling twice must not corrupt the bitfield
	if !bf.Test(3) {
		t.Error("piece 3 should still be set after double Set()")
	}
}

func TestBitfieldTestOutOfBounds(t *testing.T) {
	bf := NewBitfield(8)
	// Index beyond the allocated bits: must return false, not panic.
	if bf.Test(100) {
		t.Error("Test() out-of-bounds index should return false")
	}
}

func TestBitfieldSetOutOfBounds(t *testing.T) {
	bf := NewBitfield(8)
	// Set() out-of-bounds must be a no-op, not a panic.
	bf.Set(100)
}

// ── AllSet ────────────────────────────────────────────────────────────────────

func TestBitfieldAllSetFalseEmpty(t *testing.T) {
	bf := NewBitfield(4)
	if bf.AllSet(4) {
		t.Error("AllSet should be false on a fresh bitfield")
	}
}

func TestBitfieldAllSetTrue(t *testing.T) {
	bf := NewBitfield(4)
	bf.Set(0)
	bf.Set(1)
	bf.Set(2)
	bf.Set(3)
	if !bf.AllSet(4) {
		t.Error("AllSet should be true when all pieces are set")
	}
}

func TestBitfieldAllSetPartial(t *testing.T) {
	bf := NewBitfield(4)
	bf.Set(0)
	bf.Set(1)
	bf.Set(2)
	// piece 3 missing
	if bf.AllSet(4) {
		t.Error("AllSet should be false when one piece is missing")
	}
}

func TestBitfieldAllSetZero(t *testing.T) {
	bf := NewBitfield(0)
	// AllSet(0) — vacuously true (no pieces to check).
	if !bf.AllSet(0) {
		t.Error("AllSet(0) should return true vacuously")
	}
}

// ── Concurrency ───────────────────────────────────────────────────────────────

func TestBitfieldConcurrentSetTest(t *testing.T) {
	const numPieces = 128
	bf := NewBitfield(numPieces)

	var wg sync.WaitGroup
	// Writers: each goroutine sets its own piece.
	for i := uint32(0); i < numPieces; i++ {
		wg.Add(1)
		go func(idx uint32) {
			defer wg.Done()
			bf.Set(idx)
		}(i)
	}
	// Readers: concurrent reads must not race with writes.
	for i := uint32(0); i < numPieces; i++ {
		wg.Add(1)
		go func(idx uint32) {
			defer wg.Done()
			_ = bf.Test(idx)
		}(i)
	}
	wg.Wait()

	// After all writers finished every piece must be set.
	if !bf.AllSet(numPieces) {
		t.Error("not all pieces set after concurrent Set() calls")
	}
}
