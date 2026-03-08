package config

import (
	"time"

	bittorrent "github.com/cbluth/bittorrent/pkg/client"
)

// Runtime represents runtime configuration for the BitTorrent CLI
// This is different from the persistent config file (operations.go)
// and is used to hold CLI flags and runtime state
type Runtime struct {
	// CLI Flags
	TrackersFile        string
	Port                uint
	Timeout             time.Duration
	ResolveTimeout      time.Duration
	NoDHT               bool
	NoTracker           bool
	DHTPort             uint
	DHTNodes            int
	DHTBootstrapTimeout time.Duration
	LogFile             string

	// Runtime State
	CacheDir  string
	CacheFile string

	// Client instance (initialized at runtime)
	Client *bittorrent.Client
}

// NewRuntime creates a new runtime configuration with defaults
func NewRuntime() *Runtime {
	return &Runtime{
		Port:                6881,
		ResolveTimeout:      2 * time.Minute,
		NoDHT:               false,
		NoTracker:           false,
		DHTPort:             6881,
		DHTNodes:            100,
		DHTBootstrapTimeout: 60 * time.Second,
		Timeout:             15 * time.Second,
	}
}
