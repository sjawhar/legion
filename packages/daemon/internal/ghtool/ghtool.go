// Package ghtool is the `gh` tool of the daemon's answer loop: GitHub's own command line, run for
// an agent that is not running, with the GitHub access that agent's pane has. Per answer, Open
// writes the agent's credential bundle as a pane gets its own (its role's App with full access to
// its issue's repository owner, read-only to the project's other owners) and Close removes it;
// Run runs one call the model makes. A call runs only when it is one of the tool's commands (the
// allow list, checked before any process starts), routed to the owner it names as a pane's
// `legion gh` routes it (internal/ghroute), with the agent's Legion footer on every comment or
// review it posts, under a scrubbed environment, a time bound and an output bound, and recorded
// before it runs and finished after (answer_tool_calls). Lookup tells a restarted answer whether a
// comment or review call whose outcome was never recorded reached GitHub, by its footer.
package ghtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Agent is the stopped agent a call runs as: its issue, role, session and phase (the footer names
// the session and phase), the repository its issue targets, whose owner its role's App writes to,
// and the configured id of that App (config.GitHubApps), which the log line names beside its role.
type Agent struct {
	Issue      string
	Role       claim.Role
	Session    string
	Phase      string
	Repository ghrepo.Repository
	AppID      string
}

// App is the GitHub App the agent's role acts as: implement for an implementer or a merger, review
// for every other role (appauth.AppRoleFor).
func (a Agent) App() appauth.AppRole { return appauth.AppRoleFor(a.Role) }

// Recorder is where every call is recorded (answer_tool_calls): started before it runs, finished
// after, and read back for a restart's Lookup. *store.Store is the daemon's.
type Recorder interface {
	StartAnswerToolCall(ctx context.Context, call store.AnswerToolCall) error
	FinishAnswerToolCall(ctx context.Context, question string, seq int, result string, exit *int, at time.Time) error
	AnswerToolCall(ctx context.Context, question string, seq int) (store.AnswerToolCall, bool, error)
}

// Host is what every answer's calls run with. GH is the real gh boot found on the daemon's own
// host; Git the host's git, whose directory joins gh's on the call's PATH ("" for none); Legion
// this binary, absolute, which the wrapper GH_PATH names runs as `legion gh`. StateDir is the
// daemon's state directory, under whose answers/ each answer's directory lives. Credential is the
// function a pane's bundle comes from, appauth.GitHubCredential over the daemon's App tokens and
// the project's owners. Calls records each call, Log takes its line, Now is the clock.
type Host struct {
	GH         string
	Git        string
	Legion     string
	StateDir   string
	Credential runtime.GitHubCredential
	Calls      Recorder
	Log        *slog.Logger
	Now        func() time.Time
}

// Answer is one answer's gh environment, Open's: the agent, the question, the agent's credential
// bundle, the empty directories a call runs in, and the wrapper GH_PATH names. Close removes it.
type Answer struct {
	host     Host
	agent    Agent
	question string
	bundle   ghconfig.Bundle
	// dir is <state_dir>/answers/<question>/gh, everything below it the answer's own.
	dir                                   string
	owners                                string
	home, config, cache, data, state, cwd string
	wrapper                               string
}

// Agent is the agent the answer's calls run as.
func (a *Answer) Agent() Agent { return a.agent }

// Question is the question the answer answers.
func (a *Answer) Question() string { return a.question }

// Open builds question's gh environment for agent once, before its first call: a fresh 0700
// directory <state_dir>/answers/<question>/gh (whatever an earlier run of the same question left
// there removed first) holding the agent's bundle (host.Credential for its role and its issue's
// repository owner, written with ghconfig.WriteBundle, exactly as a tmux pane's is), the empty
// HOME, XDG and working directories every call runs with, and the GH_PATH wrapper. It logs the
// App and owners the bundle holds, never a token.
func Open(ctx context.Context, host Host, agent Agent, question string) (*Answer, error) {
	switch {
	case !filepath.IsAbs(host.GH):
		return nil, fmt.Errorf("the gh tool's gh %q is not an absolute path", host.GH)
	case host.Git != "" && !filepath.IsAbs(host.Git):
		return nil, fmt.Errorf("the gh tool's git %q is not an absolute path", host.Git)
	case !filepath.IsAbs(host.Legion):
		return nil, fmt.Errorf("the gh tool's legion %q is not an absolute path", host.Legion)
	case host.StateDir == "" || host.Credential == nil || host.Calls == nil || host.Log == nil || host.Now == nil:
		return nil, errors.New("the gh tool's host lacks its state directory, credential, recorder, log or clock")
	case agent.Repository.IsZero():
		return nil, fmt.Errorf("the gh tool for %s's %s: the issue's repository is unknown", agent.Issue, agent.Role)
	}
	name := url.PathEscape(question)
	if question == "" || name == "." || name == ".." {
		return nil, fmt.Errorf("the gh tool: %q is no question id a directory can be named for", question)
	}
	answers := filepath.Join(host.StateDir, "answers")
	a := &Answer{host: host, agent: agent, question: question, dir: filepath.Join(answers, name, "gh")}
	a.owners = filepath.Join(a.dir, "owners")
	a.home, a.cwd = filepath.Join(a.dir, "home"), filepath.Join(a.dir, "work")
	a.config, a.cache = filepath.Join(a.dir, "xdg", "config"), filepath.Join(a.dir, "xdg", "cache")
	a.data, a.state = filepath.Join(a.dir, "xdg", "data"), filepath.Join(a.dir, "xdg", "state")
	a.wrapper = filepath.Join(a.dir, "bin", "gh")
	if err := os.RemoveAll(a.dir); err != nil {
		return nil, fmt.Errorf("the gh tool for %s: clear %s: %w", question, a.dir, err)
	}
	for _, d := range []string{a.home, a.cwd, a.config, a.cache, a.data, a.state, filepath.Dir(a.wrapper)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("the gh tool for %s: %w", question, err)
		}
	}
	if err := os.Chmod(a.dir, 0o700); err != nil {
		return nil, fmt.Errorf("the gh tool for %s: %w", question, err)
	}
	opened := false
	defer func() {
		if !opened {
			_ = os.RemoveAll(a.dir)
		}
	}()
	bundle, err := host.Credential(ctx, agent.Role, strings.ToLower(agent.Repository.Owner()))
	if err != nil {
		return nil, fmt.Errorf("the gh tool for %s: the %s's github credential: %w", question, agent.Role, err)
	}
	if _, err := ghconfig.WriteBundle(a.owners, bundle); err != nil {
		return nil, fmt.Errorf("the gh tool for %s: write the %s's github credential: %w", question, agent.Role, err)
	}
	a.bundle = bundle
	// git asks the helper gh names after GH_PATH, `<GH_PATH> auth git-credential`, for a token:
	// `legion gh auth git-credential` answers it from the directory of the owner in git's path, as
	// in a pane, and reads the bundle and the real gh from the two variables a pane's environment
	// holds, which the call's own environment does not.
	script := "#!/bin/sh\nLEGION_GH_PATH=" + shellprefix.Literal(host.GH) + "\nLEGION_GH_OWNERS_DIR=" + shellprefix.Literal(a.owners) +
		"\nexport LEGION_GH_PATH LEGION_GH_OWNERS_DIR\nexec " + shellprefix.Literal(host.Legion) + " gh \"$@\"\n"
	if err := os.WriteFile(a.wrapper, []byte(script), 0o700); err != nil {
		return nil, fmt.Errorf("the gh tool for %s: write its gh wrapper: %w", question, err)
	}
	written := bundle.Owners[bundle.Default]
	host.Log.Info("answers: github credential written", "question", question, "issue", agent.Issue, "role", agent.Role,
		"app", written.App, "appID", agent.AppID, "expiresAt", written.ExpiresAt, "owners", bundle.OwnerNames())
	opened = true
	return a, nil
}

// Close removes the answer's directory, its credential bundle with it.
func (a *Answer) Close() error {
	if err := os.RemoveAll(a.dir); err != nil {
		return fmt.Errorf("the gh tool for %s: remove %s: %w", a.question, a.dir, err)
	}
	return nil
}

// Call is one gh call the model makes in an answer: the answer it runs in (which names the agent
// and the question), its seq among the answer's calls, from 1, its arguments as the model gave
// them (a JSON array of strings, gh's arguments without `gh`; no shell reads them) and the
// answer's bounds.
type Call struct {
	Answer *Answer
	Seq    int
	Args   string
	Bounds config.AnswerBounds
}

// Result is what the model reads of a call: gh's output, stdout and stderr interleaved and cut to
// the bound with a note saying how much was cut, or the refusal; gh's exit status, -1 when gh
// never ran or was stopped by a signal; and whether gh ran.
type Result struct {
	Output string
	Exit   int
	Ran    bool
}

// Run runs call. It is refused, with gh never started, when its arguments are no JSON array of
// strings, its seq is past the answer's gh_calls bound, its command is none of the tool's (the
// allow list), or it posts a comment or review in a form the agent's footer cannot be added to;
// it is refused after reading gh's help, with the command itself never run, when the owner it
// targets cannot be read for certain or is none of the bundle's, or gh's help names its command
// otherwise than its words do. Every other call runs with the footer added to what it posts, the
// routed owner's gh directory, the scrubbed environment, the working directory the answer's empty
// one, the gh_seconds bound and the gh_output_chars bound. Every call is recorded: an allowed one
// before gh starts and finished after it ends, a refused one with its refusal; one whose start
// cannot be recorded does not run. Each logs one `answers: gh` line.
func Run(ctx context.Context, call Call) Result {
	a := call.Answer
	started := a.host.Now()
	argv, err := decodeArgs(call.Args)
	if err != nil {
		return a.refuse(ctx, call.Seq, []string{call.Args}, started, err.Error())
	}
	switch {
	case call.Seq < 1:
		return a.refuse(ctx, call.Seq, argv, started, fmt.Sprintf("call %d is no call of the answer: calls are numbered from 1", call.Seq))
	case call.Bounds.GHCall <= 0 || call.Bounds.GHOutputChars <= 0:
		return a.refuse(ctx, call.Seq, argv, started, "the gh tool has no time or output bound (answers.bounds), so it runs nothing")
	case call.Seq > call.Bounds.GHCalls:
		return a.refuse(ctx, call.Seq, argv, started, fmt.Sprintf("this answer has made its %d gh calls (answers.bounds.gh_calls); answer with what the calls so far found", call.Bounds.GHCalls))
	}
	words := commandWords(argv)
	if why := refusal(argv, words); why != "" {
		return a.refuse(ctx, call.Seq, argv, started, why)
	}
	run, why := withFooter(argv, words, footer(a.agent, a.question, call.Seq))
	if why != "" {
		return a.refuse(ctx, call.Seq, argv, started, why)
	}
	ctx, cancel := context.WithTimeout(ctx, call.Bounds.GHCall)
	defer cancel()
	target, owner, dir, err := a.route(ctx, run)
	if err != nil {
		return a.refuse(ctx, call.Seq, run, started, err.Error())
	}
	if why := canonicalRefusal(words, target.Command); why != "" {
		return a.refuse(ctx, call.Seq, run, started, why)
	}
	if err := a.host.Calls.StartAnswerToolCall(ctx, a.record(call.Seq, run, owner, started)); err != nil {
		result := Result{Output: fmt.Sprintf("refused: the call could not be recorded, so it did not run: %v", err), Exit: -1}
		a.log(call.Seq, run, owner, result)
		return result
	}
	result := a.runGH(ctx, run, dir, call.Bounds)
	var exit *int
	if result.Ran {
		exit = &result.Exit
	}
	// The call ran: its record is finished whatever the answer's own deadline did meanwhile.
	if err := a.host.Calls.FinishAnswerToolCall(context.WithoutCancel(ctx), a.question, call.Seq, result.Output, exit, a.host.Now()); err != nil {
		a.host.Log.Error("answers: gh call not finished in its record", "question", a.question, "seq", call.Seq, "error", err)
	}
	a.log(call.Seq, run, owner, result)
	return result
}

// decodeArgs reads the model's arguments, a JSON array of strings.
func decodeArgs(raw string) ([]string, error) {
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil || argv == nil {
		return nil, fmt.Errorf("refused: the gh tool takes gh's arguments as a JSON array of strings, without `gh` (%q is none)", raw)
	}
	return argv, nil
}

// refuse records and logs a call refused before gh ran it, and answers its refusal.
func (a *Answer) refuse(ctx context.Context, seq int, argv []string, started time.Time, why string) Result {
	if !strings.HasPrefix(why, "refused: ") {
		why = "refused: " + why
	}
	result := Result{Output: why, Exit: -1}
	ctx = context.WithoutCancel(ctx)
	switch err := a.host.Calls.StartAnswerToolCall(ctx, a.record(seq, argv, "", started)); {
	case err != nil:
		a.host.Log.Error("answers: refused gh call not recorded", "question", a.question, "seq", seq, "error", err)
	default:
		if err := a.host.Calls.FinishAnswerToolCall(ctx, a.question, seq, result.Output, nil, a.host.Now()); err != nil {
			a.host.Log.Error("answers: refused gh call not finished in its record", "question", a.question, "seq", seq, "error", err)
		}
	}
	a.log(seq, argv, "", result)
	return result
}

// record is the row a call starts with.
func (a *Answer) record(seq int, argv []string, owner string, started time.Time) store.AnswerToolCall {
	return store.AnswerToolCall{Question: a.question, Seq: seq, Args: argv, App: string(a.agent.App()), Owner: owner, StartedAt: started}
}

// log is the call's one line: the question and seq, the App (its role and configured id) and the
// owner whose gh directory it ran with ("" for a call refused before routing), its arguments as
// run, and its exit (-1 when gh never ran).
func (a *Answer) log(seq int, argv []string, owner string, result Result) {
	attrs := []any{"question", a.question, "seq", seq, "issue", a.agent.Issue, "role", a.agent.Role,
		"app", string(a.agent.App()), "appID", a.agent.AppID, "owner", owner, "args", argv, "exit", result.Exit, "ran", result.Ran}
	if !result.Ran {
		attrs = append(attrs, "refused", result.Output)
	}
	a.host.Log.Info("answers: gh", attrs...)
}
