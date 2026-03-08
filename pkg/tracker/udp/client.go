package udp

// UDP Tracker Client (BEP 15)
// https://www.bittorrent.org/beps/bep_0015.html
//
// Implements the two-step UDP tracker protocol:
//   1. Connect   — send magic + random txID; receive connection_id (valid 2 min)
//   2. Announce  — send connection_id + announce params; receive peer list
//   2. Scrape    — send connection_id + info hashes; receive per-torrent stats
//
// Context is respected for the dial and for read deadlines.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"time"

	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/tracker"
)

const (
	connectMagic    uint64 = 0x41727101980 // BEP 15 protocol magic
	actionConnect   uint32 = 0
	actionAnnounce  uint32 = 1
	actionScrape    uint32 = 2
	actionError     uint32 = 3
	maxScrapeHashes        = 74 // BEP 15: max info hashes per UDP scrape
)

// Client is a UDP BitTorrent tracker client (BEP 15).
type Client struct {
	// Timeout for each round-trip (connect + announce/scrape). Default: 15s.
	Timeout time.Duration
}

// NewClient returns a UDP tracker client with the given per-request timeout.
func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{Timeout: timeout}
}

// Announce sends an announce to the UDP tracker at addr ("host:port").
func (c *Client) Announce(ctx context.Context, addr string, req *tracker.AnnounceRequest) (*tracker.AnnounceResponse, error) {
	conn, err := c.dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	connID, err := c.connect(conn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return c.announce(conn, connID, req)
}

// Scrape sends a scrape to the UDP tracker at addr.
func (c *Client) Scrape(ctx context.Context, addr string, req *tracker.ScrapeRequest) (*tracker.ScrapeResponse, error) {
	if len(req.InfoHashes) > maxScrapeHashes {
		return nil, fmt.Errorf("too many info hashes (max %d for UDP)", maxScrapeHashes)
	}
	conn, err := c.dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	connID, err := c.connect(conn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return c.scrape(conn, connID, req)
}

func (c *Client) dial(ctx context.Context, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: c.Timeout}
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial udp %s: %w", addr, err)
	}
	conn.SetDeadline(time.Now().Add(c.Timeout))
	return conn, nil
}

// connect performs the BEP 15 connect handshake and returns the connection ID.
func (c *Client) connect(conn net.Conn) (uint64, error) {
	txID := rand.Uint32()

	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, connectMagic)
	binary.Write(&buf, binary.BigEndian, actionConnect)
	binary.Write(&buf, binary.BigEndian, txID)

	if _, err := conn.Write(buf.Bytes()); err != nil {
		return 0, fmt.Errorf("write connect: %w", err)
	}

	resp := make([]byte, 16)
	if _, err := conn.Read(resp); err != nil {
		return 0, fmt.Errorf("read connect response: %w", err)
	}

	r := bytes.NewReader(resp)
	var action, respTxID uint32
	var connID uint64
	binary.Read(r, binary.BigEndian, &action)
	binary.Read(r, binary.BigEndian, &respTxID)
	binary.Read(r, binary.BigEndian, &connID)

	if action == actionError {
		return 0, fmt.Errorf("tracker error: %s", resp[8:])
	}
	if action != actionConnect {
		return 0, fmt.Errorf("unexpected action %d in connect response", action)
	}
	if respTxID != txID {
		return 0, errors.New("transaction ID mismatch in connect response")
	}
	return connID, nil
}

func (c *Client) announce(conn net.Conn, connID uint64, req *tracker.AnnounceRequest) (*tracker.AnnounceResponse, error) {
	txID := rand.Uint32()

	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, connID)
	binary.Write(&buf, binary.BigEndian, actionAnnounce)
	binary.Write(&buf, binary.BigEndian, txID)
	buf.Write(req.InfoHash[:])
	buf.Write(req.PeerID[:])
	binary.Write(&buf, binary.BigEndian, req.Downloaded)
	binary.Write(&buf, binary.BigEndian, req.Left)
	binary.Write(&buf, binary.BigEndian, req.Uploaded)
	binary.Write(&buf, binary.BigEndian, uint32(eventCode(req.Event)))
	binary.Write(&buf, binary.BigEndian, uint32(0))     // IP (default)
	binary.Write(&buf, binary.BigEndian, rand.Uint32()) // key
	binary.Write(&buf, binary.BigEndian, int32(req.NumWant))
	binary.Write(&buf, binary.BigEndian, req.Port)

	if _, err := conn.Write(buf.Bytes()); err != nil {
		return nil, fmt.Errorf("write announce: %w", err)
	}

	resp := make([]byte, 1024)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, fmt.Errorf("read announce response: %w", err)
	}
	return parseAnnounceResponse(resp[:n], txID)
}

func (c *Client) scrape(conn net.Conn, connID uint64, req *tracker.ScrapeRequest) (*tracker.ScrapeResponse, error) {
	txID := rand.Uint32()

	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, connID)
	binary.Write(&buf, binary.BigEndian, actionScrape)
	binary.Write(&buf, binary.BigEndian, txID)
	for _, h := range req.InfoHashes {
		buf.Write(h[:])
	}

	if _, err := conn.Write(buf.Bytes()); err != nil {
		return nil, fmt.Errorf("write scrape: %w", err)
	}

	resp := make([]byte, 8+12*len(req.InfoHashes)+64)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, fmt.Errorf("read scrape response: %w", err)
	}
	return parseScrapeResponse(resp[:n], txID, req.InfoHashes)
}

func parseAnnounceResponse(data []byte, expectedTxID uint32) (*tracker.AnnounceResponse, error) {
	if len(data) < 20 {
		return nil, errors.New("announce response too short")
	}
	r := bytes.NewReader(data)
	var action, txID uint32
	var interval, leechers, seeders int32
	binary.Read(r, binary.BigEndian, &action)
	binary.Read(r, binary.BigEndian, &txID)

	if action == actionError {
		return nil, fmt.Errorf("tracker error: %s", data[8:])
	}
	if action != actionAnnounce {
		return nil, fmt.Errorf("unexpected action %d in announce response", action)
	}
	if txID != expectedTxID {
		return nil, errors.New("transaction ID mismatch in announce response")
	}

	binary.Read(r, binary.BigEndian, &interval)
	binary.Read(r, binary.BigEndian, &leechers)
	binary.Read(r, binary.BigEndian, &seeders)

	peerData := data[20:]
	var peers []tracker.Peer
	var err error
	switch {
	case len(peerData) == 0:
		// no peers
	case len(peerData)%18 == 0:
		peers, err = parseCompact6(peerData)
	case len(peerData)%6 == 0:
		peers, err = parseCompact4(peerData)
	default:
		return nil, fmt.Errorf("invalid peer data length %d", len(peerData))
	}
	if err != nil {
		return nil, err
	}

	return &tracker.AnnounceResponse{
		Interval:   int64(interval),
		Complete:   int64(seeders),
		Incomplete: int64(leechers),
		Peers:      peers,
	}, nil
}

func parseScrapeResponse(data []byte, expectedTxID uint32, hashes []dht.Key) (*tracker.ScrapeResponse, error) {
	if len(data) < 8 {
		return nil, errors.New("scrape response too short")
	}
	r := bytes.NewReader(data)
	var action, txID uint32
	binary.Read(r, binary.BigEndian, &action)
	binary.Read(r, binary.BigEndian, &txID)

	if action == actionError {
		return nil, fmt.Errorf("tracker error: %s", data[8:])
	}
	if action != actionScrape {
		return nil, fmt.Errorf("unexpected action %d in scrape response", action)
	}
	if txID != expectedTxID {
		return nil, errors.New("transaction ID mismatch in scrape response")
	}

	stats := data[8:]
	n := min(len(stats)/12, len(hashes))
	resp := &tracker.ScrapeResponse{Files: make(map[string]tracker.ScrapeStats, n)}
	for i := range n {
		off := i * 12
		if off+12 > len(stats) {
			break
		}
		var seeders, completed, leechers int32
		sr := bytes.NewReader(stats[off : off+12])
		binary.Read(sr, binary.BigEndian, &seeders)
		binary.Read(sr, binary.BigEndian, &completed)
		binary.Read(sr, binary.BigEndian, &leechers)
		resp.Files[fmt.Sprintf("%x", hashes[i][:])] = tracker.ScrapeStats{
			Complete:   int64(seeders),
			Downloaded: int64(completed),
			Incomplete: int64(leechers),
		}
	}
	return resp, nil
}

// parseCompact4 decodes 6-byte compact IPv4 entries.
func parseCompact4(b []byte) ([]tracker.Peer, error) {
	if len(b)%6 != 0 {
		return nil, errors.New("compact peers4: length not multiple of 6")
	}
	peers := make([]tracker.Peer, len(b)/6)
	for i := range peers {
		off := i * 6
		peers[i] = tracker.Peer{
			IP:   net.IP(append([]byte(nil), b[off:off+4]...)),
			Port: binary.BigEndian.Uint16(b[off+4 : off+6]),
		}
	}
	return peers, nil
}

// parseCompact6 decodes 18-byte compact IPv6 entries.
func parseCompact6(b []byte) ([]tracker.Peer, error) {
	if len(b)%18 != 0 {
		return nil, errors.New("compact peers6: length not multiple of 18")
	}
	peers := make([]tracker.Peer, len(b)/18)
	for i := range peers {
		off := i * 18
		peers[i] = tracker.Peer{
			IP:   net.IP(append([]byte(nil), b[off:off+16]...)),
			Port: binary.BigEndian.Uint16(b[off+16 : off+18]),
		}
	}
	return peers, nil
}

func eventCode(e tracker.Event) int {
	switch e {
	case tracker.EventCompleted:
		return 1
	case tracker.EventStarted:
		return 2
	case tracker.EventStopped:
		return 3
	default:
		return 0
	}
}
