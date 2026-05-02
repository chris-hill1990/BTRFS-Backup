package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanRelativePathAndLivePath(t *testing.T) {
	pathCases := map[string]string{
		"":              "",
		".":             "",
		"/etc/passwd":   "etc/passwd",
		"./etc/passwd":  "etc/passwd",
		" etc/passwd/ ": "etc/passwd",
	}

	for input, want := range pathCases {
		if got := CleanRelativePath(input); got != want {
			t.Fatalf("CleanRelativePath(%q) = %q, want %q", input, got, want)
		}
	}

	liveCases := map[[2]string]string{
		{"/", "etc/passwd"}:       "/etc/passwd",
		{"/mnt/drive3", "docs/a"}: "/mnt/drive3/docs/a",
		{"/mnt/drive3", ""}:       "/mnt/drive3",
	}

	for input, want := range liveCases {
		if got := livePathFor(input[0], input[1]); got != want {
			t.Fatalf("livePathFor(%q, %q) = %q, want %q", input[0], input[1], got, want)
		}
	}
}

func TestListSnapshotsFiltersAndSorts(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "root-2026-04-29"))
	mkdirAll(t, filepath.Join(dir, "root-2026-04-27"))
	mkdirAll(t, filepath.Join(dir, "root-not-a-date"))
	mkdirAll(t, filepath.Join(dir, "drive3-2026-04-28"))
	writeFile(t, filepath.Join(dir, "root-2026-04-30"), "not a directory")

	got, err := listSnapshots(dir, "root")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"root-2026-04-27", "root-2026-04-29"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("listSnapshots() = %#v, want %#v", got, want)
	}
}

func TestQueryCatalogDBOnlyShowsCompletedSnapshots(t *testing.T) {
	tmp := t.TempDir()
	live := filepath.Join(tmp, "live")
	cacheDir := filepath.Join(tmp, "cache")
	mkdirAll(t, filepath.Join(live, "etc"))
	mkdirAll(t, cacheDir)
	writeFile(t, filepath.Join(live, "etc/passwd"), "current")

	config := appConfig{CacheDir: cacheDir}
	volume := volumeConfig{Name: "test", Source: live}
	db, err := openCatalogDB(filepath.Join(cacheDir, "catalog-test-any.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	entries := []catalogEntry{
		{Location: "local", Snapshot: "test-2026-04-28", Name: "passwd", EntryType: "file", RelativePath: "etc/passwd", ParentPath: "etc"},
		{Location: "local", Snapshot: "test-2026-04-29", Name: "shadow", EntryType: "file", RelativePath: "etc/shadow", ParentPath: "etc"},
	}
	if err := insertCatalogEntries(db, entries); err != nil {
		t.Fatal(err)
	}
	if err := markSnapshotDone(db, snapshotRecord{Location: "local", Snapshot: "test-2026-04-28"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := queryCatalogDB(config, volume, "any", "etc", &out); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("query returned %d lines: %q", len(lines), out.String())
	}

	fields := strings.Split(lines[0], "\t")
	if len(fields) != 10 {
		t.Fatalf("query returned %d fields: %q", len(fields), lines[0])
	}
	if fields[0] != "passwd" || fields[2] != "etc/passwd" || fields[4] != "1" || fields[8] != "1" || fields[9] != "0" {
		t.Fatalf("unexpected query row: %#v", fields)
	}
	if strings.Contains(out.String(), "shadow") {
		t.Fatalf("query exposed row from incomplete snapshot: %q", out.String())
	}

	out.Reset()
	if err := queryCatalogVersionsDB(config, volume, "any", "etc/passwd", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "test\tlocal\ttest-2026-04-28") || strings.Contains(out.String(), "test-2026-04-29") {
		t.Fatalf("versions query returned unexpected rows: %q", out.String())
	}
}

func TestPruneSnapshotRowsRemovesEntriesAndOrphanPaths(t *testing.T) {
	tmp := t.TempDir()
	live := filepath.Join(tmp, "live")
	cacheDir := filepath.Join(tmp, "cache")
	mkdirAll(t, filepath.Join(live, "etc"))
	mkdirAll(t, cacheDir)
	writeFile(t, filepath.Join(live, "etc/passwd"), "current")

	config := appConfig{CacheDir: cacheDir}
	volume := volumeConfig{Name: "test", Source: live}
	db, err := openCatalogDB(filepath.Join(cacheDir, "catalog-test-any.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	entries := []catalogEntry{
		{Location: "local", Snapshot: "test-2026-04-28", Name: "passwd", EntryType: "file", RelativePath: "etc/passwd", ParentPath: "etc"},
		{Location: "local", Snapshot: "test-2026-04-28", Name: "oldonly", EntryType: "file", RelativePath: "etc/oldonly", ParentPath: "etc"},
		{Location: "remote", Snapshot: "test-2026-04-28", Name: "passwd", EntryType: "file", RelativePath: "etc/passwd", ParentPath: "etc"},
	}
	if err := insertCatalogEntries(db, entries); err != nil {
		t.Fatal(err)
	}
	if err := markSnapshotDone(db, snapshotRecord{Location: "local", Snapshot: "test-2026-04-28"}); err != nil {
		t.Fatal(err)
	}
	if err := markSnapshotDone(db, snapshotRecord{Location: "remote", Snapshot: "test-2026-04-28"}); err != nil {
		t.Fatal(err)
	}

	prunedEntries, prunedPaths, err := pruneSnapshotRows(db, "local", "test-2026-04-28")
	if err != nil {
		t.Fatal(err)
	}
	if prunedEntries != 2 || prunedPaths != 1 {
		t.Fatalf("pruned entries=%d paths=%d, want 2 entries and 1 path", prunedEntries, prunedPaths)
	}

	var out bytes.Buffer
	if err := queryCatalogDB(config, volume, "any", "etc", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "passwd") {
		t.Fatalf("prune removed shared path still present in remote snapshot: %q", out.String())
	}
	if strings.Contains(out.String(), "oldonly") {
		t.Fatalf("prune left orphan-only path visible: %q", out.String())
	}

	out.Reset()
	if err := queryCatalogVersionsDB(config, volume, "any", "etc/passwd", &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\tlocal\t") || !strings.Contains(out.String(), "\tremote\t") {
		t.Fatalf("versions query returned unexpected pruned rows: %q", out.String())
	}
}

func TestOpenCatalogDBRejectsLegacyFlatSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE entries (
		location TEXT NOT NULL,
		snapshot TEXT NOT NULL,
		name TEXT NOT NULL,
		type TEXT NOT NULL,
		relative_path TEXT NOT NULL,
		parent_path TEXT NOT NULL,
		PRIMARY KEY (location, snapshot, relative_path)
	)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	opened, err := openCatalogDB(dbPath)
	if err == nil {
		_ = opened.Close()
		t.Fatal("expected legacy flat schema to fail")
	}
	if !strings.Contains(err.Error(), "legacy flat catalog schema") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildCatalogCacheIndexesSyntheticSnapshot(t *testing.T) {
	tmp := t.TempDir()
	live := filepath.Join(tmp, "live")
	cacheDir := filepath.Join(tmp, "cache")
	mkdirAll(t, filepath.Join(live, ".snapshots/test-2026-04-29/etc"))
	writeFile(t, filepath.Join(live, ".snapshots/test-2026-04-29/etc/passwd"), "snapshot")

	config := appConfig{
		CacheDir:                cacheDir,
		CatalogWorkers:          1,
		CatalogProgressInterval: 1000,
	}
	volume := volumeConfig{Name: "test", Source: live}

	if err := buildCatalogCache(context.Background(), config, volume, "local", true); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := queryCatalogDB(config, volume, "local", "etc", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "passwd\tfile\tetc/passwd") {
		t.Fatalf("query missing indexed snapshot row: %q", out.String())
	}

	cacheBase := filepath.Join(cacheDir, "catalog-test-local")
	db, err := openCatalogDB(cacheBase + ".db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	done, err := readDBDone(db)
	if err != nil {
		t.Fatal(err)
	}
	if !done[snapshotKey("local", "test-2026-04-29")] {
		t.Fatalf("snapshot was not marked done: %#v", done)
	}

	assertMode(t, cacheDir, 0700)
	assertMode(t, cacheBase+".db", 0600)
	assertMode(t, cacheBase+".sig", 0600)
	assertNotExists(t, cacheBase+".tsv")
	assertNotExists(t, cacheBase+".done")
}

func TestBuildCatalogCacheIgnoresLegacyDoneLogMissingSQLite(t *testing.T) {
	tmp := t.TempDir()
	live := filepath.Join(tmp, "live")
	cacheDir := filepath.Join(tmp, "cache")
	mkdirAll(t, filepath.Join(live, ".snapshots/test-2026-04-29/etc"))
	writeFile(t, filepath.Join(live, ".snapshots/test-2026-04-29/etc/passwd"), "snapshot")
	mkdirAll(t, cacheDir)
	writeFile(t, filepath.Join(cacheDir, "catalog-test-local.done"), "local\ttest-2026-04-29\n")

	config := appConfig{
		CacheDir:                cacheDir,
		CatalogWorkers:          1,
		CatalogProgressInterval: 1,
		Volumes:                 []volumeConfig{{Name: "test", Source: live}},
	}

	if err := buildCatalogCache(context.Background(), config, config.Volumes[0], "local", false); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := queryCatalogDB(config, config.Volumes[0], "local", "etc", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "passwd\tfile\tetc/passwd") {
		t.Fatalf("query missing indexed snapshot row: %q", out.String())
	}
	assertNotExists(t, filepath.Join(cacheDir, "catalog-test-local.tsv"))
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

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func assertNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s exists, want absent", path)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
