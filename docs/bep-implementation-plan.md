# BEP Implementation Plan

Remaining unimplemented BEPs, ordered by priority.

## Completed

- **BEP 54** — lt_donthave (receiver): extension handshake `lt_donthave: 4`, `HandleDontHave`, `Bitfield.Clear`
- **BEP 24** — Tracker returns external IP: `AnnounceResponse.ExternalIP` parsed from HTTP responses
- **BEP 21** — Partial seeds: `upload_only` in extension handshake, `peer.uploadOnly` tracking

---

## Next Up

### BEP 40 — Canonical Peer Priority (CRC32C)

**Complexity**: Small (2-3 days)
**Value**: Deterministic peer preference reduces connection churn in swarms.

Peers are ranked by `crc32c(sort(mask(local_ip), mask(peer_ip)))` where IPv4 uses /24 mask and IPv6 uses /48 mask. Lower CRC32C = higher priority. Used in connection slot allocation and eviction.

Already have CRC32C in BEP 42 (DHT security). This extends it to the choker/connection manager.

**Changes**:
- `pkg/choker/choker.go` — add peer priority as tiebreaker in unchoke decisions
- `pkg/client/download/downloader.go` — compute priority on connect, use in eviction (prefer keeping low-priority-value peers), use in duplicate connection resolution

**Dependencies**: Need to know own external IP (BEP 24 helps here).

---

## Medium Impact

### BEP 19 — WebSeed (GetRight-style)

**Complexity**: Medium (3-5 days)
**Value**: High. HTTP mirrors dramatically accelerate downloads, especially for new/low-seed torrents.

The torrent metainfo includes `url-list` with HTTP/HTTPS URLs. The client downloads pieces via HTTP Range requests, falling back to regular peers for verification.

**Wire format**: Standard HTTP GET with `Range: bytes=X-Y` headers. Piece boundaries map to byte ranges. Multi-file torrents need path-based URL construction.

**Changes**:
- `pkg/torrent/metainfo.go` — parse `url-list` from metainfo
- `pkg/client/download/webseed.go` (new) — HTTP client that fetches piece-aligned byte ranges, implements piece verification, handles multi-file path mapping
- `pkg/client/download/downloader.go` — integrate webseed sources alongside peers, feed completed pieces through verification pipeline

**Considerations**:
- Retry/backoff for HTTP 503
- Bandwidth limiting to avoid overwhelming HTTP mirrors
- Connection reuse (HTTP keep-alive)
- Multi-file: URL + path components = full URL per file

---

### BEP 47 — Padding Files and Extended File Attributes

**Complexity**: Medium (2-3 days)
**Value**: Better multi-file torrent interop, especially with libtorrent-created torrents.

Padding files (`.pad/N`) align file boundaries to piece boundaries, eliminating cross-file pieces. Extended attributes include `attr` field: `p`=padding, `x`=executable, `h`=hidden, `s`=symlink.

**Changes**:
- `pkg/torrent/metainfo.go` — parse `attr` field in FileInfo, detect padding files
- `pkg/client/download/downloader.go` — skip writing padding files, handle file attributes on output
- Torrent creation (if added): insert padding files to align pieces

---

### BEP 43 — Read-only DHT Nodes

**Complexity**: Small (1-2 days)
**Value**: Reduces DHT traffic for clients that only need lookups.

A read-only node sets `ro=1` in DHT queries. Responders must not add read-only nodes to their routing tables or ping them. Useful for short-lived clients or those behind restrictive NATs.

**Changes**:
- `pkg/dht/node.go` — add `ReadOnly bool` config option
- `pkg/dht/node.go` — include `ro: 1` in outgoing queries when read-only
- `pkg/dht/node.go` — skip adding `ro=1` nodes to routing table on incoming queries

---

## Lower Priority

### BEP 53 — Magnet URI File Index Selection

**Complexity**: Small (1-2 days)
**Value**: Low. Niche feature for selecting specific files from magnet links.

Adds `so` (select only) parameter to magnet URIs: `magnet:?...&so=0,2,4-6` selects file indices 0, 2, 4, 5, 6. Client downloads only those files.

**Changes**:
- `pkg/client/magnet.go` — parse `so` parameter from magnet URI
- `pkg/client/download/downloader.go` — map selected file indices to piece ranges, use as initial SetFileRange
- `cmd/bt/bt.go` — expose file selection in CLI

---

### BEP 41 — UDP Tracker Extensions

**Complexity**: Small (1-2 days)
**Value**: Low. Extends UDP tracker with optional features (URL data, authentication).

Adds option TLVs after the fixed announce request/response. Most trackers don't support these extensions.

**Changes**:
- `pkg/tracker/udp/client.go` — parse option TLVs from announce response, support sending option TLVs

---

### BEP 16 — Superseeding

**Complexity**: Medium (2-3 days)
**Value**: Low unless seeding large popular torrents. Reduces upload waste on initial seed.

Superseed mode: instead of advertising all pieces via bitfield, pretend to have only one piece. After a peer downloads it, advertise another piece only after seeing that piece propagate to other peers. Maximizes piece diversity from a single seed.

**Changes**:
- `pkg/client/download/downloader.go` — superseed mode: send fake bitfield (single piece), track which pieces each peer has seen, reveal new pieces based on propagation
- Requires tracking per-peer "seen" state

---

### BEP 17 — HTTP Seeding (Hoffman-style)

**Complexity**: Medium (3-4 days)
**Value**: Low. Superseded by BEP 19 in practice.

Similar to BEP 19 but uses a different URL scheme (`httpseeds` key). The HTTP server must understand the BitTorrent piece/range mapping. Less widely supported than BEP 19.

**Changes**:
- `pkg/torrent/metainfo.go` — parse `httpseeds` key
- `pkg/client/download/httpseed.go` (new) — Hoffman-style HTTP seed client
- Can share infrastructure with BEP 19 webseed implementation

---

### BEP 46 — Updating Torrents via DHT Mutable Items

**Complexity**: Large (5-7 days)
**Value**: Medium. Enables updateable magnet links (content changes, same link).

Uses BEP 44 mutable DHT items (already implemented) to store a pointer to the latest info hash. A magnet link contains a public key; the DHT stores `{seq: N, v: info_hash}` signed with the corresponding private key. Clients poll the DHT for updates.

**Changes**:
- `pkg/dht/node.go` — use existing BEP 44 `get`/`put` for mutable items
- New update publisher: sign and store updated info hash
- New update subscriber: poll DHT, detect sequence number changes, re-resolve metadata
- `cmd/bt/bt.go` — CLI commands for publishing and subscribing to updateable torrents

**Dependencies**: BEP 44 (implemented). Needs ed25519 signing.

---

### BEP 52 — BitTorrent v2

**Complexity**: Very large (weeks)
**Value**: Future-proofing, but v2 adoption is still low.

Complete protocol revision: SHA-256, per-file Merkle trees, hybrid v1+v2 torrents. Touches nearly every component.

**Not recommended for near-term implementation.** Track adoption before investing.

**Would require**:
- New metainfo format (`meta version`, `file tree`, per-file `pieces root`)
- SHA-256 info hash (32 bytes) alongside SHA-1
- Per-file Merkle tree verification
- Hybrid torrent support (v1+v2 info dict)
- Protocol handshake changes
- DHT changes for 32-byte keys
- Tracker changes for 32-byte info hash

---

## Implementation Order

| Phase | BEPs | Effort | Status |
|-------|------|--------|--------|
| ~~1~~ | ~~54, 24, 21~~ | ~~4 days~~ | Done |
| 2 | 40, 43 | ~4 days | Next |
| 3 | 19 | ~5 days | — |
| 4 | 47, 53 | ~3 days | — |
| 5 | 41, 16 | ~3 days | — |
| 6 | 17, 46 | ~8 days | — |
| 7 | 52 | weeks | — |

Phase 2 (BEP 40, 43) are the next quick wins. Phase 3 (BEP 19 WebSeed) is the highest-value single feature. Phases 4-6 are diminishing returns. Phase 7 (BEP 52) is a separate project.
