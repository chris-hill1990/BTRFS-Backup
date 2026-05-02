package main

import (
	"btrfs-backup/internal/catalog"
	cfgpkg "btrfs-backup/internal/config"
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(value string) error {
	if value == "" {
		return nil
	}
	*values = append(*values, value)
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags)
	syscall.Umask(0077)

	defaultScriptDir := cfgpkg.EnvString("BACKUP_SCRIPT_DIR", cfgpkg.DefaultScriptDir())
	defaultConfigFile := cfgpkg.EnvString("CONFIG_FILE", "/etc/btrfs-backup.conf")
	defaultSource := cfgpkg.EnvString("CATALOGD_SOURCE", "any")
	defaultRunNow := cfgpkg.EnvBool("CATALOGD_RUN_NOW", true)

	var scriptDir string
	var configFile string
	var source string
	var intervalFlag time.Duration
	var workersFlag int
	var progressIntervalFlag int64
	var runNow bool
	var once bool
	var resetCache bool
	var prune bool
	var pruneLocation string
	var pruneSnapshot string
	var query bool
	var versions bool
	var queryPath string
	var volumes stringList

	flag.StringVar(&scriptDir, "script-dir", defaultScriptDir, "directory containing backup-common.sh")
	flag.StringVar(&configFile, "config", defaultConfigFile, "backup config file")
	flag.StringVar(&source, "source", defaultSource, "snapshot source: local, remote, or any")
	flag.DurationVar(&intervalFlag, "interval", 0, "time between daemon cache runs")
	flag.IntVar(&workersFlag, "workers", 0, "number of snapshots to index in parallel")
	flag.Int64Var(&progressIntervalFlag, "progress-interval", -1, "log every N indexed entries per snapshot; 0 disables entry-count progress")
	flag.BoolVar(&runNow, "run-now", defaultRunNow, "run one cache pass immediately before sleeping")
	flag.BoolVar(&once, "once", false, "run one cache pass and exit")
	flag.BoolVar(&resetCache, "reset-cache", false, "clear SQLite cache, signature, checkpoint, and legacy TSV files before indexing")
	flag.BoolVar(&prune, "prune", false, "remove one deleted snapshot from the SQLite catalog and exit")
	flag.StringVar(&pruneLocation, "location", "", "snapshot location for --prune: local or remote")
	flag.StringVar(&pruneSnapshot, "snapshot", "", "snapshot name for --prune")
	flag.BoolVar(&query, "query", false, "query the SQLite catalog and exit")
	flag.BoolVar(&versions, "versions", false, "query SQLite catalog versions for --path and exit")
	flag.StringVar(&queryPath, "path", "", "relative catalog path for --query or --versions")
	flag.Var(&volumes, "volume", "volume to cache; may be repeated; default is all configured volumes")
	flag.Parse()

	if envVolumes := strings.TrimSpace(os.Getenv("CATALOGD_VOLUMES")); len(volumes) == 0 && envVolumes != "" {
		for _, volume := range strings.Split(envVolumes, ",") {
			volume = strings.TrimSpace(volume)
			if volume != "" {
				volumes = append(volumes, volume)
			}
		}
	}

	if source != "local" && source != "remote" && source != "any" {
		log.Fatalf("source must be local, remote, or any: %s", source)
	}

	config, err := cfgpkg.Load(scriptDir, configFile)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if intervalFlag > 0 {
		config.CatalogdInterval = intervalFlag
	}
	if workersFlag > 0 {
		config.CatalogWorkers = workersFlag
	}
	if progressIntervalFlag >= 0 {
		config.CatalogProgressInterval = progressIntervalFlag
	}

	if query {
		if err := catalog.RunQuery(context.Background(), config, source, volumes, catalog.CleanRelativePath(queryPath), os.Stdout); err != nil {
			log.Printf("catalog query failed: %v", err)
			os.Exit(1)
		}
		return
	}

	if prune {
		if err := catalog.RunPrunePass(context.Background(), config, source, volumes, pruneLocation, pruneSnapshot); err != nil {
			log.Printf("catalog prune failed: %v", err)
			os.Exit(1)
		}
		return
	}

	if versions {
		if err := catalog.RunVersionQuery(context.Background(), config, source, volumes, catalog.CleanRelativePath(queryPath), os.Stdout); err != nil {
			log.Printf("catalog versions query failed: %v", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("catalog daemon starting: source=%s interval=%s workers=%d cache_dir=%s volumes=%s",
		source, config.CatalogdInterval, config.CatalogWorkers, config.CacheDir, volumes.String())

	if once || runNow {
		if err := catalog.RunCachePass(ctx, config, source, volumes, resetCache); err != nil {
			log.Printf("cache pass failed: %v", err)
			if once {
				os.Exit(1)
			}
		}
		if once {
			return
		}
	}

	timer := time.NewTimer(config.CatalogdInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("catalog daemon stopping")
			return
		case <-timer.C:
			if err := catalog.RunCachePass(ctx, config, source, volumes, false); err != nil {
				log.Printf("cache pass failed: %v", err)
			}
			timer.Reset(config.CatalogdInterval)
		}
	}
}
