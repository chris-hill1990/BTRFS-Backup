package snapshots

import (
	"btrfs-backup/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListFiltersAndSortsSnapshots(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "root-2026-04-29"))
	mkdirAll(t, filepath.Join(dir, "root-2026-04-27"))
	mkdirAll(t, filepath.Join(dir, "root-not-a-date"))
	mkdirAll(t, filepath.Join(dir, "drive3-2026-04-28"))
	writeFile(t, filepath.Join(dir, "root-2026-04-30"), "not a directory")

	got, err := List(dir, "root")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"root-2026-04-27", "root-2026-04-29"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("List() = %#v, want %#v", got, want)
	}
}

func TestSnapshotDirForRoot(t *testing.T) {
	if got := SnapshotDirFor("/"); got != "/.snapshots" {
		t.Fatalf("SnapshotDirFor(/) = %q", got)
	}
	if got := SnapshotDirFor("/mnt/drive3"); got != "/mnt/drive3/.snapshots" {
		t.Fatalf("SnapshotDirFor(/mnt/drive3) = %q", got)
	}
}

func TestRetentionRecordsKeepsNewestLocalSnapshots(t *testing.T) {
	tmp := t.TempDir()
	live := filepath.Join(tmp, "live")
	snapshotDir := filepath.Join(live, ".snapshots")
	mkdirAll(t, filepath.Join(snapshotDir, "test-2026-04-27"))
	mkdirAll(t, filepath.Join(snapshotDir, "test-2026-04-28"))
	mkdirAll(t, filepath.Join(snapshotDir, "test-2026-04-29"))

	cfg := config.AppConfig{
		LocalKeep: 2,
		Volumes:   []config.Volume{{Name: "test", Source: live}},
	}

	records, err := RetentionRecords(cfg, "local", "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d retention records, want 1: %#v", len(records), records)
	}
	if records[0].Snapshot != "test-2026-04-27" || records[0].Location != "local" {
		t.Fatalf("unexpected retention record: %#v", records[0])
	}
}

func TestRetentionRecordsUsesRemoteSnapshotMtime(t *testing.T) {
	tmp := t.TempDir()
	backupMount := filepath.Join(tmp, "backup")
	remoteDir := filepath.Join(backupMount, "test")
	oldPath := filepath.Join(remoteDir, "test-2026-04-01")
	newPath := filepath.Join(remoteDir, "test-2026-04-30")
	mkdirAll(t, oldPath)
	mkdirAll(t, newPath)

	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-3 * 24 * time.Hour)
	newTime := now.Add(-24 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	cfg := config.AppConfig{
		BackupMount:         backupMount,
		RemoteRetentionDays: 1,
		Volumes:             []config.Volume{{Name: "test", Source: filepath.Join(tmp, "live")}},
	}

	records, err := RetentionRecords(cfg, "remote", "test", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d retention records, want 1: %#v", len(records), records)
	}
	if records[0].Snapshot != "test-2026-04-01" || records[0].Path != oldPath {
		t.Fatalf("unexpected retention record: %#v", records[0])
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}
