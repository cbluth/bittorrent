// pkg/cmd/peer.go - Low-level peer wire protocol commands
package cmd

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/cbluth/bittorrent/pkg/cli"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
	"github.com/cbluth/bittorrent/pkg/protocol"
)

// PeerConfig carries global flags into peer subcommands.
type PeerConfig struct {
	Port    uint
	Timeout time.Duration
}

var globalPeerConfig *PeerConfig

// RunPeerCommands routes "bt peer <subcommand> [args...]" to the appropriate handler.
func RunPeerCommands(cfg *PeerConfig, args []string) error {
	globalPeerConfig = cfg

	router := cli.NewRouter("bt peer")
	router.RegisterCommand(&cli.Command{Name: "connect", Aliases: []string{"c"}, Description: "Dial a peer and stream all wire messages", Run: CmdPeerConnect})
	router.RegisterCommand(&cli.Command{Name: "handshake", Aliases: []string{"hs"}, Description: "Probe peer capabilities via BEP 3 + BEP 10", Run: CmdPeerHandshake})
	router.RegisterCommand(&cli.Command{Name: "metadata", Aliases: []string{"meta", "info"}, Description: "Fetch info dict via BEP 9 (ut_metadata)", Run: CmdPeerMetadata})
	router.RegisterCommand(&cli.Command{Name: "raw", Aliases: []string{"wire"}, Description: "Raw wire protocol debugging session", Run: CmdPeerRaw})

	return router.Route(args)
}

// CmdPeerConnect dials a peer, performs the BEP 3 + BEP 10 handshakes, then
// reads and prints all incoming wire messages until the connection closes or
// the timeout expires. Use Ctrl+C to stop.
func CmdPeerConnect(args []string) error {
	f := flag.NewFlagSet("peer connect", flag.ExitOnError)
	infoHashHex := f.String("infohash", "0000000000000000000000000000000000000000", "Info hash (40-char hex); zeros probes without matching")
	timeout := f.Duration("timeout", globalPeerConfig.Timeout, "Connection + idle timeout")
	f.Usage = func() {
		fmt.Fprintf(f.Output(), `bt peer connect — dial a peer and stream wire messages

Usage:
  bt peer connect [options] <host:port>

Examples:
  bt peer connect 1.2.3.4:6881
  bt peer connect -infohash aabbcc... 1.2.3.4:6881

Options:
`)
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() == 0 {
		f.Usage()
		return fmt.Errorf("missing host:port argument")
	}

	infoHash, err := dht.KeyFromHex(*infoHashHex)
	if err != nil {
		return fmt.Errorf("invalid infohash: %w", err)
	}
	peerID, err := protocol.DefaultPeerID()
	if err != nil {
		return fmt.Errorf("generate peer ID: %w", err)
	}

	addr := f.Arg(0)
	fmt.Printf("Connecting to %s (timeout %s)...\n", addr, *timeout)

	conn, err := net.DialTimeout("tcp", addr, *timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*timeout))

	// BEP 3 handshake
	hs := protocol.NewHandshake(infoHash, peerID)
	if _, err := conn.Write(hs.Serialize()); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}
	peerHS, err := protocol.ReadHandshake(conn)
	if err != nil {
		return fmt.Errorf("read handshake: %w", err)
	}

	fmt.Println()
	fmt.Println("=== Connected ===")
	fmt.Printf("Peer ID:  %x  (%s)\n", peerHS.PeerID, printableID(peerHS.PeerID[:]))
	if client, ver, ok := decodeAzureusID(peerHS.PeerID[:]); ok {
		fmt.Printf("Client:   %s %s\n", clientName(client), ver)
	}
	fmt.Printf("Reserved: %x\n", peerHS.Reserved)
	extSupported := peerHS.Reserved[5]&0x10 != 0
	fmt.Printf("BEP 10:   %v\n", extSupported)

	// BEP 10 extension handshake (if supported)
	if extSupported {
		extHS := protocol.NewExtensionHandshake(0)
		extData, err := extHS.Serialize()
		if err != nil {
			return fmt.Errorf("serialize ext handshake: %w", err)
		}
		if err := protocol.WriteMessage(conn, protocol.NewExtendedMessage(protocol.ExtHandshake, extData)); err != nil {
			return fmt.Errorf("send ext handshake: %w", err)
		}
	}

	fmt.Println()
	fmt.Println("=== Messages ===")

	// Read messages until connection closes.
	for {
		conn.SetDeadline(time.Now().Add(*timeout))
		msg, err := protocol.ReadMessage(conn, *timeout)
		if err != nil {
			fmt.Printf("[disconnect: %v]\n", err)
			return nil
		}
		if msg == nil {
			fmt.Println("[keepalive]")
			continue
		}
		printMessage(msg, *timeout)
	}
}

// printMessage formats and prints a received wire message.
func printMessage(msg *protocol.Message, timeout time.Duration) {
	names := map[uint8]string{
		protocol.MsgChoke:         "Choke",
		protocol.MsgUnchoke:       "Unchoke",
		protocol.MsgInterested:    "Interested",
		protocol.MsgNotInterested: "NotInterested",
		protocol.MsgHave:          "Have",
		protocol.MsgBitfield:      "Bitfield",
		protocol.MsgRequest:       "Request",
		protocol.MsgPiece:         "Piece",
		protocol.MsgCancel:        "Cancel",
		protocol.MsgPort:          "Port",
		protocol.MsgExtended:      "Extended",
	}
	name, ok := names[msg.ID]
	if !ok {
		name = fmt.Sprintf("Unknown(%d)", msg.ID)
	}
	switch msg.ID {
	case protocol.MsgHave:
		if len(msg.Payload) == 4 {
			idx := uint32(msg.Payload[0])<<24 | uint32(msg.Payload[1])<<16 |
				uint32(msg.Payload[2])<<8 | uint32(msg.Payload[3])
			fmt.Printf("[%s] piece=%d\n", name, idx)
		} else {
			fmt.Printf("[%s] payload=%x\n", name, msg.Payload)
		}
	case protocol.MsgBitfield:
		fmt.Printf("[%s] %d bytes\n", name, len(msg.Payload))
	case protocol.MsgPiece:
		if len(msg.Payload) >= 8 {
			idx := uint32(msg.Payload[0])<<24 | uint32(msg.Payload[1])<<16 |
				uint32(msg.Payload[2])<<8 | uint32(msg.Payload[3])
			begin := uint32(msg.Payload[4])<<24 | uint32(msg.Payload[5])<<16 |
				uint32(msg.Payload[6])<<8 | uint32(msg.Payload[7])
			fmt.Printf("[%s] piece=%d begin=%d data=%d bytes\n", name, idx, begin, len(msg.Payload)-8)
		} else {
			fmt.Printf("[%s] payload=%x\n", name, msg.Payload)
		}
	case protocol.MsgExtended:
		if len(msg.Payload) > 0 {
			fmt.Printf("[%s] ext_id=%d payload=%d bytes\n", name, msg.Payload[0], len(msg.Payload)-1)
		} else {
			fmt.Printf("[%s]\n", name)
		}
	default:
		if len(msg.Payload) > 0 {
			fmt.Printf("[%s] payload=%x\n", name, msg.Payload)
		} else {
			fmt.Printf("[%s]\n", name)
		}
	}
}

// CmdPeerHandshake dials a peer, performs BEP 3 + BEP 10 handshakes, and prints
// the negotiated capabilities without transferring any data.
//
// Usage:
//
//	bt peer handshake [options] <host:port>
func CmdPeerHandshake(args []string) error {
	f := flag.NewFlagSet("peer handshake", flag.ExitOnError)
	infoHashHex := f.String("infohash", "0000000000000000000000000000000000000000", "Info hash (40-char hex); zeros probes without matching")
	timeout := f.Duration("timeout", globalPeerConfig.Timeout, "Connection timeout")
	f.Usage = func() {
		fmt.Fprintf(f.Output(), `bt peer handshake — probe peer capabilities via BEP 3 + BEP 10 handshake

Usage:
  bt peer handshake [options] <host:port>

Examples:
  bt peer handshake 1.2.3.4:6881
  bt peer handshake -infohash aabbcc... 1.2.3.4:6881

Options:
`)
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() == 0 {
		f.Usage()
		return fmt.Errorf("missing host:port argument")
	}

	infoHash, err := dht.KeyFromHex(*infoHashHex)
	if err != nil {
		return fmt.Errorf("invalid infohash: %w", err)
	}
	peerID, err := protocol.DefaultPeerID()
	if err != nil {
		return fmt.Errorf("generate peer ID: %w", err)
	}

	addr := f.Arg(0)
	fmt.Printf("Connecting to %s (timeout %s)...\n", addr, *timeout)

	conn, err := net.DialTimeout("tcp", addr, *timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*timeout))

	// BEP 3 handshake
	hs := protocol.NewHandshake(infoHash, peerID)
	if _, err := conn.Write(hs.Serialize()); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}
	peerHS, err := protocol.ReadHandshake(conn)
	if err != nil {
		return fmt.Errorf("read handshake: %w", err)
	}

	fmt.Println()
	fmt.Println("=== BEP 3 Handshake ===")
	fmt.Printf("Peer ID (hex):  %x\n", peerHS.PeerID)
	fmt.Printf("Peer ID (ascii):%s\n", printableID(peerHS.PeerID[:]))
	if client, ver, ok := decodeAzureusID(peerHS.PeerID[:]); ok {
		fmt.Printf("Client:         %s %s\n", clientName(client), ver)
	}
	fmt.Printf("Reserved bytes: %x\n", peerHS.Reserved)
	fmt.Printf("Info hash:      %x\n", peerHS.InfoHash)

	// Decode reserved flags
	fmt.Println()
	fmt.Println("=== Extension Flags ===")
	reserved := peerHS.Reserved
	fmt.Printf("BEP 10 (extensions): %v\n", reserved[5]&0x10 != 0)
	fmt.Printf("BEP 6  (fast):       %v\n", reserved[7]&0x04 != 0)
	fmt.Printf("BEP 29 (DHT):        %v\n", reserved[7]&0x01 != 0)
	fmt.Printf("BEP 55 (holepunch):  %v\n", reserved[6]&0x80 != 0)

	if !peerHS.SupportsExtensions() {
		fmt.Println("\nPeer does not support BEP 10 extension protocol.")
		return nil
	}

	// BEP 10 extension handshake
	extHS := protocol.NewExtensionHandshake(0)
	extData, err := extHS.Serialize()
	if err != nil {
		return fmt.Errorf("serialize ext handshake: %w", err)
	}
	if err := protocol.WriteMessage(conn, protocol.NewExtendedMessage(protocol.ExtHandshake, extData)); err != nil {
		return fmt.Errorf("send ext handshake: %w", err)
	}

	// Read messages until we receive the extension handshake
	var peerExt *protocol.ExtensionHandshake
	for peerExt == nil {
		msg, err := protocol.ReadMessage(conn, *timeout)
		if err != nil {
			return fmt.Errorf("read ext handshake: %w", err)
		}
		if msg == nil || msg.ID != protocol.MsgExtended {
			continue
		}
		extID, payload, err := protocol.ParseExtendedMessage(msg)
		if err != nil || extID != protocol.ExtHandshake {
			continue
		}
		peerExt, err = protocol.ParseExtensionHandshake(payload)
		if err != nil {
			return fmt.Errorf("parse ext handshake: %w", err)
		}
	}

	fmt.Println()
	fmt.Println("=== BEP 10 Extension Handshake ===")
	if peerExt.V != "" {
		fmt.Printf("Client version (v):  %s\n", peerExt.V)
	}
	if peerExt.Reqq > 0 {
		fmt.Printf("Request queue (reqq):%d\n", peerExt.Reqq)
	}
	if peerExt.MetadataSize > 0 {
		fmt.Printf("Metadata size:       %d bytes\n", peerExt.MetadataSize)
	}
	if peerExt.YourIP != "" {
		fmt.Printf("Your IP (yourip):    %s\n", peerExt.YourIP)
	}
	fmt.Println("Extensions (m dict):")
	for name, id := range peerExt.M {
		fmt.Printf("  %-20s id=%d\n", name, id)
	}

	return nil
}

// CmdPeerMetadata fetches the info dictionary from a peer that supports ut_metadata
// (BEP 9) and prints it as JSON. Optionally writes a .torrent file with -o.
//
// Usage:
//
//	bt peer metadata -infohash <hex> [options] <host:port>
func CmdPeerMetadata(args []string) error {
	f := flag.NewFlagSet("peer metadata", flag.ExitOnError)
	infoHashHex := f.String("infohash", "", "Info hash to fetch (40-char hex, required)")
	output := f.String("o", "", "Write .torrent file to this path (optional)")
	timeout := f.Duration("timeout", 30*time.Second, "Metadata fetch timeout")
	f.Usage = func() {
		fmt.Fprintf(f.Output(), `bt peer metadata — fetch info dict from a single peer via BEP 9 (ut_metadata)

Usage:
  bt peer metadata -infohash <hex> [options] <host:port>

Examples:
  bt peer metadata -infohash aabbcc... 1.2.3.4:6881
  bt peer metadata -infohash aabbcc... -o out.torrent 1.2.3.4:6881

Options:
`)
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() == 0 {
		f.Usage()
		return fmt.Errorf("missing host:port argument")
	}
	if *infoHashHex == "" {
		return fmt.Errorf("-infohash is required")
	}

	infoHash, err := dht.KeyFromHex(*infoHashHex)
	if err != nil {
		return fmt.Errorf("invalid infohash: %w", err)
	}
	peerID, err := protocol.DefaultPeerID()
	if err != nil {
		return fmt.Errorf("generate peer ID: %w", err)
	}

	addr := f.Arg(0)
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return fmt.Errorf("resolve addr: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Fetching metadata from %s (infohash %s, timeout %s)...\n", addr, *infoHashHex, *timeout)

	meta, err := protocol.FetchMetadata(tcpAddr, infoHash, peerID, *timeout, log.Logger())
	if err != nil {
		return fmt.Errorf("fetch metadata: %w", err)
	}

	// Print as JSON to stdout
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(meta); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}

	// Optionally write .torrent file
	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			return fmt.Errorf("create output file: %w", err)
		}
		defer f.Close()
		if err := meta.Write(f); err != nil {
			return fmt.Errorf("write torrent: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Wrote %s\n", *output)
	}

	return nil
}

// CmdPeerRaw sends raw bencode-encoded messages directly to a peer from stdin
// and prints received messages as hex + decoded text to stdout.
func CmdPeerRaw(args []string) error {
	f := flag.NewFlagSet("peer raw", flag.ExitOnError)
	infoHash := f.String("infohash", "", "Info hash for handshake (40-char hex, required)")
	f.Usage = func() {
		fmt.Fprintf(f.Output(), `bt peer raw — raw wire protocol session with a peer (debugging)

Usage:
  bt peer raw [options] <host:port>

  Reads hex-encoded messages from stdin, sends to peer, prints responses.

Options:
`)
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() == 0 {
		f.Usage()
		return fmt.Errorf("missing host:port argument")
	}
	if *infoHash == "" {
		return fmt.Errorf("-infohash is required")
	}

	addr := f.Arg(0)
	_ = addr

	return fmt.Errorf("not yet implemented: bt peer raw")
}

// decodeAzureusID tries to parse a peer ID in Azureus style: -XX0000-<12 random>.
// Returns (clientCode, version, ok).
func decodeAzureusID(id []byte) (string, string, bool) {
	if len(id) < 8 || id[0] != '-' || id[7] != '-' {
		return "", "", false
	}
	client := string(id[1:3])
	version := string(id[3:7])
	return client, version, true
}

// clientName maps a 2-char Azureus client code to a human-readable name.
func clientName(code string) string {
	names := map[string]string{
		"BT": "bt (this client)",
		"qB": "qBittorrent",
		"lt": "libtorrent",
		"LT": "libtorrent",
		"UT": "µTorrent",
		"TR": "Transmission",
		"DE": "Deluge",
		"AZ": "Vuze/Azureus",
		"BC": "BitComet",
		"BS": "BitSpirit",
		"WW": "WebTorrent",
		"AG": "Ares",
		"AR": "Arctic",
		"MO": "MonoTorrent",
		"SD": "Thunder/Xunlei",
		"XL": "Xunlei",
	}
	if name, ok := names[code]; ok {
		return name
	}
	return code
}

// printableID returns the peer ID bytes as a string, replacing non-printable
// bytes with dots.
func printableID(id []byte) string {
	out := make([]byte, len(id))
	for i, b := range id {
		if b >= 32 && b < 127 {
			out[i] = b
		} else {
			out[i] = '.'
		}
	}
	return string(out)
}
