package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

func indexPendingSnapshots(ctx context.Context, records []snapshotRecord, workers int, progressInterval int64, cacheBase string, dbFile string, db *sql.DB) (int, int, int64, error) {
	if workers < 1 {
		workers = 1
	}

	log.Printf("go catalog indexer: %d worker(s), %d pending snapshot(s)", workers, len(records))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan snapshotRecord)
	results := make(chan indexResult)
	var wg sync.WaitGroup
	committer := &cacheCommitter{db: db, dbFile: dbFile}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for record := range jobs {
				results <- indexSnapshot(ctx, record, cacheBase, progressInterval, committer)
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, record := range records {
			select {
			case jobs <- record:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	indexed := 0
	skippedUnavailable := 0
	var newEntries int64
	var firstErr error

	for result := range results {
		if result.Err != nil {
			cancel()
			if firstErr == nil {
				firstErr = result.Err
			}
			log.Printf("[%d/%d] error indexing %s: %v", result.Record.Index, result.Record.Total, result.Record.Snapshot, result.Err)
			continue
		}

		if result.Unavailable {
			skippedUnavailable++
			if err := committer.finishSnapshot(result.Record, checkpointFile(cacheBase, result.Record)); err != nil {
				cancel()
				if firstErr == nil {
					firstErr = err
				}
			}
			continue
		}

		indexed++
		newEntries += result.EntryCount
	}

	return indexed, skippedUnavailable, newEntries, firstErr
}

func indexSnapshot(ctx context.Context, record snapshotRecord, cacheBase string, progressInterval int64, committer *cacheCommitter) indexResult {
	info, err := os.Lstat(record.Root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		log.Printf("[%d/%d] skipping unavailable snapshot %s", record.Index, record.Total, record.Snapshot)
		return indexResult{Record: record, Unavailable: true}
	}

	checkpointPath := checkpointFile(cacheBase, record)
	checkpoint, err := readCheckpoint(checkpointPath)
	if err != nil {
		return indexResult{Record: record, Err: err}
	}

	if checkpoint.RelativePath != "" {
		log.Printf("[%d/%d] resuming %s snapshot %s after %q (%d entries already committed)",
			record.Index, record.Total, record.Location, record.Snapshot, checkpoint.RelativePath, checkpoint.EntryCount)
	} else {
		log.Printf("[%d/%d] indexing %s snapshot %s", record.Index, record.Total, record.Location, record.Snapshot)
	}

	commitInterval := progressInterval
	if commitInterval <= 0 {
		commitInterval = 5000
	}

	count := checkpoint.EntryCount
	lastRelative := checkpoint.RelativePath
	batchSize := int(commitInterval)
	if batchSize < 1 {
		batchSize = 1
	}
	batch := make([]catalogEntry, 0, batchSize)

	err = walkSorted(ctx, record.Root, func(path string, entry fs.DirEntry) (bool, error) {
		relative, err := filepath.Rel(record.Root, path)
		if err != nil {
			return true, err
		}
		relative = filepath.ToSlash(relative)

		if checkpoint.RelativePath != "" && relative <= checkpoint.RelativePath {
			return shouldDescendPastCheckpoint(relative, checkpoint.RelativePath, entry), nil
		}

		count++

		if progressInterval > 0 && count%progressInterval == 0 {
			log.Printf("[%d/%d] %s: %d entries indexed...", record.Index, record.Total, record.Snapshot, count)
		}

		entryType, err := snapshotEntryType(entry)
		if err != nil {
			return true, err
		}

		lastRelative = relative

		batch = append(batch, catalogEntry{
			Location:     record.Location,
			Snapshot:     record.Snapshot,
			Name:         entry.Name(),
			EntryType:    entryType,
			RelativePath: relative,
			ParentPath:   parentPathFor(relative),
		})

		if len(batch) >= batchSize {
			if err := committer.commitBatch(checkpointPath, batch, checkpointState{RelativePath: lastRelative, EntryCount: count}); err != nil {
				return true, err
			}
			batch = batch[:0]
		}

		return true, nil
	})
	if err != nil {
		return indexResult{Record: record, Err: err}
	}

	if len(batch) > 0 {
		if err := committer.commitBatch(checkpointPath, batch, checkpointState{RelativePath: lastRelative, EntryCount: count}); err != nil {
			return indexResult{Record: record, Err: err}
		}
	}

	if err := committer.finishSnapshot(record, checkpointPath); err != nil {
		return indexResult{Record: record, Err: err}
	}

	log.Printf("[%d/%d] finished %s: %d entries", record.Index, record.Total, record.Snapshot, count)
	return indexResult{Record: record, EntryCount: count - checkpoint.EntryCount}
}

func shouldDescendPastCheckpoint(relative string, checkpoint string, entry fs.DirEntry) bool {
	if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
		return false
	}
	if relative == checkpoint {
		return true
	}
	return strings.HasPrefix(checkpoint, relative+"/")
}

func walkSorted(ctx context.Context, root string, visit func(path string, entry fs.DirEntry) (bool, error)) error {
	var walkDir func(string) error

	walkDir = func(dir string) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			path := filepath.Join(dir, entry.Name())

			descend, err := visit(path, entry)
			if err != nil {
				return err
			}

			if entry.Type()&os.ModeSymlink != 0 {
				continue
			}

			if descend && entry.IsDir() {
				if err := walkDir(path); err != nil {
					return err
				}
			}
		}

		return nil
	}

	return walkDir(root)
}

func snapshotEntryType(entry fs.DirEntry) (string, error) {
	if entry.Type()&os.ModeSymlink != 0 {
		return "symlink", nil
	}

	info, err := entry.Info()
	if err != nil {
		return "", err
	}

	mode := info.Mode()
	switch {
	case mode.IsDir():
		return "directory", nil
	case mode.IsRegular():
		return "file", nil
	default:
		return "other", nil
	}
}

func checkpointFile(cacheBase string, record snapshotRecord) string {
	return fmt.Sprintf("%s.%s-%s.scan", cacheBase, record.Location, record.Snapshot)
}

func readCheckpoint(path string) (checkpointState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return checkpointState{}, nil
		}
		return checkpointState{}, err
	}

	line := strings.TrimSpace(string(data))
	if line == "" {
		return checkpointState{}, nil
	}

	fields := strings.SplitN(line, "\t", 2)
	state := checkpointState{RelativePath: fields[0]}
	if len(fields) == 2 {
		state.EntryCount, _ = strconv.ParseInt(fields[1], 10, 64)
	}

	return state, nil
}

func (committer *cacheCommitter) commitBatch(checkpointPath string, entries []catalogEntry, checkpoint checkpointState) error {
	committer.mu.Lock()
	defer committer.mu.Unlock()

	if len(entries) > 0 && committer.db != nil {
		if err := insertCatalogEntries(committer.db, entries); err != nil {
			return err
		}
		if err := committer.chmodDBFiles(); err != nil {
			return err
		}
	}

	return writeCheckpoint(checkpointPath, checkpoint)
}

func (committer *cacheCommitter) finishSnapshot(record snapshotRecord, checkpointPath string) error {
	committer.mu.Lock()
	defer committer.mu.Unlock()

	if committer.db != nil {
		if err := markSnapshotDone(committer.db, record); err != nil {
			return err
		}
		if err := committer.chmodDBFiles(); err != nil {
			return err
		}
	}
	if err := os.Remove(checkpointPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

func (committer *cacheCommitter) chmodDBFiles() error {
	if committer.dbFile == "" {
		return nil
	}
	return chmodBestEffort(committer.dbFile, committer.dbFile+"-wal", committer.dbFile+"-shm")
}

func writeCheckpoint(path string, checkpoint checkpointState) error {
	data := []byte(fmt.Sprintf("%s\t%d\n", checkpoint.RelativePath, checkpoint.EntryCount))
	return writeAtomic(path, data, privateCacheFileMode)
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".catalog-atomic-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return err
	}

	return os.Rename(tempName, path)
}

func snapshotKey(location, snapshot string) string {
	return location + "\t" + snapshot
}
