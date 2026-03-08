// pkg/cmd/net.go - Network diagnostics commands
package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cbluth/bittorrent/pkg/cli"
	"github.com/cbluth/bittorrent/pkg/config"
)

// NetConfig carries global flags into network diagnostic subcommands.
type NetConfig struct {
	Port    uint
	Timeout time.Duration
}

var globalNetConfig *NetConfig

// RunNetCommands routes "bt net <subcommand> [args...]" to the appropriate handler.
func RunNetCommands(cfg *NetConfig, args []string) error {
	globalNetConfig = cfg

	router := cli.NewRouter("bt net")
	router.RegisterCommand(&cli.Command{Name: "ports", Aliases: []string{"list"}, Description: "NAT-PMP gateway discovery + port mapping", Run: CmdNetPorts})
	router.RegisterCommand(&cli.Command{Name: "nat-test", Aliases: []string{"nat"}, Description: "Detect NAT type via STUN", Run: CmdNetNATTest})
	router.RegisterCommand(&cli.Command{Name: "external-ip", Aliases: []string{"ip", "myip"}, Description: "Discover external IP address", Run: CmdNetExternalIP})
	router.RegisterCommand(&cli.Command{Name: "sniff", Aliases: []string{"capture"}, Description: "Packet capture (requires libpcap)", Run: CmdNetSniff})
	router.RegisterCommand(&cli.Command{Name: "latency", Aliases: []string{"ping"}, Description: "Measure RTT to peers or trackers", Run: CmdNetLatency})

	return router.Route(args)
}

// CmdNetPorts discovers the default gateway and queries external IP via NAT-PMP,
// then optionally requests a port mapping.
func CmdNetPorts(args []string) error {
	f := cli.NewCommandFlagSet(
		"ports", []string{"list"},
		"NAT-PMP gateway discovery + port mapping",
		[]string{"bt net ports [options]"},
	)
	gateway := f.String("gateway", "", "NAT gateway address (auto-detected if empty)")
	addTCP := f.Uint("add-tcp", 0, "Request a TCP port mapping for this local port (0 = disabled)")
	addUDP := f.Uint("add-udp", 0, "Request a UDP port mapping for this local port (0 = disabled)")
	lifetime := f.Duration("lifetime", 3600*time.Second, "Requested mapping lifetime")
	if err := f.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), globalNetConfig.Timeout)
	defer cancel()

	// Discover gateway.
	gw, err := resolveGateway(*gateway)
	if err != nil {
		return fmt.Errorf("gateway discovery: %w", err)
	}
	fmt.Printf("Gateway:     %s\n", gw)

	// Query external IP via NAT-PMP.
	extIP, err := natpmpExternalIP(ctx, gw)
	if err != nil {
		fmt.Printf("External IP: (NAT-PMP unavailable: %v)\n", err)
	} else {
		fmt.Printf("External IP: %s\n", extIP)
	}

	// Request mappings if asked.
	if *addTCP > 0 {
		ext, lt, err := natpmpMapPort(ctx, gw, 2, uint16(*addTCP), uint16(*addTCP), uint32(lifetime.Seconds()))
		if err != nil {
			fmt.Printf("TCP mapping: error: %v\n", err)
		} else {
			fmt.Printf("TCP mapping: %d → %d (lifetime %ds)\n", *addTCP, ext, lt)
		}
	}
	if *addUDP > 0 {
		ext, lt, err := natpmpMapPort(ctx, gw, 1, uint16(*addUDP), uint16(*addUDP), uint32(lifetime.Seconds()))
		if err != nil {
			fmt.Printf("UDP mapping: error: %v\n", err)
		} else {
			fmt.Printf("UDP mapping: %d → %d (lifetime %ds)\n", *addUDP, ext, lt)
		}
	}

	return nil
}

// CmdNetNATTest probes NAT type via STUN Binding Requests.
func CmdNetNATTest(args []string) error {
	f := cli.NewCommandFlagSet(
		"nat-test", []string{"nat"},
		"Detect NAT type via STUN",
		[]string{"bt net nat-test [options]"},
	)
	stunServer := f.String("stun", "", "STUN server address (default: from config)")
	stunServer2 := f.String("stun2", "", "Second STUN server for comparison (default: from config)")
	if err := f.Parse(args); err != nil {
		return err
	}

	// Resolve STUN servers from config if not overridden via flags
	servers := config.STUNServers()
	if *stunServer == "" {
		if len(servers) > 0 {
			*stunServer = servers[0]
		} else {
			return fmt.Errorf("no STUN servers configured (set stun.servers or stun.feed in config)")
		}
	}
	if *stunServer2 == "" {
		if len(servers) > 1 {
			*stunServer2 = servers[1]
		} else {
			*stunServer2 = *stunServer // fallback: use the same server
		}
	}

	timeout := globalNetConfig.Timeout

	// Open a single UDP socket.
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	fmt.Printf("Local:       %s\n", localAddr)

	// First STUN query.
	mapped1, err := stunQuery(conn, *stunServer, timeout)
	if err != nil {
		return fmt.Errorf("STUN query to %s: %w", *stunServer, err)
	}
	fmt.Printf("Mapped (1):  %s (via %s)\n", mapped1, *stunServer)

	// Second STUN query from the same socket to a different server.
	mapped2, err := stunQuery(conn, *stunServer2, timeout)
	if err != nil {
		fmt.Printf("Mapped (2):  error: %v (via %s)\n", err, *stunServer2)
		fmt.Println("\nNAT Type:    inconclusive (second STUN server unreachable)")
		return nil
	}
	fmt.Printf("Mapped (2):  %s (via %s)\n", mapped2, *stunServer2)

	// Classify.
	fmt.Println()
	if mapped1.IP.Equal(localAddr.IP) && mapped1.Port == localAddr.Port {
		fmt.Println("NAT Type:    Open Internet (no NAT)")
	} else if mapped1.String() == mapped2.String() {
		fmt.Println("NAT Type:    Cone NAT (same external mapping for different destinations)")
		fmt.Println("             Hole-punching: feasible")
	} else {
		fmt.Println("NAT Type:    Symmetric NAT (different external port per destination)")
		fmt.Println("             Hole-punching: difficult — rely on relay (BEP 55)")
	}

	return nil
}

// CmdNetExternalIP discovers and prints the external IP address.
func CmdNetExternalIP(args []string) error {
	f := cli.NewCommandFlagSet(
		"external-ip", []string{"ip", "myip"},
		"Discover external IP address",
		[]string{"bt net external-ip [options]"},
	)
	_ = f.Bool("announce", false, "Announce discovered IP to DHT")
	if err := f.Parse(args); err != nil {
		return err
	}

	timeout := globalNetConfig.Timeout
	client := &http.Client{Timeout: timeout}

	queries := []struct {
		label string
		url   string
	}{
		{"IPv4 (ipify)", "https://api4.ipify.org"},
		{"IPv6 (ipify)", "https://api6.ipify.org"},
	}

	any := false
	for _, q := range queries {
		resp, err := client.Get(q.url)
		if err != nil {
			fmt.Printf("%-20s error: %v\n", q.label, err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			fmt.Printf("%-20s error reading response: %v\n", q.label, err)
			continue
		}
		ip := strings.TrimSpace(string(body))
		fmt.Printf("%-20s %s\n", q.label, ip)
		any = true
	}
	if !any {
		return fmt.Errorf("could not determine external IP address")
	}
	return nil
}

// CmdNetSniff is not implemented — requires libpcap/gopacket which is a C dependency.
func CmdNetSniff(args []string) error {
	return fmt.Errorf("bt net sniff requires libpcap (gopacket) which is not bundled; use Wireshark or tcpdump with 'tcp port 6881' instead")
}

// CmdNetLatency measures round-trip latency to peers or tracker endpoints.
func CmdNetLatency(args []string) error {
	f := cli.NewCommandFlagSet(
		"latency", []string{"ping"},
		"Measure RTT to peers or trackers",
		[]string{"bt net latency [options] <host:port> [<host:port> ...]"},
	)
	count := f.Int("n", 5, "Number of pings per host")
	_ = f.Bool("udp", true, "Use UDP (DHT KRPC ping) instead of TCP connect")
	timeout := f.Duration("timeout", globalNetConfig.Timeout, "Per-ping timeout")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() == 0 {
		f.Usage()
		return fmt.Errorf("at least one host:port argument required")
	}

	for _, addr := range f.Args() {
		rtts := make([]float64, 0, *count)
		lost := 0
		fmt.Printf("PING %s (TCP connect) × %d:\n", addr, *count)
		for i := 0; i < *count; i++ {
			start := time.Now()
			conn, err := net.DialTimeout("tcp", addr, *timeout)
			if err != nil {
				fmt.Printf("  seq=%d timeout/error: %v\n", i+1, err)
				lost++
				continue
			}
			conn.Close()
			rtt := time.Since(start).Seconds() * 1000
			rtts = append(rtts, rtt)
			fmt.Printf("  seq=%d rtt=%.3f ms\n", i+1, rtt)
		}
		fmt.Printf("--- %s ping statistics ---\n", addr)
		sent := *count
		recv := sent - lost
		loss := 100 * lost / sent
		fmt.Printf("%d pings sent, %d received, %d%% loss\n", sent, recv, loss)
		if len(rtts) > 0 {
			min, avg, max, stddev := rttStats(rtts)
			fmt.Printf("rtt min/avg/max/stddev = %.3f/%.3f/%.3f/%.3f ms\n", min, avg, max, stddev)
		}
		fmt.Println()
	}
	return nil
}

// ── NAT-PMP helpers ────────────────────────────────────────────────────────

func resolveGateway(override string) (net.IP, error) {
	if override != "" {
		ip := net.ParseIP(override)
		if ip == nil {
			return nil, fmt.Errorf("invalid gateway IP: %s", override)
		}
		return ip, nil
	}
	return defaultGateway()
}

func defaultGateway() (net.IP, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Scan() // skip header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		if fields[1] != "00000000" {
			continue
		}
		gw, err := parseHexIP(fields[2])
		if err == nil {
			return gw, nil
		}
	}
	return nil, fmt.Errorf("no default route found in /proc/net/route")
}

func parseHexIP(hex string) (net.IP, error) {
	if len(hex) != 8 {
		return nil, fmt.Errorf("invalid hex IP: %s", hex)
	}
	var b [4]byte
	for i := 0; i < 4; i++ {
		_, err := fmt.Sscanf(hex[i*2:i*2+2], "%02x", &b[i])
		if err != nil {
			return nil, err
		}
	}
	// /proc/net/route stores gateway in little-endian — reverse for network order.
	return net.IPv4(b[3], b[2], b[1], b[0]), nil
}

func natpmpExternalIP(ctx context.Context, gateway net.IP) (net.IP, error) {
	addr := &net.UDPAddr{IP: gateway, Port: 5351}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)

	if _, err := conn.Write([]byte{0, 0}); err != nil { // version=0, opcode=0
		return nil, err
	}

	buf := make([]byte, 12)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	if n < 12 {
		return nil, fmt.Errorf("response too short: %d bytes", n)
	}
	if buf[1] != 0x80 {
		return nil, fmt.Errorf("unexpected opcode %d", buf[1])
	}
	if code := binary.BigEndian.Uint16(buf[2:4]); code != 0 {
		return nil, fmt.Errorf("NAT-PMP error %d", code)
	}
	return net.IPv4(buf[8], buf[9], buf[10], buf[11]), nil
}

// natpmpMapPort requests a port mapping. opcode: 1=UDP, 2=TCP.
// Returns external port and lifetime.
func natpmpMapPort(ctx context.Context, gateway net.IP, opcode byte, internalPort, externalPort uint16, lifetime uint32) (uint16, uint32, error) {
	addr := &net.UDPAddr{IP: gateway, Port: 5351}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)

	msg := make([]byte, 12)
	msg[0] = 0      // version
	msg[1] = opcode // 1=UDP, 2=TCP
	binary.BigEndian.PutUint16(msg[4:6], internalPort)
	binary.BigEndian.PutUint16(msg[6:8], externalPort)
	binary.BigEndian.PutUint32(msg[8:12], lifetime)

	if _, err := conn.Write(msg); err != nil {
		return 0, 0, err
	}

	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, 0, err
	}
	if n < 16 {
		return 0, 0, fmt.Errorf("response too short: %d bytes", n)
	}
	if code := binary.BigEndian.Uint16(buf[2:4]); code != 0 {
		return 0, 0, fmt.Errorf("NAT-PMP error %d", code)
	}
	extPort := binary.BigEndian.Uint16(buf[10:12])
	lt := binary.BigEndian.Uint32(buf[12:16])
	return extPort, lt, nil
}

// ── STUN helpers (RFC 5389) ─────────────────────────────────────────────────

// stunQuery sends a STUN Binding Request and returns the XOR-MAPPED-ADDRESS.
func stunQuery(conn net.PacketConn, server string, timeout time.Duration) (*net.UDPAddr, error) {
	addr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, err
	}

	// Build STUN Binding Request (RFC 5389 §6).
	// Header: type(2) + length(2) + magic(4) + txID(12) = 20 bytes.
	var req [20]byte
	binary.BigEndian.PutUint16(req[0:2], 0x0001)     // Binding Request
	binary.BigEndian.PutUint16(req[2:4], 0)          // length = 0 (no attributes)
	binary.BigEndian.PutUint32(req[4:8], 0x2112A442) // magic cookie
	rand.Read(req[8:20])                             // transaction ID

	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.WriteTo(req[:], addr); err != nil {
		return nil, err
	}

	buf := make([]byte, 512)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		return nil, err
	}
	if n < 20 {
		return nil, fmt.Errorf("STUN response too short: %d bytes", n)
	}

	// Verify magic cookie and transaction ID.
	if binary.BigEndian.Uint32(buf[4:8]) != 0x2112A442 {
		return nil, fmt.Errorf("STUN magic cookie mismatch")
	}
	for i := 8; i < 20; i++ {
		if buf[i] != req[i] {
			return nil, fmt.Errorf("STUN transaction ID mismatch")
		}
	}

	// Parse attributes to find XOR-MAPPED-ADDRESS (0x0020) or MAPPED-ADDRESS (0x0001).
	msgLen := int(binary.BigEndian.Uint16(buf[2:4]))
	if 20+msgLen > n {
		return nil, fmt.Errorf("STUN message truncated")
	}

	pos := 20
	for pos+4 <= 20+msgLen {
		attrType := binary.BigEndian.Uint16(buf[pos : pos+2])
		attrLen := int(binary.BigEndian.Uint16(buf[pos+2 : pos+4]))
		attrData := buf[pos+4 : pos+4+attrLen]

		if attrType == 0x0020 && attrLen >= 8 { // XOR-MAPPED-ADDRESS
			family := attrData[1]
			if family != 0x01 { // IPv4 only
				pos += 4 + ((attrLen + 3) &^ 3)
				continue
			}
			port := binary.BigEndian.Uint16(attrData[2:4]) ^ 0x2112
			ip := make(net.IP, 4)
			ip[0] = attrData[4] ^ 0x21
			ip[1] = attrData[5] ^ 0x12
			ip[2] = attrData[6] ^ 0xA4
			ip[3] = attrData[7] ^ 0x42
			return &net.UDPAddr{IP: ip, Port: int(port)}, nil
		}

		if attrType == 0x0001 && attrLen >= 8 { // MAPPED-ADDRESS (fallback)
			family := attrData[1]
			if family != 0x01 {
				pos += 4 + ((attrLen + 3) &^ 3)
				continue
			}
			port := binary.BigEndian.Uint16(attrData[2:4])
			ip := net.IPv4(attrData[4], attrData[5], attrData[6], attrData[7])
			return &net.UDPAddr{IP: ip, Port: int(port)}, nil
		}

		pos += 4 + ((attrLen + 3) &^ 3) // padded to 4-byte boundary
	}

	return nil, fmt.Errorf("no MAPPED-ADDRESS in STUN response")
}

// ── Stats ────────────────────────────────────────────────────────────────────

func rttStats(v []float64) (min, avg, max, stddev float64) {
	min = v[0]
	max = v[0]
	sum := 0.0
	for _, x := range v {
		sum += x
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
	}
	avg = sum / float64(len(v))
	variance := 0.0
	for _, x := range v {
		d := x - avg
		variance += d * d
	}
	stddev = math.Sqrt(variance / float64(len(v)))
	return
}
