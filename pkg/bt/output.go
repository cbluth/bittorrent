package bt

import (
	"fmt"
	"net"

	bittorrent "github.com/cbluth/bittorrent/pkg/client"
	"github.com/cbluth/bittorrent/pkg/dht"
	"github.com/cbluth/bittorrent/pkg/tracker"
)

// PrintTrackerSummary prints a summary of tracker results
func PrintTrackerSummary(results *bittorrent.TrackerResults) {
	allPeers := results.AllPeers()

	// Count IPv4 vs IPv6 peers
	ipv4Peers := 0
	ipv6Peers := 0
	for _, peer := range allPeers {
		if peer.IP.To4() != nil {
			ipv4Peers++
		} else {
			ipv6Peers++
		}
	}

	fmt.Println("\n=== Tracker Summary ===")
	fmt.Printf("Total unique peers: %d\n", len(allPeers))
	fmt.Printf("  IPv4 peers: %d\n", ipv4Peers)
	fmt.Printf("  IPv6 peers: %d\n", ipv6Peers)
	fmt.Printf("Successful trackers: %d/%d\n", len(results.Successful), len(results.Successful)+len(results.Failed))
	fmt.Printf("Total seeders: %d\n", results.TotalSeeders())
	fmt.Printf("Total leechers: %d\n", results.TotalLeechers())
}

// PrintTorrentInfo prints detailed torrent information
func PrintTorrentInfo(info *bittorrent.TorrentInfo) {
	fmt.Println("\n=== Torrent Information ===")
	fmt.Printf("Name: %s\n", info.Name)
	fmt.Printf("Info Hash: %x\n", info.InfoHash)
	fmt.Printf("Total Size: %d bytes (%.2f MB)\n", info.TotalLength, float64(info.TotalLength)/(1024*1024))
	fmt.Printf("Piece Length: %d bytes\n", info.PieceLength)
	fmt.Printf("Number of Pieces: %d\n", info.NumPieces)

	if info.IsSingleFile {
		fmt.Println("Type: Single file")
	} else {
		fmt.Printf("Type: Multi-file (%d files)\n", len(info.Files))
		if len(info.Files) > 0 && len(info.Files) <= 10 {
			fmt.Println("\nFiles:")
			for i, f := range info.Files {
				fmt.Printf("  %d. %s (%d bytes)\n", i+1, f.Path, f.Length)
			}
		}
	}

	if len(info.Trackers) > 0 {
		fmt.Printf("\nTrackers from torrent: %d\n", len(info.Trackers))
		for i, t := range info.Trackers {
			if i >= 5 {
				fmt.Printf("  ... and %d more\n", len(info.Trackers)-5)
				break
			}
			fmt.Printf("  - %s\n", t)
		}
	}
}

// PrintMagnetInfo prints magnet link information
func PrintMagnetInfo(info *bittorrent.MagnetInfo) {
	fmt.Println("\n=== Magnet Information ===")
	fmt.Printf("Info Hash: %x\n", info.InfoHash)
	if info.DisplayName != "" {
		fmt.Printf("Display Name: %s\n", info.DisplayName)
	}
	if info.ExactLength > 0 {
		fmt.Printf("Exact Length: %d bytes (%.2f MB)\n", info.ExactLength, float64(info.ExactLength)/(1024*1024))
	}
	if len(info.Trackers) > 0 {
		fmt.Printf("\nTrackers from magnet: %d\n", len(info.Trackers))
		for i, t := range info.Trackers {
			if i >= 5 {
				fmt.Printf("  ... and %d more\n", len(info.Trackers)-5)
				break
			}
			fmt.Printf("  - %s\n", t)
		}
	}
}

// PrintTrackerResults prints detailed tracker query results
func PrintTrackerResults(results *bittorrent.TrackerResults, verbose bool) {
	fmt.Println("\n=== Tracker Query Results ===")
	fmt.Printf("Successful: %d/%d\n", len(results.Successful), len(results.Successful)+len(results.Failed))

	if len(results.Successful) > 0 {
		allPeers := results.AllPeers()

		// Count IPv4 vs IPv6 peers
		ipv4Peers := 0
		ipv6Peers := 0
		for _, peer := range allPeers {
			if peer.IP.To4() != nil {
				ipv4Peers++
			} else {
				ipv6Peers++
			}
		}

		fmt.Printf("Unique peers found: %d\n", len(allPeers))
		fmt.Printf("  IPv4 peers: %d\n", ipv4Peers)
		fmt.Printf("  IPv6 peers: %d\n", ipv6Peers)
		fmt.Printf("Total seeders: %d\n", results.TotalSeeders())
		fmt.Printf("Total leechers: %d\n", results.TotalLeechers())

		if verbose {
			fmt.Println("\nSuccessful trackers:")
			for _, tr := range results.Successful {
				fmt.Printf("  ✓ %s: %d peers, %d seeders, %d leechers\n",
					tr.URL, len(tr.Response.Peers), tr.Response.Complete, tr.Response.Incomplete)
			}
		}

		if len(allPeers) > 0 && verbose {
			fmt.Println("\nSample peers:")
			max := 10
			if len(allPeers) < max {
				max = len(allPeers)
			}
			for i := 0; i < max; i++ {
				peerType := "IPv4"
				if allPeers[i].IP.To4() == nil {
					peerType = "IPv6"
				}
				fmt.Printf("  [%s] %s:%d\n", peerType, allPeers[i].IP.String(), allPeers[i].Port)
			}
			if len(allPeers) > max {
				fmt.Printf("  ... and %d more\n", len(allPeers)-max)
			}
		}
	}

	if len(results.Failed) > 0 && verbose {
		fmt.Println("\nFailed trackers:")
		for _, te := range results.Failed {
			fmt.Printf("  ✗ %s: %v\n", te.URL, te.Error)
		}
	}
}

// PrintDHTSummary prints DHT routing table statistics
func PrintDHTSummary(node *dht.Node) {
	stats := node.DHT().Stats()

	fmt.Println("\n=== DHT Summary ===")
	fmt.Printf("Total nodes in routing table: %v\n", stats["node_count"])
	fmt.Printf("  IPv4 nodes: %v\n", stats["ipv4_nodes"])
	fmt.Printf("  IPv6 nodes: %v\n", stats["ipv6_nodes"])
	fmt.Printf("\nNode states:\n")
	fmt.Printf("  Good: %v\n", stats["good_nodes"])
	fmt.Printf("  Questionable: %v\n", stats["questionable_nodes"])
	fmt.Printf("  Bad: %v\n", stats["bad_nodes"])
}

// PrintCombinedSummary prints a combined summary of tracker and DHT results
func PrintCombinedSummary(trackerResults *bittorrent.TrackerResults, dhtPeers []*net.UDPAddr, dhtNode *dht.Node) {
	fmt.Println("\n=== Combined Peer Discovery Summary ===")

	// Tracker stats
	var trackerPeers []tracker.Peer
	if trackerResults != nil {
		trackerPeers = trackerResults.AllPeers()
	}

	trackerIPv4 := 0
	trackerIPv6 := 0
	for _, peer := range trackerPeers {
		if peer.IP.To4() != nil {
			trackerIPv4++
		} else {
			trackerIPv6++
		}
	}

	// DHT stats
	dhtIPv4 := 0
	dhtIPv6 := 0
	for _, peer := range dhtPeers {
		if peer.IP.To4() != nil {
			dhtIPv4++
		} else {
			dhtIPv6++
		}
	}

	// Print tracker results
	fmt.Printf("\nFrom Trackers:\n")
	if trackerResults != nil {
		fmt.Printf("  Total peers: %d\n", len(trackerPeers))
		fmt.Printf("    IPv4: %d\n", trackerIPv4)
		fmt.Printf("    IPv6: %d\n", trackerIPv6)
		fmt.Printf("  Successful trackers: %d/%d\n", len(trackerResults.Successful), len(trackerResults.Successful)+len(trackerResults.Failed))
		fmt.Printf("  Total seeders: %d\n", trackerResults.TotalSeeders())
		fmt.Printf("  Total leechers: %d\n", trackerResults.TotalLeechers())
	} else {
		fmt.Printf("  No tracker results\n")
	}

	// Print DHT results
	fmt.Printf("\nFrom DHT:\n")
	if len(dhtPeers) > 0 {
		fmt.Printf("  Total peers: %d\n", len(dhtPeers))
		fmt.Printf("    IPv4: %d\n", dhtIPv4)
		fmt.Printf("    IPv6: %d\n", dhtIPv6)
	} else {
		fmt.Printf("  No DHT peers found\n")
	}

	if dhtNode != nil {
		stats := dhtNode.DHT().Stats()
		fmt.Printf("  Routing table: %v nodes (%v IPv4, %v IPv6)\n",
			stats["node_count"], stats["ipv4_nodes"], stats["ipv6_nodes"])
	}

	// Combined totals
	totalPeers := len(trackerPeers) + len(dhtPeers)
	totalIPv4 := trackerIPv4 + dhtIPv4
	totalIPv6 := trackerIPv6 + dhtIPv6

	fmt.Printf("\nCombined Total:\n")
	fmt.Printf("  Total unique peers: %d\n", totalPeers)
	fmt.Printf("    IPv4: %d\n", totalIPv4)
	fmt.Printf("    IPv6: %d\n", totalIPv6)
}
