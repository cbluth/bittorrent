package ws

import "encoding/json"

// AnnounceRequest represents a WebSocket tracker message (announce or scrape).
type AnnounceRequest struct {
	Action     string           `json:"action"`    // "announce" or "scrape"
	InfoHash   []byte           `json:"info_hash"` // 20-byte binary info hash
	PeerID     []byte           `json:"peer_id"`   // 20-byte binary peer ID
	Downloaded int64            `json:"downloaded"`
	Left       int64            `json:"left"`
	Uploaded   int64            `json:"uploaded"`
	Event      string           `json:"event"` // started, stopped, completed
	NumWant    int              `json:"numwant"`
	Offers     []RTCOffer       `json:"offers"`     // outbound WebRTC offers
	Answer     *json.RawMessage `json:"answer"`     // WebRTC answer SDP
	OfferID    string           `json:"offer_id"`   // matches answer to offer
	ToPeerID   []byte           `json:"to_peer_id"` // destination peer for answer routing
}

// RTCOffer is a WebRTC SDP offer with a tracker-assigned identifier.
type RTCOffer struct {
	Offer   json.RawMessage `json:"offer"`
	OfferID string          `json:"offer_id"`
}

// AnnounceResponse is sent by the tracker after a successful announce.
type AnnounceResponse struct {
	Action     string `json:"action"`
	Interval   int    `json:"interval"`
	InfoHash   []byte `json:"info_hash"`
	Complete   int    `json:"complete"`
	Incomplete int    `json:"incomplete"`
}

// OfferMessage is forwarded by the tracker to a target peer.
type OfferMessage struct {
	Action   string          `json:"action"`
	Offer    json.RawMessage `json:"offer"`
	OfferID  string          `json:"offer_id"`
	PeerID   []byte          `json:"peer_id"`
	InfoHash []byte          `json:"info_hash"`
}

// AnswerMessage is forwarded by the tracker back to the offering peer.
type AnswerMessage struct {
	Action   string          `json:"action"`
	Answer   json.RawMessage `json:"answer"`
	OfferID  string          `json:"offer_id"`
	PeerID   []byte          `json:"peer_id"`
	InfoHash []byte          `json:"info_hash"`
}

// ErrorResponse signals a protocol error to the peer.
type ErrorResponse struct {
	Action        string `json:"action,omitempty"`
	FailureReason string `json:"failure reason"`
	InfoHash      []byte `json:"info_hash,omitempty"`
}

// ScrapeResponse is sent in reply to a scrape request.
type ScrapeResponse struct {
	Action string                 `json:"action"`
	Files  map[string]ScrapeStats `json:"files"`
}

// ScrapeStats holds per-torrent seeder/leecher/completed counts.
type ScrapeStats struct {
	Complete   int `json:"complete"`
	Incomplete int `json:"incomplete"`
	Downloaded int `json:"downloaded"`
}
