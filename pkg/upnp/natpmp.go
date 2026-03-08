package upnp

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"
)

const (
	natpmpPort      = 5351
	natpmpVersion   = 0
	natpmpOpUDP     = 1
	natpmpOpTCP     = 2
	natpmpTries     = 4             // fewer retries than RFC (faster failure)
	natpmpInitialMS = 250           // initial retry interval
	natpmpLifetime  = leaseDuration // reuse the 3600s constant
)

// mapPortNATPMP maps a port via NAT-PMP (RFC 6886).
// Same contract as MapPort: returns external IP, port, cleanup, error.
func mapPortNATPMP(ctx context.Context, protocol string, internalPort int, description string) (net.IP, int, func(), error) {
	gateway, err := defaultGateway()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("default gateway: %w", err)
	}
	log.Debug("NAT-PMP gateway", "sub", "upnp", "gw", gateway)

	var opcode byte
	switch protocol {
	case "TCP":
		opcode = natpmpOpTCP
	case "UDP":
		opcode = natpmpOpUDP
	default:
		return nil, 0, nil, fmt.Errorf("unsupported protocol %q", protocol)
	}

	// Initial mapping.
	result, err := natpmpRequest(ctx, gateway, opcode, uint16(internalPort), uint16(internalPort), natpmpLifetime)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("NAT-PMP add mapping: %w", err)
	}
	externalPort := int(result.mappedPort)
	log.Info("NAT-PMP mapped", "sub", "upnp", "proto", protocol, "ext", externalPort, "int", internalPort, "lifetime", result.lifetime)

	// Get external IP.
	extIP, _ := natpmpGetExternalIP(ctx, gateway)

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
				if _, err := natpmpRequest(renewCtx, gateway, opcode, uint16(internalPort), uint16(externalPort), natpmpLifetime); err != nil {
					log.Warn("NAT-PMP renewal failed", "sub", "upnp", "proto", protocol, "err", err)
				} else {
					log.Debug("NAT-PMP renewed", "sub", "upnp", "proto", protocol, "port", externalPort)
				}
			case <-renewCtx.Done():
				// Delete mapping: lifetime=0, requestedExternalPort=0 per RFC 6886 §3.4.
				delCtx, delCancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, _ = natpmpRequest(delCtx, gateway, opcode, uint16(internalPort), 0, 0)
				delCancel()
				log.Debug("NAT-PMP mapping deleted", "sub", "upnp", "proto", protocol, "port", externalPort)
				return
			}
		}
	}()

	cleanupFn := func() {
		cancel()
		<-cleanupDone
	}

	return extIP, externalPort, cleanupFn, nil
}

// natpmpResult holds a parsed NAT-PMP mapping response.
type natpmpResult struct {
	mappedPort uint16
	lifetime   uint32
}

// natpmpRequest sends a NAT-PMP port mapping request with exponential backoff retries.
func natpmpRequest(ctx context.Context, gateway net.IP, opcode byte, internalPort, externalPort uint16, lifetime uint32) (*natpmpResult, error) {
	addr := &net.UDPAddr{IP: gateway, Port: natpmpPort}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Build 12-byte request: version(1) + opcode(1) + reserved(2) + internal(2) + external(2) + lifetime(4).
	msg := make([]byte, 12)
	msg[0] = natpmpVersion
	msg[1] = opcode
	// msg[2:4] reserved
	binary.BigEndian.PutUint16(msg[4:6], internalPort)
	binary.BigEndian.PutUint16(msg[6:8], externalPort)
	binary.BigEndian.PutUint32(msg[8:12], lifetime)

	buf := make([]byte, 16)
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
		if n < 16 {
			return nil, fmt.Errorf("NAT-PMP response too short: %d bytes", n)
		}

		// Validate response.
		if buf[0] != 0 {
			return nil, fmt.Errorf("NAT-PMP unsupported version %d", buf[0])
		}
		if buf[1] != opcode|0x80 {
			return nil, fmt.Errorf("NAT-PMP unexpected opcode %d, expected %d", buf[1], opcode|0x80)
		}
		resultCode := binary.BigEndian.Uint16(buf[2:4])
		if resultCode != 0 {
			return nil, fmt.Errorf("NAT-PMP error code %d", resultCode)
		}

		return &natpmpResult{
			mappedPort: binary.BigEndian.Uint16(buf[10:12]),
			lifetime:   binary.BigEndian.Uint32(buf[12:16]),
		}, nil
	}

	return nil, fmt.Errorf("NAT-PMP request timed out after %d tries", natpmpTries)
}

// natpmpGetExternalIP sends the NAT-PMP external address request (opcode 0).
func natpmpGetExternalIP(ctx context.Context, gateway net.IP) (net.IP, error) {
	addr := &net.UDPAddr{IP: gateway, Port: natpmpPort}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	msg := []byte{natpmpVersion, 0} // version 0, opcode 0
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)

	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}

	buf := make([]byte, 12)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	if n < 12 {
		return nil, fmt.Errorf("NAT-PMP external IP response too short: %d bytes", n)
	}
	if buf[0] != 0 || buf[1] != 0x80 {
		return nil, fmt.Errorf("NAT-PMP unexpected response version=%d opcode=%d", buf[0], buf[1])
	}
	resultCode := binary.BigEndian.Uint16(buf[2:4])
	if resultCode != 0 {
		return nil, fmt.Errorf("NAT-PMP error code %d", resultCode)
	}

	return net.IPv4(buf[8], buf[9], buf[10], buf[11]), nil
}

// defaultGateway reads /proc/net/route to find the default gateway IP.
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
		// Default route: Destination == 00000000.
		if fields[1] != "00000000" {
			continue
		}
		// Gateway is a hex-encoded little-endian uint32.
		var gw uint32
		if _, err := fmt.Sscanf(fields[2], "%x", &gw); err != nil {
			continue
		}
		ip := net.IPv4(byte(gw), byte(gw>>8), byte(gw>>16), byte(gw>>24))
		return ip, nil
	}

	return nil, fmt.Errorf("no default gateway found in /proc/net/route")
}
