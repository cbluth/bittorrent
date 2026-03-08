package dht

// BEP 5: DHT Protocol
// https://www.bittorrent.org/beps/bep_0005.html
//
// Routing table backed by a k-bucket Table (routing.go) instead of a flat
// sorted list.  Benefits:
//   - O(log n) closest-node lookup via bucket walks
//   - Replacement cache: noisy network cannot permanently evict good nodes
//   - BEP 5-compliant LRS eviction with ping-before-replace

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"
)

// DHT represents a Kademlia-based Distributed Hash Table (BEP 5).
type DHT struct {
	mu sync.RWMutex

	selfNode *Node
	table    *Table        // k-bucket routing table
	nodeMap  map[Key]*Node // nodeID → Node (for KRPC operations)
}

// Config holds DHT configuration options.
type Config struct {
	BootstrapTimeout time.Duration
	RequestTimeout   time.Duration
	MaxNodes         int
}

// DefaultConfig returns sensible default configuration.
func DefaultConfig() *Config {
	return &Config{
		BootstrapTimeout: 60 * time.Second,
		RequestTimeout:   2 * time.Second,
		MaxNodes:         200,
	}
}

// NewDHT creates a new DHT routing table backed by k-buckets.
func NewDHT(selfNode *Node) *DHT {
	d := &DHT{
		selfNode: selfNode,
		nodeMap:  make(map[Key]*Node),
	}
	d.table = NewTable(selfNode.id, func(rn *Node) {
		// pingNode callback: verify a questionable LRS node.
		d.mu.RLock()
		target, ok := d.nodeMap[rn.id]
		d.mu.RUnlock()
		if !ok {
			d.table.Remove(rn.id)
			return
		}
		_, _, err := d.selfNode.Ping(target)
		if err != nil {
			d.table.Remove(rn.id)
			d.mu.Lock()
			delete(d.nodeMap, rn.id)
			d.mu.Unlock()
		} else {
			d.table.Seen(rn.id)
		}
	})
	return d
}

// AddNode adds a node to the routing table.
func (d *DHT) AddNode(node *Node) {
	if node.id.Empty() {
		return
	}
	d.mu.Lock()
	d.nodeMap[node.id] = node
	d.mu.Unlock()
	d.table.Add(node)
}

// RemoveNode removes a node from the routing table.
func (d *DHT) RemoveNode(node *Node) {
	if node.id.Empty() {
		return
	}
	d.table.Remove(node.id)
	d.mu.Lock()
	delete(d.nodeMap, node.id)
	d.mu.Unlock()
}

// Length returns the number of nodes in the routing table.
func (d *DHT) Length() int {
	return d.table.Len()
}

// Seen notifies the routing table that a response arrived from the given node ID.
func (d *DHT) Seen(id Key) {
	d.table.Seen(id)
}

// GetGoodNodes returns all nodes currently in good status.
func (d *DHT) GetGoodNodes() []*Node {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]*Node, 0, len(d.nodeMap))
	for _, n := range d.nodeMap {
		if n.State() == NodeGood {
			result = append(result, n)
		}
	}
	return result
}

// GetClosestNodes returns up to k nodes closest to target, excluding bad nodes.
// BEP 5 §Routing Table: used during iterative lookup to find closest nodes by XOR distance.
func (d *DHT) GetClosestNodes(target Key, k int) []*Node {
	rns := d.table.Closest(target, k)
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]*Node, 0, len(rns))
	for _, rn := range rns {
		if n, ok := d.nodeMap[rn.id]; ok {
			result = append(result, n)
		}
	}
	return result
}

// Bootstrap expands the routing table, starting from bootstrap nodes if needed.
// BEP 5 §Bootstrap: a new node populates its routing table by querying known nodes
// with find_node for its own ID, then iteratively expanding with random targets.
func (d *DHT) Bootstrap(ctx context.Context, bootstrapNodes []*net.UDPAddr, targetNodes int, cfg *Config) error {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if targetNodes > cfg.MaxNodes {
		targetNodes = cfg.MaxNodes
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.BootstrapTimeout)
	defer cancel()

	if d.Length() > 0 {
		log.Info("bootstrap with cached nodes", "sub", "dht", "cached", d.Length(), "target", targetNodes)
		return d.expandRoutingTable(ctx, targetNodes, cfg, nil)
	}
	log.Info("no cached nodes, querying bootstrap servers", "sub", "dht", "servers", len(bootstrapNodes))
	return d.expandRoutingTable(ctx, targetNodes, cfg, bootstrapNodes)
}

// expandRoutingTable queries known nodes to grow the routing table.
func (d *DHT) expandRoutingTable(ctx context.Context, targetNodes int, cfg *Config, bootstrapNodes []*net.UDPAddr) error {
	visited := make(map[string]bool)
	var wg sync.WaitGroup

	if len(bootstrapNodes) > 0 {
		log.Debug("querying bootstrap nodes", "sub", "dht", "count", len(bootstrapNodes))
		results := make(chan *Message, len(bootstrapNodes)*2)
		var countMu sync.Mutex
		successCount, failureCount := 0, 0

		for _, addr := range bootstrapNodes {
			wg.Add(1)
			go func(addr *net.UDPAddr) {
				defer wg.Done()
				node := &Node{address: addr}
				_, resp, err := d.selfNode.FindNode(node, RandomKey())
				countMu.Lock()
				if err != nil {
					failureCount++
					countMu.Unlock()
					log.Debug("bootstrap query failed", "sub", "dht", "addr", addr, "err", err)
					return
				}
				successCount++
				countMu.Unlock()
				log.Debug("bootstrap query succeeded", "sub", "dht", "addr", addr)
				if resp != nil {
					select {
					case results <- resp:
					case <-ctx.Done():
					}
				}
			}(addr)
		}

		go func() {
			wg.Wait()
			close(results)
		}()

		totalAdded := 0
		for resp := range results {
			if resp != nil && resp.R != nil && resp.R.Nodes != nil {
				nodes := ParseCompactNodeInfoWithID(resp.R.Nodes)
				log.Debug("parsed nodes from bootstrap response", "sub", "dht", "count", len(nodes))
				for _, n := range nodes {
					d.AddNode(n)
					totalAdded++
				}
			}
		}
		log.Info("bootstrap phase 1 complete", "sub", "dht", "ok", successCount, "fail", failureCount, "added", totalAdded)
	}

	// Iterative expansion until we reach targetNodes.
	for d.Length() < targetNodes {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		nodesToQuery := d.GetClosestNodes(RandomKey(), 20)
		if len(nodesToQuery) == 0 {
			log.Debug("no more nodes to query", "sub", "dht", "nodes", d.Length())
			break
		}

		results := make(chan *Message, len(nodesToQuery)*2)
		queryCount := 0

		for _, node := range nodesToQuery {
			addr := node.address.String()
			if visited[addr] || d.Length() >= targetNodes {
				continue
			}
			visited[addr] = true
			wg.Add(1)
			queryCount++
			go func(node *Node) {
				defer wg.Done()
				select {
				case <-ctx.Done():
					return
				default:
				}
				_, resp, err := d.selfNode.FindNode(node, RandomKey())
				if err == nil && resp != nil {
					select {
					case results <- resp:
					case <-ctx.Done():
					}
				}
			}(node)
		}

		go func() {
			wg.Wait()
			close(results)
		}()

		newFound := 0
		for resp := range results {
			if resp != nil && resp.R != nil && resp.R.Nodes != nil {
				for _, n := range ParseCompactNodeInfoWithID(resp.R.Nodes) {
					d.AddNode(n)
					newFound++
				}
			}
		}

		if queryCount > 0 && newFound == 0 {
			log.Debug("no new nodes found, stopping", "sub", "dht", "nodes", d.Length())
			break
		}
	}

	log.Info("bootstrap complete", "sub", "dht", "nodes", d.Length())
	return nil
}

// EnsureMinNodes replenishes the routing table if it has fewer than minNodes.
func (d *DHT) EnsureMinNodes(ctx context.Context, minNodes int) error {
	if d.Length() >= minNodes {
		return nil
	}
	log.Info("node count below minimum, replenishing", "sub", "dht", "current", d.Length(), "minimum", minNodes)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cfg := &Config{
		BootstrapTimeout: 30 * time.Second,
		RequestTimeout:   2 * time.Second,
		MaxNodes:         200,
	}
	if d.Length() == 0 && d.selfNode != nil && len(d.selfNode.bootstrapNodes) > 0 {
		log.Info("routing table empty, using bootstrap nodes", "sub", "dht")
		return d.expandRoutingTable(ctx, minNodes, cfg, d.selfNode.bootstrapNodes)
	}
	return d.expandRoutingTable(ctx, minNodes, cfg, nil)
}

// MaintainRoutingTable pings questionable nodes and evicts bad ones.
// BEP 5 §Routing Table: nodes that haven't responded in 15 minutes are "questionable";
// nodes that fail to respond to multiple queries are "bad" and should be evicted.
func (d *DHT) MaintainRoutingTable(ctx context.Context) {
	d.mu.RLock()
	var questionable, bad []*Node
	for _, n := range d.nodeMap {
		switch n.EvaluateState() {
		case NodeQuestionable:
			questionable = append(questionable, n)
		case NodeBad:
			bad = append(bad, n)
		}
	}
	d.mu.RUnlock()

	log.Debug("maintenance scan", "sub", "dht", "questionable", len(questionable), "bad", len(bad))

	for _, n := range questionable {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, _, err := d.selfNode.Ping(n)
		if err != nil {
			d.RemoveNode(n)
			log.Debug("removed unresponsive node", "sub", "dht", "addr", n.Address())
		} else {
			d.table.Seen(n.id)
			log.Debug("questionable node responded, marked good", "sub", "dht", "addr", n.Address())
		}
	}

	for _, n := range bad {
		d.RemoveNode(n)
		log.Debug("removed bad node", "sub", "dht", "addr", n.Address())
	}
}

// Stats returns statistics about the DHT routing table.
func (d *DHT) Stats() map[string]any {
	stats := d.table.Stats()
	d.mu.RLock()
	ipv4, ipv6 := 0, 0
	for _, n := range d.nodeMap {
		addr := n.Address()
		if addr != nil && addr.IP != nil {
			if addr.IP.To4() != nil {
				ipv4++
			} else {
				ipv6++
			}
		}
	}
	d.mu.RUnlock()
	stats["ipv4_nodes"] = ipv4
	stats["ipv6_nodes"] = ipv6
	stats["self_id"] = d.selfNode.id.Hex()
	return stats
}

// Shutdown clears the routing table.
func (d *DHT) Shutdown() error {
	d.mu.Lock()
	d.nodeMap = make(map[Key]*Node)
	d.mu.Unlock()
	return nil
}
