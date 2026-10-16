package misc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ggmolly/belfast/internal/logger"
)

const (
	// DefaultDataBaseURL is the belfast-data checkout game data is imported from.
	DefaultDataBaseURL = "https://raw.githubusercontent.com/ggmolly/belfast-data/main"
)

var (
	dataBaseURL = DefaultDataBaseURL
	// Golang maps are unordered, so we keep track of key order ourselves.
	order = []string{"Items", "Buffs", "Ships", "Skins", "Resources", "Pools", "Requisition", "BuildTimes", "ShopOffers", "Weapons", "Equipments", "Skills", "Configs", "JuustagramTemplates", "JuustagramNpcTemplates", "JuustagramLanguage", "JuustagramShipGroups"}
)

func getBelfastData(region string, file string) (*json.Decoder, *http.Response, error) {
	url := dataFileURL(region, file)
	resp, err := http.Get(url)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("failed to fetch data from %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body), resp, nil
}

type githubContent struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// SetDataBaseURL changes where game data and versions.json are fetched from.
// An empty value restores the default belfast-data repository.
func SetDataBaseURL(base string) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = DefaultDataBaseURL
	}
	dataBaseURL = base
}

// DataBaseURL returns the base URL game data is fetched from.
func DataBaseURL() string {
	return dataBaseURL
}

func dataFileURL(region string, file string) string {
	if region == "" {
		return fmt.Sprintf("%s/%s", dataBaseURL, file)
	}
	return fmt.Sprintf("%s/%s/%s", dataBaseURL, region, file)
}

// dataListURL maps a raw.githubusercontent.com base URL to the GitHub contents
// API, which is used to enumerate config directories.
func dataListURL(region string, directory string) (string, error) {
	const rawPrefix = "https://raw.githubusercontent.com/"
	if !strings.HasPrefix(dataBaseURL, rawPrefix) {
		return "", fmt.Errorf("cannot list %s/%s: data base url %q is not a raw.githubusercontent.com url", region, directory, dataBaseURL)
	}
	parts := strings.SplitN(strings.TrimPrefix(dataBaseURL, rawPrefix), "/", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("cannot list %s/%s: data base url %q must look like %sowner/repo/branch", region, directory, dataBaseURL, rawPrefix)
	}
	return fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s/%s?ref=%s", parts[0], parts[1], region, directory, parts[2]), nil
}

func listBelfastDataFiles(region string, directory string) ([]string, error) {
	url, err := dataListURL(region, directory)
	if err != nil {
		return nil, err
	}
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list data from %s: %s", url, resp.Status)
	}
	var contents []githubContent
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&contents); err != nil {
		return nil, err
	}
	files := make([]string, 0, len(contents))
	for _, entry := range contents {
		if entry.Type != "file" || !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		files = append(files, fmt.Sprintf("%s/%s", directory, entry.Name))
	}
	return files, nil
}

func filterConfigFiles(files []string, prefixes []string, include []string) []string {
	allowed := make(map[string]struct{}, len(files))
	for _, file := range files {
		name := strings.TrimPrefix(file, "ShareCfg/")
		name = strings.TrimPrefix(name, "GameCfg/")
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				allowed[file] = struct{}{}
				break
			}
		}
	}
	for _, file := range include {
		allowed[file] = struct{}{}
	}
	filtered := make([]string, 0, len(allowed))
	for file := range allowed {
		filtered = append(filtered, file)
	}
	return filtered
}

func configEntryKey(raw json.RawMessage, index int) string {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return strconv.Itoa(index)
	}
	for _, field := range []string{"id", "ID", "Id"} {
		value, ok := object[field]
		if !ok {
			continue
		}
		if key, ok := parseConfigKey(value); ok {
			return key
		}
	}
	return strconv.Itoa(index)
}

func parseConfigKey(value json.RawMessage) (string, bool) {
	var number uint64
	if err := json.Unmarshal(value, &number); err == nil {
		return strconv.FormatUint(number, 10), true
	}
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		return text, text != ""
	}
	var floatValue float64
	if err := json.Unmarshal(value, &floatValue); err == nil {
		return strconv.FormatUint(uint64(floatValue), 10), true
	}
	return "", false
}

func UpdateAllData(region string) {
	if err := ImportGameData(region); err != nil {
		logger.LogEvent("GameData", "Updating", fmt.Sprintf("failed to update game data: %s", err.Error()), logger.LOG_LEVEL_ERROR)
	}
}

// ImportGameData imports every game data table for region in one transaction.
func ImportGameData(region string) error {
	logger.LogEvent("GameData", "Updating", "Updating all game data.. this may take a while.", logger.LOG_LEVEL_INFO)
	version := GetLatestVersions()[region].Version
	if err := updateAllDataSQLC(region); err != nil {
		return err
	}
	if err := recordImportedDataVersion(region, version); err != nil {
		logger.LogEvent("GameData", "Updating", fmt.Sprintf("failed to record imported data version: %s", err.Error()), logger.LOG_LEVEL_WARN)
	}
	logger.LogEvent("GameData", "Updating", fmt.Sprintf("Game data imported (region=%s, version=%s)", region, displayVersion(version)), logger.LOG_LEVEL_INFO)
	return nil
}
