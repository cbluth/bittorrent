package dht

import (
	"strings"
	"testing"
)

// ── KeyFromBytes ──────────────────────────────────────────────────────────────

func TestKeyFromBytesValid(t *testing.T) {
	b := make([]byte, 20)
	for i := range b {
		b[i] = byte(i + 1)
	}
	k, err := KeyFromBytes(b)
	if err != nil {
		t.Fatalf("KeyFromBytes: %v", err)
	}
	for i, want := range b {
		if k[i] != want {
			t.Errorf("byte %d: got %d, want %d", i, k[i], want)
		}
	}
}

func TestKeyFromBytesTooShort(t *testing.T) {
	_, err := KeyFromBytes(make([]byte, 10))
	if err == nil {
		t.Error("KeyFromBytes with 10 bytes should return an error")
	}
}

func TestKeyFromBytesTooLong(t *testing.T) {
	_, err := KeyFromBytes(make([]byte, 21))
	if err == nil {
		t.Error("KeyFromBytes with 21 bytes should return an error")
	}
}

func TestKeyFromBytesNil(t *testing.T) {
	_, err := KeyFromBytes(nil)
	if err == nil {
		t.Error("KeyFromBytes(nil) should return an error")
	}
}

// ── KeyFromHex ────────────────────────────────────────────────────────────────

func TestKeyFromHexValid(t *testing.T) {
	hex := "0102030405060708090a0b0c0d0e0f1011121314"
	k, err := KeyFromHex(hex)
	if err != nil {
		t.Fatalf("KeyFromHex: %v", err)
	}
	if k[0] != 0x01 || k[1] != 0x02 || k[19] != 0x14 {
		t.Errorf("unexpected key bytes: %x", k)
	}
}

func TestKeyFromHexInvalidChars(t *testing.T) {
	_, err := KeyFromHex("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
	if err == nil {
		t.Error("KeyFromHex with invalid hex chars should return an error")
	}
}

func TestKeyFromHexWrongLength(t *testing.T) {
	// 10 hex chars = 5 bytes, not 20.
	_, err := KeyFromHex("0102030405")
	if err == nil {
		t.Error("KeyFromHex with 5-byte value should return an error")
	}
}

// ── Key methods ───────────────────────────────────────────────────────────────

func TestKeyEmptyZero(t *testing.T) {
	var k Key
	if !k.Empty() {
		t.Error("zero Key should be empty")
	}
}

func TestKeyEmptyNonZero(t *testing.T) {
	var k Key
	k[0] = 1
	if k.Empty() {
		t.Error("non-zero Key should not be empty")
	}
}

func TestKeyHexRoundTrip(t *testing.T) {
	original, _ := KeyFromHex("0102030405060708090a0b0c0d0e0f1011121314")
	h := original.Hex()
	if len(h) != 40 {
		t.Errorf("Hex() length: got %d, want 40", len(h))
	}
	recovered, err := KeyFromHex(h)
	if err != nil {
		t.Fatalf("KeyFromHex(k.Hex()): %v", err)
	}
	if recovered != original {
		t.Errorf("Hex round-trip mismatch: got %s, want %s", recovered.Hex(), original.Hex())
	}
}

func TestKeyBase64URLSafe(t *testing.T) {
	k := RandomKey()
	b64 := k.Base64()
	if len(b64) == 0 {
		t.Error("Base64() should not be empty")
	}
	// URL-safe base64 must not contain +, /, or = (NoPadding).
	if strings.ContainsAny(b64, "+/=") {
		t.Errorf("Base64() should be URL-safe without padding, got %q", b64)
	}
}

func TestKeyBytes(t *testing.T) {
	b := make([]byte, 20)
	for i := range b {
		b[i] = byte(i + 1)
	}
	k, _ := KeyFromBytes(b)
	got := k.Bytes()
	if len(got) != 20 {
		t.Errorf("Bytes() length: got %d, want 20", len(got))
	}
	for i, v := range got {
		if v != b[i] {
			t.Errorf("Bytes()[%d]: got %d, want %d", i, v, b[i])
		}
	}
}

func TestKeyArray20(t *testing.T) {
	k := RandomKey()
	arr := k.Array20()
	if arr != [20]byte(k) {
		t.Error("Array20() should return the same value as the key itself")
	}
}

func TestKeyLess(t *testing.T) {
	k1, _ := KeyFromHex("0000000000000000000000000000000000000001")
	k2, _ := KeyFromHex("0000000000000000000000000000000000000002")
	kMax, _ := KeyFromHex("ffffffffffffffffffffffffffffffffffffffff")

	if !k1.Less(k2) {
		t.Error("k1 should be less than k2")
	}
	if k2.Less(k1) {
		t.Error("k2 should not be less than k1")
	}
	if !k1.Less(kMax) {
		t.Error("k1 should be less than max key")
	}
	if k1.Less(k1) {
		t.Error("k1 should not be less than itself")
	}
}

// ── TransactionID ─────────────────────────────────────────────────────────────

func TestTransactionIDLength(t *testing.T) {
	tx := RandomTransactionID()
	if len(tx) != txLength {
		t.Errorf("RandomTransactionID length: got %d, want %d", len(tx), txLength)
	}
}

func TestTransactionIDBytes(t *testing.T) {
	tx := RandomTransactionID()
	b := tx.Bytes()
	if len(b) != len(tx) {
		t.Errorf("Bytes() length: got %d, want %d", len(b), len(tx))
	}
	for i := range tx {
		if b[i] != tx[i] {
			t.Errorf("Bytes()[%d]: got %d, want %d", i, b[i], tx[i])
		}
	}
}

func TestTransactionIDEqualTrue(t *testing.T) {
	tx1 := TransactionID([]byte{0xAB, 0xCD})
	tx2 := TransactionID([]byte{0xAB, 0xCD})
	if !tx1.Equal(tx2) {
		t.Error("identical TransactionIDs should be equal")
	}
}

func TestTransactionIDEqualFalse(t *testing.T) {
	tx1 := TransactionID([]byte{0x01, 0x02})
	tx2 := TransactionID([]byte{0x03, 0x04})
	if tx1.Equal(tx2) {
		t.Error("different TransactionIDs should not be equal")
	}
}

func TestTransactionIDString(t *testing.T) {
	tx := TransactionID([]byte{0x41, 0x42}) // "AB"
	if tx.String() != "AB" {
		t.Errorf("String(): got %q, want %q", tx.String(), "AB")
	}
}
