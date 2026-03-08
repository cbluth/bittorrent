package dht

// BEP 5: DHT Protocol
// https://www.bittorrent.org/beps/bep_0005.html
//
// This file implements core types for the Kademlia-based DHT:
// - Key: 20-byte node ID or info hash with XOR distance metric
// - TransactionID: KRPC transaction identifiers
// - Distance calculation using XOR metric for Kademlia routing

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Key represents a 20-byte DHT node ID or info hash.
// BEP 5: both node IDs and info hashes occupy the same 160-bit key space.
type Key [20]byte

// TransactionID represents a KRPC transaction ID.
// BEP 5: 2-byte opaque string echoed in responses to match requests.
type TransactionID []byte

const txLength = 2 // BEP 5: transaction ID length (2 bytes is conventional)

// RandomKey generates a random 20-byte key suitable for DHT node IDs.
// Keys are uniformly random binary, matching BEP 5 expectations for Kademlia XOR distance.
func RandomKey() Key {
	k := Key{}
	_, _ = rand.Read(k[:]) // never fails on supported platforms (Go 1.20+)
	return k
}

// KeyFromBytes creates a Key from a byte slice
func KeyFromBytes(b []byte) (Key, error) {
	if len(b) != 20 {
		return Key{}, fmt.Errorf("key must be 20 bytes, got %d", len(b))
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

// KeyFromHex creates a Key from a hex string
func KeyFromHex(s string) (Key, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return Key{}, fmt.Errorf("invalid hex string: %w", err)
	}
	return KeyFromBytes(b)
}

// Empty returns true if the key is all zeros
func (k Key) Empty() bool {
	return k == Key{}
}

// String returns the key as a string
func (k Key) String() string {
	return string(k[:])
}

// Hex returns the key as a hexadecimal string
func (k Key) Hex() string {
	return hex.EncodeToString(k[:])
}

// Base64 returns the key as a URL-safe base64 string without padding
func (k Key) Base64() string {
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(k[:])
}

// Distance computes the XOR distance between two keys.
// BEP 5 §Routing Table: Kademlia XOR metric — distance(A, B) = A XOR B.
// Closer keys share longer common prefixes in binary representation.
func (k Key) Distance(other Key) Key {
	var d Key
	for i := range k {
		d[i] = k[i] ^ other[i]
	}
	return d
}

// Bytes returns the key as a byte slice
func (k Key) Bytes() []byte {
	return k[:]
}

// Array20 returns the key as a [20]byte array (for compatibility with protocol functions)
func (k Key) Array20() [20]byte {
	return k
}

// Less returns true if this key is less than the other key (for sorting)
func (k Key) Less(other Key) bool {
	return bytes.Compare(k[:], other[:]) < 0
}

// RandomTransactionID generates a random transaction ID for KRPC messages.
// BEP 5: "t" field — short binary string used to correlate requests and responses.
func RandomTransactionID() TransactionID {
	t := make(TransactionID, txLength)
	_, _ = rand.Read(t) // never fails on supported platforms (Go 1.20+)
	return t
}

// String returns the transaction ID as a string
func (t TransactionID) String() string {
	return string(t)
}

// Bytes returns the transaction ID as a byte slice
func (t TransactionID) Bytes() []byte {
	return []byte(t)
}

// Equal returns true if two transaction IDs are equal
func (t TransactionID) Equal(other TransactionID) bool {
	return bytes.Equal(t, other)
}
