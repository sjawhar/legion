//go:build twobuild

// The CLI half of the two-build suite: each build's `dispatch` command against the other build's
// server (twobuild_harness_test.go holds the harness both halves start their builds with).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// ───── the CLI ──────────────────────────────────────────────────────────────

// cliSession is the session both builds' CLIs act as.
const cliSession = "twobuild-cli"

// cliBuild is one build's `dispatch` command: how it is run, and what a failure calls it.
type cliBuild struct {
	name string
	argv []string
}

func (b *twoBuilds) previousCLI() cliBuild {
	return cliBuild{name: "previous build's CLI", argv: []string{"bun", b.build.path("dispatch.js")}}
}

func currentCLI(t *testing.T) cliBuild {
	t.Helper()
	entry, err := filepath.Abs(filepath.Join("..", "..", "..", "envoy-client", "bin", "dispatch.ts"))
	if err != nil {
		t.Fatalf("resolve the current CLI: %v", err)
	}
	return cliBuild{name: "current build's CLI", argv: []string{"bun", entry}}
}

// cliRun is one run of a CLI.
type cliRun struct {
	code           int
	stdout, stderr string
}

func (r cliRun) String() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
}

// run runs the CLI against server with args. Its environment is written here, never inherited:
// a Legion pane exports its own Dispatch's URL and token, which the command would otherwise use.
// HOME and DISPATCH_STATE_DIR are home, so it reads no developer's envoy.json.
func (c cliBuild) run(t *testing.T, server *dispatchProcess, home string, args ...string) cliRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.argv[0], append(slices.Clone(c.argv[1:]), args...)...)
	cmd.Dir = home
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"DISPATCH_STATE_DIR=" + home,
		"DISPATCH_HOST=omp",
		"DISPATCH_SESSION_ID=" + cliSession,
		"DISPATCH_URL=" + server.url(""),
		"DISPATCH_TOKEN=shutdown-test-token",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	run := cliRun{code: 0, stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		run.code = exit.ExitCode()
	case err != nil:
		t.Fatalf("run %s %v: %v", c.name, args, err)
	}
	return run
}

// runOK runs the CLI and fails the test unless it exits 0.
func (c cliBuild) runOK(t *testing.T, server *dispatchProcess, serverName, home string, args ...string) cliRun {
	t.Helper()
	run := c.run(t, server, home, args...)
	if run.code != 0 {
		t.Fatalf("%s against the %s: dispatch %s: %s", c.name, serverName, strings.Join(args, " "), run)
	}
	return run
}

// servedIssue is the part of GET /api/v1/issues/{key} the CLI rows read back.
type servedIssue struct {
	Title             string          `json:"title"`
	Status            string          `json:"status"`
	Labels            []string        `json:"labels"`
	Claim             json.RawMessage `json:"claim"`
	PrimaryArtifactID string          `json:"primary_artifact_id"`
}

// servedComment is the part of a comment of GET /api/v1/issues/{key}/comments the reply row reads
// back.
type servedComment struct {
	Body    string  `json:"body"`
	ReplyTo *string `json:"reply_to"`
}

var (
	uuidPattern          = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	postedCommentPattern = regexp.MustCompile(`Posted comment (` + uuidPattern.String() + `)`)
)

// Each build's CLI against the other build's server, which is what a release does to every agent:
// its CLI is the version its plugin shipped, and the load balancer sends each command to either
// build. Each command of a list valid for both CLIs exits 0, and its effect reads back through the
// server of the CLI's own build. A read command reads what the other server wrote just before it.
func TestTwoBuildsAnswerEachOthersCLI(t *testing.T) {
	builds := newTwoBuilds(t)
	builds.startPrevious(t, nil)
	builds.startCurrent(t, nil)
	for _, direction := range []struct {
		cli           cliBuild
		server, other *dispatchProcess
	}{
		{builds.previousCLI(), builds.current(), builds.previous()},
		{currentCLI(t), builds.previous(), builds.current()},
	} {
		t.Run(direction.cli.name+" against the "+builds.buildName(direction.server), func(t *testing.T) {
			requireCLIRoundTrip(t, direction.cli, direction.server, direction.other,
				builds.buildName(direction.server), builds.buildName(direction.other))
		})
	}
}

// requireCLIRoundTrip runs every command of the list with cli against server, and reads each effect
// back through other.
func requireCLIRoundTrip(t *testing.T, cli cliBuild, server, other *dispatchProcess, serverName, otherName string) {
	home := t.TempDir()
	project := uniqueProjectKey(t, "CLI")
	other.createProject(t, project, "Two builds")
	word := strings.ToLower(uniqueProjectKey(t, "twobuild"))
	run := func(args ...string) cliRun {
		t.Helper()
		return cli.runOK(t, server, serverName, home, args...)
	}
	readBack := func(what, path, want string) {
		t.Helper()
		if answer := getJSON(t, other, path, nil); !strings.Contains(string(answer), want) {
			t.Fatalf("after %s, the %s answers GET %s without %q:\n%s", what, otherName, path, want, answer)
		}
	}

	// issue
	title := "Two builds " + word
	created := run("issue", "--project", project, "--title", title, "--spec", "The first line.\n\nThe edit target.", "--force")
	issueKey := key(t, project, created.stdout)
	readIssue := func() servedIssue {
		t.Helper()
		var read servedIssue
		getJSON(t, other, "/api/v1/issues/"+issueKey, &read)
		return read
	}
	if got := readIssue(); got.Title != title || got.PrimaryArtifactID == "" {
		t.Fatalf("after dispatch issue, the %s reads %s as %+v, want the title %q and a spec", otherName, issueKey, got, title)
	}

	// issue-update with labels and a status
	run("issue-update", "--issue", issueKey, "--label", "alpha", "--label", "beta", "--status", "todo")
	if got := readIssue(); !slices.Equal(got.Labels, []string{"alpha", "beta"}) || got.Status != "todo" {
		t.Fatalf("after dispatch issue-update, the %s reads %s with labels %v and status %q, want [alpha beta] and todo", otherName, issueKey, got.Labels, got.Status)
	}

	// read: what the other server wrote
	retitled := "Retitled on the other build " + word
	other.call(t, http.MethodPatch, "/api/v1/issues/"+issueKey, jsonBody(t, map[string]string{"title": retitled}), http.StatusOK)
	if read := run("read", "--issue", issueKey); !strings.Contains(read.stdout, retitled) {
		t.Fatalf("dispatch read --issue %s does not show the title the %s wrote, %q:\n%s", issueKey, otherName, retitled, read)
	}

	// comment
	commentBody := "A comment " + word
	commented := run("comment", "--issue", issueKey, "--body", commentBody)
	match := postedCommentPattern.FindStringSubmatch(commented.stdout)
	if match == nil {
		t.Fatalf("dispatch comment printed no comment id:\n%s", commented)
	}
	commentID := match[1]
	readBack("dispatch comment", "/api/v1/issues/"+issueKey+"/comments", commentBody)

	// reply
	replyBody := "A reply " + word
	run("comment", "--issue", issueKey, "--reply-to", commentID, "--body", replyBody)
	var comments []servedComment
	getJSON(t, other, "/api/v1/issues/"+issueKey+"/comments", &comments)
	if !slices.ContainsFunc(comments, func(c servedComment) bool {
		return c.Body == replyBody && c.ReplyTo != nil && *c.ReplyTo == commentID
	}) {
		t.Fatalf("after dispatch comment --reply-to, the %s lists no reply %q to %s: %+v", otherName, replyBody, commentID, comments)
	}

	// message
	messageBody := "A message " + word
	run("message", "--issue", issueKey, "--body", messageBody)
	readBack("dispatch message", "/api/v1/issues/"+issueKey+"/messages", messageBody)

	// ask
	question := "Which wave goes first for " + word + "? Recommendation: the quiet one, since it carries the least risk."
	asked := run("ask", "--issue", issueKey, "--question", question, "--option", "Quiet: The quiet region first.", "--option", "Busy: The busy region first.")
	askID := uuidPattern.FindString(asked.stdout)
	if askID == "" {
		t.Fatalf("dispatch ask printed no ask id:\n%s", asked)
	}
	readBack("dispatch ask", "/api/v1/asks/"+askID, word)

	// ask answer: the reply under the question
	answerBody := "The quiet region, " + word
	run("comment", "--issue", issueKey, "--reply-to-ask", askID, "--body", answerBody)
	readBack("dispatch comment --reply-to-ask", "/api/v1/asks/"+askID, answerBody)

	// artifact write
	run("artifact", "--issue", issueKey, "--name", "notes.md", "--content", "# Notes "+word+"\n")
	readBack("dispatch artifact", "/api/v1/issues/"+issueKey+"/artifacts/notes.md/text", "Notes "+word)

	// doc-read: what the other server wrote
	edited := "The first line, edited on the other build."
	other.call(t, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts/spec/edits",
		jsonBody(t, map[string]any{"ops": []map[string]string{{"op": "replace", "find": "The first line.", "with": edited}}}), http.StatusOK)
	if read := run("doc-read", "--issue", issueKey); !strings.Contains(read.stdout, edited) {
		t.Fatalf("dispatch doc-read --issue %s does not show the edit the %s made, %q:\n%s", issueKey, otherName, edited, read)
	}

	// doc-edit
	run("doc-edit", "--issue", issueKey, "--ops-json", `[{"op":"replace","find":"The edit target.","with":"The edit landed."}]`)
	readBack("dispatch doc-edit", "/api/v1/issues/"+issueKey+"/artifacts/spec/text", "The edit landed.")

	// search: the issue the other server retitled
	if found := run("search", "--query", word); !strings.Contains(found.stdout, issueKey) {
		t.Fatalf("dispatch search --query %s does not find %s:\n%s", word, issueKey, found)
	}

	// claim
	run("claim", "--issue", issueKey)
	if got := readIssue(); !strings.Contains(string(got.Claim), cliSession) {
		t.Fatalf("after dispatch claim, the %s reads %s's claim as %s, want the session %s", otherName, issueKey, got.Claim, cliSession)
	}

	// request-approval
	summary := "The two-build spec for " + word + "."
	run("request-approval", "--issue", issueKey, "--summary", summary)
	readBack("dispatch request-approval", "/api/v1/issues/"+issueKey+"/asks?state=all", summary)
}

// key is the issue key of project in what a CLI printed.
func key(t *testing.T, project, output string) string {
	t.Helper()
	found := regexp.MustCompile(`\b` + regexp.QuoteMeta(project) + `-[0-9]+\b`).FindString(output)
	if found == "" {
		t.Fatalf("no %s issue key in %q", project, output)
	}
	return found
}
