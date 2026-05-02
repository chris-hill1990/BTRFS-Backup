package snapshots

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"btrfs-backup/internal/config"
)

type Record struct {
	Volume   string `json:"volume"`
	Location string `json:"location"`
	Snapshot string `json:"snapshot"`
	Path     string `json:"path"`
}

type Info struct {
	Name    string
	Path    string
	ModTime time.Time
}

type VolumeStatus struct {
	Name         string `json:"name"`
	Source       string `json:"source"`
	LocalDir     string `json:"local_dir"`
	RemoteDir    string `json:"remote_dir"`
	LocalCount   int    `json:"local_count"`
	LatestLocal  string `json:"latest_local"`
	RemoteCount  int    `json:"remote_count"`
	LatestRemote string `json:"latest_remote"`
}

type Status struct {
	BackupMount   string
	BackupMounted bool
	Volumes       []VolumeStatus
}

func SnapshotDirFor(source string) string {
	if source == "/" {
		return "/.snapshots"
	}
	return filepath.Join(source, ".snapshots")
}

func LocationPath(backupMount string, location string, volume config.Volume) string {
	switch location {
	case "local":
		return SnapshotDirFor(volume.Source)
	case "remote":
		return filepath.Join(backupMount, volume.Name)
	default:
		return ""
	}
}

func List(directory string, volumeName string) ([]string, error) {
	infos, err := ListInfo(directory, volumeName)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(infos))
	for _, info := range infos {
		out = append(out, info.Name)
	}
	return out, nil
}

func ListInfo(directory string, volumeName string) ([]Info, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	prefix := volumeName + "-"
	out := []Info{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		suffix := strings.TrimPrefix(name, prefix)
		if suffix == name || !IsSnapshotDate(suffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, Info{
			Name:    name,
			Path:    filepath.Join(directory, name),
			ModTime: info.ModTime(),
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func Records(cfg config.AppConfig, locationFilter string, volumeFilter string) ([]Record, error) {
	records := []Record{}
	for _, volume := range cfg.Volumes {
		if volumeFilter != "" && volume.Name != volumeFilter {
			continue
		}

		for _, location := range []string{"local", "remote"} {
			if locationFilter != "all" && locationFilter != location {
				continue
			}

			directory := LocationPath(cfg.BackupMount, location, volume)
			names, err := List(directory, volume.Name)
			if err != nil {
				return nil, err
			}
			for _, snapshot := range names {
				records = append(records, Record{
					Volume:   volume.Name,
					Location: location,
					Snapshot: snapshot,
					Path:     filepath.Join(directory, snapshot),
				})
			}
		}
	}
	return records, nil
}

func RetentionRecords(cfg config.AppConfig, locationFilter string, volumeFilter string, now time.Time) ([]Record, error) {
	records := []Record{}
	for _, volume := range cfg.Volumes {
		if volumeFilter != "" && volume.Name != volumeFilter {
			continue
		}

		if locationFilter == "all" || locationFilter == "local" {
			localRecords, err := localRetentionRecords(cfg, volume)
			if err != nil {
				return nil, err
			}
			records = append(records, localRecords...)
		}

		if locationFilter == "all" || locationFilter == "remote" {
			remoteRecords, err := remoteRetentionRecords(cfg, volume, now)
			if err != nil {
				return nil, err
			}
			records = append(records, remoteRecords...)
		}
	}
	return records, nil
}

func BuildStatus(cfg config.AppConfig) (Status, error) {
	status := Status{
		BackupMount:   cfg.BackupMount,
		BackupMounted: backupMountIsMounted(cfg.BackupMount),
	}

	for _, volume := range cfg.Volumes {
		localDir := SnapshotDirFor(volume.Source)
		remoteDir := filepath.Join(cfg.BackupMount, volume.Name)
		localSnapshots, err := List(localDir, volume.Name)
		if err != nil {
			return Status{}, err
		}
		remoteSnapshots, err := List(remoteDir, volume.Name)
		if err != nil {
			return Status{}, err
		}

		status.Volumes = append(status.Volumes, VolumeStatus{
			Name:         volume.Name,
			Source:       volume.Source,
			LocalDir:     localDir,
			RemoteDir:    remoteDir,
			LocalCount:   len(localSnapshots),
			LatestLocal:  latest(localSnapshots),
			RemoteCount:  len(remoteSnapshots),
			LatestRemote: latest(remoteSnapshots),
		})
	}

	return status, nil
}

func ValidateLocationFilter(value string) error {
	switch value {
	case "all", "local", "remote":
		return nil
	default:
		return fmt.Errorf("location filter must be all, local, or remote")
	}
}

func localRetentionRecords(cfg config.AppConfig, volume config.Volume) ([]Record, error) {
	directory := LocationPath(cfg.BackupMount, "local", volume)
	names, err := List(directory, volume.Name)
	if err != nil {
		return nil, err
	}
	if len(names) <= cfg.LocalKeep {
		return nil, nil
	}

	deleteCount := len(names) - cfg.LocalKeep
	records := make([]Record, 0, deleteCount)
	for _, snapshot := range names[:deleteCount] {
		records = append(records, Record{
			Volume:   volume.Name,
			Location: "local",
			Snapshot: snapshot,
			Path:     filepath.Join(directory, snapshot),
		})
	}
	return records, nil
}

func remoteRetentionRecords(cfg config.AppConfig, volume config.Volume, now time.Time) ([]Record, error) {
	directory := LocationPath(cfg.BackupMount, "remote", volume)
	infos, err := ListInfo(directory, volume.Name)
	if err != nil {
		return nil, err
	}

	maxAge := time.Duration(cfg.RemoteRetentionDays+1) * 24 * time.Hour
	records := []Record{}
	for _, info := range infos {
		if now.Sub(info.ModTime) < maxAge {
			continue
		}
		records = append(records, Record{
			Volume:   volume.Name,
			Location: "remote",
			Snapshot: info.Name,
			Path:     info.Path,
		})
	}
	return records, nil
}

func IsSnapshotDate(value string) bool {
	if len(value) != len("2006-01-02") {
		return false
	}
	if value[4] != '-' || value[7] != '-' {
		return false
	}
	for i, char := range value {
		if i == 4 || i == 7 {
			continue
		}
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func latest(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func backupMountIsMounted(path string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	target := " " + path + " "
	return strings.Contains(string(data), target)
}
