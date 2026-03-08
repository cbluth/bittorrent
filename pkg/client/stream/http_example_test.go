package stream_test

import (
	"context"
	"fmt"
	"github.com/cbluth/bittorrent/pkg/log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cbluth/bittorrent/pkg/client/download"
	"github.com/cbluth/bittorrent/pkg/client/stream"
	"github.com/cbluth/bittorrent/pkg/torrent"
	"github.com/cbluth/bittorrent/pkg/tracker"
)

// Example: Stream a torrent download over HTTP with range request support
// This allows video players to seek and stream before download completes
func ExampleStreamHandler() {
	// Parse torrent file
	f, err := os.Open("example.torrent")
	if err != nil {
		log.Error("failed to open torrent", "err", err)
		return
	}
	defer f.Close()

	meta, err := torrent.Parse(f)
	if err != nil {
		log.Error("failed to parse torrent", "err", err)
		return
	}

	// Get piece hashes
	pieceHashes, err := meta.Info.GetPieceHashes()
	if err != nil {
		log.Error("failed to get piece hashes", "err", err)
		return
	}

	// Convert info hash
	var infoHash [20]byte
	copy(infoHash[:], meta.InfoHash[:])

	// Generate peer ID
	var peerID [20]byte
	copy(peerID[:], "-STREAM0-12345678901")

	// Query tracker for peers
	trackerClient := tracker.NewClient(15 * time.Second)
	req := &tracker.AnnounceRequest{
		InfoHash:   meta.InfoHash,
		PeerID:     meta.InfoHash,
		Port:       6881,
		Uploaded:   0,
		Downloaded: 0,
		Left:       meta.Info.TotalLength(),
		Compact:    true,
		Event:      tracker.EventStarted,
		NumWant:    50,
	}

	resp, err := trackerClient.Announce(meta.GetTrackers()[0], req)
	if err != nil {
		log.Error("failed to announce", "err", err)
		return
	}

	// Convert peers to TCPAddr
	peers := make([]*net.TCPAddr, 0, len(resp.Peers))
	for _, p := range resp.Peers {
		peers = append(peers, &net.TCPAddr{
			IP:   p.IP,
			Port: int(p.Port),
		})
	}

	// Create downloader config
	config := download.DownloaderConfig{
		InfoHash:    infoHash,
		PeerID:      peerID,
		Port:        6881,
		PieceSize:   uint32(meta.Info.PieceLength),
		TotalSize:   uint64(meta.Info.TotalLength()),
		PieceHashes: pieceHashes,
		OutputPath:  "/tmp/download/" + meta.Info.Name,
		Strategy:    download.StrategySequential, // Important for streaming!
	}

	// Create stream handler
	handler := stream.NewStreamHandler(config, peers)

	// Start download
	if err := handler.Start(); err != nil {
		log.Error("failed to start handler", "err", err)
		return
	}
	defer handler.Stop()

	// Serve over HTTP
	http.Handle("/stream", handler)

	log.Info("streaming server started", "sub", "stream", "addr", "http://localhost:8080/stream")

	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Error("listen failed", "err", err)
	}
}

// Example: Integrate into an existing HTTP API
func ExampleStreamHandler_integration() {
	// Assuming you already have torrent metadata and peers...
	var config download.DownloaderConfig
	var peers []*net.TCPAddr

	// Create stream handler
	handler := stream.NewStreamHandler(config, peers)
	handler.Start()

	// Create your HTTP mux/router
	mux := http.NewServeMux()

	// Add to your existing API
	mux.Handle("/api/torrents/stream", handler)
	mux.HandleFunc("/api/torrents/status", func(w http.ResponseWriter, r *http.Request) {
		// Your other endpoints
		fmt.Fprintf(w, "OK")
	})

	// Start server
	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go server.ListenAndServe()

	// Shutdown gracefully
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	handler.Stop()
}

// Example: Multiple concurrent streams
func ExampleStreamHandler_multiple() {
	mux := http.NewServeMux()

	// Stream multiple torrents simultaneously
	torrents := []struct {
		path   string
		config download.DownloaderConfig
		peers  []*net.TCPAddr
	}{
		// Add your torrents here
	}

	for i, t := range torrents {
		handler := stream.NewStreamHandler(t.config, t.peers)
		handler.Start()

		path := fmt.Sprintf("/stream/%d", i)
		mux.Handle(path, handler)

		log.Info("torrent streaming", "sub", "stream", "index", i, "path", path)
	}

	http.ListenAndServe(":8080", mux)
}
