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
// (packages/pi-envoy/roles/*.md: `legion handoff write --phase plan|implement|test|review`), while
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
