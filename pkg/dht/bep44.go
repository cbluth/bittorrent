package dht

// BEP 44: Storing arbitrary data in the DHT
// https://www.bittorrent.org/beps/bep_0044.html
//
// This file implements BEP 44 support for storing and retrieving arbitrary data in the DHT:
// - Immutable items: key = SHA-1(value)
// - Mutable items: key = SHA-1(public_key + salt), with ed25519 signatures

import (
	"crypto/sha1"
	"fmt"
	"net"

	"github.com/cbluth/bittorrent/pkg/bencode"
)

// GetImmutable sends a get query for an immutable item (BEP 44)
// target is the SHA-1 hash of the value
func GetImmutable(conn *net.UDPConn, addr *net.UDPAddr, nodeID, target Key, timeoutSeconds int) (TransactionID, *Message, error) {
	args := Arguments{
		ID:     nodeID[:],
		Target: target[:],
	}

	query := NewQuery(MethodGet, args)

	resp, err := SendQuery(conn, addr, query, timeoutSeconds)
	if err != nil {
		return nil, nil, err
	}

	return query.T, resp, nil
}

// GetMutable sends a get query for a mutable item (BEP 44)
// target is the SHA-1 hash of the public key (+ optional salt)
func GetMutable(conn *net.UDPConn, addr *net.UDPAddr, nodeID, target Key, timeoutSeconds int) (TransactionID, *Message, error) {
	// For mutable items, the target calculation is the same as immutable
	// The difference is in the response which will include k, seq, sig
	return GetImmutable(conn, addr, nodeID, target, timeoutSeconds)
}

// PutImmutable sends a put query for an immutable item (BEP 44)
// The target (key) is calculated as SHA-1(v)
func PutImmutable(conn *net.UDPConn, addr *net.UDPAddr, nodeID Key, token []byte, value interface{}, timeoutSeconds int) (TransactionID, *Message, error) {
	args := Arguments{
		ID:    nodeID[:],
		Token: token,
		V:     value,
	}

	query := NewQuery(MethodPut, args)

	resp, err := SendQuery(conn, addr, query, timeoutSeconds)
	if err != nil {
		return nil, nil, err
	}

	return query.T, resp, nil
}

// PutMutable sends a put query for a mutable item (BEP 44)
// publicKey is the 32-byte ed25519 public key
// signature is the 64-byte ed25519 signature of (salt + seq + v)
// cas is optional compare-and-swap value (expected sequence number)
func PutMutable(conn *net.UDPConn, addr *net.UDPAddr, nodeID Key, token []byte, publicKey []byte, seq int64, signature []byte, value interface{}, salt []byte, cas *int64, timeoutSeconds int) (TransactionID, *Message, error) {
	args := Arguments{
		ID:    nodeID[:],
		Token: token,
		K:     publicKey,
		Seq:   &seq,
		Sig:   signature,
		V:     value,
	}

	if len(salt) > 0 {
		args.Salt = salt
	}

	if cas != nil {
		args.CAS = cas
	}

	query := NewQuery(MethodPut, args)

	resp, err := SendQuery(conn, addr, query, timeoutSeconds)
	if err != nil {
		return nil, nil, err
	}

	return query.T, resp, nil
}

// CalculateImmutableTarget calculates the target key for an immutable item
// target = SHA-1(bencoded_value)
func CalculateImmutableTarget(value interface{}) (Key, error) {
	// Bencode the value
	encoded, err := bencode.EncodeBytes(value)
	if err != nil {
		return Key{}, fmt.Errorf("failed to bencode value: %w", err)
	}

	// Calculate SHA-1 hash
	hash := sha1.Sum(encoded)
	var key Key
	copy(key[:], hash[:])
	return key, nil
}

// CalculateMutableTarget calculates the target key for a mutable item
// target = SHA-1(public_key + salt)
func CalculateMutableTarget(publicKey []byte, salt []byte) (Key, error) {
	if len(publicKey) != 32 {
		return Key{}, fmt.Errorf("public key must be 32 bytes, got %d", len(publicKey))
	}

	// Concatenate public key and salt
	data := make([]byte, len(publicKey)+len(salt))
	copy(data, publicKey)
	copy(data[len(publicKey):], salt)

	// Calculate SHA-1 hash
	hash := sha1.Sum(data)
	var key Key
	copy(key[:], hash[:])
	return key, nil
}

// VerifyImmutableItem verifies that an immutable item's value matches its target
func VerifyImmutableItem(target Key, value interface{}) error {
	calculated, err := CalculateImmutableTarget(value)
	if err != nil {
		return err
	}

	if calculated != target {
		return fmt.Errorf("immutable item verification failed: target mismatch")
	}

	return nil
}

// GetSignaturePayload returns the bencoded payload that should be signed for mutable items
// payload = bencode(salt) + bencode(seq) + bencode(value)
// If salt is empty, it is omitted from the signature
func GetSignaturePayload(seq int64, value interface{}, salt []byte) ([]byte, error) {
	var payload []byte

	// Add salt if present (and non-empty)
	if len(salt) > 0 {
		saltEncoded, err := bencode.EncodeBytes(map[string]interface{}{"salt": salt})
		if err != nil {
			return nil, fmt.Errorf("failed to bencode salt: %w", err)
		}
		// Extract just the "4:salt<N>:<value>" part
		payload = append(payload, saltEncoded[1:len(saltEncoded)-1]...)
	}

	// Add sequence number
	seqEncoded, err := bencode.EncodeBytes(map[string]interface{}{"seq": seq})
	if err != nil {
		return nil, fmt.Errorf("failed to bencode seq: %w", err)
	}
	// Extract just the "3:seqi<N>e" part
	payload = append(payload, seqEncoded[1:len(seqEncoded)-1]...)

	// Add value
	valueEncoded, err := bencode.EncodeBytes(map[string]interface{}{"v": value})
	if err != nil {
		return nil, fmt.Errorf("failed to bencode value: %w", err)
	}
	// Extract just the "1:v<encoded_value>" part
	payload = append(payload, valueEncoded[1:len(valueEncoded)-1]...)

	return payload, nil
}
