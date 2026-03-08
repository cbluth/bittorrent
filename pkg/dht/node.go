package dht

// BEP 5: DHT Protocol - Node Operations
// https://www.bittorrent.org/beps/bep_0005.html
//
// This file implements DHT node operations:
// - Local and remote node management
// - UDP network operations for KRPC communication
// - Query/response handling (ping, find_node, get_peers)
// - Iterative peer lookup for info hashes
// - Bootstrap node discovery

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"
)

// NodeStatus classifies a node's liveness (BEP 5).
type NodeStatus int

const (
	NodeUnknown NodeStatus = iota
	// NodeGood: responded to one of our queries within the last 15 minutes
	// OR has ever responded and sent us a query within the last 15 minutes
	NodeGood

	// NodeQuestionable: no activity for 15 minutes
	NodeQuestionable

	// NodeBad: failed to respond to multiple queries in a row
	NodeBad
)

// String returns the string representation of NodeState
func (s NodeStatus) String() string {
	switch s {
	case NodeGood:
		return "good"
	case NodeQuestionable:
		return "questionable"
	case NodeBad:
		return "bad"
	default:
		return "unknown"
	}
}

// BEP 5 timeout constants
const (
	// NodeTimeout is the duration after which a node becomes questionable (15 minutes per BEP 5)
	NodeTimeout = 15 * time.Minute

	// NodeMaxFailedQueries is the number of consecutive failures before a node is marked bad
	NodeMaxFailedQueries = 3
)

// Node represents a DHT node (either local or remote)
type Node struct {
	mu sync.RWMutex

	// Node identity
	id      Key          // 20-byte node ID
	address *net.UDPAddr // Network address

	// Local node fields (only set for the local node)
	conn           *net.UDPConn // UDP socket (only for local node)
	port           uint16       // Listening port (only for local node)
	dht            *DHT         // DHT routing table (only for local node)
	bootstrapNodes []*net.UDPAddr
	readOnly       bool // BEP 43: read-only mode — don't respond to queries, set ro=1 in outgoing

	// Node state tracking (BEP 5)
	lastSeen       time.Time  // Last time we received a response from this node
	lastQuery      time.Time  // Last time this node sent us a query
	everResponded  bool       // Has this node ever responded to us?
	failedQueries  int        // Consecutive failed queries
	status         NodeStatus // Cached state (updated on state changes)
	lastStateCheck time.Time  // Last time we evaluated the state

	// Background maintenance
	maintenanceCtx    context.Context
	maintenanceCancel context.CancelFunc
	maintenanceWg     sync.WaitGroup
	//

	// ID       NodeID
	Addr *net.UDPAddr
	// LastSeen time.Time // last time we received any message from this node
	LastSent time.Time // last time we sent any message to this node

	// failCount tracks unanswered pings; node becomes "bad" after enough failures.
	failCount int
}

// NewRemoteNode creates a remote DHT node (without UDP socket)
// This is used when we learn about a node from DHT responses
func NewRemoteNode(addr *net.UDPAddr) *Node {
	return &Node{
		address:  addr,
		lastSeen: time.Now(),
	}
}

// NewNode creates a new local DHT node with a random ID.
// For BEP 42-compliant node IDs, use NewNodeWithID with GenerateSecureNodeID.
func NewNode(port uint16, bootstrapURLs []string) (*Node, error) {
	nodeID := RandomKey()
	return NewNodeWithID(port, bootstrapURLs, nodeID)
}

// NewNodeWithID creates a new local DHT node with a specific node ID
func NewNodeWithID(port uint16, bootstrapURLs []string, nodeID Key) (*Node, error) {
	// Parse bootstrap node URLs
	bootstrapNodes := []*net.UDPAddr{}
	for _, urlStr := range bootstrapURLs {
		// If no scheme is provided, prepend udp:// since DHT always uses UDP
		if !strings.Contains(urlStr, "://") {
			urlStr = "udp://" + urlStr
		}

		u, err := url.Parse(urlStr)
		if err != nil {
			log.Warn("failed to parse bootstrap URL", "sub", "dht", "url", urlStr, "err", err)
			continue
		}

		if u.Scheme != "udp" && u.Scheme != "" {
			log.Warn("skipping non-UDP bootstrap node", "sub", "dht", "url", urlStr, "scheme", u.Scheme)
			continue
		}

		// Use the host part for resolution
		host := u.Host
		if host == "" {
			// If Host is empty, the URL might be just "hostname:port"
			host = urlStr
		}

		addr, err := net.ResolveUDPAddr("udp", host)
		if err != nil {
			log.Warn("failed to resolve bootstrap node", "sub", "dht", "host", host, "err", err)
			continue
		}

		bootstrapNodes = append(bootstrapNodes, addr)
		log.Debug("added bootstrap node", "sub", "dht", "addr", addr)
	}

	if len(bootstrapNodes) == 0 {
		return nil, fmt.Errorf("no valid bootstrap nodes provided")
	}

	// Create UDP listener
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: int(port)})
	if err != nil {
		return nil, fmt.Errorf("failed to listen on UDP port %d: %w", port, err)
	}

	node := &Node{
		id:             nodeID,
		conn:           conn,
		port:           port,
		bootstrapNodes: bootstrapNodes,
		address:        conn.LocalAddr().(*net.UDPAddr),
		lastSeen:       time.Now(),
	}

	// Create DHT routing table
	node.dht = NewDHT(node)

	// Set up background maintenance context
	node.maintenanceCtx, node.maintenanceCancel = context.WithCancel(context.Background())

	// Start background node pool maintenance
	node.startNodePoolMaintenance()

	log.Info("created DHT node", "sub", "dht", "id", nodeID.Hex()[:16], "port", port)

	return node, nil
}

// ID returns the node's ID
func (n *Node) ID() Key {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.id
}

// Address returns the node's network address
func (n *Node) Address() *net.UDPAddr {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.address
}

// DHT returns the node's routing table (only for local node)
func (n *Node) DHT() *DHT {
	return n.dht
}

// SetReadOnly enables or disables BEP 43 read-only mode.
// In read-only mode the node does not respond to incoming queries and sets
// ro=1 in all outgoing queries so that responders will not add it to their
// routing tables.
func (n *Node) SetReadOnly(ro bool) {
	n.mu.Lock()
	n.readOnly = ro
	n.mu.Unlock()
}

// IsReadOnly reports whether the node is in BEP 43 read-only mode.
func (n *Node) IsReadOnly() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.readOnly
}

// Bootstrap performs DHT bootstrap
func (n *Node) Bootstrap(ctx context.Context, targetNodes int) error {
	if n.dht == nil {
		return fmt.Errorf("cannot bootstrap a remote node")
	}

	cfg := DefaultConfig()
	return n.dht.Bootstrap(ctx, n.bootstrapNodes, targetNodes, cfg)
}

// EnsureMinNodes ensures the routing table has at least minNodes
// If below threshold, automatically replenishes by querying existing nodes
func (n *Node) EnsureMinNodes(ctx context.Context, minNodes int) error {
	if n.dht == nil {
		return fmt.Errorf("cannot ensure nodes on a remote node")
	}

	return n.dht.EnsureMinNodes(ctx, minNodes)
}

// sendNodeQuery creates a query, stamps BEP 43 ro=1 if in read-only mode,
// and sends it via SendQuery.
func (n *Node) sendNodeQuery(method string, args Arguments, addr *net.UDPAddr, timeoutSeconds int) (*Message, *Message, error) {
	query := NewQuery(method, args)
	if n.IsReadOnly() {
		query.RO = 1
	}
	resp, err := SendQuery(n.conn, addr, query, timeoutSeconds)
	return query, resp, err
}

// Ping sends a ping query to this node
func (n *Node) Ping(target *Node) (TransactionID, *Message, error) {
	if n.conn == nil {
		return nil, nil, fmt.Errorf("cannot ping from a remote node")
	}

	// Set deadline for request
	deadline := time.Now().Add(DefaultConfig().RequestTimeout)
	n.conn.SetDeadline(deadline)

	query, resp, err := n.sendNodeQuery(MethodPing, Arguments{ID: n.id[:]}, target.address, 2)
	if err != nil {
		target.MarkFailedQuery()
		return nil, nil, err
	}

	if resp.R != nil && resp.R.ID != nil {
		target.mu.Lock()
		if target.id.Empty() {
			copy(target.id[:], resp.R.ID)
		}
		target.mu.Unlock()

		target.MarkResponse()

		if !target.id.Empty() && n.dht != nil {
			n.dht.AddNode(target)
		}
	}

	return query.T, resp, nil
}

// FindNode sends a find_node query to this node, requesting both IPv4 and IPv6 nodes (BEP 32)
func (n *Node) FindNode(target *Node, searchKey Key) (TransactionID, *Message, error) {
	if n.conn == nil {
		return nil, nil, fmt.Errorf("cannot query from a remote node")
	}

	// Set deadline for request
	deadline := time.Now().Add(DefaultConfig().RequestTimeout)
	n.conn.SetDeadline(deadline)

	// Request both IPv4 and IPv6 nodes (BEP 32) to build dual-stack routing table
	query, resp, err := n.sendNodeQuery(MethodFindNode, Arguments{
		ID:     n.id[:],
		Target: searchKey[:],
		Want:   []string{"n4", "n6"},
	}, target.address, 2)
	if err != nil {
		target.MarkFailedQuery()
		return nil, nil, err
	}

	if resp.R != nil && resp.R.ID != nil {
		target.mu.Lock()
		if target.id.Empty() {
			copy(target.id[:], resp.R.ID)
		}
		target.mu.Unlock()

		target.MarkResponse()

		if !target.id.Empty() && n.dht != nil {
			n.dht.AddNode(target)
		}
	}

	return query.T, resp, nil
}

// GetPeers sends a get_peers query to find peers for an info hash, requesting both IPv4 and IPv6 (BEP 32)
func (n *Node) GetPeers(target *Node, infoHash Key) (TransactionID, *Message, error) {
	if n.conn == nil {
		return nil, nil, fmt.Errorf("cannot query from a remote node")
	}

	// Set deadline for request
	deadline := time.Now().Add(DefaultConfig().RequestTimeout)
	n.conn.SetDeadline(deadline)

	// Request both IPv4 and IPv6 nodes and peers (BEP 32)
	query, resp, err := n.sendNodeQuery(MethodGetPeers, Arguments{
		ID:       n.id[:],
		InfoHash: infoHash[:],
		Want:     []string{"n4", "n6"},
	}, target.address, 2)
	if err != nil {
		target.MarkFailedQuery()
		return nil, nil, err
	}

	if resp.R != nil && resp.R.ID != nil {
		target.mu.Lock()
		if target.id.Empty() {
			copy(target.id[:], resp.R.ID)
		}
		target.mu.Unlock()

		target.MarkResponse()

		if !target.id.Empty() && n.dht != nil {
			n.dht.AddNode(target)
		}
	}

	return query.T, resp, nil
}

// AnnouncePeerWithImpliedPort announces a peer with optional implied_port parameter (BEP 5)
// If impliedPort is true, port argument is ignored and source UDP port is used instead.
// This is useful for NAT traversal and uTP support.
func (n *Node) AnnouncePeerWithImpliedPort(target *Node, infoHash Key, port int, token []byte, impliedPort bool) (TransactionID, *Message, error) {
	if n.conn == nil {
		return nil, nil, fmt.Errorf("cannot query from a remote node")
	}

	// Set deadline for request
	deadline := time.Now().Add(DefaultConfig().RequestTimeout)
	n.conn.SetDeadline(deadline)

	impliedPortInt := 0
	if impliedPort {
		impliedPortInt = 1
	}
	query, resp, err := n.sendNodeQuery(MethodAnnouncePeer, Arguments{
		ID:          n.id[:],
		InfoHash:    infoHash[:],
		Port:        port,
		Token:       token,
		ImpliedPort: impliedPortInt,
	}, target.address, 2)
	if err != nil {
		target.MarkFailedQuery()
		return nil, nil, err
	}

	if resp.R != nil && resp.R.ID != nil {
		target.mu.Lock()
		if target.id.Empty() {
			copy(target.id[:], resp.R.ID)
		}
		target.mu.Unlock()

		target.MarkResponse()

		if !target.id.Empty() && n.dht != nil {
			n.dht.AddNode(target)
		}
	}

	return query.T, resp, nil
}

// SampleInfohashes sends a sample_infohashes query to get a sample of infohashes from a node (BEP 51)
func (n *Node) SampleInfohashes(target *Node, targetKey Key) (TransactionID, *Message, error) {
	if n.conn == nil {
		return nil, nil, fmt.Errorf("cannot query from a remote node")
	}

	// Set deadline for request
	deadline := time.Now().Add(DefaultConfig().RequestTimeout)
	n.conn.SetDeadline(deadline)

	query, resp, err := n.sendNodeQuery(MethodSampleInfohashes, Arguments{
		ID:     n.id[:],
		Target: targetKey[:],
	}, target.address, 2)
	if err != nil {
		target.MarkFailedQuery()
		return nil, nil, err
	}

	if resp.R != nil && resp.R.ID != nil {
		target.mu.Lock()
		if target.id.Empty() {
			copy(target.id[:], resp.R.ID)
		}
		target.mu.Unlock()

		target.MarkResponse()

		if !target.id.Empty() && n.dht != nil {
			n.dht.AddNode(target)
		}
	}

	return query.T, resp, nil
}

// Stats prints node statistics including state information
func (n *Node) Stats() {
	n.mu.RLock()
	nodeID := n.id.Hex()
	addr := n.address.String()
	state := n.status
	lastSeen := n.lastSeen
	lastQuery := n.lastQuery
	failedQueries := n.failedQueries
	everResponded := n.everResponded
	n.mu.RUnlock()

	attrs := []any{
		"sub", "dht",
		"id", nodeID[:16],
		"addr", addr,
		"state", state.String(),
		"everResponded", everResponded,
		"failedQueries", failedQueries,
	}
	if !lastSeen.IsZero() {
		attrs = append(attrs, "lastSeen", time.Since(lastSeen))
	}
	if !lastQuery.IsZero() {
		attrs = append(attrs, "lastQuery", time.Since(lastQuery))
	}
	if n.dht != nil {
		stats := n.dht.Stats()
		for k, v := range stats {
			attrs = append(attrs, k, v)
		}
	}
	log.Info("node stats", attrs...)
}

// Serve handles incoming DHT requests and responses
// This should be run in a goroutine for server mode
func (n *Node) Serve(ctx context.Context) error {
	if n.conn == nil {
		return fmt.Errorf("cannot serve on remote node")
	}

	log.Info("DHT server listening", "sub", "dht", "port", n.port)

	buf := make([]byte, 2048)
	for {
		select {
		case <-ctx.Done():
			log.Info("DHT server shutting down", "sub", "dht")
			return ctx.Err()
		default:
			// Set read deadline to allow checking context periodically
			n.conn.SetReadDeadline(time.Now().Add(1 * time.Second))

			// Read incoming packet
			nBytes, remoteAddr, err := n.conn.ReadFromUDP(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Timeout is expected, continue to check context
					continue
				}
				log.Warn("error reading packet", "sub", "dht", "err", err)
				continue
			}

			// Decode the message
			var msg Message
			if err := msg.DecodeBencode(buf[:nBytes]); err != nil {
				log.Debug("failed to decode message", "sub", "dht", "addr", remoteAddr, "err", err)
				continue
			}

			msg.Origin = remoteAddr

			// Handle the message based on type
			go n.handleIncomingMessage(&msg, remoteAddr)
		}
	}
}

// handleIncomingMessage processes an incoming DHT message
func (n *Node) handleIncomingMessage(msg *Message, remoteAddr *net.UDPAddr) {
	switch msg.Y {
	case MessageTypeQuery:
		n.handleQuery(msg, remoteAddr)
	case MessageTypeResponse:
		// Responses are typically handled by the sender's context
		// For a server, we mostly care about queries
	case MessageTypeError:
		log.Debug("received error from peer", "sub", "dht", "addr", remoteAddr, "err", msg.E)
	}
}

// handleQuery processes incoming DHT queries
func (n *Node) handleQuery(msg *Message, remoteAddr *net.UDPAddr) {
	if msg.Q == "" {
		log.Debug("received query without method", "sub", "dht", "addr", remoteAddr)
		return
	}

	// BEP 43: in read-only mode, do not respond to any queries.
	if n.IsReadOnly() {
		log.Debug("ignoring query (read-only mode)", "sub", "dht", "method", msg.Q, "addr", remoteAddr)
		return
	}

	log.Debug("received query", "sub", "dht", "method", msg.Q, "addr", remoteAddr, "txID", fmt.Sprintf("%x", msg.T))

	switch msg.Q {
	case MethodPing:
		n.handlePingQuery(msg, remoteAddr)
	case MethodFindNode:
		n.handleFindNodeQuery(msg, remoteAddr)
	case MethodGetPeers:
		// For now, just log it - full implementation would require peer storage
		log.Debug("get_peers query not implemented, ignoring", "sub", "dht")
	case MethodAnnouncePeer:
		// For now, just log it - full implementation would require peer storage
		log.Debug("announce_peer query not implemented, ignoring", "sub", "dht")
	case MethodSampleInfohashes:
		n.handleSampleInfohashesQuery(msg, remoteAddr)
	default:
		log.Debug("unknown query method", "sub", "dht", "method", msg.Q)
	}
}

// sendResponse encodes and sends a KRPC response, adding the BEP 42 ip field.
func (n *Node) sendResponse(response *Message, remoteAddr *net.UDPAddr, label string) {
	// BEP 42: include requestor's compact IP+port in all responses
	response.IP = CompactIPPort(remoteAddr)

	data, err := response.EncodeBencode()
	if err != nil {
		log.Warn("failed to encode "+label+" response", "sub", "dht", "err", err)
		return
	}

	if _, err := n.conn.WriteToUDP(data, remoteAddr); err != nil {
		log.Warn("failed to send "+label+" response", "sub", "dht", "addr", remoteAddr, "err", err)
		return
	}

	log.Debug("sent "+label+" response", "sub", "dht", "addr", remoteAddr)
}

// handlePingQuery responds to ping queries
func (n *Node) handlePingQuery(msg *Message, remoteAddr *net.UDPAddr) {
	response := NewResponse(msg.T, Response{
		ID: n.id[:],
	})
	n.sendResponse(response, remoteAddr, "ping")
}

// handleFindNodeQuery responds to find_node queries
func (n *Node) handleFindNodeQuery(msg *Message, remoteAddr *net.UDPAddr) {
	if msg.A == nil || msg.A.Target == nil {
		log.Debug("find_node query missing target", "sub", "dht")
		return
	}

	// Get target from query
	var target Key
	copy(target[:], msg.A.Target)

	// Find closest nodes to the target
	closestNodes := n.dht.GetClosestNodes(target, 8)

	// Create compact node info
	var compactNodes []byte
	for _, node := range closestNodes {
		nodeID := node.ID()
		addr := node.Address()
		if addr == nil {
			continue
		}

		// Add node ID (20 bytes)
		compactNodes = append(compactNodes, nodeID[:]...)

		// Add IP (4 bytes for IPv4)
		ip := addr.IP.To4()
		if ip == nil {
			continue
		}
		compactNodes = append(compactNodes, ip...)

		// Add port (2 bytes, big endian)
		port := make([]byte, 2)
		binary.BigEndian.PutUint16(port, uint16(addr.Port))
		compactNodes = append(compactNodes, port...)
	}

	response := NewResponse(msg.T, Response{
		ID:    n.id[:],
		Nodes: compactNodes,
	})
	n.sendResponse(response, remoteAddr, "find_node")
}

// handleSampleInfohashesQuery responds to sample_infohashes queries (BEP 51)
func (n *Node) handleSampleInfohashesQuery(msg *Message, remoteAddr *net.UDPAddr) {
	if msg.A == nil {
		log.Debug("sample_infohashes query missing arguments", "sub", "dht")
		return
	}

	// For this implementation, we don't actually store infohashes, so return empty response
	// In a full implementation, this would return a subset of stored infohashes
	response := NewResponse(msg.T, Response{
		ID:       n.id[:],
		Samples:  []byte{}, // Empty samples - we don't store infohashes
		Num:      new(int), // Zero infohashes stored
		Interval: new(int), // No specific interval requirement
	})
	n.sendResponse(response, remoteAddr, "sample_infohashes")
}

// Shutdown closes the node's UDP connection and cleans up
func (n *Node) Shutdown() error {
	// Stop background maintenance
	if n.maintenanceCancel != nil {
		n.maintenanceCancel()
		n.maintenanceWg.Wait()
	}

	if n.conn != nil {
		if err := n.conn.Close(); err != nil {
			return err
		}
	}

	if n.dht != nil {
		return n.dht.Shutdown()
	}

	return nil
}

// IsLocal returns true if this is the local node (has a UDP connection)
func (n *Node) IsLocal() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.conn != nil
}

// LastSeen returns when we last heard from this node
func (n *Node) LastSeen() time.Time {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.lastSeen
}

// State returns the current state of this node
func (n *Node) State() NodeStatus {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.status
}

// EvaluateState evaluates and updates the node's current state based on BEP 5 rules
func (n *Node) EvaluateState() NodeStatus {
	n.mu.Lock()
	defer n.mu.Unlock()

	newState := n.evaluateStateUnlocked()
	n.status = newState
	n.lastStateCheck = time.Now()
	return newState
}

// evaluateStateUnlocked evaluates state without locking (caller must hold lock)
func (n *Node) evaluateStateUnlocked() NodeStatus {
	now := time.Now()

	// Bad: failed to respond multiple times in a row
	if n.failedQueries >= NodeMaxFailedQueries {
		return NodeBad
	}

	// Good: responded to one of our queries within the last 15 minutes
	if !n.lastSeen.IsZero() && now.Sub(n.lastSeen) < NodeTimeout {
		return NodeGood
	}

	// Good: has ever responded AND sent us a query within the last 15 minutes
	if n.everResponded && !n.lastQuery.IsZero() && now.Sub(n.lastQuery) < NodeTimeout {
		return NodeGood
	}

	// Questionable: no activity for 15 minutes
	return NodeQuestionable
}

// MarkResponse should be called when we receive a response from this node
func (n *Node) MarkResponse() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.lastSeen = time.Now()
	n.everResponded = true
	n.failedQueries = 0
	n.status = NodeGood
	n.lastStateCheck = time.Now()
}

// MarkQuery should be called when this node sends us a query
func (n *Node) MarkQuery() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.lastQuery = time.Now()
	// Re-evaluate state
	n.status = n.evaluateStateUnlocked()
	n.lastStateCheck = time.Now()
}

// MarkFailedQuery should be called when a query to this node fails
func (n *Node) MarkFailedQuery() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.failedQueries++
	n.status = n.evaluateStateUnlocked()
	n.lastStateCheck = time.Now()
}

// startNodePoolMaintenance starts a background goroutine that keeps the routing table
// topped up at 100 nodes by querying random targets
func (n *Node) startNodePoolMaintenance() {
	if n.dht == nil {
		return
	}

	n.maintenanceWg.Add(1)
	go func() {
		defer n.maintenanceWg.Done()

		// Check every 10 seconds
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-n.maintenanceCtx.Done():
				return
			case <-ticker.C:
				// Check if we need more nodes
				currentNodes := n.dht.Length()
				if currentNodes >= 100 {
					continue
				}

				// Expand routing table by querying random targets
				ctx, cancel := context.WithTimeout(n.maintenanceCtx, 5*time.Second)

				// Get some existing nodes to query
				randomTarget := RandomKey()
				nodes := n.dht.GetClosestNodes(randomTarget, 3)

				for _, node := range nodes {
					if n.dht.Length() >= 100 || ctx.Err() != nil {
						break
					}
					if _, _, err := n.FindNode(node, randomTarget); err == nil {
						time.Sleep(100 * time.Millisecond)
					}
				}

				cancel()
			}
		}
	}()
}

// FindPeersForInfoHash performs an iterative DHT lookup to find peers for an info hash (BEP 5)
// This implements the DHT get_peers algorithm:
// 1. Start with closest nodes from routing table
// 2. Query them with get_peers
// 3. If they return peers (values), collect them
// 4. If they return closer nodes, query those next
// 5. Continue until we find enough peers or exhaust all nodes
func (n *Node) FindPeersForInfoHash(infoHash Key, targetPeers int, maxRounds int) ([]*net.UDPAddr, error) {
	if n.dht == nil {
		return nil, fmt.Errorf("cannot perform lookup on remote node")
	}

	// Ensure we have enough nodes before expensive peer search
	// If below 100, automatically replenish from existing nodes
	if n.dht.Length() < 100 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := n.EnsureMinNodes(ctx, 100)
		cancel()
		if err != nil {
			log.Warn("failed to replenish nodes", "sub", "dht", "err", err)
		}
	}

	// Get initial closest nodes to the info hash
	candidates := n.dht.GetClosestNodes(infoHash, 8)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no nodes in routing table")
	}

	visited := make(map[string]bool)
	allPeers := make([]*net.UDPAddr, 0)
	round := 0
	maxCandidatesPerRound := 50 // Limit to avoid excessive queries

	log.Info("starting peer lookup", "sub", "dht", "infoHash", infoHash.Hex()[:16], "initialNodes", len(candidates))

	for len(candidates) > 0 && round < maxRounds && len(allPeers) < targetPeers {
		round++

		// Limit candidates per round
		if len(candidates) > maxCandidatesPerRound {
			candidates = candidates[:maxCandidatesPerRound]
		}

		log.Debug("peer lookup round", "sub", "dht", "round", round, "querying", len(candidates), "peers", len(allPeers), "target", targetPeers)

		newCandidates := make([]*Node, 0)
		queriedThisRound := 0

		for _, targetNode := range candidates {
			addr := targetNode.Address().String()
			if visited[addr] {
				continue
			}
			visited[addr] = true
			queriedThisRound++

			_, resp, err := n.GetPeers(targetNode, infoHash)
			if err != nil {
				continue
			}

			if resp.R == nil {
				continue
			}

			// Collect peers from response
			if len(resp.R.Values) > 0 {
				for _, peerData := range resp.R.Values {
					// Try IPv4 format first (6 bytes), then IPv6 (18 bytes)
					if len(peerData) == 6 {
						peers := ParseCompactPeerInfo(peerData)
						allPeers = append(allPeers, peers...)
					} else if len(peerData) == 18 {
						peers := ParseCompactPeerInfoIPv6(peerData)
						allPeers = append(allPeers, peers...)
					}
				}
			}

			// Collect closer nodes for next round (IPv4)
			if len(resp.R.Nodes) > 0 {
				closerAddrs := ParseCompactNodeInfo(resp.R.Nodes)
				for _, addr := range closerAddrs {
					if !visited[addr.String()] {
						newCandidates = append(newCandidates, NewRemoteNode(addr))
					}
				}
			}

			// Collect closer nodes for next round (IPv6 - BEP 32)
			if len(resp.R.Nodes6) > 0 {
				for _, n := range ParseCompactNodeInfoIPv6WithID(resp.R.Nodes6) {
					if !visited[n.Address().String()] {
						newCandidates = append(newCandidates, n)
					}
				}
			}
		}

		log.Debug("peer lookup round complete", "sub", "dht", "round", round, "queried", queriedThisRound, "peers", len(allPeers), "newCandidates", len(newCandidates))

		// If we have enough peers, stop
		if len(allPeers) >= targetPeers {
			log.Info("found enough peers", "sub", "dht", "found", len(allPeers), "target", targetPeers)
			break
		}

		// Move to next round
		candidates = newCandidates
	}

	// Deduplicate peers
	seen := make(map[string]bool)
	uniquePeers := make([]*net.UDPAddr, 0)
	for _, peer := range allPeers {
		key := peer.String()
		if !seen[key] {
			seen[key] = true
			uniquePeers = append(uniquePeers, peer)
		}
	}

	log.Info("peer lookup complete", "sub", "dht", "rounds", round, "uniquePeers", len(uniquePeers))
	return uniquePeers, nil
}
