package torrent

// BEP 3: The BitTorrent Protocol Specification - Metainfo File Structure
// https://www.bittorrent.org/beps/bep_0003.html
//
// This file implements .torrent file parsing and generation:
// - MetaInfo structure (announce, announce-list, info dict)
// - InfoDict structure (name, pieces, files, piece length)
// - Info hash calculation (SHA-1 of bencoded info dict)
// - Support for single-file and multi-file torrents
// - Tracker list extraction and piece hash parsing

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cbluth/bittorrent/pkg/bencode"
	"github.com/cbluth/bittorrent/pkg/dht"
)

// MetaInfo represents the complete torrent file structure.
// BEP 3: top-level dictionary with "announce" and "info" keys.
type MetaInfo struct {
	Announce     string     `bencode:"announce"`                // BEP 3: tracker URL
	AnnounceList [][]string `bencode:"announce-list,omitempty"` // BEP 12: multi-tracker metadata extension (tiered tracker list)
	Comment      string     `bencode:"comment,omitempty"`       // BEP 3: optional free-form comment
	CreatedBy    string     `bencode:"created by,omitempty"`    // BEP 3: optional creator program name
	CreationDate int64      `bencode:"creation date,omitempty"` // BEP 3: optional creation timestamp (Unix epoch)
	Info         InfoDict   `bencode:"info"`                    // BEP 3: info dictionary (hashed to produce info_hash)
	InfoHash     dht.Key    `bencode:"-"`                       // BEP 3: SHA-1 of bencoded info dict — computed, not serialised
}

// InfoDict represents the info dictionary in a torrent file.
// BEP 3: the info dict is bencoded and SHA-1 hashed to produce the info_hash.
type InfoDict struct {
	PieceLength int64  `bencode:"piece length"`      // BEP 3: number of bytes per piece
	Pieces      string `bencode:"pieces"`            // BEP 3: concatenated 20-byte SHA-1 hashes of each piece
	Private     int64  `bencode:"private,omitempty"` // BEP 27: private torrent flag (disables DHT/PEX if 1)
	Name        string `bencode:"name"`              // BEP 3: suggested file/directory name

	// BEP 3 §Single-file mode: Length is set, Files is empty.
	Length int64 `bencode:"length,omitempty"`

	// BEP 3 §Multi-file mode: Files is set, Length is zero.
	Files []FileInfo `bencode:"files,omitempty"`
}

// FileInfo represents a file in multi-file torrents.
// BEP 3 §Multi-file: each file has a length and a path (list of path components).
type FileInfo struct {
	Length int64    `bencode:"length"` // BEP 3: file size in bytes
	Path   []string `bencode:"path"`   // BEP 3: list of path components (e.g. ["dir", "file.txt"])
}

// Parse parses a torrent metainfo from a reader
func Parse(r io.Reader) (*MetaInfo, error) {
	var meta MetaInfo

	if err := bencode.NewDecoder(r).Decode(&meta); err != nil {
		return nil, fmt.Errorf("failed to decode torrent: %w", err)
	}

	// Calculate info hash
	infoHash, err := calculateInfoHash(&meta.Info)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate info hash: %w", err)
	}
	meta.InfoHash = infoHash

	return &meta, nil
}

// Write writes a torrent metainfo to a writer
func (m *MetaInfo) Write(w io.Writer) error {
	return bencode.NewEncoder(w).Encode(m)
}

// calculateInfoHash computes the SHA-1 hash of the bencoded info dictionary.
// BEP 3: info_hash = SHA-1(bencode(info_dict)) — used in handshake, tracker, DHT, magnet.
func calculateInfoHash(info *InfoDict) (dht.Key, error) {
	var hash dht.Key

	encoded, err := bencode.EncodeBytes(info)
	if err != nil {
		return hash, err
	}

	hash = sha1.Sum(encoded)
	return hash, nil
}

// GetPieceHashes returns the list of piece hashes.
// BEP 3: "pieces" is a concatenation of 20-byte SHA-1 hashes, one per piece.
func (i *InfoDict) GetPieceHashes() ([]dht.Key, error) {
	if len(i.Pieces)%20 != 0 {
		return nil, errors.New("invalid pieces length") // BEP 3: must be multiple of 20
	}

	numPieces := len(i.Pieces) / 20
	hashes := make([]dht.Key, numPieces)

	for idx := 0; idx < numPieces; idx++ {
		copy(hashes[idx][:], i.Pieces[idx*20:(idx+1)*20])
	}

	return hashes, nil
}

// TotalLength returns the total size of the torrent
func (i *InfoDict) TotalLength() int64 {
	if i.Length > 0 {
		return i.Length
	}

	var total int64
	for _, file := range i.Files {
		total += file.Length
	}
	return total
}

// IsSingleFile returns true if this is a single-file torrent
func (i *InfoDict) IsSingleFile() bool {
	return len(i.Files) == 0
}

// IsPrivate returns true if the private flag is set (BEP 27).
// Private torrents MUST only use tracker-provided peers; DHT, PEX,
// and LSD are disabled.
func (i *InfoDict) IsPrivate() bool {
	return i.Private == 1
}

// GetTrackers returns all trackers from announce (BEP 3) and announce-list (BEP 12).
// BEP 12: announce-list is a list of lists (tiers); we flatten to a deduplicated list.
func (m *MetaInfo) GetTrackers() []string {
	trackers := make([]string, 0)

	if m.Announce != "" {
		trackers = append(trackers, m.Announce)
	}

	seen := make(map[string]bool)
	seen[m.Announce] = true

	for _, tier := range m.AnnounceList {
		for _, url := range tier {
			if !seen[url] && url != "" {
				trackers = append(trackers, url)
				seen[url] = true
			}
		}
	}

	return trackers
}

// ParseMetadata parses metadata from raw bytes received via BEP 9 ut_metadata exchange.
// BEP 9: after reassembling all metadata pieces, verify SHA-1 matches the expected info_hash.
func ParseMetadata(metadata []byte, infoHash dht.Key) (*MetaInfo, error) {
	var info InfoDict
	if err := bencode.DecodeBytes(metadata, &info); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %w", err)
	}

	// Verify the info hash
	calculatedHash := sha1.Sum(metadata)
	if calculatedHash != infoHash {
		return nil, fmt.Errorf("info hash mismatch")
	}

	return &MetaInfo{
		Info:     info,
		InfoHash: infoHash,
	}, nil
}

// MarshalJSON customizes JSON output for MetaInfo
func (m *MetaInfo) MarshalJSON() ([]byte, error) {
	return json.Marshal(&struct {
		Announce     string     `json:"announce"`
		AnnounceList [][]string `json:"announce-list,omitempty"`
		Comment      string     `json:"comment,omitempty"`
		CreatedBy    string     `json:"created by,omitempty"`
		CreationDate int64      `json:"creation date,omitempty"`
		Info         InfoDict   `json:"info"`
		InfoHash     string     `json:"info_hash"`
	}{
		Announce:     m.Announce,
		AnnounceList: m.AnnounceList,
		Comment:      m.Comment,
		CreatedBy:    m.CreatedBy,
		CreationDate: m.CreationDate,
		Info:         m.Info,
		InfoHash:     hex.EncodeToString(m.InfoHash[:]),
	})
}

// MarshalJSON customizes JSON output for InfoDict
func (i *InfoDict) MarshalJSON() ([]byte, error) {
	// Convert pieces string to array of hex-encoded hashes
	pieces := []string{}
	if len(i.Pieces)%20 == 0 {
		for idx := 0; idx < len(i.Pieces); idx += 20 {
			pieceHash := i.Pieces[idx : idx+20]
			pieces = append(pieces, hex.EncodeToString([]byte(pieceHash)))
		}
	}

	type Alias InfoDict
	return json.Marshal(&struct {
		PieceLength int64      `json:"piece length"`
		Pieces      []string   `json:"pieces"`
		Private     int64      `json:"private,omitempty"`
		Name        string     `json:"name"`
		Length      int64      `json:"length,omitempty"`
		Files       []FileInfo `json:"files,omitempty"`
	}{
		PieceLength: i.PieceLength,
		Pieces:      pieces,
		Private:     i.Private,
		Name:        i.Name,
		Length:      i.Length,
		Files:       i.Files,
	})
}
