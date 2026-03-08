package dht

// BEP 42: DHT Security Extension
// https://www.bittorrent.org/beps/bep_0042.html
//
// Restricts DHT node IDs based on the node's external IP address to prevent
// Sybil attacks. The first 21 bits of the node ID must match a CRC32c hash
// of the masked IP address, and the last byte must equal the random value r.

import (
	"crypto/rand"
	"hash/crc32"
	"net"
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// IPv4 mask: 0x030f3fff applied byte-by-byte
var v4Mask = [4]byte{0x03, 0x0f, 0x3f, 0xff}

// IPv6 mask: 0x0103070f1f3f7fff applied to the high 8 bytes
var v6Mask = [8]byte{0x01, 0x03, 0x07, 0x0f, 0x1f, 0x3f, 0x7f, 0xff}

// GenerateSecureNodeID creates a BEP 42-compliant node ID for the given IP.
// The first 21 bits are derived from CRC32c(masked_ip | r<<29),
// bytes 3-18 are random, and byte 19 stores the rand value used.
func GenerateSecureNodeID(ip net.IP) Key {
	var id Key

	// Generate random r in [0, 7]
	var rb [1]byte
	rand.Read(rb[:])
	r := rb[0] & 0x07

	crc := computeBEP42CRC(ip, r)

	// First 21 bits from CRC
	id[0] = byte(crc >> 24)
	id[1] = byte(crc >> 16)
	id[2] = (byte(crc>>8) & 0xf8) // top 5 bits from CRC

	// Fill bits 3..7 of byte 2 and bytes 3-18 with random data
	var rnd [17]byte
	rand.Read(rnd[:])
	id[2] |= rnd[0] & 0x07 // low 3 bits of byte 2 are random
	copy(id[3:19], rnd[1:])

	// Last byte is the rand value
	id[19] = rb[0]

	return id
}

// ValidateNodeID checks whether a node ID is BEP 42-compliant for the given IP.
// Returns true if the first 21 bits match the expected CRC32c hash.
// Nodes on local/private networks are always valid (exempt per spec).
func ValidateNodeID(id Key, ip net.IP) bool {
	if isLocalIP(ip) {
		return true
	}

	r := id[19] & 0x07
	crc := computeBEP42CRC(ip, r)

	// Check first 21 bits (bytes 0-1 fully, top 5 bits of byte 2)
	if id[0] != byte(crc>>24) {
		return false
	}
	if id[1] != byte(crc>>16) {
		return false
	}
	if (id[2] & 0xf8) != (byte(crc>>8) & 0xf8) {
		return false
	}
	return true
}

// computeBEP42CRC computes the CRC32c hash per BEP 42.
// For IPv4: crc32c((ip & 0x030f3fff) | (r << 29)) over 4 bytes.
// For IPv6: crc32c((ip[0:8] & 0x0103070f1f3f7fff) | (r << 61)) over 8 bytes.
// The masked value is big-endian before hashing.
func computeBEP42CRC(ip net.IP, r byte) uint32 {
	ip4 := ip.To4()
	if ip4 != nil {
		var buf [4]byte
		for i := 0; i < 4; i++ {
			buf[i] = ip4[i] & v4Mask[i]
		}
		buf[0] |= r << 5
		return crc32.Checksum(buf[:], crc32cTable)
	}

	// IPv6: use high 64 bits
	ip6 := ip.To16()
	if ip6 == nil {
		return 0
	}
	var buf [8]byte
	for i := 0; i < 8; i++ {
		buf[i] = ip6[i] & v6Mask[i]
	}
	buf[0] |= r << 5
	return crc32.Checksum(buf[:], crc32cTable)
}

// isLocalIP returns true if the IP is in a local/private/loopback range.
// BEP 42: these are exempt from node ID verification.
func isLocalIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	return false
}
