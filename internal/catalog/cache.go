package catalog

import (
	"btrfs-backup/internal/snapshots"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

func RunCachePass(ctx context.Context, config appConfig, sourceFilter string, volumeNames []string, resetCache bool) error {
	volumes, err := filterVolumes(config.Volumes, volumeNames)
	if err != nil {
		return err
	}

	var errs []error
	for _, volume := range volumes {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := buildCatalogCache(ctx, config, volume, sourceFilter, resetCache); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", volume.Name, err))
		}
	}

	return errors.Join(errs...)
}

func buildCatalogCache(ctx context.Context, config appConfig, volume volumeConfig, sourceFilter string, resetCache bool) error {
	cacheBase := filepath.Join(config.CacheDir, fmt.Sprintf("catalog-%s-%s", volume.Name, sourceFilter))
	sigFile := cacheBase + ".sig"
	dbFile := cacheBase + ".db"
	legacyTSVFile := cacheBase + ".tsv"
	legacyDoneFile := cacheBase + ".done"

	if err := ensurePrivateCacheDir(config.CacheDir); err != nil {
		return err
	}

	if resetCache {
		log.Printf("resetting catalog cache: %s", cacheBase)
		if err := removeCacheFiles(legacyTSVFile, legacyDoneFile, sigFile); err != nil {
			return err
		}
		if err := removeCheckpointFiles(cacheBase); err != nil {
			return err
		}
		if err := removeCacheFiles(dbFile, dbFile+"-wal", dbFile+"-shm"); err != nil {
			return err
		}
	}

	db, err := openCatalogDB(dbFile)
	if err != nil {
		return err
	}
	defer db.Close()

	doneKeys, err := readDBDone(db)
	if err != nil {
		return err
	}

	records, err := catalogSnapshotRoots(config, volume, sourceFilter)
	if err != nil {
		return err
	}
	for i := range records {
		records[i].Index = i + 1
		records[i].Total = len(records)
	}

	log.Printf("catalog cache: %s (%s), %d snapshot(s)", volume.Name, sourceFilter, len(records))
	log.Printf("writing catalog cache: %s", dbFile)

	pending := make([]snapshotRecord, 0, len(records))
	skippedCompleted := 0

	for _, record := range records {
		key := snapshotKey(record.Location, record.Snapshot)
		if doneKeys[key] {
			skippedCompleted++
			_ = os.Remove(checkpointFile(cacheBase, record))
			log.Printf("[%d/%d] skipping completed %s snapshot %s", record.Index, record.Total, record.Location, record.Snapshot)
			continue
		}
		pending = append(pending, record)
	}

	indexed, skippedUnavailable, newEntries, err := indexPendingSnapshots(ctx, pending, config.CatalogWorkers, config.CatalogProgressInterval, cacheBase, dbFile, db)
	if err != nil {
		return err
	}

	signature, err := catalogSnapshotSignature(records)
	if err != nil {
		return err
	}
	if err := writeAtomic(sigFile, []byte(signature+"\n"), privateCacheFileMode); err != nil {
		return err
	}
	if err := setCatalogMeta(db, "signature", signature); err != nil {
		return err
	}

	if err := chmodBestEffort(dbFile, dbFile+"-wal", dbFile+"-shm", sigFile); err != nil {
		return err
	}

	log.Printf("catalog cache complete: %s (%d indexed, %d completed skipped, %d unavailable skipped, %d new entries)",
		dbFile, indexed, skippedCompleted, skippedUnavailable, newEntries)
	return nil
}

func removeCacheFiles(paths ...string) error {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func ensurePrivateCacheDir(path string) error {
	if err := os.MkdirAll(path, privateCacheDirMode); err != nil {
		return err
	}
	return os.Chmod(path, privateCacheDirMode)
}

func removeCheckpointFiles(cacheBase string) error {
	matches, err := filepath.Glob(cacheBase + ".*.scan")
	if err != nil {
		return err
	}

	for _, path := range matches {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	return nil
}

func chmodBestEffort(paths ...string) error {
	for _, path := range paths {
		if err := os.Chmod(path, privateCacheFileMode); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := chownToCacheDirOwner(path); err != nil {
			return err
		}
	}
	return nil
}

func chownToCacheDirOwner(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	fileStat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	dirStat, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}

	uid := int(dirStat.Uid)
	gid := int(dirStat.Gid)
	if int(fileStat.Uid) == uid && int(fileStat.Gid) == gid {
		return nil
	}

	if err := os.Chown(path, uid, gid); err != nil && !os.IsPermission(err) {
		return err
	}
	return nil
}

func catalogSnapshotRoots(config appConfig, volume volumeConfig, sourceFilter string) ([]snapshotRecord, error) {
	records := []snapshotRecord{}

	for _, location := range []string{"local", "remote"} {
		if sourceFilter != "any" && location != sourceFilter {
			continue
		}

		directory := snapshotLocationPath(config.BackupMount, location, volume)
		snapshotNames, err := listSnapshots(directory, volume.Name)
		if err != nil {
			return nil, err
		}

		for _, snapshot := range snapshotNames {
			records = append(records, snapshotRecord{
				Location: location,
				Snapshot: snapshot,
				Root:     filepath.Join(directory, snapshot),
			})
		}
	}

	return records, nil
}

func snapshotLocationPath(backupMount string, location string, volume volumeConfig) string {
	return snapshots.LocationPath(backupMount, location, volume)
}

func listSnapshots(directory string, volumeName string) ([]string, error) {
	return snapshots.List(directory, volumeName)
}

func catalogSnapshotSignature(records []snapshotRecord) (string, error) {
	hash := sha256.New()
	for _, record := range records {
		if _, err := fmt.Fprintf(hash, "%s\t%s\t%s\n", record.Location, record.Snapshot, record.Root); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
