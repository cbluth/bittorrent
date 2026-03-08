# bittorrent

Go BitTorrent library and CLI. Download, stream, inspect, and serve torrents.

```bash
go get github.com/cbluth/bittorrent
# or
go install github.com/cbluth/bittorrent/cmd/bt@latest
```

## CLI

See [cmd/bt/Readme.md](cmd/bt/Readme.md) for full CLI documentation.

```bash
go install github.com/cbluth/bittorrent/cmd/bt@latest

bt download "magnet:?xt=urn:btih:..."     # download
bt server                                  # HTTP streaming server
bt magnet <hash>                           # resolve metadata
bt dht get-peers <hash>                    # DHT peer lookup
bt tracker announce <url> <hash>           # tracker announce
bt torrent file.torrent                    # inspect .torrent
```

## Library Usage

```bash
go get github.com/cbluth/bittorrent
```

### Resolve metadata from an info hash

```go
c, _ := client.NewClient(&client.Config{
    Port:     6881,
    Trackers: []string{"udp://tracker.opentrackr.org:1337/announce"},
})

ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
defer cancel()

info, err := c.ResolveMetadata(ctx, "dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c")
fmt.Println(info.Name, info.TotalLength, info.NumPieces)
for _, f := range info.Files {
    fmt.Println(f.Path, f.Length)
}
```

### Parse a magnet link

```go
mag, _ := c.ParseMagnet("magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c&dn=Big+Buck+Bunny")
fmt.Println(mag.InfoHash.Hex(), mag.DisplayName, mag.Trackers)
```

### Load a .torrent file

```go
f, _ := os.Open("file.torrent")
info, _ := c.LoadTorrent(f)
fmt.Println(info.InfoHash.Hex(), info.Name, info.PieceLength, info.NumPieces)
```

### Query trackers

```go
results, _ := c.QueryTrackers(ctx, info)
fmt.Println("seeders:", results.TotalSeeders(), "leechers:", results.TotalLeechers())
for _, p := range results.AllPeers() {
    fmt.Println(p.IP, p.Port)
}
```

### DHT peer discovery

```go
node, _ := dht.NewNode(6881, []string{
    "router.bittorrent.com:6881",
    "dht.transmissionbt.com:6881",
})
defer node.Shutdown()

ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
defer cancel()
node.Bootstrap(ctx, 100)

key, _ := dht.KeyFromHex("dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c")
peers, _ := node.FindPeersForInfoHash(key, 50, 16)
for _, addr := range peers {
    fmt.Println(addr)
}
```

### Download a torrent

```go
info, _ := c.LoadTorrent(f)
meta := info.GetMetaInfo()

// Extract piece hashes
var hashes []dht.Key
for i := 0; i < len(meta.Info.Pieces); i += 20 {
    var h dht.Key
    copy(h[:], meta.Info.Pieces[i:i+20])
    hashes = append(hashes, h)
}

dl, _ := download.NewDownloader(download.DownloaderConfig{
    InfoHash:    info.InfoHash,
    PeerID:      c.PeerID(),
    Port:        6881,
    PieceSize:   uint32(meta.Info.PieceLength),
    TotalSize:   uint64(meta.Info.TotalLength()),
    PieceHashes: hashes,
    OutputPath:  "/tmp/output",
    Strategy:    download.StrategyRarestFirst,
    ShareRatio:  1.0, // seed to 1:1 ratio
})

// Feed peers from tracker results
var addrs []*net.TCPAddr
for _, p := range results.AllPeers() {
    addrs = append(addrs, &net.TCPAddr{IP: p.IP, Port: int(p.Port)})
}
dl.AddPeers(addrs)

dl.Run(ctx) // blocks until complete + ratio met
```

### Bencode

```go
// Decode
var data map[string]any
bencode.NewDecoder(reader).Decode(&data)

// Decode into struct
var meta torrent.MetaInfo
bencode.NewDecoder(reader).Decode(&meta)

// Encode
bencode.NewEncoder(writer).Encode(data)
```

### Tracker client (standalone)

```go
tc := tracker.NewClient(15 * time.Second)
resp, _ := tc.Announce("udp://tracker.opentrackr.org:1337/announce", &tracker.AnnounceRequest{
    InfoHash: infoHash,
    PeerID:   peerID,
    Port:     6881,
    Left:     1000000,
    Compact:  true,
    Event:    tracker.EventStarted,
    NumWant:  50,
})
fmt.Println(resp.Complete, resp.Incomplete, len(resp.Peers))
```

## Protocols

### Implemented

| BEP | Protocol |
|-----|----------|
| 3   | Core wire protocol, .torrent format, tracker HTTP |
| 5   | DHT (Kademlia) |
| 6   | Fast extension (Reject, Have All/None, Allowed Fast) |
| 7/32| IPv6 peers and DHT |
| 8   | Message Stream Encryption (MSE) |
| 9   | Metadata exchange (ut_metadata) |
| 10  | Extension protocol handshake |
| 11  | Peer Exchange (ut_pex) |
| 12  | Multitracker |
| 14  | Local Service Discovery |
| 15  | UDP tracker |
| 20  | Peer ID conventions |
| 23  | Compact peer lists |
| 21  | Partial seeds (upload_only) |
| 24  | Tracker returns external IP |
| 27  | Private torrents |
| 29  | uTP (Micro Transport Protocol) |
| 42  | DHT security extension |
| 44  | Mutable/immutable DHT items |
| 48  | Tracker scrape (HTTP/UDP) |
| 51  | DHT infohash sampling |
| 54  | lt_donthave extension |
| 55  | Holepunch (NAT traversal) |

WebSocket tracker and WebRTC peer transport via pion/webrtc.

### Not implemented

| BEP | Protocol |
|-----|----------|
| 16  | Superseeding |
| 17  | HTTP seeding (Hoffman-style) |
| 19  | WebSeed (GetRight-style) |
| 40  | Canonical peer priority (CRC32C) |
| 41  | UDP tracker extensions |
| 43  | Read-only DHT nodes |
| 46  | Updating torrents via DHT mutable items |
| 47  | Padding files and extended file attributes |
| 52  | BitTorrent v2 (SHA-256, per-file Merkle trees) |
| 53  | Magnet URI file index selection |

## License

See [LICENSE](LICENSE).
