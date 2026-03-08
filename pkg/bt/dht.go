package bt

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/cbluth/bittorrent/pkg/config"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
)

// Config holds runtime configuration for DHT commands
type DHTConfig struct {
	DHTPort             uint
	DHTNodes            int
	DHTBootstrapTimeout time.Duration
	CacheDir            string
	CacheFile           string
}

// BootstrapOptions contains options for DHT bootstrap
type BootstrapOptions struct {
	DHTPort          uint16
	BootstrapNodes   []string
	TargetNodes      int
	BootstrapTimeout time.Duration
	CacheDir         string
	CacheFile        string
}

// GetPeersOptions contains options for DHT get-peers
type GetPeersOptions struct {
	DHTPort          uint16
	BootstrapNodes   []string
	TargetNodes      int
	BootstrapTimeout time.Duration
	CacheDir         string
	CacheFile        string
}

// StatsOptions contains options for DHT stats
type StatsOptions struct {
	DHTPort          uint16
	BootstrapNodes   []string
	BootstrapTimeout time.Duration
	CacheDir         string
	CacheFile        string
}

// SampleOptions contains options for DHT sample
type SampleOptions struct {
	DHTPort          uint16
	BootstrapNodes   []string
	TargetNodes      int
	BootstrapTimeout time.Duration
	CacheDir         string
	CacheFile        string
}

// FindNodeOptions contains options for DHT find_node
type FindNodeOptions struct {
	DHTPort          uint16
	BootstrapNodes   []string
	TargetNodes      int
	BootstrapTimeout time.Duration
	CacheDir         string
	CacheFile        string
}

// PingOptions contains options for DHT ping
type PingOptions struct {
	DHTPort uint16
	Timeout time.Duration
}

// AnnouncePeerOptions contains options for DHT announce_peer
type AnnouncePeerOptions struct {
	DHTPort          uint16
	BootstrapNodes   []string
	TargetNodes      int
	BootstrapTimeout time.Duration
	CacheDir         string
	CacheFile        string
}

// Bootstrap bootstraps a DHT node and displays routing table
func Bootstrap(ctx context.Context, opts *BootstrapOptions) error {
	log.Info("creating dht node", "sub", "dht", "port", opts.DHTPort)

	if len(opts.BootstrapNodes) == 0 {
		log.Warn("no dht bootstrap nodes configured", "sub", "dht")
	}

	node, err := dht.NewNode(opts.DHTPort, opts.BootstrapNodes)
	if err != nil {
		return fmt.Errorf("failed to create DHT node: %w", err)
	}
	defer func() {
		cacheOps := config.NewCacheOperations()
		if err := SaveDHTNodesToCache(cacheOps, node); err != nil {
			log.Warn("failed to save dht nodes to cache", "sub", "dht", "err", err)
		}
		node.Shutdown()
	}()

	// Load cached nodes
	cachedNodes, err := LoadDHTNodesFromCache(config.NewCacheOperations(), node)
	if err != nil {
		log.Warn("failed to load cached dht nodes", "sub", "dht", "err", err)
		cachedNodes = 0
	}

	// Bootstrap DHT
	targetNodes := opts.TargetNodes
	if cachedNodes > 0 {
		log.Info("using cached dht nodes", "sub", "dht", "count", cachedNodes)
		targetNodes = targetNodes - cachedNodes
		if targetNodes < 20 {
			targetNodes = 20
		}
	}

	log.Info("bootstrapping dht", "sub", "dht", "target", targetNodes, "timeout", opts.BootstrapTimeout)

	bootstrapCtx, cancel := context.WithTimeout(ctx, opts.BootstrapTimeout)
	defer cancel()

	err = node.Bootstrap(bootstrapCtx, targetNodes)
	if err != nil && err != context.DeadlineExceeded {
		log.Warn("bootstrap error", "sub", "dht", "err", err)
	}

	routingTableSize := node.DHT().Length()
	log.Info("dht bootstrap complete", "sub", "dht", "nodes", routingTableSize)

	// Display statistics
	PrintDHTSummary(node)

	return nil
}

// GetPeers finds peers for an infohash using DHT
func GetPeers(ctx context.Context, infoHash dht.Key, opts *GetPeersOptions) error {
	log.Info("creating dht node", "sub", "dht", "port", opts.DHTPort)

	if len(opts.BootstrapNodes) == 0 {
		log.Warn("no dht bootstrap nodes configured", "sub", "dht")
	}

	node, err := dht.NewNode(opts.DHTPort, opts.BootstrapNodes)
	if err != nil {
		return fmt.Errorf("failed to create DHT node: %w", err)
	}
	defer func() {
		cacheOps := config.NewCacheOperations()
		if err := SaveDHTNodesToCache(cacheOps, node); err != nil {
			log.Warn("failed to save dht nodes to cache", "sub", "dht", "err", err)
		}
		node.Shutdown()
	}()

	// Load cached nodes and bootstrap
	cacheOps := config.NewCacheOperations()
	cachedNodes, err := LoadDHTNodesFromCache(cacheOps, node)
	if err != nil {
		log.Warn("failed to load cached dht nodes", "sub", "dht", "err", err)
		cachedNodes = 0
	}
	targetNodes := opts.TargetNodes
	if cachedNodes > 0 {
		log.Info("using cached dht nodes", "sub", "dht", "count", cachedNodes)
		targetNodes = targetNodes - cachedNodes
		if targetNodes < 20 {
			targetNodes = 20
		}
	}

	log.Info("bootstrapping dht", "sub", "dht")
	bootstrapCtx, cancel := context.WithTimeout(ctx, opts.BootstrapTimeout)
	err = node.Bootstrap(bootstrapCtx, targetNodes)
	cancel()
	if err != nil && err != context.DeadlineExceeded {
		log.Warn("bootstrap error", "sub", "dht", "err", err)
	}

	routingTableSize := node.DHT().Length()
	log.Info("dht bootstrap complete", "sub", "dht", "nodes", routingTableSize)

	if routingTableSize == 0 {
		return fmt.Errorf("failed to bootstrap DHT: no nodes in routing table")
	}

	// Query DHT for peers
	hashHex := fmt.Sprintf("%x", infoHash[:])
	log.Info("querying dht for peers", "sub", "dht", "infohash", hashHex)
	peers, err := node.FindPeersForInfoHash(infoHash, 20, 8)
	if err != nil {
		return fmt.Errorf("DHT query failed: %w", err)
	}

	// Cache the infohash from get_peers query
	if len(peers) > 0 {
		cacheOps := config.NewCacheOperations()
		if err := cacheOps.AddOrUpdateInfohash(infoHash.Hex(), "dht_get_peers", 0, 0); err != nil {
			log.Warn("failed to cache infohash", "sub", "dht", "infohash", infoHash.Hex(), "err", err)
		} else {
			log.Info("cached infohash from get_peers query", "sub", "dht", "infohash", infoHash.Hex())
		}

		// Cache participating DHT nodes that responded successfully (only "good" nodes)
		// Get nodes that were likely queried based on our routing table
		closestNodes := node.DHT().GetClosestNodes(infoHash, 20)
		cachedNodes := 0
		for _, dhtNode := range closestNodes {
			nodeID := dhtNode.ID()
			addr := dhtNode.Address()
			state := dhtNode.State().String()

			// Only cache "good" nodes
			if addr != nil && state == "good" {
				if err := cacheOps.AddOrUpdateDHTNode(nodeID.Hex(), addr.IP.String(), int(addr.Port)); err != nil {
					log.Warn("failed to cache dht node", "sub", "dht", "nodeID", nodeID.Hex()[:16], "err", err)
				} else {
					cachedNodes++
				}
			}
		}
		if cachedNodes > 0 {
			log.Info("cached good dht nodes from get_peers query", "sub", "dht", "count", cachedNodes)
		}
	}

	if len(peers) == 0 {
		fmt.Println("\n⚠ No peers found in DHT")
		return nil
	}

	// Count IPv4 vs IPv6 peers
	ipv4Peers := 0
	ipv6Peers := 0
	for _, peer := range peers {
		if peer.IP.To4() != nil {
			ipv4Peers++
		} else {
			ipv6Peers++
		}
	}

	// Display results
	fmt.Println("\n=== DHT Peers Found ===")
	fmt.Printf("Info Hash: %s\n", hashHex)
	fmt.Printf("Total peers: %d\n", len(peers))
	fmt.Printf("  IPv4 peers: %d\n", ipv4Peers)
	fmt.Printf("  IPv6 peers: %d\n", ipv6Peers)

	fmt.Println("\nPeers:")
	for i, peer := range peers {
		peerType := "IPv4"
		if peer.IP.To4() == nil {
			peerType = "IPv6"
		}
		fmt.Printf("  %3d. [%s] %s\n", i+1, peerType, peer.String())
	}

	return nil
}

// Stats displays DHT routing table statistics
func Stats(ctx context.Context, opts *StatsOptions) error {
	log.Info("creating dht node", "sub", "dht", "port", opts.DHTPort)

	if len(opts.BootstrapNodes) == 0 {
		log.Warn("no dht bootstrap nodes configured", "sub", "dht")
	}

	node, err := dht.NewNode(opts.DHTPort, opts.BootstrapNodes)
	if err != nil {
		return fmt.Errorf("failed to create DHT node: %w", err)
	}
	defer func() {
		cacheOps := config.NewCacheOperations()
		if err := SaveDHTNodesToCache(cacheOps, node); err != nil {
			log.Warn("failed to save dht nodes to cache", "sub", "dht", "err", err)
		}
		node.Shutdown()
	}()

	// Load cached nodes
	cacheOps := config.NewCacheOperations()
	cachedNodes, err := LoadDHTNodesFromCache(cacheOps, node)
	if err != nil {
		log.Warn("failed to load cached dht nodes", "sub", "dht", "err", err)
		cachedNodes = 0
	}
	if cachedNodes > 0 {
		log.Info("loaded nodes from cache", "sub", "dht", "count", cachedNodes)
	}

	// Quick bootstrap to refresh
	log.Info("refreshing dht nodes", "sub", "dht")
	bootstrapCtx, cancel := context.WithTimeout(ctx, opts.BootstrapTimeout)
	err = node.Bootstrap(bootstrapCtx, 50) // Small refresh
	cancel()
	if err != nil && err != context.DeadlineExceeded {
		log.Warn("bootstrap error", "sub", "dht", "err", err)
	}

	// Display statistics
	PrintDHTSummary(node)

	return nil
}

// Sample samples infohashes from DHT (BEP 51)
func Sample(ctx context.Context, opts *SampleOptions) error {
	log.Info("creating dht node", "sub", "dht", "port", opts.DHTPort)

	if len(opts.BootstrapNodes) == 0 {
		log.Warn("no dht bootstrap nodes configured", "sub", "dht")
	}

	node, err := dht.NewNode(opts.DHTPort, opts.BootstrapNodes)
	if err != nil {
		return fmt.Errorf("failed to create DHT node: %w", err)
	}
	defer func() {
		cacheOps := config.NewCacheOperations()
		if err := SaveDHTNodesToCache(cacheOps, node); err != nil {
			log.Warn("failed to save dht nodes to cache", "sub", "dht", "err", err)
		}
		node.Shutdown()
	}()

	// Load cached nodes and bootstrap
	cacheOps := config.NewCacheOperations()
	cachedNodes, err := LoadDHTNodesFromCache(cacheOps, node)
	if err != nil {
		log.Warn("failed to load cached dht nodes", "sub", "dht", "err", err)
		cachedNodes = 0
	}
	targetNodes := opts.TargetNodes
	if cachedNodes > 0 {
		log.Info("using cached dht nodes", "sub", "dht", "count", cachedNodes)
		targetNodes = targetNodes - cachedNodes
		if targetNodes < 20 {
			targetNodes = 20
		}
	}

	log.Info("bootstrapping dht", "sub", "dht", "target", targetNodes)
	bootstrapCtx, cancel := context.WithTimeout(ctx, opts.BootstrapTimeout)
	err = node.Bootstrap(bootstrapCtx, targetNodes)
	cancel()
	if err != nil && err != context.DeadlineExceeded {
		log.Warn("bootstrap error", "sub", "dht", "err", err)
	}

	routingTableSize := node.DHT().Length()
	log.Info("dht bootstrap complete", "sub", "dht", "nodes", routingTableSize)

	if routingTableSize == 0 {
		return fmt.Errorf("failed to bootstrap DHT: no nodes in routing table")
	}

	// Sample infohashes using BEP 51
	fmt.Println("\n=== Sampling Infohashes from DHT (BEP 51) ===")

	// Get nodes to query for sampling
	nodesToQuery := node.DHT().GetClosestNodes(dht.RandomKey(), 20) // Use random target for broad sampling
	if len(nodesToQuery) == 0 {
		return fmt.Errorf("no nodes available to query")
	}

	log.Info("querying nodes for infohash samples", "sub", "dht", "count", len(nodesToQuery))

	var allSamples [][]byte
	var totalInfohashes int
	var intervals []int
	var nodesWithSamples int

	// Query each node for samples
	for i, targetNode := range nodesToQuery {
		log.Debug("querying node for samples", "sub", "dht", "index", i+1, "total", len(nodesToQuery), "addr", targetNode.Address().String())

		// Use random target key for sampling (target doesn't affect returned samples)
		randomTarget := dht.RandomKey()
		_, resp, err := node.SampleInfohashes(targetNode, randomTarget)
		if err != nil {
			log.Debug("query failed", "sub", "dht", "err", err)
			continue
		}

		if resp.R == nil {
			log.Debug("no response data", "sub", "dht")
			continue
		}

		// Extract sample data
		if resp.R.Samples != nil {
			samples := resp.R.Samples
			if len(samples) > 0 {
				// Parse individual infohashes from samples
				infohashes, parseErr := dht.ParseSampledInfohashes(samples)
				if parseErr != nil {
					log.Debug("failed to parse samples", "sub", "dht", "err", parseErr)
					continue
				}

				allSamples = append(allSamples, samples)
				totalInfohashes += len(infohashes)
				nodesWithSamples++

				log.Debug("got infohash samples", "sub", "dht", "count", len(infohashes))
			}
		}

		// Extract interval if provided
		if resp.R.Interval != nil {
			intervals = append(intervals, *resp.R.Interval)
		}

		// Extract total count if provided
		if resp.R.Num != nil {
			log.Debug("node reports total infohashes", "sub", "dht", "count", *resp.R.Num)
		}

		// Brief pause between queries
		time.Sleep(100 * time.Millisecond)
	}

	// Display results
	fmt.Printf("\nSampling Results:\n")
	fmt.Printf("  Nodes queried: %d\n", len(nodesToQuery))
	fmt.Printf("  Nodes responding: %d\n", nodesWithSamples)
	fmt.Printf("  Total infohash samples collected: %d\n", totalInfohashes)

	if len(allSamples) > 0 {
		fmt.Println("\n=== Sampled Infohashes ===")

		// Save infohashes to cache
		cacheOps := config.NewCacheOperations()
		var savedCount int

		for i, samples := range allSamples {
			infohashes, _ := dht.ParseSampledInfohashes(samples)
			fmt.Printf("  Sample batch %d: %d infohashes\n", i+1, len(infohashes))
			for j, infohash := range infohashes {
				fmt.Printf("    %d. %s\n", j+1, infohash.Hex())

				// Save to cache
				if err := cacheOps.AddOrUpdateInfohash(infohash.Hex(), "dht_sample", 0, 0); err != nil {
					log.Warn("failed to cache infohash", "sub", "dht", "infohash", infohash.Hex(), "err", err)
				} else {
					savedCount++
				}
			}
		}

		if savedCount > 0 {
			log.Info("saved infohashes to cache", "sub", "dht", "count", savedCount)
		}

		// Cache participating DHT nodes that responded successfully (only "good" nodes)
		cachedNodes := 0
		for _, targetNode := range nodesToQuery {
			nodeID := targetNode.ID()
			addr := targetNode.Address()
			state := targetNode.State().String()

			// Only cache "good" nodes
			if addr != nil && state == "good" {
				if err := cacheOps.AddOrUpdateDHTNode(nodeID.Hex(), addr.IP.String(), int(addr.Port)); err != nil {
					log.Warn("failed to cache dht node", "sub", "dht", "nodeID", nodeID.Hex()[:16], "err", err)
				} else {
					cachedNodes++
				}
			}
		}
		if cachedNodes > 0 {
			log.Info("cached good dht nodes from sample query", "sub", "dht", "count", cachedNodes)
		}
	}

	if len(intervals) > 0 {
		// Calculate average interval
		sum := 0
		for _, interval := range intervals {
			sum += interval
		}
		avgInterval := sum / len(intervals)
		fmt.Printf("\nInterval Information:\n")
		fmt.Printf("  Average refresh interval: %d seconds (%.1f minutes)\n", avgInterval, float64(avgInterval)/60.0)
	}

	// Display current DHT stats
	PrintDHTSummary(node)

	return nil
}

// GetDefaultBootstrapNodes returns default bootstrap nodes from config
func GetDefaultBootstrapNodes() []string {
	return config.DHTBootstrapNodes()
}

// FindNode finds a specific DHT node by its ID using iterative find_node queries
func FindNode(ctx context.Context, targetID dht.Key, opts *FindNodeOptions) error {
	log.Info("searching for node", "sub", "dht", "targetID", targetID)

	// Get or generate node ID from main config
	configOps := config.NewOperations()
	var nodeID dht.Key
	cachedNodeID, err := configOps.GetNodeID()
	if err == nil && cachedNodeID != "" {
		// Load from config
		nodeID, err = dht.KeyFromHex(cachedNodeID)
		if err != nil {
			log.Warn("invalid cached node id, generating new one", "sub", "dht")
			nodeID = dht.RandomKey()
		} else {
			log.Debug("loaded node id from config", "sub", "dht", "nodeID", nodeID[:8])
		}
	} else {
		// Generate new one
		nodeID = dht.RandomKey()
		log.Debug("generated new node id", "sub", "dht", "nodeID", nodeID[:8])

		// Save to config
		if err := configOps.SetNodeID(nodeID.Hex()); err != nil {
			log.Warn("failed to save node id to config", "sub", "dht", "err", err)
		}
	}

	// Create DHT node with node ID
	if len(opts.BootstrapNodes) == 0 {
		log.Warn("no dht bootstrap nodes configured", "sub", "dht")
	}

	node, err := dht.NewNodeWithID(opts.DHTPort, opts.BootstrapNodes, nodeID)
	if err != nil {
		return fmt.Errorf("failed to create DHT node: %w", err)
	}
	defer func() {
		cacheOps := config.NewCacheOperations()
		if err := SaveDHTNodesToCache(cacheOps, node); err != nil {
			log.Warn("failed to save dht nodes to cache", "sub", "dht", "err", err)
		}
		node.Shutdown()
	}()

	log.Info("created dht node", "sub", "dht", "port", opts.DHTPort)

	// Check if we have node in cache first
	var cachedAddr *net.UDPAddr
	cacheOps := config.NewCacheOperations()
	cachedNodes, err := cacheOps.GetDHTNodes()
	if err == nil {
		targetIDHex := targetID.Hex()
		for _, n := range cachedNodes {
			if n.ID == targetIDHex {
				addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", n.IP, n.Port))
				if err == nil {
					cachedAddr = addr
					log.Info("found target node in cache", "sub", "dht", "addr", addr.String())
					break
				}
			}
		}
	}

	// If found in cache, try pinging it directly first
	if cachedAddr != nil {
		log.Debug("trying cached address", "sub", "dht")
		remoteNode := dht.NewRemoteNode(cachedAddr)
		_, resp, err := node.FindNode(remoteNode, targetID)
		if err == nil && resp.R != nil && resp.R.ID != nil {
			// Verify it's actually the right node
			respondedID := dht.Key{}
			copy(respondedID[:], resp.R.ID)
			if respondedID == targetID {
				fmt.Println("✓ Found node! (from cache)")
				fmt.Printf("  Node ID: %x\n", targetID)
				fmt.Printf("  Address: %s\n", cachedAddr.String())
				fmt.Printf("  Confirmed ID: %x\n", resp.R.ID)

				// Node found via cached address - cache will be updated in defer
				return nil
			}
			log.Warn("cached address responded with wrong id, searching dht", "sub", "dht")
		} else {
			log.Info("cached address unreachable, searching dht", "sub", "dht")
		}
	}

	// Load cached nodes and bootstrap
	lcachedNodes, err := LoadDHTNodesFromCache(cacheOps, node)
	if err != nil {
		log.Warn("failed to load cached dht nodes", "sub", "dht", "err", err)
		lcachedNodes = 0
	}
	targetNodes := opts.TargetNodes
	if lcachedNodes > 0 {
		log.Info("using cached dht nodes", "sub", "dht", "count", lcachedNodes)
		targetNodes = targetNodes - lcachedNodes
		if targetNodes < 20 {
			targetNodes = 20
		}
	}

	log.Info("bootstrapping dht", "sub", "dht")
	bootstrapCtx, cancel := context.WithTimeout(ctx, opts.BootstrapTimeout)
	err = node.Bootstrap(bootstrapCtx, targetNodes)
	cancel()
	if err != nil && err != context.DeadlineExceeded {
		log.Warn("bootstrap error", "sub", "dht", "err", err)
	}

	routingTableSize := node.DHT().Length()
	log.Info("bootstrap complete", "sub", "dht", "nodes", routingTableSize)

	if routingTableSize == 0 {
		return fmt.Errorf("failed to bootstrap DHT: no nodes in routing table")
	}

	// Find target node using iterative find_node queries
	log.Info("searching for target node in dht", "sub", "dht")

	// Get closest nodes to target from our routing table
	closestNodes := node.DHT().GetClosestNodes(targetID, 8)
	if len(closestNodes) == 0 {
		return fmt.Errorf("no nodes available to query")
	}

	// Try to find a node with matching ID in our routing table
	var targetNode *dht.Node
	for _, n := range closestNodes {
		nodeID := n.ID()
		if nodeID == targetID {
			log.Info("found exact match in routing table", "sub", "dht", "addr", n.Address().String())
			targetNode = n
			break
		}
	}

	// If exact match not found, perform iterative find_node queries
	if targetNode == nil {
		log.Debug("exact match not in routing table, performing find_node queries", "sub", "dht")

		// Query closest nodes for target
		for i, n := range closestNodes {
			nid := n.ID()
			log.Debug("querying node", "sub", "dht", "index", i+1, "total", len(closestNodes), "addr", n.Address().String(), "nodeID", nid[:8])
			_, resp, err := node.FindNode(n, targetID)
			if err != nil {
				log.Debug("query failed", "sub", "dht", "err", err)
				continue
			}

			// Check if we got the target node in the response
			if resp.R != nil && resp.R.Nodes != nil {
				nodes := dht.ParseCompactNodeInfoWithID(resp.R.Nodes)
				log.Debug("response contained nodes", "sub", "dht", "count", len(nodes))
				for _, foundNode := range nodes {
					foundID := foundNode.ID()
					if foundID == targetID {
						log.Info("found target node", "sub", "dht", "addr", foundNode.Address().String())
						targetNode = foundNode
						break
					}
				}
			}

			// Also check IPv6 nodes if available (IDs included via BEP 32 compact format)
			if resp.R != nil && resp.R.Nodes6 != nil && targetNode == nil {
				nodes6 := dht.ParseCompactNodeInfoIPv6WithID(resp.R.Nodes6)
				log.Debug("response contained ipv6 nodes", "sub", "dht", "count", len(nodes6))
				for _, n := range nodes6 {
					if n.ID() == targetID {
						log.Info("found target ipv6 node", "sub", "dht", "addr", n.Address().String())
						fmt.Println("✓ Found IPv6 node!")
						fmt.Printf("  Node ID: %x\n", targetID)
						fmt.Printf("  Address: %s\n", n.Address().String())
						return nil
					}
				}
			}

			if targetNode != nil {
				break
			}
		}
	}

	if targetNode == nil {
		return fmt.Errorf("node not found in DHT - target may be offline or not exist")
	}

	log.Info("found node", "sub", "dht", "targetID", targetID, "addr", targetNode.Address().String())

	// Success!
	fmt.Println("✓ Found node!")
	fmt.Printf("  Node ID: %x\n", targetID)
	fmt.Printf("  Address: %s\n", targetNode.Address().String())

	// Cache will be saved in defer
	return nil
}

// AnnouncePeer announces that our peer is downloading a torrent on a specific port (BEP 5)
func AnnouncePeer(ctx context.Context, infoHash dht.Key, port int, impliedPort bool, opts *AnnouncePeerOptions) error {
	log.Info("announcing peer for infohash", "sub", "dht", "infohash", infoHash)

	// Get or generate node ID from main config
	configOps := config.NewOperations()
	var nodeID dht.Key
	cachedNodeID, err := configOps.GetNodeID()
	if err == nil && cachedNodeID != "" {
		// Load from config
		nodeID, err = dht.KeyFromHex(cachedNodeID)
		if err != nil {
			log.Warn("invalid cached node id, generating new one", "sub", "dht")
			nodeID = dht.RandomKey()
		} else {
			log.Debug("loaded node id from config", "sub", "dht", "nodeID", nodeID[:8])
		}
	} else {
		// Generate new one
		nodeID = dht.RandomKey()
		log.Debug("generated new node id", "sub", "dht", "nodeID", nodeID[:8])

		// Save to config
		if err := configOps.SetNodeID(nodeID.Hex()); err != nil {
			log.Warn("failed to save node id to config", "sub", "dht", "err", err)
		}
	}

	// Create DHT node with node ID
	if len(opts.BootstrapNodes) == 0 {
		log.Warn("no dht bootstrap nodes configured", "sub", "dht")
	}

	node, err := dht.NewNodeWithID(opts.DHTPort, opts.BootstrapNodes, nodeID)
	if err != nil {
		return fmt.Errorf("failed to create DHT node: %w", err)
	}
	defer func() {
		cacheOps := config.NewCacheOperations()
		if err := SaveDHTNodesToCache(cacheOps, node); err != nil {
			log.Warn("failed to save dht nodes to cache", "sub", "dht", "err", err)
		}
		node.Shutdown()
	}()

	log.Info("created dht node", "sub", "dht", "port", opts.DHTPort)

	// Load cached nodes and bootstrap
	cacheOps := config.NewCacheOperations()
	cachedNodes, err := LoadDHTNodesFromCache(cacheOps, node)
	if err != nil {
		log.Warn("failed to load cached dht nodes", "sub", "dht", "err", err)
		cachedNodes = 0
	}
	targetNodes := opts.TargetNodes
	if cachedNodes > 0 {
		log.Info("using cached dht nodes", "sub", "dht", "count", cachedNodes)
		targetNodes = targetNodes - cachedNodes
		if targetNodes < 20 {
			targetNodes = 20
		}
	}

	log.Info("bootstrapping dht", "sub", "dht")
	bootstrapCtx, cancel := context.WithTimeout(ctx, opts.BootstrapTimeout)
	err = node.Bootstrap(bootstrapCtx, targetNodes)
	cancel()
	if err != nil && err != context.DeadlineExceeded {
		log.Warn("bootstrap error", "sub", "dht", "err", err)
	}

	routingTableSize := node.DHT().Length()
	log.Info("bootstrap complete", "sub", "dht", "nodes", routingTableSize)

	if routingTableSize == 0 {
		return fmt.Errorf("failed to bootstrap DHT: no nodes in routing table")
	}

	// First, we need to get a token by performing get_peers queries
	log.Info("getting announce token from dht nodes", "sub", "dht")

	// Find nodes close to the infohash
	closestNodes := node.DHT().GetClosestNodes(infoHash, 8)
	if len(closestNodes) == 0 {
		return fmt.Errorf("no nodes available to query for token")
	}

	var token []byte
	var tokenNode *dht.Node

	// Query closest nodes to get a token
	for i, targetNode := range closestNodes {
		if i >= 3 { // Try up to 3 nodes for token
			break
		}

		log.Debug("getting token from node", "sub", "dht", "index", i+1, "addr", targetNode.Address().String())
		_, resp, err := node.GetPeers(targetNode, infoHash)
		if err != nil {
			log.Debug("failed to get token", "sub", "dht", "err", err)
			continue
		}

		if resp.R != nil && resp.R.Token != nil {
			token = resp.R.Token
			tokenNode = targetNode
			log.Debug("got token", "sub", "dht", "addr", targetNode.Address().String())
			break
		}
	}

	if token == nil {
		return fmt.Errorf("failed to get announce token from any node")
	}

	// Now announce with the token
	log.Info("announcing to node with token", "sub", "dht", "addr", tokenNode.Address().String())

	_, resp, err := node.AnnouncePeerWithImpliedPort(tokenNode, infoHash, port, token, impliedPort)
	if err != nil {
		return fmt.Errorf("announce failed: %w", err)
	}

	if resp.R != nil {
		fmt.Println("✓ Announce successful!")
		fmt.Printf("  Info Hash: %x\n", infoHash)
		fmt.Printf("  Port: %d\n", port)
		if impliedPort {
			fmt.Printf("  Using implied port (source UDP port)\n")
		} else {
			fmt.Printf("  Explicit port: %d\n", port)
		}
		fmt.Printf("  Announced to: %s\n", tokenNode.Address().String())
		fmt.Printf("  Response ID: %x\n", resp.R.ID)
	} else {
		return fmt.Errorf("announce failed: no response received")
	}

	return nil
}

// Ping pings a DHT node by its ID
func Ping(ctx context.Context, targetID dht.Key, opts *PingOptions) error {
	log.Info("searching for node", "sub", "dht", "targetID", targetID)

	// Get or generate node ID from main config
	configOps := config.NewOperations()
	var nodeID dht.Key
	cachedNodeID, err := configOps.GetNodeID()
	if err == nil && cachedNodeID != "" {
		// Load from config
		nodeID, err = dht.KeyFromHex(cachedNodeID)
		if err != nil {
			log.Warn("invalid cached node id, generating new one", "sub", "dht")
			nodeID = dht.RandomKey()
		} else {
			log.Debug("loaded node id from config", "sub", "dht", "nodeID", nodeID[:8])
		}
	} else {
		// Generate new one
		nodeID = dht.RandomKey()
		log.Debug("generated new node id", "sub", "dht", "nodeID", nodeID[:8])

		// Save to config
		if err := configOps.SetNodeID(nodeID.Hex()); err != nil {
			log.Warn("failed to save node id to config", "sub", "dht", "err", err)
		}
	}

	// Create DHT node with the node ID
	bootstrapNodes := GetDefaultBootstrapNodes()
	if len(bootstrapNodes) == 0 {
		log.Warn("no dht bootstrap nodes configured", "sub", "dht")
	}

	node, err := dht.NewNodeWithID(opts.DHTPort, bootstrapNodes, nodeID)
	if err != nil {
		return fmt.Errorf("failed to create DHT node: %w", err)
	}
	defer node.Shutdown()

	log.Info("created dht node", "sub", "dht", "port", opts.DHTPort)
	// Check if we have the node in cache first
	var cachedAddr *net.UDPAddr
	cacheOps := config.NewCacheOperations()
	cachedNodes, err := cacheOps.GetDHTNodes()
	if err == nil && len(cachedNodes) > 0 {
		targetIDHex := targetID.Hex()
		for _, n := range cachedNodes {
			if n.ID == targetIDHex {
				addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", n.IP, n.Port))
				if err == nil {
					cachedAddr = addr
					fmt.Fprintf(os.Stderr, "found node in cache: %s\n", addr.String())
					break
				}
			}
		}
	}

	// If found in cache, try pinging it directly first
	if cachedAddr != nil {
		fmt.Fprintf(os.Stderr, "pinging cached address %s...\n", cachedAddr.String())
		remoteNode := dht.NewRemoteNode(cachedAddr)
		_, resp, err := node.Ping(remoteNode)
		if err == nil && resp.R != nil && resp.R.ID != nil {
			// Verify it's actually the right node
			respondedID := dht.Key{}
			copy(respondedID[:], resp.R.ID)
			if respondedID == targetID {
				fmt.Println("✓ Ping successful! (from cache)")
				fmt.Printf("  Node ID: %x\n", targetID)
				fmt.Printf("  Address: %s\n", cachedAddr.String())
				fmt.Printf("  Confirmed ID: %x\n", resp.R.ID)

				// Node found via cached address
				return nil
			}
			fmt.Fprintf(os.Stderr, "cached address responded with different node ID, searching DHT...\n")
		} else {
			fmt.Fprintf(os.Stderr, "cached address unreachable, searching DHT...\n")
		}
	}

	// Bootstrap to get some nodes
	log.Info("bootstrapping dht", "sub", "dht")
	bootstrapCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = node.Bootstrap(bootstrapCtx, 50)
	cancel()
	if err != nil && err != context.DeadlineExceeded {
		log.Warn("bootstrap error", "sub", "dht", "err", err)
	}

	routingTableSize := node.DHT().Length()
	log.Info("bootstrap complete", "sub", "dht", "nodes", routingTableSize)

	if routingTableSize == 0 {
		return fmt.Errorf("failed to bootstrap DHT: no nodes in routing table")
	}

	// Find the target node using iterative find_node queries
	log.Info("searching for target node in dht", "sub", "dht")

	// Get the closest nodes to the target from our routing table
	closestNodes := node.DHT().GetClosestNodes(targetID, 8)
	if len(closestNodes) == 0 {
		return fmt.Errorf("no nodes available to query")
	}

	// Try to find a node with matching ID in our routing table
	var targetNode *dht.Node
	for _, n := range closestNodes {
		nodeID := n.ID()
		if nodeID == targetID {
			log.Info("found exact match in routing table", "sub", "dht", "addr", n.Address().String())
			targetNode = n
			break
		}
	}

	// If exact match not found, perform iterative find_node queries
	if targetNode == nil {
		log.Debug("exact match not in routing table, performing find_node queries", "sub", "dht")

		// Query the closest nodes for the target
		for i, n := range closestNodes {
			nid := n.ID()
			log.Debug("querying node", "sub", "dht", "index", i+1, "total", len(closestNodes), "addr", n.Address().String(), "nodeID", nid[:8])
			_, resp, err := node.FindNode(n, targetID)
			if err != nil {
				log.Debug("query failed", "sub", "dht", "err", err)
				continue
			}

			// Check if we got the target node in the response
			if resp.R != nil && resp.R.Nodes != nil {
				nodes := dht.ParseCompactNodeInfoWithID(resp.R.Nodes)
				log.Debug("response contained nodes", "sub", "dht", "count", len(nodes))
				for _, foundNode := range nodes {
					foundID := foundNode.ID()
					if foundID == targetID {
						log.Info("found target node", "sub", "dht", "addr", foundNode.Address().String())
						targetNode = foundNode
						break
					}
				}
			}

			if targetNode != nil {
				break
			}
		}
	}

	if targetNode == nil {
		return fmt.Errorf("node not found in DHT - target may be offline or not exist")
	}

	log.Info("found node", "sub", "dht", "targetID", targetID, "addr", targetNode.Address().String())

	// Ping the found node
	log.Info("pinging node", "sub", "dht", "addr", targetNode.Address().String())
	_, resp, err := node.Ping(targetNode)
	if err != nil {
		fmt.Printf("✗ Ping failed: %v\n", err)
		return err
	}

	// Success!
	fmt.Println("✓ Ping successful!")
	fmt.Printf("  Node ID: %x\n", targetID)
	fmt.Printf("  Address: %s\n", targetNode.Address().String())
	if resp.R != nil && resp.R.ID != nil {
		fmt.Printf("  Confirmed ID: %x\n", resp.R.ID)
	}

	// Save updated routing table to cache
	if err := SaveDHTNodesToCache(cacheOps, node); err != nil {
		log.Warn("failed to save dht nodes to cache", "sub", "dht", "err", err)
	} else {
		log.Info("updated routing table cache", "sub", "dht")
	}

	return nil
}

// PingAddress pings a specific DHT node address
func PingAddress(ctx context.Context, addr *net.UDPAddr, opts *PingOptions) error {
	cacheOps := config.NewCacheOperations()

	// Get or generate a stable node ID.
	var selfID dht.Key
	if storedID, err := cacheOps.GetDHTSelfID(); err == nil && storedID != "" {
		selfID, err = dht.KeyFromHex(storedID)
		if err != nil {
			log.Warn("invalid cached node id, generating new one", "sub", "dht")
			selfID = dht.RandomKey()
		} else {
			log.Debug("loaded node id from cache", "sub", "dht", "nodeID", selfID[:8])
		}
	} else {
		selfID = dht.RandomKey()
		log.Debug("generated new node id", "sub", "dht", "nodeID", selfID[:8])
		if err := cacheOps.SetDHTSelfID(selfID.Hex()); err != nil {
			log.Warn("failed to save node id", "sub", "dht", "err", err)
		}
	}

	// Create UDP connection
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: int(opts.DHTPort)})
	if err != nil {
		return fmt.Errorf("failed to create UDP connection: %w", err)
	}
	defer conn.Close()

	log.Info("pinging dht node", "sub", "dht", "addr", addr.String(), "timeout", opts.Timeout)

	// Send ping
	timeoutSecs := int(opts.Timeout.Seconds())
	if timeoutSecs < 1 {
		timeoutSecs = 1
	}

	txID, resp, err := dht.PingNode(conn, addr, selfID, timeoutSecs)
	if err != nil {
		fmt.Printf("✗ Ping failed: %v\n", err)
		return err
	}

	// Display response
	fmt.Println("✓ Ping successful!")
	fmt.Printf("  Transaction ID: %x\n", txID)
	if resp.R != nil && resp.R.ID != nil {
		fmt.Printf("  Remote node ID: %x\n", resp.R.ID)
	}

	return nil
}
