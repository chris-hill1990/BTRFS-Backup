package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

func RunPrunePass(ctx context.Context, config appConfig, sourceFilter string, volumeNames []string, location string, snapshot string) error {
	if location != "local" && location != "remote" {
		return fmt.Errorf("--location must be local or remote")
	}
	if snapshot == "" {
		return fmt.Errorf("--snapshot is required for --prune")
	}

	volumes, err := filterVolumes(config.Volumes, volumeNames)
	if err != nil {
		return err
	}
	if len(volumes) != 1 {
		return fmt.Errorf("--prune requires exactly one --volume")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	return pruneCatalogSnapshot(config, volumes[0], sourceFilter, location, snapshot)
}

func pruneCatalogSnapshot(config appConfig, volume volumeConfig, sourceFilter string, location string, snapshot string) error {
	cacheBase := filepath.Join(config.CacheDir, fmt.Sprintf("catalog-%s-%s", volume.Name, sourceFilter))
	dbFile := cacheBase + ".db"

	if _, err := os.Stat(dbFile); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	db, err := openCatalogDB(dbFile)
	if err != nil {
		return err
	}
	defer db.Close()

	prunedEntries, prunedPaths, err := pruneSnapshotRows(db, location, snapshot)
	if err != nil {
		return err
	}
	if err := chmodBestEffort(dbFile, dbFile+"-wal", dbFile+"-shm"); err != nil {
		return err
	}

	log.Printf("pruned catalog snapshot: %s (%s %s, %d entries, %d orphan paths)",
		dbFile, location, snapshot, prunedEntries, prunedPaths)
	return nil
}

func pruneSnapshotRows(db *sql.DB, location string, snapshot string) (int64, int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	var snapshotID int64
	err = tx.QueryRow(`SELECT id FROM snapshots WHERE location = ? AND snapshot = ?`, location, snapshot).Scan(&snapshotID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, tx.Commit()
	}
	if err != nil {
		return 0, 0, err
	}

	statements := []string{
		`CREATE TEMP TABLE IF NOT EXISTS prune_paths(id INTEGER PRIMARY KEY)`,
		`DELETE FROM prune_paths`,
		`INSERT OR IGNORE INTO prune_paths(id)
			SELECT path_id FROM entries WHERE snapshot_id = ?`,
	}
	for _, statement := range statements[:2] {
		if _, err := tx.Exec(statement); err != nil {
			return 0, 0, err
		}
	}
	if _, err := tx.Exec(statements[2], snapshotID); err != nil {
		return 0, 0, err
	}

	result, err := tx.Exec(`DELETE FROM entries WHERE snapshot_id = ?`, snapshotID)
	if err != nil {
		return 0, 0, err
	}
	prunedEntries, _ := result.RowsAffected()

	if _, err := tx.Exec(`DELETE FROM snapshots WHERE id = ?`, snapshotID); err != nil {
		return 0, 0, err
	}

	result, err = tx.Exec(`
		DELETE FROM paths
		WHERE id IN (SELECT id FROM prune_paths)
			AND NOT EXISTS (
				SELECT 1 FROM entries e WHERE e.path_id = paths.id
			)
	`)
	if err != nil {
		return 0, 0, err
	}
	prunedPaths, _ := result.RowsAffected()

	if _, err := tx.Exec(`DELETE FROM prune_paths`); err != nil {
		return 0, 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return prunedEntries, prunedPaths, nil
}
