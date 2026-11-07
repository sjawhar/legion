package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// runOmp runs one Oh My Pi command to its end on the frames given, and returns what it wrote. The
// child is told nothing but environ: a pod's own PI_CONFIG_FILES would otherwise outrank the
// profile the measurement is of.
func runOmp(t *testing.T, argv []string, dir string, environ []string, stdin string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, environ, strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// A pod's agent resumes the session its claim recorded and runs the model the pod's overlay puts
// on the `default` role, not the one that session saved: `--model @default` rides the recorded
// argv, and @default is Oh My Pi's own name for that role. Measured on the pinned binary: a
// session saved on one model, the profile's `modelRoles.default` moved to another, and the
// recorded argv's own answer naming the profile's current model. The same argv with the flag
// taken out is the control, and reports the saved model instead.
//
// The resume leaves the session's earlier bytes alone and adds no second file, and — because a
// launch under the flag writes no `model_change` entry of its own — the saved pin is still in
// there, which is why the flag rides every launch and not the resuming one alone
// (TestAgentArgvCarriesTheConfiguredModel).
func TestTheResumedAgentRunsTheConfiguredModel(t *testing.T) {
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
	environ := testbin.OMPEnviron(home)

	// A session of the profile's own, saved on operator/saved: print mode answers one short turn
	// through the test's provider and writes the file the resume below names. The launch argv is
	// not used here — it is what the resume measures — and the model is the profile's role.
	if stdout, stderr, err := runOmp(t, []string{omp, "-p", "Reply ok."}, work, environ, ""); err != nil {
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

	argv := launch{spec: runtime.SpawnSpec{ResumeSessionFile: saved[0]}, prompt: "The test's system prompt."}.agentArgv([]string{omp})
	stdout, stderr, err := runOmp(t, argv, work, environ, `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("omp %s: %v\nstdout: %s\nstderr: %s", strings.Join(argv[1:], " "), err, stdout, stderr)
	}
	if got := testbin.StateModel(t, stdout); got != "operator/configured" {
		t.Errorf("the resumed agent runs %s, want the profile's operator/configured\nstderr: %s", got, stderr)
	}

	// The control: the same launch with the flag taken out reads the session's own pin.
	without := slices.DeleteFunc(slices.Clone(argv), func(word string) bool { return word == "--model" || word == "@default" })
	plain, stderr, err := runOmp(t, without, work, environ, `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("omp %s: %v\nstdout: %s\nstderr: %s", strings.Join(without[1:], " "), err, plain, stderr)
	}
	if got := testbin.StateModel(t, plain); got != "operator/saved" {
		t.Errorf("without the flag the resumed agent runs %s, want the session's own operator/saved", got)
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

// A role's fallback chain still walks under the launch's explicit model: a chain is keyed by the
// model string, and @default resolves to that same string, so `--model @default` costs the role
// nothing it had. Measured on the pinned binary against one loopback server serving two
// providers: the role's own model answers 429 with a `retry-after` past `retry.maxDelayMs`, which
// is Oh My Pi's signal to stop waiting, and the chain's next model answers the turn.
func TestAFallbackChainStillWalksUnderAnExplicitModel(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	hits := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := strings.HasPrefix(r.URL.Path, "/first")
		if first {
			hits["first"]++
		} else {
			hits["second"]++
		}
		mu.Unlock()
		if first {
			// Over the settings' maxDelayMs: Oh My Pi surfaces the failure rather than parking
			// the turn, and the role's chain is walked.
			w.Header().Set("retry-after", "120")
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		testbin.AnthropicAnswer(w, "second-model", "second-model answered")
	}))
	defer server.Close()
	testbin.OMPHome(t, omp, home)
	testbin.WriteOMPProfile(t, home,
		"providers:\n  first:\n    baseUrl: "+server.URL+"/first\n    auth: apiKey\n    api: anthropic-messages\n"+
			"    apiKey: unused\n    models:\n      - id: first-model\n        name: First\n"+
			"  second:\n    baseUrl: "+server.URL+"/second\n    auth: apiKey\n    api: anthropic-messages\n"+
			"    apiKey: unused\n    models:\n      - id: second-model\n        name: Second\n",
		"modelRoles:\n  default: first/first-model\n"+
			"retry:\n  modelFallback: true\n  maxDelayMs: 1000\n  fallbackChains:\n    first/first-model:\n      - second/second-model\n")

	argv := launch{prompt: "The test's system prompt."}.agentArgv([]string{omp})
	frames, stderr := promptOnce(t, argv, work, testbin.OMPEnviron(home), "Reply ok.")
	answered := ""
	for _, frame := range frames {
		if frame.Type == "message_end" {
			answered = frame.Message.Provider + "/" + frame.Message.Model
		}
	}
	if answered != "second/second-model" {
		t.Errorf("the turn was answered by %q, want the chain's second/second-model\nstderr: %s", answered, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["first"] == 0 || hits["second"] == 0 {
		t.Errorf("the server was called %v, want both the role's model and the chain's next", hits)
	}
}

// Measured, not wished for: with no `modelRoles.default` in its settings, `--model @default` does
// not refuse the launch — Oh My Pi reads @default as its own built-in chat role and starts on the
// model behind it, here claude-fable-5-1@default on google-vertex, which is no model of the
// profile's and reachable from no pod. (A role the settings never name at all is the loud case:
// `--model @nosuchrole` exits with `Model "@nosuchrole" not found`.) So the flag is no guard
// against an overlay that omits the role; the deployment's guard is the operator's, and
// deploy/kubernetes/operator-route/README.md says so.
func TestMeasuredNoDefaultRoleLaunchesOnABuiltInModel(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	testbin.OMPHome(t, omp, home)
	testbin.WriteOMPProfile(t, home,
		"providers:\n  operator:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n"+
			"    apiKey: unused\n    models:\n      - id: configured\n        name: Configured\n",
		"enabledModels:\n  - operator/configured\n")

	argv := launch{prompt: "The test's system prompt."}.agentArgv([]string{omp})
	stdout, stderr, err := runOmp(t, argv, work, testbin.OMPEnviron(home), `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("omp %s: %v, want a launch\nstdout: %s\nstderr: %s", strings.Join(argv[1:], " "), err, stdout, stderr)
	}
	got := testbin.StateModel(t, stdout)
	if strings.HasPrefix(got, "operator/") {
		t.Errorf("with no modelRoles.default the launch runs %s, want a model that is not the profile's", got)
	}
	t.Logf("with no modelRoles.default, --model @default silently runs %s", got)
}

// promptOnce runs argv, sends it one prompt, and returns every frame it wrote up to the end of
// that prompt. Oh My Pi exits when its standard input closes, so the input is held open until the
// turn is answered.
func promptOnce(t *testing.T, argv []string, dir string, environ []string, message string) ([]testbin.OMPFrame, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, environ
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, `{"type":"prompt","id":"r2","message":`+strconv.Quote(message)+"}\n"); err != nil {
		t.Fatal(err)
	}
	var frames []testbin.OMPFrame
	closed := false
	lines := bufio.NewScanner(stdout)
	// A frame is up to Oh My Pi's maxFrameBytes, which its ready frame reports as 1 MiB.
	lines.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for lines.Scan() {
		var frame testbin.OMPFrame
		if json.Unmarshal(lines.Bytes(), &frame) != nil {
			continue
		}
		frames = append(frames, frame)
		if frame.Type == "prompt_result" && frame.ID == "r2" && !closed {
			closed = true
			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !closed {
		if err := stdin.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("omp %s: %v\nstderr: %s", strings.Join(argv[1:], " "), err, errOut.String())
	}
	if !closed {
		t.Fatalf("omp %s never answered the prompt\nstderr: %s", strings.Join(argv[1:], " "), errOut.String())
	}
	return frames, errOut.String()
}
