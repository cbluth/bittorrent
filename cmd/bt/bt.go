package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	bt "github.com/cbluth/bittorrent/pkg/bt"
	"github.com/cbluth/bittorrent/pkg/cli"
	"github.com/cbluth/bittorrent/pkg/cli/completion"
	bittorrent "github.com/cbluth/bittorrent/pkg/client"
	"github.com/cbluth/bittorrent/pkg/cmd"
	"github.com/cbluth/bittorrent/pkg/config"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
	"github.com/cbluth/bittorrent/pkg/punch"
)

// Global configuration
var globalConfig struct {
	trackersFile        string
	port                uint
	timeout             time.Duration
	resolveTimeout      time.Duration
	noDHT               bool
	noTracker           bool
	dhtPort             uint
	dhtNodes            int
	dhtBootstrapTimeout time.Duration
	logFile             string
	verbose             bool
	cacheDir            string
	cacheFile           string
	socketPath          string // Unix socket path for node daemon
	shareRatio          float64
	jsonLog             bool
	configFile          string
	client              *bittorrent.Client
}

func main() {
	// Create CLI
	app := cli.New("bt", "BitTorrent CLI Tool", "https://github.com/cbluth/bittorrent")

	// Register global flags
	fs := app.FlagSet()
	fs.BoolVar(&globalConfig.verbose, "v", false, "Verbose output")
	fs.UintVar(&globalConfig.port, "port", 6881, "Port for peer connections")
	fs.UintVar(&globalConfig.dhtPort, "dht-port", 6881, "DHT listening port")
	fs.BoolVar(&globalConfig.noDHT, "no-dht", false, "Disable DHT peer discovery")
	fs.DurationVar(&globalConfig.timeout, "timeout", 15*time.Second, "Tracker timeout")
	fs.BoolVar(&globalConfig.noTracker, "no-tracker", false, "Disable tracker peer discovery")
	fs.IntVar(&globalConfig.dhtNodes, "dht-nodes", 100, "Target number of DHT nodes to bootstrap")
	fs.StringVar(&globalConfig.logFile, "log", "", "Path to log file (logs to stdout only if not specified)")
	fs.StringVar(&globalConfig.trackersFile, "trackers", "", "Path to file with tracker URLs (one per line)")
	fs.DurationVar(&globalConfig.resolveTimeout, "resolve-timeout", 2*time.Minute, "Metadata resolution timeout")
	fs.DurationVar(&globalConfig.dhtBootstrapTimeout, "dht-bootstrap-timeout", 60*time.Second, "DHT bootstrap timeout")
	fs.BoolVar(&globalConfig.jsonLog, "json", false, "JSON structured logging (default: human-friendly text)")
	fs.StringVar(&globalConfig.configFile, "c", "", "Path to config file (default: ~/.bt/config.json)")
	fs.StringVar(&globalConfig.socketPath, "socket", "", "Unix socket path for the running node (default: ~/.bt/node.sock)")
	fs.Float64Var(&globalConfig.shareRatio, "ratio", -1, "Share ratio target: 0.0=stop after 100%, 1.5=seed until 1.5x uploaded, unset=config default (seed forever)")

	// Set up pre-run hook for initialization
	// Note: This will be called before every command except 'completion' and 'help'
	app.SetPreRun(func() error {
		err := initializeGlobalConfig()
		if err != nil {
			return err
		}
		return nil
	})

	// Register commands
	// ── Torrents ───────────────────────────────────────────────────────────────

	app.RegisterCommand(&cli.Command{
		Name:        "download",
		Description: "Download a torrent",
		Aliases:     []string{"dl", "get"},
		Category:    "Torrents",
		Run:         cmdBitTorrent,
	})

	app.RegisterCommand(&cli.Command{
		Name:        "torrent",
		Description: "Parse and inspect .torrent files",
		Aliases:     []string{"t"},
		Category:    "Torrents",
		Run:         cmdTorrent,
	})

	// "magnet" and "hash" are aliases — both resolve metadata from any identifier
	// (magnet URI, raw hex info hash, or btih: URN).
	app.RegisterCommand(&cli.Command{
		Name:        "magnet",
		Description: "Resolve a magnet URI or info hash",
		Aliases:     []string{"m", "hash", "resolve", "metadata"},
		Category:    "Torrents",
		Run:         cmdMagnet,
	})

	app.RegisterCommand(&cli.Command{
		Name:        "fetch",
		Description: "Fetch pieces or byte ranges from peers",
		Aliases:     []string{"f"},
		Category:    "Torrents",
		Run: func(args []string) error {
			cfg := &cmd.FetchConfig{
				Port:    globalConfig.port,
				Timeout: globalConfig.timeout,
				Client:  globalConfig.client,
			}
			return cmd.RunFetchCommands(cfg, args)
		},
	})

	// ── Discovery ──────────────────────────────────────────────────────────────

	app.RegisterCommand(&cli.Command{
		Name: "tracker",
		Run: func(args []string) error {
			cfg := &cmd.TrackerConfig{
				Port:    globalConfig.port,
				Timeout: globalConfig.timeout,
			}
			return cmd.RunTrackerCommands(cfg, args)
		},
		Aliases:     []string{"tr"},
		Category:    "Discovery",
		Description: "Tracker server; client: announce, scrape, peers",
		SkipPreRun:  true,
	})

	app.RegisterCommand(&cli.Command{
		Name: "dht",
		Run: func(args []string) error {
			cfg := &bt.DHTConfig{
				DHTPort:             globalConfig.dhtPort,
				DHTNodes:            globalConfig.dhtNodes,
				DHTBootstrapTimeout: globalConfig.dhtBootstrapTimeout,
				CacheDir:            globalConfig.cacheDir,
				CacheFile:           globalConfig.cacheFile,
			}
			return cmd.RunDHTCommands(cfg, args)
		},
		Aliases:     []string{"d"},
		Category:    "Discovery",
		Description: "DHT node operations",
	})

	// ── Server ─────────────────────────────────────────────────────────────────

	app.RegisterCommand(&cli.Command{
		Name:        "server",
		Description: "HTTP streaming server (lazy per-request resolution)",
		Aliases:     []string{"serve", "http"},
		Category:    "Server",
		Run:         cmdServer,
	})

	app.RegisterCommand(&cli.Command{
		Name:        "node",
		Description: "Persistent background daemon (DHT + peer listener)",
		Aliases:     []string{"daemon"},
		Category:    "Server",
		Run:         cmdNode,
		SkipPreRun:  true,
	})

	// ── Tools ──────────────────────────────────────────────────────────────────

	app.RegisterCommand(&cli.Command{
		Name:        "config",
		Run:         config.Run,
		Aliases:     []string{"cfg"},
		Category:    "Tools",
		Description: "Manage configuration",
		SkipPreRun:  true,
	})

	app.RegisterCommand(&cli.Command{
		Name:        "bencode",
		Category:    "Tools",
		Run:         cmd.Run,
		Aliases:     []string{"b", "encode", "decode"},
		Description: "Encode/decode bencoded data",
		SkipPreRun:  true,
	})

	app.RegisterCommand(&cli.Command{
		Name:        "peer",
		Description: "Low-level peer wire operations",
		Aliases:     []string{"p"},
		Category:    "Tools",
		SkipPreRun:  true,
		Run: func(args []string) error {
			cfg := &cmd.PeerConfig{
				Port:    globalConfig.port,
				Timeout: globalConfig.timeout,
			}
			return cmd.RunPeerCommands(cfg, args)
		},
	})

	app.RegisterCommand(&cli.Command{
		Name:        "net",
		Description: "Network diagnostics (NAT, external IP, latency)",
		Aliases:     []string{"network"},
		Category:    "Tools",
		Run: func(args []string) error {
			cfg := &cmd.NetConfig{
				Port:    globalConfig.port,
				Timeout: globalConfig.timeout,
			}
			return cmd.RunNetCommands(cfg, args)
		},
	})

	app.RegisterCommand(&cli.Command{
		Name:        "completion",
		Description: "Generate shell completion",
		Aliases:     []string{},
		Category:    "Tools",
		Run:         cmdCompletion(app),
		SkipPreRun:  true,
	})

	// Run CLI
	if err := app.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func initializeGlobalConfig() error {
	// Wire JSON flag to log mode (must be set before level/topics to avoid rebuild churn).
	if globalConfig.jsonLog {
		log.SetMode(log.ModeServer)
	}

	// Wire verbose flag to pkg/log slog level (Info by default, Debug when -v).
	if globalConfig.verbose {
		log.SetLevelFromInt(3) // Debug
	} else {
		log.SetLevelFromInt(2) // Info
	}

	// Load per-subsystem log topic filter from config.
	var ops *config.Operations
	if globalConfig.configFile != "" {
		ops = config.NewOperationsWithPath(globalConfig.configFile)
	} else {
		ops = config.NewOperations()
	}
	log.SetTopics(ops.GetLogTopics())

	// Validate flag combinations
	if globalConfig.noDHT && globalConfig.noTracker {
		return fmt.Errorf("cannot use both -no-dht and -no-tracker flags (need at least one peer discovery method)")
	}

	// Resolve share ratio: -1 (flag default) means "use config file value".
	if globalConfig.shareRatio == -1 {
		globalConfig.shareRatio = ops.GetShareRatio() // Inf(1) when not configured
	} else if math.IsNaN(globalConfig.shareRatio) || globalConfig.shareRatio < 0 {
		return fmt.Errorf("invalid -ratio value: must be >= 0 or unset")
	}

	// Set up cache directory and unified cache file
	globalConfig.cacheDir = config.GetBTDir()
	globalConfig.cacheFile = config.GetCachePath()
	if err := os.MkdirAll(globalConfig.cacheDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Load trackers
	enableTrackers := !globalConfig.noTracker
	var trackersList []string

	if enableTrackers {
		// Create cache operations for tracker loading
		cacheOps := config.NewCacheOperations()
		trackersList = bt.LoadTrackers(&bt.LoadTrackersOptions{
			TrackersFile: globalConfig.trackersFile,
			CacheOps:     cacheOps,
		})
	} else {
		log.Info("trackers disabled via -no-tracker flag", "sub", "tracker")
	}

	// Create client
	client, err := bittorrent.NewClient(&bittorrent.Config{
		Port:     uint16(globalConfig.port),
		Timeout:  globalConfig.timeout,
		Trackers: trackersList,
	})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	globalConfig.client = client

	log.Debug("client initialized", "sub", "bittorrent", "peerID", fmt.Sprintf("%x", client.PeerID()), "port", client.Port())

	return nil
}

func cmdTorrent(args []string) error {
	f := cli.NewCommandFlagSet(
		"torrent", []string{"t"},
		"Load and query a .torrent file, or url pointing to a torrent file, then perform operations on the torrent file like query or editing the torrent file",
		[]string{"bt torrent [options] <file.torrent>"},
	)

	// Parse flags
	var outputFile string
	f.StringVar(&outputFile, "o", "", "Output file path for torrent information")
	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required argument
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing torrent file argument")
	}

	torrentFile := args[0]

	ctx := context.Background()
	return bt.HandleTorrentFile(ctx, globalConfig.client, torrentFile, config.NewCacheOperations(), globalConfig.verbose, globalConfig.noTracker, outputFile)
}

func cmdMagnet(args []string) error {
	f := cli.NewCommandFlagSet(
		"magnet", []string{"m", "magnet"},
		"Parse and query a magnet link or hash",
		[]string{"bt magnet [options] <magnet-uri>"},
	)

	// Parse flags
	var outputFile string
	var waitForNode bool
	f.StringVar(&outputFile, "o", "", "Output file path for torrent metadata")
	f.BoolVar(&waitForNode, "wait", false, "Wait for running node to bootstrap, then route DHT peer discovery through it")
	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required argument
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing magnet URI argument")
	}

	magnetURI := args[0]

	// If not a full magnet URI, convert hash to magnet URI
	if len(magnetURI) > 0 && magnetURI[0] != 'm' {
		// Assume it's a raw hash
		magnetURI = "magnet:?xt=urn:btih:" + magnetURI
	}

	ctx := context.Background()

	// --wait: route peer discovery through the running node's warm DHT
	if waitForNode {
		sockPath := resolveSocketPath("")
		if _, err := os.Stat(sockPath); err == nil {
			nodeClient := newNodeClient(sockPath)
			log.Debug("waiting for node to be ready", "sub", "magnet", "socket", sockPath)
			if err := waitForNodeReady(ctx, nodeClient, globalConfig.resolveTimeout); err != nil {
				log.Debug("node wait failed, falling back to ephemeral", "sub", "magnet", "err", err)
			} else {
				// Extract info hash for DHT query
				hashHex := bt.ExtractInfoHashFromMagnet(magnetURI)
				if hashHex != "" {
					log.Debug("getting peers from node DHT", "sub", "magnet", "hash", hashHex)
					peers, err := getDHTpeersFromNode(ctx, nodeClient, hashHex)
					if err != nil {
						log.Debug("node DHT query failed, falling back", "sub", "magnet", "err", err)
					} else if len(peers) > 0 {
						log.Debug("got peers from node, resolving metadata", "sub", "magnet", "peers", len(peers))
						infoHash, err := dht.KeyFromHex(hashHex)
						if err == nil {
							resolveCtx, cancel := context.WithTimeout(ctx, globalConfig.resolveTimeout)
							torrentInfo, err := globalConfig.client.ResolveMetadataFromPeers(resolveCtx, infoHash, peers)
							cancel()
							if err == nil {
								fmt.Println("✓ Successfully resolved metadata (via node socket)!")
								bt.PrintTorrentInfo(torrentInfo)
								return nil
							}
							log.Debug("peer-based resolution failed, falling back", "sub", "magnet", "err", err)
						}
					} else {
						log.Debug("no peers returned by node, falling back", "sub", "magnet")
					}
				}
			}
		} else {
			log.Debug("no node socket, running ephemeral session", "sub", "magnet", "socket", sockPath)
		}
	}

	opts := &bt.MagnetOptions{
		UseDHT:              !globalConfig.noDHT,
		DHTPort:             globalConfig.dhtPort,
		DHTNodes:            globalConfig.dhtNodes,
		DHTBootstrapTimeout: globalConfig.dhtBootstrapTimeout,
		Resolve:             true,
		ResolveTimeout:      globalConfig.resolveTimeout,
		Verbose:             globalConfig.verbose,
		NoTracker:           globalConfig.noTracker,
		OutputFile:          outputFile,
	}

	return bt.HandleMagnet(ctx, globalConfig.client, magnetURI, config.NewCacheOperations(), globalConfig.cacheDir, opts)
}

func cmdBitTorrent(args []string) error {
	f := cli.NewCommandFlagSet(
		"download", []string{"dl", "get"},
		"Download torrents using optimized CSP-based downloader with resume support",
		[]string{"bt download [options] <torrent-file|magnet>"},
	)

	// Parse flags
	var outputDir string
	var sequential bool
	f.StringVar(&outputDir, "o", ".", "Output directory for downloaded files")
	f.BoolVar(&sequential, "sequential", false, "Download pieces sequentially (for streaming)")
	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required argument
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing torrent file or magnet URI argument")
	}

	input := args[0]
	ctx := context.Background()

	strategy := "rarest-first"
	if sequential {
		strategy = "sequential"
	}
	log.Info("starting download", "sub", "download", "input", input, "output", outputDir, "strategy", strategy)

	// Run the optimized download
	return bt.RunBitTorrentDownload(ctx, globalConfig.client, input, outputDir, sequential, globalConfig.shareRatio)
}

func cmdServer(args []string) error {
	f := cli.NewCommandFlagSet(
		"server", []string{"serve", "http"},
		"On-demand HTTP streaming UI; navigate to /bt/<infohash> to start streaming any torrent",
		[]string{"bt server [options]"},
	)

	var port int
	var outputDir string
	f.IntVar(&port, "port", 8080, "HTTP server port")
	f.StringVar(&outputDir, "o", filepath.Join(config.GetBTDir(), "server"), "Directory for downloaded files")
	if err := f.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return bt.RunServeCommand(ctx, globalConfig.client, outputDir, &bt.ServeConfig{
		Port:       port,
		ShareRatio: globalConfig.shareRatio,
	})
}

// cmdNode dispatches bt node <run|start|stop|status>.
//
// The node is the long-running daemon counterpart to the ephemeral per-command
// sessions.  It keeps a stable DHT node ID, accumulates routing-table knowledge,
// and exposes an HTTP API over a Unix domain socket so other bt commands can
// route through it instead of bootstrapping fresh sessions.
//
// Subcommands:
//
//	run    — start in the foreground; Unix socket open until SIGTERM/SIGINT
//	start  — daemonize and return immediately (re-exec self with nohup)
//	stop   — send POST /shutdown to the running node via the socket
//	status — print routing table size, connected peers, and active torrents
func cmdNode(args []string) error {
	f := cli.NewCommandFlagSet(
		"node", []string{"daemon"},
		"Persistent BitTorrent node daemon (DHT + peer listener + Unix socket API)",
		[]string{
			"bt node run    [options]   Start in foreground",
			"bt node start  [options]   Daemonize and return",
			"bt node stop               Stop the running node",
			"bt node status             Print node status",
			"bt node logs   [-f]        Print node log (tail with -f)",
		},
	)

	if err := f.Parse(args); err != nil {
		return err
	}

	sub := "run"
	if f.NArg() > 0 {
		sub = f.Arg(0)
	}

	switch sub {
	case "run":
		return cmdNodeRun(f.Args()[1:])
	case "start":
		return cmdNodeStart(f.Args()[1:])
	case "stop":
		return cmdNodeStop(f.Args()[1:])
	case "status":
		return cmdNodeStatus(f.Args()[1:])
	case "logs", "log":
		return cmdNodeLogs(f.Args()[1:])
	default:
		f.Usage()
		return fmt.Errorf("unknown node subcommand: %q", sub)
	}
}

// cmdNodeRun starts the node in the foreground.
//
// Lifecycle:
//  1. Resolve socket path from -socket / $BT_SOCKET / ~/.bt/node.sock
//  2. Load or generate a stable DHT node ID (persisted in config)
//  3. Create DHT node on -dht-port, load cached routing table
//  4. Bootstrap DHT in a background goroutine; close readyCh when done
//  5. Serve HTTP API over a Unix domain socket (see CLI.md §Node Socket API)
//  6. Write PID file if -pid-file is set
//  7. Block on SIGINT/SIGTERM or POST /shutdown
//  8. Graceful shutdown: save routing table, remove socket, remove PID file
func cmdNodeRun(args []string) error {
	f := flag.NewFlagSet("node run", flag.ExitOnError)
	var (
		dhtPortFlag int
		socketPath  string
		pidFile     string
	)
	f.IntVar(&dhtPortFlag, "dht-port", int(globalConfig.dhtPort), "DHT listener port (UDP)")
	f.StringVar(&socketPath, "socket", "", "Unix socket path (default: ~/.bt/node.sock)")
	f.StringVar(&pidFile, "pid-file", "", "Write PID to this file (optional)")
	if err := f.Parse(args); err != nil {
		return err
	}

	sockPath := resolveSocketPath(socketPath)

	// Set up logging (PreRun is skipped for node commands; do minimal setup here).
	if globalConfig.verbose {
		log.SetLevelFromInt(3) // Debug
	}

	// Load or generate stable node ID
	nodeID := loadOrGenerateNodeID()
	log.Info("node starting", "sub", "node", "id", fmt.Sprintf("%x", nodeID[:8]))

	// Create DHT node
	bootstrapNodes := config.DHTBootstrapNodes()
	dhtNode, err := dht.NewNodeWithID(uint16(dhtPortFlag), bootstrapNodes, nodeID)
	if err != nil {
		return fmt.Errorf("failed to create DHT node: %w", err)
	}
	defer func() {
		cacheOps := config.NewCacheOperations()
		if err := bt.SaveDHTNodesToCache(cacheOps, dhtNode); err != nil {
			log.Warn("failed to save routing table", "sub", "node", "err", err)
		}
		dhtNode.Shutdown()
	}()

	// Load cached routing table
	cacheOps := config.NewCacheOperations()
	if n, err := bt.LoadDHTNodesFromCache(cacheOps, dhtNode); err != nil {
		log.Warn("failed to load cached nodes", "sub", "node", "err", err)
	} else if n > 0 {
		log.Debug("loaded cached DHT nodes", "sub", "node", "count", n)
	}

	// Signal handling
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startTime := time.Now()
	readyCh := make(chan struct{}) // closed when DHT bootstrap completes

	// Bootstrap DHT and run 15-minute maintenance in background
	go func() {
		log.Info("bootstrapping DHT", "sub", "node", "target", globalConfig.dhtNodes, "timeout", globalConfig.dhtBootstrapTimeout)
		bootstrapCtx, cancel := context.WithTimeout(ctx, globalConfig.dhtBootstrapTimeout)
		defer cancel()
		if err := dhtNode.Bootstrap(bootstrapCtx, globalConfig.dhtNodes); err != nil && err != context.DeadlineExceeded {
			log.Warn("bootstrap warning", "sub", "node", "err", err)
		}
		log.Info("DHT ready", "sub", "node", "nodes", dhtNode.DHT().Length())
		close(readyCh)

		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				log.Debug("DHT maintenance", "sub", "node", "nodes", dhtNode.DHT().Length())
				dhtNode.DHT().MaintainRoutingTable(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	// HTTP API mux
	mux := http.NewServeMux()

	// GET /status — returns 503 while bootstrapping, 200 when ready
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		state := "bootstrapping"
		code := http.StatusServiceUnavailable
		select {
		case <-readyCh:
			state = "ready"
			code = http.StatusOK
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":     state,
			"dht_nodes": dhtNode.DHT().Length(),
			"uptime":    time.Since(startTime).Round(time.Second).String(),
		})
	})

	// GET /dht/peers/{hash} — find peers for an infohash via the node's DHT
	mux.HandleFunc("GET /dht/peers/", func(w http.ResponseWriter, r *http.Request) {
		hashHex := strings.TrimPrefix(r.URL.Path, "/dht/peers/")
		if hashHex == "" {
			http.Error(w, "missing hash", http.StatusBadRequest)
			return
		}
		infoHash, err := dht.KeyFromHex(hashHex)
		if err != nil {
			http.Error(w, "invalid hash: "+err.Error(), http.StatusBadRequest)
			return
		}
		peers, err := dhtNode.FindPeersForInfoHash(infoHash, 50, 16)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		peerStrs := make([]string, 0, len(peers))
		for _, p := range peers {
			peerStrs = append(peerStrs, p.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hash":  hashHex,
			"peers": peerStrs,
		})
	})

	// GET /peers — DHT routing-table nodes known to this node.
	// This node daemon does not run download sessions, so "peers" means
	// DHT nodes from the routing table.
	mux.HandleFunc("GET /peers", func(w http.ResponseWriter, r *http.Request) {
		nodes := dhtNode.DHT().GetGoodNodes()
		type nodeInfo struct {
			ID   string `json:"id"`
			Addr string `json:"addr"`
		}
		result := make([]nodeInfo, 0, len(nodes))
		for _, n := range nodes {
			addr := n.Address()
			if addr == nil {
				continue
			}
			result = append(result, nodeInfo{
				ID:   n.ID().Hex(),
				Addr: addr.String(),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"peers": result})
	})

	// GET /torrents — infohashes discovered by this node via DHT sampling.
	mux.HandleFunc("GET /torrents", func(w http.ResponseWriter, r *http.Request) {
		cacheOps := config.NewCacheOperations()
		infohashes, err := cacheOps.GetInfohashes()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"torrents": infohashes})
	})

	// POST /shutdown — trigger graceful shutdown
	shutdownCh := make(chan struct{}, 1)
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "shutting down"})
		select {
		case shutdownCh <- struct{}{}:
		default:
		}
	})

	// Remove stale socket file, then listen
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", sockPath, err)
	}
	log.Info("unix socket ready", "sub", "node", "path", sockPath)

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	// Write PID file
	if pidFile != "" {
		data := strconv.Itoa(os.Getpid()) + "\n"
		if err := os.WriteFile(pidFile, []byte(data), 0644); err != nil {
			log.Warn("failed to write PID file", "sub", "node", "err", err)
		}
		defer os.Remove(pidFile)
	}

	log.Info("node ready", "sub", "node", "pid", os.Getpid())

	// Block until signal or /shutdown request
	select {
	case <-ctx.Done():
		log.Info("received signal, shutting down", "sub", "node")
	case <-shutdownCh:
		log.Info("received shutdown request", "sub", "node")
	}

	// Graceful HTTP server shutdown (gives in-flight requests 10s to finish)
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	_ = os.Remove(sockPath)
	log.Info("shutdown complete", "sub", "node")
	return nil
}

// cmdNodeStart daemonizes the node and returns immediately.
//
// Re-execs the current binary with "node run" args, detached from the
// terminal, with stdout/stderr redirected to ~/.bt/node.log.
func cmdNodeStart(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find executable: %w", err)
	}

	sockPath := resolveSocketPath("")
	logPath := filepath.Join(config.GetBTDir(), "node.log")

	// Check if node is already running
	if _, statErr := os.Stat(sockPath); statErr == nil {
		c := newNodeClient(sockPath)
		c.Timeout = 2 * time.Second
		if resp, err := c.Get("http://localhost/status"); err == nil {
			resp.Body.Close()
			fmt.Printf("Node already running (socket: %s)\n", sockPath)
			return nil
		}
		// Stale socket — remove it so node run can bind
		_ = os.Remove(sockPath)
	}

	// Build child command: inherit relevant global flags, then any extra args
	nodeArgs := []string{"node", "run"}
	if globalConfig.dhtPort != 6881 {
		nodeArgs = append(nodeArgs, fmt.Sprintf("-dht-port=%d", globalConfig.dhtPort))
	}
	nodeArgs = append(nodeArgs, args...)

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("cannot open log file %s: %w", logPath, err)
	}

	child := exec.Command(exe, nodeArgs...)
	child.Stdout = logFile
	child.Stderr = logFile
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from terminal

	if err := child.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("failed to start node: %w", err)
	}
	_ = logFile.Close()              // child inherited the fd; close parent's copy
	go func() { _ = child.Wait() }() // reap child to avoid zombies

	fmt.Printf("Node started  PID %d\n", child.Process.Pid)
	fmt.Printf("Socket:       %s\n", sockPath)
	fmt.Printf("Log:          %s\n", logPath)
	return nil
}

// cmdNodeStop sends POST /shutdown to the running node over the Unix socket.
func cmdNodeStop(args []string) error {
	f := flag.NewFlagSet("node stop", flag.ExitOnError)
	var socketPath string
	f.StringVar(&socketPath, "socket", "", "Unix socket path")
	if err := f.Parse(args); err != nil {
		return err
	}

	sockPath := resolveSocketPath(socketPath)
	c := newNodeClient(sockPath)
	resp, err := c.Post("http://localhost/shutdown", "application/json", nil)
	if err != nil {
		fmt.Println("node not running (cannot connect to socket)")
		return nil
	}
	defer resp.Body.Close()
	fmt.Println("shutdown signal sent")
	return nil
}

// cmdNodeStatus prints routing table size, connected peers, and active torrents.
func cmdNodeStatus(args []string) error {
	f := flag.NewFlagSet("node status", flag.ExitOnError)
	var socketPath string
	f.StringVar(&socketPath, "socket", "", "Unix socket path")
	if err := f.Parse(args); err != nil {
		return err
	}

	sockPath := resolveSocketPath(socketPath)
	c := newNodeClient(sockPath)
	resp, err := c.Get("http://localhost/status")
	if err != nil {
		fmt.Println("node not running")
		return nil
	}
	defer resp.Body.Close()

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return fmt.Errorf("failed to decode status: %w", err)
	}
	out, _ := json.MarshalIndent(status, "", "  ")
	fmt.Println(string(out))
	return nil
}

// cmdNodeLogs prints the node log file. With -f it polls for new lines until
// interrupted (like tail -f).
func cmdNodeLogs(args []string) error {
	f := flag.NewFlagSet("node logs", flag.ExitOnError)
	var follow bool
	f.BoolVar(&follow, "f", false, "Follow: poll for new lines until interrupted")
	if err := f.Parse(args); err != nil {
		return err
	}

	logPath := filepath.Join(config.GetBTDir(), "node.log")
	fh, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no log file found (has the node been started?)")
			return nil
		}
		return fmt.Errorf("open log: %w", err)
	}
	defer fh.Close()

	if _, err := io.Copy(os.Stdout, fh); err != nil {
		return err
	}

	if !follow {
		return nil
	}

	// Follow mode: poll for appended lines, exit on Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		if _, err := io.Copy(os.Stdout, fh); err != nil {
			return err
		}
	}
}

// ── Node socket helpers ────────────────────────────────────────────────────────

// resolveSocketPath returns the Unix socket path, applying the precedence:
// explicit flag arg → globalConfig.socketPath (-socket flag) → $BT_SOCKET → default.
func resolveSocketPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if globalConfig.socketPath != "" {
		return globalConfig.socketPath
	}
	if v := os.Getenv("BT_SOCKET"); v != "" {
		return v
	}
	return filepath.Join(config.GetBTDir(), "node.sock")
}

// newNodeClient returns an *http.Client that dials the Unix domain socket at sockPath.
// All requests use the placeholder host "localhost"; only the path matters.
func newNodeClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 10 * time.Second,
	}
}

// loadOrGenerateNodeID loads the persistent DHT node ID from config,
// or generates a new BEP 42-compliant one and saves it.
// If the public IP is available, the node ID is validated against it;
// a stale ID (wrong IP) is regenerated automatically.
func loadOrGenerateNodeID() dht.Key {
	ops := config.NewOperations()

	// Try to discover public IP for BEP 42 compliance (short timeout).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	publicIP, ipErr := punch.DiscoverPublicIP(ctx)

	// Check cached node ID.
	if hex, err := ops.GetNodeID(); err == nil && hex != "" {
		if id, err := dht.KeyFromHex(hex); err == nil {
			// If we have a public IP, validate the cached ID against it.
			if ipErr == nil {
				if dht.ValidateNodeID(id, publicIP) {
					return id
				}
				log.Info("cached node ID does not match public IP, regenerating (BEP 42)", "sub", "dht")
			} else {
				// Can't determine IP — trust the cached ID.
				return id
			}
		}
	}

	// Generate new node ID.
	var id dht.Key
	if ipErr == nil {
		id = dht.GenerateSecureNodeID(publicIP)
		log.Info("generated BEP 42 node ID", "sub", "dht", "ip", publicIP)
	} else {
		id = dht.RandomKey()
		log.Info("generated random node ID (public IP unavailable)", "sub", "dht")
	}

	if err := ops.SetNodeID(id.Hex()); err != nil {
		log.Warn("could not save node ID", "sub", "node", "err", err)
	}
	return id
}

// waitForNodeReady polls GET /status until the node reports state=="ready"
// or the context deadline is exceeded.
func waitForNodeReady(ctx context.Context, c *http.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node did not become ready within %v", timeout)
		}
		resp, err := c.Get("http://localhost/status")
		if err == nil {
			var status map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&status)
			resp.Body.Close()
			if st, ok := status["state"].(string); ok && st == "ready" {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// getDHTpeersFromNode calls GET /dht/peers/{hash} on the node socket and returns
// the peer list as []net.Addr.
func getDHTpeersFromNode(ctx context.Context, c *http.Client, hashHex string) ([]net.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/dht/peers/"+hashHex, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("node returned status %d", resp.StatusCode)
	}
	var result struct {
		Peers []string `json:"peers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	addrs := make([]net.Addr, 0, len(result.Peers))
	for _, p := range result.Peers {
		host, portStr, err := net.SplitHostPort(p)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		addrs = append(addrs, &net.UDPAddr{IP: net.ParseIP(host), Port: port})
	}
	return addrs, nil
}

// cmdCompletion generates shell completion scripts
func cmdCompletion(app *cli.CLI) func([]string) error {
	return func(args []string) error {
		f := cli.NewCommandFlagSet(
			"completion", nil,
			"Generate shell completion script",
			[]string{"bt completion [options] <shell>"},
		)

		// Parse flags
		err := f.Parse(args)
		if err != nil {
			return err
		}
		args = f.Args()

		// Check for required argument
		if len(args) == 0 {
			f.Usage()
			return fmt.Errorf("missing shell argument")
		}

		shellName := args[0]

		// Create completion generator
		gen := completion.New("bt")

		// Add global flags
		gen.AddFlagsFromFlagSet(app.FlagSet())

		// Per-command metadata: flags, subcommands, file-type hints.
		// Aliases are kept in sync with the registrations in main().
		gen.AddCommand(completion.CommandInfo{
			Name:    "config",
			Aliases: []string{"cfg"},
			SubCommands: []completion.CommandInfo{
				{Name: "get", Aliases: []string{"g"}, NoArgCompletion: true},
				{Name: "set", Aliases: []string{"s"}, NoArgCompletion: true},
				{Name: "append", Aliases: []string{"add", "a"}, NoArgCompletion: true},
				{Name: "del", Aliases: []string{"delete", "rm"}, NoArgCompletion: true},
				{Name: "list", Aliases: []string{"ls", "show"}, NoArgCompletion: true},
				{Name: "edit", Aliases: []string{"e"}, NoArgCompletion: true},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:           "torrent",
			Aliases:        []string{"t"},
			CompletionFile: "*.torrent",
			Flags: []completion.FlagInfo{
				{Name: "o", TakesValue: true},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:            "magnet",
			Aliases:         []string{"m", "hash", "resolve", "metadata"},
			NoArgCompletion: true, // arg is a URI/hash — not completable from filesystem
			Flags: []completion.FlagInfo{
				{Name: "o", TakesValue: true},
				{Name: "wait"},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:    "tracker",
			Aliases: []string{"tr"},
			SubCommands: []completion.CommandInfo{
				{Name: "server", Aliases: []string{"serve", "srv"}, NoArgCompletion: true,
					Flags: []completion.FlagInfo{
						{Name: "port", TakesValue: true},
						{Name: "storage", TakesValue: true},
						{Name: "db", TakesValue: true},
						{Name: "announce-interval", TakesValue: true},
						{Name: "max-peers", TakesValue: true},
						{Name: "ipv6"},
						{Name: "external-url", TakesValue: true},
						{Name: "stats"},
					}},
				{Name: "announce", Aliases: []string{"a"}, NoArgCompletion: true},
				{Name: "scrape", Aliases: []string{"s"}, NoArgCompletion: true},
				{Name: "peers", Aliases: []string{"p"}, NoArgCompletion: true},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:    "bencode",
			Aliases: []string{"b", "encode", "decode"},
			// reads stdin or a file
		})
		gen.AddCommand(completion.CommandInfo{
			Name:    "dht",
			Aliases: []string{"d"},
			SubCommands: []completion.CommandInfo{
				{Name: "bootstrap", Aliases: []string{"b"}, NoArgCompletion: true},
				{Name: "get-peers", Aliases: []string{"peers", "get_peers"}, NoArgCompletion: true},
				{Name: "find-node", Aliases: []string{"find_node", "fn"}, NoArgCompletion: true},
				{Name: "ping", Aliases: []string{"p"}, NoArgCompletion: true},
				{Name: "announce-peer", Aliases: []string{"announce_peer", "announce"}, NoArgCompletion: true},
				{Name: "sample", NoArgCompletion: true},
				{Name: "stats", Aliases: []string{"s"}, NoArgCompletion: true},
				{Name: "server", Aliases: []string{"srv"}, NoArgCompletion: true},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:           "download",
			Aliases:        []string{"dl", "get"},
			CompletionFile: "*.torrent",
			Flags: []completion.FlagInfo{
				{Name: "o", TakesValue: true},
				{Name: "sequential"},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:            "serve",
			Aliases:         []string{"server", "http"},
			NoArgCompletion: true,
			Flags: []completion.FlagInfo{
				{Name: "port", TakesValue: true},
				{Name: "o", TakesValue: true},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:    "peer",
			Aliases: []string{"p"},
			SubCommands: []completion.CommandInfo{
				{Name: "connect", Aliases: []string{"c"}, NoArgCompletion: true},
				{Name: "handshake", Aliases: []string{"hs"}, NoArgCompletion: true},
				{Name: "metadata", Aliases: []string{"meta", "info"}, NoArgCompletion: true},
				{Name: "raw", Aliases: []string{"wire"}, NoArgCompletion: true},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:    "net",
			Aliases: []string{"network"},
			SubCommands: []completion.CommandInfo{
				{Name: "ports", Aliases: []string{"list"}, NoArgCompletion: true},
				{Name: "nat-test", Aliases: []string{"nat"}, NoArgCompletion: true},
				{Name: "external-ip", Aliases: []string{"ip", "myip"}, NoArgCompletion: true},
				{Name: "sniff", Aliases: []string{"capture"}, NoArgCompletion: true},
				{Name: "latency", Aliases: []string{"ping"}, NoArgCompletion: true},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:    "fetch",
			Aliases: []string{"f"},
			SubCommands: []completion.CommandInfo{
				{Name: "piece", Aliases: []string{"p"}, NoArgCompletion: true},
				{Name: "range", Aliases: []string{"r", "bytes"}, NoArgCompletion: true},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:    "node",
			Aliases: []string{"daemon"},
			SubCommands: []completion.CommandInfo{
				{
					Name: "run",
					Flags: []completion.FlagInfo{
						{Name: "dht-port", TakesValue: true},
						{Name: "socket", TakesValue: true},
						{Name: "pid-file", TakesValue: true},
					},
					NoArgCompletion: true,
				},
				{
					Name: "start",
					Flags: []completion.FlagInfo{
						{Name: "dht-port", TakesValue: true},
					},
					NoArgCompletion: true,
				},
				{
					Name: "stop",
					Flags: []completion.FlagInfo{
						{Name: "socket", TakesValue: true},
					},
					NoArgCompletion: true,
				},
				{
					Name: "status",
					Flags: []completion.FlagInfo{
						{Name: "socket", TakesValue: true},
					},
					NoArgCompletion: true,
				},
				{
					Name:            "logs",
					Aliases:         []string{"log"},
					Flags:           []completion.FlagInfo{{Name: "f"}},
					NoArgCompletion: true,
				},
			},
		})
		gen.AddCommand(completion.CommandInfo{
			Name:            "completion",
			CompletionWords: []string{"bash", "zsh", "fish"},
		})
		// "help" is handled as a built-in by cli.Run but is never registered as
		// a Command, so we add it here for completion purposes only.
		gen.AddCommand(completion.CommandInfo{
			Name:            "help",
			NoArgCompletion: true,
		})

		// Generate completion script
		script, err := gen.Generate(shellName)
		if err != nil {
			return fmt.Errorf("failed to generate completion: %w", err)
		}

		fmt.Print(script)
		return nil
	}
}
