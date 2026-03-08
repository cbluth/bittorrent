package punch

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cbluth/bittorrent/pkg/config"
	"github.com/cbluth/bittorrent/pkg/log"
)

// DiscoverPublicIP discovers the host's public IPv4 address.
// It tries three methods in order: STUN (fastest, UDP), DNS, HTTP.
// Returns the first successful result.
func DiscoverPublicIP(ctx context.Context) (net.IP, error) {
	// STUN — fastest (single UDP round-trip)
	if ip, err := discoverViaSTUN(ctx); err == nil {
		return ip, nil
	}

	// DNS — fast (UDP to resolver, no TCP/TLS overhead)
	if ip, err := discoverViaDNS(ctx); err == nil {
		return ip, nil
	}

	// HTTP — slowest (TCP+TLS) but most universally available
	if ip, err := discoverViaHTTP(ctx); err == nil {
		return ip, nil
	}

	return nil, fmt.Errorf("all public IP discovery methods failed")
}

// STUN discovery (RFC 5389 Binding Request over UDP)

var defaultDNSServers = []struct {
	resolver string
	domain   string
}{
	{"resolver1.opendns.com", "myip.opendns.com"},
	{"resolver2.opendns.com", "myip.opendns.com"},
	{"ns1.google.com", "o-o.myaddr.l.google.com"},
	{"ns1-1.akamaitech.net", "whoami.akamai.net"},
}

var defaultHTTPEndpoints = []string{
	"https://api4.ipify.org",
	"https://ifconfig.me",
	"https://icanhazip.com",
	"https://geolocation-db.com/json/",
}

const perServerTimeout = 3 * time.Second

func discoverViaSTUN(ctx context.Context) (net.IP, error) {
	servers := config.STUNServers()
	if len(servers) == 0 {
		return nil, fmt.Errorf("no STUN servers configured")
	}

	for _, server := range servers {
		ip, err := stunBindingRequest(ctx, server)
		if err != nil {
			log.Debug("STUN failed", "sub", "punch", "server", server, "err", err)
			continue
		}
		log.Debug("public IP via STUN", "sub", "punch", "ip", ip, "server", server)
		return ip, nil
	}
	return nil, fmt.Errorf("all STUN servers failed")
}

func stunBindingRequest(ctx context.Context, server string) (net.IP, error) {
	addr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Build STUN Binding Request (RFC 5389 §6).
	var req [20]byte
	binary.BigEndian.PutUint16(req[0:2], 0x0001)     // Binding Request
	binary.BigEndian.PutUint16(req[2:4], 0)          // length = 0
	binary.BigEndian.PutUint32(req[4:8], 0x2112A442) // magic cookie
	rand.Read(req[8:20])                             // transaction ID

	deadline := time.Now().Add(perServerTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)

	if _, err := conn.WriteTo(req[:], addr); err != nil {
		return nil, err
	}

	buf := make([]byte, 1024)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		return nil, err
	}
	if n < 20 {
		return nil, fmt.Errorf("response too short: %d bytes", n)
	}

	// Verify response type (0x0101 = Binding Response), magic cookie, transaction ID.
	if binary.BigEndian.Uint16(buf[0:2]) != 0x0101 {
		return nil, fmt.Errorf("unexpected STUN message type: 0x%04x", binary.BigEndian.Uint16(buf[0:2]))
	}
	if binary.BigEndian.Uint32(buf[4:8]) != 0x2112A442 {
		return nil, fmt.Errorf("magic cookie mismatch")
	}
	for i := 8; i < 20; i++ {
		if buf[i] != req[i] {
			return nil, fmt.Errorf("transaction ID mismatch")
		}
	}

	// Parse attributes for XOR-MAPPED-ADDRESS or MAPPED-ADDRESS.
	msgLen := int(binary.BigEndian.Uint16(buf[2:4]))
	if 20+msgLen > n {
		return nil, fmt.Errorf("message truncated")
	}

	var fallbackIP net.IP
	pos := 20
	for pos+4 <= 20+msgLen {
		attrType := binary.BigEndian.Uint16(buf[pos : pos+2])
		attrLen := int(binary.BigEndian.Uint16(buf[pos+2 : pos+4]))
		if pos+4+attrLen > n {
			break
		}
		attrData := buf[pos+4 : pos+4+attrLen]

		switch {
		case attrType == 0x0020 && attrLen >= 8: // XOR-MAPPED-ADDRESS (preferred)
			if attrData[1] == 0x01 { // IPv4
				ip := make(net.IP, 4)
				ip[0] = attrData[4] ^ 0x21
				ip[1] = attrData[5] ^ 0x12
				ip[2] = attrData[6] ^ 0xA4
				ip[3] = attrData[7] ^ 0x42
				return ip, nil
			}
			if attrData[1] == 0x02 && attrLen >= 20 { // IPv6
				ip := make(net.IP, 16)
				for i := 0; i < 16; i++ {
					ip[i] = attrData[4+i] ^ req[4+i] // XOR with magic cookie + txn ID
				}
				return ip, nil
			}
		case attrType == 0x0001 && attrLen >= 8: // MAPPED-ADDRESS (fallback)
			if attrData[1] == 0x01 { // IPv4
				fallbackIP = net.IPv4(attrData[4], attrData[5], attrData[6], attrData[7])
			}
		}

		pos += 4 + ((attrLen + 3) &^ 3) // padded to 4-byte boundary
	}

	if fallbackIP != nil {
		return fallbackIP, nil
	}
	return nil, fmt.Errorf("no mapped address in STUN response")
}

// DNS discovery — queries special domains that return the querier's IP.

func discoverViaDNS(ctx context.Context) (net.IP, error) {
	for _, srv := range defaultDNSServers {
		ip, err := dnsLookupIP(ctx, srv.resolver, srv.domain)
		if err != nil {
			log.Debug("DNS failed", "sub", "punch", "resolver", srv.resolver, "domain", srv.domain, "err", err)
			continue
		}
		log.Debug("public IP via DNS", "sub", "punch", "ip", ip, "resolver", srv.resolver)
		return ip, nil
	}
	return nil, fmt.Errorf("all DNS servers failed")
}

func dnsLookupIP(ctx context.Context, resolver, domain string) (net.IP, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: perServerTimeout, FallbackDelay: -1}
			return d.DialContext(ctx, "udp4", resolver+":53")
		},
	}

	ctx, cancel := context.WithTimeout(ctx, perServerTimeout)
	defer cancel()

	addrs, err := r.LookupHost(ctx, domain)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if ip := net.ParseIP(strings.TrimSpace(a)); ip != nil {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("no valid IP in DNS response")
}

// HTTP discovery — GETs plain-text IP from well-known services.

func discoverViaHTTP(ctx context.Context) (net.IP, error) {
	client := &http.Client{Timeout: perServerTimeout}
	for _, endpoint := range defaultHTTPEndpoints {
		ip, err := httpGetIP(ctx, client, endpoint)
		if err != nil {
			log.Debug("HTTP failed", "sub", "punch", "url", endpoint, "err", err)
			continue
		}
		log.Debug("public IP via HTTP", "sub", "punch", "ip", ip, "url", endpoint)
		return ip, nil
	}
	return nil, fmt.Errorf("all HTTP endpoints failed")
}

func httpGetIP(ctx context.Context, client *http.Client, endpoint string) (net.IP, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "bt/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return nil, err
	}

	text := strings.TrimSpace(string(body))

	// Try plain-text IP first.
	if ip := net.ParseIP(text); ip != nil {
		return ip, nil
	}

	// Try JSON with an "IPv4" field (e.g. geolocation-db.com/json/).
	var obj struct {
		IPv4 string `json:"IPv4"`
	}
	if json.Unmarshal(body, &obj) == nil && obj.IPv4 != "" {
		if ip := net.ParseIP(obj.IPv4); ip != nil {
			return ip, nil
		}
	}

	return nil, fmt.Errorf("invalid IP in response: %q", text)
}
