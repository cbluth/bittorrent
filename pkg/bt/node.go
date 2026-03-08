package bt

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/cbluth/bittorrent/pkg/dht"
)

// Node represents a DHT network node
type Node struct {
	// Identification
	ID dht.Key // 160-bit node ID

	// Network
	IP   net.IP // IP address
	Port int    // UDP port

	// DHT protocol
	dht *dht.DHT // DHT instance

	// State
	LastSeen time.Time // Last activity timestamp
	State    NodeState // Current node state
}

// NodeState represents the state of a DHT node
type NodeState int

const (
	StateGood NodeState = iota
	StateQuestionable
	StateBad
)

// String returns a string representation of the node state
func (s NodeState) String() string {
	switch s {
	case StateGood:
		return "good"
	case StateQuestionable:
		return "questionable"
	case StateBad:
		return "bad"
	default:
		return "unknown"
	}
}

// IsGood returns whether the node is considered "good"
func (n *Node) IsGood() bool {
	// A node is good if it has responded to one of our queries
	// within the last 15 minutes, or has ever responded
	// to one of our queries and has sent us a query
	// within the last 15 minutes
	return n.State == StateGood ||
		(n.State == StateQuestionable && time.Since(n.LastSeen) <= 15*time.Minute) ||
		(n.State == StateBad && time.Since(n.LastSeen) <= 15*time.Minute)
}

// IsQuestionable returns whether the node is "questionable"
func (n *Node) IsQuestionable() bool {
	// A node is questionable if we haven't seen activity in 15-60 minutes
	return n.State == StateQuestionable &&
		time.Since(n.LastSeen) > 15*time.Minute &&
		time.Since(n.LastSeen) <= 60*time.Minute
}

// IsBad returns whether the node is "bad"
func (n *Node) IsBad() bool {
	// A node is bad if we haven't seen activity in over 60 minutes
	return n.State == StateBad && time.Since(n.LastSeen) > 60*time.Minute
}

// UpdateActivity updates the node's last seen timestamp
func (n *Node) UpdateActivity() {
	n.LastSeen = time.Now()

	// Update state based on activity
	if n.IsGood() {
		n.State = StateGood
	} else if n.IsQuestionable() {
		n.State = StateQuestionable
	} else {
		n.State = StateBad
	}
}

// String returns a string representation of the node
func (n *Node) String() string {
	return net.JoinHostPort(n.IP.String(), strconv.Itoa(n.Port))
}

// IDString returns the node ID as a hex string
func (n *Node) IDString() string {
	id := make([]byte, len(n.ID))
	copy(id, n.ID[:])

	// Convert to hex string
	hex := ""
	for _, b := range id {
		hex += fmt.Sprintf("%02x", b)
	}
	return hex
}

// NewRemoteNode creates a new remote node representation
func NewRemoteNode(addr *net.UDPAddr) *Node {
	return &Node{
		IP:    addr.IP,
		Port:  addr.Port,
		State: StateGood, // Assume good until proven otherwise
	}
}

// NewNodeFromID creates a new node from an ID
func NewNodeFromID(id dht.Key) *Node {
	return &Node{
		ID:    id,
		State: StateGood, // Assume good until proven otherwise
	}
}
