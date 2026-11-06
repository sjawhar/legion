package agenthistory

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// legionProject is the Legion project token of a `legion.yaml` whose project is sjawhar/legion,
// whose issues are the Dispatch project LEGION's: a token built from the Dispatch project would
// read legion-legion-..., and fail every assertion below.
const legionProject = "sjawharlegion"

var (
	headerTime  = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	backfillNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
)

func token(t *testing.T, issue string, role claim.Role) claim.Token {
	t.Helper()
	tok, err := claim.NewToken(legionProject, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// logs is a logger's output, safe to read while the logger writes.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// lines is every line holding each of fragments.
func (l *logs) lines(fragments ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		matched := line != ""
		for _, fragment := range fragments {
			matched = matched && strings.Contains(line, fragment)
		}
		if matched {
			found = append(found, line)
		}
	}
	return found
}

func (l *logs) logger() *slog.Logger { return slog.New(slog.NewTextHandler(l, nil)) }

func migratedStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := testpg.Database(t, "legion_agenthistory_test")
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st, dsn
}

// ompSession is an Oh My Pi session's content: a header titled title (none for ""), a subagent's
// when parent is set, and an envoy-role-claim of claimed (none for "").
func ompSession(id, title, parent, claimed string) string {
	header := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":%q,"cwd":"/work"`, id, headerTime.Format("2006-01-02T15:04:05.000Z"))
	if title != "" {
		header += fmt.Sprintf(`,"title":%q,"titleSource":"auto"`, title)
	}
	if parent != "" {
		header += fmt.Sprintf(`,"parentSession":%q`, parent)
	}
	content := header + "}\n" + `{"type":"message","id":"m1","parentId":null,"message":{"role":"user","content":"Go."}}` + "\n"
	if claimed != "" {
		content += fmt.Sprintf(`{"type":"custom","customType":"envoy-role-claim","data":{"role":%q},"id":"c1","parentId":"m1"}`, claimed) + "\n"
	}
	return content
}

// ompCase is one session of the backfill's fixture, with what the backfill must do with it: record
// it (skip "") or skip it with a line holding skip.
type ompCase struct {
	name, id, title, parent, claimed string
	role                             claim.Role
	issue                            string
	skip                             string
}

func ompCases(t *testing.T) []ompCase {
	tester := string(token(t, "LEGION-7", claim.RoleTester))
	return []ompCase{
		{name: "tester", id: "ses-tester", title: "Legion tester · LEGION-7", claimed: tester, role: claim.RoleTester, issue: "LEGION-7"},
		{name: "root-architect", id: "ses-architect", title: "Legion architect · LEGION-1", claimed: string(token(t, "LEGION-1", claim.RoleArchitect)),
			role: claim.RoleArchitect, issue: "LEGION-1"},
		{name: "controller", id: "ses-controller", title: "Legion controller · SJAWHARLEGION", claimed: "legion-sjawharlegion-controller",
			skip: "it is a controller's session"},
		{name: "subagent", id: "ses-subagent", title: "Legion tester · LEGION-7", parent: "/sessions/ses-tester.jsonl", claimed: tester,
			skip: "it is a subagent's session"},
		{name: "mismatched", id: "ses-mismatched", title: "Legion tester · LEGION-7", claimed: string(token(t, "LEGION-7", claim.RoleReviewer)),
			skip: "its envoy-role-claim names " + string(token(t, "LEGION-7", claim.RoleReviewer))},
		{name: "unclaimed", id: "ses-unclaimed", title: "Legion tester · LEGION-7", skip: "its envoy-role-claim names no role"},
		{name: "untitled", id: "ses-untitled", claimed: tester, skip: "it has no title"},
		{name: "foreign", id: "ses-foreign", title: "Fix the flaky test", claimed: tester, skip: "names no Legion role and issue"},
		{name: "registered", id: "ses-registered", title: "Legion tester · LEGION-7", claimed: tester, role: claim.RoleTester, issue: "LEGION-7"},
	}
}

// registerFirst records ses-registered as a registration, as a claim's registration before the
// backfill would: the backfill must leave it as the registration wrote it.
func registerFirst(t *testing.T, st *store.Store) store.AgentConversation {
	t.Helper()
	registeredAt := backfillNow.Add(-time.Hour)
	row, err := st.RegisterConversation(context.Background(), store.AgentConversation{
		Session: "ses-registered", Project: legionProject, Issue: "LEGION-7", Role: claim.RoleTester, Token: token(t, "LEGION-7", claim.RoleTester),
		Harness: runtime.BackendOmp, Location: "/registered/where.jsonl", StartedAt: registeredAt, RecordedAt: registeredAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// checkOMPBackfill holds the backfill's outcome to the cases: each recorded case read back as a
// scanned conversation at its path, started at its header's time; each skipped case absent with
// one log line naming its path and why; the registered one as its registration wrote it.
func checkOMPBackfill(t *testing.T, st *store.Store, log *logs, cases []ompCase, pathOf func(ompCase) string, registered store.AgentConversation) {
	t.Helper()
	ctx := context.Background()
	for _, c := range cases {
		got, found, err := st.AgentConversation(ctx, c.id)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case c.id == "ses-registered":
			if !found || got.Source != store.ConversationRegistered || got.Location != registered.Location || !got.StartedAt.Equal(registered.StartedAt) {
				t.Errorf("%s: %+v, want the registration's row %+v untouched", c.name, got, registered)
			}
		case c.skip == "":
			want := store.AgentConversation{Session: c.id, Project: legionProject, Issue: c.issue, Role: c.role, Token: token(t, c.issue, c.role),
				Harness: runtime.BackendOmp, Location: pathOf(c), Source: store.ConversationScanned}
			gotTimes := got
			got.StartedAt, got.RecordedAt = time.Time{}, time.Time{}
			if !found || got != want || !gotTimes.StartedAt.Equal(headerTime) || !gotTimes.RecordedAt.Equal(backfillNow) {
				t.Errorf("%s: %+v (found %t), want %+v started at the header's %v", c.name, gotTimes, found, want, headerTime)
			}
		default:
			if found {
				t.Errorf("%s: recorded %+v, want it skipped", c.name, got)
			}
			if lines := log.lines("path="+pathOf(c), c.skip); len(lines) != 1 {
				t.Errorf("%s: log lines naming %s and %q = %d, want one:\n%s", c.name, pathOf(c), c.skip, len(lines), strings.Join(log.lines(), "\n"))
			}
		}
	}
}

// Under tmux files, every titled Oh My Pi session under the sessions directory whose own role claim
// names its title's token is recorded as a scanned conversation started at its header's time; a
// controller's, a subagent's, a mismatched or missing claim, an untitled and a foreign session are
// each skipped with one log line naming the file and why; a session a registration recorded is left
// as the registration wrote it.
func TestBackfillRecordsTitledOMPSessionFilesAndSkipsTheRest(t *testing.T) {
	st, _ := migratedStore(t)
	registered := registerFirst(t, st)
	dir := filepath.Join(t.TempDir(), "sessions")
	cases := ompCases(t)
	pathOf := func(c ompCase) string {
		if c.parent != "" {
			return filepath.Join(dir, "--work--", "parent", c.id+".jsonl")
		}
		return filepath.Join(dir, "--work--", c.id+".jsonl")
	}
	for _, c := range cases {
		if err := os.MkdirAll(filepath.Dir(pathOf(c)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pathOf(c), []byte(ompSession(c.id, c.title, c.parent, c.claimed)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	log := &logs{}
	b := Backfill{Project: legionProject, Store: st, Now: func() time.Time { return backfillNow }, Log: log.logger()}
	if err := b.Files(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	checkOMPBackfill(t, st, log, cases, pathOf, registered)

	if err := b.Files(context.Background(), filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("Files of a directory that does not exist = %v, want nothing to do", err)
	}
}

// Under a database store, the same holds of Oh My Pi's session table: a row's title is its title
// column's where Oh My Pi set one, else its content's, and its role claim is its content's.
func TestBackfillRecordsTitledOMPSessionsOfTheTableAndSkipsTheRest(t *testing.T) {
	st, dsn := migratedStore(t)
	registered := registerFirst(t, st)
	ctx := context.Background()
	conn, err := ompsessions.ConnectURL(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	cases := ompCases(t)
	pathOf := func(c ompCase) string { return "/home/legion/.omp/agent/sessions/--work--/" + c.id + ".jsonl" }
	for _, c := range cases {
		title := c.title
		if c.name == "tester" {
			// Oh My Pi under SQL storage keeps its title in the column, not the content.
			title = ""
		}
		if _, err := ompsessions.Import(ctx, conn, pathOf(c), []byte(ompSession(c.id, title, c.parent, c.claimed)), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, "UPDATE omp_session_files SET title = $1, title_source = 'auto' WHERE path = $2", "Legion tester · LEGION-7", pathOf(cases[0])); err != nil {
		t.Fatal(err)
	}
	log := &logs{}
	b := Backfill{Project: legionProject, Store: st, Now: func() time.Time { return backfillNow }, Log: log.logger()}
	if err := b.Table(ctx, conn); err != nil {
		t.Fatal(err)
	}
	checkOMPBackfill(t, st, log, cases, pathOf, registered)
}

// claudeTranscript is a Claude Code transcript of session, renamed to title ("" for none).
func claudeTranscript(session, title string) string {
	content := fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"Go."},"uuid":"00000000-0000-4000-8000-000000000001","timestamp":%q,"cwd":"/work","sessionId":%q,"version":"2.1.310"}`,
		headerTime.Format("2006-01-02T15:04:05.000Z"), session) + "\n"
	if title != "" {
		content += fmt.Sprintf(`{"type":"custom-title","customTitle":%q,"sessionId":%q}`, title, session) + "\n"
	}
	return content
}

// A Claude Code transcript under the claim directory its title names is recorded as a scanned
// Claude Code conversation started at its first line's time; one under another claim's directory,
// and an untitled one, are each skipped with one log line naming the file and why.
func TestBackfillRecordsTitledClaudeTranscriptsUnderTheirOwnClaimDirectory(t *testing.T) {
	st, _ := migratedStore(t)
	root := filepath.Join(t.TempDir(), "claude")
	implementer := token(t, "LEGION-7", claim.RoleImplementer)
	tester := token(t, "LEGION-7", claim.RoleTester)
	transcript := func(dir claim.Token, session, title string) string {
		path := filepath.Join(root, string(dir), "projects", "-work", session+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(claudeTranscript(session, title)), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	recorded := transcript(implementer, "11111111-1111-4111-8111-111111111111", "Legion implementer · LEGION-7")
	elsewhere := transcript(tester, "22222222-2222-4222-8222-222222222222", "Legion implementer · LEGION-7")
	untitled := transcript(implementer, "33333333-3333-4333-8333-333333333333", "")

	log := &logs{}
	b := Backfill{Project: legionProject, Store: st, Now: func() time.Time { return backfillNow }, Log: log.logger()}
	if err := b.Claude(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, found, err := st.AgentConversation(ctx, "11111111-1111-4111-8111-111111111111")
	if err != nil || !found || got.Harness != runtime.BackendClaude || got.Token != implementer || got.Issue != "LEGION-7" || got.Role != claim.RoleImplementer ||
		got.Location != recorded || got.Source != store.ConversationScanned || !got.StartedAt.Equal(headerTime) || got.Project != legionProject {
		t.Errorf("the titled transcript = %+v, %t, %v; want it recorded as the implementer's Claude Code conversation", got, found, err)
	}
	for path, why := range map[string]string{elsewhere: "it lies under the claim directory " + string(tester), untitled: "it has no title"} {
		session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if _, found, _ := st.AgentConversation(ctx, session); found {
			t.Errorf("%s recorded, want it skipped", path)
		}
		if lines := log.lines("path="+path, why); len(lines) != 1 {
			t.Errorf("log lines naming %s and %q = %d, want one:\n%s", path, why, len(lines), strings.Join(log.lines(), "\n"))
		}
	}
}
