package magnet

// BEP 9: Extension for Peers to Send Metadata Files - Magnet Links
// https://www.bittorrent.org/beps/bep_0009.html
//
// This file implements magnet link parsing:
// - Magnet URI format: magnet:?xt=urn:btih:<info-hash>&...
// - Info hash extraction (hex or base32 encoding)
// - Tracker list parsing (tr parameters)
// - Display name and exact topic (dn, xt parameters)
// - Exact length and source (xl, xs parameters)

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cbluth/bittorrent/pkg/dht"
)

// Magnet represents a parsed magnet link.
// BEP 9: magnet URI format: magnet:?xt=urn:btih:<info-hash>&dn=<name>&tr=<tracker>
type Magnet struct {
	InfoHash    dht.Key  // BEP 9: xt=urn:btih:<20-byte SHA-1 info hash>
	DisplayName string   // BEP 9: dn — suggested display name
	Trackers    []string // BEP 9: tr — tracker URLs for peer discovery
	ExactLength int64    // xl — total size in bytes (optional)
	ExactSource string   // xs — exact source URL (optional)
}

// Parse parses a magnet URI and returns a Magnet struct
func Parse(magnetURI string) (*Magnet, error) {
	if !strings.HasPrefix(magnetURI, "magnet:?") {
		return nil, errors.New("invalid magnet URI: must start with 'magnet:?'")
	}

	u, err := url.Parse(magnetURI)
	if err != nil {
		return nil, fmt.Errorf("failed to parse magnet URI: %w", err)
	}

	query := u.Query()

	// Extract xt (exact topic / info hash)
	xt := query.Get("xt")
	if xt == "" {
		return nil, errors.New("magnet URI missing required 'xt' parameter")
	}

	infoHash, err := parseInfoHash(xt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse info hash: %w", err)
	}

	magnet := &Magnet{
		InfoHash:    infoHash,
		DisplayName: query.Get("dn"),
		Trackers:    query["tr"], // Get all tracker parameters
	}

	// Parse xl (exact length) if present
	if xl := query.Get("xl"); xl != "" {
		var length int64
		if _, err := fmt.Sscanf(xl, "%d", &length); err == nil {
			magnet.ExactLength = length
		}
	}

	// Parse xs (exact source) if present
	magnet.ExactSource = query.Get("xs")

	return magnet, nil
}

// parseInfoHash extracts the info hash from the xt parameter.
// BEP 9: xt = urn:btih:<info-hash> where info-hash is hex (40 chars) or base32 (32 chars).
func parseInfoHash(xt string) (dht.Key, error) {
	var hash dht.Key

	// BEP 9: xt must start with "urn:btih:" (BitTorrent Info Hash)
	if !strings.HasPrefix(xt, "urn:btih:") {
		return hash, errors.New("xt parameter must start with 'urn:btih:'")
	}

	hashStr := strings.TrimPrefix(xt, "urn:btih:")

	// Support both hex (40 chars) and base32 (32 chars) formats
	if len(hashStr) == 40 {
		// Hex format
		decoded, err := hex.DecodeString(hashStr)
		if err != nil {
			return hash, fmt.Errorf("invalid hex info hash: %w", err)
		}
		if len(decoded) != 20 {
			return hash, errors.New("info hash must be 20 bytes")
		}
		copy(hash[:], decoded)
	} else if len(hashStr) == 32 {
		// Base32 format
		decoded, err := decodeBase32(hashStr)
		if err != nil {
			return hash, fmt.Errorf("invalid base32 info hash: %w", err)
		}
		copy(hash[:], decoded)
	} else {
		return hash, fmt.Errorf("invalid info hash length: %d (expected 32 or 40)", len(hashStr))
	}

	return hash, nil
}

// decodeBase32 decodes a base32-encoded string (RFC 4648)
func decodeBase32(s string) ([]byte, error) {
	// Convert to uppercase for standard base32
	s = strings.ToUpper(s)

	const base32Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	decoded := make([]byte, 0, 20)

	// Process in groups of 8 characters (40 bits = 5 bytes)
	var buffer uint64
	var bitsInBuffer int

	for _, c := range s {
		if c == '=' {
			break
		}

		val := strings.IndexRune(base32Alphabet, c)
		if val == -1 {
			return nil, fmt.Errorf("invalid base32 character: %c", c)
		}

		buffer = (buffer << 5) | uint64(val)
		bitsInBuffer += 5

		if bitsInBuffer >= 8 {
			bitsInBuffer -= 8
			decoded = append(decoded, byte(buffer>>uint(bitsInBuffer)))
			buffer &= (1 << uint(bitsInBuffer)) - 1
		}
	}

	return decoded, nil
}

// String returns the magnet URI representation
func (m *Magnet) String() string {
	u := url.URL{
		Scheme: "magnet",
	}

	q := url.Values{}
	q.Set("xt", fmt.Sprintf("urn:btih:%x", m.InfoHash))

	if m.DisplayName != "" {
		q.Set("dn", m.DisplayName)
	}

	for _, tracker := range m.Trackers {
		q.Add("tr", tracker)
	}

	if m.ExactLength > 0 {
		q.Set("xl", fmt.Sprintf("%d", m.ExactLength))
	}

	if m.ExactSource != "" {
		q.Set("xs", m.ExactSource)
	}

	u.RawQuery = q.Encode()
	return u.String()
}

// FromInfoHash creates a basic magnet link from an info hash
func FromInfoHash(infoHash dht.Key) *Magnet {
	return &Magnet{
		InfoHash: infoHash,
	}
}

// AddTracker adds a tracker to the magnet link
func (m *Magnet) AddTracker(tracker string) {
	m.Trackers = append(m.Trackers, tracker)
}

// AddTrackers adds multiple trackers to the magnet link
func (m *Magnet) AddTrackers(trackers []string) {
	m.Trackers = append(m.Trackers, trackers...)
}
