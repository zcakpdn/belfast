package misc

import (
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ggmolly/belfast/internal/connection"
	"github.com/ggmolly/belfast/internal/consts"
	"github.com/ggmolly/belfast/internal/logger"
	"github.com/ggmolly/belfast/internal/protobuf"
	"github.com/ggmolly/belfast/internal/region"
	"google.golang.org/protobuf/proto"
)

const (
	hashCacheFile = ".cached_hashes"
)

var (
	errRegionMismatch  = errors.New("Region mismatch")
	errVersionMismatch = errors.New("Version mismatch")
)

type VersionMap map[string]Version
type HashMap []GameChecksum

type Version struct {
	Region  string
	Version string
}

type GameChecksum struct {
	Category string
	Hash     string
}

type hashCache struct {
	Region  string
	Version string
	Hashes  HashMap
}

var (
	updateMu sync.RWMutex
	// azurLaneHashes are the resource hashes of the official server, re-emitted to clients.
	azurLaneHashes HashMap
	// azurLaneVersions are the versions the game data source (versions.json) was built from.
	azurLaneVersions VersionMap
	// liveVersions are the client versions the official gateways currently advertise.
	liveVersions = map[string]string{}
	// hashesFetchedAt is when azurLaneHashes was last fetched from the official server.
	hashesFetchedAt time.Time

	// gatewayAddr resolves the official gateway for a region; overridden in tests.
	gatewayAddr = func(region string) string {
		return fmt.Sprintf("%s:80", consts.RegionGateways[region])
	}
	gatewayTimeout = 10 * time.Second
)

func GetLatestVersions() VersionMap {
	updateMu.RLock()
	defer updateMu.RUnlock()
	return azurLaneVersions
}

// LiveClientVersion returns the client version the official gateway advertised
// on the last successful poll, or "" if it was never reached.
func LiveClientVersion(region string) string {
	updateMu.RLock()
	defer updateMu.RUnlock()
	return liveVersions[region]
}

// hashVersionKey is the version the hash cache is keyed on: the live client
// version when known, otherwise the data version.
func hashVersionKey(region string) string {
	if live := liveVersions[region]; live != "" {
		return live
	}
	return azurLaneVersions[region].Version
}

func hashFromCache() (HashMap, error) {
	// use gob to read the cache file
	file, err := os.Open(hashCacheFile)
	if err != nil { // no cache
		return nil, err
	}
	defer file.Close()
	decoder := gob.NewDecoder(file)
	var cache hashCache
	err = decoder.Decode(&cache)
	if err != nil {
		logger.LogEvent("GameUpdate", "GetHashes", err.Error(), logger.LOG_LEVEL_ERROR)
		return nil, err
	}
	region := region.Current()
	if cache.Region != region {
		return nil, errRegionMismatch
	}
	updateMu.RLock()
	expected := hashVersionKey(region)
	updateMu.RUnlock()
	if cache.Version != expected {
		return nil, errVersionMismatch
	}
	return cache.Hashes, nil
}

func GetGameHashes() HashMap {
	return getGameHashes(false)
}

func GetGameHashesWithUpdate() HashMap {
	return getGameHashes(true)
}

func getGameHashes(triggerUpdate bool) HashMap {
	updateMu.RLock()
	hashes := azurLaneHashes
	updateMu.RUnlock()
	if hashes != nil {
		return hashes
	}

	// check if we have a cached version
	if cached, err := hashFromCache(); err == nil {
		updateMu.Lock()
		if azurLaneHashes == nil {
			azurLaneHashes = cached
		}
		hashes = azurLaneHashes
		updateMu.Unlock()
		return hashes
	}

	// no cache, get the hashes from the server
	logger.LogEvent("GameUpdate", "GetHashes", "No cached hashes, fetching from server", logger.LOG_LEVEL_INFO)
	region := region.Current()
	if _, err := RefreshGameHashes(region); err != nil {
		logger.LogEvent("GameUpdate", "GetHashes", err.Error(), logger.LOG_LEVEL_ERROR)
		return nil
	}
	if triggerUpdate {
		go UpdateAllData(region)
	}
	updateMu.RLock()
	defer updateMu.RUnlock()
	return azurLaneHashes
}

// RefreshGameHashes asks the official gateway for the current resource hashes
// and replaces the in-memory and on-disk cache. It reports whether they changed.
func RefreshGameHashes(region string) (bool, error) {
	promptUpdate := protobuf.CS_10800{
		State:    proto.Uint32(59),  // 59 is something, might need to update this later?
		Platform: proto.String("1"), // iOS
	}
	if region == "CN" {
		// fix version invalid?
		promptUpdate.State = proto.Uint32(56)
	}
	var responseData protobuf.SC_10801
	if err := queryOfficialGateway(region, 10800, &promptUpdate, &responseData); err != nil {
		return false, err
	}
	var hashes HashMap
	for _, hash := range responseData.GetVersion() {
		if !strings.Contains(hash, "hash$") {
			continue
		}
		fields := strings.Split(hash, "$")
		hashes = append(hashes, GameChecksum{
			Category: fields[1],
			Hash:     hash,
		})
	}
	if len(hashes) == 0 {
		return false, fmt.Errorf("official gateway returned no resource hashes")
	}

	updateMu.Lock()
	changed := !sameHashes(azurLaneHashes, hashes)
	azurLaneHashes = hashes
	hashesFetchedAt = time.Now()
	cache := hashCache{
		Region:  region,
		Version: hashVersionKey(region),
		Hashes:  hashes,
	}
	updateMu.Unlock()

	if err := writeHashCache(cache); err != nil {
		logger.LogEvent("GameUpdate", "GetHashes", err.Error(), logger.LOG_LEVEL_ERROR)
	}
	return changed, nil
}

// RefreshLiveClientVersion asks the official gateway which client version it
// currently expects (CS_10996) and records it. It reports whether it changed.
func RefreshLiveClientVersion(region string) (string, bool, error) {
	request := protobuf.CS_10996{
		State:    proto.Uint32(1),
		Platform: proto.String("0"), // Android
	}
	var response protobuf.SC_10997
	if err := queryOfficialGateway(region, 10996, &request, &response); err != nil {
		return "", false, err
	}
	version := fmt.Sprintf("%d.%d.%d", response.GetVersion1(), response.GetVersion2(), response.GetVersion3())
	if response.GetVersion4() != 0 {
		version = fmt.Sprintf("%s.%d", version, response.GetVersion4())
	}
	if version == "0.0.0" {
		return "", false, fmt.Errorf("official gateway returned an empty version")
	}
	updateMu.Lock()
	changed := liveVersions[region] != version
	liveVersions[region] = version
	updateMu.Unlock()
	return version, changed, nil
}

func sameHashes(a HashMap, b HashMap) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func writeHashCache(cache hashCache) error {
	file, err := os.OpenFile(hashCacheFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	return gob.NewEncoder(file).Encode(cache)
}

// queryOfficialGateway sends one request packet to the region's official
// gateway and decodes the first response packet into response.
func queryOfficialGateway(region string, packetID int, request proto.Message, response proto.Message) error {
	if _, ok := consts.RegionGateways[region]; !ok {
		return fmt.Errorf("no official gateway known for region %q", region)
	}
	sock, err := net.DialTimeout("tcp", gatewayAddr(region), gatewayTimeout)
	if err != nil {
		return err
	}
	defer sock.Close()
	if err := sock.SetDeadline(time.Now().Add(gatewayTimeout)); err != nil {
		return err
	}
	packet, err := proto.Marshal(request)
	if err != nil {
		return err
	}
	connection.InjectPacketHeader(packetID, &packet, 0)
	if _, err := sock.Write(packet); err != nil {
		return err
	}
	payload, err := readGatewayPacket(sock)
	if err != nil {
		return fmt.Errorf("CS_%d: %w", packetID, err)
	}
	return proto.Unmarshal(payload, response)
}

// readGatewayPacket reads one framed packet: a 2-byte length (covering the
// rest of the header and the payload), a 0x00 byte, a 2-byte id and a 2-byte index.
func readGatewayPacket(r io.Reader) ([]byte, error) {
	var sizeBuf [2]byte
	if _, err := io.ReadFull(r, sizeBuf[:]); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(sizeBuf[:]))
	if size < 5 {
		return nil, fmt.Errorf("invalid packet size %d", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body[5:], nil
}

func LastCacheUpdate() time.Time {
	updateMu.RLock()
	fetched := hashesFetchedAt
	updateMu.RUnlock()
	if !fetched.IsZero() {
		return fetched
	}
	file, err := os.Stat(hashCacheFile)
	if err != nil {
		return time.Time{}
	}
	return file.ModTime()
}

func LastCacheUpdateVersion() string {
	updateMu.RLock()
	defer updateMu.RUnlock()
	if azurLaneHashes == nil {
		return ""
	}
	return hashVersionKey(region.Current())
}

// RefreshDataVersions downloads versions.json from the data source and returns
// the regions whose data version changed.
func RefreshDataVersions() ([]string, error) {
	resp, err := http.Get(dataFileURL("", "versions.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch versions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch versions: %s", resp.Status)
	}
	deserializedMap := make(map[string]string)
	if err := json.NewDecoder(resp.Body).Decode(&deserializedMap); err != nil {
		return nil, fmt.Errorf("failed to parse versions: %w", err)
	}
	updated := make(VersionMap, len(deserializedMap))
	var changed []string
	updateMu.Lock()
	for region, version := range deserializedMap {
		updated[region] = Version{
			Region:  region,
			Version: version,
		}
		if azurLaneVersions[region].Version != version {
			changed = append(changed, region)
		}
	}
	azurLaneVersions = updated
	updateMu.Unlock()
	return changed, nil
}

func init() {
	// Download the latest versions and parse them to the map
	if _, err := RefreshDataVersions(); err != nil {
		logger.LogEvent("GameUpdate", "init", err.Error(), logger.LOG_LEVEL_ERROR)
	}
}

// SetGameHashesForTest replaces the in-memory resource hashes.
func SetGameHashesForTest(hashes HashMap) {
	updateMu.Lock()
	defer updateMu.Unlock()
	azurLaneHashes = hashes
}
