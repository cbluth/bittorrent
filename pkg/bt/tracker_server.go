package bt

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cbluth/bittorrent/pkg/log"
	trackerhttp "github.com/cbluth/bittorrent/pkg/tracker/http"
)

func MainForTracker() {
	port := flag.Int("port", 6969, "Tracker server port")
	storage := flag.String("storage", "memory", "Storage backend")
	dbPath := flag.String("db-path", "", "Database path")
	announceInterval := flag.Duration("announce-interval", 30*time.Minute, "Announce interval")
	maxPeers := flag.Int("max-peers", 200, "Maximum peers")
	enableIPv6 := flag.Bool("enable-ipv6", false, "Enable IPv6")
	externalURL := flag.String("external-url", "", "External URL")
	sslCert := flag.String("ssl-cert", "", "SSL certificate")
	sslKey := flag.String("ssl-key", "", "SSL key")
	allowedIPs := flag.String("allowed-ips", "", "Allowed IPs")
	enableStats := flag.Bool("enable-stats", true, "Enable stats")

	flag.Parse()

	config := &trackerhttp.ServerConfig{
		Port:             *port,
		Storage:          *storage,
		DatabasePath:     *dbPath,
		AnnounceInterval: *announceInterval,
		MaxPeers:         *maxPeers,
		EnableIPv6:       *enableIPv6,
		ExternalURL:      *externalURL,
		SSLCert:          *sslCert,
		SSLKey:           *sslKey,
		AllowedIPs:       parseAllowedIPs(*allowedIPs),
		EnableStats:      *enableStats,
	}

	server, err := trackerhttp.NewServer(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create tracker server: %v\n", err)
		os.Exit(1)
	}

	log.Info("starting tracker server", "sub", "tracker", "port", *port)
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start tracker server: %v\n", err)
		os.Exit(1)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Info("shutting down tracker server", "sub", "tracker")
	server.Shutdown()
	log.Info("tracker server stopped", "sub", "tracker")
}

func parseAllowedIPs(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, ip := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(ip); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
