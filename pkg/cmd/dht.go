// pkg/cmd/dht.go - DHT CLI commands extracted from pkg/bt
package cmd

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/cbluth/bittorrent/pkg/bt"
	"github.com/cbluth/bittorrent/pkg/cli"
	"github.com/cbluth/bittorrent/pkg/config"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
)

// Global DHT config holder (set by RunDHTCommands)
var globalDHTConfig *bt.DHTConfig

// RunDHTCommands handles DHT commands with routing
func RunDHTCommands(cfg *bt.DHTConfig, args []string) error {
	// Store config for command functions to use
	globalDHTConfig = cfg

	// Create router and register all command handlers with their aliases
	router := cli.NewRouter("bt dht")
	router.RegisterCommand(&cli.Command{Name: "bootstrap", Aliases: []string{"b"}, Description: "Bootstrap DHT and display routing table", Run: CmdBootstrap})
	router.RegisterCommand(&cli.Command{Name: "get-peers", Aliases: []string{"peers", "get_peers"}, Description: "Find peers for an infohash using DHT", Run: CmdGetPeers})
	router.RegisterCommand(&cli.Command{Name: "find-node", Aliases: []string{"find_node", "fn"}, Description: "Find a specific DHT node by its ID", Run: CmdFindNode})
	router.RegisterCommand(&cli.Command{Name: "ping", Aliases: []string{"p"}, Description: "Ping a DHT node by ID or address", Run: CmdPing})
	router.RegisterCommand(&cli.Command{Name: "announce-peer", Aliases: []string{"announce_peer", "announce"}, Description: "Announce to DHT that we have a torrent", Run: CmdAnnouncePeer})
	router.RegisterCommand(&cli.Command{Name: "sample", Description: "Sample infohashes from DHT (BEP 51)", Run: CmdSample})
	router.RegisterCommand(&cli.Command{Name: "stats", Aliases: []string{"s"}, Description: "Show DHT routing table statistics", Run: CmdStats})
	router.RegisterCommand(&cli.Command{Name: "server", Aliases: []string{"serve"}, Description: "Start a persistent DHT node server", Run: CmdServer})

	// Route to the appropriate command handler
	return router.Route(args)
}

// CmdBootstrap handles the bootstrap command
func CmdBootstrap(args []string) error {
	f := cli.NewCommandFlagSet(
		"bootstrap", []string{"b"},
		"Bootstrap DHT and display routing table",
		[]string{"bt dht bootstrap"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}

	ctx := context.Background()

	opts := &bt.BootstrapOptions{
		DHTPort:          uint16(globalDHTConfig.DHTPort),
		BootstrapNodes:   bt.GetDefaultBootstrapNodes(),
		TargetNodes:      globalDHTConfig.DHTNodes,
		BootstrapTimeout: globalDHTConfig.DHTBootstrapTimeout,
		CacheDir:         globalDHTConfig.CacheDir,
		CacheFile:        globalDHTConfig.CacheFile,
	}

	return bt.Bootstrap(ctx, opts)
}

// CmdGetPeers handles the get-peers command
func CmdGetPeers(args []string) error {
	f := cli.NewCommandFlagSet(
		"get-peers", []string{"peers", "get_peers"},
		"Find peers for an infohash using DHT",
		[]string{"bt dht get-peers <infohash>"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required argument
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing infohash argument")
	}

	ctx := context.Background()
	hashHex := args[0]

	// Parse info hash
	infoHash, err := dht.KeyFromHex(hashHex)
	if err != nil {
		return fmt.Errorf("invalid info hash: %w", err)
	}

	opts := &bt.GetPeersOptions{
		DHTPort:          uint16(globalDHTConfig.DHTPort),
		BootstrapNodes:   bt.GetDefaultBootstrapNodes(),
		TargetNodes:      globalDHTConfig.DHTNodes,
		BootstrapTimeout: globalDHTConfig.DHTBootstrapTimeout,
		CacheDir:         globalDHTConfig.CacheDir,
		CacheFile:        globalDHTConfig.CacheFile,
	}

	return bt.GetPeers(ctx, infoHash, opts)
}

// CmdStats handles the stats command
func CmdStats(args []string) error {
	f := cli.NewCommandFlagSet(
		"stats", []string{"s"},
		"Show DHT routing table statistics",
		[]string{"bt dht stats"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}

	ctx := context.Background()

	opts := &bt.StatsOptions{
		DHTPort:          uint16(globalDHTConfig.DHTPort),
		BootstrapNodes:   bt.GetDefaultBootstrapNodes(),
		BootstrapTimeout: globalDHTConfig.DHTBootstrapTimeout,
		CacheDir:         globalDHTConfig.CacheDir,
		CacheFile:        globalDHTConfig.CacheFile,
	}

	return bt.Stats(ctx, opts)
}

// CmdSample handles the sample command
func CmdSample(args []string) error {
	f := cli.NewCommandFlagSet(
		"sample", nil,
		"Sample infohashes from DHT (BEP 51)",
		[]string{"bt dht sample"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}

	ctx := context.Background()

	opts := &bt.SampleOptions{
		DHTPort:          uint16(globalDHTConfig.DHTPort),
		BootstrapNodes:   bt.GetDefaultBootstrapNodes(),
		TargetNodes:      globalDHTConfig.DHTNodes,
		BootstrapTimeout: globalDHTConfig.DHTBootstrapTimeout,
		CacheDir:         globalDHTConfig.CacheDir,
		CacheFile:        globalDHTConfig.CacheFile,
	}

	return bt.Sample(ctx, opts)
}

// CmdFindNode handles the find-node command
func CmdFindNode(args []string) error {
	f := cli.NewCommandFlagSet(
		"find-node", []string{"find_node", "fn"},
		"Find a specific DHT node by its ID",
		[]string{"bt dht find-node <node-id>"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required argument
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing node ID argument")
	}

	ctx := context.Background()
	targetIDHex := args[0]

	// Parse node ID
	targetID, err := dht.KeyFromHex(targetIDHex)
	if err != nil {
		return fmt.Errorf("invalid node ID: %w", err)
	}

	opts := &bt.FindNodeOptions{
		DHTPort:          uint16(globalDHTConfig.DHTPort),
		BootstrapNodes:   bt.GetDefaultBootstrapNodes(),
		TargetNodes:      globalDHTConfig.DHTNodes,
		BootstrapTimeout: globalDHTConfig.DHTBootstrapTimeout,
		CacheDir:         globalDHTConfig.CacheDir,
		CacheFile:        globalDHTConfig.CacheFile,
	}

	return bt.FindNode(ctx, targetID, opts)
}

// CmdAnnouncePeer handles the announce-peer command
func CmdAnnouncePeer(args []string) error {
	f := cli.NewCommandFlagSet(
		"announce-peer", []string{"announce_peer", "announce"},
		"Announce that we're downloading a torrent on a specific port",
		[]string{"bt dht announce-peer [options] <infohash>"},
	)
	var impliedPort bool
	f.BoolVar(&impliedPort, "implied_port", false, "Use source UDP port instead of explicit port")
	port := f.Int("port", 0, "Port to announce (0 = use source port)")

	// Parse flags
	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required argument
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing infohash argument")
	}

	ctx := context.Background()
	infoHashHex := args[0]

	// Parse info hash
	infoHash, err := dht.KeyFromHex(infoHashHex)
	if err != nil {
		return fmt.Errorf("invalid info hash: %w", err)
	}

	// Determine port to announce
	announcePort := *port
	if !impliedPort && announcePort == 0 {
		return fmt.Errorf("must specify --port when not using --implied-port")
	}

	// If using implied port, port value will be ignored by DHT node
	// The DHT node will use the source UDP port automatically

	opts := &bt.AnnouncePeerOptions{
		DHTPort:          uint16(globalDHTConfig.DHTPort),
		BootstrapNodes:   bt.GetDefaultBootstrapNodes(),
		TargetNodes:      globalDHTConfig.DHTNodes,
		BootstrapTimeout: globalDHTConfig.DHTBootstrapTimeout,
		CacheDir:         globalDHTConfig.CacheDir,
		CacheFile:        globalDHTConfig.CacheFile,
	}

	return bt.AnnouncePeer(ctx, infoHash, announcePort, impliedPort, opts)
}

// CmdPing handles the ping command
func CmdPing(args []string) error {
	f := cli.NewCommandFlagSet(
		"ping", []string{"p"},
		"Ping a DHT node by ID or address",
		[]string{"bt dht ping <node-id>", "bt dht ping <ip>:<port>"},
	)
	timeout := f.Duration("timeout", 5*time.Second, "Ping timeout")

	// Parse flags
	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required argument
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing node ID or address argument")
	}

	ctx := context.Background()
	target := args[0]

	// Try to parse as IP:port address first
	addr, err := net.ResolveUDPAddr("udp", target)
	if err == nil {
		// It's an address - ping it directly
		return bt.PingAddress(ctx, addr, &bt.PingOptions{
			DHTPort: uint16(globalDHTConfig.DHTPort),
			Timeout: *timeout,
		})
	}

	// Try to parse as node ID
	nodeID, err := dht.KeyFromHex(target)
	if err != nil {
		return fmt.Errorf("invalid format - must be either <ip>:<port> or a 40-character hex node ID")
	}

	// Node ID lookup not fully implemented yet
	opts := &bt.PingOptions{
		DHTPort: uint16(globalDHTConfig.DHTPort),
		Timeout: *timeout,
	}

	return bt.Ping(ctx, nodeID, opts)
}

// CmdServer handles the server command - starts a DHT node and keeps it running
func CmdServer(args []string) error {
	f := cli.NewCommandFlagSet(
		"server", []string{"serve"},
		"Start a DHT node server",
		[]string{"bt dht server [--port <port>]"},
	)
	port := f.Uint("port", uint(globalDHTConfig.DHTPort), "DHT server port")

	// Parse flags
	err := f.Parse(args)
	if err != nil {
		return err
	}

	ctx := context.Background()
	cacheOps := config.NewCacheOperations()

	// Get or generate a stable node ID.
	var nodeID dht.Key
	if selfID, err := cacheOps.GetDHTSelfID(); err == nil && selfID != "" {
		nodeID, err = dht.KeyFromHex(selfID)
		if err != nil {
			log.Warn("invalid cached node ID, generating new", "sub", "dht")
			nodeID = dht.RandomKey()
		} else {
			log.Debug("loaded node ID from cache", "sub", "dht", "id", fmt.Sprintf("%x", nodeID[:8]))
		}
	} else {
		nodeID = dht.RandomKey()
		log.Debug("generated new node ID", "sub", "dht", "id", fmt.Sprintf("%x", nodeID[:8]))
		if err := cacheOps.SetDHTSelfID(nodeID.Hex()); err != nil {
			log.Warn("failed to save node ID", "sub", "dht", "err", err)
		}
	}

	// Create DHT node with the node ID
	bootstrapNodes := bt.GetDefaultBootstrapNodes()
	if len(bootstrapNodes) == 0 {
		log.Warn("no DHT bootstrap nodes configured", "sub", "dht")
	}

	node, err := dht.NewNodeWithID(uint16(*port), bootstrapNodes, nodeID)
	if err != nil {
		return fmt.Errorf("failed to create DHT node: %w", err)
	}
	defer node.Shutdown()

	log.Info("created DHT node", "sub", "dht", "id", fmt.Sprintf("%x", nodeID[:8]), "port", *port)
	fmt.Printf("DHT Server started\n")
	fmt.Printf("  Node ID: %x\n", nodeID)
	fmt.Printf("  Port: %d\n", *port)
	fmt.Printf("  Address: 127.0.0.1:%d\n", *port)

	// Add localhost entry to the unified cache.
	if err := cacheOps.AddOrUpdateDHTNode(nodeID.Hex(), "127.0.0.1", int(*port)); err != nil {
		log.Warn("failed to cache localhost DHT entry", "sub", "dht", "err", err)
	} else {
		log.Debug("added localhost entry to cache", "sub", "dht", "port", *port)
	}

	// Bootstrap the DHT
	log.Info("bootstrapping DHT", "sub", "dht")
	bootstrapCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = node.Bootstrap(bootstrapCtx, globalDHTConfig.DHTNodes)
	cancel()
	if err != nil && err != context.DeadlineExceeded {
		log.Warn("bootstrap error", "sub", "dht", "err", err)
	}

	routingTableSize := node.DHT().Length()
	log.Info("bootstrap complete", "sub", "dht", "nodes", routingTableSize)
	fmt.Printf("\nServer is running. Press Ctrl+C to stop.\n")
	fmt.Printf("You can ping this node with: ./bt --dht-port %d dht ping %x\n", (*port+1)%65535, nodeID)
	fmt.Printf("Or by address: ./bt --dht-port %d dht ping 127.0.0.1:%d\n\n", (*port+1)%65535, *port)

	// Start serving incoming requests
	if err := node.Serve(ctx); err != nil {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}
