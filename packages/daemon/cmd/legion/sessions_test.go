package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
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
	r := newSessionsImportRig(t, []map[string]string{
		{"token": "legion-legion-legion-1-tester", "tree": "LEGION-1", "sessionFile": tester, "state": "working"},
		{"token": "legion-legion-legion-1-architect", "tree": "LEGION-1", "sessionFile": architect},
		{"token": "legion-legion-legion-1-planner", "tree": "LEGION-1", "sessionFile": ""},
		{"token": "legion-legion-legion-1-reviewer", "tree": "LEGION-1", "sessionFile": lost},
		{"token": "legion-legion-legion-2-architect", "tree": "LEGION-2", "sessionFile": agentSessions + "/other/2026-10-08T15-00-00-000Z_0004.jsonl"},
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
		"legion sessions import: copied 2, copied before 0, nothing recorded 1, failed 1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("first import printed\n%s\nwant a line containing %q", out, want)
		}
	}
	if strings.Contains(out, "legion-legion-legion-2") {
		t.Errorf("--tree LEGION-1 touched another tree's claim:\n%s", out)
	}
	if held, ok := r.held(tester); !ok || held != "{\"type\":\"session\",\"id\":\"0002\"}\n{\"type\":\"message\"}\n" {
		t.Errorf("the table holds %q (%t) for the tester, want its file", held, ok)
	}

	r.write(lost, "{\"type\":\"session\",\"id\":\"0003\"}\n")
	code, out, _ = r.run("--tree", "LEGION-1")
	if code != 0 || !strings.Contains(out, "legion-legion-legion-1-architect copied before "+architect+" (identical, 31 bytes") ||
		!strings.Contains(out, "legion sessions import: copied 1, copied before 2, nothing recorded 1, failed 0\n") {
		t.Fatalf("second import = %d:\n%s\nwant the two copied before, the found one copied, exit 0", code, out)
	}

	r.write(architect, "{\"type\":\"session\",\"id\":\"0001\"}\n{\"type\":\"message\",\"written\":\"after the copy\"}\n")
	code, out, _ = r.run("--tree", "LEGION-1")
	if code != 1 || !strings.Contains(out, "legion-legion-legion-1-architect refused "+architect+": the session table already holds "+architect+" with other content") {
		t.Fatalf("import of a changed file = %d:\n%s\nwant the architect refused", code, out)
	}
	if held, _ := r.held(architect); held != "{\"type\":\"session\",\"id\":\"0001\"}\n" {
		t.Errorf("after the refusal the table holds %q for the architect, want what it held", held)
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
