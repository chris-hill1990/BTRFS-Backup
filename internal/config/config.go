package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Volume struct {
	Name   string
	Source string
}

type AppConfig struct {
	BackupMount             string
	LocalKeep               int
	RemoteRetentionDays     int
	CacheDir                string
	CatalogWorkers          int
	CatalogProgressInterval int64
	CatalogdInterval        time.Duration
	Volumes                 []Volume
}

func Load(scriptDir string, configFile string) (AppConfig, error) {
	commonPath := filepath.Join(scriptDir, "backup-common.sh")
	script := `
set -euo pipefail
source "$1"
printf 'BACKUP_MOUNT\t%s\n' "$BACKUP_MOUNT"
printf 'LOCAL_KEEP\t%s\n' "$LOCAL_KEEP"
printf 'REMOTE_RETENTION_DAYS\t%s\n' "$REMOTE_RETENTION_DAYS"
printf 'CACHE_DIR\t%s\n' "$CACHE_DIR"
printf 'CATALOG_WORKERS\t%s\n' "${CATALOG_WORKERS:-}"
printf 'CATALOG_PROGRESS_INTERVAL\t%s\n' "${CATALOG_PROGRESS_INTERVAL:-}"
printf 'CATALOGD_INTERVAL\t%s\n' "${CATALOGD_INTERVAL:-}"
for name in "${VOLUME_NAMES[@]}"; do
  printf 'VOLUME\t%s\t%s\n' "$name" "${VOLUMES[$name]}"
done
`

	cmd := exec.Command("bash", "-c", script, "backup-catalogd-config", commonPath)
	cmd.Env = append(os.Environ(), "CONFIG_FILE="+configFile)

	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return AppConfig{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return AppConfig{}, err
	}

	config := AppConfig{
		BackupMount:             "/mnt/backup",
		LocalKeep:               3,
		RemoteRetentionDays:     365,
		CacheDir:                "/var/cache/btrfs-backup",
		CatalogWorkers:          defaultWorkers(),
		CatalogProgressInterval: 5000,
		CatalogdInterval:        envDuration("CATALOGD_INTERVAL", 30*time.Minute),
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 2 {
			continue
		}

		switch fields[0] {
		case "BACKUP_MOUNT":
			config.BackupMount = fields[1]
		case "LOCAL_KEEP":
			if fields[1] != "" {
				config.LocalKeep = parseInt(fields[1], config.LocalKeep)
			}
		case "REMOTE_RETENTION_DAYS":
			if fields[1] != "" {
				config.RemoteRetentionDays = parseInt(fields[1], config.RemoteRetentionDays)
			}
		case "CACHE_DIR":
			config.CacheDir = fields[1]
		case "CATALOG_WORKERS":
			if fields[1] != "" {
				config.CatalogWorkers = parseInt(fields[1], config.CatalogWorkers)
			}
		case "CATALOG_PROGRESS_INTERVAL":
			if fields[1] != "" {
				config.CatalogProgressInterval = int64(parseInt(fields[1], int(config.CatalogProgressInterval)))
			}
		case "CATALOGD_INTERVAL":
			if fields[1] != "" {
				config.CatalogdInterval = parseDuration(fields[1], config.CatalogdInterval)
			}
		case "VOLUME":
			if len(fields) >= 3 {
				config.Volumes = append(config.Volumes, Volume{Name: fields[1], Source: fields[2]})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return AppConfig{}, err
	}

	if config.CatalogWorkers < 1 {
		config.CatalogWorkers = 1
	}
	if config.LocalKeep < 0 {
		config.LocalKeep = 0
	}
	if config.RemoteRetentionDays < 0 {
		config.RemoteRetentionDays = 0
	}
	if config.CatalogdInterval <= 0 {
		config.CatalogdInterval = 30 * time.Minute
	}

	return config, nil
}

func FilterVolumes(configured []Volume, requested []string) ([]Volume, error) {
	if len(requested) == 0 {
		return configured, nil
	}

	byName := make(map[string]Volume, len(configured))
	for _, volume := range configured {
		byName[volume.Name] = volume
	}

	volumes := make([]Volume, 0, len(requested))
	for _, name := range requested {
		volume, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown volume: %s", name)
		}
		volumes = append(volumes, volume)
	}

	return volumes, nil
}

func defaultWorkers() int {
	workers := runtime.NumCPU()
	if workers > 4 {
		return 4
	}
	if workers < 1 {
		return 1
	}
	return workers
}

func DefaultScriptDir() string {
	executable, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(executable)
}

func EnvString(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func EnvBool(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback
	}

	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		fmt.Fprintf(os.Stderr, "invalid %s=%q, using %t\n", name, value, fallback)
		return fallback
	}
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return parseDuration(value, fallback)
}

func parseDuration(value string, fallback time.Duration) time.Duration {
	duration, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid duration %q, using %s\n", value, fallback)
		return fallback
	}
	return duration
}

func parseInt(value string, fallback int) int {
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "invalid integer %q, using %d\n", value, fallback)
		return fallback
	}
	return parsed
}
