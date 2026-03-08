// pkg/cmd/fetch.go - Raw piece/block/range fetch commands
//
// "bt fetch" provides surgical access to torrent data without a full download session.
// It is the BitTorrent equivalent of "curl --range".
package cmd

import (
	"context"
	"crypto/sha1"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/cbluth/bittorrent/pkg/cli"
	bittorrent "github.com/cbluth/bittorrent/pkg/client"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/protocol"
)

// FetchConfig carries global state into fetch subcommands.
type FetchConfig struct {
	Port    uint
	Timeout time.Duration
	Client  *bittorrent.Client
}

var globalFetchConfig *FetchConfig

// RunFetchCommands routes "bt fetch <subcommand> [args...]" to the appropriate handler.
func RunFetchCommands(cfg *FetchConfig, args []string) error {
	globalFetchConfig = cfg

	router := cli.NewRouter("bt fetch")
	router.RegisterCommand(&cli.Command{Name: "piece", Aliases: []string{"p"}, Description: "Fetch a single piece by index, verify SHA-1", Run: CmdFetchPiece})
	router.RegisterCommand(&cli.Command{Name: "range", Aliases: []string{"r", "bytes"}, Description: "Fetch an arbitrary byte range from a torrent", Run: CmdFetchRange})

	return router.Route(args)
}

const blockSize = 16384 // 16 KiB — standard BT block size

// CmdFetchPiece fetches a single piece from the swarm by its piece index, verifies
// the SHA-1 hash, and writes the raw bytes to stdout or a file.
func CmdFetchPiece(args []string) error {
	f := flag.NewFlagSet("fetch piece", flag.ExitOnError)
	infoHash := f.String("infohash", "", "Info hash (40-char hex, required)")
	pieceIdx := f.Int("index", -1, "Piece index to fetch (required)")
	output := f.String("o", "", "Write piece bytes to this file (default: stdout)")
	timeout := f.Duration("timeout", 60*time.Second, "Fetch timeout")
	verify := f.Bool("verify", true, "Verify piece SHA-1 hash")
	f.Usage = func() {
		fmt.Fprintf(f.Output(), `bt fetch piece — fetch a single piece from the swarm by index

Usage:
  bt fetch piece [options]

Options:
`)
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if *infoHash == "" {
		return fmt.Errorf("-infohash is required")
	}
	if *pieceIdx < 0 {
		return fmt.Errorf("-index is required and must be >= 0")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Resolve metadata.
	ihKey, err := dht.KeyFromHex(*infoHash)
	if err != nil {
		return fmt.Errorf("invalid info hash: %w", err)
	}

	fmt.Fprintf(os.Stderr, "resolving metadata for %s...\n", *infoHash)
	info, err := globalFetchConfig.Client.ResolveMetadataFromHash(ctx, ihKey)
	if err != nil {
		return fmt.Errorf("metadata resolution: %w", err)
	}
	meta := info.GetMetaInfo()
	if meta == nil {
		return fmt.Errorf("no metainfo available")
	}

	numPieces := len(meta.Info.Pieces) / 20
	if *pieceIdx >= numPieces {
		return fmt.Errorf("piece index %d out of range (torrent has %d pieces)", *pieceIdx, numPieces)
	}

	// Determine piece length.
	pieceLen := int(meta.Info.PieceLength)
	if *pieceIdx == numPieces-1 {
		totalLen := meta.Info.TotalLength()
		lastLen := int(totalLen) - (*pieceIdx * pieceLen)
		if lastLen > 0 && lastLen < pieceLen {
			pieceLen = lastLen
		}
	}

	// Extract expected hash.
	var expectedHash [20]byte
	copy(expectedHash[:], meta.Info.Pieces[*pieceIdx*20:(*pieceIdx+1)*20])

	// Get peers.
	fmt.Fprintf(os.Stderr, "querying trackers for peers...\n")
	results, err := globalFetchConfig.Client.QueryTrackers(ctx, info)
	if err != nil {
		return fmt.Errorf("tracker query: %w", err)
	}
	peers := results.AllPeers()
	if len(peers) == 0 {
		return fmt.Errorf("no peers found")
	}
	fmt.Fprintf(os.Stderr, "found %d peers, fetching piece %d (%d bytes)...\n", len(peers), *pieceIdx, pieceLen)

	// Try peers until one works.
	peerID := globalFetchConfig.Client.PeerID()
	start := time.Now()
	var data []byte
	var usedPeer string
	for _, peer := range peers {
		if ctx.Err() != nil {
			break
		}
		addr := &net.TCPAddr{IP: peer.IP, Port: int(peer.Port)}
		d, err := fetchPieceFromPeer(ctx, addr, ihKey, peerID, uint32(*pieceIdx), uint32(pieceLen))
		if err != nil {
			continue
		}
		data = d
		usedPeer = addr.String()
		break
	}
	if data == nil {
		return fmt.Errorf("failed to fetch piece from any peer")
	}
	elapsed := time.Since(start)

	// Verify hash.
	if *verify {
		hash := sha1.Sum(data)
		if hash != expectedHash {
			return fmt.Errorf("SHA-1 mismatch: got %x, expected %x", hash, expectedHash)
		}
		fmt.Fprintf(os.Stderr, "SHA-1 verified OK\n")
	}

	fmt.Fprintf(os.Stderr, "piece %d fetched from %s in %v (%d bytes)\n", *pieceIdx, usedPeer, elapsed.Round(time.Millisecond), len(data))

	// Write output.
	if *output != "" {
		return os.WriteFile(*output, data, 0644)
	}
	_, err = os.Stdout.Write(data)
	return err
}

// CmdFetchRange fetches an arbitrary byte range from a torrent's logical byte space.
func CmdFetchRange(args []string) error {
	f := flag.NewFlagSet("fetch range", flag.ExitOnError)
	infoHash := f.String("infohash", "", "Info hash (40-char hex, required)")
	start := f.Int64("start", 0, "Start offset in logical byte space")
	length := f.Int64("length", -1, "Number of bytes to fetch (-1 = to end of torrent)")
	output := f.String("o", "", "Write bytes to this file (default: stdout)")
	timeout := f.Duration("timeout", 120*time.Second, "Fetch timeout")
	f.Usage = func() {
		fmt.Fprintf(f.Output(), `bt fetch range — fetch a byte range from the torrent's logical byte space

Usage:
  bt fetch range [options]

  The logical byte space is the concatenation of all torrent files in metainfo order.

Options:
`)
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if *infoHash == "" {
		return fmt.Errorf("-infohash is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	ihKey, err := dht.KeyFromHex(*infoHash)
	if err != nil {
		return fmt.Errorf("invalid info hash: %w", err)
	}

	fmt.Fprintf(os.Stderr, "resolving metadata for %s...\n", *infoHash)
	info, err := globalFetchConfig.Client.ResolveMetadataFromHash(ctx, ihKey)
	if err != nil {
		return fmt.Errorf("metadata resolution: %w", err)
	}
	meta := info.GetMetaInfo()
	if meta == nil {
		return fmt.Errorf("no metainfo available")
	}

	totalLen := meta.Info.TotalLength()
	pieceLen := int64(meta.Info.PieceLength)
	numPieces := len(meta.Info.Pieces) / 20

	if *start >= totalLen {
		return fmt.Errorf("start offset %d beyond torrent size %d", *start, totalLen)
	}
	fetchLen := *length
	if fetchLen < 0 || *start+fetchLen > totalLen {
		fetchLen = totalLen - *start
	}

	firstPiece := int(*start / pieceLen)
	lastPiece := int((*start + fetchLen - 1) / pieceLen)

	// Get peers.
	fmt.Fprintf(os.Stderr, "querying trackers for peers...\n")
	results, err := globalFetchConfig.Client.QueryTrackers(ctx, info)
	if err != nil {
		return fmt.Errorf("tracker query: %w", err)
	}
	peers := results.AllPeers()
	if len(peers) == 0 {
		return fmt.Errorf("no peers found")
	}

	fmt.Fprintf(os.Stderr, "fetching pieces %d-%d (%d bytes from offset %d)...\n", firstPiece, lastPiece, fetchLen, *start)

	peerID := globalFetchConfig.Client.PeerID()

	// Fetch each piece in the range.
	var assembled []byte
	for pi := firstPiece; pi <= lastPiece; pi++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Determine this piece's length.
		pLen := int(pieceLen)
		if pi == numPieces-1 {
			lastLen := int(totalLen) - pi*int(pieceLen)
			if lastLen > 0 && lastLen < pLen {
				pLen = lastLen
			}
		}

		var data []byte
		for _, peer := range peers {
			if ctx.Err() != nil {
				break
			}
			addr := &net.TCPAddr{IP: peer.IP, Port: int(peer.Port)}
			d, err := fetchPieceFromPeer(ctx, addr, ihKey, peerID, uint32(pi), uint32(pLen))
			if err != nil {
				continue
			}

			// Verify SHA-1.
			var expected [20]byte
			copy(expected[:], meta.Info.Pieces[pi*20:(pi+1)*20])
			hash := sha1.Sum(d)
			if hash != expected {
				continue
			}
			data = d
			break
		}
		if data == nil {
			return fmt.Errorf("failed to fetch piece %d from any peer", pi)
		}
		assembled = append(assembled, data...)
	}

	// Slice to exact range.
	offsetInFirst := int(*start) - firstPiece*int(pieceLen)
	result := assembled[offsetInFirst : offsetInFirst+int(fetchLen)]

	fmt.Fprintf(os.Stderr, "fetched %d bytes\n", len(result))

	if *output != "" {
		return os.WriteFile(*output, result, 0644)
	}
	_, err = os.Stdout.Write(result)
	return err
}

// fetchPieceFromPeer connects to a single peer, requests all blocks of the
// given piece, and returns the assembled data.
func fetchPieceFromPeer(ctx context.Context, addr *net.TCPAddr, infoHash, peerID dht.Key, pieceIdx, pieceLen uint32) ([]byte, error) {
	// Connect with short timeout.
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// BEP 3 handshake.
	hs := protocol.NewHandshake(infoHash, peerID)
	if _, err := conn.Write(hs.Serialize()); err != nil {
		return nil, err
	}
	peerHS, err := protocol.ReadHandshake(conn)
	if err != nil {
		return nil, err
	}
	if peerHS.InfoHash != infoHash {
		return nil, fmt.Errorf("info hash mismatch")
	}

	// Send interested.
	if err := protocol.WriteMessage(conn, protocol.NewInterestedMessage()); err != nil {
		return nil, err
	}

	// Read messages until unchoked, then request blocks.
	unchoked := false
	hasPiece := false
	buf := make([]byte, pieceLen)
	blocksReceived := make(map[uint32]bool)
	numBlocks := (pieceLen + blockSize - 1) / blockSize

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		msg, err := protocol.ReadMessage(conn, 15*time.Second)
		if err != nil {
			return nil, err
		}
		if msg == nil {
			continue // keep-alive
		}

		switch msg.ID {
		case protocol.MsgBitfield:
			bf, err := protocol.ParseBitfieldMessage(msg)
			if err == nil && protocol.HasPiece(bf.Bitfield, int(pieceIdx)) {
				hasPiece = true
			}
		case protocol.MsgHaveAll:
			hasPiece = true
		case protocol.MsgHave:
			have, err := protocol.ParseHaveMessage(msg)
			if err == nil && have.PieceIndex == pieceIdx {
				hasPiece = true
			}
		case protocol.MsgUnchoke:
			unchoked = true
		case protocol.MsgPiece:
			p, err := protocol.ParsePieceMessage(msg)
			if err != nil || p.Index != pieceIdx {
				continue
			}
			copy(buf[p.Begin:], p.Block)
			blocksReceived[p.Begin] = true
			if uint32(len(blocksReceived)) == numBlocks {
				return buf, nil
			}
		case protocol.MsgReject:
			rej, err := protocol.ParseRejectMessage(msg)
			if err == nil && rej.Index == pieceIdx {
				return nil, fmt.Errorf("request rejected")
			}
		case protocol.MsgChoke:
			return nil, fmt.Errorf("choked")
		}

		// Once unchoked and we know peer has the piece, send requests.
		if unchoked && hasPiece && len(blocksReceived) == 0 {
			for i := uint32(0); i < numBlocks; i++ {
				begin := i * blockSize
				bLen := uint32(blockSize)
				if i == numBlocks-1 {
					bLen = pieceLen - begin
				}
				if err := protocol.WriteMessage(conn, protocol.NewRequestMessage(pieceIdx, begin, bLen)); err != nil {
					return nil, err
				}
			}
		}
	}
}
