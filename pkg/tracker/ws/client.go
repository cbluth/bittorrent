package ws

// Client — WebTorrent WebSocket tracker client with WebRTC peer establishment.
//
// Flow:
//  1. Dial the ws:// or wss:// tracker URL.
//  2. Send an announce carrying numWant outbound SDP offers (one PeerConnection each).
//  3. Loop on incoming messages:
//     - "offer" forwarded by the tracker: create answer SDP, reply via announce.
//     - "answer" for one of our pending offers: complete the PeerConnection.
//  4. When a WebRTC DataChannel opens, wrap it as a net.Conn and call OnConn.
//
// ICE gathering is done in "complete" mode (all candidates bundled in SDP) before
// sending, which is what the WebTorrent tracker protocol expects.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"
)

const defaultNumWant = 5

// defaultICEServers are public STUN servers used when none are configured.
var defaultICEServers = []webrtc.ICEServer{
	{URLs: []string{"stun:stun.l.google.com:19302"}},
	{URLs: []string{"stun:global.stun.twilio.com:3478"}},
}

// Client connects to a WebTorrent-compatible WebSocket tracker and establishes
// WebRTC DataChannel connections with peers in the swarm.
type Client struct {
	// URL is the ws:// or wss:// tracker announce URL.
	URL string

	// PeerID is our 20-byte local peer ID.
	PeerID [20]byte

	// InfoHash identifies the torrent swarm to join.
	InfoHash [20]byte

	// ICEServers for WebRTC NAT traversal (STUN/TURN).
	// Defaults to defaultICEServers when nil.
	ICEServers []webrtc.ICEServer

	// NumWant controls how many outbound offers are sent per announce.
	// Defaults to 5.
	NumWant int

	// OnConn is called for every established WebRTC DataChannel peer connection.
	// The net.Conn wraps the data channel. The caller must Close it when done.
	// Called from a goroutine; implementations must be goroutine-safe.
	OnConn func(conn net.Conn)

	mu      sync.Mutex
	pending map[string]*webrtc.PeerConnection // offerID → PC awaiting answer
}

// Run connects to the tracker and processes messages until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	if c.NumWant <= 0 {
		c.NumWant = defaultNumWant
	}
	if len(c.ICEServers) == 0 {
		c.ICEServers = defaultICEServers
	}
	backoff := time.Second
	for ctx.Err() == nil {
		if err := c.runSession(ctx); err != nil {
			log.Warn("tracker connection ended", "sub", "tracker", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > time.Minute {
			backoff = time.Minute
		}
	}
	return nil
}

func (c *Client) runSession(ctx context.Context) error {
	c.mu.Lock()
	for _, pc := range c.pending {
		pc.Close()
	}
	c.pending = make(map[string]*webrtc.PeerConnection)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		for _, pc := range c.pending {
			pc.Close()
		}
		c.pending = nil
		c.mu.Unlock()
	}()

	wsConn, _, err := websocket.Dial(ctx, c.URL, nil)
	if err != nil {
		return fmt.Errorf("ws dial %s: %w", c.URL, err)
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "")

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := c.sendAnnounce(sessionCtx, wsConn, "started"); err != nil {
		return fmt.Errorf("initial announce: %w", err)
	}

	intervals := make(chan time.Duration, 1)
	go func() {
		interval := 2 * time.Minute
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case interval = <-intervals:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(interval)
			case <-timer.C:
				if err := c.sendAnnounce(sessionCtx, wsConn, ""); err != nil {
					cancel()
					return
				}
				timer.Reset(interval)
			}
		}
	}()

	for {
		var raw json.RawMessage
		if err := wsjson.Read(sessionCtx, wsConn, &raw); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		if err := c.dispatch(sessionCtx, wsConn, raw, intervals); err != nil {
			log.Warn("dispatch error", "sub", "tracker", "err", err)
		}
	}
}

// sendAnnounce creates numWant PeerConnections, gathers ICE, and sends the
// announce message with all offers.
func (c *Client) sendAnnounce(ctx context.Context, wsConn *websocket.Conn, event string) error {
	offers := make([]RTCOffer, 0, c.NumWant)

	for range c.NumWant {
		pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
			ICEServers: c.ICEServers,
		})
		if err != nil {
			return fmt.Errorf("new peer connection: %w", err)
		}

		dc, err := pc.CreateDataChannel("webtorrent", nil)
		if err != nil {
			pc.Close()
			return fmt.Errorf("create data channel: %w", err)
		}
		c.hookDataChannel(dc, pc)

		sdpOffer, err := pc.CreateOffer(nil)
		if err != nil {
			pc.Close()
			return fmt.Errorf("create offer: %w", err)
		}

		// Wait for all ICE candidates before sending (non-trickle mode).
		gatherDone := webrtc.GatheringCompletePromise(pc)
		if err := pc.SetLocalDescription(sdpOffer); err != nil {
			pc.Close()
			return fmt.Errorf("set local description: %w", err)
		}
		select {
		case <-gatherDone:
		case <-ctx.Done():
			pc.Close()
			return ctx.Err()
		}

		var offerBytes [20]byte
		if _, err := rand.Read(offerBytes[:]); err != nil {
			pc.Close()
			return fmt.Errorf("generate offer ID: %w", err)
		}
		offerID := binaryText(offerBytes[:])
		c.mu.Lock()
		c.pending[offerID] = pc
		c.mu.Unlock()
		time.AfterFunc(50*time.Second, func() {
			c.mu.Lock()
			if c.pending[offerID] == pc {
				delete(c.pending, offerID)
				pc.Close()
			}
			c.mu.Unlock()
		})

		localSDP := pc.LocalDescription()
		sdpJSON, err := json.Marshal(localSDP)
		if err != nil {
			pc.Close()
			return fmt.Errorf("marshal SDP: %w", err)
		}
		offers = append(offers, RTCOffer{
			OfferID: offerID,
			Offer:   json.RawMessage(sdpJSON),
		})
	}

	req := AnnounceRequest{
		Action:   "announce",
		InfoHash: binaryString(c.InfoHash[:]),
		PeerID:   binaryString(c.PeerID[:]),
		Offers:   offers,
		NumWant:  c.NumWant,
		Event:    event,
	}
	return wsjson.Write(ctx, wsConn, req)
}

// dispatch routes an incoming tracker message to the right handler.
func (c *Client) dispatch(ctx context.Context, wsConn *websocket.Conn, raw json.RawMessage, intervals chan<- time.Duration) error {
	// Decode just enough to distinguish offer vs answer vs tracker response.
	var envelope struct {
		Action   string                     `json:"action"`
		Interval int                        `json:"interval"`
		Offer    *webrtc.SessionDescription `json:"offer"`
		Answer   *webrtc.SessionDescription `json:"answer"`
		OfferID  string                     `json:"offer_id"`
		PeerID   binaryString               `json:"peer_id"`
		InfoHash binaryString               `json:"info_hash"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("unmarshal envelope: %w", err)
	}

	switch {
	case envelope.Offer != nil:
		return c.handleOffer(ctx, wsConn, envelope.Offer, envelope.OfferID, []byte(envelope.PeerID))
	case envelope.Answer != nil:
		return c.handleAnswer(envelope.Answer, envelope.OfferID)
	default:
		if envelope.Action == "announce" && envelope.Interval > 0 {
			select {
			case intervals <- time.Duration(envelope.Interval) * time.Second:
			default:
			}
		}
		return nil
	}
}

// handleOffer creates an answer for an incoming WebRTC offer and sends it back.
func (c *Client) handleOffer(
	ctx context.Context,
	wsConn *websocket.Conn,
	offer *webrtc.SessionDescription,
	offerID string,
	fromPeerID []byte,
) error {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: c.ICEServers,
	})
	if err != nil {
		return fmt.Errorf("new peer connection: %w", err)
	}

	// Wire up incoming data channel (initiator creates it; we just receive it).
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		c.hookDataChannel(dc, pc)
	})

	if err := pc.SetRemoteDescription(*offer); err != nil {
		pc.Close()
		return fmt.Errorf("set remote description: %w", err)
	}

	sdpAnswer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return fmt.Errorf("create answer: %w", err)
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(sdpAnswer); err != nil {
		pc.Close()
		return fmt.Errorf("set local description: %w", err)
	}
	select {
	case <-gatherDone:
	case <-ctx.Done():
		pc.Close()
		return ctx.Err()
	}

	localSDP := pc.LocalDescription()
	answerJSON, err := json.Marshal(localSDP)
	if err != nil {
		pc.Close()
		return fmt.Errorf("marshal answer SDP: %w", err)
	}

	raw := json.RawMessage(answerJSON)
	req := answerRequest{
		Action:   "announce",
		InfoHash: binaryString(c.InfoHash[:]),
		PeerID:   binaryString(c.PeerID[:]),
		Answer:   &raw,
		OfferID:  offerID,
		ToPeerID: binaryString(fromPeerID),
	}
	return wsjson.Write(ctx, wsConn, req)
}

// handleAnswer completes a pending outbound PeerConnection with the remote SDP.
func (c *Client) handleAnswer(answer *webrtc.SessionDescription, offerID string) error {
	c.mu.Lock()
	pc, ok := c.pending[offerID]
	if ok {
		delete(c.pending, offerID)
	}
	c.mu.Unlock()

	if !ok {
		return fmt.Errorf("unknown offer_id %q", offerID)
	}
	return pc.SetRemoteDescription(*answer)
}

// hookDataChannel registers open/close/message handlers and calls OnConn when ready.
func (c *Client) hookDataChannel(dc *webrtc.DataChannel, pc *webrtc.PeerConnection) {
	dc.OnOpen(func() {
		conn := newDCConn(dc, pc)
		if c.OnConn != nil {
			go c.OnConn(conn)
		}
	})
}

func binaryText(b []byte) string {
	runes := make([]rune, len(b))
	for i, v := range b {
		runes[i] = rune(v)
	}
	return string(runes)
}

// --- DataChannel → net.Conn adapter ---

// dcConn wraps a pion DataChannel as a net.Conn using an io.Pipe for reads.
// Deadlines are not supported (data channels have no timeout primitives).
type dcConn struct {
	dc   *webrtc.DataChannel
	pc   *webrtc.PeerConnection
	pr   *io.PipeReader
	pw   *io.PipeWriter
	once sync.Once
}

func newDCConn(dc *webrtc.DataChannel, pc *webrtc.PeerConnection) *dcConn {
	pr, pw := io.Pipe()
	c := &dcConn{dc: dc, pc: pc, pr: pr, pw: pw}

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if _, err := pw.Write(msg.Data); err != nil {
			c.closePipe(err)
		}
	})
	dc.OnClose(func() {
		c.closePipe(io.EOF)
	})
	return c
}

func (c *dcConn) closePipe(err error) {
	c.once.Do(func() { c.pw.CloseWithError(err) })
}

func (c *dcConn) Read(b []byte) (int, error) { return c.pr.Read(b) }
func (c *dcConn) Write(b []byte) (int, error) {
	if err := c.dc.Send(b); err != nil {
		return 0, err
	}
	return len(b), nil
}
func (c *dcConn) Close() error {
	c.closePipe(io.EOF)
	_ = c.dc.Close()
	return c.pc.Close()
}
func (c *dcConn) LocalAddr() net.Addr                { return dcAddr("local") }
func (c *dcConn) RemoteAddr() net.Addr               { return dcAddr("remote") }
func (c *dcConn) SetDeadline(_ time.Time) error      { return nil }
func (c *dcConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *dcConn) SetWriteDeadline(_ time.Time) error { return nil }

type dcAddr string

func (a dcAddr) Network() string { return "webrtc" }
func (a dcAddr) String() string  { return string(a) }
