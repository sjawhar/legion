package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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
// registers it as, and answers whatever rebuild failure the test gave it.
type countingHolder struct {
	name  string
	built int
	// fails is what its rebuild answers: a holder the daemon could not build again — a NATS the
	// new seed cannot reach — answers one, and the rebuild succeeded when it is nil.
	fails error
}

func (h *countingHolder) holder() string { return h.name }

func (h *countingHolder) rebuild(*config.Config) error {
	h.built++
	return h.fails
}

// countingCredentialHolder is a countingHolder built from credentials of its own, which it says
// as every credential holder does (credentialHolder).
type countingCredentialHolder struct {
	countingHolder
	reads func(cfg *config.Config) string
}

func (h *countingCredentialHolder) credentials(cfg *config.Config) string { return h.reads(cfg) }

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
	rebuild := newRebuilder(quietLogger(), config.Settled(*cfg))
	server := &countingCredentialHolder{
		countingHolder: countingHolder{name: config.HolderAPIServer},
		reads:          func(cfg *config.Config) string { return credentialDigest(cfg.OperatorTokenFile) },
	}
	natsClient := &countingCredentialHolder{
		countingHolder: countingHolder{name: config.HolderNATS},
		reads:          func(cfg *config.Config) string { return credentialDigest(cfg.NatsNkeySeedFile) },
	}
	rebuild.register(server, natsClient)

	// The first reread after boot rebuilds nothing: each holder was built from its files as they
	// stand, and what they said was recorded as the holder was registered. Without that the
	// kubelet's first refresh of any watched directory — `agent_secrets.token_file`'s among them
	// — would reconnect NATS, migrate every durable consumer and drop every App lease.
	rebuild.rereadCredentials(cfg)
	if server.built != 0 || natsClient.built != 0 {
		t.Fatalf("the first reread rebuilt the API server %d times and NATS %d, want neither: nothing they read has moved since boot", server.built, natsClient.built)
	}

	// A wake whose watched files did not move rebuilds nothing either.
	rebuild.rereadCredentials(cfg)
	if server.built != 0 || natsClient.built != 0 {
		t.Fatalf("a reread over unmoved files rebuilt the API server %d times and NATS %d, want neither", server.built, natsClient.built)
	}

	// A rotation of the operator bearer in place rebuilds the API server, and only it.
	if err := os.WriteFile(operatorFile, []byte("rotated-operator\n"), 0o600); err != nil {
		t.Fatalf("rotate the operator token: %v", err)
	}
	rebuild.rereadCredentials(cfg)
	if server.built != 1 {
		t.Fatalf("the API server was rebuilt %d times over a rotated operator_token_file, want once", server.built)
	}
	if natsClient.built != 0 {
		t.Fatalf("NATS was rebuilt %d times over a rotation of someone else's file, want never", natsClient.built)
	}

	// And the wake after that rotation, with the file settled again, rebuilds nothing.
	rebuild.rereadCredentials(cfg)
	if server.built != 1 || natsClient.built != 0 {
		t.Fatalf("the reread after the rotation rebuilt the API server %d times and NATS %d, want 1 and 0", server.built, natsClient.built)
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
	rebuild := newRebuilder(w.log, config.Settled(*live.Snapshot()))
	live.OnChange(rebuild.observe)
	// The daemon's own open reads what the consumers hear as the rebuild starts
	// (workflowRuntime.connect); this one, whose consumers hear the boot's set and whose
	// rebuild is what the test is about, opens with it. The open and the swap run under the
	// mapping's own lock, and the handover — what the rebuilt consumers are to hear, worked out
	// and handed to them, and the swap — under the lock every fact applies under
	// (natsHandover), all the daemon's own.
	w.nats = newNATSHolder(conn, consumers, w.log, open, &w.mappingMu,
		func(consumers *intake.Consumers, renames ghrepo.Renames, swap func()) error {
			return w.natsHandover(t.Context(), consumers, renames, swap)
		})
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

// A NATS rebuild hears every repository the daemon hears now, not the set it booted on. The
// repositories GitHub intake hears move while the daemon runs — a mapping read replaces them on
// the consumers themselves (refreshMapping's SetRepositories) and names no setting, so nothing
// is rebuilt for it — and a rebuild that rewrote the shared durable consumer's FilterSubjects
// back to the boot's list would leave a repository mapped since boot unheard: JetStream would
// skip its merges, check runs and reviews, and redeliver none of them when the filter widened
// again.
func TestANatsRebuildHearsTheRepositoriesMappedSinceBoot(t *testing.T) {
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
	cfg := daemonGrantConfig(url)
	// The boot: the consumers are opened through the daemon's own path, with the repositories
	// the mapping serves now (grantWorkflow's sjawhar/legion).
	if err := w.connect(t.Context(), cfg, newRebuilder(w.log, config.Settled(cfg)), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	mapped := ghrepo.MustParse("acme/widgets")
	if err := w.nats.intake().SetRepositories(t.Context(), append(w.nats.intake().Repositories(), mapped)); err != nil {
		t.Fatalf("hear the repository mapped after boot: %v", err)
	}

	// Any rebuild: a rotated seed, a changed URL, a reread of the credential files.
	if err := w.nats.rebuild(&cfg); err != nil {
		t.Fatalf("rebuild the NATS holder: %v", err)
	}

	if heard := w.nats.intake().Repositories(); !slices.Contains(heard, mapped) {
		t.Fatalf("after the rebuild GitHub intake hears %v, want the repository mapped since boot among them", heard)
	}
	stream, err := js.Stream(t.Context(), "ENVOY_NOTIFICATIONS")
	if err != nil {
		t.Fatalf("open the notification stream: %v", err)
	}
	if filters := githubFiltersOf(t, stream, cfg.Project); !slices.Contains(filters, "notifications.github.acme.widgets.>") {
		t.Fatalf("the GitHub durable consumer filters on %v, want the subject of the repository mapped since boot among them", filters)
	}
}

// That read and the swap onto the new consumers run under one hold of the lock every fact
// applies under (intake.SerializeFacts): a mapping read holds that lock across its own
// SetRepositories (refreshMapping), so one that landed between the read and the swap would set
// its repositories on the consumers the rebuild is replacing, and the repository it mapped
// would go unheard with them until something else moved the mapping again.
func TestANatsRebuildWaitsForAMappingReadHoldingTheFactLock(t *testing.T) {
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
	w := grantWorkflow(t, &syncBuffer{})
	cfg := daemonGrantConfig(url)
	if err := w.connect(t.Context(), cfg, newRebuilder(w.log, config.Settled(cfg)), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)

	// A mapping read in flight: its transaction holds the fact lock from its record snapshot
	// through its SetRepositories.
	mapping, err := w.pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the mapping read's transaction: %v", err)
	}
	defer func() { _ = mapping.Rollback(t.Context()) }()
	if err := intake.SerializeFacts(t.Context(), mapping); err != nil {
		t.Fatalf("take the fact lock as a mapping read does: %v", err)
	}

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- w.nats.rebuild(&cfg) }()
	select {
	case err := <-rebuilt:
		t.Fatalf("the rebuild read what the consumers hear while a mapping read held the fact lock: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	// The mapping read maps a repository and commits; the rebuild then reads the set it left.
	mapped := ghrepo.MustParse("acme/widgets")
	if err := w.nats.intake().SetRepositories(t.Context(), append(w.nats.intake().Repositories(), mapped)); err != nil {
		t.Fatalf("hear the repository the mapping read mapped: %v", err)
	}
	if err := mapping.Commit(t.Context()); err != nil {
		t.Fatalf("commit the mapping read: %v", err)
	}
	select {
	case err := <-rebuilt:
		if err != nil {
			t.Fatalf("rebuild the NATS holder: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the rebuild did not return once the mapping read had committed")
	}

	if heard := w.nats.intake().Repositories(); !slices.Contains(heard, mapped) {
		t.Fatalf("after the rebuild GitHub intake hears %v, want the repository the mapping read mapped while the rebuild waited among them", heard)
	}
}

// The fact lock alone does not close the window, because opening the new consumers is itself an
// UpdateConsumer on the very GitHub durable the old intake is still pulling from, and that open
// runs outside the fact lock. So: the rebuild reads set A; a mapping read then widens the
// durable to B and serves the new mapping; the rebuild's open writes A back over B; and until
// the swap sets B again the old intake pulls under A. The pinned nats-server keeps a consumer's
// position across a filter update and steps past what the filter does not match, so a GitHub
// event of a repository in B and not in A that lands in that window is delivered to nothing,
// ever.
//
// The rebuild therefore holds the lock a mapping read is made under (mappingMu) from before it
// reads that set to after the swap: while a mapping read holds it the rebuild has not opened
// anything, and the set it opens with is the one that read left.
func TestANatsRebuildOpensNoConsumerWhileAMappingReadHoldsTheMappingLock(t *testing.T) {
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
	w := grantWorkflow(t, &syncBuffer{})
	cfg := daemonGrantConfig(url)
	if err := w.connect(t.Context(), cfg, newRebuilder(w.log, config.Settled(cfg)), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)

	// A mapping read in flight, holding the lock it is made under (refreshMapping): it has not
	// widened the durable's filter yet.
	w.mappingMu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			w.mappingMu.Unlock()
		}
	}()

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- w.nats.rebuild(&cfg) }()
	select {
	case err := <-rebuilt:
		t.Fatalf("the rebuild opened its consumers, and so wrote the GitHub durable's filter, while a mapping read held the mapping lock: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	// The mapping read widens the durable to the set it serves, then lets the lock go.
	mapped := ghrepo.MustParse("acme/widgets")
	if err := w.nats.intake().SetRepositories(t.Context(), append(w.nats.intake().Repositories(), mapped)); err != nil {
		t.Fatalf("hear the repository the mapping read mapped: %v", err)
	}
	w.mappingMu.Unlock()
	unlocked = true

	select {
	case err := <-rebuilt:
		if err != nil {
			t.Fatalf("rebuild the NATS holder: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the rebuild did not return once the mapping read had let the mapping lock go")
	}

	if heard := w.nats.intake().Repositories(); !slices.Contains(heard, mapped) {
		t.Fatalf("after the rebuild GitHub intake hears %v, want the repository the mapping read mapped while the rebuild waited among them", heard)
	}
	stream, err := js.Stream(t.Context(), "ENVOY_NOTIFICATIONS")
	if err != nil {
		t.Fatalf("open the notification stream: %v", err)
	}
	if filters := githubFiltersOf(t, stream, cfg.Project); !slices.Contains(filters, "notifications.github.acme.widgets.>") {
		t.Fatalf("the GitHub durable consumer filters on %v, want the subject of the repository the mapping read mapped among them", filters)
	}
}

// A `repository_renames` change on a running daemon has GitHub intake hear the pair's other
// name. Four things hold that key (config.Classes' row), the intake consumers among them, and
// their rebuild is what must add the partner: the set the last mapping read left was worked out
// under the renames before the change, so a rebuild that copied it would answer the operator
// `rebuilt` with the GitHub durable still filtering the old name's subjects alone — the merges,
// check runs and reviews GitHub publishes under the name the repository moved to delivered to
// nothing, and never redelivered, until something unrelated read the mapping again.
//
// The two holders are registered as the daemon registers them: the mapping before the consumers
// (workflowRuntime.connect), so the equivalence reaches the mapping first and the consumers'
// rebuild follows it. What they hear is worked out all the same from the configuration that
// rebuild is handed, not from the mapping's own (workflowRuntime.natsHearing). The call below
// is the registration a daemon with no workflow makes (daemon.go's serve) and decides no order:
// a second registration of a name keeps the position the first gave it.
func TestAChangedRepositoryRenamesHasGitHubIntakeHearThePartner(t *testing.T) {
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
	w := grantWorkflow(t, &syncBuffer{})
	cfg := daemonGrantConfig(url)
	rebuild := newRebuilder(w.log, config.Settled(cfg))
	if err := w.connect(t.Context(), cfg, rebuild, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	rebuild.register(newMappingHolder(w.repositories))
	stream, err := js.Stream(t.Context(), "ENVOY_NOTIFICATIONS")
	if err != nil {
		t.Fatalf("open the notification stream: %v", err)
	}
	const moved = "notifications.github.cybermonklabs.legion.>"
	if filters := githubFiltersOf(t, stream, cfg.Project); slices.Contains(filters, moved) {
		t.Fatalf("the GitHub durable consumer filters on %v before the change, so this proves nothing", filters)
	}

	// The operator configures the transfer while the daemon runs.
	changed := cfg
	changed.RepositoryRenames = ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	rebuild.observe(&changed, []config.Change{changeOfPath(t, "repository_renames")})

	if filters := githubFiltersOf(t, stream, cfg.Project); !slices.Contains(filters, moved) {
		t.Fatalf("the GitHub durable consumer filters on %v, want the subject of the name the repository moved to among them", filters)
	}

	// And the event GitHub publishes under that name reaches the daemon: JetStream matches the
	// subject onto the durable, and the decoder takes an event's repository from the set the
	// same swap set (intake's SetRepositories).
	heard := &heardPullRequests{}
	stop := runDaemonGrantConsumers(t, w, heard)
	defer stop()
	publishDaemonGrantEnvelope(t, js, "notifications.github.cybermonklabs.legion.pr.42", "github", "renamed-partner-pr-opened",
		`{"action":"opened","author":"author","base_ref":"main","body":"Dispatch: CAPTURE-1","head_ref":"legion/CAPTURE-1",`+
			`"head_sha":"head-captured","kind":"pr","number":"42","repo":"cybermonklabs/legion","title":"Captured GitHub event",`+
			`"updated_at":"2026-09-23T00:00:00Z","url":"https://github.com/cybermonklabs/legion/pull/42"}`)
	testwait.Eventually(t, "the daemon applying the pull request GitHub published under the name the repository moved to", func() bool {
		return slices.Contains(heard.repositories(), "cybermonklabs/legion")
	})
}

// A mapping read made between the two rebuilds a `repository_renames` change runs — the intake
// consumers' and the repository mapping's (config.Classes' row, Also) — hands those consumers no
// narrower a set of repositories than the one their filter was just built with.
//
// The read works out what GitHub intake is to hear under the mapping's own equivalence
// (refreshMapping, repositories.Renames), so the order the two holders are rebuilt in decides
// what it reads. Rebuilt the other way round — the consumers first — that read answers the pair
// before the change while the durable already filters on the pair after it, and hands the
// consumers a set without the name the repository moved to: the pinned nats-server steps a
// consumer past what its filter does not match, so every merge, check run and review GitHub
// publishes under the new name is delivered to nothing, with nothing to redeliver it.
//
// Nothing here decides that order: the holders are the daemon's own, registered by the daemon's
// own path (workflowRuntime.connect), the change goes through the real rebuilder.observe, and
// the read is the real refreshMapping, made from inside the consumers' own rebuild so that it
// lands in the window whatever the order is. What is asserted is what that read handed the
// consumers, not which holder ran first.
func TestAMappingReadBetweenTheRenameRebuildsNarrowsNoGitHubFilter(t *testing.T) {
	const project = "TRANSFER"
	url := workflowNATS(t)
	probe, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect to the test NATS server: %v", err)
	}
	t.Cleanup(probe.Close)
	js, err := jetstream.New(probe)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	stream, err := js.Stream(t.Context(), "ENVOY_NOTIFICATIONS")
	if err != nil {
		t.Fatalf("open the notification stream: %v", err)
	}

	// A workflow whose project maps one repository, with a Dispatch and a GitHub of its own, so
	// the mapping read below is the daemon's own read and not a stand-in for it.
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"})
	client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: project, Default: true}}}
	repos := newRepositories(project, ghrepo.Renames{})
	w := &workflowRuntime{
		pool: isolatedOutboxPool(t), records: record.NewStore(), dispatch: client, dispatchProject: project,
		installs: &installations{}, repositories: repos, log: quietLogger(), mappingChanged: make(chan struct{}, 1),
		defaultBranches: newDefaultBranches(roleTokens{}, repos, github.url, defaultBranchTTL, time.Now),
	}
	if err := w.loadMapping(t.Context()); err != nil {
		t.Fatalf("read the project's repository mapping: %v", err)
	}
	if err := w.nameMapping(t.Context()); err != nil {
		t.Fatalf("name the mapping as GitHub spells it: %v", err)
	}
	cfg := config.Config{Project: project, NatsURLs: []string{url}, WorkerRPCTimeout: time.Second,
		Projects: map[string]config.Project{project: {}}}
	rebuild := newRebuilder(w.log, config.Settled(cfg))
	if err := w.connect(t.Context(), cfg, rebuild, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	// The registration a daemon with no workflow makes (daemon.go's serve). It decides no order:
	// a second registration of a name keeps the position the first gave it.
	rebuild.register(newMappingHolder(w.repositories))

	// The read, made the moment the consumers' own rebuild returns: the first instant of the
	// window between the two holders, with the filter that rebuild has just written on the
	// durable.
	var filterBuiltWith, handed []ghrepo.Repository
	rebuild.register(&readingNATSHolder{natsHolder: w.nats, read: func() {
		filterBuiltWith = w.nats.intake().Repositories()
		if err := w.refreshMapping(t.Context()); err != nil {
			t.Errorf("read the repository mapping in the window: %v", err)
			return
		}
		handed = w.nats.intake().Repositories()
	}})

	// The operator configures the transfer while the daemon runs.
	moved := ghrepo.MustParse("acme-archive/widgets")
	changed := cfg
	changed.RepositoryRenames = ghrepo.MustRenames(ghrepo.MustRename("acme/widgets", moved.String()))
	rebuild.observe(&changed, []config.Change{changeOfPath(t, "repository_renames")})

	if !slices.Contains(filterBuiltWith, moved) {
		t.Fatalf("the consumers were rebuilt hearing %v, without the name the repository moved to, so a read that leaves it out narrows nothing and this proves nothing", filterBuiltWith)
	}
	if !slices.Contains(handed, moved) {
		t.Fatalf("the mapping read handed the consumers %v, want the name the repository moved to among them: it worked out what GitHub intake hears under renames older than the ones their filter was built with", handed)
	}
	for _, repo := range filterBuiltWith {
		if !slices.Contains(handed, repo) {
			t.Fatalf("the mapping read handed the consumers %v, narrower than the %v their filter had just been built with", handed, filterBuiltWith)
		}
	}
	if filters := githubFiltersOf(t, stream, project); !slices.Contains(filters, "notifications.github.acme-archive.widgets.>") {
		t.Fatalf("the GitHub durable consumer filters on %v, want the subject of the name the repository moved to among them", filters)
	}
}

// readingNATSHolder is the daemon's own NATS holder with a read of the repository mapping made
// the moment its rebuild returns, which is the window a `repository_renames` change opens
// between that rebuild and the mapping's. It answers the NATS holder's own name, so registering
// it replaces that holder where the daemon registered it and moves no holder's position
// (rebuilder.register).
type readingNATSHolder struct {
	*natsHolder
	read func()
}

func (h *readingNATSHolder) rebuild(cfg *config.Config) error {
	if err := h.natsHolder.rebuild(cfg); err != nil {
		return err
	}
	h.read()
	return nil
}

// heardPullRequests is an intake handler that records the repository of every pull request the
// daemon applied, so a test can say whose event reached it.
type heardPullRequests struct {
	mu    sync.Mutex
	repos []string
}

func (h *heardPullRequests) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if opened, is := fact.(intake.PullRequestOpened); is {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.repos = append(h.repos, opened.Repo)
	}
	return intake.Result{}, nil
}

func (h *heardPullRequests) repositories() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.repos)
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

// The two bounds of an update round are read where the round is decided, not off the
// configuration the daemon booted on: the notice bound at every sweep (supervision.live) and
// the re-task bound at every ask the machine decides (supervise.Timeouts.PodUpdateRetask,
// through liveBudgets). The rig runs the daemon's own sweep and machines over a settings holder
// a real legion.yaml backs, so a change here is the change an operator makes.
//
// The notice bound: a round pinned by an operator-suspended claim waits. Past the bound the
// rig's own configuration carries, and under the holder's, nothing is told — the sweep reads
// the holder — and the controller is told at the next sweep once the holder's bound is under
// the wait.
func TestThePodUpdateNoticeChangedUnderTheRunningSweepIsToldAtTheNextTick(t *testing.T) {
	live := liveSettings(t, "pod_update_notice_hours: 8")
	rig := newPodUpdateRigOn(t, live,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateSuspended, session: true,
			contract: api.DaemonAPIVersion, suspendedBy: supervise.SuspendedByOperator},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.clock.advance(podUpdateNotice)
	rig.tick()

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times %s after the round opened; the holder's bound is 8 hours",
			len(notices), podUpdateNotice)
	}

	apply(t, live, map[string]any{"pod_update_notice_hours": 1})
	rig.tick()

	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times under the bound the operator changed to an hour, want once", len(notices))
	}
	if notices[0].Update == nil || notices[0].Update.Round != podUpdateRound {
		t.Fatalf("the notice is %+v, want the stalled round's", notices[0])
	}
}

// The re-task bound: a worker that answered nothing is asked again only once the bound has
// passed since the last ask, and the bound is the holder's at that ask. Past the rig's own and
// under the holder's, the worker is not asked; once the operator shortens it, the next sweep
// asks again.
func TestThePodUpdateRetaskChangedUnderTheRunningSweepIsReadByTheNextAsk(t *testing.T) {
	live := liveSettings(t, "pod_update_retask_hours: 2", "pod_update_notice_hours: 8")
	rig := newPodUpdateRigOn(t, live, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	asked := rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	first := asked.UpdateTaskedAt
	// The ask ends with a turn of the worker's own, which answers nothing: what leaves a claim
	// asked and unanswered for the next ask to reach (TestPodUpdateAsksAgainAfterTheRetaskBound).
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token, DeliveryID: "a-delivery-of-the-worker-s-own"})
	rig.endTurn(claim.RoleImplementer, asked.Pending.ID)
	rig.awaitClaim(claim.RoleImplementer, "the unanswered ask to end with its turn", func(c supervise.Claim) bool {
		return c.Pending == nil
	})

	rig.clock.advance(podUpdateRetask)
	rig.tick()

	if at := rig.claimOf(claim.RoleImplementer).UpdateTaskedAt; !at.Equal(*first) {
		t.Fatalf("the worker was asked again at %s, %s after the first ask; the holder's bound is two hours", at, podUpdateRetask)
	}

	apply(t, live, map[string]any{"pod_update_retask_hours": 0.25})
	rig.tick()

	again := rig.awaitSwept(claim.RoleImplementer, "the worker to be asked again under the bound the operator shortened",
		func(c supervise.Claim) bool { return c.UpdateTaskedAt != nil && c.UpdateTaskedAt.After(*first) })
	if again.Pending == nil || again.Pending.ID == asked.Pending.ID {
		t.Errorf("the second ask = %+v, want a delivery of its own", again.Pending)
	}
}

// A capability decision an operator records reaches the next read of the state: the deployment's
// report is built from the settings holder at every read (supervision.deployment), so the row
// the daemon reported open stands decided with the operator's reason, at once and with nothing
// rebuilt.
func TestACapabilityDecisionChangedThroughTheRouteIsInTheNextStateRead(t *testing.T) {
	live := liveSettings(t)
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	if row := capabilityRow(t, d, capabilities.Secrets); row.Status != capabilities.StatusOpen {
		t.Fatalf("the secrets row before the decision = %+v, want it open: this deployment enrolls no process", row)
	}

	const reason = "dispatch://LEGION-205 enrolls pods later"
	applied := apply(t, live, map[string]any{"capabilities.decided": map[string]any{"secrets": reason}})

	if len(applied) != 1 || applied[0].Path != "capabilities.decided" {
		t.Fatalf("the apply reported %+v, want one capabilities.decided change", applied)
	}
	if applied[0].Outcome != config.OutcomeAtOnce || applied[0].Holder != "" {
		t.Fatalf("the change is %+v, want it taking effect at once with no holder", applied[0])
	}
	row := capabilityRow(t, d, capabilities.Secrets)
	if row.Status != capabilities.StatusDecided || row.Decision != reason {
		t.Fatalf("the secrets row after the decision = %+v, want it decided with the operator's reason", row)
	}
	if strings.Contains(logs.String(), "holder rebuilt") {
		t.Fatalf("a decision-class change rebuilt a holder:\n%s", logs)
	}
}

// capabilityRow is one row of the deployment's capability report as the running daemon's state
// carries it.
func capabilityRow(t *testing.T, d *daemon, name capabilities.Name) api.CapabilityState {
	t.Helper()
	for _, row := range d.state().Capabilities {
		if row.Name == string(name) {
			return row
		}
	}
	t.Fatalf("the state carries no %s capability row", name)
	return api.CapabilityState{}
}

// The two boot bounds are the runtime's to rebuild: every machine reads them per decision, and
// the Sandbox runtime holds them too — each wait of a launch and of a pod's observation is
// bounded by the boot timeout it was built with, and its launch bound is derived from it. The
// route answers `rebuilt` naming the runtime, and the runtime is built again under the work it
// is running, so no reader is left bounding a launch by the value the daemon booted on.
func TestTheWorkerBootBoundsChangedThroughTheRouteRebuildTheRuntime(t *testing.T) {
	live := liveSettings(t)
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(fake.NewRuntime(), &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)

	applied := apply(t, live, map[string]any{
		"worker_boot_timeout_seconds":                 90,
		"worker_boot_registration_deadline_intervals": 4,
	})

	if len(applied) != 2 {
		t.Fatalf("the apply reported %+v, want both boot bounds changed", applied)
	}
	for _, change := range applied {
		if change.Class != config.ClassRebuild || change.Outcome != config.OutcomeRebuilt || change.Holder != config.HolderRuntime {
			t.Fatalf("the change of %s is %+v, want a rebuild of the runtime", change.Path, change)
		}
	}
	awaitLog(t, logs, `"holder":"runtime"`, 5*time.Second, "the runtime was not built again for the boot bounds")
	state := d.state()
	if state.Settings.WorkerBootTimeoutSeconds != 90 {
		t.Errorf("the daemon reads worker_boot_timeout_seconds %d, want the applied 90", state.Settings.WorkerBootTimeoutSeconds)
	}
}

// The design gate reaches the next launch: the launch specs hold a copy of it for every tree
// architect's prompt, and the gate's row names them beside the API server (config.Row.Also), so
// an architect started after an operator moved the gate is told the policy the daemon decides
// by — and not the one it booted on.
func TestADesignGateChangedThroughTheRouteReachesTheNextArchitectsLaunch(t *testing.T) {
	live := liveSettings(t)
	rt := fake.NewRuntime()
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	booted := lastLaunch(t, rt, d.spawn(architect()))
	if want := DesignGateFragment(config.DesignGateRootIssues); !strings.Contains(booted.Prompt.Addressing, want) {
		t.Fatalf("the first architect is addressed %q, want it to carry %q", booted.Prompt.Addressing, want)
	}

	applied := apply(t, live, map[string]any{"gates.design": "off"})

	if len(applied) != 1 || applied[0].Path != "gates.design" || applied[0].New != string(config.DesignGateOff) {
		t.Fatalf("the apply reported %+v, want one gates.design change to off", applied)
	}
	if applied[0].Outcome != config.OutcomeAtOnce || applied[0].Holder != config.HolderAPIServer {
		t.Fatalf("the change is %+v, want it deciding at once with the API server named", applied[0])
	}
	awaitLog(t, logs, `"holder":"launch-specs"`, 5*time.Second, "the launch specs were not built again for the gate")
	next := lastLaunch(t, rt, d.spawn(api.SpawnRequest{
		Tree: "LEGION-2", Issue: "LEGION-2", Role: claim.RoleArchitect, Prompt: "Reply ready and wait.",
	}))
	if want := DesignGateFragment(config.DesignGateOff); !strings.Contains(next.Prompt.Addressing, want) {
		t.Fatalf("the architect started after the change is addressed %q, want it to carry %q", next.Prompt.Addressing, want)
	}
}

// The project block reaches the next launch too: the launch specs copy the project's merge rules
// into every merger's prompt and its Slack channels into the controller's, and the runtime hands
// the daemon-launched controller's pod the same channels, so the row names both beside the API
// server. The diff names `projects` whatever member moved — the block is compared whole — so one
// change of a project's merge rules is what proves all three were built again.
//
// The change carries `github_apps` beside it because the loader takes a `projects` block in no
// file without one (config's resolveStage3), and a file holding neither is what this harness
// boots: the key this test is about is the project's own merge rules. This daemon has no
// Dispatch and so no App manager to build again, which the rebuilder logs and nothing else.
func TestTheProjectsBlockChangedThroughTheRouteReachesTheNextMergersLaunch(t *testing.T) {
	live := liveSettings(t)
	rt := fake.NewRuntime()
	logs := &syncBuffer{}
	d := startDaemonOn(t, live, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
	// A worker binds the work of a tree an architect opened, so the root goes first.
	d.spawn(architect())
	merger := func(issue string) runtime.SpawnSpec {
		return lastLaunch(t, rt, d.spawn(api.SpawnRequest{Tree: "LEGION-1", Issue: issue, Role: claim.RoleMerger, Prompt: "Wait."}))
	}
	if want := MergeRulesFragment(config.MergeRules{}); !strings.Contains(merger("LEGION-2").Prompt.Addressing, want) {
		t.Fatalf("the first merger is addressed without %q, want the file's own rules", want)
	}

	applied := apply(t, live, map[string]any{
		"projects": map[string]any{
			live.Snapshot().Project: map[string]any{"merge": map[string]any{"by_hand": []string{"^docs/"}}},
		},
		"github_apps": map[string]any{
			"implement": map[string]any{"app_id": "1", "private_key": "implement-test-key"},
			"review":    map[string]any{"app_id": "2", "private_key": "review-test-key"},
		},
	})

	projects, held := changeOf(applied, "projects")
	if !held {
		t.Fatalf("the apply reported %+v, want the projects block among the changes", applied)
	}
	if projects.Outcome != config.OutcomeRebuilt || projects.Holder != config.HolderAPIServer {
		t.Fatalf("the change is %+v, want a rebuild naming the API server", projects)
	}
	for _, holder := range []string{config.HolderLaunchSpecs, config.HolderRuntime} {
		awaitLog(t, logs, `"holder":"`+holder+`"`, 5*time.Second, "the project's change left "+holder+" holding the boot's block")
	}
	if addressing := merger("LEGION-3").Prompt.Addressing; !strings.Contains(addressing, "`^docs/`") {
		t.Fatalf("the merger started after the change is addressed %q, want the rules the operator set", addressing)
	}
}

// changeOf is the applied change of path, and whether the apply reported one.
func changeOf(applied []config.Change, path string) (config.Change, bool) {
	for _, change := range applied {
		if change.Path == path {
			return change, true
		}
	}
	return config.Change{}, false
}
