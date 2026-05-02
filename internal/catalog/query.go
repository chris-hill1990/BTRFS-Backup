package catalog

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func livePathFor(source string, relativePath string) string {
	source = filepath.Clean(source)

	if relativePath == "" {
		return source
	}
	if source == "/" {
		return "/" + relativePath
	}
	return source + "/" + relativePath
}

func CleanRelativePath(relativePath string) string {
	relativePath = strings.TrimSpace(relativePath)
	for strings.HasPrefix(relativePath, "./") {
		relativePath = strings.TrimPrefix(relativePath, "./")
	}
	relativePath = strings.Trim(relativePath, "/")
	if relativePath == "." {
		return ""
	}
	return relativePath
}

func RunQuery(ctx context.Context, config appConfig, sourceFilter string, volumeNames []string, relativePath string, output io.Writer) error {
	volumes, err := filterVolumes(config.Volumes, volumeNames)
	if err != nil {
		return err
	}
	if len(volumes) != 1 {
		return fmt.Errorf("--query requires exactly one --volume")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	return queryCatalogDB(config, volumes[0], sourceFilter, relativePath, output)
}

func RunVersionQuery(ctx context.Context, config appConfig, sourceFilter string, volumeNames []string, relativePath string, output io.Writer) error {
	volumes, err := filterVolumes(config.Volumes, volumeNames)
	if err != nil {
		return err
	}
	if len(volumes) != 1 {
		return fmt.Errorf("--versions requires exactly one --volume")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	return queryCatalogVersionsDB(config, volumes[0], sourceFilter, relativePath, output)
}

func queryCatalogDB(config appConfig, volume volumeConfig, sourceFilter string, relativePath string, output io.Writer) error {
	dbFile, err := catalogDBFile(config, volume, sourceFilter)
	if err != nil {
		return err
	}

	db, err := openCatalogDB(dbFile)
	if err != nil {
		return err
	}
	defer db.Close()

	query := `
		WITH grouped AS (
			SELECT
				p.relative_path,
				MIN(p.name) AS name,
				CASE WHEN COUNT(DISTINCT e.type) = 1 THEN MIN(e.type) ELSE 'mixed' END AS entry_type,
				COUNT(DISTINCT s.snapshot) AS versions_count,
				MIN(s.snapshot) AS first_snapshot,
				MAX(s.snapshot) AS latest_snapshot
			FROM paths p
			JOIN entries e ON e.path_id = p.id
			JOIN snapshots s ON s.id = e.snapshot_id AND s.done = 1
			WHERE p.parent_path = ?
			GROUP BY p.id
		)
		SELECT relative_path, name, entry_type, versions_count, first_snapshot, latest_snapshot
		FROM grouped
		ORDER BY CASE WHEN entry_type = 'directory' THEN 0 ELSE 1 END, lower(name)
	`
	args := []any{relativePath}
	if sourceFilter != "any" {
		query = `
			WITH grouped AS (
				SELECT
					p.relative_path,
					MIN(p.name) AS name,
					CASE WHEN COUNT(DISTINCT e.type) = 1 THEN MIN(e.type) ELSE 'mixed' END AS entry_type,
					COUNT(DISTINCT s.snapshot) AS versions_count,
					MIN(s.snapshot) AS first_snapshot,
					MAX(s.snapshot) AS latest_snapshot
				FROM paths p
				JOIN entries e ON e.path_id = p.id
				JOIN snapshots s ON s.id = e.snapshot_id AND s.done = 1
				WHERE p.parent_path = ? AND s.location = ?
				GROUP BY p.id
			)
			SELECT relative_path, name, entry_type, versions_count, first_snapshot, latest_snapshot
			FROM grouped
			ORDER BY CASE WHEN entry_type = 'directory' THEN 0 ELSE 1 END, lower(name)
		`
		args = append(args, sourceFilter)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	writer := bufio.NewWriter(output)
	defer writer.Flush()

	for rows.Next() {
		var entryRelative string
		var name string
		var entryType string
		var versionsCount int
		var firstSnapshot string
		var latestSnapshot string

		if err := rows.Scan(&entryRelative, &name, &entryType, &versionsCount, &firstSnapshot, &latestSnapshot); err != nil {
			return err
		}

		livePath := livePathFor(volume.Source, entryRelative)
		presentLive := 0
		deleted := 1
		if _, err := os.Lstat(livePath); err == nil {
			presentLive = 1
			deleted = 0
		}

		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%d\t%d\n",
			name,
			entryType,
			entryRelative,
			livePath,
			versionsCount,
			versionsCount,
			firstSnapshot,
			latestSnapshot,
			presentLive,
			deleted); err != nil {
			return err
		}
	}

	return rows.Err()
}

func queryCatalogVersionsDB(config appConfig, volume volumeConfig, sourceFilter string, relativePath string, output io.Writer) error {
	dbFile, err := catalogDBFile(config, volume, sourceFilter)
	if err != nil {
		return err
	}

	db, err := openCatalogDB(dbFile)
	if err != nil {
		return err
	}
	defer db.Close()

	query := `
		SELECT s.location, s.snapshot
		FROM paths p
		JOIN entries e ON e.path_id = p.id
		JOIN snapshots s ON s.id = e.snapshot_id AND s.done = 1
		WHERE p.relative_path = ?
		GROUP BY s.location, s.snapshot
		ORDER BY s.snapshot, s.location
	`
	args := []any{relativePath}
	if sourceFilter != "any" {
		query = `
			SELECT s.location, s.snapshot
			FROM paths p
			JOIN entries e ON e.path_id = p.id
			JOIN snapshots s ON s.id = e.snapshot_id AND s.done = 1
			WHERE p.relative_path = ? AND s.location = ?
			GROUP BY s.location, s.snapshot
			ORDER BY s.snapshot, s.location
		`
		args = append(args, sourceFilter)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	writer := bufio.NewWriter(output)
	defer writer.Flush()

	requestedPath := livePathFor(volume.Source, relativePath)
	for rows.Next() {
		var location string
		var snapshot string

		if err := rows.Scan(&location, &snapshot); err != nil {
			return err
		}

		candidate := snapshotContentPath(filepath.Join(snapshotLocationPath(config.BackupMount, location, volume), snapshot), relativePath)
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", volume.Name, location, snapshot, candidate, requestedPath); err != nil {
			return err
		}
	}

	return rows.Err()
}

func catalogDBFile(config appConfig, volume volumeConfig, sourceFilter string) (string, error) {
	dbFile := filepath.Join(config.CacheDir, fmt.Sprintf("catalog-%s-%s.db", volume.Name, sourceFilter))
	if err := readableFile(dbFile); err == nil {
		return dbFile, nil
	} else if sourceFilter == "any" {
		return "", err
	}

	anyDBFile := filepath.Join(config.CacheDir, fmt.Sprintf("catalog-%s-any.db", volume.Name))
	if err := readableFile(anyDBFile); err != nil {
		return "", err
	}
	return anyDBFile, nil
}

func readableFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return file.Close()
}

func snapshotContentPath(snapshotRoot string, relativePath string) string {
	if relativePath == "" {
		return snapshotRoot
	}
	return filepath.Join(snapshotRoot, filepath.FromSlash(relativePath))
}
