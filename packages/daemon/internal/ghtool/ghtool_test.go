package ghtool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// helpDir is gh 2.98.0's saved help, beside `legion gh`'s tests (cmd/legion/testdata/gh-help).
func helpDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "cmd", "legion", "testdata", "gh-help"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeTokens mints a lease per App role and owner, a read-only one marked as such, as
// appauth.Manager does, and records what it was asked.
type fakeTokens struct {
	mu    sync.Mutex
	asked []string
}

func (f *fakeTokens) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return f.lease("ghs_"+string(role)+"_"+owner, role, owner)
}

func (f *fakeTokens) ReadToken(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return f.lease("ghs_"+string(role)+"_"+owner+"_read", role, owner)
}

func (f *fakeTokens) lease(token string, role appauth.AppRole, owner string) (appauth.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, token)
	return appauth.Lease{Token: token, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// fakeRecorder is the call record in memory, with a hook each start and finish calls.
type fakeRecorder struct {
	mu       sync.Mutex
	rows     map[string]store.AnswerToolCall
	onStart  func(store.AnswerToolCall)
	onFinish func(question string, seq int)
}

func newFakeRecorder() *fakeRecorder { return &fakeRecorder{rows: map[string]store.AnswerToolCall{}} }

func callKey(question string, seq int) string { return fmt.Sprintf("%s.%d", question, seq) }

func (f *fakeRecorder) StartAnswerToolCall(_ context.Context, call store.AnswerToolCall) error {
	f.mu.Lock()
	if _, ok := f.rows[callKey(call.Question, call.Seq)]; ok {
		f.mu.Unlock()
		return fmt.Errorf("gh call %s.%d %w", call.Question, call.Seq, store.ErrAnswerToolCallRecorded)
	}
	f.rows[callKey(call.Question, call.Seq)] = call
	f.mu.Unlock()
	if f.onStart != nil {
		f.onStart(call)
	}
	return nil
}

func (f *fakeRecorder) FinishAnswerToolCall(_ context.Context, question string, seq int, result string, exit *int, at time.Time) error {
	if f.onFinish != nil {
		f.onFinish(question, seq)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[callKey(question, seq)]
	if !ok || row.Result != nil {
		return fmt.Errorf("finish gh call %s.%d: no started, unfinished call has that number", question, seq)
	}
	row.Result, row.Exit, row.FinishedAt = &result, exit, &at
	f.rows[callKey(question, seq)] = row
	return nil
}

func (f *fakeRecorder) AnswerToolCall(_ context.Context, question string, seq int) (store.AnswerToolCall, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[callKey(question, seq)]
	return row, ok, nil
}

func (f *fakeRecorder) row(question string, seq int) (store.AnswerToolCall, bool) {
	row, ok, _ := f.AnswerToolCall(context.Background(), question, seq)
	return row, ok
}

// fixture is one answer of an implementer on acme/widgets, in a project whose owners are acme and
// sjawhar, its gh the stand-in.
type fixture struct {
	t      *testing.T
	stand  string // the stand-in gh's directory
	host   Host
	tokens *fakeTokens
	calls  *fakeRecorder
	logs   *bytes.Buffer
	answer *Answer
}

const question = "dispatch:m-1"

var implementer = Agent{Issue: "LEG-1", Role: claim.RoleImplementer, Session: "s-1", Phase: "implement", Repository: ghrepo.MustParse("acme/widgets"), AppID: "1001"}

var bounds = config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 60000, GHCalls: 100}

// standInGH links this test binary as `gh` into a directory of its own, behaving as b says.
func standInGH(t *testing.T, b behaviour) string {
	t.Helper()
	dir := t.TempDir()
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(dir, "gh")); err != nil {
		t.Fatal(err)
	}
	setBehaviour(t, dir, b)
	return dir
}

func setBehaviour(t *testing.T, dir string, b behaviour) {
	t.Helper()
	b.HelpDir = helpDir(t)
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "behaviour.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T, agent Agent, b behaviour) *fixture {
	t.Helper()
	f := &fixture{t: t, stand: standInGH(t, b), tokens: &fakeTokens{}, calls: newFakeRecorder(), logs: &bytes.Buffer{}}
	root := t.TempDir()
	f.host = Host{
		GH: filepath.Join(f.stand, "gh"), Git: filepath.Join(root, "gitbin", "git"), Legion: filepath.Join(root, "legion"),
		StateDir: filepath.Join(root, "state"), Calls: f.calls, Log: slog.New(slog.NewJSONHandler(f.logs, nil)), Now: time.Now,
		Credential: appauth.GitHubCredential(f.tokens, func() []string { return []string{"acme", "Sjawhar"} }),
	}
	answer, err := Open(context.Background(), f.host, agent, question)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = answer.Close() })
	f.answer = answer
	return f
}

func args(argv ...string) string {
	data, err := json.Marshal(argv)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func (f *fixture) run(seq int, argv ...string) Result {
	return Run(context.Background(), Call{Answer: f.answer, Seq: seq, Args: args(argv...), Bounds: bounds})
}

// invocations are the stand-in's runs so far: helps says whether to include its help reads.
func (f *fixture) invocations(helps bool) []invocation {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.stand, "invocations.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var all []invocation
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var inv invocation
		if err := json.Unmarshal(scanner.Bytes(), &inv); err != nil {
			f.t.Fatal(err)
		}
		if helps || !inv.Help {
			all = append(all, inv)
		}
	}
	return all
}

func (f *fixture) clearInvocations() {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.stand, "invocations.jsonl")); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
}

// logLines are the JSON log lines with msg.
func (f *fixture) logLines(msg string) []map[string]any {
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == msg {
			lines = append(lines, entry)
		}
	}
	return lines
}

// wantEnv is exactly the environment a call's gh runs with, its GH_CONFIG_DIR owner's.
func (f *fixture) wantEnv(owner string) []string {
	a := f.answer
	return slices.Sorted(slices.Values([]string{
		"PATH=" + f.stand + ":" + filepath.Dir(f.host.Git),
		"HOME=" + a.home, "XDG_CONFIG_HOME=" + a.config, "XDG_CACHE_HOME=" + a.cache, "XDG_DATA_HOME=" + a.data, "XDG_STATE_HOME=" + a.state,
		"GH_CONFIG_DIR=" + filepath.Join(a.dir, "owners", owner), "GH_PATH=" + filepath.Join(a.dir, "bin", "gh"),
		"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1", "NO_COLOR=1",
	}))
}

// Open writes the agent's bundle as a pane's is written: the role's App (implement for an
// implementer, review for a reviewer) with the write lease for the issue's owner, the default, and
// a read-only lease for every other owner of the project, under a fresh 0700
// <state_dir>/answers/<question>/gh, what an earlier run left there gone; Close removes it.
func TestOpenWritesTheAgentsBundleAsAPanesAndCloseRemovesIt(t *testing.T) {
	for _, tc := range []struct {
		agent Agent
		app   string
	}{
		{implementer, "implement"},
		{Agent{Issue: "LEG-1", Role: claim.RoleReviewer, Session: "s-2", Phase: "review", Repository: ghrepo.MustParse("Acme/widgets")}, "review"},
	} {
		stale := filepath.Join(t.TempDir(), "state")
		dir := filepath.Join(stale, "answers", "dispatch:m-1", "gh")
		if err := os.MkdirAll(filepath.Join(dir, "leftover"), 0o755); err != nil {
			t.Fatal(err)
		}
		tokens := &fakeTokens{}
		host := Host{
			GH: "/opt/gh/gh", Legion: "/opt/legion/legion", StateDir: stale, Calls: newFakeRecorder(), Log: slog.New(slog.DiscardHandler), Now: time.Now,
			Credential: appauth.GitHubCredential(tokens, func() []string { return []string{"acme", "Sjawhar"} }),
		}
		answer, err := Open(context.Background(), host, tc.agent, question)
		if err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: the answer's directory %s = %v, %v; want 0700", tc.app, dir, info, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "leftover")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: what an earlier run left in %s is still there (%v)", tc.app, dir, err)
		}
		for owner, want := range map[string]string{"acme": "ghs_" + tc.app + "_acme", "sjawhar": "ghs_" + tc.app + "_sjawhar_read"} {
			hosts, err := os.ReadFile(filepath.Join(dir, "owners", owner, ghconfig.HostsFile))
			if err != nil {
				t.Fatal(err)
			}
			if token, err := ghconfig.TokenFromHosts(hosts); err != nil || token != want {
				t.Errorf("%s: %s's gh directory holds %q, %v; want %q", tc.app, owner, token, err, want)
			}
		}
		if answer.bundle.Default != "acme" {
			t.Errorf("%s: the bundle's default owner is %q, want the issue's owner acme", tc.app, answer.bundle.Default)
		}
		if err := answer.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: after Close %s = %v, want it gone", tc.app, dir, err)
		}
	}
}

// Every refused form never starts gh: a command outside the allow list, an option opening a
// browser, gh api's --hostname, and every write form whose text the tool cannot add the footer to;
// arguments that are no JSON array of strings. A command whose owner cannot be read for certain or
// is none of the project's is refused once gh's help is read, the command itself never run. Each
// refusal names what it refuses, is what the model reads, and is recorded with no exit.
func TestRefusedFormsNeverRun(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "ran"})
	for i, tc := range []struct {
		argv     []string
		raw      string // the call's arguments as given, when no JSON array
		want     string
		readHelp bool // refused by the routing, after reading gh's help
	}{
		{argv: []string{"extension", "install", "owner/gh-x"}, want: `"extension" is refused`},
		{argv: []string{"alias", "set", "co", "pr checkout"}, want: `"alias" is refused`},
		{argv: []string{"config", "get", "git_protocol"}, want: `"config" is refused`},
		{argv: []string{"auth", "token"}, want: `"auth" is refused`},
		{argv: []string{"codespace", "list"}, want: `"codespace" is refused`},
		{argv: []string{"browse", "-R", "acme/widgets"}, want: `"browse" is refused`},
		{argv: []string{"repo", "clone", "acme/widgets"}, want: `"repo clone" is refused`},
		{argv: []string{"repo", "list"}, want: `"repo list" is refused`},
		{argv: []string{"repo"}, want: `"repo" is refused`},
		{argv: []string{"pr", "checkout", "5", "-R", "acme/widgets"}, want: `"checkout" is refused`},
		{argv: []string{"-R", "acme/widgets", "pr", "co", "5"}, want: `"co" is refused`},
		{argv: []string{"gist", "list"}, want: `"gist" is refused`},
		{argv: []string{"secret", "list", "-R", "acme/widgets"}, want: `"secret" is refused`},
		{argv: []string{"ssh-key", "list"}, want: `"ssh-key" is refused`},
		{argv: []string{"gpg-key", "list"}, want: `"gpg-key" is refused`},
		{argv: []string{"release", "download", "v1", "-R", "acme/widgets"}, want: `"release download" is refused`},
		{argv: []string{"release", "upload", "v1", "a.tgz", "-R", "acme/widgets"}, want: `"release upload" is refused`},
		{argv: []string{"release", "create", "v1", "-R", "acme/widgets"}, want: `"release create" is refused`},
		{argv: []string{"run", "download", "7", "-R", "acme/widgets"}, want: `"download" is refused`},
		{argv: []string{"issue", "create", "-R", "acme/widgets", "--title", "t", "--body", "b"}, want: `"issue create" is refused`},
		{argv: []string{"co", "5"}, want: `"co" is refused`},
		{argv: []string{"help", "pr"}, want: `"help" is refused`},
		{argv: []string{"--version"}, want: `"--version" is refused`},
		{argv: []string{}, want: "names no gh command"},
		{argv: []string{"pr", "view", "5", "-R", "acme/widgets", "--web"}, want: `"--web" opens a web browser`},
		{argv: []string{"pr", "view", "5", "-R", "acme/widgets", "-w"}, want: `"-w" opens a web browser`},
		{argv: []string{"run", "view", "7", "-R", "acme/widgets", "--web=true"}, want: "opens a web browser"},
		{argv: []string{"api", "--hostname", "ghe.example.com", "repos/acme/widgets"}, want: "--hostname sends the request to another host"},
		{argv: []string{"pr", "comment", "5", "-R", "acme/widgets", "--body-file", "body.md"}, want: "use --body||pass the text itself with --body"},
		{argv: []string{"pr", "comment", "5", "-R", "acme/widgets", "-F", "body.md"}, want: "pass the text itself with --body"},
		{argv: []string{"issue", "comment", "3", "-R", "acme/widgets", "-Fbody.md"}, want: "pass the text itself with --body"},
		{argv: []string{"pr", "review", "5", "-R", "acme/widgets", "--approve", "-F", "-"}, want: "pass the text itself with --body"},
		{argv: []string{"pr", "comment", "5", "-R", "acme/widgets", "--editor"}, want: "--editor opens an editor"},
		{argv: []string{"pr", "comment", "5", "-R", "acme/widgets", "-e"}, want: "--editor opens an editor"},
		{argv: []string{"api", "repos/acme/widgets/issues/5/comments", "-F", "body=@body.md"}, want: `"body=@body.md" reads a field's value from a file`},
		{argv: []string{"api", "repos/acme/widgets/issues/5/comments", "--input", "body.json"}, want: "--input reads the request's body from a file"},
		{argv: []string{"api", "graphql", "-f", "query=mutation { addComment(input: {subjectId: \"x\", body: \"y\"}) { clientMutationId } }"}, want: "make the write through its REST endpoint"},
		{argv: []string{"workflow", "run", "ci.yml", "-R", "acme/widgets", "-F", "token=@/etc/passwd"}, want: "reads a field's value from a file"},
		{argv: []string{"pr", "create", "-R", "acme/widgets", "--recover", "state.json"}, want: "reads a file on the daemon's host"},
		{raw: `pr view 5`, want: "takes gh's arguments as a JSON array of strings"},
		{raw: `["pr", 5]`, want: "takes gh's arguments as a JSON array of strings"},
		{argv: []string{"api", "repos/other/thing"}, want: `"other" is not an owner of this project`, readHelp: true},
		{argv: []string{"pr", "view", "5", "-R", "acme/widgets", "--bogus"}, want: `"--bogus" is no option`, readHelp: true},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1", "-R", "acme/widgets"}, want: "more than one owner", readHelp: true},
	} {
		seq := i + 1
		f.clearInvocations()
		raw := tc.raw
		if raw == "" {
			raw = args(tc.argv...)
		}
		got := Run(context.Background(), Call{Answer: f.answer, Seq: seq, Args: raw, Bounds: bounds})
		ok := slices.ContainsFunc(strings.Split(tc.want, "||"), func(want string) bool { return strings.Contains(got.Output, want) })
		if got.Ran || got.Exit != -1 || !strings.HasPrefix(got.Output, "refused: ") || !ok {
			t.Errorf("gh %s = %+v; want a refusal containing %q, gh never run", raw, got, tc.want)
		}
		if ran := f.invocations(false); len(ran) != 0 {
			t.Errorf("gh %s ran gh: %+v", raw, ran)
		}
		if helps := f.invocations(true); !tc.readHelp && len(helps) != 0 {
			t.Errorf("gh %s started gh to read its help (%+v); a command the tool refuses by its words starts nothing", raw, helps)
		}
		row, recorded := f.calls.row(question, seq)
		if !recorded || row.Result == nil || *row.Result != got.Output || row.Exit != nil || row.FinishedAt == nil {
			t.Errorf("gh %s is recorded as %+v (%t); want its refusal and no exit", raw, row, recorded)
		}
	}
}

// Every allowed form runs once, with exactly its arguments, the gh directory of the owner it
// names (the issue owner's for a command naming none), exactly the scrubbed environment, and the
// answer's empty working directory, HOME and XDG directories. Its record starts before gh runs
// and finishes after, with gh's output and exit, and one log line names its App and owner.
func TestAllowedFormsRunWithTheRoutedOwnerAndTheScrubbedEnvironment(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "gh's answer\n"})
	ranAtStart := map[int]int{}
	f.calls.onStart = func(call store.AnswerToolCall) { ranAtStart[call.Seq] = len(f.invocations(false)) }
	ranAtFinish := map[int]int{}
	f.calls.onFinish = func(_ string, seq int) { ranAtFinish[seq] = len(f.invocations(false)) }
	for i, tc := range []struct {
		argv  []string
		owner string
	}{
		{[]string{"pr", "view", "5", "-R", "sjawhar/legion", "--json", "title"}, "sjawhar"},
		{[]string{"-R", "acme/widgets", "pr", "ls"}, "acme"},
		{[]string{"pr", "diff", "5", "-R", "acme/widgets"}, "acme"},
		{[]string{"pr", "checks", "5", "-R", "acme/widgets"}, "acme"},
		{[]string{"api", "repos/sjawhar/legion/contents/README.md"}, "sjawhar"},
		{[]string{"api", "user"}, "acme"},
		{[]string{"search", "issues", "repo:sjawhar/legion", "is:open"}, "sjawhar"},
		{[]string{"run", "list", "-R", "acme/widgets"}, "acme"},
		{[]string{"run", "view", "7", "-R", "acme/widgets", "--log-failed"}, "acme"},
		{[]string{"workflow", "list", "-R", "acme/widgets"}, "acme"},
		{[]string{"repo", "view", "sjawhar/legion", "--json", "name"}, "sjawhar"},
		{[]string{"release", "list", "-R", "sjawhar/legion"}, "sjawhar"},
		{[]string{"release", "view", "v1", "-R", "sjawhar/legion"}, "sjawhar"},
		{[]string{"issue", "list", "-R", "acme/widgets"}, "acme"},
		{[]string{"issue", "view", "3", "-R", "acme/widgets"}, "acme"},
		{[]string{"label", "list", "-R", "acme/widgets"}, "acme"},
		{[]string{"cache", "list", "-R", "acme/widgets"}, "acme"},
		// Writes that post no comment or review run as they are.
		{[]string{"pr", "merge", "5", "-R", "acme/widgets", "--squash"}, "acme"},
		{[]string{"run", "rerun", "7", "-R", "acme/widgets", "--failed"}, "acme"},
		{[]string{"workflow", "run", "ci.yml", "-R", "acme/widgets", "-f", "level=1"}, "acme"},
		{[]string{"api", "repos/acme/widgets/issues/5/comments"}, "acme"},
	} {
		seq := i + 1
		f.clearInvocations()
		got := f.run(seq, tc.argv...)
		if !got.Ran || got.Exit != 0 || got.Output != "gh's answer\n" {
			t.Errorf("gh %q = %+v; want gh's answer, exit 0", tc.argv, got)
			continue
		}
		ran := f.invocations(false)
		if len(ran) != 1 {
			t.Fatalf("gh %q ran gh %d times: %+v", tc.argv, len(ran), ran)
		}
		inv := ran[0]
		if !slices.Equal(inv.Argv, tc.argv) {
			t.Errorf("gh %q ran gh with %q, want its arguments as given", tc.argv, inv.Argv)
		}
		if want := f.wantEnv(tc.owner); !slices.Equal(inv.Env, want) {
			t.Errorf("gh %q ran with the environment\n%q\nwant exactly\n%q", tc.argv, inv.Env, want)
		}
		if inv.Cwd != f.answer.cwd {
			t.Errorf("gh %q ran in %s, want the answer's working directory %s", tc.argv, inv.Cwd, f.answer.cwd)
		}
		for name, empty := range inv.Empty {
			if !empty {
				t.Errorf("gh %q ran with %s not empty", tc.argv, name)
			}
		}
		if ranAtStart[seq] != 0 || ranAtFinish[seq] != 1 {
			t.Errorf("gh %q: gh had run %d times when its record started and %d when it finished; want 0 and 1", tc.argv, ranAtStart[seq], ranAtFinish[seq])
		}
		row, _ := f.calls.row(question, seq)
		if row.Result == nil || *row.Result != got.Output || row.Exit == nil || *row.Exit != 0 || row.Owner != tc.owner || row.App != "implement" || !slices.Equal(row.Args, tc.argv) {
			t.Errorf("gh %q is recorded as %+v; want its output, exit 0, App implement and owner %s", tc.argv, row, tc.owner)
		}
	}
	var logged bool
	for _, line := range f.logLines("answers: gh") {
		if line["seq"] == float64(1) {
			logged = true
			if line["question"] != question || line["app"] != "implement" || line["appID"] != "1001" || line["owner"] != "sjawhar" || line["exit"] != float64(0) || fmt.Sprint(line["args"]) != fmt.Sprint([]any{"pr", "view", "5", "-R", "sjawhar/legion", "--json", "title"}) {
				t.Errorf("the log line of call 1 = %v; want its question, App implement 1001, owner sjawhar, arguments and exit 0", line)
			}
		}
	}
	if !logged {
		t.Errorf("no `answers: gh` line for call 1 in %s", f.logs)
	}
}

// The tool adds the agent's footer to every comment or review it posts: pr and issue comment's
// --body in each spelling, a review's --body, a bare --approve, --request-changes or --comment as
// --body with the footer alone, a closing or reopening --comment, a gh api write's literal body
// field, and `-f body=<footer>` on a POST creating or submitting a review with none. The review
// intake still reads the agent's session from it.
func TestTheFooterIsAddedToEveryWriteForm(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	foot := func(seq int) string {
		return `<!-- legion: {"session":"s-1","phase":"implement","answer":"dispatch:m-1.` + fmt.Sprint(seq) + `"} -->`
	}
	repo := []string{"-R", "acme/widgets"}
	for i, tc := range []struct {
		argv []string
		want func(foot string) []string
	}{
		{append([]string{"pr", "comment", "5", "--body", "Done."}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "--body", "Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "-b", "Done."}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "-b", "Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "--body=Done."}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "--body=Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "-bDone."}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "-bDone.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "--edit-last", "--body", "Edited."}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "--edit-last", "--body", "Edited.\n\n" + s}, repo...)
		}},
		{append([]string{"issue", "comment", "3", "--body", "Done."}, repo...), func(s string) []string {
			return append([]string{"issue", "comment", "3", "--body", "Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "review", "5", "--approve", "--body", "LGTM"}, repo...), func(s string) []string {
			return append([]string{"pr", "review", "5", "--approve", "--body", "LGTM\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "review", "5", "--approve"}, repo...), func(s string) []string {
			return append(append([]string{"pr", "review", "5", "--approve"}, repo...), "--body", s)
		}},
		{append([]string{"pr", "review", "5", "--request-changes"}, repo...), func(s string) []string {
			return append(append([]string{"pr", "review", "5", "--request-changes"}, repo...), "--body", s)
		}},
		{append([]string{"pr", "review", "5", "-c"}, repo...), func(s string) []string {
			return append(append([]string{"pr", "review", "5", "-c"}, repo...), "--body", s)
		}},
		{append([]string{"pr", "close", "5", "-c", "Superseded by #6."}, repo...), func(s string) []string {
			return append([]string{"pr", "close", "5", "-c", "Superseded by #6.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "reopen", "5", "--comment", "Back."}, repo...), func(s string) []string {
			return append([]string{"pr", "reopen", "5", "--comment", "Back.\n\n" + s}, repo...)
		}},
		{[]string{"api", "-X", "POST", "repos/acme/widgets/issues/5/comments", "-f", "body=Done."}, func(s string) []string {
			return []string{"api", "-X", "POST", "repos/acme/widgets/issues/5/comments", "-f", "body=Done.\n\n" + s}
		}},
		{[]string{"api", "repos/acme/widgets/issues/5/comments", "-F", "body=Done."}, func(s string) []string {
			return []string{"api", "repos/acme/widgets/issues/5/comments", "-F", "body=Done.\n\n" + s}
		}},
		{[]string{"api", "repos/acme/widgets/issues/5/comments", "--raw-field=body=Done."}, func(s string) []string {
			return []string{"api", "repos/acme/widgets/issues/5/comments", "--raw-field=body=Done.\n\n" + s}
		}},
		{[]string{"api", "-X", "PATCH", "repos/acme/widgets/issues/comments/7", "-f", "body=Edited."}, func(s string) []string {
			return []string{"api", "-X", "PATCH", "repos/acme/widgets/issues/comments/7", "-f", "body=Edited.\n\n" + s}
		}},
		{[]string{"api", "repos/acme/widgets/pulls/5/reviews", "-f", "event=APPROVE"}, func(s string) []string {
			return []string{"api", "repos/acme/widgets/pulls/5/reviews", "-f", "event=APPROVE", "-f", "body=" + s}
		}},
		{[]string{"api", "-X", "POST", "/repos/acme/widgets/pulls/5/reviews/9/events", "-f", "event=COMMENT"}, func(s string) []string {
			return []string{"api", "-X", "POST", "/repos/acme/widgets/pulls/5/reviews/9/events", "-f", "event=COMMENT", "-f", "body=" + s}
		}},
	} {
		seq := i + 1
		f.clearInvocations()
		got := f.run(seq, tc.argv...)
		want := tc.want(foot(seq))
		ran := f.invocations(false)
		if !got.Ran || len(ran) != 1 || !slices.Equal(ran[0].Argv, want) {
			t.Errorf("gh %q = %+v, ran %+v; want gh run once with %q", tc.argv, got, ran, want)
			continue
		}
		if env := f.wantEnv("acme"); !slices.Equal(ran[0].Env, env) {
			t.Errorf("gh %q ran with %q, want %q", tc.argv, ran[0].Env, env)
		}
		posted := want[slices.IndexFunc(want, func(arg string) bool { return strings.Contains(arg, "<!-- legion:") })]
		if session := (intake.PullRequestReview{Body: posted}).LegionSession(); session != "s-1" {
			t.Errorf("the review intake reads the session %q from %q, want s-1", session, posted)
		}
	}
}

// gh's stdout and stderr reach the model interleaved and cut at gh_output_chars characters, a
// multi-byte character counting once, with a note of how many were cut; a gh running past
// gh_seconds is stopped, whatever it printed so far kept with a note.
func TestOutputIsCutAndASlowGhIsStopped(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: strings.Repeat("é", 30), Stderr: "warning", Exit: 1})
	got := Run(context.Background(), Call{Answer: f.answer, Seq: 1, Args: args("api", "user"), Bounds: config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 10, GHCalls: 5}})
	if want := strings.Repeat("é", 10) + "\n[cut: 27 more characters]"; got.Output != want || got.Exit != 1 || !got.Ran {
		t.Errorf("a long answer = %+v; want %q, exit 1", got, want)
	}
	whole := Run(context.Background(), Call{Answer: f.answer, Seq: 2, Args: args("api", "user"), Bounds: bounds})
	if want := strings.Repeat("é", 30) + "warning"; whole.Output != want {
		t.Errorf("stdout and stderr read %q, want %q", whole.Output, want)
	}

	setBehaviour(t, f.stand, behaviour{Stdout: "partial", Sleep: time.Minute})
	start := time.Now()
	got = Run(context.Background(), Call{Answer: f.answer, Seq: 3, Args: args("api", "user"), Bounds: config.AnswerBounds{GHCall: 2 * time.Second, GHOutputChars: 100, GHCalls: 5}})
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("a gh sleeping a minute under a 2s bound returned after %s", elapsed)
	}
	if !got.Ran || !strings.Contains(got.Output, "[stopped: gh ran past its 2s bound (answers.bounds.gh_seconds)]") {
		t.Errorf("a slow gh = %+v; want it stopped with the note", got)
	}
	if row, _ := f.calls.row(question, 3); row.Result == nil || row.Exit == nil {
		t.Errorf("the stopped call is recorded as %+v; want it finished", row)
	}
}

// An answer makes at most gh_calls calls: the call past the bound is refused without gh starting.
func TestTheCallPastTheBoundIsRefused(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "ok"})
	capped := config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 100, GHCalls: 2}
	for seq := 1; seq <= 3; seq++ {
		f.clearInvocations()
		got := Run(context.Background(), Call{Answer: f.answer, Seq: seq, Args: args("api", "user"), Bounds: capped})
		switch {
		case seq <= 2 && !got.Ran:
			t.Errorf("call %d of 2 = %+v; want it run", seq, got)
		case seq == 3 && (got.Ran || !strings.Contains(got.Output, "this answer has made its 2 gh calls")):
			t.Errorf("call 3 of 2 = %+v; want it refused", got)
		case seq == 3 && len(f.invocations(true)) != 0:
			t.Errorf("call 3 of 2 started gh: %+v", f.invocations(true))
		}
	}
}

// GH_PATH names a wrapper running this binary's `legion gh`, so the helper gh names for its git
// (`<GH_PATH> auth git-credential`) is `legion gh auth git-credential`, told the real gh and the
// answer's bundle as a pane's environment tells them.
func TestGHPathRunsLegionGhOverTheAnswersBundle(t *testing.T) {
	f := newFixture(t, implementer, behaviour{})
	got := filepath.Join(t.TempDir(), "legion-ran")
	if err := os.WriteFile(f.host.Legion, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" \"$LEGION_GH_PATH\" \"$LEGION_GH_OWNERS_DIR\" \"$GH_CONFIG_DIR\" > '"+got+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(f.answer.wrapper, "auth", "git-credential", "get")
	cmd.Env = f.answer.env(filepath.Join(f.answer.owners, "acme"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the GH_PATH wrapper: %v: %s", err, out)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if want := "gh auth git-credential get\n" + f.host.GH + "\n" + f.answer.owners + "\n" + filepath.Join(f.answer.owners, "acme") + "\n"; string(data) != want {
		t.Errorf("the wrapper ran legion with %q, want %q", data, want)
	}
}

// A restart finds the comment or review a call with no recorded result posted, by its footer's
// answer key, on the pull request or issue the call wrote to, reading GitHub through the stand-in
// with the routed owner's directory; a call whose key is on none is not found. A finished call, an
// unrecorded one and one posting nothing are not looked up.
func TestLookupFindsTheCommentByItsAnswerKey(t *testing.T) {
	f := newFixture(t, implementer, behaviour{})
	foot := func(seq int) string { return footer(implementer, question, seq) }
	comments := fmt.Sprintf(`[[{"id":11,"html_url":"https://github.com/acme/widgets/pull/5#issuecomment-11","body":%q},{"id":12,"html_url":"https://github.com/acme/widgets/pull/5#issuecomment-12","body":%q}],[{"id":13,"html_url":"x","body":"no footer"}]]`,
		"Earlier.\n\n"+foot(1), "Done.\n\n"+foot(2))
	reviews := fmt.Sprintf(`[[{"id":21,"html_url":"https://github.com/acme/widgets/pull/5#pullrequestreview-21","body":%q}]]`, "Earlier review.\n\n"+foot(1))
	setBehaviour(t, f.stand, behaviour{Responses: map[string]string{
		"api repos/acme/widgets/issues/5/comments --paginate --slurp":  comments,
		"api repos/acme/widgets/pulls/5/reviews --paginate --slurp":    reviews,
		"pr view feature-x -R acme/widgets --json number --jq .number": "5\n",
	}})
	start := func(seq int, argv ...string) {
		if err := f.calls.StartAnswerToolCall(context.Background(), store.AnswerToolCall{Question: question, Seq: seq, Args: argv, App: "implement", Owner: "acme", StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	start(2, "pr", "comment", "5", "-R", "acme/widgets", "--body", "Done.\n\n"+foot(2))
	start(3, "pr", "review", "5", "-R", "acme/widgets", "--approve", "--body", foot(3))
	start(4, "pr", "comment", "feature-x", "-R", "acme/widgets", "--body", "Done.\n\n"+foot(4))
	start(5, "api", "repos/acme/widgets/issues/5/comments", "-f", "body=Done.\n\n"+foot(2))
	start(6, "pr", "view", "5", "-R", "acme/widgets")
	start(7, "pr", "comment", "https://github.com/acme/widgets/pull/5", "--body", "Done.\n\n"+foot(7))

	for _, tc := range []struct {
		seq  int
		want Found
	}{
		{2, Found{Exists: true, URL: "https://github.com/acme/widgets/pull/5#issuecomment-12", ID: 12}},
		{3, Found{}},
		{4, Found{}},
		{5, Found{}},
		{7, Found{}},
	} {
		f.clearInvocations()
		got, err := Lookup(context.Background(), f.answer, tc.seq, bounds)
		if err != nil || got != tc.want {
			t.Errorf("Lookup of call %d = %+v, %v; want %+v", tc.seq, got, err, tc.want)
		}
		for _, inv := range f.invocations(false) {
			if want := f.wantEnv("acme"); !slices.Equal(inv.Env, want) {
				t.Errorf("Lookup of call %d ran gh %q with %q, want %q", tc.seq, inv.Argv, inv.Env, want)
			}
		}
	}
	if _, err := Lookup(context.Background(), f.answer, 6, bounds); !errors.Is(err, ErrNotAWrite) {
		t.Errorf("Lookup of a pr view = %v, want ErrNotAWrite", err)
	}
	if _, err := Lookup(context.Background(), f.answer, 9, bounds); err == nil {
		t.Errorf("Lookup of an unrecorded call succeeded, want an error")
	}
	exit := 0
	if err := f.calls.FinishAnswerToolCall(context.Background(), question, 2, "posted", &exit, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup(context.Background(), f.answer, 2, bounds); err == nil || !strings.Contains(err.Error(), "has its result recorded") {
		t.Errorf("Lookup of a finished call = %v, want a refusal", err)
	}
	if row, _ := f.calls.row(question, 3); row.Result != nil {
		t.Errorf("Lookup wrote to the call record: %+v", row)
	}
}

// With the daemon's Postgres store, a call's row is written before gh runs, with no result, and
// finished after with gh's output and exit; a refused call's row has its refusal and no exit.
func TestCallsAreRecordedInPostgres(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, testpg.Database(t, "legion_ghtool"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, implementer, behaviour{Stdout: "gh's answer"})
	f.answer.host.Calls = checkedStore{Store: s, t: t, f: f}
	got := f.run(1, "pr", "view", "5", "-R", "sjawhar/legion")
	if !got.Ran {
		t.Fatalf("the call = %+v, want it run", got)
	}
	row, ok, err := s.AnswerToolCall(ctx, question, 1)
	if err != nil || !ok || row.Result == nil || *row.Result != "gh's answer" || row.Exit == nil || *row.Exit != 0 || row.FinishedAt == nil || row.Owner != "sjawhar" || row.App != "implement" {
		t.Errorf("the call's row = %+v, %t, %v; want it finished with gh's answer, exit 0, App implement, owner sjawhar", row, ok, err)
	}
	f.clearInvocations()
	f.run(2, "auth", "token")
	if row, ok, err := s.AnswerToolCall(ctx, question, 2); err != nil || !ok || row.Result == nil || !strings.Contains(*row.Result, `"auth" is refused`) || row.Exit != nil {
		t.Errorf("the refused call's row = %+v, %t, %v; want its refusal and no exit", row, ok, err)
	}
}

// checkedStore is the Postgres store, checking at each start that the row holds no result and gh
// has not run, and at each finish that gh has run.
type checkedStore struct {
	*store.Store
	t *testing.T
	f *fixture
}

func (c checkedStore) StartAnswerToolCall(ctx context.Context, call store.AnswerToolCall) error {
	if err := c.Store.StartAnswerToolCall(ctx, call); err != nil {
		return err
	}
	row, ok, err := c.Store.AnswerToolCall(ctx, call.Question, call.Seq)
	if err != nil || !ok || row.Result != nil || len(c.f.invocations(false)) != 0 {
		c.t.Errorf("at the start of call %d: row %+v, %t, %v and gh run %d times; want the row with no result and gh not run", call.Seq, row, ok, err, len(c.f.invocations(false)))
	}
	return nil
}

func (c checkedStore) FinishAnswerToolCall(ctx context.Context, question string, seq int, result string, exit *int, at time.Time) error {
	if exit != nil && len(c.f.invocations(false)) != 1 {
		c.t.Errorf("at the finish of call %d gh had run %d times, want once", seq, len(c.f.invocations(false)))
	}
	return c.Store.FinishAnswerToolCall(ctx, question, seq, result, exit, at)
}
