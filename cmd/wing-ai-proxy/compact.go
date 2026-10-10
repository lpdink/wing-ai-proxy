package main

import (
	"fmt"

	"github.com/abiter/wing-ai-proxy/internal/audit"
	"github.com/abiter/wing-ai-proxy/internal/config"
)

// runCompact implements the "compact" subcommand: it gzip-compresses legacy
// plain-TEXT audit bodies in place and optionally vacuums the database.
//
// Usage: wing-ai-proxy compact [--vacuum] [config-path]
func runCompact(args []string) error {
	vacuum := false
	configPath := ""
	for _, arg := range args {
		switch arg {
		case "--vacuum":
			vacuum = true
		default:
			if configPath != "" {
				return fmt.Errorf("usage: wing-ai-proxy compact [--vacuum] [config-path]")
			}
			configPath = arg
		}
	}
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Database.Driver != "sqlite" {
		return fmt.Errorf("compact: only the sqlite driver is supported, got %q", cfg.Database.Driver)
	}

	fmt.Printf("compacting audit database %s\n", cfg.Database.DSN)

	result, err := audit.Compact(cfg.Database.DSN, audit.CompactOptions{
		Vacuum: vacuum,
		Progress: func(stats audit.CompactStats) {
			fmt.Printf("progress: rows=%d input=%d bytes output=%d bytes\n",
				stats.RowsProcessed, stats.InputBytes, stats.OutputBytes)
		},
	})
	if err != nil {
		return err
	}

	if result.RowsProcessed > 0 {
		fmt.Printf("compacted %d rows: input %d bytes -> output %d bytes\n",
			result.RowsProcessed, result.InputBytes, result.OutputBytes)
	} else {
		fmt.Println("no legacy text bodies found")
	}

	if vacuum {
		fmt.Printf("vacuum reclaimed %d bytes\n", result.ReclaimedBytes)
	} else if result.RowsProcessed > 0 {
		fmt.Println("tip: run again with --vacuum to reclaim disk space")
	}
	return nil
}
