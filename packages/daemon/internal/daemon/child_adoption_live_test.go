//go:build e2e

// The live proof's driver for INFRA-252: a labelled root still queued in admission, which a person
// then makes a child of a live tree, joins that tree and runs there, rather than taking a slot and
// an architect of its own. Admission's sweep adopts it once its recorded parent is in the live tree
// (admit's adoptQueuedRoots); an architect's release_children adopts it from the Dispatch chain it
// reads, from todo, from backlog, and from the state INFRA-214 reached, before it answers
// (api's adoptReleasedChildren); a todo root under an unreleased child of a live tree waits in no
// line (record.Waiting's AncestorTree); and a child recorded in another tree is refused by name.
// scripts/e2e/child-adoption.sh builds this driver with `-tags e2e` and owns the nats-server and
// the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration load,
// the real store and its migrations, the real HTTP API, the real claim supervision, the real
// openWorkflow, the real JetStream consumers and the real outbox lanes. Facts enter only by the
// transports production has: the Dispatch envelopes of the ENVOY_NOTIFICATIONS stream and the
// daemon's own HTTP routes. A step reads GET /legion/v1/state for the admission line and each
// issue's phase, generation, slot, architect and planner; POST /legion/v1/children/park answering
// 200, not 403 ISSUE_OUTSIDE_TREE, as the route-visible proof that an issue is the architect's
// tree's; what the daemon wrote to the Dispatch stand-in; and, for tree membership, which no route
// shows, the daemon's own record of the issue in its PostgreSQL (record.Store), as
// merged_root_waits_live_test.go reads and writes it. Step 4 reads the outbox table for the rows
// the old linger queued, which no route shows either.
//
// What the proof stands in for lies outside the change. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. Dispatch is childAdoptionBoard: it holds each
// issue's status, parent and labels as Dispatch's board does, answers the issue reads the release
// and the event preparation make, and records every status and message the daemon writes. Envoy's
// listener is the package's noticeListener. GitHub is childAdoptionGitHub, which knows each
// repository's default branch and takes each issue branch the workflow creates. No agent runs:
// every claim is launched by the package's fake runtime, which provisions each claim's workspace in
// its own pod (fake.Runtime.InPod), so no host clone is made; the tree's architect is brought to
// ready over the daemon's own worker stream and HTTP routes. The live tree, the slot a second tree
// holds, and step 6's other tree are seeded through the store, as every daemon test seeds its
// fixture; every issue the change acts on is recorded by the daemon from its own envelopes.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

const (
	// childAdoptionRepo is the repository the project's Dispatch mapping serves (servedMapping).
	childAdoptionRepo = "acme/widgets"
	// childAdoptionCap is the admission cap: the live tree holds one slot and a second tree the
	// other, so a queued root waits in the line until step 5 frees the second slot.
	childAdoptionCap = 2
	// childAdoptionLinger is how long a tree that left the workflow lingers: long enough for step
	// 4 to release a root inside its linger, as INFRA-214's release came, and short enough for the
	// step to wait the linger out.
	childAdoptionLinger = 8 * time.Second
	// childAdoptionSettle is how long a step that must see nothing happen watches for it.
	childAdoptionSettle = 3 * time.Second
	// childAdoptionOutboxPoll is the margin past a row's due time a step allows the outbox lane
	// that wakes for it (outbox_lanes.go arms a timer at the row's next_at) to run it.
	childAdoptionOutboxPoll = 2 * time.Second
	// childAdoptionGate is the live tree's approved design document.
	childAdoptionGate = "5b7c9e1a-3d2f-4a6b-8c0e-1f3a5b7d9e2c"
)

// childAdoptionLive is one run of the proof: the booted daemon, the live tree it runs, and the
// stand-ins the steps read the daemon's writes back from.
type childAdoptionLive struct {
	t       *testing.T
	cfg     config.Config
	rt      *fake.Runtime
	built   *built
	d       *daemon
	js      jetstream.JetStream
	board   *childAdoptionBoard
	notices *noticeListener
	st      *store.Store
	records record.Store
	project string
	// tree is the live tree's root, T, which holds a slot and whose design gate is approved, so a
	// todo child that joins it starts planning at once; filler, X, is a second tree holding the
	// cap's other slot until step 5 parks it.
	tree, filler string
	// architect is T's architect session, registered once over the daemon's own routes.
	architect *childAdoptionArchitect
	// seq is the Dispatch sequence the next envelope the driver publishes carries.
	seq int64
}

// TestAQueuedRootUnderALiveTreeJoinsItLive drives the six steps of the proof against one booted
// daemon.
func TestAQueuedRootUnderALiveTreeJoinsItLive(t *testing.T) {
	natsURL := os.Getenv(mergedRootWaitsNATSURL)
	if natsURL == "" {
		t.Skipf("%s is unset, so there is no broker to drive the daemon's intake through; scripts/e2e/child-adoption.sh starts a nats-server and sets it", mergedRootWaitsNATSURL)
	}
	r := newChildAdoptionLive(t, natsURL)
	r.step("queued-root-leaves-the-line", r.queuedRootLeavesTheLine)
	r.step("release-adopts-from-todo", r.releaseAdoptsFromTodo)
	r.step("release-adopts-from-backlog", r.releaseAdoptsFromBacklog)
	r.step("release-adopts-infra-214-state", r.releaseAdoptsInfra214State)
	r.step("held-under-an-unreleased-child", r.heldUnderAnUnreleasedChild)
	r.step("release-refuses", r.releaseRefuses)
}

// newChildAdoptionLive boots the daemon over natsURL with the live tree and the second slotted
// tree recorded, and registers the live tree's architect.
func newChildAdoptionLive(t *testing.T, natsURL string) *childAdoptionLive {
	t.Helper()
	cfg := workflowConfig(t, natsURL)
	cfg.AdmissionCap = childAdoptionCap
	cfg.Linger = childAdoptionLinger
	board := newChildAdoptionBoard(t, cfg.Project)
	dispatchServer := httptest.NewServer(board)
	t.Cleanup(dispatchServer.Close)
	cfg.DispatchURL = dispatchServer.URL
	notices := newNoticeListener()
	envoyServer := httptest.NewServer(notices)
	t.Cleanup(envoyServer.Close)
	cfg.EnvoyURL = envoyServer.URL

	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	rt := fake.NewRuntime()
	rt.InPod = true
	r := &childAdoptionLive{
		t: t, cfg: cfg, rt: rt, built: &built{}, board: board, notices: notices, records: record.NewStore(), project: project,
		tree: cfg.Project + "-1", filler: cfg.Project + "-2", seq: 1,
	}
	r.js = mergedRootWaitsStream(t, natsURL)
	ctx := context.Background()
	if r.st, err = store.Open(ctx, cfg.PostgresDSN); err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(r.st.Close)
	if _, err := r.st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	r.seedTrees()
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = childAdoptionGitHub(t)
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), 60*time.Second)
	r.architect = r.registerArchitect()
	return r
}

// childAdoptionGitHub is GitHub as this proof's daemon reads it: every repository has default
// branch main at one commit, each issue branch the workflow creates there is taken, and its GraphQL
// root knows no pull request, so each disarm finishes with nothing armed. Nothing reaches
// github.com.
func childAdoptionGitHub(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
		parts := strings.Split(rest, "/")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		case r.Method == http.MethodGet && isRepository && len(parts) == 2:
			_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
		case r.Method == http.MethodGet && isRepository && strings.HasSuffix(rest, "/git/ref/heads/main"):
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"base-main"}}`)
		case r.Method == http.MethodPost && isRepository && strings.HasSuffix(rest, "/git/refs"):
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// seedTrees records the two trees admission's cap holds as the daemon would hold them mid-run: T
// in planning with its slot, its approved design gate, its lifecycle open under the workflow and
// its architect claim as a launch a previous daemon began and never recorded, which the boot
// relaunches; and X, a second root in planning holding the other slot.
func (r *childAdoptionLive) seedTrees() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	for _, key := range []string{r.tree, r.filler} {
		r.board.show(key, "in_progress", "", true)
	}
	approved := 1
	r.write("the live tree and the second slotted tree", func(tx pgx.Tx) error {
		for i, key := range []string{r.tree, r.filler} {
			if err := r.records.PutIssue(ctx, tx, record.Issue{
				Key: key, Repo: childAdoptionRepo, DefaultBranch: "main", GitHubRepo: childAdoptionRepo, Tree: key, Project: r.cfg.Project,
				Title: key, Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: fmt.Sprintf("A%d", i),
				LastDispatchSeq: r.seq, HandedOver: true,
			}); err != nil {
				return err
			}
			if err := r.putSlot(ctx, tx, key); err != nil {
				return err
			}
		}
		return r.records.PutGate(ctx, tx, record.DesignGate{Issue: r.tree, ArtifactID: childAdoptionGate, LatestVersion: 1, ApprovedVersion: &approved})
	})
	for _, key := range []string{r.tree, r.filler} {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", key, err)
		}
	}
	architect := r.token(r.tree, claim.RoleArchitect)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: architect, Project: r.project, Tree: r.tree, Issue: r.tree, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch: %v", err)
	}
	writePrompt(t, r.cfg, architect)
}

// putSlot gives key the next free admission slot: slot indexes are unique across the shared
// database, so a tree takes the next one free.
func (r *childAdoptionLive) putSlot(ctx context.Context, tx pgx.Tx, key string) error {
	taken, err := r.records.Slots(ctx, tx)
	if err != nil {
		return err
	}
	index := 0
	for _, slot := range taken {
		index = max(index, slot.Index+1)
	}
	return r.records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: index, AdmittedAt: time.Now()})
}

// log is where the booted daemon writes its own log: the file mergedRootWaitsLog names, and
// nowhere when it names none.
func (r *childAdoptionLive) log() *slog.Logger {
	r.t.Helper()
	path := os.Getenv(mergedRootWaitsLog)
	if path == "" {
		return quietLogger()
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.t.Fatalf("open the daemon's log %s: %v", path, err)
	}
	r.t.Cleanup(func() { file.Close() })
	return slog.New(slog.NewTextHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// childAdoptionArchitect is T's architect session as its agent holds it: the session its
// registration named and the capability secret the registration returned, which buys its grants.
type childAdoptionArchitect struct {
	session string
	secret  string
}

// registerArchitect brings T's relaunched architect claim to ready, as its pane's shim and its
// agent do: it waits for the launch, dials the worker stream with the launch's boot token,
// registers the session and reports it ready.
func (r *childAdoptionLive) registerArchitect() *childAdoptionArchitect {
	t := r.t
	t.Helper()
	token := r.token(r.tree, claim.RoleArchitect)
	testwait.Eventually(t, "the launch of "+string(token), func() bool { return r.launched(token) })
	launch := lastLaunch(t, r.rt, token)
	dialShim(t, r.built.address, launch.BootToken)
	const session = "ses_architect"
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", token, err)
	}
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	return &childAdoptionArchitect{session: session, secret: registered.Secret}
}

// grant is a capability grant minted for T's architect now, as its `legion` tool mints one per
// operation.
func (r *childAdoptionLive) grant() string {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: r.architect.session, Secret: r.architect.secret, Tree: r.tree, Issue: r.tree}, false)
	if status != http.StatusOK {
		t.Fatalf("the architect's grant = %d %s, want 200", status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the architect's grant: %v", err)
	}
	return granted.GrantID
}

// release is T's architect's release_children of keys, as POST /legion/v1/waves/release: the
// status and, for any answer but 200, the failure it names.
func (r *childAdoptionLive) release(keys ...string) (int, api.Failure) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/waves/release", api.WaveReleaseRequest{GrantID: r.grant(), Issues: keys}, false)
	var failure api.Failure
	if status != http.StatusOK {
		if err := json.Unmarshal(body, &failure); err != nil {
			r.t.Fatalf("decode the release's answer %d %s: %v", status, body, err)
		}
	}
	return status, failure
}

// parks requires T's architect's park_child of key to answer 200: the route serves only an issue
// recorded in the architect's own tree (403 ISSUE_OUTSIDE_TREE otherwise), so the 200 is the
// route-visible proof that key is T's.
func (r *childAdoptionLive) parks(key string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/children/park", api.ChildRequest{GrantID: r.grant(), Issue: key}, false)
	if status != http.StatusOK {
		r.t.Fatalf("park_child of %s by the architect of %s = %d %s, want 200", key, r.tree, status, body)
	}
	r.note("drove POST /legion/v1/children/park of %s by %s's architect: %d", key, r.tree, status)
}

// token is the claim token of role on issue, as the daemon spells it.
func (r *childAdoptionLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// launched says whether the fake runtime was asked to launch token's claim.
func (r *childAdoptionLive) launched(token claim.Token) bool {
	return slices.ContainsFunc(r.rt.Calls(), func(call fake.Call) bool {
		return (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token
	})
}

// stopped is every call the fake runtime took that stops or ends one of issue's claims, or
// cleans up a tree under issue's key.
func (r *childAdoptionLive) stopped(issue string) []string {
	tokens := map[claim.Token]bool{}
	for _, role := range claim.Roles {
		tokens[r.token(issue, role)] = true
	}
	var got []string
	for _, call := range r.rt.Calls() {
		switch {
		case call.Method == "Suspend" && tokens[call.Locator.Claim],
			call.Method == "Release" && tokens[call.Released.Claim],
			call.Method == "CleanupTree" && call.Tree == issue:
			got = append(got, fmt.Sprintf("%s %s%s%s", call.Method, call.Locator.Claim, call.Released.Claim, call.Tree))
		}
	}
	return got
}

// step runs one of the proof's steps between its `== name` and `ok name (<s>s)` lines.
func (r *childAdoptionLive) step(name string, body func()) {
	r.t.Helper()
	started := time.Now()
	fmt.Printf("== %s\n", name)
	body()
	fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
}

// note prints what a step drove or read, under its heading.
func (r *childAdoptionLive) note(format string, args ...any) {
	fmt.Printf("   "+format+"\n", args...)
}

// write runs body in one transaction against the daemon's own database, failing the test with
// what it was seeding on an error.
func (r *childAdoptionLive) write(what string, body func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := r.st.Tx(context.Background(), body); err != nil {
		r.t.Fatalf("seed %s: %v", what, err)
	}
	r.note("seeded: %s", what)
}

// recorded is the daemon's own record of key in its PostgreSQL, nil when it records none.
func (r *childAdoptionLive) recorded(key string) *record.Issue {
	r.t.Helper()
	var issue *record.Issue
	if err := r.st.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		r.t.Fatalf("read the record of %s: %v", key, err)
	}
	return issue
}

// inTree requires the daemon's record of key to name tree as its tree, and returns the record.
func (r *childAdoptionLive) inTree(key, tree string) record.Issue {
	r.t.Helper()
	issue := r.recorded(key)
	if issue == nil {
		r.t.Fatalf("the daemon records no %s", key)
	}
	if issue.Tree != tree {
		r.t.Fatalf("the daemon records %s in the tree of %s, want %s", key, issue.Tree, tree)
	}
	return *issue
}

// state is GET /legion/v1/state now.
func (r *childAdoptionLive) state() api.State {
	r.t.Helper()
	return r.d.state()
}

// issue is key as GET /legion/v1/state shows it now.
func (r *childAdoptionLive) issue(key string) api.Issue {
	r.t.Helper()
	issue, ok := r.state().Issues[key]
	if !ok {
		r.t.Fatalf("the state records no issue %s", key)
	}
	return issue
}

// waiting says whether key is in GET /legion/v1/state's admission.waiting now.
func (r *childAdoptionLive) waiting(key string) bool {
	r.t.Helper()
	return slices.Contains(r.state().Admission.Waiting, key)
}

// waitFor polls the state until want is true of key; when the wait times out, it logs what the
// state and the daemon's record last showed of key.
func (r *childAdoptionLive) waitFor(what, key string, want func(api.Issue) bool) api.Issue {
	r.t.Helper()
	var last api.State
	held := false
	defer func() {
		if !held {
			r.t.Logf("%s as last seen: %s", key, r.describe(last, key))
		}
	}()
	testwait.Eventually(r.t, what, func() bool {
		last = r.state()
		issue, ok := last.Issues[key]
		return ok && want(issue)
	})
	held = true
	return last.Issues[key]
}

// describe is key as state shows it, with the tree the daemon's record names.
func (r *childAdoptionLive) describe(state api.State, key string) string {
	issue, ok := state.Issues[key]
	if !ok {
		return "not in the state"
	}
	slot, architect, tree := "none", "none", "none"
	if issue.Slot != nil {
		slot = fmt.Sprint(issue.Slot.Index)
	}
	if issue.Architect != nil {
		architect = issue.Architect.State
	}
	if recorded := r.recorded(key); recorded != nil {
		tree = recorded.Tree
	}
	return fmt.Sprintf("phase %s, generation %d, status %s, slot %s, architect %s, workers %v, admission.waiting %v, recorded in the tree of %s",
		issue.Phase, issue.Generation, issue.Status, slot, architect, slices.Sorted(maps.Keys(issue.Workers)), state.Admission.Waiting, tree)
}

// waitWaiting waits for key to be in admission.waiting.
func (r *childAdoptionLive) waitWaiting(key string) {
	r.t.Helper()
	testwait.Eventually(r.t, key+" in admission.waiting", func() bool { return r.waiting(key) })
	r.note("read back: %s is in admission.waiting %v", key, r.state().Admission.Waiting)
}

// planning says whether issue is in planning with a planner claim.
func planning(issue api.Issue) bool {
	_, planner := issue.Workers[claim.RolePlanner]
	return issue.Phase == phase.Planning && planner
}

// runsInTree requires key, as GET /legion/v1/state shows it, to be planning with its planner's
// claim, holding no slot and no architect of its own, and out of the admission line.
func (r *childAdoptionLive) runsInTree(key string) api.Issue {
	r.t.Helper()
	got := r.waitFor(key+" planning with its planner's claim", key, planning)
	if got.Slot != nil {
		r.t.Fatalf("%s holds admission slot %d, want none: it runs in the tree of %s", key, got.Slot.Index, r.tree)
	}
	if got.Architect != nil {
		r.t.Fatalf("%s has an architect of its own (%s), want none: it runs in the tree of %s", key, got.Architect.State, r.tree)
	}
	if r.waiting(key) {
		r.t.Fatalf("%s is still in admission.waiting %v", key, r.state().Admission.Waiting)
	}
	r.note("read back: %s is in %s at generation %d with its planner's claim %s, no slot, no architect, not in admission.waiting",
		key, got.Phase, got.Generation, got.Workers[claim.RolePlanner].Claim.State)
	return got
}

// statusWrites is every status write the daemon made to the Dispatch stand-in after the first
// since writes.
func (r *childAdoptionLive) statusWrites(since int) []childAdoptionWrite {
	var got []childAdoptionWrite
	for _, write := range r.board.writes()[since:] {
		if write.Status != "" {
			got = append(got, write)
		}
	}
	return got
}

// outboxMark is the newest outbox row id now: a row queued later has a greater id.
func (r *childAdoptionLive) outboxMark() int64 {
	r.t.Helper()
	var mark int64
	if err := r.st.Pool().QueryRow(context.Background(), "select coalesce(max(id), 0) from outbox").Scan(&mark); err != nil {
		r.t.Fatalf("read the outbox's newest row: %v", err)
	}
	return mark
}

// outboxRows counts key's outbox rows matching where, after mark.
func (r *childAdoptionLive) outboxRows(key string, mark int64, where string) int {
	r.t.Helper()
	var n int
	if err := r.st.Pool().QueryRow(context.Background(),
		"select count(*) from outbox where issue = $1 and id > $2 and "+where, key, mark).Scan(&n); err != nil {
		r.t.Fatalf("count the outbox rows of %s: %v", key, err)
	}
	return n
}

// told is every child-status notice the daemon published to T's architect topic, by its reason.
func (r *childAdoptionLive) told() []string {
	r.t.Helper()
	var reasons []string
	for _, published := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(r.token(r.tree, claim.RoleArchitect))) {
		var payload struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(published.Payload), &payload); err != nil {
			r.t.Fatalf("decode the notice payload %q: %v", published.Payload, err)
		}
		if payload.Kind == "child-status" {
			reasons = append(reasons, payload.Reason)
		}
	}
	return reasons
}

// key is this run's issue n.
func (r *childAdoptionLive) key(n int) string { return fmt.Sprintf("%s-%d", r.cfg.Project, n) }

// Step 1 (AC1). C, a labelled todo root with no parent, queues in admission. A person makes it a
// child of T, and Dispatch's event says so: C leaves the line and joins T, planning under T's open
// gate with no slot and no architect of its own, and T's architect is told.
func (r *childAdoptionLive) queuedRootLeavesTheLine() {
	t := r.t
	c := r.key(3)
	r.publishIssue(c, "todo", "", true, false)
	r.note("drove %s labelled todo with no parent: a notifications.dispatch.issue.%s.issue.updated envelope", c, c)
	r.waitWaiting(c)
	r.publishIssue(c, "todo", r.tree, true, false)
	r.note("drove %s's re-parent under %s: an issue.updated envelope, status todo, parent %s", c, r.tree, r.tree)
	got := r.runsInTree(c)
	recorded := r.inTree(c, r.tree)
	r.note("read back: the daemon records %s in the tree of %s at generation %d, parent %s", c, recorded.Tree, recorded.Generation, *recorded.Parent)
	if got.Generation != recorded.Generation {
		t.Fatalf("the state shows %s at generation %d and the record at %d", c, got.Generation, recorded.Generation)
	}
	architect := r.token(c, claim.RoleArchitect)
	deadline := time.Now().Add(childAdoptionSettle)
	for time.Now().Before(deadline) {
		if r.launched(architect) {
			t.Fatalf("the fake runtime was asked to launch %s, want no architect of %s's own", architect, c)
		}
		if got := r.issue(c); got.Slot != nil || got.Architect != nil || r.waiting(c) {
			t.Fatalf("within %s of joining, %s took a slot, an architect or a place in the line: %+v", childAdoptionSettle, c, got)
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.note("read back over %s: the fake runtime was asked to launch no %s, and %s took no slot", childAdoptionSettle, architect, c)
	want := fmt.Sprintf("%s joined the tree of %s from the admission queue", c, r.tree)
	testwait.Eventually(t, "the child-status notice to the architect of "+r.tree, func() bool { return slices.Contains(r.told(), want) })
	r.note("read back: a child-status notice on %s's architect topic — %s", r.tree, want)
}

// Step 2 (AC2, todo). D, a labelled todo root, queues. Dispatch shows it as a child of T, but no
// event of the re-parent reaches the daemon. T's architect releases it: the release adopts D before
// it answers, so D is in T and planning with no further envelope, the release wrote nothing to
// Dispatch, and T's architect can park it.
func (r *childAdoptionLive) releaseAdoptsFromTodo() {
	t := r.t
	d := r.key(4)
	r.publishIssue(d, "todo", "", true, false)
	r.note("drove %s labelled todo with no parent: an issue.updated envelope", d)
	r.waitWaiting(d)
	r.board.show(d, "todo", r.tree, true)
	r.note("the Dispatch stand-in shows %s todo under %s; no envelope of the re-parent is published", d, r.tree)
	writes := len(r.board.writes())
	status, failure := r.release(d)
	if status != http.StatusOK {
		t.Fatalf("release_children of %s = %d %s %s, want 200", d, status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/waves/release {%s} by %s's architect: %d", d, r.tree, status)
	recorded := r.inTree(d, r.tree)
	got := r.issue(d)
	if got.Phase != phase.Planning {
		t.Fatalf("%s is in %s as the release answers, want planning", d, got.Phase)
	}
	r.note("read back as the release answered, before any envelope: the daemon records %s in the tree of %s at generation %d, and the state shows it in %s",
		d, recorded.Tree, recorded.Generation, got.Phase)
	r.runsInTree(d)
	if wrote := r.statusWrites(writes); len(wrote) != 0 {
		t.Fatalf("the release wrote %+v to Dispatch, want nothing: %s was already todo", wrote, d)
	}
	r.note("read back: the Dispatch stand-in took no status write")
	r.parks(d)
}

// Step 3 (AC2, backlog). F, a labelled todo root, queues, then a person moves it to backlog, which
// takes it out of the line and lingers it. Dispatch shows it re-parented under T, still in backlog,
// with no event of that. T's architect releases it: before the release answers, F is in T, parked
// at done, and the release wrote todo to Dispatch. That write's own event runs F in T.
func (r *childAdoptionLive) releaseAdoptsFromBacklog() {
	t := r.t
	f := r.key(5)
	r.publishIssue(f, "todo", "", true, false)
	r.note("drove %s labelled todo with no parent: an issue.updated envelope", f)
	r.waitWaiting(f)
	r.publishIssue(f, "backlog", "", true, false)
	r.note("drove %s to backlog: an issue.updated envelope", f)
	left := r.waitFor(f+" out of the line in backlog", f, func(got api.Issue) bool { return got.Status == "backlog" && got.Phase == phase.Done })
	if r.waiting(f) {
		t.Fatalf("%s is in admission.waiting in backlog", f)
	}
	if lingering := r.recorded(f); lingering.LingerUntil == nil {
		t.Fatalf("%s left the workflow without lingering", f)
	}
	r.note("read back: %s is %s, phase %s, out of admission.waiting, its record lingering", f, left.Status, left.Phase)
	r.board.show(f, "backlog", r.tree, true)
	r.note("the Dispatch stand-in shows %s backlog under %s; no envelope of the re-parent is published", f, r.tree)
	writes := len(r.board.writes())
	status, failure := r.release(f)
	if status != http.StatusOK {
		t.Fatalf("release_children of %s = %d %s %s, want 200", f, status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/waves/release {%s} by %s's architect: %d", f, r.tree, status)
	recorded := r.inTree(f, r.tree)
	if recorded.Phase != phase.Done || recorded.LingerUntil != nil {
		t.Fatalf("the daemon records %s in %s (lingering %v) as the release answers, want parked at done with no linger", f, recorded.Phase, recorded.LingerUntil)
	}
	r.note("read back as the release answered, before any envelope: the daemon records %s in the tree of %s at generation %d, phase %s, no linger",
		f, recorded.Tree, recorded.Generation, recorded.Phase)
	wrote := r.statusWrites(writes)
	if len(wrote) != 1 || wrote[0].Issue != f || wrote[0].Status != "todo" {
		t.Fatalf("the release wrote %+v to Dispatch, want the one todo of %s", wrote, f)
	}
	r.note("read back: the Dispatch stand-in took the release's PATCH of %s to todo", f)
	r.publishIssue(f, "todo", r.tree, true, true)
	r.note("drove that write's own event: an issue.updated envelope of %s, status todo, actor %s", f, dispatch.DaemonSession(f))
	r.runsInTree(f)
	r.inTree(f, r.tree)
	r.parks(f)
}

// Step 4 (AC3). E reaches INFRA-214's state: a labelled todo root, moved to backlog (which lingers
// it), its label taken off and moved back to todo, so admission readmits nothing. Dispatch shows it
// under T with no event of that. T's architect releases it inside its linger: E is in T and
// planning, and once its old linger's deadline passes, the linger's close stops none of it.
func (r *childAdoptionLive) releaseAdoptsInfra214State() {
	t := r.t
	e := r.key(6)
	r.publishIssue(e, "todo", "", true, false)
	r.note("drove %s labelled todo with no parent: an issue.updated envelope", e)
	r.waitWaiting(e)
	r.publishIssue(e, "backlog", "", true, false)
	r.note("drove %s to backlog: an issue.updated envelope", e)
	r.waitFor(e+" in backlog", e, func(got api.Issue) bool { return got.Status == "backlog" && got.Phase == phase.Done })
	lingering := r.recorded(e)
	if lingering.LingerUntil == nil {
		t.Fatalf("%s left the workflow without lingering", e)
	}
	deadline, generation := *lingering.LingerUntil, lingering.Generation
	r.note("read back: %s lingers at generation %d until %s", e, generation, deadline.Format(time.RFC3339Nano))
	r.publishIssue(e, "backlog", "", false, false)
	r.publishIssue(e, "todo", "", false, false)
	r.note("drove %s's label off, then %s back to todo with no label: two issue.updated envelopes", e, e)
	r.waitFor(e+" todo without the label", e, func(got api.Issue) bool { return got.Status == "todo" })
	if got := r.issue(e); got.Phase != phase.Done || r.waiting(e) {
		t.Fatalf("%s is in %s (waiting %v) after its unlabelled todo, want done and out of the line, as INFRA-214 stood", e, got.Phase, r.waiting(e))
	}
	r.note("read back: %s is todo, phase done, not in admission.waiting — INFRA-214's state", e)
	r.board.show(e, "todo", r.tree, false)
	r.note("the Dispatch stand-in shows %s todo under %s; no envelope of the re-parent is published", e, r.tree)
	mark := r.outboxMark()
	status, failure := r.release(e)
	if status != http.StatusOK {
		t.Fatalf("release_children of %s = %d %s %s, want 200", e, status, failure.Code, failure.Error)
	}
	released := time.Now()
	if !released.Before(deadline) {
		t.Fatalf("the release came at %s, past %s's linger deadline %s: raise childAdoptionLinger", released.Format(time.RFC3339Nano), e, deadline.Format(time.RFC3339Nano))
	}
	r.note("drove POST /legion/v1/waves/release {%s} by %s's architect inside its linger: %d", e, r.tree, status)
	recorded := r.inTree(e, r.tree)
	if got := r.issue(e); got.Phase != phase.Planning {
		t.Fatalf("%s is in %s as the release answers, want planning", e, got.Phase)
	}
	r.note("read back as the release answered: the daemon records %s in the tree of %s at generation %d, the state shows it in planning", e, recorded.Tree, recorded.Generation)
	adopted := r.runsInTree(e)

	time.Sleep(time.Until(deadline.Add(childAdoptionOutboxPoll)))
	testwait.Eventually(t, "the old linger's close of "+e+" run", func() bool { return r.outboxRows(e, 0, "kind = 'linger_close'") == 0 })
	r.note("read back past the old deadline plus %s: no linger_close row of %s is left in the outbox — the old linger's close ran", childAdoptionOutboxPoll, e)
	got := r.issue(e)
	if !planning(got) || got.Generation != adopted.Generation {
		t.Fatalf("after its old linger's deadline %s is in %s at generation %d with workers %v, want planning at %d with its planner", e, got.Phase, got.Generation, got.Workers, adopted.Generation)
	}
	if again := r.inTree(e, r.tree); again.Generation != recorded.Generation || again.LingerUntil != nil {
		t.Fatalf("after its old linger's deadline the daemon records %s at generation %d, lingering %v; want generation %d, no linger", e, again.Generation, again.LingerUntil, recorded.Generation)
	}
	r.note("read back: %s is still in planning at generation %d with its planner's claim %s, in the tree of %s", e, got.Generation, got.Workers[claim.RolePlanner].Claim.State, r.tree)
	if closes := r.outboxRows(e, mark, "kind = 'supervise' and payload->>'op' = 'tree_close'"); closes != 0 {
		t.Fatalf("the outbox holds %d tree_close rows of %s queued after the release, want none", closes, e)
	}
	if removals := r.outboxRows(e, mark, "kind = 'workspace_remove'"); removals != 0 {
		t.Fatalf("the outbox holds %d workspace_remove rows of %s queued after the release, want none", removals, e)
	}
	r.note("read back: the outbox holds no tree_close supervise row and no workspace_remove row of %s queued after the release (rows past id %d)", e, mark)
	if stops := r.stopped(e); len(stops) != 0 {
		t.Fatalf("the fake runtime was asked %v of %s, want nothing stopped", stops, e)
	}
	r.note("read back: the fake runtime was asked to suspend, release or clean up nothing of %s", e)
}

// Step 5 (D2). P is a child of T on Dispatch alone, in backlog, never released and never recorded.
// H, a labelled todo issue under P, is recorded as a root of its own, but waits in no line: its
// nearest recorded ancestor's tree is live. X then leaves, freeing the cap's other slot, and H
// still takes no slot and no architect. T's architect releases H: it joins T and plans.
func (r *childAdoptionLive) heldUnderAnUnreleasedChild() {
	t := r.t
	p, h := r.key(7), r.key(8)
	r.board.show(p, "backlog", r.tree, false)
	r.note("the Dispatch stand-in shows %s backlog under %s; the daemon has no event of it", p, r.tree)
	r.publishIssue(h, "todo", p, true, false)
	r.note("drove %s labelled todo under %s: an issue.updated envelope", h, p)
	testwait.Eventually(t, "the daemon's record of "+h, func() bool { return r.recorded(h) != nil })
	if recorded := r.recorded(h); recorded.Tree != h {
		t.Fatalf("the daemon records %s in the tree of %s, want a root of its own until a release", h, recorded.Tree)
	}
	held := func(when string) {
		t.Helper()
		got := r.issue(h)
		if r.waiting(h) || got.Slot != nil || got.Architect != nil || r.launched(r.token(h, claim.RoleArchitect)) {
			t.Fatalf("%s, %s is waiting %v, slot %v, architect %v: want out of the line with no slot and no architect", when, h, r.waiting(h), got.Slot, got.Architect)
		}
	}
	held("recorded")
	r.note("read back: the daemon records %s as a root of its own, and it is not in admission.waiting %v, with no slot and no architect", h, r.state().Admission.Waiting)

	r.publishIssue(r.filler, "backlog", "", true, false)
	r.note("drove %s, the second slotted tree, to backlog: an issue.updated envelope", r.filler)
	r.waitFor(r.filler+"'s slot freed", r.filler, func(got api.Issue) bool { return got.Slot == nil })
	deadline := time.Now().Add(childAdoptionSettle)
	for time.Now().Before(deadline) {
		held("with a slot free")
		time.Sleep(100 * time.Millisecond)
	}
	state := r.state()
	if !slices.Equal(state.Admission.Active, []string{r.tree}) {
		t.Fatalf("admission.active is %v, want %s's slot alone", state.Admission.Active, r.tree)
	}
	r.note("read back over %s: admission.active %v of cap %d, admission.waiting %v, and %s holds no slot and no architect",
		childAdoptionSettle, state.Admission.Active, state.Admission.Cap, state.Admission.Waiting, h)

	status, failure := r.release(h)
	if status != http.StatusOK {
		t.Fatalf("release_children of %s = %d %s %s, want 200", h, status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/waves/release {%s} by %s's architect: %d", h, r.tree, status)
	recorded := r.inTree(h, r.tree)
	r.note("read back as the release answered: the daemon records %s in the tree of %s at generation %d, parent %s", h, recorded.Tree, recorded.Generation, *recorded.Parent)
	r.runsInTree(h)
	r.parks(h)
}

// Step 6 (AC4). U is a second live tree, holding the slot X freed, with G, a child parked in
// backlog. Dispatch shows G in backlog under T. T's architect's release of G is refused 409
// CHILD_IN_OTHER_TREE, naming G and U, and changes nothing: G's record is as it was and nothing is
// written to Dispatch.
func (r *childAdoptionLive) releaseRefuses() {
	t := r.t
	u, g := r.key(9), r.key(10)
	ctx := context.Background()
	r.board.show(u, "in_progress", "", true)
	r.write(fmt.Sprintf("%s, a second tree holding the free slot, and %s, its child parked in backlog", u, g), func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: u, Repo: childAdoptionRepo, DefaultBranch: "main", GitHubRepo: childAdoptionRepo, Tree: u, Project: r.cfg.Project,
			Title: u, Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "B",
			LastDispatchSeq: r.seq, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.putSlot(ctx, tx, u); err != nil {
			return err
		}
		return r.records.PutIssue(ctx, tx, record.Issue{
			Key: g, Repo: childAdoptionRepo, DefaultBranch: "main", GitHubRepo: childAdoptionRepo, Tree: u, Project: r.cfg.Project,
			Title: g, Parent: &u, Phase: phase.Done, Generation: 1, Status: "backlog", DispatchStatus: "backlog", Rank: "C",
			LastDispatchSeq: r.seq,
		})
	})
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, u, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", u, err)
	}
	before := r.inTree(g, u)
	if got := r.issue(u); got.Slot == nil {
		t.Fatalf("%s holds no slot", u)
	}
	r.board.show(g, "backlog", r.tree, false)
	r.note("the Dispatch stand-in shows %s backlog under %s", g, r.tree)
	writes := len(r.board.writes())
	status, failure := r.release(g)
	if status != http.StatusConflict || failure.Code != "CHILD_IN_OTHER_TREE" {
		t.Fatalf("release_children of %s = %d %s %s, want 409 CHILD_IN_OTHER_TREE", g, status, failure.Code, failure.Error)
	}
	for _, want := range []string{g, u} {
		if !strings.Contains(failure.Error, want) {
			t.Fatalf("the refusal %q does not name %s", failure.Error, want)
		}
	}
	r.note("drove POST /legion/v1/waves/release {%s} by %s's architect: %d %s — %s", g, r.tree, status, failure.Code, failure.Error)
	after := r.inTree(g, u)
	if after.Generation != before.Generation || after.Phase != before.Phase || after.Status != before.Status ||
		after.Parent == nil || *after.Parent != u || after.LastDispatchSeq != before.LastDispatchSeq {
		t.Fatalf("the refused release changed %s's record from %+v to %+v", g, before, after)
	}
	r.note("read back: the daemon records %s as before — the tree of %s, parent %s, phase %s, status %s, generation %d",
		g, after.Tree, *after.Parent, after.Phase, after.Status, after.Generation)
	if wrote := r.statusWrites(writes); len(wrote) != 0 {
		t.Fatalf("the refused release wrote %+v to Dispatch, want nothing", wrote)
	}
	r.note("read back: the Dispatch stand-in took no status write")
}

// publishIssue publishes Dispatch's issue.updated event of key at status under parent ("" for
// none), labelled for Legion when handed, as a person's move from the Dispatch dashboard makes one
// — or, when daemon, as the echo of the daemon's own write — on the next sequence. The event stands
// for a write already on Dispatch's board, so the board shows it first.
func (r *childAdoptionLive) publishIssue(key, status, parent string, handed, daemon bool) {
	r.t.Helper()
	r.board.show(key, status, parent, handed)
	r.seq++
	actor := map[string]string{"kind": "user", "id": "operator"}
	if daemon {
		actor = map[string]string{"kind": "session", "id": dispatch.DaemonSession(key)}
	}
	var parentKey any
	if parent != "" {
		parentKey = parent
	}
	payload, err := json.Marshal(map[string]any{
		"id": r.seq, "issue_key": key, "project": r.cfg.Project, "seq": r.seq, "type": "issue.updated",
		"actor": actor, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"key": key, "status": status, "title": key, "parent": parentKey, "rank": "M",
			"labels": childAdoptionLabels(handed), "last_seq": r.seq, "project": r.cfg.Project, "resolved_repo": childAdoptionRepo,
		},
	})
	if err != nil {
		r.t.Fatalf("encode the Dispatch event: %v", err)
	}
	subject := "notifications.dispatch.issue." + key + ".issue.updated"
	eventID := fmt.Sprintf("%s-%s-%s-%d", r.cfg.Project, key, status, r.seq)
	envelope, err := json.Marshal(map[string]any{
		"event_id": eventID, "source": "dispatch", "source_event_id": eventID, "topic": subject, "dedupe_key": eventID,
		"issued_at": time.Now().UnixMilli(), "payload_summary": subject, "payload": string(payload), "trace_id": eventID,
	})
	if err != nil {
		r.t.Fatalf("encode the %s envelope: %v", subject, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.js.Publish(ctx, subject, envelope); err != nil {
		r.t.Fatalf("publish %s: %v", subject, err)
	}
}

// childAdoptionLabels is an issue's labels: Legion's when it is handed to Legion, none otherwise.
func childAdoptionLabels(handed bool) []string {
	if handed {
		return []string{dispatch.LegionLabel}
	}
	return []string{}
}

// childAdoptionBoard is the Dispatch stand-in of this proof: it holds each issue's status, parent
// and labels as Dispatch's board does, answers the issue reads the release (its walk up the parent
// chain) and the event preparation (its walk to the nearest recorded ancestor) make, and records
// every status and message the daemon writes, so a step can read back what the daemon told Dispatch
// and tell a write that never came from one it cannot see. The issue list the boot reads is empty,
// as workflowConfig's stand-in answers it. Anything else is a call this proof does not expect, and
// fails the test.
type childAdoptionBoard struct {
	t       *testing.T
	project string
	mu      sync.Mutex
	issues  map[string]childAdoptionIssue
	posted  []childAdoptionWrite
}

// childAdoptionIssue is one issue as the board holds it.
type childAdoptionIssue struct {
	status, parent string
	handed         bool
}

// childAdoptionWrite is one write the daemon made to Dispatch: a status it set on an issue, or a
// message it posted on one.
type childAdoptionWrite struct {
	Issue  string
	Status string
	Body   string
}

func newChildAdoptionBoard(t *testing.T, project string) *childAdoptionBoard {
	return &childAdoptionBoard{t: t, project: project, issues: map[string]childAdoptionIssue{}}
}

// show puts key on the board at status under parent ("" for none), labelled for Legion when
// handed.
func (b *childAdoptionBoard) show(key, status, parent string, handed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.issues[key] = childAdoptionIssue{status: status, parent: parent, handed: handed}
}

func (b *childAdoptionBoard) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if servedMapping(b.t, w, req, b.project) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := req.URL.Path
	key := issueOfDispatchPath(path)
	switch {
	case req.Method == http.MethodGet && path == "/api/v1/issues":
		_, _ = io.WriteString(w, `[]`)
	case req.Method == http.MethodGet && path == "/healthz":
		_, _ = io.WriteString(w, `{"status":"ok","commit":null}`)
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/events"):
		_, _ = io.WriteString(w, `[]`)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/issues/") && key != "":
		b.showIssue(key, w)
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/messages"):
		b.record(childAdoptionWrite{Issue: key, Body: b.decode(req, w)}, w)
	case req.Method == http.MethodPatch && strings.HasPrefix(path, "/api/v1/issues/"):
		write := childAdoptionWrite{Issue: key, Status: b.decode(req, w)}
		b.mu.Lock()
		issue := b.issues[key]
		issue.status = write.Status
		b.issues[key] = issue
		b.mu.Unlock()
		b.record(write, w)
	default:
		b.t.Errorf("Dispatch request = %s %s", req.Method, path)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}
}

// showIssue answers Dispatch's issue detail of key from the board, and 404 for an issue it does
// not hold.
func (b *childAdoptionBoard) showIssue(key string, w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	issue, ok := b.issues[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"NOT_FOUND","error":"no such issue"}`)
		return
	}
	var parent any
	if issue.parent != "" {
		parent = issue.parent
	}
	if err := json.NewEncoder(w).Encode(map[string]any{
		"key": key, "project": b.project, "title": key, "status": issue.status, "parent": parent, "rank": "M",
		"labels": childAdoptionLabels(issue.handed), "last_seq": 1, "resolved_repo": childAdoptionRepo,
	}); err != nil {
		b.t.Errorf("write the Dispatch issue %s: %v", key, err)
	}
}

// decode is the write's status or body, whichever the daemon sent.
func (b *childAdoptionBoard) decode(req *http.Request, w http.ResponseWriter) string {
	var write struct {
		Status string `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.NewDecoder(req.Body).Decode(&write); err != nil {
		b.t.Errorf("decode the Dispatch write to %s: %v", req.URL.Path, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return ""
	}
	if write.Status != "" {
		return write.Status
	}
	return write.Body
}

func (b *childAdoptionBoard) record(write childAdoptionWrite, w http.ResponseWriter) {
	b.mu.Lock()
	b.posted = append(b.posted, write)
	b.mu.Unlock()
	_, _ = io.WriteString(w, `{}`)
}

// writes is every status the daemon has set and every message it has posted, oldest first.
func (b *childAdoptionBoard) writes() []childAdoptionWrite {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]childAdoptionWrite(nil), b.posted...)
}
