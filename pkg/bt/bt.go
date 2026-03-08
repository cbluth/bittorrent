package bt

// BitTorrent Download Orchestrator
//
// This file coordinates the download engine (pkg/client/download/downloader.go)
// with torrent parsing, tracker queries, and peer discovery.
//
// The actual download logic lives in pkg/client/download/downloader.go which implements:
//   - 100 parallel connection attempts (vs sequential in old code)
//   - 250-deep request pipelining (vs 10 in old code)
//   - CSP-based synchronization with zero mutexes
//   - Resume support with hash verification
//   - First/last piece prioritization
//   - Rarest-first + endgame piece selection
//   - Direct-to-disk streaming (no memory accumulation)
//
// This wrapper handles the "orchestration" layer:
//   - Parsing .torrent files and magnet links
//   - Querying trackers for peer lists
//   - Converting between types (dht.Key ↔ [20]byte)
//   - Logging and progress reporting

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cbluth/bittorrent/pkg/bencode"
	"github.com/cbluth/bittorrent/pkg/client"
	"github.com/cbluth/bittorrent/pkg/client/download"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
	"github.com/cbluth/bittorrent/pkg/torrent"
	"github.com/cbluth/bittorrent/pkg/tracker"
)

// RunBitTorrentDownload orchestrates a torrent download using the optimized downloader
func RunBitTorrentDownload(ctx context.Context, btClient *client.Client, input, outputDir string, sequential bool, shareRatio float64) error {
	log.Info("initializing download", "sub", "download")

	// Parse input (torrent file or magnet)
	var meta *torrent.MetaInfo
	var err error

	switch {
	case strings.HasPrefix(input, "magnet:"):
		// Magnet URI — extract infohash and resolve metadata via trackers/DHT
		hashHex := extractInfoHashHex(input)
		if hashHex == "" {
			return fmt.Errorf("could not extract info hash from magnet URI")
		}
		log.Info("resolving metadata from magnet URI", "sub", "download", "infohash", hashHex)
		resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		info, err := btClient.ResolveMetadata(resolveCtx, hashHex)
		if err != nil {
			return fmt.Errorf("failed to resolve magnet metadata: %w", err)
		}
		meta = info.GetMetaInfo()

	case isInfoHashHex(input):
		// Bare info hash (40-char hex) — resolve metadata via trackers/DHT
		log.Info("resolving metadata from info hash", "sub", "download", "infohash", input)
		resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		info, err := btClient.ResolveMetadata(resolveCtx, input)
		if err != nil {
			return fmt.Errorf("failed to resolve info hash metadata: %w", err)
		}
		meta = info.GetMetaInfo()

	default:
		// Local .torrent file
		log.Info("parsing torrent file", "sub", "download", "file", input)
		f, err := os.Open(input)
		if err != nil {
			return fmt.Errorf("failed to open torrent file: %w", err)
		}
		defer f.Close()
		meta, err = torrent.Parse(f)
		if err != nil {
			return fmt.Errorf("failed to parse torrent: %w", err)
		}
	}

	totalLen := meta.Info.TotalLength()
	log.Info("torrent info", "sub", "download",
		"name", meta.Info.Name,
		"size", fmt.Sprintf("%.2f MB", float64(totalLen)/(1024*1024)),
		"bytes", totalLen,
		"pieces", len(meta.Info.Pieces)/20,
		"pieceSize", fmt.Sprintf("%.0f KB", float64(meta.Info.PieceLength)/1024),
		"infohash", hex.EncodeToString(meta.InfoHash[:]),
	)

	// Convert piece hashes
	pieceHashes, err := meta.Info.GetPieceHashes()
	if err != nil {
		return fmt.Errorf("failed to get piece hashes: %w", err)
	}

	// Generate or reuse peer ID
	var peerID dht.Key
	peerIDSlice := btClient.PeerID()
	copy(peerID[:], peerIDSlice[:])

	// Convert info hash from dht.Key to [20]byte
	var infoHash dht.Key
	copy(infoHash[:], meta.InfoHash[:])

	log.Info("downloader initialized", "sub", "download")
	log.Debug("local identity", "sub", "download",
		"peerID", fmt.Sprintf("%x", peerID[:8]),
		"port", btClient.Port(),
	)

	// Query trackers for peers
	log.Info("querying trackers for peers", "sub", "download")
	trackerURLs := meta.GetTrackers()
	if len(trackerURLs) == 0 {
		log.Info("no trackers in torrent file, using global tracker list", "sub", "download")
		trackerURLs = btClient.GetTrackers()
		if len(trackerURLs) == 0 {
			return fmt.Errorf("no trackers available (torrent has none and global list is empty)")
		}
		log.Info("using trackers from global list", "sub", "download", "count", len(trackerURLs))
	} else {
		log.Info("found trackers in torrent", "sub", "download", "count", len(trackerURLs))
	}

	// Create tracker client
	trackerClient := tracker.NewClient(15 * time.Second)

	// Convert info hash to dht.Key for tracker
	var infoHashKey dht.Key
	copy(infoHashKey[:], meta.InfoHash[:])

	// Announce to all trackers in parallel
	peersChan := make(chan []*net.TCPAddr, len(trackerURLs))
	for _, trackerURL := range trackerURLs {
		go func(url string) {
			log.Debug("announcing to tracker", "sub", "tracker", "url", url)

			req := &tracker.AnnounceRequest{
				InfoHash:   infoHashKey,
				PeerID:     peerID,
				Port:       btClient.Port(),
				Uploaded:   0,
				Downloaded: 0,
				Left:       meta.Info.TotalLength(),
				Compact:    true,
				NoPeerID:   true,
				Event:      tracker.EventStarted,
				NumWant:    200, // Request many peers for parallel dialing
			}

			resp, err := trackerClient.Announce(url, req)
			if err != nil {
				log.Warn("tracker announce failed", "sub", "tracker", "url", url, "err", err)
				peersChan <- nil
				return
			}

			// Convert tracker.Peer to *net.TCPAddr
			tcpPeers := make([]*net.TCPAddr, 0, len(resp.Peers))
			for _, p := range resp.Peers {
				tcpPeers = append(tcpPeers, &net.TCPAddr{
					IP:   p.IP,
					Port: int(p.Port),
				})
			}

			log.Info("tracker announce success", "sub", "tracker",
				"url", url,
				"peers", len(resp.Peers),
				"seeders", resp.Complete,
				"leechers", resp.Incomplete,
				"interval", resp.Interval,
			)

			peersChan <- tcpPeers
		}(trackerURL)
	}

	// Collect all peers from trackers
	var allPeers []*net.TCPAddr
	seenPeers := make(map[string]bool)

	for i := 0; i < len(trackerURLs); i++ {
		peers := <-peersChan
		for _, peer := range peers {
			addr := peer.String()
			if !seenPeers[addr] {
				seenPeers[addr] = true
				allPeers = append(allPeers, peer)
			}
		}
	}

	log.Info("total unique peers discovered", "sub", "download", "count", len(allPeers))

	if len(allPeers) == 0 {
		return fmt.Errorf("no peers found from trackers")
	}

	// Determine piece selection strategy
	strategy := download.StrategyRarestFirst
	if sequential {
		strategy = download.StrategySequential
		log.Info("piece selection strategy", "sub", "download", "strategy", "sequential")
	} else {
		log.Info("piece selection strategy", "sub", "download", "strategy", "rarest-first")
	}

	log.Info("starting download", "sub", "download",
		"maxParallelDials", download.MaxParallelDials,
		"maxActivePeers", download.MaxActivePeers,
		"requestQueueDepth", download.RequestQueueDepth,
		"requestQueueKB", download.RequestQueueDepth*download.BlockSize/1024,
	)
	log.Info("press Ctrl+C to stop", "sub", "download")

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Prepare file list for multi-file torrents
	var files []download.FileInfo
	if !meta.Info.IsSingleFile() {
		// Multi-file torrent: calculate file offsets
		var currentOffset int64
		for _, f := range meta.Info.Files {
			files = append(files, download.FileInfo{
				Path:   f.Path,
				Length: f.Length,
				Offset: currentOffset,
			})
			currentOffset += f.Length
		}
	}

	// Determine output path
	outputPath := filepath.Join(outputDir, meta.Info.Name)
	// For multi-file torrents, outputPath is the directory
	// For single-file torrents, it's the file path

	// Raw info dict for BEP 9 ut_metadata serving (best-effort; ignore encode error).
	rawInfoDict, _ := bencode.EncodeBytes(&meta.Info)

	// Use the new clean API
	config := download.DownloaderConfig{
		InfoHash:    infoHash,
		PeerID:      peerID,
		Port:        btClient.Port(),
		PieceSize:   uint32(meta.Info.PieceLength),
		TotalSize:   uint64(meta.Info.TotalLength()),
		PieceHashes: pieceHashes,
		OutputPath:  outputPath,
		Strategy:    strategy,
		Files:       files,
		ShareRatio:  shareRatio,
		RawInfoDict: rawInfoDict,
		Private:     meta.Info.IsPrivate(),
	}

	err = download.Download(ctx, config, allPeers)
	if err != nil && err != context.Canceled {
		return fmt.Errorf("download failed: %w", err)
	}

	log.Info("download stopped", "sub", "download")
	return nil
}

// isInfoHashHex returns true if s looks like a bare 40-char (SHA-1) or
// 64-char (SHA-256 v2) hex info hash.
func isInfoHashHex(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// extractInfoHashHex pulls the xt=urn:btih:<hash> value out of a magnet URI.
func extractInfoHashHex(magnetURI string) string {
	const prefix = "urn:btih:"
	lower := strings.ToLower(magnetURI)
	idx := strings.Index(lower, prefix)
	if idx < 0 {
		return ""
	}
	hash := magnetURI[idx+len(prefix):]
	if end := strings.IndexAny(hash, "&"); end >= 0 {
		hash = hash[:end]
	}
	return hash
}
