package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// agentSessions is the sessions directory an agent's Oh My Pi writes under in a pod, the prefix of
// every session file a claim records.
const agentSessions = "/home/legion/.omp/profiles/legion/agent/sessions"

// sessionsImportRig is a session database, a tree volume holding session files, and the claims
// list `legion claims list --json` prints for them.
type sessionsImportRig struct {
	t                       *testing.T
	dsnFile, volume, claims string
}

func newSessionsImportRig(t *testing.T, claims []map[string]string) *sessionsImportRig {
	t.Helper()
	_, dsnFile := testpg.DSNFile(t, "legion_sessions_import_test")
	r := &sessionsImportRig{t: t, dsnFile: dsnFile, volume: t.TempDir(), claims: filepath.Join(t.TempDir(), "claims.json")}
	body, err := json.Marshal(map[string]any{"claims": claims})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.claims, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

// write puts content at the recorded session file's place on the tree volume.
func (r *sessionsImportRig) write(sessionFile, content string) {
	r.t.Helper()
	onVolume := filepath.Join(r.volume, "sessions", strings.TrimPrefix(sessionFile, agentSessions+"/"))
	if err := os.MkdirAll(filepath.Dir(onVolume), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(onVolume, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sessionsImportRig) run(extra ...string) (int, string, string) {
	r.t.Helper()
	var out, errb bytes.Buffer
	args := append([]string{"legion", "sessions", "import", "--dsn-file", r.dsnFile, "--claims", r.claims, "--tree-volume", r.volume}, extra...)
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// held is the session the table holds at path, as Oh My Pi reads it.
func (r *sessionsImportRig) held(path string) (string, bool) {
	r.t.Helper()
	conn, err := ompsessions.Connect(context.Background(), r.dsnFile)
	if err != nil {
		r.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	content, ok, err := ompsessions.Read(context.Background(), conn, path)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(content), ok
}

// `legion sessions import` copies each claim's recorded session file from the tree volume into the
// session table under the path the claim recorded, the key Oh My Pi resumes it by, and says per
// claim what it did: copied, nothing recorded, or failed for a file the volume does not hold, which
// exits 1. Run again it copies nothing twice, and a session an agent has since written is refused
// and left as it was.
func TestSessionsImportCopiesEachClaimsSessionOnceAndReportsPerClaim(t *testing.T) {
	architect := agentSessions + "/--legion-workspaces-sjawhar-legion-legion-1--/2026-10-08T12-00-00-000Z_0001.jsonl"
	tester := agentSessions + "/--legion-workspaces-sjawhar-legion-legion-1--/2026-10-08T13-00-00-000Z_0002.jsonl"
	lost := agentSessions + "/--legion-workspaces-sjawhar-legion-legion-1--/2026-10-08T14-00-00-000Z_0003.jsonl"
	other := agentSessions + "/other/2026-10-08T15-00-00-000Z_0004.jsonl"
	r := newSessionsImportRig(t, []map[string]string{
		{"token": "legion-legion-legion-1-tester", "tree": "LEGION-1", "sessionFile": tester, "state": "working"},
		{"token": "legion-legion-legion-1-architect", "tree": "LEGION-1", "sessionFile": architect},
		{"token": "legion-legion-legion-1-planner", "tree": "LEGION-1", "sessionFile": ""},
		{"token": "legion-legion-legion-1-reviewer", "tree": "LEGION-1", "sessionFile": lost},
		{"token": "legion-legion-legion-2-architect", "tree": "LEGION-2", "sessionFile": other},
	})
	r.write(architect, "{\"type\":\"session\",\"id\":\"0001\"}\n")
	r.write(tester, "{\"type\":\"session\",\"id\":\"0002\"}\n{\"type\":\"message\"}\n")

	code, out, errb := r.run("--tree", "LEGION-1")
	if code != 1 || errb != "" {
		t.Fatalf("first import = %d, stderr %q; want 1 for the claim whose file is gone", code, errb)
	}
	for _, want := range []string{
		"legion-legion-legion-1-architect copied " + architect + " (31 bytes, sha256 ",
		"legion-legion-legion-1-planner recorded no session: nothing to copy\n",
		"legion-legion-legion-1-reviewer failed " + lost + ": read the session file: ",
		"legion-legion-legion-1-tester copied " + tester + " (50 bytes, sha256 ",
		"legion-legion-legion-1-reviewer missing " + lost + ": the session table holds no such session (tree LEGION-1)\n",
		"legion-legion-legion-2-architect missing " + other + ": the session table holds no such session (tree LEGION-2)\n",
		"legion sessions import: copied 2, copied before 0, nothing recorded 1, failed 1, refused 0, missing 2\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("first import printed\n%s\nwant a line containing %q", out, want)
		}
	}
	if strings.Contains(out, "legion-legion-legion-2-architect copied") || strings.Contains(out, "legion-legion-legion-2-architect failed") {
		t.Errorf("--tree LEGION-1 copied another tree's claim:\n%s", out)
	}
	if held, ok := r.held(tester); !ok || held != "{\"type\":\"session\",\"id\":\"0002\"}\n{\"type\":\"message\"}\n" {
		t.Errorf("the table holds %q (%t) for the tester, want its file", held, ok)
	}

	r.write(lost, "{\"type\":\"session\",\"id\":\"0003\"}\n")
	code, out, _ = r.run("--tree", "LEGION-1")
	if code != 0 || !strings.Contains(out, "legion-legion-legion-1-architect copied before "+architect+" (identical, 31 bytes") ||
		!strings.Contains(out, "legion sessions import: copied 1, copied before 2, nothing recorded 1, failed 0, refused 0, missing 1\n") {
		t.Fatalf("second import = %d:\n%s\nwant the two copied before, the found one copied, the other tree's still missing, exit 0", code, out)
	}

	r.write(architect, "{\"type\":\"session\",\"id\":\"0001\"}\n{\"type\":\"message\",\"written\":\"after the copy\"}\n")
	code, out, _ = r.run("--tree", "LEGION-1")
	if code != 1 || !strings.Contains(out, "legion-legion-legion-1-architect refused "+architect+": the session table already holds "+architect+" with other content") ||
		!strings.Contains(out, "failed 0, refused 1, missing 1\n") {
		t.Fatalf("import of a changed file = %d:\n%s\nwant the architect refused and counted as refused", code, out)
	}

	// The daemon-launched controller belongs to no tree: --claim selects it, read from its own volume.
	r.write(other, "{\"type\":\"session\",\"id\":\"0004\"}\n")
	if code, out, _ := r.run("--claim", "legion-legion-legion-2-architect"); code != 0 ||
		!strings.Contains(out, "legion-legion-legion-2-architect copied "+other) || strings.Contains(out, "legion-legion-legion-1-tester copied") {
		t.Fatalf("import of one claim = %d:\n%s\nwant that claim alone copied, exit 0", code, out)
	}
	// A selection no claim matches is a mistyped key, never a clean copy.
	for _, selection := range [][]string{{"--tree", "ACME-404"}, {"--claim", "legion-acme-acme-404-architect"}} {
		if code, _, errb := r.run(selection...); code != 1 || !strings.Contains(errb, "has no claim of "+selection[1]) {
			t.Errorf("import of %v = %d, %q; want exit 1 naming it", selection, code, errb)
		}
	}
	if held, _ := r.held(architect); held != "{\"type\":\"session\",\"id\":\"0001\"}\n" {
		t.Errorf("after the refusal the table holds %q for the architect, want what it held", held)
	}
}

// --mark-lost copies nothing and marks, in the stopped daemon's own database, each claim whose
// recorded session the table lacks as the daemon marks a claim whose volume was lost: no session,
// no session file, its workspace lost. A claim the table holds, and one whose record no longer
// names that session, are left as they are.
func TestSessionsImportMarksAClaimWhoseSessionTheTableLacksLost(t *testing.T) {
	copied := agentSessions + "/--a--/2026-10-08T12-00-00-000Z_0001.jsonl"
	gone := agentSessions + "/--b--/2026-10-08T12-00-00-000Z_0002.jsonl"
	moved := agentSessions + "/--c--/2026-10-08T12-00-00-000Z_0003.jsonl"
	r := newSessionsImportRig(t, []map[string]string{
		{"token": "legion-legion-legion-1-architect", "tree": "LEGION-1", "sessionFile": copied},
		{"token": "legion-legion-legion-2-architect", "tree": "LEGION-2", "sessionFile": gone},
		{"token": "legion-legion-legion-3-architect", "tree": "LEGION-3", "sessionFile": moved},
	})
	r.write(copied, "{\"type\":\"session\",\"id\":\"0001\"}\n")
	if code, out, _ := r.run("--tree", "LEGION-1"); code != 0 || !strings.Contains(out, "missing 2\n") {
		t.Fatalf("import = %d:\n%s\nwant two claims of other trees missing, exit 0", code, out)
	}
	daemonDSN, daemonDSNFile := testpg.DSNFile(t, "legion_sessions_mark_test")
	st, err := store.Open(context.Background(), daemonDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		token       legionclaim.Token
		issue, file string
	}{
		{"legion-legion-legion-1-architect", "LEGION-1", copied},
		{"legion-legion-legion-2-architect", "LEGION-2", gone},
		{"legion-legion-legion-3-architect", "LEGION-3", moved + ".newer"},
	} {
		if err := st.PutClaim(context.Background(), supervise.Claim{Token: c.token, Project: "legion", Tree: c.issue, TreeEpoch: 1, Issue: c.issue,
			Role: legionclaim.RoleArchitect, Generation: 2, State: supervise.StateSuspended, Session: "ses-" + c.issue, SessionFile: c.file}); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "sessions", "import", "--dsn-file", r.dsnFile, "--claims", r.claims, "--mark-lost", "--daemon-dsn-file", daemonDSNFile}, &out, &errb)
	for _, want := range []string{
		"legion-legion-legion-2-architect marked lost: the session table holds no " + gone,
		"legion-legion-legion-3-architect failed to mark lost: the daemon's database holds no such claim recording " + moved,
		"legion sessions import: marked lost 1, failed 1\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--mark-lost printed\n%s\nwant a line containing %q", out.String(), want)
		}
	}
	if code != 1 {
		t.Errorf("--mark-lost = %d, want 1 for the claim it could not mark", code)
	}
	stored, err := st.Claims(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range stored {
		lost := c.Token == "legion-legion-legion-2-architect"
		if lost != (c.Session == "" && c.SessionFile == "" && c.WorkspaceLost) {
			t.Errorf("claim %s: session %q, session file %q, workspace lost %t; want it marked lost %t", c.Token, c.Session, c.SessionFile, c.WorkspaceLost, lost)
		}
	}
}

// The claims list is the one `legion claims list --json` prints, read from stdin with `-`; anything
// else is refused before the database is opened, as is a missing flag.
func TestSessionsImportRefusesWhatIsNotAClaimsList(t *testing.T) {
	r := newSessionsImportRig(t, nil)
	if err := os.WriteFile(r.claims, []byte(`[{"token":"legion-legion-legion-1-architect"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := r.run(); code != 1 || !strings.Contains(errb, "is not what `legion claims list --json` prints") {
		t.Errorf("import of a bare array = %d, %q; want the refusal", code, errb)
	}
	if err := os.WriteFile(r.claims, []byte(`{"claim":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := r.run(); code != 1 || !strings.Contains(errb, "has no claims member") {
		t.Errorf("import of a list with no claims = %d, %q; want the refusal", code, errb)
	}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "sessions", "import", "--claims", r.claims}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "--dsn-file is required") {
		t.Errorf("import without --dsn-file = %d, %q; want the usage error", code, errb.String())
	}

	stdin, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(`{"claims":[{"token":"legion-legion-legion-1-planner","tree":"LEGION-1","sessionFile":""}]}`); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	previous := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = previous; _ = stdin.Close() })
	out.Reset()
	if code := run(context.Background(), []string{"legion", "sessions", "import", "--dsn-file", r.dsnFile, "--claims", "-"}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "legion-legion-legion-1-planner recorded no session: nothing to copy") {
		t.Errorf("import from stdin = %d:\n%s", code, out.String())
	}
}
