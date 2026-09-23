package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
	if code != 1 || !strings.Contains(errb.String(), filepath.Join(".legion", "tester.json")) {
		t.Fatalf("handoff complete = %d, stderr %q; want a missing committed tester handoff refusal", code, errb.String())
	}
}

// fakeHandoffJJ writes the jj a pane is told as LEGION_JJ_PATH, which reports every listed path as
// committed on @- and names commit as @-; a decoy jj first on PATH fails naming itself.
func fakeHandoffJJ(t *testing.T, commit string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jj")
	script := `#!/bin/sh
for last; do :; done
case " $* " in
*" file list "*) printf '%s\n' "$last" ;;
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
	if err := os.WriteFile(filepath.Join(workspace, ".legion", "tester.json"), []byte("{}\n"), 0o644); err != nil {
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
