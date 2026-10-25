package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// INFRA-225's proof that the daemon itself, and not only its handlers under a unit test, passes a
// blocked issue over: the real Dispatch server with its events on, a real Postgres, a real NATS,
// and a real daemon process-in-process reading Dispatch's events off that bus. Everything the
// test does to Dispatch it does through Dispatch's own API — the issues, their `blocked_by`
// links, and every close — so every decision below is reached from an event the daemon consumed,
// never from a fact or a row the test wrote.
//
// It drives the three things the feature promises, in one run over one tree of issues, because
// each step is the state the next one starts from:
//
//  1. a blocked root is passed over while a slot stands free, and the roots nothing blocks take
//     the slots instead;
//  2. that root is admitted when its blocker closes — and, as the negative control the step
//     carries, not when the blocker is merely moved to backlog, which stops an issue without
//     finishing it;
//  3. a child released through release_children is held, the answer naming the issue it waits on,
//     and starts when that issue closes.
func TestBlockedIssuesAreNotStartedAgainstScratchDispatch(t *testing.T) {
	// The NATS server is taken first: testnats.URL empties it of every stream, and Dispatch's own
	// start reconciles ENVOY_NOTIFICATIONS on it, which the daemon then consumes from.
	natsURL := testnats.URL(t)
	baseURL, token := startScratchDispatchWithNATS(t, natsURL)

	cfg := workflowConfig(t, natsURL)
	// Dispatch takes a project key of at most ten characters; workflowConfig's is longer.
	cfg.Project = "B" + randomSuffix(t)[:9]
	cfg.Projects = map[string]config.Project{cfg.Project: {}}
	cfg.PostgresDSN = blockersDatabase(t)
	// Three slots for three roots, so the slot the blocked root does not take stands visibly free.
	cfg.AdmissionCap = 3
	// With the design gate off, the architect's own registration opens its tree, which is what
	// makes starting a released child possible at all.
	cfg.Gates.Design = config.DesignGateOff
	cfg.DispatchURL = baseURL
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the scratch Dispatch bearer: %v", err)
	}
	dispatchCall(t, baseURL, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": cfg.Project, "name": "Blocked issues"}, nil)
	dispatchCall(t, baseURL, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": cfg.Project, "default": true}, nil)

	rt := fake.NewRuntime()
	// A pods runtime provisions each claim's workspace itself, so the daemon clones nothing: these
	// issues name a repository that exists only in this test's GitHub stand-in.
	rt.InPod = true
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = blockersGitHub(t)
	d := startDaemon(t, cfg, o)

	// The blocker is itself a root Legion works, which is what a blocked_by link between two
	// Legion issues looks like. Dispatch ranks a new issue after the last, so the waiting line is
	// the order they are created in: the blocked root stands ahead of the root admitted in its
	// place.
	blocker := createBlockersIssue(t, baseURL, cfg.Project, "The blocker", "", nil)
	blocked := createBlockersIssue(t, baseURL, cfg.Project, "The blocked root", "", []string{blocker})
	free := createBlockersIssue(t, baseURL, cfg.Project, "The root behind it", "", nil)
	for _, key := range []string{blocker, blocked, free} {
		setDispatchStatus(t, baseURL, key, "todo")
	}

	// 1. The blocked root is passed over; the root behind it takes a slot, and one slot is left.
	state, body := settled(t, d, "admit the roots nothing blocks", func(s api.State) bool {
		return slices.Contains(s.Admission.Active, blocker) && slices.Contains(s.Admission.Active, free)
	})
	t.Logf("a blocked root passed over while a slot stood free:\n%s", body)
	if slices.Contains(state.Admission.Active, blocked) {
		t.Fatalf("Legion admitted %s although %s, the issue it waits on, is not done; active = %v\nstate %s",
			blocked, blocker, state.Admission.Active, body)
	}
	if !slices.Contains(state.Admission.Waiting, blocked) {
		t.Fatalf("Legion dropped %s out of the waiting line %v; a blocked root keeps its place\nstate %s",
			blocked, state.Admission.Waiting, body)
	}
	if waits, ok := blockedOn(state, blocked); !ok || !slices.Equal(waits, []string{blocker}) {
		t.Fatalf("the state's admission.blocked is %v; want %s named as waiting on %s\nstate %s",
			state.Admission.Blocked, blocked, blocker, body)
	}
	if len(state.Admission.Active) >= state.Admission.Cap {
		t.Fatalf("Legion took all %d slots with %v, so no slot stood free while %s was passed over\nstate %s",
			state.Admission.Cap, state.Admission.Active, blocked, body)
	}

	// 2a. The negative control: a blocker moved to backlog has stopped, not finished. Admission
	// demonstrably ran on that very event — it released the blocker's own slot — and still left
	// the blocked root where it stood.
	awaitDispatchStatus(t, baseURL, blocker, "in_progress")
	setDispatchStatus(t, baseURL, blocker, "backlog")
	state, body = settled(t, d, "release the slot of the blocker moved to backlog", func(s api.State) bool {
		return !slices.Contains(s.Admission.Active, blocker)
	})
	t.Logf("the blocker moved to backlog is not finished, so the root is still passed over:\n%s", body)
	if slices.Contains(state.Admission.Active, blocked) {
		t.Fatalf("Legion admitted %s when %s went to backlog; only done finishes a blocker\nstate %s", blocked, blocker, body)
	}
	if waits, ok := blockedOn(state, blocked); !ok || !slices.Equal(waits, []string{blocker}) {
		t.Fatalf("the state's admission.blocked is %v after %s went to backlog; want %s still waiting on it\nstate %s",
			state.Admission.Blocked, blocker, blocked, body)
	}

	// 2b. The close is a real Dispatch write, and the daemon admits the root off its event.
	setDispatchStatus(t, baseURL, blocker, "done")
	state, body = settled(t, d, "admit the blocked root once its blocker closed", func(s api.State) bool {
		return slices.Contains(s.Admission.Active, blocked)
	})
	t.Logf("the blocker closed through Dispatch, so the root is admitted:\n%s", body)
	if waits, ok := blockedOn(state, blocked); ok {
		t.Fatalf("%s is admitted and the state still lists it blocked by %v\nstate %s", blocked, waits, body)
	}

	// 3. The architect of the root behind the blocked one releases a child that waits on an issue
	// Dispatch has not finished. The grant is the real one its launched claim mints.
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatalf("the project token of %s: %v", cfg.Project, err)
	}
	architect, err := claim.NewToken(project, free, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("the architect claim token of %s: %v", free, err)
	}
	testwait.Eventually(t, "the daemon to launch the architect of "+free, func() bool {
		for _, call := range rt.Calls() {
			if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == architect {
				return true
			}
		}
		return false
	})
	grant := architectGrants(t, d, lastLaunch(t, rt, architect), free)
	// The tree's design gate is what lets any child of it start; with the policy off, registering
	// the root's own document opens it.
	if status, answer := d.request(http.MethodPost, "/legion/v1/gates/register", api.GateRegisterRequest{
		GrantID: grant(), Issue: free, ArtifactID: dispatchPrimaryArtifact(t, baseURL, free), Version: 1,
	}, false); status != http.StatusOK {
		t.Fatalf("the architect's design-gate registration on %s = %d; body %s", free, status, answer)
	}

	child := createBlockersIssue(t, baseURL, cfg.Project, "The held child", free, []string{blocked})
	setDispatchStatus(t, baseURL, child, "todo")
	_, body = settled(t, d, "record the child "+child+" under "+free, func(s api.State) bool {
		recorded, ok := s.Issues[child]
		return ok && recorded.Phase == phase.Admitted
	})
	t.Logf("the child is recorded and held at %s before the release:\n%s", phase.Admitted, body)

	status, answer := d.request(http.MethodPost, "/legion/v1/waves/release",
		api.WaveReleaseRequest{GrantID: grant(), Issues: []string{child}}, false)
	if status != http.StatusOK {
		t.Fatalf("release_children of %s = %d; body %s", child, status, answer)
	}
	var released api.WaveReleaseResponse
	if err := json.Unmarshal(answer, &released); err != nil {
		t.Fatalf("decode the release of %s from %s: %v", child, answer, err)
	}
	want := api.WaveReleaseResponse{
		Released: []string{child}, Started: []string{},
		Held: []api.HeldChild{{Issue: child, BlockedBy: []string{blocked}}},
	}
	if !reflect.DeepEqual(released, want) {
		state, body = settled(t, d, "answer its own state", func(api.State) bool { return true })
		t.Fatalf("release_children answered %s; want %s held on %s and nothing started. The daemon's own state names that "+
			"same wait at the same moment, admission.blocked = %v\nstate %s", answer, child, blocked, state.Admission.Blocked, body)
	}

	awaitDispatchStatus(t, baseURL, blocked, "in_progress")
	setDispatchStatus(t, baseURL, blocked, "done")
	state, body = settled(t, d, "start the held child once its blocker closed", func(s api.State) bool {
		recorded, ok := s.Issues[child]
		return ok && recorded.Phase != phase.Admitted
	})
	t.Logf("the released child started once its blocker closed:\n%s", body)
	if got := state.Issues[child].Phase; got != phase.Planning {
		t.Fatalf("the child %s left %s into %s; want its first phase, %s\nstate %s", child, phase.Admitted, got, phase.Planning, body)
	}
}

// settled polls the daemon's state until want holds and returns it with the JSON it was read
// from, so every assertion over it quotes the very answer it judged. The daemon is event-driven:
// nothing here waits a fixed span, and a condition that never holds fails naming the state the
// last poll read — what Legion was doing instead.
func settled(t *testing.T, d *daemon, what string, want func(api.State) bool) (api.State, string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-20*time.Second).Before(deadline) {
		deadline = testDeadline.Add(-20 * time.Second)
	}
	for {
		status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
		if status != http.StatusOK {
			t.Fatalf("GET the state = %d; body %s", status, body)
		}
		var state api.State
		if err := json.Unmarshal(body, &state); err != nil {
			t.Fatalf("decode the state %s: %v", body, err)
		}
		if want(state) {
			return state, string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Legion did not %s; its state is %s", what, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// blockedOn is what the state says issue waits on, and whether it names it at all.
func blockedOn(state api.State, issue string) ([]string, bool) {
	for _, row := range state.Admission.Blocked {
		if row.Issue == issue {
			return row.BlockedBy, true
		}
	}
	return nil, false
}

// architectGrants registers the session of an architect launch with the daemon and answers a
// fresh grant on every call, as the architect's plugin does: one grant authorises one command.
func architectGrants(t *testing.T, d *daemon, launch runtime.SpawnSpec, tree string) func() string {
	t.Helper()
	session := "ses_" + string(launch.Claim)
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent", PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register the architect of %s = %d; body %s", tree, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the architect's registration %s: %v", body, err)
	}
	return func() string {
		t.Helper()
		status, body := d.request(http.MethodPost, "/legion/v1/grants", api.GrantRequest{
			SessionID: session, Secret: registered.Secret, Tree: tree, Issue: tree,
		}, false)
		if status != http.StatusOK {
			t.Fatalf("the architect's grant on %s = %d; body %s", tree, status, body)
		}
		var minted api.GrantResponse
		if err := json.Unmarshal(body, &minted); err != nil {
			t.Fatalf("decode the architect's grant %s: %v", body, err)
		}
		return minted.GrantID
	}
}

// blockersDatabase is a Postgres database of this daemon's own, dropped when the test ends. The
// daemon migrates it itself at boot. It matters because of one table: processed_events, keyed by
// (source, event_id) alone. The scratch Dispatch numbers its own events from one in a database of
// its own, so every run publishes `dispatch-1` again, and a daemon sharing the test server's
// database with every other test would take this run's events for ones already processed and skip
// them. The admission slot index, unique across the whole database, is a second reason.
func blockersDatabase(t *testing.T) string {
	t.Helper()
	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse the test DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("connect the test admin database: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "legion_blockers_test_" + outboxSuffix(t)
	if _, err := admin.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	daemonURL := *base
	daemonURL.Path = "/" + name
	return daemonURL.String()
}

// blockersGitHub is GitHub's REST API as this daemon's boot and its outbox call it: a repository's
// default branch, the ref of that branch, and the create of an issue branch, for any repository.
// The issue-branch row admission queues runs before the tree's architect starts, so a daemon whose
// GitHub answers nothing would never launch the architect whose grant this test needs. Any other
// request is a 404, logged rather than failed: nothing this test asserts on reads GitHub.
func blockersGitHub(t *testing.T) string {
	t.Helper()
	const head = "c0ffee0123456789abcdef0123456789abcdef01"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
		parts := strings.SplitN(rest, "/", 3)
		if !isRepository || len(parts) < 2 {
			t.Logf("the GitHub stand-in answered 404 to %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		owner, name := parts[0], parts[1]
		tail := ""
		if len(parts) == 3 {
			tail = parts[2]
		}
		switch {
		case r.Method == http.MethodGet && tail == "":
			_, _ = io.WriteString(w, `{"full_name":"`+owner+"/"+name+`","default_branch":"main"}`)
		case r.Method == http.MethodGet && tail == "git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"`+head+`","type":"commit"}}`)
		case r.Method == http.MethodPost && tail == "git/refs":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"ref":"refs/heads/legion","object":{"sha":"`+head+`"}}`)
		default:
			t.Logf("the GitHub stand-in answered 404 to %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// scratchDispatchClient is the test's own client of the scratch Dispatch server; the daemon has
// its own.
var scratchDispatchClient = &http.Client{Timeout: 10 * time.Second}

// dispatchCall sends one request to the scratch Dispatch server as the person `smoke` — the
// trusted identity header the harness starts the server with — and decodes its answer into `into`
// when that is not nil.
func dispatchCall(t *testing.T, baseURL, method, path string, body, into any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "smoke")
	response, err := scratchDispatchClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", method, path, err)
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		t.Fatalf("%s %s = %s: %s", method, path, response.Status, answer)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode the answer to %s %s from %s: %v", method, path, answer, err)
		}
	}
}

// createBlockersIssue creates one issue of project through Dispatch's own API, carrying the legion
// label that hands it to Legion, optionally under parent, and waiting on waits. It is created in
// triage, where every Dispatch issue starts; the test moves it on with its own write.
func createBlockersIssue(t *testing.T, baseURL, project, title, parent string, waits []string) string {
	t.Helper()
	body := map[string]any{"project": project, "title": title, "labels": []string{"legion"}}
	if parent != "" {
		body["parent"] = parent
	}
	if len(waits) > 0 {
		body["blocked_by"] = waits
	}
	var created struct {
		Key string `json:"key"`
	}
	dispatchCall(t, baseURL, http.MethodPost, "/api/v1/issues", body, &created)
	if created.Key == "" {
		t.Fatalf("Dispatch created %q without a key", title)
	}
	return created.Key
}

// setDispatchStatus moves an issue through Dispatch, as a person at the dashboard does. The event
// Dispatch makes of it is the daemon's only news of the move.
func setDispatchStatus(t *testing.T, baseURL, key, status string) {
	t.Helper()
	dispatchCall(t, baseURL, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"status": status}, nil)
}

// awaitDispatchStatus waits for Dispatch itself to show key at status. The test writes a status of
// its own only after the daemon's own write for that issue has landed, so the two never race for
// the last word on it.
func awaitDispatchStatus(t *testing.T, baseURL, key, status string) {
	t.Helper()
	var last string
	testwait.Eventually(t, "Dispatch to show "+key+" "+status, func() bool {
		var read struct {
			Status string `json:"status"`
		}
		dispatchCall(t, baseURL, http.MethodGet, "/api/v1/issues/"+key, nil, &read)
		last = read.Status
		return read.Status == status
	})
	if last != status {
		t.Fatalf("Dispatch shows %s %s, want %s", key, last, status)
	}
}

// dispatchPrimaryArtifact is the document Dispatch created with an issue, which is the artifact
// the architect registers as its tree's design.
func dispatchPrimaryArtifact(t *testing.T, baseURL, key string) string {
	t.Helper()
	var read struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	dispatchCall(t, baseURL, http.MethodGet, "/api/v1/issues/"+key, nil, &read)
	if read.PrimaryArtifactID == "" {
		t.Fatalf("Dispatch shows no primary document for %s", key)
	}
	return read.PrimaryArtifactID
}
