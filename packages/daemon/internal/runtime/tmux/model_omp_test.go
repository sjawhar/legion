package tmux

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// A pane that resumes the session its claim recorded runs the model the operator's settings put
// on the `default` role, not the one that session saved: the pane's command carries `--model
// @default`, and @default is Oh My Pi's own name for that role. Measured on the pinned binary: a
// session saved on one model, the profile's `modelRoles.default` moved to another, and the pane's
// own command answering with the profile's current model. The same command with the flag taken
// out is the control, and reports the saved model instead.
//
// The resume leaves the session's earlier bytes alone and adds no second file, because a launch
// under the flag records no model of its own — which is why the flag rides every launch and not
// the resuming one alone (TestInnerCommand).
func TestTheResumedPaneRunsTheConfiguredModel(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testbin.AnthropicAnswer(w, "saved", "ok")
	}))
	defer server.Close()
	testbin.OMPHome(t, omp, home)
	agent := testbin.WriteOMPProfile(t, home,
		"providers:\n  operator:\n    baseUrl: "+server.URL+"\n    auth: apiKey\n    api: anthropic-messages\n"+
			"    apiKey: unused\n    models:\n      - id: saved\n        name: Saved\n      - id: configured\n        name: Configured\n",
		"modelRoles:\n  default: operator/saved\n")
	if err := os.WriteFile(filepath.Join(dir, "role.md"), []byte("The test's role prompt.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := testbin.OMPEnviron(home)
	run := func(command, stdin string) (stdout, stderr string, exitErr error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, shell, "-c", command)
		cmd.Dir, cmd.Env, cmd.Stdin = work, environ, strings.NewReader(stdin)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return out.String(), errOut.String(), err
	}

	// A session of the profile's own, saved on operator/saved: print mode answers one short turn
	// through the test's provider and writes the file the pane resumes below.
	if stdout, stderr, err := run(shellprefix.Word(omp)+" -p 'Reply ok.'", ""); err != nil {
		t.Fatalf("saving a session: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	sessions := filepath.Join(agent, "sessions")
	saved := testbin.SessionFiles(t, sessions)
	if len(saved) != 1 {
		t.Fatalf("the profile holds %q after one print-mode turn, want one session file", saved)
	}
	before, err := os.ReadFile(saved[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(before, []byte(`"model":"operator/saved"`)) {
		t.Fatalf("the saved session names no model_change on operator/saved:\n%s", before)
	}

	// The operator moves the role, as apply.sh does for a running deployment's next pods.
	if err := os.WriteFile(filepath.Join(agent, "config.yml"), []byte("modelRoles:\n  default: operator/configured\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	spec := runtime.SpawnSpec{ResumeSessionFile: saved[0], Prompt: runtime.PromptParts{RolePromptPaths: []string{filepath.Join(dir, "role.md")}}}
	inner, pairs, err := NewOmpLauncher(nil, shellprefix.Word(omp)).Launch(spec, Host{})
	if err != nil || pairs != nil {
		t.Fatalf("Launch = %v, %v", pairs, err)
	}
	stdout, stderr, err := run(inner, `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("%s: %v\nstdout: %s\nstderr: %s", inner, err, stdout, stderr)
	}
	if got := testbin.StateModel(t, stdout); got != "operator/configured" {
		t.Errorf("the resumed pane runs %s, want the profile's operator/configured\nstderr: %s", got, stderr)
	}

	// The control: the same command with the flag taken out reads the session's own pin.
	without := strings.Replace(inner, " --model @default", "", 1)
	if without == inner {
		t.Fatalf("the pane's command carries no --model @default: %s", inner)
	}
	plain, stderr, err := run(without, `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("%s: %v\nstdout: %s\nstderr: %s", without, err, plain, stderr)
	}
	if got := testbin.StateModel(t, plain); got != "operator/saved" {
		t.Errorf("without the flag the resumed pane runs %s, want the session's own operator/saved", got)
	}

	after, err := os.ReadFile(saved[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, before) {
		t.Errorf("the resumed session's earlier bytes changed:\nbefore: %s\nafter:  %s", before, after)
	}
	if files := testbin.SessionFiles(t, sessions); !slices.Equal(files, saved) {
		t.Errorf("the profile holds %q after the resumes, want the one session file %q", files, saved)
	}
}
