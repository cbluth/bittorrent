package bt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cbluth/bittorrent/pkg/bencode"
	"github.com/cbluth/bittorrent/pkg/client"
	"github.com/cbluth/bittorrent/pkg/client/download"
	"github.com/cbluth/bittorrent/pkg/client/stream"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
	"github.com/cbluth/bittorrent/pkg/torrent"
	"github.com/cbluth/bittorrent/pkg/tracker"
)

// ServeConfig holds configuration for the bt serve command.
type ServeConfig struct {
	Port       int
	ShareRatio float64 // 0=stop immediately, >0=seed until ratio met, Inf=seed forever
}

// serveFileEntry maps a URL subpath to a file's metadata within a torrent.
// urlSubpath is relative to /bt/<hash>/ (e.g. "TorrentName/ep01.mkv").
type serveFileEntry struct {
	diskPath   string // absolute path on disk
	fileSize   int64
	fileOffset int64  // byte offset of this file within the full torrent
	urlSubpath string // URL-decoded subpath after /bt/<hash>/
}

// serveSession represents one active torrent being lazily initialized and served.
type serveSession struct {
	ready      chan struct{} // closed when init completes (success or failure)
	err        error
	name       string // torrent display name
	totalSize  int64
	files      []serveFileEntry // indexed for request dispatch
	handler    *stream.StreamHandler
	mu         sync.Mutex
	activeFile *serveFileEntry // the file currently being streamed (set on first GET)
}

// serveServer is the on-demand HTTP streaming server.
type serveServer struct {
	btClient   *client.Client
	baseDir    string // e.g. ~/.bt/serve
	shareRatio float64
	sessions   sync.Map // hex hash → *serveSession
}

// RunServeCommand starts the on-demand multi-torrent HTTP streaming server.
// baseDir is the root directory where per-torrent downloads are stored.
func RunServeCommand(ctx context.Context, btClient *client.Client, baseDir string, cfg *ServeConfig) error {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return fmt.Errorf("failed to create serve directory: %w", err)
	}

	srv := &serveServer{
		btClient:   btClient,
		baseDir:    baseDir,
		shareRatio: cfg.ShareRatio,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/bt/", srv.handleBT)
	mux.HandleFunc("/", srv.handleRoot)

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Info("http server starting", "sub", "server", "addr", fmt.Sprintf("http://localhost:%d", cfg.Port))
	log.Info("access torrents", "sub", "server", "url", fmt.Sprintf("http://localhost:%d/bt/<infohash>", cfg.Port))
	log.Info("press ctrl+c to stop", "sub", "server")

	httpSrv := &http.Server{Addr: addr, Handler: mux}

	// Shut down when context is cancelled.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		// Stop all active sessions.
		srv.sessions.Range(func(_, v any) bool {
			sess := v.(*serveSession)
			select {
			case <-sess.ready:
				if sess.handler != nil {
					sess.handler.Stop()
				}
			default:
			}
			return true
		})
	}()

	if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// getOrCreate returns the existing session for hashHex, or creates and starts
// initialising a new one. Exactly one init goroutine runs per hash.
func (s *serveServer) getOrCreate(hashHex string) *serveSession {
	sess := &serveSession{ready: make(chan struct{})}
	actual, loaded := s.sessions.LoadOrStore(hashHex, sess)
	if !loaded {
		go s.initSession(sess, hashHex)
	}
	return actual.(*serveSession)
}

// metaCachePath returns the path for the cached .torrent file for hashHex.
func (s *serveServer) metaCachePath(hashHex string) string {
	return filepath.Join(s.baseDir, hashHex+".torrent")
}

// loadCachedMeta loads a previously resolved MetaInfo from disk, or nil if not present.
func (s *serveServer) loadCachedMeta(hashHex string) *torrent.MetaInfo {
	p := s.metaCachePath(hashHex)
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	meta, err := torrent.Parse(f)
	if err != nil {
		log.Warn("cache parse error, will re-resolve", "sub", "server", "hash", hashHex[:8], "err", err)
		return nil
	}
	return meta
}

// saveCachedMeta writes the resolved MetaInfo to disk as both .torrent and .json.
func (s *serveServer) saveCachedMeta(hashHex string, meta *torrent.MetaInfo) {
	if err := os.MkdirAll(s.baseDir, 0755); err != nil {
		return
	}

	// Save .torrent (bencode)
	p := s.metaCachePath(hashHex)
	var buf bytes.Buffer
	if err := meta.Write(&buf); err != nil {
		log.Warn("cache write error", "sub", "server", "hash", hashHex[:8], "err", err)
		return
	}
	if err := os.WriteFile(p, buf.Bytes(), 0644); err != nil {
		log.Warn("cache save error", "sub", "server", "hash", hashHex[:8], "err", err)
	}

	// Save .json
	jsonPath := filepath.Join(s.baseDir, hashHex+".json")
	jsonData, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		log.Warn("json encode error", "sub", "server", "hash", hashHex[:8], "err", err)
		return
	}
	if err := os.WriteFile(jsonPath, jsonData, 0644); err != nil {
		log.Warn("json save error", "sub", "server", "hash", hashHex[:8], "err", err)
	}
	log.Info("saved metadata", "sub", "server", "hash", hashHex[:8], "torrent", p, "json", jsonPath)
}

// initSession resolves metadata (BEP 9 ut_metadata via peers, or from disk cache),
// discovers peers (BEP 3 tracker + BEP 5 DHT), and starts the download.
// It always closes sess.ready when done (on success or failure).
func (s *serveServer) initSession(sess *serveSession, hashHex string) {
	defer close(sess.ready)

	// Try the on-disk cache before hitting the network.
	var meta *torrent.MetaInfo
	if cached := s.loadCachedMeta(hashHex); cached != nil {
		log.Debug("loaded metadata from cache", "sub", "server", "hash", hashHex[:8])
		meta = cached
	} else {
		log.Info("resolving metadata from peers", "sub", "server", "hash", hashHex[:8])
		resolveCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		info, err := s.btClient.ResolveMetadata(resolveCtx, hashHex)
		if err != nil {
			sess.err = fmt.Errorf("failed to resolve metadata: %w", err)
			log.Warn("resolve error", "sub", "server", "hash", hashHex[:8], "err", err)
			return
		}
		meta = info.GetMetaInfo()
		s.saveCachedMeta(hashHex, meta)
	}

	sess.name = meta.Info.Name
	sess.totalSize = meta.Info.TotalLength()

	pieceHashes, err := meta.Info.GetPieceHashes()
	if err != nil {
		sess.err = fmt.Errorf("failed to get piece hashes: %w", err)
		return
	}

	var peerID dht.Key
	peerIDSlice := s.btClient.PeerID()
	copy(peerID[:], peerIDSlice[:])
	var infoHash dht.Key
	copy(infoHash[:], meta.InfoHash[:])

	// Discover peers from trackers — merge embedded and feed trackers.
	trackerURLs := meta.GetTrackers()
	if feedTrackers := s.btClient.GetTrackers(); len(feedTrackers) > 0 {
		seen := make(map[string]bool, len(trackerURLs))
		for _, u := range trackerURLs {
			seen[u] = true
		}
		for _, u := range feedTrackers {
			if !seen[u] {
				trackerURLs = append(trackerURLs, u)
			}
		}
	}

	var allPeers []*net.TCPAddr
	if len(trackerURLs) > 0 {
		log.Info("querying trackers", "sub", "server", "hash", hashHex[:8], "trackers", len(trackerURLs))
		trackerClient := tracker.NewClient(10 * time.Second)
		var infoHashKey dht.Key
		copy(infoHashKey[:], meta.InfoHash[:])

		peersChan := make(chan []*net.TCPAddr, len(trackerURLs))
		for _, trackerURL := range trackerURLs {
			go func(u string) {
				req := &tracker.AnnounceRequest{
					InfoHash: infoHashKey,
					PeerID:   peerID,
					Port:     s.btClient.Port(),
					Left:     meta.Info.TotalLength(),
					Compact:  true,
					NoPeerID: true,
					Event:    tracker.EventStarted,
					NumWant:  200,
				}
				resp, err := trackerClient.Announce(u, req)
				if err != nil {
					peersChan <- nil
					return
				}
				tcpPeers := make([]*net.TCPAddr, 0, len(resp.Peers))
				for _, p := range resp.Peers {
					tcpPeers = append(tcpPeers, &net.TCPAddr{IP: p.IP, Port: int(p.Port)})
				}
				peersChan <- tcpPeers
			}(trackerURL)
		}

		// Collect results with a deadline — don't wait for every slow tracker.
		// Stop early once we have enough peers or 5s passes.
		deadline := time.After(5 * time.Second)
		seen := make(map[string]bool)
		remaining := len(trackerURLs)
	collect:
		for remaining > 0 {
			select {
			case peers := <-peersChan:
				remaining--
				for _, peer := range peers {
					if addr := peer.String(); !seen[addr] {
						seen[addr] = true
						allPeers = append(allPeers, peer)
					}
				}
			case <-deadline:
				log.Info("tracker deadline reached, starting with available peers", "sub", "server",
					"hash", hashHex[:8], "peers", len(allPeers), "responded", len(trackerURLs)-remaining, "total", len(trackerURLs))
				break collect
			}
		}
		log.Info("peer discovery complete", "sub", "server", "hash", hashHex[:8], "peers", len(allPeers))
	}

	if len(allPeers) == 0 {
		sess.err = fmt.Errorf("no peers found from trackers")
		return
	}

	// Prepare output directory and download config.
	dir := filepath.Join(s.baseDir, hashHex)
	if err := os.MkdirAll(dir, 0755); err != nil {
		sess.err = fmt.Errorf("failed to create output dir: %w", err)
		return
	}
	outputPath := filepath.Join(dir, meta.Info.Name)

	var dlFiles []download.FileInfo
	if !meta.Info.IsSingleFile() {
		var offset int64
		for _, f := range meta.Info.Files {
			dlFiles = append(dlFiles, download.FileInfo{
				Path:   f.Path,
				Length: f.Length,
				Offset: offset,
			})
			offset += f.Length
		}
	}

	rawInfoDict, _ := bencode.EncodeBytes(&meta.Info)

	dlCfg := download.DownloaderConfig{
		InfoHash:    infoHash,
		PeerID:      peerID,
		Port:        s.btClient.Port(),
		PieceSize:   uint32(meta.Info.PieceLength),
		TotalSize:   uint64(meta.Info.TotalLength()),
		PieceHashes: pieceHashes,
		OutputPath:  outputPath,
		Strategy:    download.StrategySequential,
		Files:       dlFiles,
		ShareRatio:  s.shareRatio,
		RawInfoDict: rawInfoDict,
		Private:     meta.Info.IsPrivate(),
	}

	handler := stream.NewStreamHandler(dlCfg, allPeers)
	if err := handler.Start(); err != nil {
		sess.err = fmt.Errorf("failed to start download: %w", err)
		return
	}

	// Build file entries.  urlSubpath = "<TorrentName>" for single-file,
	// "<TorrentName>/<subpath>" for multi-file.
	var files []serveFileEntry
	if meta.Info.IsSingleFile() {
		files = append(files, serveFileEntry{
			diskPath:   outputPath,
			fileSize:   meta.Info.Length,
			fileOffset: 0,
			urlSubpath: meta.Info.Name,
		})
	} else {
		var offset int64
		for _, f := range meta.Info.Files {
			// Sanitise path to prevent traversal.
			relPath := path.Clean(strings.Join(f.Path, "/"))
			if strings.HasPrefix(relPath, "..") {
				offset += f.Length
				continue
			}
			files = append(files, serveFileEntry{
				diskPath:   filepath.Join(outputPath, filepath.FromSlash(relPath)),
				fileSize:   f.Length,
				fileOffset: offset,
				urlSubpath: meta.Info.Name + "/" + relPath,
			})
			offset += f.Length
		}
	}

	sess.handler = handler
	sess.files = files
	log.Info("session ready", "sub", "server", "hash", hashHex[:8], "name", meta.Info.Name, "peers", len(allPeers), "files", len(files))

	// Periodic tracker re-announce to discover fresh peers.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-handler.Done():
				return
			case <-ticker.C:
				trackerClient := tracker.NewClient(15 * time.Second)
				var newPeers []*net.TCPAddr
				for _, u := range trackerURLs {
					req := &tracker.AnnounceRequest{
						InfoHash: infoHash,
						PeerID:   peerID,
						Port:     s.btClient.Port(),
						Left:     meta.Info.TotalLength(),
						Compact:  true,
						NoPeerID: true,
						NumWant:  200,
					}
					resp, err := trackerClient.Announce(u, req)
					if err != nil {
						continue
					}
					for _, p := range resp.Peers {
						newPeers = append(newPeers, &net.TCPAddr{IP: p.IP, Port: int(p.Port)})
					}
				}
				if len(newPeers) > 0 {
					log.Debug("re-announce discovered peers", "sub", "server", "hash", hashHex[:8], "peers", len(newPeers))
					handler.AddPeers(newPeers)
				}
			}
		}
	}()
}

// handleBT is the catch-all handler for /bt/<hash>[/<subpath>].
func (s *serveServer) handleBT(w http.ResponseWriter, r *http.Request) {
	// Strip "/bt/" prefix.
	rest := strings.TrimPrefix(r.URL.Path, "/bt/")
	// rest = "<hash>" | "<hash>/" | "<hash>/<subpath>"
	slashIdx := strings.IndexByte(rest, '/')
	var hashHex, subpath string
	if slashIdx < 0 {
		hashHex = rest
		// No trailing slash on the index URL — redirect so relative links work.
		http.Redirect(w, r, "/bt/"+hashHex+"/", http.StatusFound)
		return
	}
	hashHex = rest[:slashIdx]
	subpath = rest[slashIdx+1:]

	if !isInfoHashHex(hashHex) {
		http.Error(w, "invalid infohash (must be 40 or 64 hex chars)", http.StatusBadRequest)
		return
	}

	sess := s.getOrCreate(hashHex)

	if subpath == "" {
		// Index request from a browser: non-blocking check so we can return
		// an auto-refreshing "please wait" page while metadata resolves.
		select {
		case <-sess.ready:
		default:
			s.serveResolving(w, r, hashHex)
			return
		}
	} else {
		// File request (e.g. from VLC): block until the session is ready.
		// The request context carries the client's deadline/disconnect signal.
		select {
		case <-sess.ready:
		case <-r.Context().Done():
			return
		}
	}

	if sess.err != nil {
		http.Error(w, "initialization failed: "+sess.err.Error(), http.StatusBadGateway)
		return
	}

	// Empty subpath (trailing slash) → show torrent index.
	if subpath == "" {
		s.serveTorrentIndex(w, r, hashHex, sess)
		return
	}

	// Decode and match to a file entry.
	decoded, err := url.PathUnescape(subpath)
	if err != nil {
		http.Error(w, "invalid path encoding", http.StatusBadRequest)
		return
	}

	for _, fe := range sess.files {
		if fe.urlSubpath == decoded || fe.urlSubpath == subpath {
			s.serveFile(w, r, sess, fe)
			return
		}
	}

	http.NotFound(w, r)
}

// contentTypeForExt returns the MIME type for common media file extensions.
func contentTypeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".mp3":
		return "audio/mpeg"
	case ".flac":
		return "audio/flac"
	case ".ogg":
		return "audio/ogg"
	case ".opus":
		return "audio/opus"
	default:
		return "application/octet-stream"
	}
}

// serveFile streams a single file from a torrent session.
func (s *serveServer) serveFile(w http.ResponseWriter, r *http.Request, sess *serveSession, fe serveFileEntry) {
	ct := contentTypeForExt(filepath.Ext(fe.diskPath))

	log.Info("http request", "sub", "stream",
		"method", r.Method,
		"path", r.URL.Path,
		"range", r.Header.Get("Range"),
		"file", filepath.Base(fe.diskPath),
		"size", fe.fileSize,
		"remote", r.RemoteAddr)

	// HEAD: VLC (and other players) probe with HEAD to confirm Accept-Ranges support
	// before issuing range GETs. Respond immediately using declared metadata — no
	// need to wait for bytes on disk.
	if r.Method == http.MethodHead {
		h := w.Header()
		h.Set("Content-Type", ct)
		h.Set("Content-Length", strconv.FormatInt(fe.fileSize, 10))
		h.Set("Accept-Ranges", "bytes")
		h.Set("Content-Disposition", "inline")
		w.WriteHeader(http.StatusOK)
		return
	}

	// GET: tell the downloader to focus on this file's pieces — but only when
	// the requested file changes.  Seeks within the same file are handled by
	// JumpToOffset (called from StreamingReadSeeker.Seek inside http.ServeContent)
	// so we must NOT reset the priority piece on every range request.
	if sess.handler != nil {
		sess.mu.Lock()
		fileChanged := sess.activeFile == nil || sess.activeFile.fileOffset != fe.fileOffset
		if fileChanged {
			feCopy := fe
			sess.activeFile = &feCopy
		}
		sess.mu.Unlock()
		if fileChanged {
			log.Debug("new file requested", "sub", "server", "offset", fe.fileOffset, "size", fe.fileSize, "path", fe.diskPath)
			sess.handler.SetFileRange(fe.fileOffset, fe.fileSize)
		}
	}

	// Wait for the file to appear on disk (downloader creates it on first write).
	timeout := time.After(30 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
waitLoop:
	for {
		select {
		case <-timeout:
			http.Error(w, "timeout waiting for download to start", http.StatusGatewayTimeout)
			return
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := os.Stat(fe.diskPath); err == nil {
				break waitLoop
			}
		}
	}

	f, err := os.Open(fe.diskPath)
	if err != nil {
		http.Error(w, "failed to open file", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", "inline")

	streamReader, err := sess.handler.GetStreamingReadSeeker(r.Context(), f, fe.fileSize, fe.fileOffset)
	if err != nil {
		http.Error(w, "failed to create streaming reader", http.StatusInternalServerError)
		return
	}

	http.ServeContent(w, r, filepath.Base(fe.diskPath), time.Time{}, streamReader)
}

// serveResolving renders a "please wait" page that auto-refreshes every 3 s.
func (s *serveServer) serveResolving(w http.ResponseWriter, r *http.Request, hashHex string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = serveResolvingTmpl.Execute(w, hashHex)
}

// serveTorrentIndex renders the file listing for a single torrent.
func (s *serveServer) serveTorrentIndex(w http.ResponseWriter, r *http.Request, hashHex string, sess *serveSession) {
	type fileLink struct {
		Name string
		Size string
		URL  string
	}
	data := struct {
		Hash      string
		Name      string
		TotalSize string
		Files     []fileLink
	}{
		Hash:      hashHex,
		Name:      sess.name,
		TotalSize: formatSize(sess.totalSize),
	}
	for _, fe := range sess.files {
		data.Files = append(data.Files, fileLink{
			Name: fe.urlSubpath,
			Size: formatSize(fe.fileSize),
			URL:  "/bt/" + hashHex + "/" + encodeURLPath(fe.urlSubpath),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = serveTorrentTmpl.Execute(w, data)
}

// handleRoot renders the global active-torrent list.
func (s *serveServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	type entry struct {
		Hash   string
		Name   string
		Status string
		URL    string
	}
	var entries []entry
	s.sessions.Range(func(k, v any) bool {
		hashHex := k.(string)
		sess := v.(*serveSession)
		e := entry{Hash: hashHex, URL: "/bt/" + hashHex + "/"}
		select {
		case <-sess.ready:
			if sess.err != nil {
				e.Name = "(error)"
				e.Status = sess.err.Error()
			} else {
				e.Name = sess.name
				e.Status = "ready"
			}
		default:
			e.Name = "(resolving…)"
			e.Status = "resolving"
		}
		entries = append(entries, e)
		return true
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = serveRootTmpl.Execute(w, entries)
}

// encodeURLPath percent-encodes each path component individually.
func encodeURLPath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

var serveResolvingTmpl = template.Must(template.New("resolving").Parse(`<!DOCTYPE html>
<html><head><meta charset="UTF-8"><meta http-equiv="refresh" content="3">
<title>Resolving…</title>
<style>body{font-family:monospace;margin:40px;color:#555}code{font-size:.9em}</style>
</head><body>
<h2>Resolving metadata…</h2>
<p>Hash: <code>{{.}}</code></p>
<p>Contacting peers to fetch torrent metadata. This page will refresh automatically.</p>
</body></html>`))

var serveTorrentTmpl = template.Must(template.New("torrent").Parse(`<!DOCTYPE html>
<html><head><meta charset="UTF-8">
<title>{{.Name}}</title>
<style>body{font-family:monospace;margin:20px}a{color:#06c}table{border-collapse:collapse;width:100%}
td,th{padding:4px 8px;text-align:left}th{border-bottom:1px solid #ccc}tr:hover{background:#f5f5f5}</style>
</head><body>
<h2>{{.Name}}</h2>
<p>Hash: <code>{{.Hash}}</code> &nbsp; Total: {{.TotalSize}}</p>
<table><tr><th>File</th><th>Size</th></tr>
{{range .Files}}<tr><td><a href="{{.URL}}">{{.Name}}</a></td><td>{{.Size}}</td></tr>{{end}}
</table></body></html>`))

// formatSize returns a human-readable byte size string.
func formatSize(n int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.2f GB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.2f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.2f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

var serveRootTmpl = template.Must(template.New("root").Parse(`<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>bt serve</title>
<style>body{font-family:monospace;margin:20px}a{color:#06c}code{font-size:.9em}
table{border-collapse:collapse;width:100%}td,th{padding:4px 8px;text-align:left}
th{border-bottom:1px solid #ccc}tr:hover{background:#f5f5f5}</style>
</head><body>
<h2>bt serve — active torrents</h2>
{{if .}}<table><tr><th>Hash</th><th>Name</th><th>Status</th></tr>
{{range .}}<tr><td><a href="{{.URL}}"><code>{{.Hash}}</code></a></td><td>{{.Name}}</td><td>{{.Status}}</td></tr>{{end}}
</table>{{else}}<p>No active torrents. Navigate to <code>/bt/&lt;infohash&gt;</code> to start one.</p>{{end}}
</body></html>`))
