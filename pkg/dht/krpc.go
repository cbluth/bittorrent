package dht

// BEP 5: DHT Protocol - KRPC Implementation
// https://www.bittorrent.org/beps/bep_0005.html
//
// BEP 32: IPv6 Extension for DHT
// https://www.bittorrent.org/beps/bep_0032.html
//
// This file implements the KRPC protocol used by the BitTorrent DHT:
// - Message encoding/decoding (bencode format)
// - Query types: ping, find_node, get_peers, announce_peer
// - Response and error handling
// - Compact node info and peer info parsing (BEP 5 & BEP 32 formats)
// - IPv6 support via nodes6 and want parameters (BEP 32)

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/cbluth/bittorrent/pkg/bencode"
)

// KRPC message types (BEP 5)
const (
	MessageTypeQuery    = "q"
	MessageTypeResponse = "r"
	MessageTypeError    = "e"
)

// KRPC query methods
const (
	MethodPing             = "ping"
	MethodFindNode         = "find_node"
	MethodGetPeers         = "get_peers"
	MethodAnnouncePeer     = "announce_peer"
	MethodGet              = "get"               // BEP 44
	MethodPut              = "put"               // BEP 44
	MethodSampleInfohashes = "sample_infohashes" // BEP 51
)

// Version is the DHT client version identifier
const Version = "NI01" // 4 characters

// BEP 44 error codes
const (
	ErrorGeneric       = 201 // Generic error
	ErrorServer        = 202 // Server error
	ErrorProtocol      = 203 // Protocol error (malformed packet, invalid bencoding, etc.)
	ErrorMethodUnknown = 204 // Method unknown
	ErrorSeqLessThan   = 302 // Sequence number less than current (BEP 44 mutable items)
)

// BEP 44 constants
const (
	MaxValueSize = 1000 // Maximum size of value in put request (bytes)
	MaxSaltSize  = 64   // Maximum size of salt (bytes)
)

// Message represents a KRPC message (BEP 5)
type Message struct {
	Origin *net.UDPAddr `bencode:"-" json:"origin,omitempty"` // Not part of protocol, added locally

	// Required fields
	T  TransactionID `bencode:"t" json:"t"`                       // Transaction ID
	Y  string        `bencode:"y" json:"y"`                       // Message type: "q", "r", or "e"
	V  string        `bencode:"v,omitempty" json:"v,omitempty"`   // BEP 5: Client version (4 chars)
	IP []byte        `bencode:"ip,omitempty" json:"ip,omitempty"` // BEP 42: requestor's compact IP+port

	// BEP 43: Read-only flag — set to 1 in outgoing queries when the node
	// is in read-only mode.  Responders must not add ro=1 nodes to routing tables.
	RO int `bencode:"ro,omitempty" json:"ro,omitempty"`

	// Query-specific fields
	Q string     `bencode:"q,omitempty" json:"q,omitempty"` // Query method name
	A *Arguments `bencode:"a,omitempty" json:"a,omitempty"` // Query arguments

	// Response-specific fields
	R *Response `bencode:"r,omitempty" json:"r,omitempty"` // Response data

	// Error-specific fields
	E []any `bencode:"e,omitempty" json:"e,omitempty"` // BEP 5: Error as [code, message]
}

// Arguments contains query arguments (BEP 5 + BEP 32 + BEP 44)
type Arguments struct {
	ID          []byte   `bencode:"id" json:"id"`                                         // Querying node's ID
	Port        int      `bencode:"port,omitempty" json:"port,omitempty"`                 // Port for announce_peer
	Want        []string `bencode:"want,omitempty" json:"want,omitempty"`                 // BEP 32: list of "n4" and/or "n6"
	Token       []byte   `bencode:"token,omitempty" json:"token,omitempty"`               // Token for announce_peer / put (BEP 44)
	Target      []byte   `bencode:"target,omitempty" json:"target,omitempty"`             // Target node ID for find_node / get (BEP 44)
	InfoHash    []byte   `bencode:"info_hash,omitempty" json:"info_hash,omitempty"`       // Info hash for get_peers/announce_peer
	ImpliedPort int      `bencode:"implied_port,omitempty" json:"implied_port,omitempty"` // BEP 5: 0 or 1, use source port if 1

	// BEP 44: Storing arbitrary data in the DHT
	V    any    `bencode:"v,omitempty" json:"v,omitempty"`       // Value (any bencoded type, size <= 1000 bytes)
	Seq  *int64 `bencode:"seq,omitempty" json:"seq,omitempty"`   // Sequence number for mutable items
	K    []byte `bencode:"k,omitempty" json:"k,omitempty"`       // ed25519 public key (32 bytes) for mutable items
	Sig  []byte `bencode:"sig,omitempty" json:"sig,omitempty"`   // ed25519 signature (64 bytes) for mutable items
	Salt []byte `bencode:"salt,omitempty" json:"salt,omitempty"` // Optional salt for mutable items (max 64 bytes)
	CAS  *int64 `bencode:"cas,omitempty" json:"cas,omitempty"`   // Compare-and-swap: expected seq number
}

// Response contains query response data (BEP 5 + BEP 32 + BEP 44 + BEP 51)
type Response struct {
	ID     []byte   `bencode:"id,omitempty" json:"id,omitempty"`         // Responding node's ID
	Nodes  []byte   `bencode:"nodes,omitempty" json:"nodes,omitempty"`   // Compact IPv4 node info (26 bytes per node)
	Nodes6 []byte   `bencode:"nodes6,omitempty" json:"nodes6,omitempty"` // BEP 32: Compact IPv6 node info (38 bytes per node)
	Token  []byte   `bencode:"token,omitempty" json:"token,omitempty"`   // Token for announce_peer / get (BEP 44)
	Values [][]byte `bencode:"values,omitempty" json:"values,omitempty"` // Compact peer info (6 or 18 bytes per peer)

	// BEP 44: Storing arbitrary data in the DHT
	V   any    `bencode:"v,omitempty" json:"v,omitempty"`     // Value (any bencoded type)
	Seq *int64 `bencode:"seq,omitempty" json:"seq,omitempty"` // Sequence number for mutable items
	K   []byte `bencode:"k,omitempty" json:"k,omitempty"`     // ed25519 public key (32 bytes) for mutable items
	Sig []byte `bencode:"sig,omitempty" json:"sig,omitempty"` // ed25519 signature (64 bytes) for mutable items

	// BEP 51: DHT Infohash Indexing
	Interval *int   `bencode:"interval,omitempty" json:"interval,omitempty"` // Refresh interval in seconds (0-21600)
	Num      *int   `bencode:"num,omitempty" json:"num,omitempty"`           // Number of infohashes in storage
	Samples  []byte `bencode:"samples,omitempty" json:"samples,omitempty"`   // Concatenated infohashes (N × 20 bytes)
}

// Error represents a KRPC error (BEP 5)
type Error struct {
	Code    int    `bencode:"-" json:"code"`    // Error code
	Message string `bencode:"-" json:"message"` // Error message
}

// EncodeBencode encodes the message as bencode
func (m *Message) EncodeBencode() ([]byte, error) {
	return bencode.EncodeBytes(m)
}

// DecodeBencode decodes a bencode message
func (m *Message) DecodeBencode(data []byte) error {
	return bencode.DecodeBytes(data, m)
}

// String returns a JSON representation of the message for debugging
func (m *Message) String() string {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprintf("Message{error: %v}", err)
	}
	return string(b)
}

// NewQuery creates a new KRPC query message
func NewQuery(method string, args Arguments) *Message {
	return &Message{
		T: RandomTransactionID(),
		Y: MessageTypeQuery,
		V: Version, // BEP 5: Include client version
		Q: method,
		A: &args,
	}
}

// NewResponse creates a new KRPC response message
func NewResponse(txID TransactionID, resp Response) *Message {
	return &Message{
		T: txID,
		Y: MessageTypeResponse,
		V: Version, // BEP 5: Include client version
		R: &resp,
	}
}

// NewError creates a new KRPC error message (BEP 5 format: [code, "message"])
func NewError(txID TransactionID, code int, message string) *Message {
	return &Message{
		T: txID,
		Y: MessageTypeError,
		V: Version,
		E: []any{code, message}, // BEP 5: Error as list [code, message]
	}
}

// ParseError extracts error code and message from BEP 5 format
func (m *Message) ParseError() (int, string, error) {
	if len(m.E) < 2 {
		return 0, "", fmt.Errorf("invalid error format")
	}

	code, ok := m.E[0].(int)
	if !ok {
		// Try int64 (bencode decodes integers as int64)
		if code64, ok := m.E[0].(int64); ok {
			code = int(code64)
		} else {
			return 0, "", fmt.Errorf("error code is not an integer")
		}
	}

	msg, ok := m.E[1].(string)
	if !ok {
		return 0, "", fmt.Errorf("error message is not a string")
	}

	return code, msg, nil
}

// ParseCompactNodeInfo parses compact node info (26 bytes per node: 20-byte ID + 4-byte IP + 2-byte port)
// This is the format used in find_node responses (BEP 5)
func ParseCompactNodeInfo(data []byte) []*net.UDPAddr {
	addrs := []*net.UDPAddr{}

	// Each node is 26 bytes: 20-byte ID + 4-byte IP + 2-byte port
	// Extract all IP:port pairs
	for i := 0; i < len(data); i += 26 {
		if i+26 > len(data) {
			break
		}

		// Skip node ID (bytes 0-19), extract IP:port (bytes 20-25)
		ipBytes := data[i+20 : i+24]
		portBytes := data[i+24 : i+26]

		ip := net.IPv4(ipBytes[0], ipBytes[1], ipBytes[2], ipBytes[3])
		port := int(binary.BigEndian.Uint16(portBytes))
		addrs = append(addrs, &net.UDPAddr{IP: ip, Port: port})
	}

	return addrs
}

// ParseCompactNodeInfoWithID parses compact node info and returns nodes with both ID and address
// Each node is 26 bytes: 20-byte ID + 4-byte IPv4 + 2-byte port
func ParseCompactNodeInfoWithID(data []byte) []*Node {
	nodes := []*Node{}

	for i := 0; i < len(data); i += 26 {
		if i+26 > len(data) {
			break
		}

		// Extract node ID (bytes 0-19)
		var nodeID Key
		copy(nodeID[:], data[i:i+20])

		// Extract IP:port (bytes 20-25)
		ipBytes := data[i+20 : i+24]
		portBytes := data[i+24 : i+26]

		ip := net.IPv4(ipBytes[0], ipBytes[1], ipBytes[2], ipBytes[3])
		port := int(binary.BigEndian.Uint16(portBytes))
		addr := &net.UDPAddr{IP: ip, Port: port}

		// Create node with ID and address
		node := NewRemoteNode(addr)
		node.id = nodeID
		nodes = append(nodes, node)
	}

	return nodes
}

// ParseCompactPeerInfo parses compact peer info (6 bytes per peer: 4-byte IP + 2-byte port)
// This is the format used in get_peers responses (BEP 5)
func ParseCompactPeerInfo(data []byte) []*net.UDPAddr {
	addrs := []*net.UDPAddr{}

	for i := 0; i < len(data); i += 6 {
		if i+6 > len(data) {
			break
		}

		chunk := data[i : i+6]
		if len(chunk) == 6 {
			ip := net.IPv4(chunk[0], chunk[1], chunk[2], chunk[3])
			port := int(binary.BigEndian.Uint16(chunk[4:6]))
			addrs = append(addrs, &net.UDPAddr{IP: ip, Port: port})
		}
	}

	return addrs
}

// ParseCompactNodeInfoIPv6 parses compact IPv6 node info (BEP 32)
// Format: 38 bytes per node (20-byte ID + 16-byte IPv6 + 2-byte port)
// This is used in the "nodes6" parameter of find_node and get_peers responses.
// Deprecated: use ParseCompactNodeInfoIPv6WithID to preserve node IDs.
func ParseCompactNodeInfoIPv6(data []byte) []*net.UDPAddr {
	nodes := ParseCompactNodeInfoIPv6WithID(data)
	addrs := make([]*net.UDPAddr, len(nodes))
	for i, n := range nodes {
		addrs[i] = n.Address()
	}
	return addrs
}

// ParseCompactNodeInfoIPv6WithID parses compact IPv6 node info (BEP 32) and
// returns nodes with both their 20-byte ID and IPv6 address.
// Each entry is 38 bytes: 20-byte node ID + 16-byte IPv6 + 2-byte port.
func ParseCompactNodeInfoIPv6WithID(data []byte) []*Node {
	nodes := []*Node{}

	for i := 0; i+38 <= len(data); i += 38 {
		var nodeID Key
		copy(nodeID[:], data[i:i+20])

		ip := make(net.IP, 16)
		copy(ip, data[i+20:i+36])
		port := int(binary.BigEndian.Uint16(data[i+36 : i+38]))

		node := NewRemoteNode(&net.UDPAddr{IP: ip, Port: port})
		node.id = nodeID
		nodes = append(nodes, node)
	}

	return nodes
}

// ParseCompactPeerInfoIPv6 parses compact IPv6 peer info (BEP 32)
// Format: 18 bytes per peer (16-byte IPv6 + 2-byte port)
// This is used in the "values" parameter of get_peers responses over IPv6
func ParseCompactPeerInfoIPv6(data []byte) []*net.UDPAddr {
	addrs := []*net.UDPAddr{}

	for i := 0; i < len(data); i += 18 {
		if i+18 > len(data) {
			break
		}

		chunk := data[i : i+18]
		if len(chunk) == 18 {
			ip := net.IP(chunk[0:16])
			port := int(binary.BigEndian.Uint16(chunk[16:18]))
			addrs = append(addrs, &net.UDPAddr{IP: ip, Port: port})
		}
	}

	return addrs
}

// SendQuery sends a KRPC query and waits for a response
func SendQuery(conn *net.UDPConn, addr *net.UDPAddr, query *Message, timeoutSeconds int) (*Message, error) {
	// Encode the query
	data, err := query.EncodeBencode()
	if err != nil {
		return nil, fmt.Errorf("failed to encode query: %w", err)
	}

	// Send the query
	if _, err := conn.WriteToUDP(data, addr); err != nil {
		return nil, fmt.Errorf("failed to send query: %w", err)
	}

	// Set read deadline for timeout
	if timeoutSeconds > 0 {
		deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
		conn.SetReadDeadline(deadline)
	}

	// Read response with timeout
	buf := make([]byte, 2048)
	n, origin, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Decode response
	var resp Message
	if err := resp.DecodeBencode(buf[:n]); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	resp.Origin = origin
	return &resp, nil
}

// PingNode sends a ping query to a node
func PingNode(conn *net.UDPConn, addr *net.UDPAddr, nodeID Key, timeoutSeconds int) (TransactionID, *Message, error) {
	query := NewQuery(MethodPing, Arguments{
		ID: nodeID[:],
	})

	resp, err := SendQuery(conn, addr, query, timeoutSeconds)
	if err != nil {
		return nil, nil, err
	}

	return query.T, resp, nil
}

// FindNode sends a find_node query to locate nodes close to a target
func FindNode(conn *net.UDPConn, addr *net.UDPAddr, nodeID, target Key, timeoutSeconds int) (TransactionID, *Message, error) {
	return FindNodeWithWant(conn, addr, nodeID, target, nil, timeoutSeconds)
}

// FindNodeWithWant sends a find_node query with optional want parameter (BEP 32)
// want can be nil (default behavior), or []string{"n4"}, []string{"n6"}, or []string{"n4", "n6"}
func FindNodeWithWant(conn *net.UDPConn, addr *net.UDPAddr, nodeID, target Key, want []string, timeoutSeconds int) (TransactionID, *Message, error) {
	args := Arguments{
		ID:     nodeID[:],
		Target: target[:],
	}

	// Add want parameter for IPv6 support (BEP 32)
	if len(want) > 0 {
		args.Want = want
	}

	query := NewQuery(MethodFindNode, args)

	resp, err := SendQuery(conn, addr, query, timeoutSeconds)
	if err != nil {
		return nil, nil, err
	}

	return query.T, resp, nil
}

// GetPeers sends a get_peers query to find peers for an info hash
func GetPeers(conn *net.UDPConn, addr *net.UDPAddr, nodeID, infoHash Key, timeoutSeconds int) (TransactionID, *Message, error) {
	return GetPeersWithWant(conn, addr, nodeID, infoHash, nil, timeoutSeconds)
}

// GetPeersWithWant sends a get_peers query with optional want parameter (BEP 32)
// want can be nil (default behavior), or []string{"n4"}, []string{"n6"}, or []string{"n4", "n6"}
func GetPeersWithWant(conn *net.UDPConn, addr *net.UDPAddr, nodeID, infoHash Key, want []string, timeoutSeconds int) (TransactionID, *Message, error) {
	args := Arguments{
		ID:       nodeID[:],
		InfoHash: infoHash[:],
	}

	// Add want parameter for IPv6 support (BEP 32)
	if len(want) > 0 {
		args.Want = want
	}

	query := NewQuery(MethodGetPeers, args)

	resp, err := SendQuery(conn, addr, query, timeoutSeconds)
	if err != nil {
		return nil, nil, err
	}

	return query.T, resp, nil
}

// AnnouncePeer announces that the peer controlling this node is downloading a torrent
// on the specified port. The token must be from a recent get_peers response from the same node.
func AnnouncePeer(conn *net.UDPConn, addr *net.UDPAddr, nodeID, infoHash Key, port int, token []byte, timeoutSeconds int) (TransactionID, *Message, error) {
	return AnnouncePeerWithImpliedPort(conn, addr, nodeID, infoHash, port, token, false, timeoutSeconds)
}

// AnnouncePeerWithImpliedPort announces a peer with optional implied_port parameter (BEP 5)
// If impliedPort is true, the port argument is ignored and the source UDP port is used instead.
// This is useful for NAT traversal and uTP support.
func AnnouncePeerWithImpliedPort(conn *net.UDPConn, addr *net.UDPAddr, nodeID, infoHash Key, port int, token []byte, impliedPort bool, timeoutSeconds int) (TransactionID, *Message, error) {
	args := Arguments{
		ID:       nodeID[:],
		InfoHash: infoHash[:],
		Port:     port,
		Token:    token,
	}

	// Add implied_port if requested (BEP 5)
	if impliedPort {
		args.ImpliedPort = 1
	}

	query := NewQuery(MethodAnnouncePeer, args)

	resp, err := SendQuery(conn, addr, query, timeoutSeconds)
	if err != nil {
		return nil, nil, err
	}

	return query.T, resp, nil
}

// CompactIPPort encodes an IP and port as BEP 42 compact format:
// 4-byte IPv4 (or 16-byte IPv6) + 2-byte big-endian port.
func CompactIPPort(addr *net.UDPAddr) []byte {
	if addr == nil {
		return nil
	}
	ip4 := addr.IP.To4()
	if ip4 != nil {
		buf := make([]byte, 6)
		copy(buf[:4], ip4)
		binary.BigEndian.PutUint16(buf[4:], uint16(addr.Port))
		return buf
	}
	ip6 := addr.IP.To16()
	if ip6 == nil {
		return nil
	}
	buf := make([]byte, 18)
	copy(buf[:16], ip6)
	binary.BigEndian.PutUint16(buf[16:], uint16(addr.Port))
	return buf
}

// ParseCompactIPPort decodes a BEP 42 compact IP+port (6 or 18 bytes) into a net.IP.
func ParseCompactIPPort(data []byte) net.IP {
	switch len(data) {
	case 6:
		return net.IPv4(data[0], data[1], data[2], data[3])
	case 18:
		ip := make(net.IP, 16)
		copy(ip, data[:16])
		return ip
	default:
		return nil
	}
}
