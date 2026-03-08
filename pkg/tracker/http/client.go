package http

// HTTP Tracker Client (BEP 3 / BEP 7)
// https://www.bittorrent.org/beps/bep_0003.html
// https://www.bittorrent.org/beps/bep_0007.html
//
// Standalone HTTP tracker client with context support, compact/dictionary
// peer parsing, IPv4 + IPv6 (peers6), and scrape URL derivation.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cbluth/bittorrent/pkg/bencode"
	"github.com/cbluth/bittorrent/pkg/tracker"
)

// Client is an HTTP BitTorrent tracker client.
type Client struct {
	// Timeout for each request (default: 15s).
	Timeout time.Duration

	// UserAgent sent in the User-Agent header.
	UserAgent string

	httpClient *http.Client
}

// NewClient creates an HTTP tracker client with the given timeout.
func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		Timeout:    timeout,
		UserAgent:  "bt/1.0",
		httpClient: &http.Client{Timeout: timeout},
	}
}

// Announce sends an announce request to the HTTP tracker at trackerURL.
func (c *Client) Announce(ctx context.Context, trackerURL string, req *tracker.AnnounceRequest) (*tracker.AnnounceResponse, error) {
	params := url.Values{}
	params.Set("info_hash", string(req.InfoHash[:]))
	params.Set("peer_id", string(req.PeerID[:]))
	params.Set("port", strconv.Itoa(int(req.Port)))
	params.Set("uploaded", strconv.FormatInt(req.Uploaded, 10))
	params.Set("downloaded", strconv.FormatInt(req.Downloaded, 10))
	params.Set("left", strconv.FormatInt(req.Left, 10))
	params.Set("compact", "1") // always request compact; fall back to dict if needed
	if req.NoPeerID {
		params.Set("no_peer_id", "1")
	}
	if req.Event != tracker.EventNone {
		params.Set("event", string(req.Event))
	}
	if req.NumWant > 0 {
		params.Set("numwant", strconv.Itoa(req.NumWant))
	}

	fullURL := trackerURL + "?" + params.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if c.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker returned HTTP %d", resp.StatusCode)
	}

	return parseAnnounceResponse(resp.Body)
}

// Scrape sends a scrape request to the HTTP tracker.
// The scrape URL is derived from trackerURL by replacing "/announce" with "/scrape".
func (c *Client) Scrape(ctx context.Context, trackerURL string, req *tracker.ScrapeRequest) (*tracker.ScrapeResponse, error) {
	scrapeURL := scrapeURLFrom(trackerURL)

	params := url.Values{}
	for _, h := range req.InfoHashes {
		params.Add("info_hash", string(h[:]))
	}

	fullURL := scrapeURL + "?" + params.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if c.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker returned HTTP %d", resp.StatusCode)
	}

	return parseScrapeResponse(resp.Body)
}

// --- peer parsing helpers ---

// parsePeers handles both compact (binary string) and dictionary list formats.
func parsePeers(raw bencode.RawMessage) ([]tracker.Peer, error) {
	var compact string
	if err := bencode.DecodeBytes(raw, &compact); err == nil {
		return parseCompact4([]byte(compact))
	}
	var dict []struct {
		IP     string `bencode:"ip"`
		Port   int64  `bencode:"port"`
		PeerID string `bencode:"peer id"`
	}
	if err := bencode.DecodeBytes(raw, &dict); err != nil {
		return nil, fmt.Errorf("parse peers: %w", err)
	}
	peers := make([]tracker.Peer, len(dict))
	for i, p := range dict {
		peers[i] = tracker.Peer{IP: net.ParseIP(p.IP), Port: uint16(p.Port), ID: []byte(p.PeerID)}
	}
	return peers, nil
}

// parsePeers6 handles compact IPv6 (18 bytes per peer) or dictionary list.
func parsePeers6(raw bencode.RawMessage) ([]tracker.Peer, error) {
	var compact string
	if err := bencode.DecodeBytes(raw, &compact); err == nil {
		return parseCompact6([]byte(compact))
	}
	var dict []struct {
		IP   string `bencode:"ip"`
		Port int64  `bencode:"port"`
	}
	if err := bencode.DecodeBytes(raw, &dict); err != nil {
		return nil, fmt.Errorf("parse peers6: %w", err)
	}
	peers := make([]tracker.Peer, len(dict))
	for i, p := range dict {
		peers[i] = tracker.Peer{IP: net.ParseIP(p.IP), Port: uint16(p.Port)}
	}
	return peers, nil
}

// parseCompact4 decodes 6-byte compact IPv4 peer entries (4B IP + 2B port).
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

// parseCompact6 decodes 18-byte compact IPv6 peer entries (16B IP + 2B port).
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

// scrapeURLFrom derives the scrape URL from an announce URL (BEP 3).
func scrapeURLFrom(announceURL string) string {
	const suffix = "/announce"
	if len(announceURL) >= len(suffix) && announceURL[len(announceURL)-len(suffix):] == suffix {
		return announceURL[:len(announceURL)-len(suffix)] + "/scrape"
	}
	return announceURL // best-effort: try as-is
}

// parseAnnounceResponse decodes a bencoded tracker announce response.
func parseAnnounceResponse(r io.Reader) (*tracker.AnnounceResponse, error) {
	var raw struct {
		FailureReason string             `bencode:"failure reason"`
		Interval      int64              `bencode:"interval"`
		MinInterval   int64              `bencode:"min interval"`
		TrackerID     string             `bencode:"tracker id"`
		Complete      int64              `bencode:"complete"`
		Incomplete    int64              `bencode:"incomplete"`
		Peers         bencode.RawMessage `bencode:"peers"`
		Peers6        bencode.RawMessage `bencode:"peers6"`
		ExternalIP    string             `bencode:"external ip"` // BEP 24
	}
	if err := bencode.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if raw.FailureReason != "" {
		return nil, fmt.Errorf("tracker: %s", raw.FailureReason)
	}

	peers, err := parsePeers(raw.Peers)
	if err != nil {
		return nil, fmt.Errorf("parse peers: %w", err)
	}
	if len(raw.Peers6) > 0 {
		p6, err := parsePeers6(raw.Peers6)
		if err == nil {
			peers = append(peers, p6...)
		}
	}

	resp := &tracker.AnnounceResponse{
		Interval:    raw.Interval,
		MinInterval: raw.MinInterval,
		TrackerID:   raw.TrackerID,
		Complete:    raw.Complete,
		Incomplete:  raw.Incomplete,
		Peers:       peers,
	}
	// BEP 24: parse external IP (4-byte IPv4 or 16-byte IPv6 packed binary)
	if len(raw.ExternalIP) == 4 || len(raw.ExternalIP) == 16 {
		resp.ExternalIP = net.IP([]byte(raw.ExternalIP))
	}
	return resp, nil
}

// parseScrapeResponse decodes a bencoded tracker scrape response.
func parseScrapeResponse(r io.Reader) (*tracker.ScrapeResponse, error) {
	var raw struct {
		FailureReason string                      `bencode:"failure reason"`
		Files         map[string]map[string]int64 `bencode:"files"`
	}
	if err := bencode.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode scrape response: %w", err)
	}
	if raw.FailureReason != "" {
		return nil, fmt.Errorf("tracker: %s", raw.FailureReason)
	}

	resp := &tracker.ScrapeResponse{Files: make(map[string]tracker.ScrapeStats, len(raw.Files))}
	for ih, stats := range raw.Files {
		resp.Files[ih] = tracker.ScrapeStats{
			Complete:   stats["complete"],
			Downloaded: stats["downloaded"],
			Incomplete: stats["incomplete"],
		}
	}
	return resp, nil
}
