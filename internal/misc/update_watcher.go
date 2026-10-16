package misc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ggmolly/belfast/internal/logger"
)

const importedDataVersionFile = ".imported_data_version"

var importedDataMu sync.Mutex

// UpdateWatcherOptions configures StartUpdateWatcher.
type UpdateWatcherOptions struct {
	Region   string
	Interval time.Duration
	// Import re-imports game data for a region. Nil disables re-imports, which
	// is what the gateway wants since it has no game database.
	Import func(region string) error
}

// StartUpdateWatcher follows the official game's updates until ctx is done.
// Every interval it:
//   - re-reads versions.json from the data source,
//   - asks the official gateway for the client version it expects and for its
//     resource hashes, so clients updated through the store keep matching,
//   - re-imports game data when the data source moved past what was imported.
func StartUpdateWatcher(ctx context.Context, opts UpdateWatcherOptions) {
	if opts.Interval <= 0 {
		opts.Interval = time.Hour
	}
	go func() {
		pollGameUpdates(opts)
		ticker := time.NewTicker(opts.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pollGameUpdates(opts)
			}
		}
	}()
}

func pollGameUpdates(opts UpdateWatcherOptions) {
	region := opts.Region

	if _, err := RefreshDataVersions(); err != nil {
		logger.LogEvent("GameUpdate", "Watcher", err.Error(), logger.LOG_LEVEL_WARN)
	}

	live, liveChanged, err := RefreshLiveClientVersion(region)
	if err != nil {
		logger.LogEvent("GameUpdate", "Watcher", fmt.Sprintf("could not read client version from official gateway: %s", err.Error()), logger.LOG_LEVEL_WARN)
	} else if liveChanged {
		logger.LogEvent("GameUpdate", "Watcher", fmt.Sprintf("official %s client version is %s", region, live), logger.LOG_LEVEL_INFO)
	}

	hashesChanged, err := RefreshGameHashes(region)
	if err != nil {
		logger.LogEvent("GameUpdate", "Watcher", fmt.Sprintf("could not refresh resource hashes from official gateway: %s", err.Error()), logger.LOG_LEVEL_WARN)
	} else if hashesChanged {
		logger.LogEvent("GameUpdate", "Watcher", "resource hashes changed, clients will receive the new ones", logger.LOG_LEVEL_INFO)
	}

	dataVersion := GetLatestVersions()[region].Version
	if opts.Import != nil && dataVersion != "" {
		imported := ImportedDataVersion(region)
		if imported != dataVersion {
			logger.LogEvent("GameUpdate", "Watcher", fmt.Sprintf("game data %s -> %s, re-importing", displayVersion(imported), dataVersion), logger.LOG_LEVEL_INFO)
			if err := opts.Import(region); err != nil {
				logger.LogEvent("GameUpdate", "Watcher", fmt.Sprintf("game data import failed: %s", err.Error()), logger.LOG_LEVEL_ERROR)
			}
		}
	}

	if live != "" && dataVersion != "" && CompareVersions(live, dataVersion) > 0 {
		logger.LogEvent("GameUpdate", "Watcher", fmt.Sprintf("client version %s is newer than game data %s; content added in the update stays missing until the data source catches up", live, dataVersion), logger.LOG_LEVEL_WARN)
	}
}

func displayVersion(version string) string {
	if version == "" {
		return "(none)"
	}
	return version
}

// ImportedDataVersion returns the data version last imported for region.
func ImportedDataVersion(region string) string {
	importedDataMu.Lock()
	defer importedDataMu.Unlock()
	return readImportedDataVersions()[region]
}

func recordImportedDataVersion(region string, version string) error {
	if version == "" {
		return nil
	}
	importedDataMu.Lock()
	defer importedDataMu.Unlock()
	versions := readImportedDataVersions()
	versions[region] = version
	data, err := json.Marshal(versions)
	if err != nil {
		return err
	}
	return os.WriteFile(importedDataVersionFile, data, 0644)
}

func readImportedDataVersions() map[string]string {
	versions := map[string]string{}
	data, err := os.ReadFile(importedDataVersionFile)
	if err != nil {
		return versions
	}
	if err := json.Unmarshal(data, &versions); err != nil {
		return map[string]string{}
	}
	return versions
}

// CompareVersions compares dotted numeric versions such as 9.2.557.
// Missing or non-numeric segments count as 0.
func CompareVersions(a string, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
