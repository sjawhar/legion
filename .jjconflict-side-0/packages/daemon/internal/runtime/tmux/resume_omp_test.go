package tmux

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// A role moved back to Oh My Pi from Claude Code resumes the session file its claim recorded, which
// is a Claude Code transcript, and the omp launcher passes it to `omp --resume` as it passes any
// recorded file. On the pinned binary that launch fails loudly: omp exits non-zero naming the file
// and saying it left it unmodified, answers no RPC frame, and the transcript's bytes are the same
// after. A fresh launch from the same command, HOME and profile answers `ready`, so the refusal is
// the transcript's, not a launch that could not start.
func TestThePinnedOhMyPiRefusesToResumeAClaudeCodeTranscript(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
	testbin.OMPHome(t, omp, home)
	// A profile whose one model is an operator's provider that nothing listens on: RPC mode starts
	// only on a profile with a model, and naming it keeps the test off any credential the machine
	// carries. get_state makes no model call, so the provider is never dialled.
	agent := filepath.Join(home, ".omp", "profiles", "legion", "agent")
	for path, content := range map[string]string{
		filepath.Join(agent, "models.yml"): "providers:\n  operator:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n" +
			"    apiKey: unused\n    models:\n      - id: resume-test\n        name: Resume test\n",
		filepath.Join(agent, "config.yml"): "modelRoles:\n  default: operator/resume-test\n",
		filepath.Join(dir, "role.md"):      "The test's role prompt.\n",
		filepath.Join(work, ".keep"):       "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	transcript, err := os.ReadFile(filepath.Join("testdata", "claude-transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(dir, "claude", "projects", "-workspace-LEGION-43", "11111111-2222-4333-8444-555555555555.jsonl")
	if err := os.MkdirAll(filepath.Dir(recorded), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recorded, transcript, 0o600); err != nil {
		t.Fatal(err)
	}

	launch := func(resume string) (stdout, stderr string, exitErr error) {
		t.Helper()
		spec := runtime.SpawnSpec{ResumeSessionFile: resume, Prompt: runtime.PromptParts{RolePromptPaths: []string{filepath.Join(dir, "role.md")}}}
		inner, pairs, err := NewOmpLauncher(nil, shellprefix.Word(omp)).Launch(spec, Host{})
		if err != nil || pairs != nil {
			t.Fatalf("Launch = %v, %v", pairs, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, shell, "-c", inner)
		cmd.Dir = work
		cmd.Env = []string{"HOME=" + home, "OMP_PROFILE=legion", "PATH=/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}
		// One RPC request, then end of input: a session that started answers it and exits.
		cmd.Stdin = strings.NewReader(`{"type":"get_state","id":"1"}` + "\n")
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err = cmd.Run()
		return out.String(), errOut.String(), err
	}

	if stdout, stderr, err := launch(""); err != nil || !strings.Contains(stdout, `"type":"ready"`) {
		t.Fatalf("a fresh launch = %v, want ready on stdout\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	stdout, stderr, err := launch(recorded)
	if err == nil {
		t.Errorf("resuming the Claude Code transcript exited 0, want a refusal\nstdout: %s", stdout)
	}
	if want := `Cannot resume session "` + recorded + `": the session header is missing or malformed. The file was not modified.`; !strings.Contains(stderr, want) {
		t.Errorf("stderr does not say %q:\n%s", want, stderr)
	}
	if stdout != "" {
		t.Errorf("resuming the Claude Code transcript wrote RPC frames, want none:\n%s", stdout)
	}
	if after, err := os.ReadFile(recorded); err != nil || !bytes.Equal(after, transcript) {
		t.Errorf("the transcript after the refusal = %q, %v, want its bytes unchanged", after, err)
	}
}
