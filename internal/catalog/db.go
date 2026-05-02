package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

const catalogSchemaVersion = "2"

func readDBDone(db *sql.DB) (map[string]bool, error) {
	doneKeys := make(map[string]bool)

	rows, err := db.Query(`SELECT location, snapshot FROM snapshots WHERE done = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var location string
		var snapshot string
		if err := rows.Scan(&location, &snapshot); err != nil {
			return nil, err
		}
		doneKeys[snapshotKey(location, snapshot)] = true
	}

	return doneKeys, rows.Err()
}

func openCatalogDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA busy_timeout=5000",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	if err := ensureCatalogSchemaCompatible(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := initCatalogTables(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := initCatalogIndexes(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := chmodBestEffort(path, path+"-wal", path+"-shm"); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

func initCatalogTables(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			id INTEGER PRIMARY KEY,
			location TEXT NOT NULL,
			snapshot TEXT NOT NULL,
			done INTEGER NOT NULL DEFAULT 1,
			UNIQUE (location, snapshot)
		)`,
		`CREATE TABLE IF NOT EXISTS paths (
			id INTEGER PRIMARY KEY,
			relative_path TEXT NOT NULL UNIQUE,
			parent_path TEXT NOT NULL,
			name TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS entries (
			snapshot_id INTEGER NOT NULL,
			path_id INTEGER NOT NULL,
			type TEXT NOT NULL,
			PRIMARY KEY (snapshot_id, path_id),
			FOREIGN KEY (snapshot_id) REFERENCES snapshots(id),
			FOREIGN KEY (path_id) REFERENCES paths(id)
		)`,
	}

	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}

	return setCatalogMeta(db, "schema_version", catalogSchemaVersion)
}

func initCatalogIndexes(db *sql.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_paths_parent_name ON paths(parent_path, name, id)`,
		`CREATE INDEX IF NOT EXISTS idx_entries_path_snapshot_type ON entries(path_id, snapshot_id, type)`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_done_location ON snapshots(done, location, id, snapshot)`,
	}

	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}

	return nil
}

func ensureCatalogSchemaCompatible(db *sql.DB) error {
	var createSQL string
	err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = 'entries'`).Scan(&createSQL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	normalized := strings.Contains(createSQL, "snapshot_id") && strings.Contains(createSQL, "path_id")
	if normalized {
		return nil
	}

	return fmt.Errorf("legacy flat catalog schema found; remove the catalog DB or run cache --reset-cache to rebuild with schema v%s", catalogSchemaVersion)
}

func parentPathFor(relativePath string) string {
	index := strings.LastIndex(relativePath, "/")
	if index < 0 {
		return ""
	}
	return relativePath[:index]
}

func insertCatalogEntries(db *sql.DB, entries []catalogEntry) error {
	if len(entries) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insertSnapshotStmt, err := tx.Prepare(`INSERT INTO snapshots(location, snapshot, done)
		VALUES (?, ?, 0)
		ON CONFLICT(location, snapshot) DO NOTHING`)
	if err != nil {
		return err
	}
	defer insertSnapshotStmt.Close()

	selectSnapshotStmt, err := tx.Prepare(`SELECT id FROM snapshots WHERE location = ? AND snapshot = ?`)
	if err != nil {
		return err
	}
	defer selectSnapshotStmt.Close()

	insertPathStmt, err := tx.Prepare(`INSERT INTO paths(relative_path, parent_path, name)
		VALUES (?, ?, ?)
		ON CONFLICT(relative_path) DO NOTHING`)
	if err != nil {
		return err
	}
	defer insertPathStmt.Close()

	selectPathStmt, err := tx.Prepare(`SELECT id FROM paths WHERE relative_path = ?`)
	if err != nil {
		return err
	}
	defer selectPathStmt.Close()

	insertEntryStmt, err := tx.Prepare(`INSERT OR IGNORE INTO entries(snapshot_id, path_id, type)
		VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insertEntryStmt.Close()

	snapshotIDs := make(map[string]int64)
	pathIDs := make(map[string]int64, len(entries))

	for _, entry := range entries {
		snapshotID, err := catalogSnapshotID(insertSnapshotStmt, selectSnapshotStmt, snapshotIDs, entry.Location, entry.Snapshot)
		if err != nil {
			return err
		}
		pathID, err := catalogPathID(insertPathStmt, selectPathStmt, pathIDs, entry)
		if err != nil {
			return err
		}
		if _, err := insertEntryStmt.Exec(snapshotID, pathID, entry.EntryType); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func catalogSnapshotID(insertStmt *sql.Stmt, selectStmt *sql.Stmt, cache map[string]int64, location string, snapshot string) (int64, error) {
	key := snapshotKey(location, snapshot)
	if id, ok := cache[key]; ok {
		return id, nil
	}

	if _, err := insertStmt.Exec(location, snapshot); err != nil {
		return 0, err
	}

	var id int64
	if err := selectStmt.QueryRow(location, snapshot).Scan(&id); err != nil {
		return 0, err
	}
	cache[key] = id
	return id, nil
}

func catalogPathID(insertStmt *sql.Stmt, selectStmt *sql.Stmt, cache map[string]int64, entry catalogEntry) (int64, error) {
	if id, ok := cache[entry.RelativePath]; ok {
		return id, nil
	}

	if _, err := insertStmt.Exec(entry.RelativePath, entry.ParentPath, entry.Name); err != nil {
		return 0, err
	}

	var id int64
	if err := selectStmt.QueryRow(entry.RelativePath).Scan(&id); err != nil {
		return 0, err
	}
	cache[entry.RelativePath] = id
	return id, nil
}

func markSnapshotDone(db *sql.DB, record snapshotRecord) error {
	_, err := db.Exec(`INSERT INTO snapshots(location, snapshot, done)
		VALUES (?, ?, 1)
		ON CONFLICT(location, snapshot) DO UPDATE SET done = 1`,
		record.Location, record.Snapshot)
	return err
}

func setCatalogMeta(db *sql.DB, key string, value string) error {
	_, err := db.Exec(`INSERT INTO meta(key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
