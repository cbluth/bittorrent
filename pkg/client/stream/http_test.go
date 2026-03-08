package stream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cbluth/bittorrent/pkg/client/download"
)

// ── parseRange ────────────────────────────────────────────────────────────────

func TestParseRangeBasic(t *testing.T) {
	ranges, err := parseRange("bytes=0-499", 1000)
	if err != nil {
		t.Fatalf("parseRange: %v", err)
	}
	if len(ranges) != 1 {
		t.Fatalf("len(ranges): got %d, want 1", len(ranges))
	}
	if ranges[0].start != 0 || ranges[0].end != 499 {
		t.Errorf("range: got [%d, %d], want [0, 499]", ranges[0].start, ranges[0].end)
	}
}

func TestParseRangeOpenEnd(t *testing.T) {
	ranges, err := parseRange("bytes=500-", 1000)
	if err != nil {
		t.Fatalf("parseRange: %v", err)
	}
	if len(ranges) != 1 {
		t.Fatalf("len(ranges): got %d, want 1", len(ranges))
	}
	if ranges[0].start != 500 || ranges[0].end != 999 {
		t.Errorf("range: got [%d, %d], want [500, 999]", ranges[0].start, ranges[0].end)
	}
}

func TestParseRangeSuffix(t *testing.T) {
	// "bytes=-200" means last 200 bytes of a 1000-byte file: [800, 999].
	ranges, err := parseRange("bytes=-200", 1000)
	if err != nil {
		t.Fatalf("parseRange: %v", err)
	}
	if len(ranges) != 1 {
		t.Fatalf("len(ranges): got %d, want 1", len(ranges))
	}
	if ranges[0].start != 800 || ranges[0].end != 999 {
		t.Errorf("range: got [%d, %d], want [800, 999]", ranges[0].start, ranges[0].end)
	}
}

func TestParseRangeSuffixLargerThanFile(t *testing.T) {
	// Suffix larger than file size is clamped to the whole file.
	ranges, err := parseRange("bytes=-5000", 1000)
	if err != nil {
		t.Fatalf("parseRange: %v", err)
	}
	if ranges[0].start != 0 || ranges[0].end != 999 {
		t.Errorf("range: got [%d, %d], want [0, 999]", ranges[0].start, ranges[0].end)
	}
}

func TestParseRangeEndBeyondSize(t *testing.T) {
	// End is clamped to size-1.
	ranges, err := parseRange("bytes=0-9999", 1000)
	if err != nil {
		t.Fatalf("parseRange: %v", err)
	}
	if ranges[0].end != 999 {
		t.Errorf("end: got %d, want 999 (clamped to size-1)", ranges[0].end)
	}
}

func TestParseRangeStartBeyondSize(t *testing.T) {
	_, err := parseRange("bytes=1000-1500", 1000)
	if err == nil {
		t.Error("start >= size should return an error")
	}
}

func TestParseRangeEndBeforeStart(t *testing.T) {
	_, err := parseRange("bytes=500-100", 1000)
	if err == nil {
		t.Error("end < start should return an error")
	}
}

func TestParseRangeEmpty(t *testing.T) {
	ranges, err := parseRange("", 1000)
	if err != nil {
		t.Fatalf("parseRange empty: %v", err)
	}
	if ranges != nil {
		t.Errorf("empty header: got %v, want nil", ranges)
	}
}

func TestParseRangeNoPrefix(t *testing.T) {
	_, err := parseRange("0-499", 1000)
	if err == nil {
		t.Error("missing 'bytes=' prefix should return an error")
	}
}

func TestParseRangeInvalidStart(t *testing.T) {
	_, err := parseRange("bytes=abc-499", 1000)
	if err == nil {
		t.Error("non-numeric start should return an error")
	}
}

func TestParseRangeNegativeStart(t *testing.T) {
	_, err := parseRange("bytes=-1-499", 1000)
	if err == nil {
		t.Error("negative start should return an error")
	}
}

func TestParseRangeMultipleRanges(t *testing.T) {
	// parseRange supports multiple ranges in a single header.
	// The HTTP handler rejects multi-range requests (len != 1 → 416), but the
	// parser itself returns them all without error.
	ranges, err := parseRange("bytes=0-100,200-300", 1000)
	if err != nil {
		t.Fatalf("parseRange multi: %v", err)
	}
	if len(ranges) != 2 {
		t.Errorf("len(ranges): got %d, want 2", len(ranges))
	}
	if ranges[0].start != 0 || ranges[0].end != 100 {
		t.Errorf("range[0]: got [%d, %d], want [0, 100]", ranges[0].start, ranges[0].end)
	}
	if ranges[1].start != 200 || ranges[1].end != 300 {
		t.Errorf("range[1]: got [%d, %d], want [200, 300]", ranges[1].start, ranges[1].end)
	}
}

// ── ServeHTTP method guard ────────────────────────────────────────────────────

func TestServeHTTPMethodNotAllowed(t *testing.T) {
	cfg := download.DownloaderConfig{PieceSize: 512 * 1024}
	sh := NewStreamHandler(cfg, nil)
	defer sh.Stop()

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/", nil)
		sh.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got status %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}
