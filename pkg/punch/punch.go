// Package punch implements BEP 55 Holepunch Extension.
// https://www.bittorrent.org/beps/bep_0055.html
//
// Holepunching allows two peers behind NAT to establish a direct connection
// via a mutual third peer (the relay) that both are already connected to.
//
// # Protocol overview
//
//  1. Peers advertise holepunch support in their BEP 10 extension handshake:
//     {"m": {"ut_holepunch": N}}
//     and set FlagHolepunch (0x08) in PEX peer flags.
//
//  2. When peer A cannot connect directly to peer B, A sends a Rendezvous
//     message to relay peer C (a peer both A and B are connected to):
//     A → C: Rendezvous{Addr: B}
//
//  3. C forwards a Connect message to B:
//     C → B: Connect{Addr: A}
//     and also sends a Connect back to A:
//     C → A: Connect{Addr: B}
//
//  4. Both A and B attempt simultaneous outbound TCP (or uTP) connects to
//     each other. Because both sides initiate at the same time, NAT mappings
//     are opened on both ends and the connection succeeds.
//
// # Wire format
//
// Each holepunch message is a BEP 10 extended message with the negotiated
// ut_holepunch extension ID. The payload is a bencoded dict:
//
//	{
//	  "msg_type": <int>   // 0=Rendezvous, 1=Connect, 2=Error
//	  "addr":     <bytes> // compact IPv4 (6 bytes) or IPv6 (18 bytes) address
//	  "err_code": <int>   // only present for Error messages
//	}
//
// # Error codes
//
//	0  NoError      (unused)
//	1  NotConnected relay is not connected to the target peer
//	2  NoSupport    target peer does not support holepunching
//	3  NoSelf       initiator and target are the same peer
//	4  InvalidAddr  address in Rendezvous is unroutable or malformed
package punch

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/cbluth/bittorrent/pkg/bencode"
)

// Message type constants (msg_type field).
const (
	MsgRendezvous uint8 = 0 // A → relay C: "please connect me to B"
	MsgConnect    uint8 = 1 // relay C → B (and C → A): "initiate punch to addr"
	MsgError      uint8 = 2 // relay C → A: something went wrong
)

// Error code constants (err_code field in Error messages).
const (
	ErrNotConnected uint8 = 1 // relay is not connected to target
	ErrNoSupport    uint8 = 2 // target does not support holepunching
	ErrNoSelf       uint8 = 3 // initiator == target
	ErrInvalidAddr  uint8 = 4 // address is unroutable or malformed
)

// Message is a decoded ut_holepunch payload.
type Message struct {
	Type    uint8
	Addr    *net.UDPAddr // compact IPv4 or IPv6 address
	ErrCode uint8        // only meaningful for MsgError
}

// wireMsg is the bencoded representation of a holepunch message.
type wireMsg struct {
	MsgType uint8  `bencode:"msg_type"`
	Addr    []byte `bencode:"addr"`
	ErrCode uint8  `bencode:"err_code,omitempty"`
}

// Encode serialises a Message into the bencoded wire format.
func Encode(msg *Message) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("punch: encode: nil message")
	}
	addr, err := encodeAddr(msg.Addr)
	if err != nil {
		return nil, fmt.Errorf("punch: encode addr: %w", err)
	}
	w := wireMsg{
		MsgType: msg.Type,
		Addr:    addr,
		ErrCode: msg.ErrCode,
	}
	return bencode.EncodeBytes(w)
}

// Decode parses a bencoded ut_holepunch payload into a Message.
func Decode(data []byte) (*Message, error) {
	var w wireMsg
	if err := bencode.DecodeBytes(data, &w); err != nil {
		return nil, fmt.Errorf("punch: decode: %w", err)
	}
	addr, err := decodeAddr(w.Addr)
	if err != nil {
		return nil, fmt.Errorf("punch: decode addr: %w", err)
	}
	return &Message{
		Type:    w.MsgType,
		Addr:    addr,
		ErrCode: w.ErrCode,
	}, nil
}

// encodeAddr encodes a UDP address as compact IPv4 (6 bytes) or IPv6 (18 bytes).
func encodeAddr(addr *net.UDPAddr) ([]byte, error) {
	if addr == nil {
		return nil, fmt.Errorf("nil addr")
	}
	port := uint16(addr.Port)
	if ip4 := addr.IP.To4(); ip4 != nil {
		buf := make([]byte, 6)
		copy(buf[:4], ip4)
		binary.BigEndian.PutUint16(buf[4:], port)
		return buf, nil
	}
	ip6 := addr.IP.To16()
	if ip6 == nil {
		return nil, fmt.Errorf("invalid IP address: %v", addr.IP)
	}
	buf := make([]byte, 18)
	copy(buf[:16], ip6)
	binary.BigEndian.PutUint16(buf[16:], port)
	return buf, nil
}

// decodeAddr decodes a compact IPv4 (6 bytes) or IPv6 (18 bytes) address.
func decodeAddr(data []byte) (*net.UDPAddr, error) {
	switch len(data) {
	case 6:
		ip := make(net.IP, 4)
		copy(ip, data[:4])
		port := binary.BigEndian.Uint16(data[4:6])
		return &net.UDPAddr{IP: ip, Port: int(port)}, nil
	case 18:
		ip := make(net.IP, 16)
		copy(ip, data[:16])
		port := binary.BigEndian.Uint16(data[16:18])
		return &net.UDPAddr{IP: ip, Port: int(port)}, nil
	default:
		return nil, fmt.Errorf("compact addr must be 6 or 18 bytes, got %d", len(data))
	}
}
