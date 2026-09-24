package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

func TestHandoffWriteReadAndMessagesPersistInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "write", "--workspace", workspace, "--phase", "implement", "--data", `{"filesChanged":["x.go"]}`}, &out, &errb); code != 0 {
		t.Fatalf("handoff write = %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, ".legion", "implement.json")); err != nil {
		t.Fatalf("handoff file: %v", err)
	}
	out.Reset()
	if code := run(context.Background(), []string{"legion", "handoff", "read", "--workspace", workspace, "--phase", "implement"}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"phase": "implement"`) {
		t.Fatalf("handoff read = %d: stdout %s stderr %s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"legion", "handoff", "message", "--workspace", workspace, "--from", "architect", "--to", "implement", "--body", "start"}, &out, &errb); code != 0 {
		t.Fatalf("handoff message = %d: %s", code, errb.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"legion", "handoff", "messages", "--workspace", workspace}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"body": "start"`) {
		t.Fatalf("handoff messages = %d: stdout %s stderr %s", code, out.String(), errb.String())
	}
}

func TestHandoffCompleteRefusesUncommittedOrMissingPhaseFileBeforeHTTP(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("LEGION_ROLE", "tester")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "tests passed", "--verdict", "pass"}, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), filepath.Join(".legion", "test.json")) {
		t.Fatalf("handoff complete = %d, stderr %q; want a refusal naming the tester's handoff file, .legion/test.json", code, errb.String())
	}
}

// fakeHandoffJJ writes the jj a pane is told as LEGION_JJ_PATH, which reports the handoff committed
// (no working-copy change) and names commit as the commit carrying it; a decoy jj first on PATH
// fails naming itself.
func fakeHandoffJJ(t *testing.T, commit string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jj")
	script := `#!/bin/sh
case " $* " in
*" diff "*) ;;
*" log "*) printf '%s' "` + commit + `" ;;
*) echo "unexpected jj $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake jj: %v", err)
	}
	decoys := t.TempDir()
	if err := os.WriteFile(filepath.Join(decoys, "jj"), []byte("#!/bin/sh\necho 'the jj on PATH ran' >&2\nexit 97\n"), 0o700); err != nil {
		t.Fatalf("write the decoy jj: %v", err)
	}
	t.Setenv("PATH", decoys+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// handoffDaemon answers /legion/v1/handoff/complete and hands back the request bodies it read.
func handoffDaemon(t *testing.T) *[]map[string]any {
	t.Helper()
	bodies := &[]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/legion/v1/handoff/complete" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		*bodies = append(*bodies, body)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)
	t.Setenv("LEGION_DAEMON_URL", server.URL)
	t.Setenv("LEGION_GRANT_FILE", "")
	if err := os.Unsetenv("LEGION_GRANT_FILE"); err != nil {
		t.Fatalf("unset LEGION_GRANT_FILE: %v", err)
	}
	t.Setenv("LEGION_GRANT", "grant-1")
	return bodies
}

// `legion handoff complete` resolves the committed handoff with the jj the daemon resolved at boot
// (LEGION_JJ_PATH, set on every pane) and refuses without it: never a PATH lookup.
func TestHandoffCompleteResolvesTheCommitWithTheJJBootResolved(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".legion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".legion", "test.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_ROLE", "tester")
	jj := fakeHandoffJJ(t, "c0ffee")
	bodies := handoffDaemon(t)
	args := []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "tests passed", "--verdict", "pass"}

	t.Setenv("LEGION_JJ_PATH", "")
	var out, errb bytes.Buffer
	if code := run(context.Background(), args, &out, &errb); code != 1 || !strings.Contains(errb.String(), "LEGION_JJ_PATH") {
		t.Fatalf("handoff complete without LEGION_JJ_PATH = %d, stderr %q; want a refusal naming it", code, errb.String())
	}

	t.Setenv("LEGION_JJ_PATH", jj)
	errb.Reset()
	if code := run(context.Background(), args, &out, &errb); code != 0 {
		t.Fatalf("handoff complete = %d, stderr %q", code, errb.String())
	}
	if len(*bodies) != 1 || (*bodies)[0]["commit"] != "c0ffee" {
		t.Fatalf("daemon read %v, want one completion naming commit c0ffee", *bodies)
	}
}

// The merger verifies and publishes READY and writes no handoff (packages/pi-envoy/roles/merger.md:
// "merger is not a file-backed phase"), so its completion needs no .legion file and reports the
// commit its workspace sits on.
func TestHandoffCompleteReadyForTheMergerNeedsNoHandoffFile(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("LEGION_ROLE", "merger")
	t.Setenv("LEGION_JJ_PATH", fakeHandoffJJ(t, "beef"))
	bodies := handoffDaemon(t)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb); code != 0 {
		t.Fatalf("merger handoff complete --ready = %d, stderr %q", code, errb.String())
	}
	if len(*bodies) != 1 || (*bodies)[0]["ready"] != true || (*bodies)[0]["commit"] != "beef" {
		t.Fatalf("daemon read %v, want one READY naming commit beef", *bodies)
	}
}

// handoffRepo is a real colocated jj repository standing in for a pane workspace. It returns the
// workspace and the absolute jj a pane is told as LEGION_JJ_PATH.
func handoffRepo(t *testing.T) (string, string) {
	t.Helper()
	jj, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("jj is required: %v", err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	handoffJJ(t, jj, filepath.Dir(workspace), "git", "init", "--colocate", workspace)
	return workspace, jj
}

func handoffJJ(t *testing.T, jj, dir string, args ...string) string {
	t.Helper()
	command := exec.Command(jj, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("jj %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeHandoffFile(t *testing.T, workspace, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspace, ".legion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".legion", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Every file-backed role prompt tells its pane to write its handoff under the phase word
// (packages/pi-envoy/roles/*.md: the legion tool's handoff_write with phase plan|implement|test|review), while
// the pane's LEGION_ROLE is the claim word (planner, implementer, tester, reviewer). A pane that
// follows its prompt from its workspace and commits the handoff must be able to complete its
// phase, reporting the commit that carries that handoff.
func TestHandoffCompleteAcceptsTheHandoffItsRolePromptWrites(t *testing.T) {
	for _, tc := range []struct{ role, phase, verdict string }{
		{"planner", "plan", ""},
		{"implementer", "implement", ""},
		{"tester", "test", "pass"},
		{"reviewer", "review", ""},
	} {
		t.Run(tc.role, func(t *testing.T) {
			workspace, jj := handoffRepo(t)
			t.Chdir(workspace)
			var out, errb bytes.Buffer
			if code := run(context.Background(), []string{"legion", "handoff", "write", "--phase", tc.phase, "--data", `{"summary":"done"}`}, &out, &errb); code != 0 {
				t.Fatalf("handoff write --phase %s = %d: %s", tc.phase, code, errb.String())
			}
			handoffJJ(t, jj, workspace, "commit", "-m", tc.phase+": record handoff")
			carrying := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
			t.Setenv("LEGION_ROLE", tc.role)
			t.Setenv("LEGION_JJ_PATH", jj)
			bodies := handoffDaemon(t)
			args := []string{"legion", "handoff", "complete", "--summary", "phase done"}
			if tc.verdict != "" {
				args = append(args, "--verdict", tc.verdict)
			}
			errb.Reset()
			if code := run(context.Background(), args, &out, &errb); code != 0 {
				t.Fatalf("%s handoff complete after committing .legion/%s.json = %d, stderr %q", tc.role, tc.phase, code, errb.String())
			}
			if len(*bodies) != 1 || (*bodies)[0]["commit"] != carrying {
				t.Fatalf("daemon read %v, want one completion naming %s, the commit carrying .legion/%s.json", *bodies, carrying, tc.phase)
			}
		})
	}
}

// The Stage 3 proof's primary issue, reduced: the base branch already carries .legion handoffs from
// an earlier merged pull request (sjawhar/legion-smoke main holds .legion/implementer.json from #89
// and .legion/implement.json from a later merge), the implementer commits its product change, and
// its fresh handoff is still uncommitted in @. Run from the workspace, as a pane runs it, the
// completion must refuse: the commit it would report carries another issue's handoff, not this
// phase's.
func TestHandoffCompleteRefusesWhenOnlyAStaleBaseHandoffIsCommitted(t *testing.T) {
	workspace, jj := handoffRepo(t)
	t.Chdir(workspace)
	writeHandoffFile(t, workspace, "implementer.json", `{"issue":"EARLIER-1"}`+"\n")
	writeHandoffFile(t, workspace, "implement.json", `{"issue":"EARLIER-1"}`+"\n")
	handoffJJ(t, jj, workspace, "commit", "-m", "an earlier merged pull request")
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handoffJJ(t, jj, workspace, "commit", "-m", "feat: the product change")
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "write", "--phase", "implement", "--data", `{"issue":"THIS-1"}`}, &out, &errb); code != 0 {
		t.Fatalf("handoff write = %d: %s", code, errb.String())
	}
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t)
	errb.Reset()
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "implemented"}, &out, &errb)
	if code != 1 || len(*bodies) != 0 {
		t.Fatalf("handoff complete with this phase's handoff uncommitted = %d, daemon read %v, stderr %q; want a refusal before any request", code, *bodies, errb.String())
	}
}

// --workspace names the pane workspace from any directory. A handoff committed there completes the
// phase whatever the caller's working directory: the committed-handoff check must not resolve the
// handoff path against the caller's directory. The handoff is committed under both the phase and
// the role word so this test is independent of which one the check reads.
func TestHandoffCompleteWithWorkspaceFlagIgnoresTheCallersDirectory(t *testing.T) {
	workspace, jj := handoffRepo(t)
	writeHandoffFile(t, workspace, "implement.json", `{"issue":"THIS-1"}`+"\n")
	writeHandoffFile(t, workspace, "implementer.json", `{"issue":"THIS-1"}`+"\n")
	handoffJJ(t, jj, workspace, "commit", "-m", "implement: record handoff")
	carrying := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
	t.Chdir(t.TempDir())
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "implemented"}, &out, &errb); code != 0 {
		t.Fatalf("handoff complete --workspace %s from another directory = %d, stderr %q", workspace, code, errb.String())
	}
	if len(*bodies) != 1 || (*bodies)[0]["commit"] != carrying {
		t.Fatalf("daemon read %v, want one completion naming %s", *bodies, carrying)
	}
}

// A pane's workspace is cloned from the repository, so the base branch is its origin's main
// (trunk()). A handoff that only the base carries — main already holds .legion/implement.json from
// an earlier merged pull request, and this phase wrote none — is inherited, never this phase's: the
// completion refuses before any request, even with nothing uncommitted in the workspace.
func TestHandoffCompleteRefusesAHandoffOnlyTheOriginsMainCarries(t *testing.T) {
	seed, jj := handoffRepo(t)
	writeHandoffFile(t, seed, "implement.json", `{"issue":"EARLIER-1"}`+"\n")
	handoffJJ(t, jj, seed, "commit", "-m", "an earlier merged pull request")
	handoffJJ(t, jj, seed, "bookmark", "set", "main", "-r", "@-")
	workspace := filepath.Join(t.TempDir(), "workspace")
	handoffJJ(t, jj, filepath.Dir(workspace), "git", "clone", seed, workspace)
	trunk := handoffJJ(t, jj, workspace, "log", "-r", "trunk()", "--no-graph", "-T", "commit_id")
	if origin := handoffJJ(t, jj, workspace, "log", "-r", "main@origin", "--no-graph", "-T", "commit_id"); trunk != origin {
		t.Fatalf("trunk() in the clone is %q, want origin's main %q", trunk, origin)
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handoffJJ(t, jj, workspace, "commit", "-m", "feat: the product change")
	t.Chdir(workspace)
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t)
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "implemented"}, &out, &errb)
	if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), "only the base branch carries it") {
		t.Fatalf("handoff complete with only origin's main carrying .legion/implement.json = %d, daemon read %v, stderr %q; want the inherited-handoff refusal before any request", code, *bodies, errb.String())
	}
}

// The implementer's production check writes no handoff: skills/legion-worker/SKILL.md's completion
// gate says the post-merge production check "writes no .legion/<phase>.json, commits no handoff,
// and reports with `legion handoff complete` alone", and the daemon's workflow does not treat
// production_check as file-backed (internal/workflow/effects.go fileBacked). The phase starts only
// after the ordinary human squash merge deleted the issue branch, so the implementer's
// `jj git fetch` abandons the branch and leaves `@` on main, which now carries the merged
// .legion/implement.json. The daemon's state names the issue's phase; the completion must reach
// the daemon. In the Stage 3 acceptance run at 40a40069 the implementer was refused here twice
// ("only the base branch carries it") and completed only after resurrecting the deleted branch
// with `jj new <its old head>`.
func TestHandoffCompleteReportsTheProductionCheckOnTheMergedMain(t *testing.T) {
	seed, jj := handoffRepo(t)
	writeHandoffFile(t, seed, "implement.json", `{"issue":"THIS-1"}`+"\n")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handoffJJ(t, jj, seed, "commit", "-m", "feat: the smoke change (#1)")
	handoffJJ(t, jj, seed, "bookmark", "set", "main", "-r", "@-")
	workspace := filepath.Join(t.TempDir(), "workspace")
	handoffJJ(t, jj, filepath.Dir(workspace), "git", "clone", seed, workspace)
	merged := handoffJJ(t, jj, workspace, "log", "-r", "trunk()", "--no-graph", "-T", "commit_id")
	if parent := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id"); parent != merged {
		t.Fatalf("the workspace's @- is %q, want the merged main %q", parent, merged)
	}
	t.Chdir(workspace)
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_ISSUE", "THIS-1")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemonInPhase(t, "THIS-1", phase.ProductionCheck)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "production check verified"}, &out, &errb); code != 0 {
		t.Fatalf("implementer handoff complete in production_check on the merged main = %d, stderr %q", code, errb.String())
	}
	if len(*bodies) != 1 {
		t.Fatalf("daemon read %v, want one production-check completion", *bodies)
	}
}

// handoffDaemonInPhase is handoffDaemon that also serves the daemon's state document
// (GET /legion/v1/state, internal/api/state.go) with ISSUE in phase P.
func handoffDaemonInPhase(t *testing.T, issue string, p phase.Phase) *[]map[string]any {
	t.Helper()
	bodies := &[]map[string]any{}
	state := api.State{Issues: map[string]api.Issue{issue: {Key: issue, Generation: 1, Phase: p}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/legion/v1/state":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state)
		case r.Method == http.MethodPost && r.URL.Path == "/legion/v1/handoff/complete":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			*bodies = append(*bodies, body)
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("LEGION_DAEMON_URL", server.URL)
	t.Setenv("LEGION_GRANT_FILE", "")
	if err := os.Unsetenv("LEGION_GRANT_FILE"); err != nil {
		t.Fatalf("unset LEGION_GRANT_FILE: %v", err)
	}
	t.Setenv("LEGION_GRANT", "grant-1")
	return bodies
}

// The end game every clean review round ends in (skills/legion-worker/SKILL.md: the reviewer
// approves only a head that carries no .legion/, and the implementer pushes the .legion/
// deletion): once the branch head has deleted .legion/, the implementer and the tester report
// completion without recreating it (the same skill's completion gate: once .legion/ is gone, "a
// later rebase, bare-gate re-check, confirmation, retro, or the post-merge production check writes
// no .legion/<phase>.json, commits no handoff, and reports with `legion handoff complete` alone";
// packages/pi-envoy/roles/implementer.md and tester.md say the same). The commit that deleted the
// handoff is the last commit on the branch that changed it, and the completion reports it.
func TestHandoffCompleteAfterTheLegionDeletionRecreatesNothing(t *testing.T) {
	for _, tc := range []struct{ role, verdict string }{
		{"implementer", ""},
		{"tester", "pass"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			workspace, jj := handoffRepo(t)
			t.Chdir(workspace)
			if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("smoke\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			handoffJJ(t, jj, workspace, "commit", "-m", "feat: the product change")
			writeHandoffFile(t, workspace, "implement.json", `{"issue":"THIS-1"}`+"\n")
			handoffJJ(t, jj, workspace, "commit", "-m", "implement: record handoff")
			writeHandoffFile(t, workspace, "test.json", `{"issue":"THIS-1"}`+"\n")
			handoffJJ(t, jj, workspace, "commit", "-m", "test: record handoff")
			if err := os.RemoveAll(filepath.Join(workspace, ".legion")); err != nil {
				t.Fatal(err)
			}
			handoffJJ(t, jj, workspace, "commit", "-m", "chore: remove .legion/ before approval")
			deletion := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
			t.Setenv("LEGION_ROLE", tc.role)
			t.Setenv("LEGION_JJ_PATH", jj)
			bodies := handoffDaemon(t)
			args := []string{"legion", "handoff", "complete", "--summary", "the .legion/ deletion is pushed"}
			if tc.verdict != "" {
				args = append(args, "--verdict", tc.verdict)
			}
			var out, errb bytes.Buffer
			if code := run(context.Background(), args, &out, &errb); code != 0 {
				t.Fatalf("%s handoff complete after the .legion/ deletion = %d, stderr %q", tc.role, code, errb.String())
			}
			if len(*bodies) != 1 || (*bodies)[0]["commit"] != deletion {
				t.Fatalf("daemon read %v, want one completion naming %s, the commit that deleted .legion/", *bodies, deletion)
			}
			if _, err := os.Stat(filepath.Join(workspace, ".legion")); !os.IsNotExist(err) {
				t.Fatalf(".legion/ after the completion: %v, want it still absent", err)
			}
		})
	}
}

// A handoff commit is its own role's: the pane's App identity (JJ_USER/JJ_EMAIL, which the daemon
// sets on every pane) authors it. A tester whose handoff landed in the implementer's commit is
// refused before any request, naming the author and the fix; the same handoff committed by the
// tester itself completes.
func TestHandoffCompleteRefusesAHandoffCommitAnotherAppAuthored(t *testing.T) {
	for _, tc := range []struct {
		name   string
		author string
		ok     bool
	}{
		{name: "authored by the implementer", author: "legion-implementer[bot]"},
		{name: "authored by the tester", author: "legion-reviewer[bot]", ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, jj := handoffRepo(t)
			t.Chdir(workspace)
			as := func(args ...string) {
				t.Helper()
				command := exec.Command(jj, args...)
				command.Dir = workspace
				command.Env = append(os.Environ(), "JJ_USER="+tc.author, "JJ_EMAIL=bot@example.invalid")
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("jj %v: %v\n%s", args, err, output)
				}
			}
			// The commit the handoff lands in is authored by tc.author, as a role's own fresh
			// working copy is.
			as("new")
			writeHandoffFile(t, workspace, "test.json", `{"issue":"THIS-1"}`+"\n")
			as("commit", "-m", "test: record handoff")
			t.Setenv("LEGION_ROLE", "tester")
			t.Setenv("LEGION_JJ_PATH", jj)
			t.Setenv("JJ_USER", "legion-reviewer[bot]")
			t.Setenv("JJ_EMAIL", "bot@example.invalid")
			bodies := handoffDaemon(t)
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "tests pass", "--verdict", "pass"}, &out, &errb)
			if tc.ok {
				if code != 0 || len(*bodies) != 1 {
					t.Fatalf("handoff complete on the tester's own commit = %d, daemon read %v, stderr %q", code, *bodies, errb.String())
				}
				return
			}
			if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), "legion-implementer[bot]") || !strings.Contains(errb.String(), "jj new") {
				t.Fatalf("handoff complete on the implementer's commit = %d, daemon read %v, stderr %q; want a refusal naming the author and jj new, before any request", code, *bodies, errb.String())
			}
		})
	}
}
