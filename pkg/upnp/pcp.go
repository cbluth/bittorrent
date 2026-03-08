package upnp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"
)

// PCP (Port Control Protocol, RFC 6887) — successor to NAT-PMP.
// Same port 5351, binary UDP, but version 2 with 12-byte nonce and IPv6 support.

const (
	pcpVersion = 2
	pcpOpMap   = 1
	// Protocol numbers per IANA.
	pcpProtoTCP = 6
	pcpProtoUDP = 17
)

// mapPortPCP maps a port via PCP (RFC 6887).
// Same contract as MapPort: returns external IP, port, cleanup, error.
func mapPortPCP(ctx context.Context, protocol string, internalPort int, description string) (net.IP, int, func(), error) {
	gateway, err := defaultGateway()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("default gateway: %w", err)
	}
	log.Debug("PCP gateway", "sub", "upnp", "gw", gateway)

	var proto byte
	switch protocol {
	case "TCP":
		proto = pcpProtoTCP
	case "UDP":
		proto = pcpProtoUDP
	default:
		return nil, 0, nil, fmt.Errorf("unsupported protocol %q", protocol)
	}

	localIP, err := localIPForGateway(gateway.String())
	if err != nil {
		return nil, 0, nil, fmt.Errorf("local IP: %w", err)
	}

	// Generate a 12-byte nonce for this mapping (replay protection).
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, 0, nil, fmt.Errorf("nonce: %w", err)
	}

	// Initial mapping.
	resp, err := pcpMapRequest(ctx, gateway, localIP, nonce, proto, uint16(internalPort), uint16(internalPort), leaseDuration)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("PCP MAP: %w", err)
	}
	externalPort := int(resp.externalPort)
	log.Info("PCP mapped", "sub", "upnp", "proto", protocol, "ext", externalPort, "int", internalPort, "lifetime", resp.lifetime)

	// Renewal + cleanup.
	renewCtx, cancel := context.WithCancel(ctx)
	cleanupDone := make(chan struct{})

	go func() {
		defer close(cleanupDone)
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := pcpMapRequest(renewCtx, gateway, localIP, nonce, proto, uint16(internalPort), uint16(externalPort), leaseDuration); err != nil {
					log.Warn("PCP renewal failed", "sub", "upnp", "proto", protocol, "err", err)
				} else {
					log.Debug("PCP renewed", "sub", "upnp", "proto", protocol, "port", externalPort)
				}
			case <-renewCtx.Done():
				// Delete: lifetime=0 per RFC 6887 §11.1.
				delCtx, delCancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, _ = pcpMapRequest(delCtx, gateway, localIP, nonce, proto, uint16(internalPort), uint16(externalPort), 0)
				delCancel()
				log.Debug("PCP mapping deleted", "sub", "upnp", "proto", protocol, "port", externalPort)
				return
			}
		}
	}()

	cleanupFn := func() {
		cancel()
		<-cleanupDone
	}

	return resp.externalIP, externalPort, cleanupFn, nil
}

// pcpMapResult holds a parsed PCP MAP response.
type pcpMapResult struct {
	externalPort uint16
	externalIP   net.IP
	lifetime     uint32
}

// pcpMapRequest sends a PCP MAP request and returns the response.
// Packet layout per RFC 6887 §7.1 (request) and §7.2 (response).
func pcpMapRequest(ctx context.Context, gateway, localIP net.IP, nonce [12]byte, proto byte, internalPort, externalPort uint16, lifetime uint32) (*pcpMapResult, error) {
	addr := &net.UDPAddr{IP: gateway, Port: natpmpPort} // PCP uses same port 5351
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Build PCP request: 24-byte header + 36-byte MAP opdata = 60 bytes.
	msg := make([]byte, 60)

	// Header (24 bytes).
	msg[0] = pcpVersion
	msg[1] = pcpOpMap // R=0 (request), opcode=1 (MAP)
	// msg[2:4] reserved
	binary.BigEndian.PutUint32(msg[4:8], lifetime)
	// Client IP as IPv4-mapped IPv6 (16 bytes at offset 8).
	clientIP := localIP.To4()
	if clientIP == nil {
		return nil, fmt.Errorf("PCP: IPv6 not supported")
	}
	// IPv4-mapped IPv6: ::ffff:a.b.c.d
	msg[18] = 0xff
	msg[19] = 0xff
	copy(msg[20:24], clientIP)

	// MAP opdata (36 bytes at offset 24).
	copy(msg[24:36], nonce[:]) // 12-byte nonce
	msg[36] = proto            // protocol number
	// msg[37:40] reserved
	binary.BigEndian.PutUint16(msg[40:42], internalPort)
	binary.BigEndian.PutUint16(msg[42:44], externalPort)
	// Suggested external address: IPv4-mapped IPv6 (16 bytes at offset 44).
	// All zeros = let server choose.

	buf := make([]byte, 60)
	for try := 0; try < natpmpTries; try++ {
		timeout := time.Duration(natpmpInitialMS<<try) * time.Millisecond
		deadline := time.Now().Add(timeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		conn.SetDeadline(deadline)

		if _, err := conn.Write(msg); err != nil {
			return nil, err
		}

		n, err := conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return nil, err
		}
		if n < 60 {
			return nil, fmt.Errorf("PCP response too short: %d bytes", n)
		}

		// Validate response header.
		if buf[0] != pcpVersion {
			return nil, fmt.Errorf("PCP unsupported version %d", buf[0])
		}
		if buf[1] != pcpOpMap|0x80 {
			return nil, fmt.Errorf("PCP unexpected opcode %d", buf[1])
		}
		resultCode := buf[3]
		if resultCode != 0 {
			return nil, fmt.Errorf("PCP error code %d (%s)", resultCode, pcpResultString(resultCode))
		}

		respLifetime := binary.BigEndian.Uint32(buf[4:8])
		// MAP opdata starts at offset 24.
		respExtPort := binary.BigEndian.Uint16(buf[42:44])
		// External IP at offset 44, 16 bytes (IPv4-mapped IPv6).
		var extIP net.IP
		if buf[58] == 0xff && buf[59] == 0xff {
			// Has an IPv4-mapped address in the last 4 bytes? Check [54:58] for ffff pattern.
			// Actually: bytes 44-59, IPv4-mapped = [10 zeros][0xff][0xff][4 bytes IPv4]
			extIP = net.IPv4(buf[56], buf[57], buf[58], buf[59])
		}
		// More robust: just check if there's an IPv4 mapped address
		extIPv6 := net.IP(buf[44:60])
		if v4 := extIPv6.To4(); v4 != nil {
			extIP = v4
		}

		return &pcpMapResult{
			externalPort: respExtPort,
			externalIP:   extIP,
			lifetime:     respLifetime,
		}, nil
	}

	return nil, fmt.Errorf("PCP request timed out after %d tries", natpmpTries)
}

// pcpResultString returns a human-readable name for PCP result codes.
func pcpResultString(code byte) string {
	names := [...]string{
		0:  "Success",
		1:  "UnsupportedVersion",
		2:  "NotAuthorized",
		3:  "MalformedRequest",
		4:  "UnsupportedOpcode",
		5:  "UnsupportedOption",
		6:  "MalformedOption",
		7:  "NetworkFailure",
		8:  "NoResources",
		9:  "UnsupportedProtocol",
		10: "UserExceededQuota",
		11: "CannotProvideExternal",
		12: "AddressMismatch",
		13: "ExcessiveRemotePeers",
	}
	if int(code) < len(names) {
		return names[code]
	}
	return fmt.Sprintf("Unknown(%d)", code)
}
