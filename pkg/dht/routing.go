// Kademlia k-bucket routing table for the BitTorrent DHT (BEP 5 / BEP 32).
//
// Design (BEP 5 §Routing Table):
//   - 160 buckets, one per bit of the 160-bit XOR distance space.
//   - Each bucket holds at most K=8 nodes ("good" set), oldest first.
//   - Each bucket also holds a replacement cache of up to K candidates.
//   - When a bucket is full, the least-recently-seen (LRS) node is pinged.
//     If it responds, the candidate is discarded.  If not (bad), it is replaced.
//   - Nodes are good (responded within NodeTimeout), questionable (silent for
//     NodeTimeout), or bad (known unreachable / too many failed queries).
package dht

import (
	"sync"
	"time"
)

const (
	// K is the maximum number of nodes per bucket (Kademlia bucket size).
	K = 8

	// Alpha is the number of parallel lookup requests (Kademlia concurrency).
	Alpha = 3

	// KeyBits is the number of bits in a node ID / info hash.
	KeyBits = 160
)

// XORDistance returns the XOR distance between two Keys.
func XORDistance(a, b Key) Key {
	var d Key
	for i := range a {
		d[i] = a[i] ^ b[i]
	}
	return d
}

// CommonPrefixLen returns the number of leading bits shared by a and b.
// This determines which bucket a node falls into.
func CommonPrefixLen(a, b Key) int {
	d := XORDistance(a, b)
	for i, bt := range d {
		if bt == 0 {
			continue
		}
		for bit := 7; bit >= 0; bit-- {
			if (bt>>uint(bit))&1 == 1 {
				return i*8 + (7 - bit)
			}
		}
	}
	return KeyBits // identical
}

// bucket holds up to K good nodes (LRU order: oldest at index 0) and a
// replacement cache of up to K candidates (newest first).
type bucket struct {
	nodes       []*Node
	replacement []*Node
}

// addOrUpdate inserts or refreshes a node in the bucket.
// Returns true if the node was added/refreshed; false if the bucket is full
// and the LRS node should be pinged before deciding what to do.
func (b *bucket) addOrUpdate(n *Node) bool {
	// Existing node → refresh and move to back (MRU).
	for i, existing := range b.nodes {
		if existing.id == n.id {
			existing.lastSeen = n.lastSeen
			existing.Addr = n.Addr
			// Move to back without allocating: shift left, place at end.
			copy(b.nodes[i:], b.nodes[i+1:])
			b.nodes[len(b.nodes)-1] = existing
			return true
		}
	}

	// Room in bucket: just append.
	if len(b.nodes) < K {
		b.nodes = append(b.nodes, n)
		return true
	}

	// Bucket full. If LRS is bad, replace it immediately.
	lrs := b.nodes[0]
	if lrs.EvaluateState() == NodeBad {
		b.nodes[0] = n
		return true
	}

	// LRS is still alive. Park candidate in replacement cache and signal caller.
	for _, r := range b.replacement {
		if r.id == n.id {
			return false // already cached
		}
	}
	if len(b.replacement) < K {
		// Newest first.
		b.replacement = append([]*Node{n}, b.replacement...)
	}
	return false
}

// Table is a Kademlia routing table with KeyBits buckets.
type Table struct {
	mu      sync.RWMutex
	self    Key
	buckets [KeyBits]*bucket

	// pingNode is called (in a goroutine) when the table needs to verify a
	// questionable LRS node.  The callback should KRPC-ping the node, then
	// call Seen(node.id) on success or Remove(node.id) on timeout.
	pingNode func(n *Node)
}

// NewTable creates an empty routing table for the local node with the given ID.
func NewTable(self Key, pingNode func(n *Node)) *Table {
	t := &Table{self: self, pingNode: pingNode}
	for i := range t.buckets {
		t.buckets[i] = &bucket{}
	}
	return t
}

// bucketIndex returns the bucket a node with the given ID belongs in.
func (t *Table) bucketIndex(id Key) int {
	cpl := CommonPrefixLen(t.self, id)
	if cpl >= KeyBits {
		return KeyBits - 1 // our own ID: shouldn't be added
	}
	return cpl
}

// Add attempts to add or refresh a node.  If the bucket is full, a ping of
// the LRS node is triggered in a goroutine via the pingNode callback.
func (t *Table) Add(n *Node) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if n.id == t.self {
		return
	}

	idx := t.bucketIndex(n.id)
	b := t.buckets[idx]

	if !b.addOrUpdate(n) && t.pingNode != nil && len(b.nodes) > 0 {
		lrs := b.nodes[0]
		go t.pingNode(lrs)
	}
}

// Seen moves a node to the back of its bucket (MRU) and resets its lastSeen.
// Call this whenever any KRPC response arrives from the node.
func (t *Table) Seen(id Key) {
	t.mu.Lock()
	defer t.mu.Unlock()

	b := t.buckets[t.bucketIndex(id)]
	for i, n := range b.nodes {
		if n.id == id {
			n.lastSeen = time.Now()
			n.failedQueries = 0
			copy(b.nodes[i:], b.nodes[i+1:])
			b.nodes[len(b.nodes)-1] = n
			return
		}
	}
}

// Remove evicts a node from the routing table and promotes the best replacement.
func (t *Table) Remove(id Key) {
	t.mu.Lock()
	defer t.mu.Unlock()

	b := t.buckets[t.bucketIndex(id)]
	for i, n := range b.nodes {
		if n.id == id {
			b.nodes = append(b.nodes[:i], b.nodes[i+1:]...)
			if len(b.replacement) > 0 {
				b.nodes = append(b.nodes, b.replacement[0])
				b.replacement = b.replacement[1:]
			}
			return
		}
	}
}

// Closest returns up to k non-bad nodes closest to target, sorted by XOR distance.
func (t *Table) Closest(target Key, k int) []*Node {
	t.mu.RLock()
	defer t.mu.RUnlock()

	all := make([]*Node, 0, k*2)
	for _, b := range t.buckets {
		for _, n := range b.nodes {
			if n.EvaluateState() != NodeBad {
				all = append(all, n)
			}
		}
	}
	sortByDistance(all, target)
	if len(all) > k {
		all = all[:k]
	}
	return all
}

// Len returns the total number of nodes (good + questionable).
func (t *Table) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	total := 0
	for _, b := range t.buckets {
		total += len(b.nodes)
	}
	return total
}

// All returns every node across all buckets (good + questionable).
func (t *Table) All() []*Node {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var result []*Node
	for _, b := range t.buckets {
		result = append(result, b.nodes...)
	}
	return result
}

// Questionable returns nodes that have been silent for NodeTimeout and need a ping.
func (t *Table) Questionable() []*Node {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var result []*Node
	for _, b := range t.buckets {
		for _, n := range b.nodes {
			if n.EvaluateState() == NodeQuestionable {
				result = append(result, n)
			}
		}
	}
	return result
}

// Stats returns a summary of the routing table state.
func (t *Table) Stats() map[string]any {
	t.mu.RLock()
	defer t.mu.RUnlock()
	good, questionable, bad := 0, 0, 0
	for _, b := range t.buckets {
		for _, n := range b.nodes {
			switch n.EvaluateState() {
			case NodeGood:
				good++
			case NodeQuestionable:
				questionable++
			case NodeBad:
				bad++
			}
		}
	}
	return map[string]any{
		"total":        good + questionable + bad,
		"good":         good,
		"questionable": questionable,
		"bad":          bad,
	}
}

// sortByDistance sorts nodes by XOR distance to target (ascending).
// Insertion sort is fine for small n (K=8 per bucket → ≤ 1280 nodes typical).
func sortByDistance(nodes []*Node, target Key) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0; j-- {
			da := XORDistance(nodes[j-1].id, target)
			db := XORDistance(nodes[j].id, target)
			if lessKey(db, da) {
				nodes[j-1], nodes[j] = nodes[j], nodes[j-1]
			} else {
				break
			}
		}
	}
}

// lessKey returns true if a < b in big-endian byte comparison.
func lessKey(a, b Key) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
