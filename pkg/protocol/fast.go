package protocol

// BEP 6: Fast Extension — Allowed Fast Set algorithm.
// https://www.bittorrent.org/beps/bep_0006.html

import (
	"crypto/sha1"
	"encoding/binary"
	"net"
)

// AllowedFastSet computes the canonical allowed fast set per BEP 6.
//
// Algorithm:
//
//	x = (ip & 0xFFFFFF00) ++ infohash   (24 bytes)
//	while |a| < k:
//	    x = SHA1(x)
//	    for i in 0..4 and |a| < k:
//	        index = BigEndian(x[i*4 : i*4+4]) % numPieces
//	        if index not in a: add to a
func AllowedFastSet(k int, numPieces uint32, infoHash [20]byte, ip net.IP) []uint32 {
	if numPieces == 0 || k <= 0 {
		return nil
	}

	// Cap k at numPieces — can't produce more unique indices than pieces exist.
	if uint32(k) > numPieces {
		k = int(numPieces)
	}

	// Use IPv4 form (BEP 6 specifies a 4-byte IP).
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}

	// x = (ip & 0xFFFFFF00) ++ infohash  → 24 bytes
	x := make([]byte, 24)
	copy(x[0:4], ip4)
	x[3] = 0 // mask to /24
	copy(x[4:24], infoHash[:])

	seen := make(map[uint32]bool, k)
	result := make([]uint32, 0, k)

	for len(result) < k {
		hash := sha1.Sum(x)
		x = hash[:] // feed SHA1 output back as input

		for i := 0; i < 5 && len(result) < k; i++ {
			idx := binary.BigEndian.Uint32(x[i*4:i*4+4]) % numPieces
			if !seen[idx] {
				seen[idx] = true
				result = append(result, idx)
			}
		}
	}

	return result
}
