package catalog

import (
	"btrfs-backup/internal/config"
	"database/sql"
	"io/fs"
	"sync"
)

const (
	privateCacheDirMode  fs.FileMode = 0700
	privateCacheFileMode fs.FileMode = 0600
)

type volumeConfig = config.Volume
type appConfig = config.AppConfig

func filterVolumes(configured []volumeConfig, requested []string) ([]volumeConfig, error) {
	return config.FilterVolumes(configured, requested)
}

type snapshotRecord struct {
	Index    int
	Total    int
	Location string
	Snapshot string
	Root     string
}

type indexResult struct {
	Record      snapshotRecord
	EntryCount  int64
	Unavailable bool
	Err         error
}

type checkpointState struct {
	RelativePath string
	EntryCount   int64
}

type catalogEntry struct {
	Location     string
	Snapshot     string
	Name         string
	EntryType    string
	RelativePath string
	ParentPath   string
}

type cacheCommitter struct {
	db     *sql.DB
	dbFile string
	mu     sync.Mutex
}
