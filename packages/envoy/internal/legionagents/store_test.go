package legionagents_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/testnats"
)

func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }

// clock is a test's clock, which a test moves to tell a first write's recorded_at from a rewrite's.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// registry is one test's store over its own NATS account, with the bucket's raw handle beside it so
// a test can say what the bucket holds.
type registry struct {
	store *legionagents.Store
	raw   natsgo.KeyValue
	clock *clock
}

func openRegistry(t *testing.T) *registry {
	t.Helper()
	return openRegistryAt(t, testnats.URL(t))
}

func openRegistryAt(t *testing.T, uri string) *registry {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	c := &clock{now: time.Date(2026, 10, 26, 4, 0, 0, 0, time.UTC)}
	store, err := legionagents.Open(conn, legionagents.WithReplicas(1), legionagents.WithClock(c.Now))
	if err != nil {
		t.Fatalf("open the registry: %v", err)
	}
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	raw, err := js.KeyValue(bus.LegionAgentBucket)
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	return &registry{store: store, raw: raw, clock: c}
}

// contents is every key the bucket holds and its value.
func (r *registry) contents(t *testing.T) map[string]string {
	t.Helper()
	keys, err := r.raw.Keys()
	if errors.Is(err, natsgo.ErrNoKeysFound) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatalf("list the bucket's keys: %v", err)
	}
	held := map[string]string{}
	for _, key := range keys {
		entry, err := r.raw.Get(key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		held[key] = string(entry.Value())
	}
	return held
}

// agent is a valid entry of the session id on issue's role, under the Legion project token
// project, started at started.
func agent(project, issue, role, id, started string) legionagents.Entry {
	return legionagents.Entry{
		SessionID:     id,
		LegionProject: project,
		Issue:         issue,
		Role:          role,
		RoleToken:     "legion-" + project + "-" + strings.ToLower(issue) + "-" + role,
		Title:         "Legion " + role + " · " + issue,
		Harness:       "omp",
		StartedAt:     started,
	}
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func sessionIDs(agents []legionagents.Agent) []string {
	ids := make([]string, len(agents))
	for i, a := range agents {
		ids[i] = a.SessionID
	}
	return ids
}

func TestRoleTokenIsTheDaemonsToken(t *testing.T) {
	if got := legionagents.RoleToken("infra", "INFRA-448", "tester"); got != "legion-infra-infra-448-tester" {
		t.Fatalf("RoleToken = %q, want legion-infra-infra-448-tester", got)
	}
	if got := legionagents.RoleToken("sjawharlegion", "LEGION-7", "architect"); got != "legion-sjawharlegion-legion-7-architect" {
		t.Fatalf("RoleToken = %q, want legion-sjawharlegion-legion-7-architect", got)
	}
}

// One entry written is read back whole by each read: by its session, its role token and its issue,
// carrying the recorded_at the store stamped at the write and no replacement.
func TestAnEntryRoundTripsThroughEveryRead(t *testing.T) {
	r := openRegistry(t)
	want := agent("infra", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z")
	want.RecordedAt = "1999-01-01T00:00:00Z" // a writer's recorded_at is not the store's
	stored, err := r.store.Put(ctx(t), want)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	want.RecordedAt = "2026-10-26T04:00:00Z"
	if stored != want {
		t.Fatalf("stored = %#v, want %#v", stored, want)
	}
	got, err := r.store.Get(ctx(t), "ses-tester")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Entry != want || got.ReplacedBy != nil || got.ReplacedAt != nil {
		t.Fatalf("get = %#v, want %#v with no replacement", got, want)
	}
	byRole, err := r.store.ByRoleToken(ctx(t), "legion-infra-infra-448-tester")
	if err != nil || len(byRole) != 1 || byRole[0].Entry != want {
		t.Fatalf("by role token = %#v, %v; want the one entry", byRole, err)
	}
	byIssue, err := r.store.ByIssue(ctx(t), "INFRA-448")
	if err != nil || len(byIssue) != 1 || byIssue[0].Entry != want {
		t.Fatalf("by issue = %#v, %v; want the one entry", byIssue, err)
	}
	held := r.contents(t)
	value, _ := json.Marshal(want)
	for _, key := range []string{"session.ses-tester", "issue.INFRA-448.ses-tester", "role.legion-infra-infra-448-tester.ses-tester"} {
		if held[key] != string(value) {
			t.Fatalf("key %s holds %q, want %q", key, held[key], value)
		}
	}
	if len(held) != 3 {
		t.Fatalf("the bucket holds %v, want the three keys", held)
	}
}

func TestABatchRoundTripsInOrder(t *testing.T) {
	r := openRegistry(t)
	entries := []legionagents.Entry{
		agent("infra", "INFRA-448", "planner", "ses-planner", "2026-10-26T01:00:00Z"),
		agent("infra", "INFRA-448", "implementer", "ses-impl", "2026-10-26T02:00:00Z"),
		agent("infra", "INFRA-449", "tester", "ses-other", "2026-10-26T03:00:00Z"),
	}
	stored, written, err := r.store.PutAll(ctx(t), entries)
	if err != nil || written != 3 {
		t.Fatalf("put all = %d, %v; want 3 written", written, err)
	}
	for i := range entries {
		entries[i].RecordedAt = "2026-10-26T04:00:00Z"
		if stored[i] != entries[i] {
			t.Fatalf("stored[%d] = %#v, want %#v", i, stored[i], entries[i])
		}
		got, err := r.store.Get(ctx(t), entries[i].SessionID)
		if err != nil || got.Entry != entries[i] {
			t.Fatalf("get %s = %#v, %v", entries[i].SessionID, got, err)
		}
	}
	byIssue, err := r.store.ByIssue(ctx(t), "INFRA-448")
	if err != nil {
		t.Fatalf("by issue: %v", err)
	}
	if got := sessionIDs(byIssue); !slices.Equal(got, []string{"ses-planner", "ses-impl"}) {
		t.Fatalf("by issue = %v, want the planner then the implementer", got)
	}
}

// Writing the same session again rewrites all three keys with the new title, harness and start,
// and keeps the recorded_at of its first write.
func TestARewriteKeepsRecordedAtAndRewritesEveryKey(t *testing.T) {
	r := openRegistry(t)
	first := agent("infra", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z")
	if _, err := r.store.Put(ctx(t), first); err != nil {
		t.Fatalf("first put: %v", err)
	}
	before := r.contents(t)
	r.clock.advance(time.Hour)
	if stored, err := r.store.Put(ctx(t), first); err != nil || stored.RecordedAt != "2026-10-26T04:00:00Z" {
		t.Fatalf("identical rewrite = %#v, %v; want the first recorded_at", stored, err)
	}
	if after := r.contents(t); !mapsEqual(before, after) {
		t.Fatalf("an identical rewrite changed the bucket: %v, then %v", before, after)
	}
	next := first
	next.Title, next.Harness, next.StartedAt = "Legion tester · INFRA-448 (resumed)", "claude", "2026-10-26T03:20:00Z"
	stored, err := r.store.Put(ctx(t), next)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	next.RecordedAt = "2026-10-26T04:00:00Z"
	if stored != next {
		t.Fatalf("rewrite stored %#v, want %#v", stored, next)
	}
	value, _ := json.Marshal(next)
	for key, held := range r.contents(t) {
		if held != string(value) {
			t.Fatalf("after the rewrite %s holds %s, want %s", key, held, value)
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A session belongs to one issue and role for good: a write naming another Legion project, issue
// or role for a recorded session is refused, naming what it is recorded for, and writes nothing.
func TestARePutMovingASessionIsRefusedAndWritesNothing(t *testing.T) {
	r := openRegistry(t)
	if _, err := r.store.Put(ctx(t), agent("infra", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z")); err != nil {
		t.Fatalf("put: %v", err)
	}
	before := r.contents(t)
	for name, moved := range map[string]legionagents.Entry{
		"another issue":          agent("infra", "INFRA-449", "tester", "ses-tester", "2026-10-26T03:15:00Z"),
		"another role":           agent("infra", "INFRA-448", "reviewer", "ses-tester", "2026-10-26T03:15:00Z"),
		"another Legion project": agent("sjawharlegion", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z"),
	} {
		_, err := r.store.Put(ctx(t), moved)
		var conflict *legionagents.ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("%s: err = %v, want a conflict", name, err)
		}
		if got := err.Error(); got != "legion agent ses-tester is recorded for INFRA-448 tester" {
			t.Fatalf("%s: err = %q", name, got)
		}
		if after := r.contents(t); !mapsEqual(before, after) {
			t.Fatalf("%s: the refused write changed the bucket: %v, then %v", name, before, after)
		}
	}
}

// Each entry the registry does not store is refused as invalid, by either write, and nothing is
// written.
func TestEveryInvalidEntryIsRefusedByBothWrites(t *testing.T) {
	r := openRegistry(t)
	valid := func() legionagents.Entry {
		return agent("infra", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z")
	}
	for _, tc := range []struct {
		name  string
		edit  func(*legionagents.Entry)
		names string
	}{
		{"no session id", func(e *legionagents.Entry) { e.SessionID = "" }, "session_id"},
		{"a session id holding a dot", func(e *legionagents.Entry) { e.SessionID = "ses.tester" }, "session_id"},
		{"a session id outside the key alphabet", func(e *legionagents.Entry) { e.SessionID = "ses:tester" }, "session_id"},
		{"a session id holding a slash", func(e *legionagents.Entry) { e.SessionID = "ses/tester" }, "session_id"},
		{"a session id too long", func(e *legionagents.Entry) { e.SessionID = strings.Repeat("s", 129) }, "session_id"},
		{"a role outside the six", func(e *legionagents.Entry) {
			e.Role, e.RoleToken = "controller", "legion-infra-infra-448-controller"
		}, "role"},
		{"a legion_project that is not a project token", func(e *legionagents.Entry) {
			e.LegionProject, e.RoleToken = "Sjawhar/Legion", "legion-Sjawhar/Legion-infra-448-tester"
		}, "legion_project"},
		{"no legion_project", func(e *legionagents.Entry) { e.LegionProject = "" }, "legion_project"},
		{"a role_token of another Legion project", func(e *legionagents.Entry) {
			e.RoleToken = "legion-sjawharlegion-infra-448-tester"
		}, "role_token"},
		// The Dispatch project is INFRA and the Legion project token here is sjawharlegion: a token
		// built from the Dispatch project's segment is not the daemon's.
		{"a role_token built from the Dispatch project", func(e *legionagents.Entry) {
			e.LegionProject = "sjawharlegion"
		}, "role_token"},
		{"a role_token of another issue", func(e *legionagents.Entry) { e.RoleToken = "legion-infra-infra-449-tester" }, "role_token"},
		{"a role_token of another role", func(e *legionagents.Entry) { e.RoleToken = "legion-infra-infra-448-reviewer" }, "role_token"},
		{"a lower-case issue", func(e *legionagents.Entry) { e.Issue = "infra-448" }, "issue"},
		{"an issue that is not a key", func(e *legionagents.Entry) { e.Issue = "INFRA" }, "issue"},
		{"a harness outside the two", func(e *legionagents.Entry) { e.Harness = "codex" }, "harness"},
		{"a started_at with an offset", func(e *legionagents.Entry) { e.StartedAt = "2026-10-26T05:15:00+02:00" }, "started_at"},
		{"a started_at that is not RFC 3339", func(e *legionagents.Entry) { e.StartedAt = "2026-10-26 03:15:00" }, "started_at"},
		{"a title too long", func(e *legionagents.Entry) { e.Title = strings.Repeat("t", 1025) }, "title"},
	} {
		entry := valid()
		tc.edit(&entry)
		_, err := r.store.Put(ctx(t), entry)
		if !errors.Is(err, legionagents.ErrInvalid) || !strings.Contains(err.Error(), tc.names) {
			t.Fatalf("%s: put err = %v, want an invalid entry naming %s", tc.name, err, tc.names)
		}
		_, written, err := r.store.PutAll(ctx(t), []legionagents.Entry{valid(), entry})
		var batch *legionagents.BatchError
		if !errors.As(err, &batch) || batch.Index != 1 || !errors.Is(err, legionagents.ErrInvalid) || written != 0 {
			t.Fatalf("%s: put all = %d, %v; want entries[1] refused as invalid, nothing written", tc.name, written, err)
		}
		if held := r.contents(t); len(held) != 0 {
			t.Fatalf("%s: a refused write left %v", tc.name, held)
		}
	}
}

// A batch member that would move a recorded session refuses the batch before its first write, and
// so does one that moves a session an earlier member of the same batch records.
func TestABatchMemberMovingASessionRefusesTheWholeBatch(t *testing.T) {
	r := openRegistry(t)
	if _, err := r.store.Put(ctx(t), agent("infra", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z")); err != nil {
		t.Fatalf("put: %v", err)
	}
	before := r.contents(t)
	for name, batch := range map[string][]legionagents.Entry{
		"a recorded session": {
			agent("infra", "INFRA-448", "reviewer", "ses-reviewer", "2026-10-26T03:30:00Z"),
			agent("infra", "INFRA-449", "tester", "ses-tester", "2026-10-26T03:15:00Z"),
		},
		"a session earlier in the batch": {
			agent("infra", "INFRA-450", "planner", "ses-new", "2026-10-26T03:30:00Z"),
			agent("infra", "INFRA-450", "implementer", "ses-new", "2026-10-26T03:30:00Z"),
		},
	} {
		_, written, err := r.store.PutAll(ctx(t), batch)
		var refused *legionagents.BatchError
		var conflict *legionagents.ConflictError
		if !errors.As(err, &refused) || refused.Index != 1 || !errors.As(err, &conflict) || written != 0 {
			t.Fatalf("%s: put all = %d, %v; want entries[1] refused as a conflict, nothing written", name, written, err)
		}
		if after := r.contents(t); !mapsEqual(before, after) {
			t.Fatalf("%s: the refused batch changed the bucket: %v, then %v", name, before, after)
		}
	}
}

func TestABatchPastTheBoundIsRefusedWhole(t *testing.T) {
	r := openRegistry(t)
	batch := make([]legionagents.Entry, legionagents.MaxBatch+1)
	for i := range batch {
		batch[i] = agent("infra", "INFRA-448", "tester", "ses-"+strconv.Itoa(i), "2026-10-26T03:15:00Z")
	}
	if _, written, err := r.store.PutAll(ctx(t), batch); !errors.Is(err, legionagents.ErrTooMany) || written != 0 {
		t.Fatalf("put all = %d, %v; want ErrTooMany with nothing written", written, err)
	}
	if held := r.contents(t); len(held) != 0 {
		t.Fatalf("the refused batch left %v", held)
	}
}

// Within one role token the session that started next replaced the one before it, at its own
// start; the newest has no replacement. The derivation is made at each read, never stored.
func TestEachReadDerivesWhoReplacedASession(t *testing.T) {
	r := openRegistry(t)
	_, _, err := r.store.PutAll(ctx(t), []legionagents.Entry{
		agent("infra", "INFRA-448", "reviewer", "ses-new-reviewer", "2026-10-26T03:00:00Z"),
		agent("infra", "INFRA-448", "reviewer", "ses-old-reviewer", "2026-10-26T01:00:00Z"),
		agent("infra", "INFRA-448", "reviewer", "ses-mid-reviewer", "2026-10-26T02:00:00Z"),
		agent("infra", "INFRA-448", "implementer", "ses-impl", "2026-10-26T00:30:00Z"),
	})
	if err != nil {
		t.Fatalf("put all: %v", err)
	}
	byRole, err := r.store.ByRoleToken(ctx(t), "legion-infra-infra-448-reviewer")
	if err != nil {
		t.Fatalf("by role token: %v", err)
	}
	if got := sessionIDs(byRole); !slices.Equal(got, []string{"ses-new-reviewer", "ses-mid-reviewer", "ses-old-reviewer"}) {
		t.Fatalf("by role token = %v, want newest first", got)
	}
	replaced := func(a legionagents.Agent) (by, at string) {
		if (a.ReplacedBy == nil) != (a.ReplacedAt == nil) {
			t.Fatalf("%s: replaced_by %v and replaced_at %v disagree", a.SessionID, a.ReplacedBy, a.ReplacedAt)
		}
		if a.ReplacedBy == nil {
			return "", ""
		}
		return *a.ReplacedBy, *a.ReplacedAt
	}
	for _, tc := range []struct{ id, by, at string }{
		{"ses-new-reviewer", "", ""},
		{"ses-mid-reviewer", "ses-new-reviewer", "2026-10-26T03:00:00Z"},
		{"ses-old-reviewer", "ses-mid-reviewer", "2026-10-26T02:00:00Z"},
		{"ses-impl", "", ""},
	} {
		got, err := r.store.Get(ctx(t), tc.id)
		if err != nil {
			t.Fatalf("get %s: %v", tc.id, err)
		}
		if by, at := replaced(got); by != tc.by || at != tc.at {
			t.Fatalf("%s replaced by %q at %q, want %q at %q", tc.id, by, at, tc.by, tc.at)
		}
	}
	for key, value := range r.contents(t) {
		if strings.Contains(value, "replaced") {
			t.Fatalf("key %s stores a derived field: %s", key, value)
		}
	}
}

// Each listing reads only its own issue's or role token's keys, and an issue lists its agents by
// role in workflow order, then newest first.
func TestListingsFilterByIssueAndRoleToken(t *testing.T) {
	r := openRegistry(t)
	_, _, err := r.store.PutAll(ctx(t), []legionagents.Entry{
		agent("infra", "INFRA-448", "merger", "ses-merger", "2026-10-26T05:00:00Z"),
		agent("infra", "INFRA-448", "tester", "ses-tester-1", "2026-10-26T02:00:00Z"),
		agent("infra", "INFRA-448", "tester", "ses-tester-2", "2026-10-26T03:00:00Z"),
		agent("infra", "INFRA-448", "architect", "ses-architect", "2026-10-26T01:00:00Z"),
		agent("infra", "INFRA-4480", "tester", "ses-other-issue", "2026-10-26T04:00:00Z"),
		agent("infra", "INFRA-44", "tester", "ses-shorter-issue", "2026-10-26T04:00:00Z"),
	})
	if err != nil {
		t.Fatalf("put all: %v", err)
	}
	byIssue, err := r.store.ByIssue(ctx(t), "INFRA-448")
	if err != nil {
		t.Fatalf("by issue: %v", err)
	}
	if got := sessionIDs(byIssue); !slices.Equal(got, []string{"ses-architect", "ses-tester-2", "ses-tester-1", "ses-merger"}) {
		t.Fatalf("by issue = %v, want the architect, the testers newest first, the merger", got)
	}
	byRole, err := r.store.ByRoleToken(ctx(t), "legion-infra-infra-448-tester")
	if err != nil {
		t.Fatalf("by role token: %v", err)
	}
	if got := sessionIDs(byRole); !slices.Equal(got, []string{"ses-tester-2", "ses-tester-1"}) {
		t.Fatalf("by role token = %v, want the two testers of INFRA-448", got)
	}
	other, err := r.store.ByIssue(ctx(t), "INFRA-4480")
	if err != nil || !slices.Equal(sessionIDs(other), []string{"ses-other-issue"}) {
		t.Fatalf("by issue INFRA-4480 = %v, %v", sessionIDs(other), err)
	}
}

// A read of a session, issue or role token the registry holds nothing for is a real absence: the
// session is ErrNotFound, and a listing answers no agents.
func TestAnAbsenceIsAnsweredAsOne(t *testing.T) {
	r := openRegistry(t)
	if _, err := r.store.Put(ctx(t), agent("infra", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := r.store.Get(ctx(t), "ses-unknown"); !errors.Is(err, legionagents.ErrNotFound) || err.Error() != "unknown legion agent ses-unknown" {
		t.Fatalf("get an unknown session: %v, want ErrNotFound naming it", err)
	}
	if agents, err := r.store.ByIssue(ctx(t), "INFRA-1"); err != nil || len(agents) != 0 {
		t.Fatalf("by issue with none = %v, %v; want none", agents, err)
	}
	if agents, err := r.store.ByRoleToken(ctx(t), "legion-infra-infra-448-reviewer"); err != nil || len(agents) != 0 {
		t.Fatalf("by role token with none = %v, %v; want none", agents, err)
	}
}

// The project.<legion_project> key is the daemon's own record (INFRA-204), and no read reaches it,
// whatever it holds.
func TestAProjectKeyIsIgnoredByEveryRead(t *testing.T) {
	r := openRegistry(t)
	if _, err := r.store.Put(ctx(t), agent("infra", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z")); err != nil {
		t.Fatalf("put: %v", err)
	}
	for _, key := range []string{"project.infra", "project.INFRA-448"} {
		if _, err := r.raw.Put(key, []byte(`{"answering":true,"updated_at":"2026-10-26T04:00:00Z"}`)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	got, err := r.store.Get(ctx(t), "ses-tester")
	if err != nil || got.ReplacedBy != nil {
		t.Fatalf("get = %#v, %v", got, err)
	}
	if agents, err := r.store.ByIssue(ctx(t), "INFRA-448"); err != nil || !slices.Equal(sessionIDs(agents), []string{"ses-tester"}) {
		t.Fatalf("by issue = %v, %v; want the tester alone", sessionIDs(agents), err)
	}
	if agents, err := r.store.ByRoleToken(ctx(t), "legion-infra-infra-448-tester"); err != nil || !slices.Equal(sessionIDs(agents), []string{"ses-tester"}) {
		t.Fatalf("by role token = %v, %v; want the tester alone", sessionIDs(agents), err)
	}
}

// A read naming a session id, issue or role token the registry could never hold is refused as
// invalid before anything is sent.
func TestAReadOfAMalformedNameIsRefused(t *testing.T) {
	r := openRegistry(t)
	if _, err := r.store.Get(ctx(t), "ses.tester"); !errors.Is(err, legionagents.ErrInvalid) {
		t.Fatalf("get a session id with a dot: %v", err)
	}
	if _, err := r.store.ByIssue(ctx(t), "INFRA-448.>"); !errors.Is(err, legionagents.ErrInvalid) {
		t.Fatalf("by issue with a wildcard: %v", err)
	}
	if _, err := r.store.ByIssue(ctx(t), "infra-448"); !errors.Is(err, legionagents.ErrInvalid) {
		t.Fatalf("by a lower-case issue: %v", err)
	}
	if _, err := r.store.ByRoleToken(ctx(t), "legion-infra-controller"); !errors.Is(err, legionagents.ErrInvalid) {
		t.Fatalf("by the controller's token: %v", err)
	}
	if _, err := r.store.ByRoleToken(ctx(t), "legion-infra-infra-448-tester.*"); !errors.Is(err, legionagents.ErrInvalid) {
		t.Fatalf("by a token with a wildcard: %v", err)
	}
}

// The bucket is file-backed: what a store wrote is there for the store a restarted server's next
// connection opens.
func TestEntriesSurviveANATSRestart(t *testing.T) {
	server, uri := testnats.Start(t)
	r := openRegistryAt(t, uri)
	want, err := r.store.Put(ctx(t), agent("infra", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	server.Stop(t)
	server.Start(t)
	reopened := openRegistryAt(t, uri)
	got, err := reopened.store.Get(ctx(t), "ses-tester")
	if err != nil || got.Entry != want {
		t.Fatalf("get after the restart = %#v, %v; want %#v", got, err, want)
	}
}

// Reopen moves the store to a new connection, as the listener's reconnect hook does when the bus
// replaces a closed one.
func TestReopenMovesTheStoreToANewConnection(t *testing.T) {
	uri := testnats.URL(t)
	first := testnats.Connect(t, uri)
	store, err := legionagents.Open(first, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.Put(ctx(t), agent("infra", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z")); err != nil {
		t.Fatalf("put: %v", err)
	}
	first.Close()
	if _, err := store.Get(ctx(t), "ses-tester"); err == nil {
		t.Fatal("get on a closed connection answered")
	}
	second := testnats.Connect(t, uri)
	t.Cleanup(second.Close)
	if err := store.Reopen(second); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, err := store.Get(ctx(t), "ses-tester"); err != nil || got.SessionID != "ses-tester" {
		t.Fatalf("get after reopen = %#v, %v", got, err)
	}
}
