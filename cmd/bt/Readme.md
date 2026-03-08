# bt

BitTorrent CLI tool. Download, stream, inspect, and serve torrents. Includes DHT, tracker client/server, metadata resolution, and a background daemon.

## Install

```bash
go install github.com/cbluth/bittorrent/cmd/bt@latest
```

Or build from source:

```bash
go build -o bt ./cmd/bt
```

## Usage

```
bt [options] <command>
```

### Global Options

```
-v                    Debug logging
-port <n>             Peer port (default: 6881)
-dht-port <n>         DHT port (default: 6881)
-no-dht               Disable DHT
-no-tracker           Disable trackers
-trackers <file>      Tracker URL list (one per line)
-timeout <dur>        Tracker timeout (default: 15s)
-resolve-timeout <dur> Metadata resolution timeout (default: 2m)
-ratio <n>            Share ratio target (0.0=leech only, 1.5=seed 1.5x, unset=seed forever)
-json                 JSON structured logging
-c <path>             Config file (default: ~/.bt/config.json)
-socket <path>        Unix socket for node daemon
```

## Commands

### Download

```bash
bt download <torrent-file|magnet>
bt download -o /tmp/out -sequential "magnet:?xt=urn:btih:..."
```

Aliases: `dl`, `get`. Flags: `-o <dir>` (output), `-sequential` (streaming order).

### Stream Server

```bash
bt server
bt server -port 9090 -o /tmp/media
```

HTTP streaming server. Navigate to `http://localhost:8080/bt/<infohash>` to stream any torrent. Aliases: `serve`, `http`.

### Magnet / Hash Resolve

```bash
bt magnet "magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c"
bt magnet dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c
bt magnet <hash> -o metadata.torrent
bt magnet <hash> --wait    # route through running node's DHT
```

Resolves metadata from a magnet URI, raw info hash, or `btih:` URN via BEP 9. Aliases: `hash`, `resolve`, `metadata`.

### Torrent Inspect

```bash
bt torrent file.torrent
bt torrent file.torrent -o info.json
bt torrent https://example.com/file.torrent
```

### DHT

```bash
bt dht bootstrap
bt dht get-peers <infohash>
bt dht find-node <node-id>
bt dht ping <host:port>
bt dht announce-peer <infohash>
bt dht sample
bt dht stats
bt dht server
```

### Tracker

```bash
bt tracker announce <infohash|torrent>
bt tracker scrape <infohash|torrent>
bt tracker peers <infohash|torrent>
bt tracker server -port 8080
```

Supports HTTP, HTTPS, UDP, and WebSocket trackers.

### Node Daemon

Long-running background process with a warm DHT routing table and Unix socket API.

```bash
bt node start          # daemonize
bt node run            # foreground
bt node status         # print state
bt node logs -f        # tail logs
bt node stop           # graceful shutdown
```

Socket API (used by other `bt` commands via `--wait`):

```
GET  /status          Node state + DHT size + uptime
GET  /dht/peers/<hash> Find peers for infohash
GET  /peers           DHT routing table nodes
GET  /torrents        Cached infohashes
POST /shutdown        Graceful stop
```

### Peer

```bash
bt peer handshake <host:port> <infohash>
bt peer metadata <host:port> <infohash>
bt peer raw <host:port> <infohash>
```

Low-level peer wire protocol operations.

### Fetch

```bash
bt fetch piece <infohash> <index>
bt fetch range <infohash> <offset> <length>
```

Fetch individual pieces or byte ranges from the swarm.

### Net

```bash
bt net external-ip
bt net nat-test
bt net ports
bt net latency <host:port>
bt net sniff
```

### Bencode

```bash
bt bencode decode file.torrent
echo '{"key":"val"}' | bt bencode encode
```

Aliases: `encode`, `decode`.

### Config

```bash
bt config list
bt config get <key>
bt config set <key> <value>
bt config del <key>
bt config edit
```

Config file: `~/.bt/config.json`. Log topic filtering:

```bash
bt config set log '["peer","stream","dht"]'
```

### Shell Completion

```bash
eval "$(bt completion bash)"
eval "$(bt completion zsh)"
bt completion fish | source
```

## Protocols

BEP 3 (wire), BEP 5 (DHT), BEP 6 (fast extension), BEP 7/32 (IPv6), BEP 8 (MSE encryption), BEP 9 (metadata), BEP 10 (extensions), BEP 11 (PEX), BEP 12 (multitracker), BEP 14 (LSD), BEP 15 (UDP tracker), BEP 23 (compact peers), BEP 29 (uTP), BEP 42 (DHT security), BEP 44 (mutable DHT), BEP 51 (DHT infohash indexing), BEP 55 (holepunch).
