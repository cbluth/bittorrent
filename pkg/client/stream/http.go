// Package stream provides HTTP streaming support for in-progress downloads
package stream

import (
	"context"
	"fmt"
	"github.com/cbluth/bittorrent/pkg/log"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cbluth/bittorrent/pkg/client/download"
)

// StreamingReadSeeker wraps an os.File and waits for data to be available
// This is used for serving incomplete files via HTTP while they're being downloaded
type StreamingReadSeeker struct {
	file       *os.File
	size       int64
	pieceSize  int64
	downloader *download.Downloader
	ctx        context.Context
	fileOffset int64 // Offset of this file within the torrent (for multi-file torrents)
}

// NewStreamingReadSeeker creates a new StreamingReadSeeker
func NewStreamingReadSeeker(file *os.File, size int64, pieceSize int64, downloader *download.Downloader, ctx context.Context, fileOffset int64) *StreamingReadSeeker {
	return &StreamingReadSeeker{
		file:       file,
		size:       size,
		pieceSize:  pieceSize,
		downloader: downloader,
		ctx:        ctx,
		fileOffset: fileOffset,
	}
}

// Read implements io.Reader
func (s *StreamingReadSeeker) Read(p []byte) (n int, err error) {
	// Get current position
	offset, err := s.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}

	log.Debug("read", "sub", "stream", "offset", offset, "bufSize", len(p))

	// Wait for data at this offset
	if err := s.waitForData(offset); err != nil {
		log.Debug("read waitForData failed", "sub", "stream", "err", err)
		return 0, err
	}

	// Read from file
	n, err = s.file.Read(p)
	log.Debug("read completed", "sub", "stream", "bytes", n, "err", err)
	return n, err
}

// Seek implements io.Seeker
func (s *StreamingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	// Intercept SeekEnd so http.ServeContent gets the declared torrent file size,
	// not the current (growing) on-disk size.
	if whence == io.SeekEnd {
		newOffset := s.size + offset
		if _, err := s.file.Seek(newOffset, io.SeekStart); err != nil {
			return 0, err
		}
		log.Info("seek end", "sub", "seek", "offset", offset, "size", s.size, "result", newOffset, "piece", uint32(newOffset/s.pieceSize))
		return newOffset, nil
	}
	newOffset, err := s.file.Seek(offset, whence)
	log.Info("seek", "sub", "seek", "offset", offset, "whence", whence, "result", newOffset, "piece", uint32(newOffset/s.pieceSize))
	return newOffset, err
}

// ReadAt implements io.ReaderAt
func (s *StreamingReadSeeker) ReadAt(p []byte, offset int64) (n int, err error) {
	log.Debug("readAt", "sub", "stream", "offset", offset, "bufSize", len(p))

	// Wait for data at this offset
	if err := s.waitForData(offset); err != nil {
		log.Debug("readAt waitForData failed", "sub", "stream", "err", err)
		return 0, err
	}

	// Read from file
	n, err = s.file.ReadAt(p, offset)
	log.Debug("readAt completed", "sub", "stream", "bytes", n, "err", err)
	return n, err
}

// waitForData waits until data is available at the given offset
func (s *StreamingReadSeeker) waitForData(offset int64) error {
	if offset >= s.size {
		log.Debug("waitForData EOF: offset past size", "sub", "stream", "offset", offset, "size", s.size)
		return io.EOF
	}

	// Calculate global torrent offset (file offset + position within file)
	globalOffset := s.fileOffset + offset

	// Calculate which piece we need in the global torrent
	pieceIndex := uint32(globalOffset / s.pieceSize)
	if pieceIndex >= s.downloader.NumPieces() {
		log.Debug("waitForData EOF: piece index past end", "sub", "stream", "pieceIndex", pieceIndex, "numPieces", s.downloader.NumPieces())
		return io.EOF
	}

	log.Info("waitForData", "sub", "seek", "fileOffset", offset, "globalOffset", globalOffset, "piece", pieceIndex)

	// Fast path: piece already done.  PieceByIndex uses a read-only map so no lock needed.
	picker := s.downloader.PiecePicker()
	if picker != nil {
		if pa := picker.PieceByIndex(pieceIndex); pa != nil && pa.Piece.Done.Load() {
			return nil
		}
	}

	// Register this goroutine as a waiter so the downloader can focus on the
	// minimum-needed piece across all concurrent HTTP goroutines.
	s.downloader.RegisterWaiter(pieceIndex)
	defer s.downloader.UnregisterWaiter(pieceIndex)

	log.Info("waiting for piece", "sub", "seek", "piece", pieceIndex, "completed", s.downloader.CountCompletedPieces(), "total", s.downloader.NumPieces())

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	timeout := time.After(120 * time.Second)
	logTicker := time.NewTicker(5 * time.Second)
	defer logTicker.Stop()

	waitStart := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return fmt.Errorf("context cancelled")
		case <-timeout:
			return fmt.Errorf("timeout waiting for piece %d at offset %d after %v (file progress: %d/%d pieces)",
				pieceIndex, offset, time.Since(waitStart).Round(time.Second),
				s.downloader.CountCompletedPieces(), s.downloader.NumPieces())
		case <-logTicker.C:
			// Periodic stall report.
			requestors := 0
			if picker := s.downloader.PiecePicker(); picker != nil {
				if pa := picker.PieceByIndex(pieceIndex); pa != nil {
					pa.Requesting.Range(func(_, _ interface{}) bool { requestors++; return true })
				}
			}
			totalPeers, unchokedPeers, idlePeers := s.downloader.PeerStats()
			log.Info("stall", "sub", "seek", "piece", pieceIndex, "waited", time.Since(waitStart).Round(time.Second), "requestors", requestors, "peers", totalPeers, "unchoked", unchokedPeers, "idle", idlePeers, "completed", s.downloader.CountCompletedPieces(), "total", s.downloader.NumPieces())
			// Failsafe: abort stalled requestors and reassign.
			// requestors==0: no one picked it up → redirect priority.
			// requestors>0 but waited>15s: peer stalled on this piece
			// while other pieces complete → disconnect and let a fresh peer take over.
			const stallThreshold = 15 * time.Second
			waitDuration := time.Since(waitStart)
			if requestors == 0 {
				log.Info("failsafe RequestSeek", "sub", "seek", "piece", pieceIndex)
				s.downloader.RequestSeek(pieceIndex)
			} else if waitDuration > stallThreshold {
				log.Info("failsafe force-reassign", "sub", "seek", "piece", pieceIndex, "requestors", requestors, "waited", waitDuration.Round(time.Second))
				s.downloader.ForceReassignPiece(pieceIndex)
			}
		case <-ticker.C:
			// Check if piece is complete.
			if picker := s.downloader.PiecePicker(); picker != nil {
				if pa := picker.PieceByIndex(pieceIndex); pa != nil && pa.Piece.Done.Load() {
					return nil
				}
			}
		}
	}
}

// StreamHandler provides HTTP range request support for a downloading torrent
// This allows streaming the file before it's fully downloaded
type StreamHandler struct {
	config      download.DownloaderConfig
	peers       []*net.TCPAddr
	downloader  *download.Downloader
	file        *os.File
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.RWMutex
	started     bool
	downloadErr error
}

// NewStreamHandler creates a new HTTP handler for streaming a torrent download
func NewStreamHandler(config download.DownloaderConfig, peers []*net.TCPAddr) *StreamHandler {
	ctx, cancel := context.WithCancel(context.Background())
	return &StreamHandler{
		config: config,
		peers:  peers,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start begins the download in the background
func (sh *StreamHandler) Start() error {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if sh.started {
		return fmt.Errorf("download already started")
	}

	// Ensure sequential mode for streaming
	sh.config.Strategy = download.StrategySequential

	// Create downloader
	downloader, err := download.NewDownloader(sh.config)
	if err != nil {
		return fmt.Errorf("failed to create downloader: %w", err)
	}
	sh.downloader = downloader

	// Start downloader in background
	log.Info("launching downloader", "sub", "stream", "peers", len(sh.peers))
	go func() {
		if err := downloader.Run(sh.ctx); err != nil && err != context.Canceled {
			log.Warn("downloader exited with error", "sub", "stream", "err", err)
			sh.mu.Lock()
			sh.downloadErr = err
			sh.mu.Unlock()
		} else {
			log.Debug("downloader exited cleanly", "sub", "stream")
		}
	}()

	// Add peers (triggers connection attempts)
	downloader.AddPeers(sh.peers)

	sh.started = true
	return nil
}

// Stop stops the download
func (sh *StreamHandler) Stop() {
	sh.cancel()
	if sh.downloader != nil {
		sh.downloader.Close()
	}
	if sh.file != nil {
		sh.file.Close()
	}
}

// Done returns a channel that is closed when the handler's context is cancelled.
func (sh *StreamHandler) Done() <-chan struct{} {
	return sh.ctx.Done()
}

// AddPeers feeds additional peer addresses to the running downloader.
func (sh *StreamHandler) AddPeers(addrs []*net.TCPAddr) {
	sh.mu.RLock()
	downloader := sh.downloader
	sh.mu.RUnlock()
	if downloader != nil {
		downloader.AddPeers(addrs)
	}
}

// SetPriorityPiece sets the priority piece for sequential downloading (for seeking).
// Deprecated: prefer SetFileRange which also sets the piece bound.
func (sh *StreamHandler) SetPriorityPiece(offset int64) {
	sh.mu.RLock()
	downloader := sh.downloader
	sh.mu.RUnlock()

	if downloader == nil {
		return
	}

	picker := downloader.PiecePicker()
	if picker == nil {
		return
	}

	pieceIndex := uint32(offset / int64(sh.config.PieceSize))
	numPieces := downloader.NumPieces()
	if pieceIndex >= numPieces {
		pieceIndex = numPieces - 1
	}
	picker.SetPriorityPiece(pieceIndex)
}

// SetFileRange restricts the downloader to only fetch pieces that cover
// [fileOffset, fileOffset+fileSize) within the torrent.  Call this as soon as
// an HTTP request arrives for a file — before waiting for the file on disk —
// so downloading begins immediately at the right place.
func (sh *StreamHandler) SetFileRange(fileOffset, fileSize int64) {
	sh.mu.RLock()
	downloader := sh.downloader
	sh.mu.RUnlock()

	if downloader == nil {
		return
	}
	downloader.SetFileRange(fileOffset, fileOffset+fileSize)
}

// GetStreamingReadSeeker creates a streaming ReadSeeker for the file
// This wraps the file and waits for data to be available before reading
// fileSize should be the actual size of THIS file (not the total torrent size)
// fileOffset is the byte offset of this file within the torrent (0 for single-file torrents)
func (sh *StreamHandler) GetStreamingReadSeeker(ctx context.Context, file *os.File, fileSize int64, fileOffset int64) (*StreamingReadSeeker, error) {
	log.Debug("GetStreamingReadSeeker", "sub", "stream", "file", file.Name(), "size", fileSize, "offset", fileOffset)

	sh.mu.RLock()
	defer sh.mu.RUnlock()

	if sh.downloader == nil {
		log.Debug("GetStreamingReadSeeker: downloader not started", "sub", "stream")
		return nil, fmt.Errorf("downloader not started")
	}

	log.Debug("creating StreamingReadSeeker", "sub", "stream",
		"fileSize", fileSize, "fileOffset", fileOffset,
		"pieceSize", sh.config.PieceSize, "numPieces", sh.downloader.NumPieces())

	return NewStreamingReadSeeker(
		file,
		fileSize, // Use actual file size, not total torrent size
		int64(sh.config.PieceSize),
		sh.downloader,
		ctx,        // Use request context so cancellation works properly
		fileOffset, // Pass the file offset within the torrent
	), nil
}

// ServeHTTP implements http.Handler with range request support
func (sh *StreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only support GET and HEAD
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Start download if not started
	sh.mu.RLock()
	if !sh.started {
		sh.mu.RUnlock()
		if err := sh.Start(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		sh.mu.RUnlock()
	}

	// Wait for file to be created
	if err := sh.waitForFile(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Open the file
	file, err := os.Open(sh.config.OutputPath)
	if err != nil {
		http.Error(w, "Failed to open file", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Get file info
	stat, err := file.Stat()
	if err != nil {
		http.Error(w, "Failed to stat file", http.StatusInternalServerError)
		return
	}

	totalSize := int64(sh.config.TotalSize)

	// Set content type based on file extension
	contentType := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(stat.Name())) {
	case ".mp4":
		contentType = "video/mp4"
	case ".mkv":
		contentType = "video/x-matroska"
	case ".avi":
		contentType = "video/x-msvideo"
	case ".webm":
		contentType = "video/webm"
	case ".mp3":
		contentType = "audio/mpeg"
	}

	// Parse range header
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		// No range request - serve entire file
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.FormatInt(totalSize, 10))
		w.Header().Set("Accept-Ranges", "bytes")

		if r.Method == http.MethodHead {
			return
		}

		sh.serveContent(w, file, 0, totalSize)
		return
	}

	// Parse range header (format: "bytes=start-end")
	ranges, err := parseRange(rangeHeader, totalSize)
	if err != nil || len(ranges) != 1 {
		http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", totalSize))
		return
	}

	start := ranges[0].start
	end := ranges[0].end
	contentLength := end - start + 1

	// Set headers for partial content
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusPartialContent)

	if r.Method == http.MethodHead {
		return
	}

	// Seek to start position
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		http.Error(w, "Failed to seek", http.StatusInternalServerError)
		return
	}

	// Serve the requested range
	sh.serveContent(w, file, start, end+1)
}

// serveContent streams the file content, waiting for data to be available
func (sh *StreamHandler) serveContent(w http.ResponseWriter, file *os.File, start, end int64) {
	pieceSize := int64(sh.config.PieceSize)
	buffer := make([]byte, 64*1024) // 64 KB buffer
	offset := start

	for offset < end {
		// Calculate which piece we need
		pieceIndex := uint32(offset / pieceSize)
		pieceStart := int64(pieceIndex) * pieceSize
		pieceEnd := pieceStart + pieceSize
		if pieceEnd > int64(sh.config.TotalSize) {
			pieceEnd = int64(sh.config.TotalSize)
		}

		// Wait for enough data in this piece to be available
		// We wait for the piece to have data at the offset we need
		if err := sh.waitForData(offset, pieceEnd); err != nil {
			// Download failed or was cancelled
			return
		}

		// Read from file
		toRead := int64(len(buffer))
		if offset+toRead > end {
			toRead = end - offset
		}

		n, err := file.ReadAt(buffer[:toRead], offset)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				// Client disconnected
				return
			}
			offset += int64(n)
		}

		if err != nil {
			if err == io.EOF {
				// Reached end of available data, wait for more
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return
		}
	}
}

// waitForFile waits for the output file to be created
func (sh *StreamHandler) waitForFile() error {
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for file creation")
		case <-sh.ctx.Done():
			return fmt.Errorf("download cancelled")
		case <-ticker.C:
			if _, err := os.Stat(sh.config.OutputPath); err == nil {
				return nil
			}
		}
	}
}

// waitForData waits for data to be available at the given offset
// This is a simple approach: we just check if the file size has reached the needed offset
func (sh *StreamHandler) waitForData(offset, pieceEnd int64) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	timeout := time.After(2 * time.Minute)

	for {
		select {
		case <-sh.ctx.Done():
			return fmt.Errorf("download cancelled")
		case <-timeout:
			return fmt.Errorf("timeout waiting for data at offset %d", offset)
		case <-ticker.C:
			// Check if file has grown to include the data we need
			stat, err := os.Stat(sh.config.OutputPath)
			if err != nil {
				continue
			}

			// If file size is past our offset, data should be available
			// Note: With sequential download, pieces are written in order
			if stat.Size() >= offset+1 {
				return nil
			}

			// Check if download failed
			sh.mu.RLock()
			err = sh.downloadErr
			sh.mu.RUnlock()
			if err != nil && err != context.Canceled {
				return err
			}
		}
	}
}

// httpRange represents a byte range
type httpRange struct {
	start int64
	end   int64
}

// parseRange parses a Range header value
func parseRange(s string, size int64) ([]httpRange, error) {
	if s == "" {
		return nil, nil
	}

	const prefix = "bytes="
	if !strings.HasPrefix(s, prefix) {
		return nil, fmt.Errorf("invalid range header")
	}

	s = s[len(prefix):]
	ranges := []httpRange{}

	for _, ra := range strings.Split(s, ",") {
		ra = strings.TrimSpace(ra)
		if ra == "" {
			continue
		}

		start, end, ok := strings.Cut(ra, "-")
		if !ok {
			return nil, fmt.Errorf("invalid range format")
		}
		start = strings.TrimSpace(start)
		end = strings.TrimSpace(end)

		var r httpRange

		if start == "" {
			// Suffix range: "-500" means last 500 bytes
			if end == "" {
				return nil, fmt.Errorf("invalid range format")
			}
			n, err := strconv.ParseInt(end, 10, 64)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid range format")
			}
			if n > size {
				n = size
			}
			r.start = size - n
			r.end = size - 1
		} else {
			// Regular range: "0-499" or "0-"
			n, err := strconv.ParseInt(start, 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("invalid range format")
			}
			if n >= size {
				return nil, fmt.Errorf("range not satisfiable")
			}
			r.start = n

			if end == "" {
				// Open-ended: "500-"
				r.end = size - 1
			} else {
				n, err := strconv.ParseInt(end, 10, 64)
				if err != nil || n < r.start {
					return nil, fmt.Errorf("invalid range format")
				}
				if n >= size {
					n = size - 1
				}
				r.end = n
			}
		}

		ranges = append(ranges, r)
	}

	return ranges, nil
}
