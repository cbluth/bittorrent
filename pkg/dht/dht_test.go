package dht

import (
	"context"
	"testing"
	"time"
)

func TestKeyGeneration(t *testing.T) {
	k1 := RandomKey()
	k2 := RandomKey()

	if k1.Empty() {
		t.Error("RandomKey generated empty key")
	}

	if k1 == k2 {
		t.Error("Two random keys should not be equal")
	}
}

func TestKeyDistance(t *testing.T) {
	k1, _ := KeyFromHex("0000000000000000000000000000000000000000")
	k2, _ := KeyFromHex("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF")

	dist := k1.Distance(k2)

	// XOR of 0x00...00 and 0xFF...FF should be 0xFF...FF
	if dist != k2 {
		t.Errorf("Distance calculation incorrect: got %s, want %s", dist.Hex(), k2.Hex())
	}

	// Distance should be symmetric
	dist2 := k2.Distance(k1)
	if dist != dist2 {
		t.Error("Distance should be symmetric")
	}

	// Distance to self should be zero
	dist3 := k1.Distance(k1)
	expected, _ := KeyFromHex("0000000000000000000000000000000000000000")
	if dist3 != expected {
		t.Error("Distance to self should be zero")
	}
}

func TestTransactionID(t *testing.T) {
	tx1 := RandomTransactionID()
	tx2 := RandomTransactionID()

	if len(tx1) == 0 {
		t.Error("RandomTransactionID generated empty ID")
	}

	if tx1.Equal(tx2) {
		t.Error("Two random transaction IDs should not be equal")
	}
}

func TestNodeCreation(t *testing.T) {
	// Create a node on a random available port
	testBootstrapNodes := []string{
		"udp://127.0.0.1:6881", // Use localhost for testing
	}
	node, err := NewNode(0, testBootstrapNodes)
	if err != nil {
		t.Fatalf("Failed to create node: %v", err)
	}
	defer node.Shutdown()

	if node.ID().Empty() {
		t.Error("Node ID should not be empty")
	}

	if !node.IsLocal() {
		t.Error("Created node should be local")
	}

	if node.DHT() == nil {
		t.Error("Node should have DHT routing table")
	}
}

func TestDHTAddRemoveNode(t *testing.T) {
	// Create local node
	testBootstrapNodes := []string{
		"udp://127.0.0.1:6881", // Use localhost for testing
	}
	localNode, err := NewNode(0, testBootstrapNodes)
	if err != nil {
		t.Fatalf("Failed to create node: %v", err)
	}
	defer localNode.Shutdown()

	dht := localNode.DHT()

	// Create a remote node
	remoteID := RandomKey()
	remoteNode := &Node{
		id: remoteID,
	}

	// Initially DHT should be empty
	if dht.Length() != 0 {
		t.Errorf("DHT should start empty, got %d nodes", dht.Length())
	}

	// Add node
	dht.AddNode(remoteNode)

	if dht.Length() != 1 {
		t.Errorf("DHT should have 1 node, got %d", dht.Length())
	}

	// Remove node
	dht.RemoveNode(remoteNode)

	if dht.Length() != 0 {
		t.Errorf("DHT should be empty after removal, got %d nodes", dht.Length())
	}
}

func TestDHTGetClosestNodes(t *testing.T) {
	// Create local node
	testBootstrapNodes := []string{
		"udp://127.0.0.1:6881", // Use localhost for testing
	}
	localNode, err := NewNode(0, testBootstrapNodes)
	if err != nil {
		t.Fatalf("Failed to create node: %v", err)
	}
	defer localNode.Shutdown()

	dht := localNode.DHT()

	// Add some nodes
	for i := 0; i < 10; i++ {
		node := &Node{
			id: RandomKey(),
		}
		dht.AddNode(node)
	}

	// Find closest nodes to a target
	target := RandomKey()
	closest := dht.GetClosestNodes(target, 5)

	if len(closest) != 5 {
		t.Errorf("Expected 5 closest nodes, got %d", len(closest))
	}

	// Verify nodes are sorted by distance
	for i := 1; i < len(closest); i++ {
		dist1 := target.Distance(closest[i-1].ID())
		dist2 := target.Distance(closest[i].ID())

		if !dist1.Less(dist2) && dist1 != dist2 {
			t.Error("Nodes not sorted by distance")
		}
	}
}

func TestKRPCMessageEncoding(t *testing.T) {
	// Create a ping query
	nodeID := RandomKey()
	query := NewQuery(MethodPing, Arguments{
		ID: nodeID[:],
	})

	// Encode
	data, err := query.EncodeBencode()
	if err != nil {
		t.Fatalf("Failed to encode message: %v", err)
	}

	// Decode
	var decoded Message
	err = decoded.DecodeBencode(data)
	if err != nil {
		t.Fatalf("Failed to decode message: %v", err)
	}

	// Verify
	if decoded.Y != MessageTypeQuery {
		t.Errorf("Expected message type %s, got %s", MessageTypeQuery, decoded.Y)
	}

	if decoded.Q != MethodPing {
		t.Errorf("Expected query method %s, got %s", MethodPing, decoded.Q)
	}
}

func TestBootstrapWithTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping bootstrap test in short mode")
	}

	// Create node
	testBootstrapNodes := []string{
		"udp://127.0.0.1:6881", // Use localhost for testing
	}
	node, err := NewNode(0, testBootstrapNodes)
	if err != nil {
		t.Fatalf("Failed to create node: %v", err)
	}
	defer node.Shutdown()

	// Bootstrap with short timeout (should still get some nodes)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = node.Bootstrap(ctx, 50)
	if err != nil && err != context.DeadlineExceeded {
		t.Errorf("Bootstrap failed: %v", err)
	}

	// Check if we got any nodes
	if node.DHT().Length() == 0 {
		t.Log("Warning: Bootstrap did not add any nodes (network might be unavailable)")
	} else {
		t.Logf("Bootstrap added %d nodes", node.DHT().Length())
	}
}
