package dht

// BEP 51: DHT Infohash Indexing
// https://www.bittorrent.org/beps/bep_0051.html
//
// This file implements BEP 51 support for sampling infohashes from DHT nodes:
// - sample_infohashes query for DHT crawling/indexing
// - Enables surveying the entire DHT without non-compliant behavior

import (
	"fmt"
	"net"
)

// BEP 51 constants
const (
	MaxSampleInterval = 21600 // Maximum refresh interval (6 hours) in seconds
)

// SampleInfohashes sends a sample_infohashes query (BEP 51)
// This query requests that a remote node return a string of concatenated infohashes
// for which it holds get_peers values.
//
// The target parameter is used for keyspace traversal - it affects the returned nodes
// but not the sampled infohashes.
//
// Returns:
// - interval: how long (in seconds) before the next request (0-21600)
// - num: total number of infohashes in the node's storage
// - samples: concatenated 20-byte infohashes
// - nodes: nodes close to the target (for iterative lookups)
func SampleInfohashes(conn *net.UDPConn, addr *net.UDPAddr, nodeID, target Key, timeoutSeconds int) (TransactionID, *Message, error) {
	args := Arguments{
		ID:     nodeID[:],
		Target: target[:],
	}

	query := NewQuery(MethodSampleInfohashes, args)

	resp, err := SendQuery(conn, addr, query, timeoutSeconds)
	if err != nil {
		return nil, nil, err
	}

	return query.T, resp, nil
}

// ParseSampledInfohashes extracts individual infohashes from the concatenated samples field
// Each infohash is 20 bytes, so the samples should be a multiple of 20 bytes
func ParseSampledInfohashes(samples []byte) ([]Key, error) {
	if len(samples)%20 != 0 {
		return nil, fmt.Errorf("invalid samples length: %d (must be multiple of 20)", len(samples))
	}

	numInfohashes := len(samples) / 20
	infohashes := make([]Key, numInfohashes)

	for i := range numInfohashes {
		offset := i * 20
		copy(infohashes[i][:], samples[offset:offset+20])
	}

	return infohashes, nil
}
