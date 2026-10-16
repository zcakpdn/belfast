package misc

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ggmolly/belfast/internal/connection"
	"github.com/ggmolly/belfast/internal/protobuf"
	"google.golang.org/protobuf/proto"
)

type fakeGateway struct {
	mu      sync.Mutex
	version [3]uint32
	hashes  []string
}

func (g *fakeGateway) set(version [3]uint32, hashes ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.version = version
	g.hashes = hashes
}

// startFakeGateway serves CS_10800 and CS_10996 like the official gateway.
func startFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	gateway := &fakeGateway{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go gateway.serve(conn)
		}
	}()
	previous := gatewayAddr
	gatewayAddr = func(string) string { return listener.Addr().String() }
	t.Cleanup(func() { gatewayAddr = previous })
	return gateway
}

func (g *fakeGateway) serve(conn net.Conn) {
	defer conn.Close()
	var sizeBuf [2]byte
	if _, err := io.ReadFull(conn, sizeBuf[:]); err != nil {
		return
	}
	body := make([]byte, binary.BigEndian.Uint16(sizeBuf[:]))
	if _, err := io.ReadFull(conn, body); err != nil {
		return
	}
	packetID := int(binary.BigEndian.Uint16(body[1:3]))
	g.mu.Lock()
	var response proto.Message
	var responseID int
	switch packetID {
	case 10800:
		responseID = 10801
		response = &protobuf.SC_10801{
			GatewayIp:               proto.String("gateway"),
			GatewayPort:             proto.Uint32(80),
			Url:                     proto.String(""),
			Version:                 append([]string{"dTag-1"}, g.hashes...),
			ProxyIp:                 proto.String("proxy"),
			ProxyPort:               proto.Uint32(20000),
			IsTs:                    proto.Uint32(0),
			Timestamp:               proto.Uint32(1),
			Monday_0OclockTimestamp: proto.Uint32(1),
		}
	case 10996:
		responseID = 10997
		response = &protobuf.SC_10997{
			Version1:    proto.Uint32(g.version[0]),
			Version2:    proto.Uint32(g.version[1]),
			Version3:    proto.Uint32(g.version[2]),
			Version4:    proto.Uint32(0),
			GatewayIp:   proto.String("gateway"),
			GatewayPort: proto.Uint32(80),
			Url:         proto.String(""),
		}
	}
	g.mu.Unlock()
	if response == nil {
		return
	}
	data, err := proto.Marshal(response)
	if err != nil {
		return
	}
	connection.InjectPacketHeader(responseID, &data, 0)
	// Split the write to make sure readers do not rely on a single read.
	conn.Write(data[:3])
	time.Sleep(10 * time.Millisecond)
	conn.Write(data[3:])
}

func resetUpdateState(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	updateMu.Lock()
	savedHashes, savedVersions, savedLive := azurLaneHashes, azurLaneVersions, liveVersions
	azurLaneHashes, azurLaneVersions, liveVersions = nil, VersionMap{}, map[string]string{}
	updateMu.Unlock()
	savedBase := dataBaseURL
	t.Cleanup(func() {
		updateMu.Lock()
		azurLaneHashes, azurLaneVersions, liveVersions = savedHashes, savedVersions, savedLive
		updateMu.Unlock()
		dataBaseURL = savedBase
	})
}

// serveVersions serves versions.json as the data source and returns a setter for it.
func serveVersions(t *testing.T, initial string) func(string) {
	t.Helper()
	var mu sync.Mutex
	versions := initial
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/versions.json" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		io.WriteString(w, versions)
	}))
	t.Cleanup(server.Close)
	SetDataBaseURL(server.URL + "/")
	return func(value string) {
		mu.Lock()
		defer mu.Unlock()
		versions = value
	}
}

func TestRefreshFromOfficialGateway(t *testing.T) {
	resetUpdateState(t)
	gateway := startFakeGateway(t)
	gateway.set([3]uint32{9, 4, 120}, "$azhash$1$aaa", "$cvhash$1$bbb")

	version, changed, err := RefreshLiveClientVersion("EN")
	if err != nil || !changed || version != "9.4.120" {
		t.Fatalf("live version = %q changed=%v err=%v", version, changed, err)
	}
	resolved, err := ResolveRegionVersion("EN")
	if err != nil || resolved != "9.4.120" {
		t.Fatalf("ResolveRegionVersion = %q, %v; want live version", resolved, err)
	}

	hashesChanged, err := RefreshGameHashes("EN")
	if err != nil || !hashesChanged {
		t.Fatalf("first refresh changed=%v err=%v", hashesChanged, err)
	}
	if got := GetGameHashes(); len(got) != 2 || got[0].Category != "azhash" {
		t.Fatalf("unexpected hashes %+v", got)
	}
	if hashesChanged, _ = RefreshGameHashes("EN"); hashesChanged {
		t.Fatalf("unchanged hashes reported as changed")
	}

	gateway.set([3]uint32{9, 4, 121}, "$azhash$2$ccc", "$cvhash$1$bbb")
	if _, changed, _ = RefreshLiveClientVersion("EN"); !changed {
		t.Fatalf("version bump not detected")
	}
	if hashesChanged, _ = RefreshGameHashes("EN"); !hashesChanged {
		t.Fatalf("hash change not detected")
	}
	if got := GetGameHashes(); got[0].Hash != "$azhash$2$ccc" {
		t.Fatalf("hashes not replaced in memory: %+v", got)
	}

	// The on-disk cache is keyed on the live version and is reused after a restart.
	updateMu.Lock()
	azurLaneHashes = nil
	updateMu.Unlock()
	cached, err := hashFromCache()
	if err != nil || cached[0].Hash != "$azhash$2$ccc" {
		t.Fatalf("hash cache = %+v, %v", cached, err)
	}
}

func TestPollReimportsWhenDataVersionChanges(t *testing.T) {
	resetUpdateState(t)
	gateway := startFakeGateway(t)
	gateway.set([3]uint32{9, 2, 557}, "$azhash$1$aaa")
	setVersions := serveVersions(t, `{"EN":"9.2.557"}`)

	var imports []string
	opts := UpdateWatcherOptions{Region: "EN", Import: func(region string) error {
		imports = append(imports, GetLatestVersions()[region].Version)
		return recordImportedDataVersion(region, GetLatestVersions()[region].Version)
	}}

	pollGameUpdates(opts)
	pollGameUpdates(opts)
	if len(imports) != 1 || imports[0] != "9.2.557" {
		t.Fatalf("expected one import of 9.2.557, got %v", imports)
	}

	setVersions(`{"EN":"9.4.120"}`)
	pollGameUpdates(opts)
	if len(imports) != 2 || imports[1] != "9.4.120" {
		t.Fatalf("expected re-import of 9.4.120, got %v", imports)
	}
	if got := ImportedDataVersion("EN"); got != "9.4.120" {
		t.Fatalf("imported version = %q", got)
	}
}

func TestPollWithoutImporterOnlyRefreshes(t *testing.T) {
	resetUpdateState(t)
	gateway := startFakeGateway(t)
	gateway.set([3]uint32{9, 4, 0}, "$azhash$1$aaa")
	serveVersions(t, `{"EN":"9.2.557"}`)

	pollGameUpdates(UpdateWatcherOptions{Region: "EN"})
	if LiveClientVersion("EN") != "9.4.0" {
		t.Fatalf("live version not refreshed")
	}
	if GetLatestVersions()["EN"].Version != "9.2.557" {
		t.Fatalf("data version not refreshed")
	}
	if ImportedDataVersion("EN") != "" {
		t.Fatalf("gateway poll must not record an import")
	}
}

func TestReadGatewayPacketRejectsShortHeader(t *testing.T) {
	if _, err := readGatewayPacket(bytes.NewReader([]byte{0x00, 0x02, 0x00, 0x00})); err == nil {
		t.Fatalf("expected error for size < 5")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"9.2.557", "9.2.557", 0},
		{"9.4.1", "9.2.557", 1},
		{"9.2.557", "9.10.0", -1},
		{"9.2", "9.2.0", 0},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestDataListURL(t *testing.T) {
	t.Cleanup(func() { SetDataBaseURL("") })
	SetDataBaseURL("https://raw.githubusercontent.com/someone/belfast-data/dev/")
	url, err := dataListURL("EN", "ShareCfg")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://api.github.com/repos/someone/belfast-data/contents/EN/ShareCfg?ref=dev" {
		t.Fatalf("unexpected url %q", url)
	}
	if got := dataFileURL("EN", "ShareCfg/a.json"); got != "https://raw.githubusercontent.com/someone/belfast-data/dev/EN/ShareCfg/a.json" {
		t.Fatalf("unexpected file url %q", got)
	}
	SetDataBaseURL("http://localhost:8000/data")
	if _, err := dataListURL("EN", "ShareCfg"); err == nil {
		t.Fatalf("expected error for non-github base url")
	}
}
