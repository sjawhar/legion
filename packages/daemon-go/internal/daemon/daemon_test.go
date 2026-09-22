package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// testDSN is the devbox and CI Postgres these tests run against. The daemon has no in-memory
// mode: what it does on a boot is what its store recorded.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to test against. Start one and set it:\n" +
			"  docker run -d --name legion-pg -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion -e POSTGRES_DB=legion -p 127.0.0.1::5432 postgres:16\n" +
			"  port=$(docker port legion-pg 5432/tcp | head -1 | sed 's/.*://')\n" +
			"  LEGION_TEST_PG_DSN=postgres://legion:legion@127.0.0.1:$port/legion go test ./internal/daemon/")
	}
	return dsn
}

// testConfig is a daemon of its own: its project name is unique, so the boots it records are its
// own, and its port is one nothing else holds.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Project:      "TEST" + randomSuffix(t),
		Port:         freePort(t),
		Bind:         "127.0.0.1",
		PostgresDSN:  testDSN(t),
		StateDir:     t.TempDir(),
		Runtime:      config.Runtime{Name: "tmux"},
		AdmissionCap: 4,
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func boots(t *testing.T, cfg config.Config) (int, time.Time) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	count, firstBootAt, err := st.Boots(ctx, cfg.Project)
	if err != nil {
		t.Fatalf("read the boots: %v", err)
	}
	return count, firstBootAt
}

// A daemon told to stop still owes its store a recorded, stamped boot: the restart count and the
// first boot time are what `legion state` answers from.
func TestRunRecordsEveryBootAndKeepsTheFirstBootTime(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Run(ctx, cfg, quietLogger()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	count, firstBootAt := boots(t, cfg)
	if count != 1 {
		t.Fatalf("boots after one run = %d, want 1", count)
	}
	if firstBootAt.IsZero() {
		t.Fatal("the first boot has no recorded time")
	}

	if err := Run(ctx, cfg, quietLogger()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	countAgain, firstBootAgain := boots(t, cfg)
	if countAgain != count+1 {
		t.Fatalf("boots after a restart = %d, want %d", countAgain, count+1)
	}
	if !firstBootAgain.Equal(firstBootAt) {
		t.Fatalf("the first boot time moved on a restart: %s, was %s", firstBootAgain, firstBootAt)
	}
}

// The refusal an operator reads when Postgres is not there names where the daemon went, and
// never how it would have got in.
func TestRunRefusesAnUnreachablePostgresByHostAndNotByPassword(t *testing.T) {
	cfg := config.Config{
		Project:      "TEST" + randomSuffix(t),
		Port:         freePort(t),
		Bind:         "127.0.0.1",
		PostgresDSN:  "postgres://legion:hunter2@127.0.0.1:1/legion",
		StateDir:     t.TempDir(),
		Runtime:      config.Runtime{Name: "tmux"},
		AdmissionCap: 4,
	}

	refused := make(chan error, 1)
	go func() { refused <- Run(context.Background(), cfg, quietLogger()) }()

	select {
	case err := <-refused:
		if err == nil {
			t.Fatal("Run started without a Postgres to start against")
		}
		if !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Errorf("the refusal does not name the host it could not reach: %v", err)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the refusal leaks the DSN password: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not refuse an unreachable Postgres within one second")
	}
}

// A recorded boot is a boot that served. A daemon whose port is already taken never ran, and a
// row for it would inflate `daemon.boots` — the value `legion state` answers with and the stage
// gate asserts on.
func TestRunRecordsNoBootWhenItCannotTakeItsPort(t *testing.T) {
	cfg := testConfig(t)
	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	occupied, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("occupy %s: %v", address, err)
	}
	defer occupied.Close()

	err = Run(context.Background(), cfg, quietLogger())
	if err == nil {
		t.Fatal("Run returned no error although another listener held its port")
	}
	if !strings.Contains(err.Error(), address) {
		t.Errorf("the refusal does not name the address it could not take: %v", err)
	}
	if count, _ := boots(t, cfg); count != 0 {
		t.Fatalf("boots after a start that never served = %d, want 0", count)
	}
}

// The API the daemon serves answers from the store it just migrated: this is the whole of
// `legion state` and of the plugin's read.
func TestRunServesTheStateOfItsOwnBoot(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan error, 1)
	go func() { stopped <- Run(ctx, cfg, quietLogger()) }()

	// One transport for every request this test makes, so it can close its own connections
	// before it asks the daemon to stop: net/http gives a connection that has sent no request
	// five seconds before a Shutdown may close it, and a connection this client dialled is this
	// client's to clean up.
	transport := &http.Transport{}
	client := &http.Client{Transport: transport}

	base := "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
	waitForHealthz(t, client, base)

	response, err := client.Get(base + "/legion/v1/state")
	if err != nil {
		t.Fatalf("GET the state: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET the state = %d, want 200", response.StatusCode)
	}
	var state api.State
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatalf("decode the state: %v", err)
	}

	if state.Daemon.Project != cfg.Project {
		t.Errorf("project = %q, want %q", state.Daemon.Project, cfg.Project)
	}
	if state.Daemon.SchemaVersion < 1 {
		t.Errorf("schema version = %d, want the migrated schema", state.Daemon.SchemaVersion)
	}
	if state.Daemon.Boots != 1 {
		t.Errorf("boots = %d, want this daemon's own first boot", state.Daemon.Boots)
	}
	if state.Daemon.StartedAt.IsZero() {
		t.Error("the running daemon reports no start time")
	}
	if state.Admission.Cap != cfg.AdmissionCap {
		t.Errorf("admission cap = %d, want the configured %d", state.Admission.Cap, cfg.AdmissionCap)
	}
	if len(state.Issues) != 0 {
		t.Errorf("issues = %v, want none before Stage 2 admits any", state.Issues)
	}

	transport.CloseIdleConnections()
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	if _, err := client.Get(base + "/healthz"); err == nil {
		t.Error("the API still answers after Run returned")
	}
}

func waitForHealthz(t *testing.T, client *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := client.Get(base + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never answered /healthz on %s: %v", base, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
