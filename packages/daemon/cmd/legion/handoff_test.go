package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

func TestHandoffWriteAndReadPersistInWorkspace(t *testing.T) {
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
}

// A handoff past one argv string's 128 KiB cap (MAX_ARG_STRLEN) can only arrive on stdin: the
// legion tool sends every handoff_write payload that way.
func TestHandoffWriteReadsAPayloadOverTheArgvCapFromStdin(t *testing.T) {
	workspace := t.TempDir()
	records := make([]string, 0, 2000)
	for i := range 2000 {
		records = append(records, fmt.Sprintf(`{"round":%d,"note":"%s"}`, i, strings.Repeat("r", 80)))
	}
	payload := `{"filesChanged":["x.go"],"rounds":[` + strings.Join(records, ",") + `]}`
	if len(payload) <= 128*1024 {
		t.Fatalf("payload is %d bytes, not over the 128 KiB argv cap", len(payload))
	}
	input, err := os.CreateTemp(t.TempDir(), "handoff-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = previous; _ = input.Close() })
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "write", "--workspace", workspace, "--phase", "test"}, &out, &errb); code != 0 {
		t.Fatalf("handoff write from stdin = %d: %s", code, errb.String())
	}
	written, err := os.ReadFile(filepath.Join(workspace, ".legion", "test.json"))
	if err != nil {
		t.Fatalf("handoff file: %v", err)
	}
	var handoff struct {
		Phase  string            `json:"phase"`
		Rounds []json.RawMessage `json:"rounds"`
	}
	if err := json.Unmarshal(written, &handoff); err != nil || handoff.Phase != "test" || len(handoff.Rounds) != 2000 {
		t.Fatalf("handoff file = phase %q, %d rounds, err %v; want test and 2000", handoff.Phase, len(handoff.Rounds), err)
	}
}

// The command writes a handoff's schemaVersion, phase and completed itself. A payload carrying them
// — an agent copying handoff_read's output into data — is refused naming every one it carries and
// that the command writes them, so the next call succeeds; nothing is written.
func TestHandoffWriteRefusesTheFieldsItWritesNamingEach(t *testing.T) {
	workspace := t.TempDir()
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoff", "write", "--workspace", workspace, "--phase", "implement",
		"--data", `{"schemaVersion":1,"completed":"2026-09-25T00:00:00Z","proof":["ran it"]}`}, &out, &errb)
	// The refusal's fixed parenthetical names every reserved field, so the carried list is asserted
	// where the refusal lists what the data carries.
	refusal := errb.String()
	if code != 1 || !strings.Contains(refusal, "data carries schemaVersion, completed, which this command writes itself") {
		t.Fatalf("handoff write with schemaVersion and completed in data = %d, stderr %q; want one refusal naming both and that the command writes them", code, refusal)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".legion")); !os.IsNotExist(err) {
		t.Fatalf(".legion after the refused write: %v, want none", err)
	}
}

// A tester whose handoff is missing is refused before any request, and the refusal names the file
// its phase ends with, .legion/test.json.
func TestHandoffCompleteRefusesAMissingPhaseFileBeforeTheRequest(t *testing.T) {
	workspace, jj := handoffRepo(t)
	t.Setenv("LEGION_ROLE", "tester")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t, phase.Testing)
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "tests passed", "--verdict", "pass"}, &out, &errb)
	if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), filepath.Join(".legion", "test.json")) {
		t.Fatalf("handoff complete = %d, daemon read %v, stderr %q; want a refusal naming the tester's handoff file, .legion/test.json, before any request", code, *bodies, errb.String())
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
*" remote list "*) echo "origin https://github.com/acme/widgets.git" ;;
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

// handoffDaemon is the daemon a pane completes its phase with: it serves the daemon's state
// document (GET /legion/v1/state, internal/api/state.go) with the pane's issue, THIS-1, in phase p,
// answers /legion/v1/handoff/complete, and hands back the completion bodies it read.
func handoffDaemon(t *testing.T, p phase.Phase) *[]map[string]any {
	t.Helper()
	bodies := &[]map[string]any{}
	state := api.State{Issues: map[string]api.Issue{
		"THIS-1":  {Key: "THIS-1", Generation: 1, Phase: p, PullRequest: &api.PullRequestView{Number: 42, Head: "c0de"}},
		"OTHER-2": {Key: "OTHER-2", Generation: 1, Phase: phase.Merging},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/legion/v1/state":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state)
		case r.Method == http.MethodPost && r.URL.Path == "/legion/v1/gh-token":
			_, _ = w.Write([]byte(`{"token":"installation-token","appLogin":"legion-implementer[bot]"}`))
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
	t.Setenv("LEGION_ISSUE", "THIS-1")
	t.Setenv("LEGION_GRANT_FILE", "")
	if err := os.Unsetenv("LEGION_GRANT_FILE"); err != nil {
		t.Fatalf("unset LEGION_GRANT_FILE: %v", err)
	}
	t.Setenv("LEGION_GRANT", "grant-1")
	// The pane's identity is the one the daemon puts on it; a caller's exported JJ_USER/JJ_EMAIL
	// would otherwise be what `ownHandoff` compares the commit's author against, so whether these
	// tests pass would depend on the shell that ran them.
	t.Setenv("JJ_USER", "")
	t.Setenv("JJ_EMAIL", "")
	readyGitHub(t, `{"id":1,"name":"ci","status":"completed","conclusion":"success"}`, `{"context":"legacy","state":"success"}`, "clean")
	return bodies
}

// readyGitHub serves acme/widgets#42 to a merger's READY check: head c0de on main, whose ruleset
// requires the check "ci" and whose branch protection requires the status "legacy", reporting the
// given check run and commit status on the head (either may be empty) and the pull request's
// mergeable_state, plus the token route of the daemon the check redeems its grant at.
func readyGitHub(t *testing.T, checkRun, status, mergeableState string) {
	t.Helper()
	repo := "/repos/acme/widgets"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case repo + "/pulls/42":
			_, _ = w.Write([]byte(`{"head":{"sha":"c0de0000000000000000000000000000000000ff"},"base":{"ref":"main"},"mergeable_state":"` + mergeableState + `"}`))
		case repo + "/rules/branches/main":
			_, _ = w.Write([]byte(`[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]`))
		case repo + "/branches/main":
			_, _ = w.Write([]byte(`{"name":"main","protection":{"required_status_checks":{"contexts":["legacy"],"checks":[{"context":"legacy"}]}}}`))
		case repo + "/commits/c0de0000000000000000000000000000000000ff/check-runs":
			runs := "[]"
			if checkRun != "" {
				runs = "[" + checkRun + "]"
			}
			_, _ = w.Write([]byte(`{"total_count":` + strconv.Itoa(strings.Count(runs, `"name"`)) + `,"check_runs":` + runs + `}`))
		case repo + "/commits/c0de0000000000000000000000000000000000ff/status":
			statuses := "[]"
			if status != "" {
				statuses = "[" + status + "]"
			}
			_, _ = w.Write([]byte(`{"statuses":` + statuses + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("LEGION_GITHUB_API_URL", server.URL)
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
	bodies := handoffDaemon(t, phase.Testing)
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
	bodies := handoffDaemon(t, phase.Merging)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb); code != 0 {
		t.Fatalf("merger handoff complete --ready = %d, stderr %q", code, errb.String())
	}
	if len(*bodies) != 1 || (*bodies)[0]["ready"] != true || (*bodies)[0]["commit"] != "beef" {
		t.Fatalf("daemon read %v, want one READY naming commit beef", *bodies)
	}
}

// The completion names no run: the pane's own LEGION_GENERATION is the claim's launch counter,
// which moves on every relaunch within one run, and a live worker is handed the next run's task
// without being relaunched at all. The daemon attributes the completion to the delivery whose
// turn is running (internal/api's TestHandoffCompleteCarriesTheRunOfTheTaskBeingWorked), so the
// CLI sends exactly the fields the grant does not already carry.
func TestHandoffCompleteSendsNoGeneration(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("LEGION_ROLE", "merger")
	t.Setenv("LEGION_GENERATION", "7")
	t.Setenv("LEGION_JJ_PATH", fakeHandoffJJ(t, "beef"))
	bodies := handoffDaemon(t, phase.Merging)
	var out, errb bytes.Buffer

	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb); code != 0 {
		t.Fatalf("handoff complete = %d, stderr %q", code, errb.String())
	}

	if len(*bodies) != 1 {
		t.Fatalf("daemon read %v, want one completion", *bodies)
	}
	if _, sent := (*bodies)[0]["generation"]; sent {
		t.Fatalf("the completion carries %v, want no generation: the daemon reads the run from the delivery", (*bodies)[0]["generation"])
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
	for _, tc := range []struct {
		role, phase, verdict string
		current              phase.Phase
	}{
		{"planner", "plan", "", phase.Planning},
		{"implementer", "implement", "", phase.Implementing},
		{"tester", "test", "pass", phase.Testing},
		{"reviewer", "review", "", phase.Reviewing},
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
			bodies := handoffDaemon(t, tc.current)
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
	bodies := handoffDaemon(t, phase.Implementing)
	errb.Reset()
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "implemented"}, &out, &errb)
	if code != 1 || len(*bodies) != 0 {
		t.Fatalf("handoff complete with this phase's handoff uncommitted = %d, daemon read %v, stderr %q; want a refusal before any request", code, *bodies, errb.String())
	}
}

// --workspace names the pane workspace from any directory. A handoff committed there completes the
// phase whatever the caller's working directory: the committed-handoff check must not resolve the
// handoff path against the caller's directory.
func TestHandoffCompleteWithWorkspaceFlagIgnoresTheCallersDirectory(t *testing.T) {
	workspace, jj := handoffRepo(t)
	writeHandoffFile(t, workspace, "implement.json", `{"issue":"THIS-1"}`+"\n")
	handoffJJ(t, jj, workspace, "commit", "-m", "implement: record handoff")
	carrying := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
	t.Chdir(t.TempDir())
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t, phase.Implementing)
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
	bodies := handoffDaemon(t, phase.Implementing)
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "implemented"}, &out, &errb)
	if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), "only the base branch carries it") {
		t.Fatalf("handoff complete with only origin's main carrying .legion/implement.json = %d, daemon read %v, stderr %q; want the inherited-handoff refusal before any request", code, *bodies, errb.String())
	}
}

// The implementer's production check writes no handoff: skills/legion-worker/SKILL.md's completion
// gate says the post-merge production check "writes no .legion/<phase>.json, commits no handoff,
// and reports with `handoff_complete` alone", and the daemon's workflow does not treat
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
	t.Setenv("LEGION_JJ_PATH", jj)
	bodies := handoffDaemon(t, phase.ProductionCheck)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--summary", "production check verified"}, &out, &errb); code != 0 {
		t.Fatalf("implementer handoff complete in production_check on the merged main = %d, stderr %q", code, errb.String())
	}
	if len(*bodies) != 1 {
		t.Fatalf("daemon read %v, want one production-check completion", *bodies)
	}
}

// The phase decides what a completion reports, and it is read before anything else. Retro writes
// no handoff, so the implementer's retro reports the commit its workspace stands on, never the
// commit that carried its last implementing handoff: that one was already reported, and the same
// commit in a file-backed phase would be refused as not new. A role reporting a phase it does not
// run — a tester whose issue moved on to reviewing — is not told to write another role's handoff:
// its completion reaches the daemon, which answers whose phase it is.
func TestHandoffCompleteReadsThePhaseBeforeTheHandoff(t *testing.T) {
	for _, tc := range []struct {
		name, role string
		current    phase.Phase
	}{
		{name: "the implementer's retro", role: "implementer", current: phase.Retro},
		{name: "a tester after its phase", role: "tester", current: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, jj := handoffRepo(t)
			t.Chdir(workspace)
			writeHandoffFile(t, workspace, "implement.json", `{"issue":"THIS-1"}`+"\n")
			handoffJJ(t, jj, workspace, "commit", "-m", "implement: record handoff")
			if err := os.WriteFile(filepath.Join(workspace, "docs.md"), []byte("learning\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			handoffJJ(t, jj, workspace, "commit", "-m", "docs: the retro's learning")
			standing := handoffJJ(t, jj, workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id")
			t.Setenv("LEGION_ROLE", tc.role)
			t.Setenv("LEGION_JJ_PATH", jj)
			bodies := handoffDaemon(t, tc.current)
			args := []string{"legion", "handoff", "complete", "--summary", "done"}
			if tc.role == "tester" {
				args = append(args, "--verdict", "pass")
			}
			var out, errb bytes.Buffer
			if code := run(context.Background(), args, &out, &errb); code != 0 {
				t.Fatalf("%s handoff complete in %s = %d, stderr %q", tc.role, tc.current, code, errb.String())
			}
			if len(*bodies) != 1 || (*bodies)[0]["commit"] != standing {
				t.Fatalf("daemon read %v, want one completion naming %s, the commit the workspace stands on", *bodies, standing)
			}
		})
	}
}

// A handoff is written under its phase word, the one every role prompt passes to the legion tool's
// handoff_write (plan, implement, test, review, and the sub-architect's architect), and a pane's
// LEGION_ROLE is its claim role. A role word as a phase would write a file nothing reads, and a
// phase word as the role names no claim, so each is refused.
func TestHandoffTakesPhaseWordsForPhasesAndRolesForRoles(t *testing.T) {
	workspace := t.TempDir()
	var out, errb bytes.Buffer
	for _, args := range [][]string{
		{"write", "--phase", "planner", "--data", `{"summary":"done"}`},
		{"write", "--phase", "merge", "--data", `{"summary":"done"}`},
		{"read", "--phase", "implementer"},
	} {
		errb.Reset()
		if code := run(context.Background(), append([]string{"legion", "handoff", args[0], "--workspace", workspace}, args[1:]...), &out, &errb); code != 2 {
			t.Fatalf("legion handoff %v = %d, stderr %q; want the usage refusal", args, code, errb.String())
		}
	}
	if entries, err := os.ReadDir(filepath.Join(workspace, ".legion")); !os.IsNotExist(err) {
		t.Fatalf(".legion after the refused writes: %v %v, want none", entries, err)
	}
	t.Setenv("LEGION_ROLE", "test")
	bodies := handoffDaemon(t, phase.Testing)
	errb.Reset()
	if code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "done", "--verdict", "pass"}, &out, &errb); code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), "LEGION_ROLE") {
		t.Fatalf("handoff complete with LEGION_ROLE=test = %d, daemon read %v, stderr %q; want a refusal naming LEGION_ROLE", code, *bodies, errb.String())
	}
}

// The end game every clean review round ends in (skills/legion-worker/references/merge-gate.md: the reviewer
// approves only a head that carries no .legion/, and the implementer pushes the .legion/
// deletion): once the branch head has deleted .legion/, the implementer and the tester report
// completion without recreating it (skills/legion-worker/SKILL.md's completion gate: once .legion/ is gone, "a
// later rebase, bare-gate re-check, confirmation, retro, or the post-merge production check writes
// no .legion/<phase>.json, commits no handoff, and reports with `handoff_complete` alone";
// packages/pi-envoy/roles/implementer.md and tester.md say the same). The commit that deleted the
// handoff is the last commit on the branch that changed it, and the completion reports it.
func TestHandoffCompleteAfterTheLegionDeletionRecreatesNothing(t *testing.T) {
	for _, tc := range []struct {
		role, verdict string
		current       phase.Phase
	}{
		{"implementer", "", phase.Implementing},
		{"tester", "pass", phase.Testing},
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
			bodies := handoffDaemon(t, tc.current)
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
			bodies := handoffDaemon(t, phase.Testing)
			// After the harness, which clears whatever identity the calling shell exported.
			t.Setenv("JJ_USER", "legion-reviewer[bot]")
			t.Setenv("JJ_EMAIL", "bot@example.invalid")
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

// A merger's READY names a head a human merges, which GitHub merges only once every check the base
// branch requires has succeeded there. A head whose push skipped CI when it should not have reports
// none of them, so the completion refuses READY naming the head and the check, and nothing reaches
// the daemon; a required check still running, or one that failed, is refused the same way.
//
// A pull request that conflicts with its base (mergeable_state "dirty") gets no pull_request run,
// so a required check with no result on its head is refused naming the conflict rather than a
// skipped push. The conflict changes only that text, never which heads are refused: every row is
// posted or refused by its checks alone, whatever its mergeable_state, and a conflicting head
// whose required checks all succeeded is posted.
func TestHandoffCompleteReadyRefusesAHeadWithoutItsRequiredChecksGreen(t *testing.T) {
	const (
		ciGreen      = `{"id":1,"name":"ci","status":"completed","conclusion":"success"}`
		ciSkipped    = `{"id":1,"name":"ci","status":"completed","conclusion":"skipped"}`
		ciRunning    = `{"id":1,"name":"ci","status":"in_progress","conclusion":null}`
		legacyGreen  = `{"context":"legacy","state":"success"}`
		legacyFailed = `{"context":"legacy","state":"failure"}`
		skippedPush  = `: its push may have skipped CI`
		conflict     = `: the pull request conflicts with main, and GitHub starts no pull_request CI`
	)
	for _, tc := range []struct {
		name, checkRun, status, mergeableState, refusal string
	}{
		{"every required check green", ciGreen, legacyGreen, "clean", ""},
		{"a required check that ended skipped counts", ciSkipped, legacyGreen, "clean", ""},
		{"every required check green on a conflicting pull request", ciGreen, legacyGreen, "dirty", ""},
		{"a head whose push skipped CI", "", "", "blocked", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + skippedPush},
		{"a head whose mergeability GitHub has not computed", "", "", "unknown", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + skippedPush},
		{"a head of a conflicting pull request", "", "", "dirty", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + conflict},
		{"a conflicting pull request missing one required check", ciGreen, "", "dirty", `head c0de00000000 of pull request #42 has no result for the required check "legacy"` + conflict},
		{"a required check still running", ciRunning, legacyGreen, "blocked", `the required check "ci" is still running on head c0de00000000`},
		{"a required check still running on a conflicting pull request", ciRunning, legacyGreen, "dirty", `the required check "ci" is still running on head c0de00000000`},
		{"a required status that failed", ciGreen, legacyFailed, "blocked", `the required check "legacy" ended failure on head c0de00000000`},
		{"a required status that failed on a conflicting pull request", ciGreen, legacyFailed, "dirty", `the required check "legacy" ended failure on head c0de00000000`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			t.Setenv("LEGION_ROLE", "merger")
			t.Setenv("LEGION_JJ_PATH", fakeHandoffJJ(t, "beef"))
			bodies := handoffDaemon(t, phase.Merging)
			readyGitHub(t, tc.checkRun, tc.status, tc.mergeableState)
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb)
			if tc.refusal == "" {
				if code != 0 || len(*bodies) != 1 {
					t.Fatalf("READY = %d, daemon read %v, stderr %q; want it posted", code, *bodies, errb.String())
				}
				return
			}
			if code != 1 || len(*bodies) != 0 {
				t.Fatalf("READY = %d, daemon read %v, stderr %q; want it refused and nothing posted", code, *bodies, errb.String())
			}
			if !strings.Contains(errb.String(), "READY refused: "+tc.refusal) {
				t.Fatalf("READY refused with stderr %q; want the refusal to name %q", errb.String(), tc.refusal)
			}
		})
	}
}

// A private repository whose plan has no rulesets answers the rulesets read 403, "make this
// repository public to enable this feature" (docs/solutions/legion/controller-gate-2-required-checks-live-reads.md).
// It can define no ruleset, so none requires a check there, and READY rests on the branch's
// protection alone: posted when that requires nothing, refused when it requires a check the head
// lacks. Only that answer means no rulesets: a rulesets read that fails otherwise (another 403,
// such as a token that lost access, or a server error) leaves the required checks unknown, and
// READY is refused naming the read.
func TestHandoffCompleteReadyOnARepositoryWhosePlanHasNoRulesets(t *testing.T) {
	const planAnswer = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.","status":"403"}`
	unprotected := `{"name":"main","protected":false}`
	for _, tc := range []struct {
		name, branch string
		rulesStatus  int
		rulesBody    string
		posted       bool
		refusal      string
	}{
		{"and no branch protection", unprotected, http.StatusForbidden, planAnswer, true, ""},
		{"and branch protection requiring a check the head lacks", `{"name":"main","protected":true,"protection":{"required_status_checks":{"contexts":["legacy"]}}}`, http.StatusForbidden, planAnswer, false, `has no result for the required check "legacy"`},
		{"but the read is refused for another reason", unprotected, http.StatusForbidden, `{"message":"Resource not accessible by integration","status":"403"}`, false, "GET /rules/branches/main with 403"},
		{"but the read fails", unprotected, http.StatusInternalServerError, `{"message":"Server Error"}`, false, "GET /rules/branches/main with 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := "/repos/acme/widgets"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case repo + "/pulls/42":
					_, _ = w.Write([]byte(`{"head":{"sha":"c0de0000000000000000000000000000000000ff"},"base":{"ref":"main"}}`))
				case repo + "/rules/branches/main":
					w.WriteHeader(tc.rulesStatus)
					_, _ = w.Write([]byte(tc.rulesBody))
				case repo + "/branches/main":
					_, _ = w.Write([]byte(tc.branch))
				case repo + "/commits/c0de0000000000000000000000000000000000ff/check-runs":
					_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
				case repo + "/commits/c0de0000000000000000000000000000000000ff/status":
					_, _ = w.Write([]byte(`{"statuses":[]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			workspace := t.TempDir()
			t.Setenv("LEGION_ROLE", "merger")
			t.Setenv("LEGION_JJ_PATH", fakeHandoffJJ(t, "beef"))
			bodies := handoffDaemon(t, phase.Merging)
			t.Setenv("LEGION_GITHUB_API_URL", server.URL)
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb)
			if tc.posted {
				// A base branch requiring no check has nothing to refuse, and READY says so rather
				// than reading like a head whose every required check was read and passed.
				if code != 0 || len(*bodies) != 1 || !strings.Contains(out.String(), `no check is required on "main" of acme/widgets`) {
					t.Fatalf("READY = %d, daemon read %v, stdout %q, stderr %q; want it posted, saying it read no checks", code, *bodies, out.String(), errb.String())
				}
				return
			}
			if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), tc.refusal) {
				t.Fatalf("READY = %d, daemon read %v, stderr %q; want a refusal naming %q", code, *bodies, errb.String(), tc.refusal)
			}
		})
	}
}
