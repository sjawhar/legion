package daemon

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// liveSettings is the settings holder of a daemon whose configuration is a real legion.yaml: the
// one thing config.Settled cannot stand in for, since a settings change is applied by reading
// that file again under the overlay. lines are appended to the file, each a key the body below
// does not carry — a file holding one key twice is a file no operator writes. `envoy_url` is
// among them: the body leaves it out and the loader settles its default
// (http://127.0.0.1:9020), so a test that wants the daemon's notices to reach a listener of its
// own names it here.
func liveSettings(t *testing.T, lines ...string) *config.Live {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "operator-token")
	if err := os.WriteFile(tokenFile, []byte(testOperatorToken+"\n"), 0o600); err != nil {
		t.Fatalf("write the operator token: %v", err)
	}
	port := holdPortOn(t, "127.0.0.1")
	body := fmt.Sprintf(`project: TEST%s
port: %d
bind: 127.0.0.1
postgres_dsn: %s
state_dir: %s
runtime: tmux
admission_cap: 4
daemon_url: http://127.0.0.1:%d
worker_boot_timeout_seconds: 120
worker_boot_registration_deadline_intervals: 3
worker_rpc_timeout_seconds: 5
worker_stop_timeout_seconds: 10
tree_stop_timeout_seconds: 60
slow_command_timeout_seconds: 300
probe_interval_seconds: 30
launch_failure_limit: 3
prompt_failure_limit: 3
prompt_retire_limit: 2
operator_token_file: %s
controller: operator
`, randomSuffix(t), port, testDSN(t), shortTempDir(t), port, tokenFile)
	path := filepath.Join(dir, "legion.yaml")
	if err := os.WriteFile(path, []byte(body+strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	live, err := config.NewLive(path, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return live
}

// operatorTokenFile is the file the holder's configuration names as the operator bearer's.
func operatorTokenFile(t *testing.T, live *config.Live) string {
	t.Helper()
	file := live.Snapshot().OperatorTokenFile
	if file == "" {
		t.Fatal("the configuration names no operator_token_file")
	}
	return file
}

// apply is one route change against the running daemon's holder, refusing nothing.
func apply(t *testing.T, live *config.Live, overlay map[string]any) []config.Change {
	t.Helper()
	applied, refused, err := live.Apply(config.SourceRoute, "someone", overlay)
	if err != nil {
		t.Fatalf("apply %v: %v", overlay, err)
	}
	if len(refused) != 0 {
		t.Fatalf("apply %v refused %+v, want every path applied", overlay, refused)
	}
	return applied
}

// A decision-class change takes effect at once, with nothing rebuilt: the daemon's next read of
// the admission cap is the new one, the change says so to the caller (at-once, no holder), and
// no holder was built again for it. What admission then does with the new cap is its own test
// (internal/admit's TestTheAdmissionCapChangedThroughTheSettingsHolderIsUsedByTheNextDecision):
// a deployment with no Dispatch, which is what this harness boots, admits nothing.
func TestAnAdmissionCapChangedThroughTheRouteIsReadByTheNextDecision(t *testing.T) {
	live := liveSettings(t)
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)

	if got := d.state().Admission.Cap; got != 4 {
		t.Fatalf("the admission cap before the change = %d, want the file's 4", got)
	}
	applied := apply(t, live, map[string]any{"admission_cap": 7})

	if len(applied) != 1 || applied[0].Path != "admission_cap" || applied[0].New != "7" {
		t.Fatalf("the apply reported %+v, want one admission_cap change to 7", applied)
	}
	if applied[0].Class != config.ClassDecision || applied[0].Outcome != config.OutcomeAtOnce || applied[0].Holder != "" {
		t.Fatalf("the change is %+v, want a decision-class change taking effect at once with no holder", applied[0])
	}
	if got := d.state().Admission.Cap; got != 7 {
		t.Fatalf("the admission cap the running daemon reads = %d, want the applied 7", got)
	}
	if strings.Contains(logs.String(), "holder rebuilt") {
		t.Fatalf("a decision-class change rebuilt a holder:\n%s", logs)
	}
}

// A rebuild-class change rebuilds exactly the holder its row names and nothing else, and the
// worker stream's own rebuild keeps every connection: a shim connected before the change is
// still connected after it, and its claim is still the one the daemon serves (acceptance A5).
func TestARebuildClassChangeRebuildsOnlyItsHolderAndKeepsTheWorkerConnected(t *testing.T) {
	live := liveSettings(t)
	rt := fake.NewRuntime()
	rec := &built{}
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(rt, rec), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	token := d.spawn(architect())
	readyClaim(t, d, rt, token)
	worker := dialShim(t, rec.address, lastLaunch(t, rt, token).BootToken)
	testwait.Eventually(t, "the shim's claim to be ready", func() bool {
		return d.claim(token).State == string(supervise.StateReady)
	})

	applied := apply(t, live, map[string]any{"worker_rpc_timeout_seconds": 9})

	if len(applied) != 1 || applied[0].Holder != config.HolderWorkerStream || applied[0].Outcome != config.OutcomeRebuilt {
		t.Fatalf("the apply reported %+v, want one worker-stream rebuild", applied)
	}
	awaitLog(t, logs, `"holder":"worker-stream"`, 5*time.Second, "the worker stream's settings were not rebuilt")
	for _, other := range []string{"nats", "dispatch-client", "envoy-publisher", "github-apps", "api-server", "controller-keeper", "runtime", "launch-specs"} {
		if strings.Contains(logs.String(), `"holder":"`+other+`"`) {
			t.Fatalf("the worker stream's change rebuilt %s too:\n%s", other, logs)
		}
	}
	// The connection is the one the shim dialled before the change: it is still registered, and
	// the daemon still answers on it.
	if got := d.claim(token).State; got != string(supervise.StateReady) {
		t.Fatalf("the worker's claim across the rebuild = %s, want it still ready on the connection it held", got)
	}
	stillOpen(t, worker)
}

// A rebuild that fails leaves the old holder serving: an operator_token_file the daemon cannot
// read is logged and changes nothing, and the bearer the daemon already holds still opens the
// operator routes.
func TestARebuildThatFailsLeavesTheOldHolderServing(t *testing.T) {
	live := liveSettings(t)
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	if status, _ := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true); status != http.StatusOK {
		t.Fatalf("the operator bearer before the change = %d, want 200", status)
	}

	missing := filepath.Join(t.TempDir(), "no-such-operator-token")
	apply(t, live, map[string]any{"operator_token_file": missing})

	awaitLog(t, logs, "rebuilding a holder failed", 5*time.Second, "the failed rebuild was not logged")
	if status, body := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true); status != http.StatusOK {
		t.Fatalf("the operator bearer after the failed rebuild = %d (%s), want the old holder still serving", status, body)
	}
}

// A credential file rotated in place names no changed setting — its path is what the
// configuration holds, and the path did not move — so the watcher asks the holders to read their
// files again (rebuilder.rereadCredentials). The API server then compares against the rotated
// bearer, and the one it held before is refused.
func TestARotatedCredentialFileWhosePathDidNotChangeIsReadAgain(t *testing.T) {
	live := liveSettings(t)
	var rebuild *rebuilder
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.settingsRebuilder = func(r *rebuilder) { rebuild = r }
	d := startDaemonOn(t, live, o, quietLogger(), 10*time.Second)
	if status, _ := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true); status != http.StatusOK {
		t.Fatalf("the operator bearer before the rotation = %d, want 200", status)
	}

	rotated := "rotated-" + testOperatorToken
	if err := os.WriteFile(operatorTokenFile(t, live), []byte(rotated+"\n"), 0o600); err != nil {
		t.Fatalf("rotate the operator token: %v", err)
	}
	if rebuild == nil {
		t.Fatal("the daemon handed the test no rebuild observer")
	}
	rebuild.rereadCredentials(live.Snapshot())

	if status, _ := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true); status != http.StatusForbidden {
		t.Fatalf("the bearer the daemon held before the rotation = %d, want 403 once the file was read again", status)
	}
	if status := bearerRequest(t, d, rotated); status != http.StatusOK {
		t.Fatalf("the rotated bearer = %d, want 200", status)
	}
}

// countingHolder is a holder that counts the rebuilds asked of it, under whichever name the test
// registers it as.
type countingHolder struct {
	name  string
	built int
}

func (h *countingHolder) holder() string { return h.name }

func (h *countingHolder) rebuild(*config.Config) error {
	h.built++
	return nil
}

// The file watcher wakes on the state of a whole directory, so a projected token the kubelet
// refreshed wakes it beside credentials that did not move. A reread rebuilds a credential holder
// only when the files that holder reads have moved: the wake that follows a kubelet refresh of
// something else leaves the NATS connection and its durable consumers where they are, and the
// wake that follows a rotation of the operator bearer rebuilds the API server alone.
func TestARereadRebuildsOnlyTheHoldersWhoseCredentialFilesMoved(t *testing.T) {
	dir := t.TempDir()
	operatorFile := filepath.Join(dir, "operator-token")
	seedFile := filepath.Join(dir, "nats-seed")
	for file, contents := range map[string]string{operatorFile: "operator\n", seedFile: "seed\n"} {
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
	cfg := &config.Config{OperatorTokenFile: operatorFile, NatsNkeySeedFile: seedFile}
	rebuild := newRebuilder(quietLogger())
	server := &countingHolder{name: config.HolderAPIServer}
	natsClient := &countingHolder{name: config.HolderNATS}
	rebuild.register(server, natsClient)

	// The first reread holds no digest of its own, so it reads every holder's files: the daemon
	// has no record of what they said when it booted on them.
	rebuild.rereadCredentials(cfg)
	if server.built != 1 || natsClient.built != 1 {
		t.Fatalf("the first reread rebuilt the API server %d times and NATS %d, want one each", server.built, natsClient.built)
	}

	// A wake whose watched files did not move rebuilds nothing.
	rebuild.rereadCredentials(cfg)
	if server.built != 1 || natsClient.built != 1 {
		t.Fatalf("a reread over unmoved files rebuilt the API server %d times and NATS %d, want the one each the first reread did", server.built, natsClient.built)
	}

	// A rotation of the operator bearer in place rebuilds the API server, and only it.
	if err := os.WriteFile(operatorFile, []byte("rotated-operator\n"), 0o600); err != nil {
		t.Fatalf("rotate the operator token: %v", err)
	}
	rebuild.rereadCredentials(cfg)
	if server.built != 2 {
		t.Fatalf("the API server was rebuilt %d times over a rotated operator_token_file, want 2", server.built)
	}
	if natsClient.built != 1 {
		t.Fatalf("NATS was rebuilt %d times over a rotation of someone else's file, want the one the first reread did", natsClient.built)
	}

	// And the wake after that rotation, with the file settled again, rebuilds nothing.
	rebuild.rereadCredentials(cfg)
	if server.built != 2 || natsClient.built != 1 {
		t.Fatalf("the reread after the rotation rebuilt the API server %d times and NATS %d, want 2 and 1", server.built, natsClient.built)
	}
}

// bearerRequest is one operator route called with the bearer the test names.
func bearerRequest(t *testing.T, d *daemon, token string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, d.base+"/legion/v1/operator/claims", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := d.client.Do(request)
	if err != nil {
		t.Fatalf("call the operator route: %v", err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

// A changed NATS seed rebuilds the NATS client: the holder opens a connection for the
// configuration the change settled, moves this project's durable consumers onto it, and closes
// the connection it replaced (acceptance A5).
//
// The connection is opened through the daemon's own openNATS, with one difference this pod
// forces: its NATS server takes no nkeys ("nkeys not supported by the server"), so the seed the
// changed key names is not carried onto the wire. The seed is what drives the rebuild, which is
// what this test is about; that the seed a key names is the one the connection authenticates as
// is TestTheWorkflowConnectsAsTheNatsSeedTheDaemonResolved's, against a server that takes one.
func TestAChangedNatsSeedRebuildsTheNatsClient(t *testing.T) {
	url := testnats.URL(t)
	probe, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect to the test NATS server: %v", err)
	}
	t.Cleanup(probe.Close)
	js, err := jetstream.New(probe)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	testnats.CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	logs := &syncBuffer{}
	w := grantWorkflow(t, logs)
	live := liveSettings(t,
		"nats_urls: ["+url+"]",
		"nats_nkey_seed_file: "+testnats.SeedFile(t, testnats.UserSeed(t)+"\n"))
	heard, err := w.heardAtBoot(t.Context())
	if err != nil {
		t.Fatalf("read the repositories this project hears about: %v", err)
	}
	var seeds []string
	open := func(c config.Config) (*nats.Conn, *intake.Consumers, error) {
		seeds = append(seeds, c.NatsNkeySeedFile)
		c.NatsNkeySeedFile = ""
		return openNATS(t.Context(), w, c, func(string) (string, bool) { return "", false }, intake.ConsumerSpec{
			Project: c.Project, Repositories: heard, AckWait: c.WorkerRPCTimeout, NakDelay: time.Second, Logger: w.log,
		})
	}
	conn, consumers, err := open(*live.Snapshot())
	if err != nil {
		t.Fatalf("open the workflow's NATS: %v", err)
	}
	rebuild := newRebuilder(w.log)
	live.OnChange(rebuild.observe)
	w.nats = newNATSHolder(conn, consumers, open)
	rebuild.register(w.nats)
	t.Cleanup(w.stop)
	before := w.nats.connection()

	rotated := testnats.SeedFile(t, testnats.UserSeed(t)+"\n")
	applied := apply(t, live, map[string]any{"nats_nkey_seed_file": rotated})

	if len(seeds) != 2 || seeds[1] != rotated {
		t.Fatalf("the holder opened its connections for %v, want the second for the seed the change named", seeds)
	}

	if len(applied) != 1 || applied[0].Holder != config.HolderNATS || applied[0].New != "(withheld)" {
		t.Fatalf("the apply reported %+v, want one nats rebuild whose value is withheld", applied)
	}
	after := w.nats.connection()
	if after == before {
		t.Fatal("the changed seed left the daemon on the connection it had, want the client rebuilt")
	}
	if !after.IsConnected() {
		t.Fatalf("the rebuilt NATS client is not connected: %v", after.LastError())
	}
	if !before.IsClosed() {
		t.Fatal("the connection the rebuild replaced was not closed")
	}
	if w.nats.intake() == nil {
		t.Fatal("the rebuilt client carries no durable consumers")
	}
}

// stillOpen fails unless the shim's connection is still open: a read of it times out rather than
// reaching the end a closed connection gives at once.
func stillOpen(t *testing.T, s *shim) {
	t.Helper()
	if err := s.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	switch _, err := s.lines.ReadByte(); {
	case err == nil:
		t.Fatal("the daemon wrote on the connection where the test expected it idle")
	case os.IsTimeout(err):
	default:
		t.Fatalf("the connection the rebuild kept is gone: %v", err)
	}
}
