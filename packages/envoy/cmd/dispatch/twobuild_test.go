//go:build twobuild

// Two builds of Dispatch serving one Postgres and one NATS at once, which is what a rolling release
// runs: the build that serves now (the previous build) and the build that rolls in beside it (this
// tree). Each row starts one envoy-dispatch of each build, on one database storetest cloned at this
// tree's schema (the previous build boots on it, which is the SQL half of the rule in
// cmd/dispatch/README.md) and one NATS server, with one signing key, and drives the surface the
// other build's clients use: the CLI, the stored documents and the document socket, and the
// messages and subscription settings on the bus.
//
// The previous build is a directory packages/envoy/scripts/build-dispatch-build.sh writes, named by
// DISPATCH_PREVIOUS_BUILD. The suite skips only when that variable is unset; a directory it names
// that lacks a part fails the suite, naming the parts it lacks. The current side is this tree: its
// server from cmdtest.Build, its CLI run as `bun packages/envoy-client/bin/dispatch.ts`, its corpus
// read from the working tree.
//
// Its own build tag and its own CI job, dispatch-two-builds in
// .github/workflows/envoy-and-contracts.yaml, for the reason the multiprocess suite has its own: a
// row waits for durable hand-overs and settings polls, tens of seconds each. That job builds the
// previous release with build-dispatch-build.sh (or the commit a manual run names in its
// previous_commit input), caches the build by its commit, and runs this suite against it.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	natsgo "github.com/nats-io/nats.go"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testnats"
)

// ───── the previous build ───────────────────────────────────────────────────

// previousBuildVariable names the directory build-dispatch-build.sh wrote for the previous build.
const previousBuildVariable = "DISPATCH_PREVIOUS_BUILD"

// previousBuildParts is every part of a previous build's directory a row reads, as
// build-dispatch-build.sh lays it out.
var previousBuildParts = []struct {
	path string
	dir  bool
}{
	{"COMMIT", false},
	{"envoy-dispatch", false},
	{"dispatch.js", false},
	{"web/index.html", false},
	{"pmdoc/testdata/corpus", true},
	{"pmdoc/schema/blocks.json", false},
	{"text/testdata/dispatch-text-references.json", false},
	{"text/testdata/dispatch-href-references.json", false},
}

// previousBuild is the directory of the previous build and the commit it was built from.
type previousBuild struct {
	dir    string
	commit string
}

// openPreviousBuild reads DISPATCH_PREVIOUS_BUILD. It skips the test when the variable is unset,
// and fails it when the directory the variable names lacks any part a row reads.
func openPreviousBuild(t *testing.T) previousBuild {
	t.Helper()
	dir, set := os.LookupEnv(previousBuildVariable)
	if !set {
		t.Skipf("%s is unset. Build the previous build with packages/envoy/scripts/build-dispatch-build.sh --previous-release --out <dir>, then set %s=<dir>.",
			previousBuildVariable, previousBuildVariable)
	}
	if dir == "" {
		t.Fatalf("%s is set but empty. Set it to the directory build-dispatch-build.sh wrote.", previousBuildVariable)
	}
	var missing []string
	for _, part := range previousBuildParts {
		info, err := os.Stat(filepath.Join(dir, part.path))
		switch {
		case err != nil:
			missing = append(missing, part.path)
		case part.dir && !info.IsDir(), !part.dir && info.IsDir():
			missing = append(missing, part.path+" (wrong kind of file)")
		case part.dir:
			if entries, err := os.ReadDir(filepath.Join(dir, part.path)); err != nil || len(entries) == 0 {
				missing = append(missing, part.path+" (empty)")
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s=%s is not a complete previous build. It has no %s. Build it again with packages/envoy/scripts/build-dispatch-build.sh.",
			previousBuildVariable, dir, strings.Join(missing, ", "))
	}
	commit, err := os.ReadFile(filepath.Join(dir, "COMMIT"))
	if err != nil {
		t.Fatalf("read the previous build's COMMIT: %v", err)
	}
	return previousBuild{dir: dir, commit: strings.TrimSpace(string(commit))}
}

func (b previousBuild) path(parts ...string) string {
	return filepath.Join(append([]string{b.dir}, parts...)...)
}

// ───── the two builds ───────────────────────────────────────────────────────

// twoBuilds is one process of each build on one database and one NATS server: a is the previous
// build's, b is this tree's.
type twoBuilds struct {
	*dispatchPair
	build previousBuild
	// binary is this tree's envoy-dispatch, built once so a restart runs the same binary.
	binary string
}

// newTwoBuilds opens the previous build, the database and the NATS server, and starts nothing.
func newTwoBuilds(t *testing.T) *twoBuilds {
	t.Helper()
	build := openPreviousBuild(t)
	return &twoBuilds{dispatchPair: newPair(t), build: build, binary: cmdtest.Build(t, "envoy-dispatch")}
}

// options is what both builds' processes start with: the pair's NATS, the one signing key, and env.
func (b *twoBuilds) options(env map[string]string) dispatchOptions {
	return dispatchOptions{NATSURL: b.nats, SigningKey: multiprocessSigningKey, Env: env}
}

// startPrevious starts a process of the previous build, its own web app served, and returns once it
// answers /healthz.
func (b *twoBuilds) startPrevious(t *testing.T, env map[string]string) *dispatchProcess {
	t.Helper()
	options := b.options(env)
	options.Binary, options.WebDist = b.build.path("envoy-dispatch"), b.build.path("web")
	b.a = startDispatchProcessWith(t, b.databaseURL(t), options)
	b.a.waitHealthy(t)
	return b.a
}

// startCurrent starts a process of this tree and returns once it answers /healthz.
func (b *twoBuilds) startCurrent(t *testing.T, env map[string]string) *dispatchProcess {
	t.Helper()
	options := b.options(env)
	options.Binary = b.binary
	b.b = startDispatchProcessWith(t, b.databaseURL(t), options)
	b.b.waitHealthy(t)
	return b.b
}

func (b *twoBuilds) previous() *dispatchProcess { return b.a }
func (b *twoBuilds) current() *dispatchProcess  { return b.b }

// buildName is what a failure calls a process: which build it runs.
func (b *twoBuilds) buildName(p *dispatchProcess) string {
	if p == b.a {
		return b.previousName()
	}
	return b.currentName()
}

// previousName and currentName are what a failure calls each build.
func (b *twoBuilds) previousName() string {
	return "previous build (" + shortCommit(b.build.commit) + ")"
}

func (b *twoBuilds) currentName() string { return "current build" }

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// stop stops p as a deploy does, SIGTERM and its graceful shutdown, and waits for it to exit.
func stop(t *testing.T, p *dispatchProcess) {
	t.Helper()
	p.Terminate(t)
	p.WaitExit(t, "SIGTERM")
}

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

// getJSON reads path from p as the test's login and decodes it into into.
func getJSON(t *testing.T, p *dispatchProcess, path string, into any) []byte {
	t.Helper()
	answer := p.call(t, http.MethodGet, path, "", http.StatusOK)
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode GET %s: %v: %s", path, err, answer)
		}
	}
	return answer
}

// jsonBody is value as a request body.
func jsonBody(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode a request body: %v", err)
	}
	return string(body)
}

// servedIssue is the part of GET /api/v1/issues/{key} the CLI rows read back.
type servedIssue struct {
	Title             string          `json:"title"`
	Status            string          `json:"status"`
	Labels            []string        `json:"labels"`
	Claim             json.RawMessage `json:"claim"`
	PrimaryArtifactID string          `json:"primary_artifact_id"`
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
	var comments []struct {
		Body    string  `json:"body"`
		ReplyTo *string `json:"reply_to"`
	}
	getJSON(t, other, "/api/v1/issues/"+issueKey+"/comments", &comments)
	if !slices.ContainsFunc(comments, func(c struct {
		Body    string  `json:"body"`
		ReplyTo *string `json:"reply_to"`
	}) bool {
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

// ───── stored documents ─────────────────────────────────────────────────────

// referenceServerURL is DISPATCH_SERVER_URL for the document rows: the origin the reference
// fixtures' dashboard links name (text/testdata), so both builds read those links as references.
const referenceServerURL = "https://dispatch.test"

// documentSide is one build as a writer or a reader of documents: its process, the corpus and
// reference fixtures of its own tree, its block manifest, and what its page presents on a
// document socket.
type documentSide struct {
	name     string
	process  *dispatchProcess
	corpus   string
	fixtures string
	manifest blockManifest
	page     pageParameters
}

// documentSides is the previous build's side and the current build's, each from its own tree.
func (b *twoBuilds) documentSides(t *testing.T) []documentSide {
	t.Helper()
	tree := filepath.Join("..", "..", "internal", "dispatch")
	previousManifest := readManifest(t, b.build.path("pmdoc", "schema", "blocks.json"))
	currentManifest := readManifest(t, filepath.Join(tree, "pmdoc", "schema", "blocks.json"))
	return []documentSide{
		{
			name:     b.buildName(b.previous()),
			process:  b.previous(),
			corpus:   b.build.path("pmdoc", "testdata", "corpus"),
			fixtures: b.build.path("text", "testdata"),
			manifest: previousManifest,
			page:     previousManifest.page(pageSendsCreatable(t, b.build.path("web"))),
		},
		{
			name:     b.buildName(b.current()),
			process:  b.current(),
			corpus:   filepath.Join(tree, "pmdoc", "testdata", "corpus"),
			fixtures: filepath.Join(tree, "text", "testdata"),
			manifest: currentManifest,
			// The current page sends schema_creatable (packages/dispatch/web/src/features/doc/connection.ts).
			page: currentManifest.page(true),
		},
	}
}

// elementVersions are the two numbers the manifest may give an element: the schema version that
// first reads it and the one that first creates it, each 1 where it names none.
type elementVersions struct {
	Since          *int `json:"since"`
	CreatableSince *int `json:"creatable_since"`
}

type manifestAttribute struct {
	elementVersions
	Kind           string                     `json:"kind"`
	Choices        []string                   `json:"choices"`
	ChoiceVersions map[string]elementVersions `json:"choice_versions"`
	Default        json.RawMessage            `json:"default"`
	Server         bool                       `json:"server"`
}

type manifestType struct {
	elementVersions
	Name       string                       `json:"name"`
	Content    string                       `json:"content"`
	Attributes map[string]manifestAttribute `json:"attributes"`
}

// blockManifest is a build's pmdoc/schema/blocks.json.
type blockManifest struct {
	Version int               `json:"version"`
	Nodes   []elementVersions `json:"nodes"`
	Marks   []elementVersions `json:"marks"`
	Types   []manifestType    `json:"types"`
}

func readManifest(t *testing.T, path string) blockManifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the block manifest: %v", err)
	}
	var manifest blockManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode the block manifest %s: %v", path, err)
	}
	return manifest
}

// creatable is R for the manifest: the highest since among the elements an editor of its version
// may create, by the rule the page states (schemaCreatable in
// packages/dispatch/web/src/features/doc/schema.ts) and pmdoc.SchemaCreatable computes.
func (m blockManifest) creatable() int {
	creatable := 0
	count := func(element elementVersions) {
		since, creatableSince := 1, 1
		if element.Since != nil {
			since = *element.Since
		}
		if element.CreatableSince != nil {
			creatableSince = *element.CreatableSince
		}
		if creatableSince <= m.Version {
			creatable = max(creatable, since)
		}
	}
	for _, element := range slices.Concat(m.Nodes, m.Marks) {
		count(element)
	}
	for _, typed := range m.Types {
		count(typed.elementVersions)
		for _, attribute := range typed.Attributes {
			count(attribute.elementVersions)
			for _, choice := range attribute.Choices {
				count(attribute.ChoiceVersions[choice])
			}
		}
	}
	return creatable
}

// pageParameters is what a build's page presents when it opens a document socket
// (features/doc/connection.ts): the query its provider's websocket carries, and the token its
// provider authenticates with, which is the schema version alone.
type pageParameters struct {
	query url.Values
	token string
}

func (p pageParameters) String() string { return "?" + p.query.Encode() + " with token " + p.token }

// page is what a page built with this manifest presents: schema_version, and schema_creatable
// when that build's page sends it.
func (m blockManifest) page(sendsCreatable bool) pageParameters {
	query := url.Values{"schema_version": {strconv.Itoa(m.Version)}}
	if sendsCreatable {
		query.Set("schema_creatable", strconv.Itoa(m.creatable()))
	}
	return pageParameters{query: query, token: strconv.Itoa(m.Version)}
}

// pageSendsCreatable reports whether a built web app's scripts name schema_creatable, the
// parameter a page sends from INFRA-558 on. A page before it sends schema_version alone.
func pageSendsCreatable(t *testing.T, web string) bool {
	t.Helper()
	scripts, err := filepath.Glob(filepath.Join(web, "assets", "*.js"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("the previous build's web app at %s holds no assets/*.js: %v", web, err)
	}
	for _, script := range scripts {
		raw, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		if bytes.Contains(raw, []byte("schema_creatable")) {
			return true
		}
	}
	return false
}

// createDocument creates an issue in project whose spec is markdown, through p, and returns its key
// and its spec's id. It is forced past the duplicate check: the rows create many issues of one
// shape on purpose.
func createDocument(t *testing.T, p *dispatchProcess, project, title, markdown string) (string, string) {
	t.Helper()
	created := p.call(t, http.MethodPost, "/api/v1/issues",
		jsonBody(t, map[string]any{"project": project, "title": title, "spec": markdown, "force": true}), http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" || issue.PrimaryArtifactID == "" {
		t.Fatalf("create the document %q: %v: %s", title, err, created)
	}
	return issue.Key, issue.PrimaryArtifactID
}

// appendParagraph appends paragraph at the end of the document through p, an edit that versions
// the document in the request's own transaction.
func appendParagraph(t *testing.T, p *dispatchProcess, artifactID, paragraph string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits",
		jsonBody(t, map[string]any{"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": paragraph + "\n"}}}), http.StatusOK)
}

// withParagraph is render with paragraph appended as the document's last block.
func withParagraph(render, paragraph string) string {
	switch {
	case render == "":
		return paragraph + "\n"
	case strings.HasSuffix(render, "\n"):
		return render + "\n" + paragraph + "\n"
	default:
		return render + "\n\n" + paragraph + "\n"
	}
}

// The Hocuspocus frames a document socket's admission is read from (ygo provider/websocket).
const (
	hocuspocusAuth          = 2
	hocuspocusAuthToken     = 0
	hocuspocusAuthDenied    = 1
	hocuspocusAuthenticated = 2
	// writableScope is the scope an Authenticated frame names for a socket that may write.
	writableScope = "read-write"
)

// documentSocket is a document socket as a page's provider holds it: authenticated with the
// page's token, reading every frame the room sends until the connection ends.
type documentSocket struct {
	artifactID string
	scope      string
	ended      chan struct{}
	endErr     error
}

// openDocumentSocket opens server's socket for the document with page's parameters, authenticates
// as the page's provider does, and returns once the server has answered with the socket's scope.
func openDocumentSocket(t *testing.T, server *dispatchProcess, artifactID string, page pageParameters) *documentSocket {
	t.Helper()
	address := "ws://127.0.0.1:" + strconv.Itoa(server.port) + "/ws/doc/" + artifactID + "?" + page.query.Encode()
	connection, response, err := gws.DefaultDialer.Dial(address, http.Header{"X-Dispatch-User": {dispatchTestLogin}})
	if err != nil {
		status := 0
		var body []byte
		if response != nil {
			status = response.StatusCode
			body, _ = io.ReadAll(response.Body)
		}
		t.Fatalf("open the document socket %s: status %d %s: %v", address, status, body, err)
	}
	socket := &documentSocket{artifactID: artifactID, ended: make(chan struct{})}
	t.Cleanup(func() {
		_ = connection.Close()
		<-socket.ended
	})
	auth := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(hocuspocusAuth)
		encoder.WriteVarUint(hocuspocusAuthToken)
		encoder.WriteVarString(page.token)
	})
	if err := connection.WriteMessage(gws.BinaryMessage, auth); err != nil {
		t.Fatalf("authenticate the document socket: %v", err)
	}
	scopes := make(chan string, 1)
	go func() {
		defer close(socket.ended)
		for {
			_, message, err := connection.ReadMessage()
			if err != nil {
				socket.endErr = err
				return
			}
			decoder := encoding.NewDecoder(message)
			if _, err := decoder.ReadVarString(); err != nil {
				continue
			}
			if kind, err := decoder.ReadVarUint(); err != nil || kind != hocuspocusAuth {
				continue
			}
			answer, err := decoder.ReadVarUint()
			if err != nil {
				continue
			}
			text, _ := decoder.ReadVarString()
			switch answer {
			case hocuspocusAuthenticated:
				text = "scope " + text
			case hocuspocusAuthDenied:
				text = "denied: " + text
			default:
				continue
			}
			select {
			case scopes <- text:
			default:
			}
		}
	}()
	select {
	case answer := <-scopes:
		socket.scope = strings.TrimPrefix(answer, "scope ")
	case <-socket.ended:
		t.Fatalf("the document socket for %s closed before the server answered its authentication: %v", artifactID, socket.endErr)
	case <-time.After(10 * time.Second):
		t.Fatalf("the server never answered the document socket's authentication for %s", artifactID)
	}
	return socket
}

func (s *documentSocket) closed() bool {
	select {
	case <-s.ended:
		return true
	default:
		return false
	}
}

// corpusFiles is every markdown file of a corpus directory, by name.
func corpusFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the corpus %s: %v", dir, err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			files = append(files, entry.Name())
		}
	}
	if len(files) == 0 {
		t.Fatalf("the corpus %s holds no markdown file", dir)
	}
	return files
}

// A document either build wrote reads and edits on the other. For each writer X and each file of
// X's own corpus: a document created on X answers /text on Y and takes an append edit there, and X
// then renders it as its first render with the appended paragraph after it; on Y, a document socket
// opened with X's page parameters is admitted writable and stays open. A version X wrote holding
// every typed block of X's manifest is approved, the answer to its decision block makes Y write the
// next version, and the approval is carried to it. A body holding every reference form of X's
// text/testdata keeps its refs rows after Y rewrites it.
func TestTwoBuildsReadAndEditEachOthersDocuments(t *testing.T) {
	builds := newTwoBuilds(t)
	env := map[string]string{"DISPATCH_SERVER_URL": referenceServerURL}
	builds.startPrevious(t, env)
	builds.startCurrent(t, env)
	sides := builds.documentSides(t)
	for index, writer := range sides {
		reader := sides[1-index]
		t.Run(writer.name+" writes, "+reader.name+" reads", func(t *testing.T) {
			project := uniqueProjectKey(t, "DOC")
			writer.process.createProject(t, project, "Two builds")
			t.Run("corpus", func(t *testing.T) {
				requireCorpusCrossesBuilds(t, writer, reader, project)
			})
			t.Run("approval carry", func(t *testing.T) {
				requireApprovalCarriedAcrossBuilds(t, writer, reader, project)
			})
			t.Run("references", func(t *testing.T) {
				requireReferencesKeptAcrossBuilds(t, builds, writer, reader, project)
			})
		})
	}
}

// socketOpenFor is how long every socket the corpus row opened must stay open after the last one
// was admitted: long enough for a server that closes a socket it admitted to have done so.
const socketOpenFor = 3 * time.Second

func requireCorpusCrossesBuilds(t *testing.T, writer, reader documentSide, project string) {
	var sockets []*documentSocket
	files := corpusFiles(t, writer.corpus)
	for _, file := range files {
		markdown, err := os.ReadFile(filepath.Join(writer.corpus, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		_, artifactID := createDocument(t, writer.process, project, "Corpus "+file, string(markdown))
		first := writer.process.documentText(t, artifactID)
		reader.process.documentText(t, artifactID)

		paragraph := "Appended on the " + reader.name + " to " + file + "."
		appendParagraph(t, reader.process, artifactID, paragraph)
		if got, want := writer.process.documentText(t, artifactID), withParagraph(first, paragraph); got != want {
			t.Errorf("%s: after the %s appended a paragraph, the %s renders\n%q\nwant its first render with the paragraph after it\n%q",
				file, reader.name, writer.name, got, want)
		}

		socket := openDocumentSocket(t, reader.process, artifactID, writer.page)
		if socket.scope != writableScope {
			t.Errorf("%s: the %s admits a socket with the %s page's parameters (%s) as %q, want %q",
				file, reader.name, writer.name, writer.page, socket.scope, writableScope)
		}
		sockets = append(sockets, socket)
	}
	time.Sleep(socketOpenFor)
	for _, socket := range sockets {
		if socket.closed() {
			t.Errorf("the %s closed the writable socket it admitted for document %s: %v", reader.name, socket.artifactID, socket.endErr)
		}
	}
	t.Logf("%d corpus documents of the %s read, took an edit and admitted a writable socket on the %s", len(files), writer.name, reader.name)
}

// typedBlockID is the block id the typed-block document gives each typed block: tb-<type name>.
func typedBlockID(name string) string { return "tb-" + name }

// typedBlocksMarkdown is a document holding one block of every typed block the manifest declares,
// each with its id and every attribute a writer sets at its default. A decision block (the ask
// type) is given two choices, First and Second.
func typedBlocksMarkdown(t *testing.T, manifest blockManifest) string {
	t.Helper()
	blocks := []string{"# Every typed block\n"}
	for _, typed := range manifest.Types {
		attributes := []string{"#" + typedBlockID(typed.Name)}
		for _, name := range slices.Sorted(maps.Keys(typed.Attributes)) {
			attribute := typed.Attributes[name]
			if attribute.Server || len(attribute.Default) == 0 {
				continue
			}
			var value any
			if err := json.Unmarshal(attribute.Default, &value); err != nil {
				t.Fatalf("decode the default of %s.%s: %v", typed.Name, name, err)
			}
			switch value := value.(type) {
			case string:
				if value == "" {
					attributes = append(attributes, name)
				} else {
					attributes = append(attributes, fmt.Sprintf("%s=%q", name, value))
				}
			case bool:
				attributes = append(attributes, fmt.Sprintf("%s=%q", name, strconv.FormatBool(value)))
			default:
				t.Fatalf("the harness writes no %T default, as %s.%s has; teach typedBlocksMarkdown to write it", value, typed.Name, name)
			}
		}
		var content []string
		for _, token := range strings.Fields(typed.Content) {
			switch strings.TrimRight(token, "+*?") {
			case "block", "paragraph":
				content = append(content, "The "+typed.Name+" block says one thing.")
			case "bullet_list":
				content = append(content, "- First: The first choice.\n- Second: The second choice.")
			default:
				t.Fatalf("the harness writes no %q content, as the %s block takes; teach typedBlocksMarkdown to write it", token, typed.Name)
			}
		}
		blocks = append(blocks, ":::"+typed.Name+"{"+strings.Join(attributes, " ")+"}\n"+strings.Join(content, "\n\n")+"\n:::\n")
	}
	return strings.Join(blocks, "\n")
}

// servedReview is a review as GET /api/v1/artifacts/{id}/reviews lists it, newest first.
type servedReview struct {
	Version int         `json:"version"`
	State   string      `json:"state"`
	Actor   model.Actor `json:"actor"`
}

func requireApprovalCarriedAcrossBuilds(t *testing.T, writer, reader documentSide, project string) {
	if !slices.ContainsFunc(writer.manifest.Types, func(typed manifestType) bool { return typed.Name == "ask" }) {
		t.Fatalf("the %s's manifest declares no ask block, the decision block an approval is carried by", writer.name)
	}
	issueKey, artifactID := createDocument(t, writer.process, project, "Every typed block", typedBlocksMarkdown(t, writer.manifest))
	askID := waitForBlockAsk(t, writer.process, issueKey, typedBlockID("ask"))

	var approval servedReview
	approved := writer.process.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews", `{"state":"approved"}`, http.StatusCreated)
	if err := json.Unmarshal(approved, &approval); err != nil || approval.State != "approved" {
		t.Fatalf("approve the typed-block document on the %s: %v: %s", writer.name, err, approved)
	}
	reader.process.call(t, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		`{"selected":["First"],"expected_edited_at":null}`, http.StatusOK)

	deadline := time.Now().Add(30 * time.Second)
	for {
		var reviews []servedReview
		getJSON(t, writer.process, "/api/v1/artifacts/"+artifactID+"/reviews", &reviews)
		if len(reviews) > 1 && reviews[0].State == "approved" && reviews[0].Version > approval.Version &&
			reviews[0].Actor == (model.Actor{Kind: "user", ID: dispatchTestLogin}) {
			t.Logf("the %s carried the approval of version %d to version %d", reader.name, approval.Version, reviews[0].Version)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the answer on the %s to the decision block of a version the %s wrote and %s approved (version %d) carried no approval within 30s; reviews: %+v\n%s:\n%s",
				reader.name, writer.name, dispatchTestLogin, approval.Version, reviews, reader.name, reader.process.Output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForBlockAsk waits for the ask settlement indexed from the decision block blockID of the
// issue's documents, and returns its id.
func waitForBlockAsk(t *testing.T, p *dispatchProcess, issueKey, blockID string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var asks []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
		}
		getJSON(t, p, "/api/v1/issues/"+issueKey+"/asks?state=all", &asks)
		for _, ask := range asks {
			if ask.BlockID != nil && *ask.BlockID == blockID {
				return ask.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ask was indexed from the block %s of %s within 20s: %+v", blockID, issueKey, asks)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// referencesMarkdown is a body holding every reference form of a build's text/testdata that stands
// alone as a document body: each body of its text table, and each link of its href table as the
// dashboard writes it and as its dispatch:// form, a paragraph each. Two kinds of text row are left
// out, each logged:
//   - A row that names an origin of its own: its links are references only to a server at that
//     origin, and both builds serve referenceServerURL here.
//   - A row that opens a typed block (a line that starts with `:::`): the table holds the opening
//     line alone, so the text reader is tested on an attribute's value. A document must close
//     the block, and the writer's manifest must declare the attribute. The row carries neither,
//     so the writer refuses it as a document (`typed block "callout" is unclosed at document
//     level`).
//
// Every other row must stand alone. A writer that refuses one fails the row, so a refused row is
// never dropped without a trace.
func referencesMarkdown(t *testing.T, fixtures string) string {
	t.Helper()
	var texts []struct {
		Body   string `json:"body"`
		Origin string `json:"origin"`
	}
	var hrefs []struct {
		Href string `json:"href"`
		Ref  string `json:"ref"`
	}
	for name, into := range map[string]any{"dispatch-text-references.json": &texts, "dispatch-href-references.json": &hrefs} {
		raw, err := os.ReadFile(filepath.Join(fixtures, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
	}
	paragraphs := []string{"# Every reference form"}
	for _, row := range texts {
		switch {
		case row.Origin != "" && row.Origin != referenceServerURL:
			t.Logf("left out the text row %q: it names the origin %s", row.Body, row.Origin)
		case opensTypedBlock(row.Body):
			t.Logf("left out the text row %q: it opens a typed block it does not close", row.Body)
		default:
			paragraphs = append(paragraphs, row.Body)
		}
	}
	for _, row := range hrefs {
		paragraphs = append(paragraphs, referenceServerURL+row.Href, row.Ref)
	}
	return strings.Join(paragraphs, "\n\n") + "\n"
}

// opensTypedBlock reports whether a line of body starts with the `:::` of a typed block's fence.
func opensTypedBlock(body string) bool {
	for line := range strings.Lines(body) {
		if strings.HasPrefix(line, ":::") {
			return true
		}
	}
	return false
}

// documentRefs is every refs row the document is the source of, as `kind id`, sorted.
func documentRefs(t *testing.T, builds *twoBuilds, artifactID string) []string {
	t.Helper()
	rows, err := builds.database.Pool.Query(context.Background(), `
		select to_kind || ' ' || to_id from refs
		where from_kind = 'artifact' and from_id = $1
		order by 1
	`, artifactID)
	if err != nil {
		t.Fatalf("read the document's refs: %v", err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatalf("read a refs row: %v", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the document's refs: %v", err)
	}
	return refs
}

// referencesSettle is how long the references row waits after Y's edit before it reads the refs
// rows again: past the default two-second settle delay, so a settlement that rewrites the document
// after the edit has rewritten it too.
const referencesSettle = 4 * time.Second

// requireReferencesKeptAcrossBuilds creates the body holding every reference form of X's
// text/testdata on X, appends a paragraph on Y, and checks the refs rows after the rewrite and
// after it settled (requireRefsKept).
func requireReferencesKeptAcrossBuilds(t *testing.T, builds *twoBuilds, writer, reader documentSide, project string) {
	_, artifactID := createDocument(t, writer.process, project, "Every reference form", referencesMarkdown(t, writer.fixtures))
	written := documentRefs(t, builds, artifactID)
	if len(written) == 0 {
		t.Fatalf("the %s wrote no refs rows for the body holding every reference form of its text/testdata", writer.name)
	}
	appendParagraph(t, reader.process, artifactID, "Rewritten on the "+reader.name+".")
	requireRefsKept(t, documentRefs(t, builds, artifactID), written, "after the "+reader.name+" rewrote the body", writer.name)
	time.Sleep(referencesSettle)
	requireRefsKept(t, documentRefs(t, builds, artifactID), written, "after the "+reader.name+"'s rewrite settled", writer.name)
	t.Logf("the %d refs rows the %s wrote survived the %s's rewrite", len(written), writer.name, reader.name)
}

// requireRefsKept fails unless got holds every row of written: Y keeps each edge X wrote, and an
// edge of a kind Y does not know survives Y's rewrite. Y may add rows, but only of a kind that
// written holds no row of: a kind Y extracts and X does not, such as `block` from a `#b-` link
// when X is a build before the block graph. A row Y adds of a kind X wrote means the two builds
// read one reference form as different targets.
func requireRefsKept(t *testing.T, got, written []string, when, writerName string) {
	t.Helper()
	kinds := map[string]bool{}
	for _, row := range written {
		kind, _, _ := strings.Cut(row, " ")
		kinds[kind] = true
	}
	var lost, added []string
	for _, row := range written {
		if !slices.Contains(got, row) {
			lost = append(lost, row)
		}
	}
	for _, row := range got {
		if kind, _, _ := strings.Cut(row, " "); kinds[kind] && !slices.Contains(written, row) {
			added = append(added, row)
		}
	}
	if len(lost) > 0 || len(added) > 0 {
		t.Fatalf("%s, the body's refs rows lack %d of the %d the %s wrote\n%s\nand hold %d rows of kinds the %s wrote that it did not write\n%s\nall rows now:\n%s",
			when, len(lost), len(written), writerName, refRowsText(lost), len(added), writerName, refRowsText(added), refRowsText(got))
	}
}

// refRowsText is rows one per line, each row longer than 120 bytes cut to its first 120 and its
// length: the text tables hold target ids of about 2 KB.
func refRowsText(rows []string) string {
	var text strings.Builder
	for _, row := range rows {
		if len(row) > 120 {
			row = fmt.Sprintf("%s… (%d bytes)", row[:120], len(row))
		}
		text.WriteString("\t" + row + "\n")
	}
	return text.String()
}

// ───── messages and subscription settings ───────────────────────────────────

// settingsPolls is two of the delivery intake's settings polls (delivery.settingsPollInterval,
// 30 s, unexported) and a margin: the window a standby of the build before generations compares
// the consumers' settings with its own, twice.
const settingsPolls = 2*30*time.Second + 5*time.Second

// consumerGenerationKey is the consumer metadata key this tree stamps its generation under
// (delivery.consumerGenerationKey, unexported).
const consumerGenerationKey = "dispatch.consumer-generation"

// consumerSettings is what a delivery durable's settings must keep while two builds run.
type consumerSettings struct {
	FilterSubject string
	AckWait       time.Duration
	MaxAckPending int
	Generation    string
}

// streamSettings is what the notification stream's settings must keep while two builds run.
type streamSettings struct {
	MaxAge     time.Duration
	Duplicates time.Duration
	Subjects   []string
	Replicas   int
}

type busSettings struct {
	Consumers map[string]consumerSettings
	Stream    streamSettings
}

func readBusSettings(t *testing.T, js natsgo.JetStreamContext) busSettings {
	t.Helper()
	settings := busSettings{Consumers: map[string]consumerSettings{}}
	for _, durable := range deliveryDurables {
		info, err := js.ConsumerInfo(bus.Stream, durable)
		if err != nil {
			t.Fatalf("read the durable %s: %v", durable, err)
		}
		settings.Consumers[durable] = consumerSettings{
			FilterSubject: info.Config.FilterSubject,
			AckWait:       info.Config.AckWait,
			MaxAckPending: info.Config.MaxAckPending,
			Generation:    info.Config.Metadata[consumerGenerationKey],
		}
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the notification stream: %v", err)
	}
	settings.Stream = streamSettings{
		MaxAge:     info.Config.MaxAge,
		Duplicates: info.Config.Duplicates,
		Subjects:   slices.Clone(info.Config.Subjects),
		Replicas:   info.Config.Replicas,
	}
	return settings
}

func requireBusSettings(t *testing.T, js natsgo.JetStreamContext, want busSettings, when string) {
	t.Helper()
	if got := readBusSettings(t, js); fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("%s, the bus settings are\n%+v\nwant them unchanged from\n%+v", when, got, want)
	}
}

// pullRequestFake is the loopback GitHub both builds' intakes call while they handle a merged pull
// request: the App's installation and token, the pull request, and its first commit. It is one
// state served by one server per build (serve), so each handling is recorded with the build that
// did it. The first commit is the handler's last call to GitHub before it writes the pull
// request's rows and acknowledges the message, and the fake takes delay to answer it, which spreads
// the messages' handling over a stop. A handling counts as done when that answer is sent, so a
// handling counted done can still be unacknowledged; one whose process went away while it waited
// (the request's context ended) is counted as abandoned instead, since a message a process stopped
// handling unfinished is one NATS must deliver again. Anything else is answered with an empty list.
type pullRequestFake struct {
	delay time.Duration

	mu        sync.Mutex
	done      map[int][]handling
	abandoned map[int]int
}

// handling is one done handling of a pull request: the build whose server answered it, and when.
type handling struct {
	build string
	at    time.Time
}

func (h handling) String() string { return h.build + " at " + h.at.Format("15:04:05.000") }

func startPullRequestFake(delay time.Duration) *pullRequestFake {
	return &pullRequestFake{delay: delay, done: map[int][]handling{}, abandoned: map[int]int{}}
}

// serve starts a server on the fake's state whose done handlings are build's, and returns its URL:
// the DISPATCH_GITHUB_API_BASE of build's processes.
func (f *pullRequestFake) serve(t *testing.T, build string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":1,"app_slug":"twobuild","permissions":{"contents":"read","actions":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_twobuild","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"number":%s,"title":"A pull request","user":{"login":"octocat"},"labels":[],"merged_at":"2026-10-27T00:00:00Z","additions":1,"deletions":1}`, r.PathValue("number"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			f.mu.Lock()
			f.abandoned[number]++
			f.mu.Unlock()
			return
		}
		_, _ = io.WriteString(w, `[{"sha":"0000000000000000000000000000000000000001","commit":{"message":"A change.","author":{"date":"2026-10-26T00:00:00Z"},"committer":{"date":"2026-10-26T00:00:00Z"}}}]`)
		f.mu.Lock()
		f.done[number] = append(f.done[number], handling{build: build, at: time.Now()})
		f.mu.Unlock()
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// handled is each pull request's done handlings, by number, in the order they were done, and how
// many times a stopping process abandoned it.
func (f *pullRequestFake) handled() (done map[int][]handling, abandoned map[int]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	done = make(map[int][]handling, len(f.done))
	for number, handlings := range f.done {
		done[number] = slices.Clone(handlings)
	}
	return done, maps.Clone(f.abandoned)
}

// seedPullRequestSettings writes the delivery settings the intakes route by: acme/deploy as the
// deploy repository and octocat as the one author whose merged pull requests are read.
func seedPullRequestSettings(t *testing.T, builds *twoBuilds) {
	t.Helper()
	if _, err := delivery.PutSettings(context.Background(), builds.database.Pool, delivery.DeliverySettings{
		DeployRepo:           "acme/deploy",
		DeployWorkflowPath:   ".github/workflows/deploy.yaml",
		PRChecksWorkflowPath: ".github/workflows/pr.yaml",
		ProductionJobNames:   []string{"production"},
		PopulationAuthors:    []string{"octocat"},
	}, model.Actor{Kind: "user", ID: dispatchTestLogin}); err != nil {
		t.Fatalf("seed the delivery settings: %v", err)
	}
}

// publishMergedPullRequest publishes a GitHub-shaped message, a merge of pull request number of
// acme/deploy by octocat, on notifications.github.acme.deploy.pr.<number>, with delivery as its
// message id.
func publishMergedPullRequest(t *testing.T, js natsgo.JetStreamContext, number int, delivery string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "pr", "action": "closed", "merged": "true",
		"repo": "acme/deploy", "number": strconv.Itoa(number), "author": "octocat",
	})
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	envelope := contracts.Envelope{
		EventID:       delivery,
		Source:        "github",
		SourceEventID: delivery,
		Topic:         contracts.GithubResourceSubject("acme", "deploy", "pr", strconv.Itoa(number)),
		DedupeKey:     delivery,
		IssuedAt:      time.Now().Unix(),
		Payload:       string(payload),
		TraceID:       delivery,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	if _, err := js.Publish(envelope.Topic, data, natsgo.MsgId(delivery)); err != nil {
		t.Fatalf("publish %s: %v", envelope.Topic, err)
	}
}

// boundLines is how many times p has logged that it bound durable.
func boundLines(p *dispatchProcess, durable string) int {
	return len(p.logLines(`msg="dispatch delivery: NATS consumer bound"`, "name="+durable))
}

// waitForBound waits for p to have logged that it bound durable more than before times.
func waitForBound(t *testing.T, p *dispatchProcess, name, durable string, before int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for boundLines(p, durable) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("the %s did not bind %s within %v:\n%s", name, durable, within, p.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// standbyWindow is how long a restarted process of this tree is watched while a process of the
// build before holds delivers from a durable: a hold's whole TTL (delivery.intakeHoldTTL, 15 s),
// two bind attempts (delivery.durableBindInterval, 2 s each) and a margin. A hold the process left
// behind when it stopped has lapsed within it, so a take-over that waits for that lapse is seen.
const standbyWindow = 15*time.Second + 2*2*time.Second + 3*time.Second

// consumerBinding is where the durable delivers and whether a subscriber is bound there.
func consumerBinding(t *testing.T, js natsgo.JetStreamContext, durable string) string {
	t.Helper()
	info, err := js.ConsumerInfo(bus.Stream, durable)
	if err != nil {
		t.Fatalf("read the durable %s: %v", durable, err)
	}
	return fmt.Sprintf("delivering to %s (bound %t)", info.Config.DeliverSubject, info.PushBound)
}

// pullRequestMessages is how many GitHub-shaped messages the row publishes: half across a stop of
// each build while it holds the intake.
const pullRequestMessages = 50

// publishAcrossStop publishes a merged pull request for each of numbers while holder delivers from
// the events durable, stops holder gracefully once ten of them are handled, and waits for standby to
// take the durable over and for every one of numbers to be handled. It returns the time holder's
// stop returned, which is after holder's process exited: every handling holder did is before it.
func publishAcrossStop(t *testing.T, builds *twoBuilds, js natsgo.JetStreamContext, fake *pullRequestFake, numbers []int, holder, standby *dispatchProcess) time.Time {
	t.Helper()
	handledOf := func() int {
		done, _ := fake.handled()
		count := 0
		for _, number := range numbers {
			if len(done[number]) > 0 {
				count++
			}
		}
		return count
	}
	for _, number := range numbers {
		publishMergedPullRequest(t, js, number, fmt.Sprintf("twobuild-delivery-%d-%s", number, strings.ToLower(uniqueProjectKey(t, ""))))
	}
	// The holder stops with the messages part handled: past the first ten, well before the last,
	// at 300 ms a message.
	deadline := time.Now().Add(60 * time.Second)
	for handledOf() < 10 {
		if time.Now().After(deadline) {
			t.Fatalf("the %s handled %d of %d pull requests within 60s:\n%s", builds.buildName(holder), handledOf(), len(numbers), holder.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	standbyBound := boundLines(standby, bus.DispatchDeliveryDurable)
	stop(t, holder)
	stopped := time.Now()
	t.Logf("stopped the %s with %d of %d pull requests handled", builds.buildName(holder), handledOf(), len(numbers))
	waitForBound(t, standby, builds.buildName(standby), bus.DispatchDeliveryDurable, standbyBound, 60*time.Second)
	deadline = time.Now().Add(120 * time.Second)
	for handledOf() < len(numbers) {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d pull requests were handled within 120s of the %s's stop:\n%s",
				handledOf(), len(numbers), builds.buildName(holder), standby.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	return stopped
}

// pullRequestNumbers is count pull request numbers from first.
func pullRequestNumbers(first, count int) []int {
	numbers := make([]int, count)
	for index := range numbers {
		numbers[index] = first + index
	}
	return numbers
}

// Messages and subscription settings pass between the two builds with none skipped or repeated,
// but for the one repeat at-least-once delivery allows. Issues created on either build produce
// outbox events whose ids are exactly the ids the stream holds. GitHub-shaped messages are each
// handled once, the set of their delivery ids the set handled, across a graceful stop of the build
// that holds the intake: this tree first, then the previous build. The one message the previous
// build had in its handler when it was stopped may be handled again, by this tree after the
// previous build exited: the previous build's stop does not wait for the message in its handler
// (delivery.StartIntakes of that release says so), so its process can exit after the handling's
// last GitHub call and before it acknowledges the message, and NATS delivers that message again to
// this tree when it takes the durable over. This tree's stop waits for the message in its handler
// and acknowledges it, so every other repeat fails the test, one whose first handling is this
// tree's included. The delivery durables' filter, ack wait, ack bound and generation, and the
// stream's age, duplicate window, subjects and replicas, stay as they are across two settings polls
// with both builds running, and across a restart of each build in turn. While a process of the
// previous build, which holds no hold, delivers from the durable, a restarted process of this tree
// stands by rather than take the durable from it.
func TestTwoBuildsPassMessagesBothWays(t *testing.T) {
	builds := newTwoBuilds(t)
	seedPullRequestSettings(t, builds)
	// One App key for both builds; each build calls its own server of the one fake, so each
	// handling is recorded with the build that did it.
	fake := startPullRequestFake(300 * time.Millisecond)
	app := githubAppEnv(t, "")
	previousEnv, currentEnv := maps.Clone(app), maps.Clone(app)
	previousEnv["DISPATCH_GITHUB_API_BASE"] = fake.serve(t, builds.previousName())
	currentEnv["DISPATCH_GITHUB_API_BASE"] = fake.serve(t, builds.currentName())

	// This tree creates the durables, stamped with its generation, and holds them; the previous
	// build starts beside it as a standby, which is a rolling release's first moment.
	builds.startCurrent(t, currentEnv)
	for _, durable := range deliveryDurables {
		waitForBound(t, builds.current(), builds.buildName(builds.current()), durable, 0, 60*time.Second)
	}
	builds.startPrevious(t, previousEnv)

	js, err := testnats.Connect(t, builds.nats).JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	head := streamHead(t, js)
	baseline := readBusSettings(t, js)
	t.Logf("bus settings with both builds running: %+v", baseline)

	t.Run("outbox events", func(t *testing.T) {
		project := uniqueProjectKey(t, "OBX")
		builds.current().createProject(t, project, "Two builds")
		for _, writer := range []*dispatchProcess{builds.previous(), builds.current(), builds.previous(), builds.current()} {
			key, _ := createDocument(t, writer, project, "Created on the "+builds.buildName(writer), "spec")
			writer.call(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", `{"body":"A comment."}`, http.StatusCreated)
		}
		waitForEveryEventPublished(t, builds.database, 60*time.Second)
		events := publishedEventIDs(t, builds)
		streamed := map[string]bool{}
		for _, item := range readStreamFrom(t, js, head) {
			if item.Source == "dispatch" {
				streamed[item.SourceEventID] = true
			}
		}
		if got, want := slices.Sorted(maps.Keys(streamed)), slices.Sorted(maps.Keys(events)); !slices.Equal(got, want) {
			t.Errorf("the stream holds the events\n%v\nwant exactly the events both builds wrote\n%v", got, want)
		}
	})

	time.Sleep(settingsPolls)
	requireBusSettings(t, js, baseline, "after two settings polls with both builds running")

	if boundLines(builds.previous(), bus.DispatchDeliveryDurable) != 0 {
		t.Fatalf("the %s bound %s while the %s held it:\n%s", builds.buildName(builds.previous()), bus.DispatchDeliveryDurable,
			builds.buildName(builds.current()), builds.previous().Output.String())
	}
	half := pullRequestMessages / 2
	publishAcrossStop(t, builds, js, fake, pullRequestNumbers(1001, half), builds.current(), builds.previous())

	// This tree, stopped above, comes back while the previous build delivers from the durable
	// under no hold, as every build before holds does: it stands by and never takes the binding
	// from it.
	binding := consumerBinding(t, js, bus.DispatchDeliveryDurable)
	builds.startCurrent(t, currentEnv)
	time.Sleep(standbyWindow)
	if bound, now := boundLines(builds.current(), bus.DispatchDeliveryDurable), consumerBinding(t, js, bus.DispatchDeliveryDurable); bound > 0 || now != binding {
		t.Errorf("the restarted %s took %s from the %s, which delivers from it under no hold: the durable was %s and is %s, and the restarted build logged %d binds:\n%s",
			builds.buildName(builds.current()), bus.DispatchDeliveryDurable, builds.buildName(builds.previous()), binding, now, bound, builds.current().Output.String())
	}
	requireBusSettings(t, js, baseline, "after the current build restarted")

	// The previous build, now the holder, stops with the second half part handled; this tree
	// takes the durables over, and the previous build comes back.
	previousStopped := publishAcrossStop(t, builds, js, fake, pullRequestNumbers(1001+half, pullRequestMessages-half), builds.previous(), builds.current())
	builds.startPrevious(t, previousEnv)
	requireBusSettings(t, js, baseline, "after the previous build restarted")

	// Read last, after both restarts, so a message either stop had redelivered and handled again
	// is counted. A handling a stopping process abandoned is not one: NATS delivers that message
	// again, which is what at-least-once delivery is for. Neither is the handling the previous
	// build had in its handler when it was stopped, done but not acknowledged when its process
	// exited (see the doc comment): that message may be handled again, once, by this tree.
	time.Sleep(5 * time.Second)
	done, abandoned := fake.handled()
	previousName, currentName := builds.previousName(), builds.currentName()
	var previousLast time.Time
	for _, handlings := range done {
		for _, h := range handlings {
			if h.build == previousName && !h.at.After(previousStopped) && h.at.After(previousLast) {
				previousLast = h.at
			}
		}
	}
	var twice, again, unhandled []string
	for number, handlings := range done {
		switch {
		case len(handlings) == 1:
		case len(handlings) == 2 &&
			handlings[0].build == previousName && handlings[0].at.Equal(previousLast) &&
			handlings[1].build == currentName && handlings[1].at.After(previousStopped):
			again = append(again, fmt.Sprintf("#%d %v", number, handlings))
		default:
			twice = append(twice, fmt.Sprintf("#%d %d times %v", number, len(handlings), handlings))
		}
	}
	for _, number := range pullRequestNumbers(1001, pullRequestMessages) {
		if len(done[number]) == 0 {
			unhandled = append(unhandled, "#"+strconv.Itoa(number))
		}
	}
	if len(done) != pullRequestMessages || len(twice) > 0 || len(unhandled) > 0 {
		slices.Sort(twice)
		t.Errorf("the %d GitHub messages were handled as %d distinct pull requests; handled more than once: %v; never handled: %v (the %s exited at %s)",
			pullRequestMessages, len(done), twice, unhandled, previousName, previousStopped.Format("15:04:05.000"))
	}
	if len(abandoned) > 0 {
		t.Logf("handlings a stopping process abandoned unfinished, each handled again after: %v", abandoned)
	}
	if len(again) > 0 {
		t.Logf("the handling the %s had in its handler when it was stopped (it exited at %s), handled again by the %s: %v",
			previousName, previousStopped.Format("15:04:05.000"), currentName, again)
	}
}

// publishedEventIDs is the id of every event the outbox has published, as the stream's envelopes
// carry it (source_event_id).
func publishedEventIDs(t *testing.T, builds *twoBuilds) map[string]bool {
	t.Helper()
	rows, err := builds.database.Pool.Query(context.Background(), `select id::text from events where published_at is not null`)
	if err != nil {
		t.Fatalf("read the published events: %v", err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read an event id: %v", err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the published events: %v", err)
	}
	return ids
}
