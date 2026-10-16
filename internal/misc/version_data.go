package misc

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

var (
	localVersionsOnce sync.Once
	localVersions     map[string]string
	localVersionsErr  error
)

// ResolveRegionVersion returns the client version reported to clients: the
// version the official gateway advertises when it was reachable, otherwise the
// version of the game data source.
func ResolveRegionVersion(region string) (string, error) {
	if live := LiveClientVersion(region); live != "" {
		return live, nil
	}
	versions := GetLatestVersions()
	if versions != nil {
		if entry, ok := versions[region]; ok {
			return entry.Version, nil
		}
	}

	local, err := loadLocalVersions()
	if err != nil {
		return "", err
	}

	version, ok := local[region]
	if !ok {
		return "", fmt.Errorf("missing version for region %q", region)
	}

	return version, nil
}

func loadLocalVersions() (map[string]string, error) {
	localVersionsOnce.Do(func() {
		file, err := os.Open("belfast-data/versions.json")
		if err != nil {
			localVersionsErr = err
			return
		}
		defer file.Close()
		decoder := json.NewDecoder(file)
		if err := decoder.Decode(&localVersions); err != nil {
			localVersionsErr = err
		}
	})

	return localVersions, localVersionsErr
}
