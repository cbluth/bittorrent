// Package lsd implements BEP 14: Local Service Discovery.
//
// LSD uses UDP multicast on the LAN to announce info hashes so nearby
// peers can connect directly without tracker or DHT round-trips.
package lsd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"
)

const (
	multicastAddr = "239.192.152.143:6771"
	multicastTTL  = 32
	maxPacketSize = 512
)

// Run starts LSD: announces the info hash periodically and listens for
// announcements from other peers on the LAN. Discovered peers are sent
// to the addPeers callback. Blocks until ctx is cancelled.
func Run(ctx context.Context, infoHash [20]byte, port uint16, addPeers func([]*net.TCPAddr)) {
	cookie := generateCookie()
	ihHex := hex.EncodeToString(infoHash[:])

	go announce(ctx, ihHex, port, cookie)
	listen(ctx, ihHex, port, cookie, addPeers)
}

// generateCookie returns a random hex string for self-suppression.
func generateCookie() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// buildPacket formats a BT-SEARCH announcement per BEP 14.
func buildPacket(infoHashHex string, port uint16, cookie string) []byte {
	return []byte(fmt.Sprintf(
		"BT-SEARCH * HTTP/1.1\r\nHost: %s\r\nPort: %d\r\nInfohash: %s\r\ncookie: %s\r\n\r\n",
		multicastAddr, port, infoHashHex, cookie,
	))
}

// parsedPacket holds the fields extracted from a BT-SEARCH announcement.
type parsedPacket struct {
	infoHash string
	port     uint16
	cookie   string
}

// parsePacket extracts info hash, port, and cookie from a BT-SEARCH packet.
// Returns nil if the packet is malformed.
func parsePacket(data []byte) *parsedPacket {
	s := string(data)
	if !strings.HasPrefix(s, "BT-SEARCH * HTTP/1.1\r\n") {
		return nil
	}

	var p parsedPacket
	for _, line := range strings.Split(s, "\r\n") {
		if k, v, ok := strings.Cut(line, ": "); ok {
			switch strings.ToLower(k) {
			case "infohash":
				v = strings.ToLower(v)
				if len(v) != 40 {
					return nil
				}
				p.infoHash = v
			case "port":
				n, err := strconv.ParseUint(v, 10, 16)
				if err != nil {
					return nil
				}
				p.port = uint16(n)
			case "cookie":
				p.cookie = v
			}
		}
	}

	if p.infoHash == "" || p.port == 0 {
		return nil
	}
	return &p
}

// announce sends BT-SEARCH multicast packets on an initial burst schedule
// (0s, 2s, 4s) then every 5 minutes.
func announce(ctx context.Context, infoHashHex string, port uint16, cookie string) {
	addr, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		log.Warn("LSD: resolve multicast addr failed", "sub", "lsd", "err", err)
		return
	}

	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		log.Warn("LSD: dial multicast failed", "sub", "lsd", "err", err)
		return
	}
	defer conn.Close()

	pkt := buildPacket(infoHashHex, port, cookie)

	// Initial burst: 0s, 2s, 4s.
	for i := 0; i < 3; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		if _, err := conn.Write(pkt); err != nil {
			log.Debug("LSD: announce write failed", "sub", "lsd", "err", err)
		} else {
			log.Debug("LSD: announced", "sub", "lsd", "hash", infoHashHex[:8])
		}
	}

	// Steady state: every 5 minutes.
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := conn.Write(pkt); err != nil {
				log.Debug("LSD: announce write failed", "sub", "lsd", "err", err)
			}
		}
	}
}

// listen joins the multicast group and dispatches discovered peers.
func listen(ctx context.Context, infoHashHex string, port uint16, cookie string, addPeers func([]*net.TCPAddr)) {
	addr, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		log.Warn("LSD: resolve multicast addr failed", "sub", "lsd", "err", err)
		return
	}

	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		log.Warn("LSD: listen multicast failed (multicast may not be supported)", "sub", "lsd", "err", err)
		return
	}
	defer conn.Close()

	log.Info("LSD: listening", "sub", "lsd", "addr", multicastAddr)

	buf := make([]byte, maxPacketSize)
	for {
		// Use a read deadline so we can check ctx periodically.
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Timeout is expected; just loop.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			log.Debug("LSD: read error", "sub", "lsd", "err", err)
			continue
		}

		p := parsePacket(buf[:n])
		if p == nil {
			continue
		}

		// Self-suppression: ignore our own announcements.
		if p.cookie == cookie {
			continue
		}

		// Only accept announcements for our info hash.
		if strings.ToLower(p.infoHash) != strings.ToLower(infoHashHex) {
			continue
		}

		peerAddr := &net.TCPAddr{
			IP:   src.IP,
			Port: int(p.port),
		}

		log.Info("LSD: discovered peer", "sub", "lsd", "peer", peerAddr, "hash", p.infoHash[:8])
		addPeers([]*net.TCPAddr{peerAddr})
	}
}
