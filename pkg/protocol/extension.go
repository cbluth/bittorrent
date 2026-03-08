package protocol

// BEP 10: Extension Protocol
// https://www.bittorrent.org/beps/bep_0010.html
//
// This file implements the extension protocol handshake:
// - Extension handshake message format
// - Extension ID negotiation for ut_metadata and other extensions
// - Metadata size advertisement
// - Extension capability announcement

import (
	"fmt"

	"github.com/cbluth/bittorrent/pkg/bencode"
)

// ExtensionHandshake represents the BEP 10 extension handshake.
// Sent as extended message with ext_id=0 immediately after the BEP 3 handshake.
type ExtensionHandshake struct {
	M            map[string]int `bencode:"m"`                       // BEP 10: dict of extension name → local message ID
	P            int            `bencode:"p,omitempty"`             // BEP 10: local TCP listen port
	V            string         `bencode:"v,omitempty"`             // BEP 10: client name and version
	YourIP       string         `bencode:"yourip,omitempty"`        // BEP 10: compact IP of the remote peer
	IPv6         string         `bencode:"ipv6,omitempty"`          // BEP 10: compact IPv6 address
	IPv4         string         `bencode:"ipv4,omitempty"`          // BEP 10: compact IPv4 address
	Reqq         int            `bencode:"reqq,omitempty"`          // BEP 10: max outstanding request hint
	MetadataSize int            `bencode:"metadata_size,omitempty"` // BEP 9: total size of info dict in bytes
	UploadOnly   int            `bencode:"upload_only,omitempty"`   // BEP 21: 1 = partial seed (not downloading)
}

// NewExtensionHandshake creates a minimal extension handshake with metadata support.
// BEP 10: the "m" dict declares the extension IDs THIS peer will use when SENDING.
// BEP 9: "ut_metadata" enables metadata exchange for magnet link resolution.
func NewExtensionHandshake(port int) *ExtensionHandshake {
	return &ExtensionHandshake{
		M: map[string]int{
			"ut_metadata": 1, // BEP 9: we send ut_metadata messages with ext_id=1
		},
	}
}

// Serialize encodes the extension handshake to bencode
func (e *ExtensionHandshake) Serialize() ([]byte, error) {
	return bencode.EncodeBytes(e)
}

// ParseExtensionHandshake parses an extension handshake from bencode
func ParseExtensionHandshake(data []byte) (*ExtensionHandshake, error) {
	var h ExtensionHandshake
	if err := bencode.DecodeBytes(data, &h); err != nil {
		return nil, fmt.Errorf("failed to decode extension handshake: %w", err)
	}
	return &h, nil
}

// GetMetadataExtID returns the peer's declared extension ID for ut_metadata (BEP 9).
// This is the ID the PEER will use when sending ut_metadata messages to us.
func (e *ExtensionHandshake) GetMetadataExtID() (int, bool) {
	id, ok := e.M["ut_metadata"]
	return id, ok
}

// HasMetadataSupport checks if peer supports metadata exchange (BEP 9).
func (e *ExtensionHandshake) HasMetadataSupport() bool {
	_, ok := e.M["ut_metadata"]
	return ok
}
