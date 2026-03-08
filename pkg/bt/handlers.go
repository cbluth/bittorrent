package bt

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	bittorrent "github.com/cbluth/bittorrent/pkg/client"
	"github.com/cbluth/bittorrent/pkg/config"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/log"
)

// HandleTorrentFile processes a torrent file.
func HandleTorrentFile(ctx context.Context, client *bittorrent.Client, path string, cacheOps *config.CacheOperations, verbose bool, noTracker bool, outputFile string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open torrent file: %w", err)
	}
	defer file.Close()

	info, err := client.LoadTorrent(file)
	if err != nil {
		return fmt.Errorf("failed to load torrent: %w", err)
	}

	PrintTorrentInfo(info)

	// Save to JSON file if output file is specified and ends with .json
	if outputFile != "" && strings.HasSuffix(strings.ToLower(outputFile), ".json") {
		meta := info.GetMetaInfo()
		if meta == nil {
			log.Warn("cannot save JSON file, no metadata available", "sub", "torrent")
		} else {
			jsonData, err := json.MarshalIndent(meta, "", "  ")
			if err != nil {
				log.Warn("failed to convert to JSON", "sub", "torrent", "err", err)
			} else {
				if err := os.WriteFile(outputFile, jsonData, 0644); err != nil {
					log.Warn("failed to write JSON file", "sub", "torrent", "err", err)
				} else {
					fmt.Printf("✓ Saved JSON file to: %s\n", outputFile)
				}
			}
		}
	}

	// Skip tracker queries if disabled
	if noTracker {
		log.Info("trackers disabled via -no-tracker flag", "sub", "tracker")
		return nil
	}

	// Query trackers
	log.Debug("querying trackers", "sub", "tracker")
	results, err := client.QueryTrackers(ctx, info)
	if err != nil {
		return fmt.Errorf("failed to query trackers: %w", err)
	}

	PrintTrackerResults(results, verbose)

	// Save tracker results to cache
	if results != nil && len(results.Successful) > 0 {
		for _, tr := range results.Successful {
			if err := cacheOps.AddOrUpdateTracker(tr.URL, true); err != nil {
				log.Warn("failed to cache tracker", "sub", "tracker", "url", tr.URL, "err", err)
			}
		}
		log.Info("saved working trackers to cache", "sub", "tracker", "count", len(results.Successful))
	}

	// Print tracker summary
	PrintTrackerSummary(results)
	return nil
}

// MagnetOptions contains options for magnet handling.
type MagnetOptions struct {
	UseDHT              bool
	DHTPort             uint
	DHTNodes            int
	DHTBootstrapTimeout time.Duration
	Resolve             bool
	ResolveTimeout      time.Duration
	Verbose             bool
	NoTracker           bool
	OutputFile          string
}

// HandleMagnet processes a magnet link.
func HandleMagnet(ctx context.Context, client *bittorrent.Client, magnetURI string, cacheOps *config.CacheOperations, cacheDir string, opts *MagnetOptions) error {
	info, err := client.ParseMagnet(magnetURI)
	if err != nil {
		return fmt.Errorf("failed to parse magnet: %w", err)
	}

	PrintMagnetInfo(info)

	if opts.Resolve {
		if opts.Verbose {
			fmt.Println("\n=== Resolving Metadata (BEP 9) ===")
			if !opts.NoTracker {
				fmt.Println("1. Querying trackers for peers...")
			} else {
				fmt.Println("1. Trackers disabled, using DHT only...")
			}
		}

		resolveCtx, cancel := context.WithTimeout(ctx, opts.ResolveTimeout)
		defer cancel()

		torrentInfo, err := client.ResolveMetadataFromHash(resolveCtx, info.InfoHash)
		if err != nil {
			return fmt.Errorf("metadata resolution failed: %w", err)
		}
		fmt.Println("✓ Successfully resolved metadata!")

		// Save to .torrent file if output file is specified
		if opts.OutputFile != "" {
			meta := torrentInfo.GetMetaInfo()
			if meta == nil {
				log.Warn("cannot save torrent file, no metadata available", "sub", "torrent")
			} else {
				f, err := os.Create(opts.OutputFile)
				if err != nil {
					log.Warn("failed to create torrent file", "sub", "torrent", "err", err)
				} else {
					defer f.Close()
					if err := meta.Write(f); err != nil {
						log.Warn("failed to write torrent file", "sub", "torrent", "err", err)
					} else {
						fmt.Printf("✓ Saved torrent file to: %s\n", opts.OutputFile)
					}
				}
			}
		}

		PrintTorrentInfo(torrentInfo)
		return nil
	}

	// Query trackers (skip if disabled)
	var results *bittorrent.TrackerResults
	if opts.NoTracker {
		log.Info("trackers disabled via -no-tracker flag", "sub", "tracker")
	} else {
		log.Debug("querying trackers", "sub", "tracker")
		results, err = client.QueryTrackers(ctx, info)
		if err != nil {
			return fmt.Errorf("failed to query trackers: %w", err)
		}

		PrintTrackerResults(results, opts.Verbose)

		// Save tracker results to cache
		if results != nil && len(results.Successful) > 0 {
			for _, tr := range results.Successful {
				if err := cacheOps.AddOrUpdateTracker(tr.URL, true); err != nil {
					log.Warn("failed to cache tracker", "sub", "tracker", "url", tr.URL, "err", err)
				}
			}
			log.Info("saved working trackers to cache", "sub", "tracker", "count", len(results.Successful))
		}
	}

	// Query DHT if enabled
	var dhtNode *dht.Node
	var dhtPeers []*net.UDPAddr
	if opts.UseDHT {
		log.Info("querying DHT for additional peers", "sub", "dht")

		bootstrapNodes := config.DHTBootstrapNodes()
		if len(bootstrapNodes) == 0 {
			log.Warn("no DHT bootstrap nodes configured", "sub", "dht")
		}
		dhtNode, err = dht.NewNode(uint16(opts.DHTPort), bootstrapNodes)
		if err != nil {
			log.Warn("failed to create DHT node", "sub", "dht", "err", err)
		} else {
			defer func() {
				localCacheOps := config.NewCacheOperations()
				if err := SaveDHTNodesToCache(localCacheOps, dhtNode); err != nil {
					log.Warn("failed to save DHT nodes to cache", "sub", "dht", "err", err)
				}
				dhtNode.Shutdown()
			}()

			localCacheOps := config.NewCacheOperations()
			cachedNodes, err := LoadDHTNodesFromCache(localCacheOps, dhtNode)
			if err != nil {
				log.Warn("failed to load cached DHT nodes", "sub", "dht", "err", err)
				cachedNodes = 0
			}

			targetNodes := opts.DHTNodes
			if cachedNodes > 0 {
				log.Debug("using cached DHT nodes", "sub", "dht", "count", cachedNodes)
				targetNodes = targetNodes - cachedNodes
				if targetNodes < 20 {
					targetNodes = 20
				}
			}

			bootstrapCtx, cancel := context.WithTimeout(ctx, opts.DHTBootstrapTimeout)
			err = dhtNode.Bootstrap(bootstrapCtx, targetNodes)
			cancel()
			if err != nil && err != context.DeadlineExceeded {
				log.Warn("DHT bootstrap error", "sub", "dht", "err", err)
			}

			routingTableSize := dhtNode.DHT().Length()
			log.Info("DHT bootstrap complete", "sub", "dht", "nodes", routingTableSize)

			if routingTableSize > 0 {
				dhtPeers, err = dhtNode.FindPeersForInfoHash(info.InfoHash, 20, 8)
				if err != nil {
					log.Warn("DHT query failed", "sub", "dht", "err", err)
				} else {
					log.Info("found peers from DHT", "sub", "dht", "count", len(dhtPeers))
				}
			}
		}
	}

	PrintCombinedSummary(results, dhtPeers, dhtNode)
	return nil
}

// InfoHashOptions contains options for info hash resolution.
type InfoHashOptions struct {
	UseDHT              bool
	DHTPort             uint
	DHTNodes            int
	DHTBootstrapTimeout time.Duration
	ResolveTimeout      time.Duration
	Verbose             bool
	NoTracker           bool
}

// HandleInfoHash resolves metadata from an info hash.
func HandleInfoHash(ctx context.Context, client *bittorrent.Client, hashHex string, cacheOps *config.CacheOperations, cacheDir string, opts *InfoHashOptions) error {
	fmt.Printf("=== Resolving Metadata for Info Hash ===\n")
	fmt.Printf("Info Hash: %s\n\n", hashHex)
	fmt.Println("Process:")
	fmt.Println("  1. Query trackers to find peers")
	fmt.Println("  2. Query DHT for more nodes (expand to 100 nodes)")
	fmt.Println("  3. Query DHT for peers with the info hash")
	fmt.Println("  4. Connect to peers and request metadata using BEP 9 protocol")
	fmt.Println()

	infoHash, err := dht.KeyFromHex(hashHex)
	if err != nil {
		return fmt.Errorf("invalid info hash: %w", err)
	}

	// Create a minimal MagnetInfo to use QueryTrackers
	magnetInfo := &bittorrent.MagnetInfo{
		InfoHash: infoHash,
	}

	type trackerResultChan struct {
		results *bittorrent.TrackerResults
		err     error
	}
	trackerChan := make(chan trackerResultChan, 1)

	type dhtResultChan struct {
		node  *dht.Node
		peers []*net.UDPAddr
		err   error
	}
	dhtChan := make(chan dhtResultChan, 1)

	// Create DHT node first (needed for both expansion and peer lookup)
	var dhtNode *dht.Node
	if opts.UseDHT {
		bootstrapNodes := config.DHTBootstrapNodes()
		if len(bootstrapNodes) == 0 {
			log.Warn("no DHT bootstrap nodes configured", "sub", "dht")
		}
		dhtNode, err = dht.NewNode(uint16(opts.DHTPort), bootstrapNodes)
		if err != nil {
			log.Warn("failed to create DHT node", "sub", "dht", "err", err)
			opts.UseDHT = false
		} else {
			defer func() {
				localCacheOps := config.NewCacheOperations()
				if err := SaveDHTNodesToCache(localCacheOps, dhtNode); err != nil {
					log.Warn("failed to save DHT nodes to cache", "sub", "dht", "err", err)
				}
				dhtNode.Shutdown()
			}()

			localCacheOps := config.NewCacheOperations()
			cachedNodes, err := LoadDHTNodesFromCache(localCacheOps, dhtNode)
			if err != nil {
				log.Warn("failed to load cached DHT nodes", "sub", "dht", "err", err)
			} else if cachedNodes > 0 {
				log.Debug("using cached DHT nodes", "sub", "dht", "count", cachedNodes)
			}
		}
	}

	// 1. Start tracker queries in parallel (if not disabled)
	if !opts.NoTracker {
		go func() {
			log.Debug("querying trackers for peers", "sub", "tracker")
			results, err := client.QueryTrackers(ctx, magnetInfo)
			if err != nil {
				log.Warn("tracker query failed", "sub", "tracker", "err", err)
			} else {
				PrintTrackerResults(results, opts.Verbose)
				if len(results.Successful) > 0 {
					for _, tr := range results.Successful {
						if err := cacheOps.AddOrUpdateTracker(tr.URL, true); err != nil {
							log.Warn("failed to cache tracker", "sub", "tracker", "url", tr.URL, "err", err)
						}
					}
					log.Info("saved working trackers to cache", "sub", "tracker", "count", len(results.Successful))
				}
			}
			trackerChan <- trackerResultChan{results: results, err: err}
		}()
	} else {
		log.Info("trackers disabled via -no-tracker flag", "sub", "tracker")
		trackerChan <- trackerResultChan{results: nil, err: nil}
	}

	// 2. Start DHT operations in parallel (if enabled)
	if opts.UseDHT {
		go func() {
			expandDone := make(chan struct{})
			go func() {
				defer close(expandDone)
				currentNodes := dhtNode.DHT().Length()
				if currentNodes >= 100 {
					log.Debug("DHT already has enough nodes", "sub", "dht", "count", currentNodes)
					return
				}
				log.Debug("expanding routing table", "sub", "dht", "current", currentNodes, "target", 100)
				expandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				if err := dhtNode.EnsureMinNodes(expandCtx, 100); err != nil {
					log.Warn("DHT expansion warning", "sub", "dht", "err", err, "nodes", dhtNode.DHT().Length())
				} else {
					log.Debug("DHT expansion complete", "sub", "dht", "nodes", dhtNode.DHT().Length())
				}
			}()

			log.Debug("querying DHT for peers", "sub", "dht")
			dhtPeers, err := dhtNode.FindPeersForInfoHash(infoHash, 20, 8)
			if err != nil {
				log.Warn("DHT peer query failed", "sub", "dht", "err", err)
			} else {
				log.Info("found peers from DHT", "sub", "dht", "count", len(dhtPeers))
			}
			<-expandDone
			dhtChan <- dhtResultChan{node: dhtNode, peers: dhtPeers, err: err}
		}()
	} else {
		dhtChan <- dhtResultChan{node: nil, peers: nil, err: nil}
	}

	// 3. Wait for tracker and DHT results
	trackerResult := <-trackerChan
	dhtResult := <-dhtChan

	// Collect all peers
	var allPeers []net.Addr
	if trackerResult.results != nil {
		for _, p := range trackerResult.results.AllPeers() {
			allPeers = append(allPeers, &net.TCPAddr{IP: p.IP, Port: int(p.Port)})
		}
	}
	for _, p := range dhtResult.peers {
		allPeers = append(allPeers, p)
	}

	if len(allPeers) == 0 {
		return fmt.Errorf("metadata resolution failed: no peers available from trackers or DHT")
	}

	// 4. Start metadata resolution with all peers
	log.Info("starting metadata resolution", "sub", "torrent", "timeout", opts.ResolveTimeout)
	resolveCtx, cancel := context.WithTimeout(ctx, opts.ResolveTimeout)
	defer cancel()

	info, err := client.ResolveMetadataFromPeers(resolveCtx, infoHash, allPeers)
	if err != nil {
		return fmt.Errorf("metadata resolution failed: %w", err)
	}

	fmt.Println("\n✓ Successfully resolved metadata!")
	PrintTorrentInfo(info)
	PrintCombinedSummary(trackerResult.results, dhtResult.peers, dhtResult.node)
	return nil
}

// ExtractInfoHashFromMagnet extracts the info hash from a magnet URI.
func ExtractInfoHashFromMagnet(magnetURI string) string {
	const prefix = "xt=urn:btih:"
	idx := strings.Index(magnetURI, prefix)
	if idx == -1 {
		return ""
	}
	start := idx + len(prefix)
	end := start
	for end < len(magnetURI) && magnetURI[end] != '&' {
		end++
	}
	return strings.ToLower(magnetURI[start:end])
}
