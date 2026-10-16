package entrypoint

import (
	"context"
	"fmt"
	"time"

	"github.com/ggmolly/belfast/internal/config"
	"github.com/ggmolly/belfast/internal/logger"
	"github.com/ggmolly/belfast/internal/misc"
	"github.com/ggmolly/belfast/internal/region"
)

// configureGameDataSource points game data downloads at the configured source.
func configureGameDataSource(updates config.UpdatesConfig) {
	misc.SetDataBaseURL(updates.DataBaseURL)
	if misc.DataBaseURL() == misc.DefaultDataBaseURL {
		return // versions.json from the default source was loaded at startup
	}
	if _, err := misc.RefreshDataVersions(); err != nil {
		logger.LogEvent("GameUpdate", "DataSource", err.Error(), logger.LOG_LEVEL_WARN)
	}
}

func startUpdateWatcher(updates config.UpdatesConfig, importData func(string) error) {
	if updates.Enabled != nil && !*updates.Enabled {
		logger.LogEvent("GameUpdate", "Watcher", "disabled by config", logger.LOG_LEVEL_INFO)
		return
	}
	if updates.ReimportData != nil && !*updates.ReimportData {
		importData = nil
	}
	interval := time.Duration(*updates.IntervalMinutes) * time.Minute
	if interval == 0 {
		logger.LogEvent("GameUpdate", "Watcher", fmt.Sprintf("checking %s updates once at startup (data: %s)", region.Current(), misc.DataBaseURL()), logger.LOG_LEVEL_INFO)
	} else {
		logger.LogEvent("GameUpdate", "Watcher", fmt.Sprintf("following %s updates every %s (data: %s)", region.Current(), interval, misc.DataBaseURL()), logger.LOG_LEVEL_INFO)
	}
	misc.StartUpdateWatcher(context.Background(), misc.UpdateWatcherOptions{
		Region:   region.Current(),
		Interval: interval,
		Import:   importData,
	})
}
