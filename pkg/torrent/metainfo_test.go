package torrent

import (
	"bytes"
	"strings"
	"testing"
)

// ── InfoDict.IsSingleFile ─────────────────────────────────────────────────────

func TestIsSingleFileTrue(t *testing.T) {
	info := &InfoDict{Length: 1024}
	if !info.IsSingleFile() {
		t.Error("InfoDict with Length set should be single-file")
	}
}

func TestIsSingleFileFalse(t *testing.T) {
	info := &InfoDict{
		Files: []FileInfo{
			{Length: 512, Path: []string{"a.txt"}},
		},
	}
	if info.IsSingleFile() {
		t.Error("InfoDict with Files set should not be single-file")
	}
}

// ── InfoDict.TotalLength ──────────────────────────────────────────────────────

func TestTotalLengthSingleFile(t *testing.T) {
	info := &InfoDict{Length: 1024 * 1024}
	if got := info.TotalLength(); got != 1024*1024 {
		t.Errorf("TotalLength: got %d, want %d", got, 1024*1024)
	}
}

func TestTotalLengthMultiFile(t *testing.T) {
	info := &InfoDict{
		Files: []FileInfo{
			{Length: 100, Path: []string{"a.txt"}},
			{Length: 200, Path: []string{"b.txt"}},
			{Length: 300, Path: []string{"c.txt"}},
		},
	}
	if got := info.TotalLength(); got != 600 {
		t.Errorf("TotalLength: got %d, want 600", got)
	}
}

func TestTotalLengthEmpty(t *testing.T) {
	info := &InfoDict{}
	if got := info.TotalLength(); got != 0 {
		t.Errorf("TotalLength of empty InfoDict: got %d, want 0", got)
	}
}

// ── InfoDict.GetPieceHashes ───────────────────────────────────────────────────

func TestGetPieceHashesValid(t *testing.T) {
	// 2 pieces = 40 bytes of SHA-1 hashes.
	info := &InfoDict{Pieces: strings.Repeat("\x00", 40)}
	hashes, err := info.GetPieceHashes()
	if err != nil {
		t.Fatalf("GetPieceHashes: %v", err)
	}
	if len(hashes) != 2 {
		t.Errorf("len(hashes): got %d, want 2", len(hashes))
	}
}

func TestGetPieceHashesDistinct(t *testing.T) {
	// Two pieces with distinct hash bytes.
	p1 := strings.Repeat("\x01", 20)
	p2 := strings.Repeat("\x02", 20)
	info := &InfoDict{Pieces: p1 + p2}

	hashes, err := info.GetPieceHashes()
	if err != nil {
		t.Fatalf("GetPieceHashes: %v", err)
	}
	if hashes[0][0] != 0x01 {
		t.Errorf("hashes[0][0]: got %02x, want 01", hashes[0][0])
	}
	if hashes[1][0] != 0x02 {
		t.Errorf("hashes[1][0]: got %02x, want 02", hashes[1][0])
	}
}

func TestGetPieceHashesInvalidLength(t *testing.T) {
	// 21 bytes — not a multiple of 20.
	info := &InfoDict{Pieces: strings.Repeat("\x00", 21)}
	_, err := info.GetPieceHashes()
	if err == nil {
		t.Error("GetPieceHashes should fail for non-multiple-of-20 pieces length")
	}
}

func TestGetPieceHashesEmpty(t *testing.T) {
	info := &InfoDict{Pieces: ""}
	hashes, err := info.GetPieceHashes()
	if err != nil {
		t.Fatalf("GetPieceHashes empty: %v", err)
	}
	if len(hashes) != 0 {
		t.Errorf("empty pieces: got %d hashes, want 0", len(hashes))
	}
}

// ── MetaInfo.GetTrackers ──────────────────────────────────────────────────────

func TestGetTrackersDeduplicated(t *testing.T) {
	m := &MetaInfo{
		Announce: "http://tracker1.example.com/announce",
		AnnounceList: [][]string{
			{"http://tracker2.example.com/announce"},
			{"http://tracker1.example.com/announce"}, // duplicate of Announce
			{"http://tracker3.example.com/announce"},
		},
	}
	trackers := m.GetTrackers()
	if len(trackers) != 3 {
		t.Errorf("GetTrackers: got %d trackers, want 3 (got %v)", len(trackers), trackers)
	}
}

func TestGetTrackersAnnounceOnly(t *testing.T) {
	m := &MetaInfo{Announce: "udp://tracker.example.com:6881/announce"}
	trackers := m.GetTrackers()
	if len(trackers) != 1 || trackers[0] != m.Announce {
		t.Errorf("GetTrackers: got %v, want [%s]", trackers, m.Announce)
	}
}

func TestGetTrackersEmpty(t *testing.T) {
	m := &MetaInfo{}
	trackers := m.GetTrackers()
	if len(trackers) != 0 {
		t.Errorf("GetTrackers with no announce: got %v", trackers)
	}
}

func TestGetTrackersSkipsEmpty(t *testing.T) {
	m := &MetaInfo{
		Announce:     "http://tracker1.example.com/announce",
		AnnounceList: [][]string{{"", "http://tracker2.example.com/announce"}},
	}
	trackers := m.GetTrackers()
	for _, tr := range trackers {
		if tr == "" {
			t.Error("GetTrackers should not include empty tracker URLs")
		}
	}
}

// ── Parse / Write round-trip ──────────────────────────────────────────────────

func TestParseWriteRoundTrip(t *testing.T) {
	original := &MetaInfo{
		Announce: "http://tracker.example.com/announce",
		Info: InfoDict{
			Name:        "test-torrent",
			PieceLength: 524288,
			Pieces:      strings.Repeat("\x00", 20), // 1 piece hash
			Length:      524288,
		},
	}

	var buf bytes.Buffer
	if err := original.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}

	parsed, err := Parse(&buf)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if parsed.Announce != original.Announce {
		t.Errorf("Announce: got %q, want %q", parsed.Announce, original.Announce)
	}
	if parsed.Info.Name != original.Info.Name {
		t.Errorf("Name: got %q, want %q", parsed.Info.Name, original.Info.Name)
	}
	if parsed.Info.PieceLength != original.Info.PieceLength {
		t.Errorf("PieceLength: got %d, want %d", parsed.Info.PieceLength, original.Info.PieceLength)
	}
	if parsed.Info.TotalLength() != original.Info.TotalLength() {
		t.Errorf("TotalLength: got %d, want %d", parsed.Info.TotalLength(), original.Info.TotalLength())
	}
	if parsed.InfoHash.Empty() {
		t.Error("InfoHash should not be empty after Parse")
	}
}

func TestInfoHashDeterministic(t *testing.T) {
	// Writing and re-parsing the same MetaInfo should produce the same InfoHash.
	info := &MetaInfo{
		Announce: "http://tracker.example.com/announce",
		Info: InfoDict{
			Name:        "deterministic",
			PieceLength: 262144,
			Pieces:      strings.Repeat("\xAB", 20),
			Length:      262144,
		},
	}

	hash1 := func() string {
		var buf bytes.Buffer
		info.Write(&buf)
		m, _ := Parse(&buf)
		return m.InfoHash.Hex()
	}()

	hash2 := func() string {
		var buf bytes.Buffer
		info.Write(&buf)
		m, _ := Parse(&buf)
		return m.InfoHash.Hex()
	}()

	if hash1 != hash2 {
		t.Errorf("InfoHash is not deterministic: %s vs %s", hash1, hash2)
	}
}
