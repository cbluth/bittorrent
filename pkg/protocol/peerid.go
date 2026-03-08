package protocol

// BEP 20: Peer ID Conventions
// https://www.bittorrent.org/beps/bep_0020.html
//
// This file implements peer ID generation:
// - Azureus-style peer ID format: -XX0000-<12 random chars>
// - Client identification in handshakes
// - Random peer ID generation for anonymity

import (
	"crypto/rand"
	"fmt"

	"github.com/cbluth/bittorrent/pkg/dht"
)

// GeneratePeerID creates a unique peer ID following Azureus-style convention
// Format: -XX0000-<12 random chars>
func GeneratePeerID(clientID string, version string) (dht.Key, error) {
	var peerID dht.Key

	prefix := fmt.Sprintf("-%s%s-", clientID, version)
	if len(prefix) > 8 {
		return peerID, fmt.Errorf("client ID and version too long")
	}

	copy(peerID[:], prefix)

	_, err := rand.Read(peerID[len(prefix):])
	if err != nil {
		return peerID, fmt.Errorf("failed to generate random bytes: %w", err)
	}

	return peerID, nil
}

// DefaultPeerID generates a peer ID with default client identification
func DefaultPeerID() (dht.Key, error) {
	return GeneratePeerID("BT", "0001")
}
