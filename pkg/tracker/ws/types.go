package ws

import (
	"encoding/json"
	"fmt"
)

type binaryString []byte

func (b binaryString) MarshalJSON() ([]byte, error) {
	runes := make([]rune, len(b))
	for i, v := range b {
		runes[i] = rune(v)
	}
	return json.Marshal(string(runes))
}

func (b *binaryString) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	bytes := make([]byte, 0, len(value))
	for _, v := range value {
		if v > 255 {
			return fmt.Errorf("invalid binary string character: %U", v)
		}
		bytes = append(bytes, byte(v))
	}
	*b = bytes
	return nil
}

// AnnounceRequest represents a WebSocket tracker message (announce or scrape).
type AnnounceRequest struct {
	Action	string	`json:"action"`    // "announce" or "scrape"
	InfoHash	binaryString	`json:"info_hash"` // 20-byte binary info hash
	PeerID	binaryString	`json:"peer_id"`   // 20-byte binary peer ID
	Downloaded	int64	`json:"downloaded"`
	Left	int64	`json:"left"`
	Uploaded	int64	`json:"uploaded"`
	Event	string	`json:"event,omitempty"` // started, stopped, completed
	NumWant	int	`json:"numwant,omitempty"`
	Offers	[]RTCOffer	`json:"offers,omitempty"`     // outbound WebRTC offers
	Answer	*json.RawMessage	`json:"answer,omitempty"`     // WebRTC answer SDP
	OfferID	string	`json:"offer_id,omitempty"`   // matches answer to offer
	ToPeerID	binaryString	`json:"to_peer_id,omitempty"` // destination peer for answer routing
}

type answerRequest struct {
	Action	string	`json:"action"`
	InfoHash	binaryString	`json:"info_hash"`
	PeerID	binaryString	`json:"peer_id"`
	Answer	*json.RawMessage	`json:"answer"`
	OfferID	string	`json:"offer_id"`
	ToPeerID	binaryString	`json:"to_peer_id"`
}

// RTCOffer is a WebRTC SDP offer with a tracker-assigned identifier.
type RTCOffer struct {
	Offer   json.RawMessage `json:"offer"`
	OfferID string          `json:"offer_id"`
}

// AnnounceResponse is sent by the tracker after a successful announce.
type AnnounceResponse struct {
	Action	string	`json:"action"`
	Interval	int	`json:"interval"`
	InfoHash	binaryString	`json:"info_hash"`
	Complete	int	`json:"complete"`
	Incomplete	int	`json:"incomplete"`
}

// OfferMessage is forwarded by the tracker to a target peer.
type OfferMessage struct {
	Action	string	`json:"action"`
	Offer	json.RawMessage	`json:"offer"`
	OfferID	string	`json:"offer_id"`
	PeerID	binaryString	`json:"peer_id"`
	InfoHash	binaryString	`json:"info_hash"`
}

// AnswerMessage is forwarded by the tracker back to the offering peer.
type AnswerMessage struct {
	Action	string	`json:"action"`
	Answer	json.RawMessage	`json:"answer"`
	OfferID	string	`json:"offer_id"`
	PeerID	binaryString	`json:"peer_id"`
	InfoHash	binaryString	`json:"info_hash"`
}

// ErrorResponse signals a protocol error to the peer.
type ErrorResponse struct {
	Action	string	`json:"action,omitempty"`
	FailureReason	string	`json:"failure reason"`
	InfoHash	binaryString	`json:"info_hash,omitempty"`
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
