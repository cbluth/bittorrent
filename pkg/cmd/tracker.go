// pkg/cmd/tracker.go - Tracker CLI commands extracted from pkg/tracker
package cmd

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cbluth/bittorrent/pkg/cli"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
	"github.com/cbluth/bittorrent/pkg/tracker"
	trackerhttp "github.com/cbluth/bittorrent/pkg/tracker/http"
	ws "github.com/cbluth/bittorrent/pkg/tracker/ws"
)

// TrackerConfig holds runtime configuration for tracker commands
type TrackerConfig struct {
	Port    uint
	Timeout time.Duration
}

// TrackerAnnounceOptions represents options for tracker announce
type TrackerAnnounceOptions struct {
	Port       uint16
	Uploaded   int64
	Downloaded int64
	Left       int64
	Event      tracker.Event
	Compact    bool
	Timeout    time.Duration
}

// ScrapeOptions represents options for tracker scrape
type TrackerScrapeOptions struct {
	Timeout time.Duration
}

// PeersOptions represents options for getting peers
type TrackerPeersOptions struct {
	Port    uint16
	Compact bool
	Timeout time.Duration
}

// AnnounceResult represents the result of an announce operation
type TrackerAnnounceResult struct {
	Response   *tracker.AnnounceResponse
	TrackerURL string
	Error      error
}

// ScrapeResult represents the result of a scrape operation
type TrackerScrapeResult struct {
	Response   *tracker.ScrapeResponse
	TrackerURL string
	Error      error
}

// PeersResult represents the result of getting peers
type TrackerPeersResult struct {
	Peers      []tracker.Peer
	TrackerURL string
	Error      error
}

// AnnounceToTracker announces to a specific tracker
func AnnounceToTracker(trackerURL string, infoHash dht.Key, peerID dht.Key, opts *TrackerAnnounceOptions) (*TrackerAnnounceResult, error) {
	client := tracker.NewClient(opts.Timeout)

	req := &tracker.AnnounceRequest{
		InfoHash:   infoHash,
		PeerID:     peerID,
		Port:       opts.Port,
		Uploaded:   opts.Uploaded,
		Downloaded: opts.Downloaded,
		Left:       opts.Left,
		Compact:    opts.Compact,
		Event:      opts.Event,
		NumWant:    50, // Request up to 50 peers
	}

	resp, err := client.Announce(trackerURL, req)
	return &TrackerAnnounceResult{
		Response:   resp,
		TrackerURL: trackerURL,
		Error:      err,
	}, err
}

// ScrapeTracker scrapes statistics from a tracker
func ScrapeTracker(trackerURL string, infoHashes []dht.Key, opts *TrackerScrapeOptions) (*TrackerScrapeResult, error) {
	client := tracker.NewClient(opts.Timeout)

	req := &tracker.ScrapeRequest{
		InfoHashes: infoHashes,
	}

	resp, err := client.Scrape(trackerURL, req)
	return &TrackerScrapeResult{
		Response:   resp,
		TrackerURL: trackerURL,
		Error:      err,
	}, err
}

// GetPeersFromTracker gets peer list from a tracker
func GetPeersFromTracker(trackerURL string, infoHash dht.Key, peerID dht.Key, opts *TrackerPeersOptions) (*TrackerPeersResult, error) {
	client := tracker.NewClient(opts.Timeout)

	req := &tracker.AnnounceRequest{
		InfoHash:   infoHash,
		PeerID:     peerID,
		Port:       opts.Port,
		Uploaded:   0,
		Downloaded: 0,
		Left:       0,
		Compact:    opts.Compact,
		Event:      tracker.EventNone,
		NumWant:    200, // Request many peers
	}

	resp, err := client.Announce(trackerURL, req)
	if err != nil {
		return &TrackerPeersResult{
			TrackerURL: trackerURL,
			Error:      err,
		}, err
	}

	return &TrackerPeersResult{
		Peers:      resp.Peers,
		TrackerURL: trackerURL,
		Error:      nil,
	}, nil
}

// FormatAnnounceResult formats an announce result for display
func FormatAnnounceResult(result *TrackerAnnounceResult) string {
	if result.Error != nil {
		return fmt.Sprintf("Announce failed: %v", result.Error)
	}

	resp := result.Response
	output := fmt.Sprintf("\n=== Announce Response from %s ===\n", result.TrackerURL)
	output += fmt.Sprintf("Interval: %d seconds\n", resp.Interval)
	if resp.MinInterval > 0 {
		output += fmt.Sprintf("Min Interval: %d seconds\n", resp.MinInterval)
	}
	if resp.TrackerID != "" {
		output += fmt.Sprintf("Tracker ID: %s\n", resp.TrackerID)
	}
	output += fmt.Sprintf("Complete (seeders): %d\n", resp.Complete)
	output += fmt.Sprintf("Incomplete (leechers): %d\n", resp.Incomplete)
	output += fmt.Sprintf("Peers returned: %d\n", len(resp.Peers))

	if len(resp.Peers) > 0 {
		output += "\nSample peers (first 10):\n"
		max := 10
		if len(resp.Peers) < max {
			max = len(resp.Peers)
		}
		for i := 0; i < max; i++ {
			peer := resp.Peers[i]
			peerType := "IPv4"
			if peer.IP.To4() == nil {
				peerType = "IPv6"
			}
			output += fmt.Sprintf("  [%s] %s:%d\n", peerType, peer.IP.String(), peer.Port)
		}
		if len(resp.Peers) > max {
			output += fmt.Sprintf("  ... and %d more\n", len(resp.Peers)-max)
		}
	}

	return output
}

// FormatScrapeResult formats a scrape result for display
func FormatScrapeResult(result *TrackerScrapeResult, infoHashes []dht.Key) string {
	if result.Error != nil {
		return fmt.Sprintf("Scrape failed: %v", result.Error)
	}

	var output strings.Builder
	fmt.Fprintf(&output, "\n=== Scrape Response from %s ===\n", result.TrackerURL)
	fmt.Fprintf(&output, "Torrents: %d\n\n", len(result.Response.Files))

	for _, hash := range infoHashes {
		hashHex := fmt.Sprintf("%x", hash[:])
		if stats, ok := result.Response.Files[hashHex]; ok {
			fmt.Fprintf(&output, "Info Hash: %s\n", hashHex)
			fmt.Fprintf(&output, "  Seeders: %d\n", stats.Complete)
			fmt.Fprintf(&output, "  Leechers: %d\n", stats.Incomplete)
			fmt.Fprintf(&output, "  Downloaded: %d times\n\n", stats.Downloaded)
		} else {
			output.WriteString(fmt.Sprintf("Info Hash: %s\n", hashHex))
			output.WriteString("  No data available\n\n")
		}
	}

	return output.String()
}

// FormatPeersResult formats a peers result for display
func FormatPeersResult(result *TrackerPeersResult) string {
	if result.Error != nil {
		return fmt.Sprintf("Failed to get peers: %v", result.Error)
	}

	output := fmt.Sprintf("\n=== Peers from %s ===\n", result.TrackerURL)
	output += fmt.Sprintf("Total peers: %d\n\n", len(result.Peers))

	// Count IPv4 vs IPv6
	ipv4Count := 0
	ipv6Count := 0
	for _, peer := range result.Peers {
		if peer.IP.To4() != nil {
			ipv4Count++
		} else {
			ipv6Count++
		}
	}

	output += fmt.Sprintf("IPv4 peers: %d\n", ipv4Count)
	output += fmt.Sprintf("IPv6 peers: %d\n", ipv6Count)

	if len(result.Peers) > 0 {
		output += "\nPeer list (first 20):\n"
		max := 20
		if len(result.Peers) < max {
			max = len(result.Peers)
		}
		for i := 0; i < max; i++ {
			peer := result.Peers[i]
			peerType := "IPv4"
			if peer.IP.To4() == nil {
				peerType = "IPv6"
			}
			output += fmt.Sprintf("  [%s] %s:%d\n", peerType, peer.IP.String(), peer.Port)
		}
		if len(result.Peers) > max {
			output += fmt.Sprintf("  ... and %d more\n", len(result.Peers)-max)
		}
	}

	return output
}

// CmdWSPeers connects to a WebSocket/WebTorrent tracker and discovers WebRTC peers.
//
// Usage:
//
//	bt tracker ws <ws(s)://tracker-url> <infohash>
//
// Connects to the WebSocket tracker, announces with WebRTC offers, and prints each
// peer connection as it is established via the ICE/DataChannel handshake.
// Exits after -timeout (default 30s) or Ctrl-C.
func CmdWSPeers(args []string) error {
	f := cli.NewCommandFlagSet(
		"ws", []string{"wss", "webtorrent"},
		"Announce to a WebSocket/WebTorrent tracker and discover WebRTC peers",
		[]string{"bt tracker ws [options] <ws://tracker-url> <infohash>"},
	)
	timeout := f.Duration("timeout", 30*time.Second, "How long to wait for peer connections")
	numWant := f.Int("numwant", 5, "Number of WebRTC offers to send per announce")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() < 2 {
		f.Usage()
		return fmt.Errorf("missing tracker-url or infohash")
	}

	trackerURL := f.Arg(0)
	if !strings.HasPrefix(trackerURL, "ws://") && !strings.HasPrefix(trackerURL, "wss://") {
		return fmt.Errorf("tracker URL must start with ws:// or wss://")
	}

	infoHash, err := dht.KeyFromHex(f.Arg(1))
	if err != nil {
		return fmt.Errorf("invalid infohash: %w", err)
	}

	var peerID dht.Key
	rand.Read(peerID[:])

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var mu strings.Builder // reuse as counter
	_ = mu
	connCount := 0

	client := &ws.Client{
		URL:      trackerURL,
		PeerID:   [20]byte(peerID),
		InfoHash: [20]byte(infoHash),
		NumWant:  *numWant,
		OnConn: func(conn net.Conn) {
			connCount++
			fmt.Printf("[ws] WebRTC peer #%d connected: local=%s remote=%s\n",
				connCount, conn.LocalAddr(), conn.RemoteAddr())
			// Close immediately — CLI just proves connectivity, doesn't download.
			conn.Close()
		},
	}

	fmt.Printf("Announcing to %s for infohash %x (timeout %s, numwant %d)\n",
		trackerURL, infoHash, *timeout, *numWant)

	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("ws tracker: %w", err)
	}

	fmt.Printf("\nDone. %d WebRTC peer connection(s) established.\n", connCount)
	return nil
}

// Global config holder (set by RunTrackerCommands)
var globalTrackerConfig *TrackerConfig

// RunTrackerCommands handles tracker commands with routing.
// With no subcommand (or flags only), it starts the tracker server.
// Subcommands: server/serve/srv, announce/a, scrape/s, peers/p
func RunTrackerCommands(cfg *TrackerConfig, args []string) error {
	// Store config for command functions to use
	globalTrackerConfig = cfg

	// No subcommand or first arg looks like a non-help flag → default to tracker server
	if len(args) == 0 {
		return CmdTrackerServer(args)
	}
	if strings.HasPrefix(args[0], "-") && args[0] != "-h" && args[0] != "-help" && args[0] != "--help" {
		return CmdTrackerServer(args)
	}

	router := cli.NewRouter("bt tracker")
	router.RegisterCommand(&cli.Command{Name: "server", Aliases: []string{"serve", "srv"}, Description: "Run a BitTorrent tracker server", Run: CmdTrackerServer})
	router.RegisterCommand(&cli.Command{Name: "announce", Aliases: []string{"a"}, Description: "Announce to a tracker", Run: CmdAnnounce})
	router.RegisterCommand(&cli.Command{Name: "scrape", Aliases: []string{"s"}, Description: "Scrape tracker for torrent statistics", Run: CmdScrape})
	router.RegisterCommand(&cli.Command{Name: "peers", Aliases: []string{"p"}, Description: "Get peer list from tracker", Run: CmdPeers})
	router.RegisterCommand(&cli.Command{Name: "ws", Aliases: []string{"wss", "webtorrent"}, Description: "Announce to a WebSocket/WebTorrent tracker", Run: CmdWSPeers})

	return router.Route(args)
}

// CmdTrackerServer starts the HTTP tracker server.
func CmdTrackerServer(args []string) error {
	f := cli.NewCommandFlagSet(
		"server", []string{"serve", "srv"},
		"Run a BitTorrent tracker server",
		[]string{"bt tracker [server] [options]"},
	)
	var (
		port            int
		storage         string
		databasePath    string
		announceMinutes int
		maxPeers        int
		enableIPv6      bool
		externalURL     string
		sslCert         string
		sslKey          string
		allowedIPsStr   string
		enableStats     bool
	)
	f.IntVar(&port, "port", 6969, "tracker server port")
	f.StringVar(&storage, "storage", "memory", "storage backend (memory, sqlite, postgres)")
	f.StringVar(&databasePath, "db", "", "database path (for sqlite)")
	f.IntVar(&announceMinutes, "announce-interval", 30, "announce interval in minutes")
	f.IntVar(&maxPeers, "max-peers", 200, "maximum peers to return")
	f.BoolVar(&enableIPv6, "ipv6", false, "enable IPv6 support")
	f.StringVar(&externalURL, "external-url", "", "external URL for the tracker")
	f.StringVar(&sslCert, "ssl-cert", "", "SSL certificate path")
	f.StringVar(&sslKey, "ssl-key", "", "SSL key path")
	f.StringVar(&allowedIPsStr, "allowed-ips", "", "comma-separated list of allowed IPs")
	f.BoolVar(&enableStats, "stats", true, "enable statistics")
	if err := f.Parse(args); err != nil {
		return err
	}

	var allowedIPs []string
	if allowedIPsStr != "" {
		for _, ip := range strings.Split(allowedIPsStr, ",") {
			if s := strings.TrimSpace(ip); s != "" {
				allowedIPs = append(allowedIPs, s)
			}
		}
	}

	config := &trackerhttp.ServerConfig{
		Port:             port,
		Storage:          storage,
		DatabasePath:     databasePath,
		AnnounceInterval: time.Duration(announceMinutes) * time.Minute,
		MaxPeers:         maxPeers,
		EnableIPv6:       enableIPv6,
		ExternalURL:      externalURL,
		SSLCert:          sslCert,
		SSLKey:           sslKey,
		AllowedIPs:       allowedIPs,
		EnableStats:      enableStats,
	}
	server, err := trackerhttp.NewServer(config)
	if err != nil {
		return fmt.Errorf("create tracker server: %w", err)
	}
	log.Info("starting tracker server", "sub", "tracker", "port", port)
	if err := server.Start(); err != nil {
		return fmt.Errorf("start tracker server: %w", err)
	}
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Info("shutting down tracker server", "sub", "tracker")
	server.Shutdown()
	return nil
}

// CmdAnnounce handles the announce command
func CmdAnnounce(args []string) error {
	f := cli.NewCommandFlagSet(
		"announce", []string{"a"},
		"Announce to a BitTorrent tracker",
		[]string{"bt tracker announce <tracker-url> <infohash>"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required arguments
	if len(args) < 2 {
		f.Usage()
		return fmt.Errorf("missing tracker-url or infohash argument")
	}

	trackerURL := args[0]
	hashHex := args[1]

	// Parse info hash
	infoHash, err := dht.KeyFromHex(hashHex)
	if err != nil {
		return fmt.Errorf("invalid info hash: %w", err)
	}

	// Generate random peer ID
	var peerID dht.Key
	rand.Read(peerID[:])

	// Perform announce
	opts := &TrackerAnnounceOptions{
		Port:       uint16(globalTrackerConfig.Port),
		Uploaded:   0,
		Downloaded: 0,
		Left:       0,
		Event:      tracker.EventNone,
		Compact:    true,
		Timeout:    globalTrackerConfig.Timeout,
	}

	result, err := AnnounceToTracker(trackerURL, infoHash, peerID, opts)
	if err != nil {
		return fmt.Errorf("announce failed: %w", err)
	}

	fmt.Print(FormatAnnounceResult(result))
	return nil
}

// CmdScrape handles the scrape command
func CmdScrape(args []string) error {
	f := cli.NewCommandFlagSet(
		"scrape", []string{"s"},
		"Scrape tracker for torrent statistics",
		[]string{"bt tracker scrape <tracker-url> <infohash> [infohash...]"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required arguments
	if len(args) < 2 {
		f.Usage()
		return fmt.Errorf("missing tracker-url or infohash argument")
	}

	trackerURL := args[0]
	hashes := args[1:]

	// Parse info hashes
	infoHashes := make([]dht.Key, len(hashes))
	for i, hashHex := range hashes {
		hash, err := dht.KeyFromHex(hashHex)
		if err != nil {
			return fmt.Errorf("invalid info hash %s: %w", hashHex, err)
		}
		infoHashes[i] = hash
	}

	// Perform scrape
	opts := &TrackerScrapeOptions{
		Timeout: globalTrackerConfig.Timeout,
	}

	result, err := ScrapeTracker(trackerURL, infoHashes, opts)
	if err != nil {
		return fmt.Errorf("scrape failed: %w", err)
	}

	fmt.Print(FormatScrapeResult(result, infoHashes))
	return nil
}

// CmdPeers handles the peers command
func CmdPeers(args []string) error {
	f := cli.NewCommandFlagSet(
		"peers", []string{"p"},
		"Get peer list from tracker",
		[]string{"bt tracker peers <tracker-url> <infohash>"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required arguments
	if len(args) < 2 {
		f.Usage()
		return fmt.Errorf("missing tracker-url or infohash argument")
	}

	trackerURL := args[0]
	hashHex := args[1]

	// Parse info hash
	infoHash, err := dht.KeyFromHex(hashHex)
	if err != nil {
		return fmt.Errorf("invalid info hash: %w", err)
	}

	// Generate random peer ID
	var peerID dht.Key
	rand.Read(peerID[:])

	// Get peers
	opts := &TrackerPeersOptions{
		Port:    uint16(globalTrackerConfig.Port),
		Compact: true,
		Timeout: globalTrackerConfig.Timeout,
	}

	result, err := GetPeersFromTracker(trackerURL, infoHash, peerID, opts)
	if err != nil {
		return fmt.Errorf("failed to get peers: %w", err)
	}

	fmt.Print(FormatPeersResult(result))
	return nil
}
