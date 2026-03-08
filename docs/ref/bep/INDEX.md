# BEP Archive Index

Converted BEP text extracts. For implementation-ready condensed notes, see `../bep-reference.md`.

Files marked **[ref]** have detailed implementation notes in `../bep-reference.md`.

---

## Final / Active

| BEP | Title | File | Notes |
|-----|-------|------|-------|
| 0 | Index of BitTorrent Enhancement Proposals | [00.md](00.md) | |
| 1 | The BEP Process | [01.md](01.md) | |
| 2 | Sample reStructuredText BEP Template | [02.md](02.md) | |
| 3 | The BitTorrent Protocol Specification | [03.md](03.md) | **[ref]** Core wire protocol, metainfo, tracker, choking |
| 4 | Known Number Allocations | [04.md](04.md) | Reserved bytes, extension IDs |
| 20 | Peer ID Conventions | [20.md](20.md) | **[ref]** Azureus-style `-XX1234-`, mainline style |

---

## Accepted

| BEP | Title | File | Notes |
|-----|-------|------|-------|
| 5 | DHT Protocol | [05.md](05.md) | **[ref]** Kademlia-based DHT, KRPC, routing table, k-buckets |
| 6 | Fast Extension | [06.md](06.md) | **[ref]** SUGGEST, HAVE_ALL, HAVE_NONE, REJECT, ALLOWED_FAST |
| 9 | Extension for Peers to Send Metadata Files | [09.md](09.md) | **[ref]** `ut_metadata`, magnet link support |
| 10 | Extension Protocol | [10.md](10.md) | **[ref]** BEP10 handshake, reserved bit 20, local extension IDs |
| 11 | Peer Exchange (PEX) | [11.md](11.md) | **[ref]** `ut_pex`, per-peer flags, bencode key ordering gotcha |
| 12 | Multitracker Metadata Extension | [12.md](12.md) | `announce-list` tiers |
| 14 | Local Service Discovery (LSD) | [14.md](14.md) | Multicast peer discovery on LAN |
| 15 | UDP Tracker Protocol | [15.md](15.md) | **[ref]** connect/announce/scrape over UDP, binary format |
| 19 | HTTP/FTP Seeding (GetRight-style) | [19.md](19.md) | `url-list` key in metainfo |
| 23 | Tracker Returns Compact Peer Lists | [23.md](23.md) | **[ref]** 6-byte compact peer, `compact=1` param |
| 27 | Private Torrents | [27.md](27.md) | `private=1` disables DHT/PEX |
| 29 | uTorrent Transport Protocol (uTP) | [29.md](29.md) | **[ref]** Delay-based congestion over UDP |
| 55 | Holepunch Extension | [55.md](55.md) | **[ref]** NAT traversal via relay peer |

---

## Draft

| BEP | Title | File | Notes |
|-----|-------|------|-------|
| 7 | IPv6 Tracker Extension | [07.md](07.md) | `ipv6` announce param, `peers6` response key |
| 16 | Superseeding | [16.md](16.md) | Efficient initial seeding strategy |
| 17 | HTTP Seeding (Hoffman-style) | [17.md](17.md) | Alternative webseed protocol |
| 21 | Extension for Partial Seeds | [21.md](21.md) | Advertise partial availability |
| 24 | Tracker Returns External IP | [24.md](24.md) | `external ip` in tracker response |
| 30 | Merkle Tree Torrent Extension | — | SHA1 Merkle trees (predates BEP 52); no extract available |
| 31 | Tracker Failure Retry Extension | — | `retry in` key |
| 32 | IPv6 Extension for DHT | [32.md](32.md) | **[ref]** `want`, `nodes6` |
| 33 | DHT Scrape | — | Scrape extension for DHT |
| 34 | DNS Tracker Preferences | — | SRV records for tracker discovery |
| 35 | Torrent Signing | — | RSA-signed torrents |
| 36 | Torrent RSS Feeds | — | Feed URL in metainfo |
| 38 | Finding Local Data via Torrent File Hints | — | |
| 39 | Updating Torrents via Feed URL | — | |
| 40 | Canonical Peer Priority | [40.md](40.md) | **[ref]** CRC32C-based peer ordering |
| 41 | UDP Tracker Protocol Extensions | [41.md](41.md) | |
| 42 | DHT Security Extension | [42.md](42.md) | **[ref]** Node ID derived from external IP |
| 43 | Read-only DHT Nodes | — | `ro` flag in KRPC messages |
| 44 | Storing Arbitrary Data in the DHT | [44.md](44.md) | **[ref]** Immutable/mutable items, ed25519 |
| 45 | Multiple-Address Operation for BitTorrent DHT | — | Multi-homed DHT nodes |
| 46 | Updating Torrents via DHT Mutable Items | — | BEP 44 + auto-updating torrents |
| 47 | Padding Files and Extended File Attributes | [47.md](47.md) | **[ref]** `attr` field (p/x/h/l) |
| 48 | Tracker Protocol Extension: Scrape | [48.md](48.md) | |
| 49 | Distributed Torrent Feeds | — | |
| 50 | Publish/Subscribe Protocol | — | |
| 51 | DHT Infohash Indexing | [51.md](51.md) | |
| 52 | BitTorrent Protocol Specification v2 | [52.md](52.md) | **[ref]** SHA2-256, per-file Merkle trees, hybrid torrents |
| 53 | Magnet URI Extension — File Index Selection | — | `so=` param |
| 54 | The lt_donthave Extension | — | Retract a have announcement |

---

## Deferred

| BEP | Title | File | Notes |
|-----|-------|------|-------|
| 8 | Message Stream Encryption (Tracker Peer Obfuscation) | [08.md](08.md) | **[ref]** DH + RC4; see also `../mse-encryption.md` |
| 18 | Search Engine Specification | [18.md](18.md) | |
| 22 | BitTorrent Local Tracker Discovery Protocol | [22.md](22.md) | |
| 26 | Zeroconf Peer Advertising and Discovery | — | |
| 28 | Tracker Exchange | — | |

---

## Pending Standards Track

| BEP | Title | File | Notes |
|-----|-------|------|-------|
| 1000 | Pending Standards Track | [1000.md](1000.md) | |
