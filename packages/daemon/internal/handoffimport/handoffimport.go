// Package handoffimport is `legion handoffs import`, the one-time and re-runnable move of the
// handoff files Legion's workers committed under `.legion/<KEY>/` onto their Dispatch issues
// (INFRA-177). For every issue the running daemon holds in a phase other than done, it reads the
// issue's branch `legion/<KEY>` as the implement App, writes each of `.legion/<KEY>/plan.json`,
// `implement.json`, `test.json` and `review.json` to Dispatch as that phase's handoff record under
// the daemon's Dispatch session, and then removes all of `.legion/` from the branch in one commit
// moved there by a non-forced ref update.
package handoffimport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// handoffDir is the directory the workers committed their handoffs under, and that the removal
// commit takes off every branch whole: it can hold other issues' directories, merged in from
// branches that carried them.
const handoffDir = ".legion"

// phaseWords are the handoff words a file-backed phase ends with (phase.HandoffFile), in the
// workflow's order: the file `.legion/<KEY>/<word>.json`, and the phase of its Dispatch record.
var phaseWords = func() []string {
	var words []string
	for _, p := range phase.All {
		if word, ok := phase.HandoffFile(p); ok {
			words = append(words, word)
		}
	}
	return words
}()

// removalAttempts is how many times the removal is tried on a branch that moved between its read
// and the ref update, the branch read again before each, before the issue is reported failed.
const removalAttempts = 3

// stateTimeout bounds the read of the daemon's state.
const stateTimeout = 30 * time.Second

// Tokens mints the implement App's installation token for a repository owner (appauth.Manager).
type Tokens interface {
	Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error)
}

// Dispatch is the handoff surface of Dispatch the import reads and writes (dispatch.HTTPClient):
// the newest record of an issue's phase, with its body, and a new record written under the
// daemon's session (dispatch.DaemonSession).
type Dispatch interface {
	LatestHandoff(ctx context.Context, key, phase string) (dispatch.Handoff, bool, error)
	WriteHandoff(ctx context.Context, key, phase, body string) (dispatch.Handoff, error)
}

// Options is what a run reads from and writes to.
type Options struct {
	// Project is the configuration's project, which the daemon's state must name.
	Project string
	// DaemonURL is the daemon's API root, whose unauthenticated GET /legion/v1/state names the
	// issues in the workflow.
	DaemonURL string
	// GitHubAPI is GitHub's API root; empty is https://api.github.com.
	GitHubAPI string
	Tokens    Tokens
	Dispatch  Dispatch
	// DryRun reads everything and prints what the run would do, writing nothing.
	DryRun bool
}

// outcome is what the run did, or found to do, with one phase's file or one branch.
type outcome int

const (
	imported outcome = iota
	unchanged
	keptWorker
	removed
	noBranch
	failed
)

// countLabels name each outcome in the closing count line, in its order.
var countLabels = []struct {
	outcome    outcome
	label, dry string
}{
	{imported, "imported", "would import"},
	{unchanged, "unchanged", "unchanged"},
	{keptWorker, "kept worker record", "kept worker record"},
	{removed, "removed .legion/", "would remove .legion/"},
	{noBranch, "no branch", "no branch"},
	{failed, "failed", "failed"},
}

// result is one printed line's outcome and its words.
type result struct {
	outcome outcome
	text    string
}

// failure is the failed line for err: `failed: <err>`.
func failure(err error) result { return result{failed, "failed: " + err.Error()} }

// runner is one run: its options, and the count of each outcome printed so far.
type runner struct {
	Options
	out    io.Writer
	counts map[outcome]int
}

// Run is `legion handoffs import` once the configuration is read: one line per issue and phase on
// stdout — `<KEY> <word>: imported v<n>`, `unchanged`, `kept worker record v<n>` or `failed:
// <why>`, then `<KEY>: removed .legion/ at <sha>`, `unchanged` (the branch carries nothing under
// .legion/), `no branch` or `failed: <why>` — and a closing count of each. Under DryRun the lines
// say what the run would do (`would import v<n>`, `would remove .legion/ …`) and nothing is
// written. A run that cannot start, or that meets a Dispatch without the handoff routes, says why
// on stderr. It answers the command's exit: 0 when every line went through, 1 otherwise.
func Run(ctx context.Context, o Options, stdout, stderr io.Writer) int {
	r := &runner{Options: o, out: stdout, counts: map[outcome]int{}}
	err := r.run(ctx)
	r.printCounts()
	if err != nil {
		fmt.Fprintf(stderr, "legion handoffs import: %v\n", err)
		return 1
	}
	if r.counts[failed] > 0 {
		return 1
	}
	return 0
}

func (r *runner) run(ctx context.Context) error {
	issues, err := r.readState(ctx)
	if err != nil {
		return err
	}
	for _, issue := range issues {
		if err := r.issue(ctx, issue); err != nil {
			return err
		}
	}
	return nil
}

// workflowIssue is one issue of the daemon's state the run reads: its key and repository.
type workflowIssue struct {
	key, repo string
}

// readState is every issue the daemon holds in a phase other than done, with a repository, by key:
// an issue with no repository is an operator's claim, which the workflow does not record and no
// branch of which a worker committed handoffs to. The state is decoded into only the fields the run
// reads, so a daemon of an earlier contract answers it as well.
func (r *runner) readState(ctx context.Context) ([]workflowIssue, error) {
	timed, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	address := strings.TrimRight(r.DaemonURL, "/") + "/legion/v1/state"
	request, err := http.NewRequestWithContext(timed, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("read the daemon's state at %s: %w", address, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read the daemon's state at %s: %w", address, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read the daemon's state at %s: %w", address, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the daemon's state at %s answered %d: %s", address, response.StatusCode, strings.TrimSpace(string(body)))
	}
	var state struct {
		Daemon struct {
			Project string `json:"project"`
		} `json:"daemon"`
		Issues map[string]struct {
			Phase phase.Phase `json:"phase"`
			Repo  string      `json:"repo"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, fmt.Errorf("decode the daemon's state at %s: %w", address, err)
	}
	if r.Project != "" && state.Daemon.Project != r.Project {
		return nil, fmt.Errorf("the daemon at %s serves project %q, and the configuration names %q", r.DaemonURL, state.Daemon.Project, r.Project)
	}
	var issues []workflowIssue
	for key, issue := range state.Issues {
		if issue.Phase != phase.Done && issue.Repo != "" {
			issues = append(issues, workflowIssue{key: key, repo: issue.Repo})
		}
	}
	slices.SortFunc(issues, func(a, b workflowIssue) int { return strings.Compare(a.key, b.key) })
	return issues, nil
}

// issue imports one issue's handoff files and removes .legion/ from its branch, reading the branch
// again and starting over when it moved under the removal, at most removalAttempts times. Each
// phase's line is the last attempt's, except that a file imported by an earlier attempt and found
// unchanged by a later one stays imported. An error ends the whole run after the lines gathered: a
// Dispatch without the handoff routes, which every issue would meet the same way.
func (r *runner) issue(ctx context.Context, issue workflowIssue) error {
	repo, err := ghrepo.Parse("the daemon's repository of "+issue.key, issue.repo)
	if err != nil {
		r.print(issue.key, "", failure(err))
		return nil
	}
	lease, err := r.Tokens.Token(ctx, appauth.Implement, repo.Owner())
	if err != nil {
		r.print(issue.key, "", result{failed, fmt.Sprintf("failed: mint the implement App's token for %s: %v", repo.Owner(), err)})
		return nil
	}
	b := branch{key: issue.key, repo: repo, github: githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(r.GitHubAPI, repo)}}

	phases := map[string]result{}
	var last result
	var stop error
	for attempt := 1; ; attempt++ {
		var moved bool
		last, moved, stop = r.attempt(ctx, b, phases)
		if !moved || stop != nil {
			break
		}
		if attempt == removalAttempts {
			last = result{failed, fmt.Sprintf("failed: %s moved under the removal commit %d times; run the import again", b.ref(), removalAttempts)}
			break
		}
	}
	for _, word := range phaseWords {
		if phaseResult, ok := phases[word]; ok {
			r.print(issue.key, word, phaseResult)
		}
	}
	if stop != nil {
		return stop
	}
	r.print(issue.key, "", last)
	return nil
}

// attempt is one pass over the branch at its current head: each phase's file into phases, then the
// removal. moved says the branch moved between the read and the ref update.
func (r *runner) attempt(ctx context.Context, b branch, phases map[string]result) (last result, moved bool, err error) {
	head, found, err := b.head(ctx)
	if err != nil {
		return failure(err), false, nil
	}
	if !found {
		return result{noBranch, "no branch"}, false, nil
	}
	top, err := b.list(ctx, handoffDir, head)
	if err != nil {
		return failure(err), false, nil
	}
	if len(top) == 0 {
		return result{unchanged, "unchanged"}, false, nil
	}
	allThere := true
	if slices.ContainsFunc(top, func(e contentEntry) bool { return e.Name == b.key && e.Type == "dir" }) {
		files, err := b.list(ctx, handoffDir+"/"+b.key, head)
		if err != nil {
			return failure(err), false, nil
		}
		for _, word := range phaseWords {
			name := word + ".json"
			if !slices.ContainsFunc(files, func(e contentEntry) bool { return e.Name == name && e.Type == "file" }) {
				continue
			}
			current, err := r.phase(ctx, b, head, word)
			if err != nil {
				return result{}, false, err
			}
			if previous, ok := phases[word]; !ok || previous.outcome != imported || current.outcome != unchanged {
				phases[word] = current
			}
			if current.outcome == failed {
				allThere = false
			}
		}
	}
	if !allThere {
		return result{failed, "failed: .legion/ stays on the branch until every handoff above is on Dispatch"}, false, nil
	}
	return r.remove(ctx, b, head)
}

// phase writes `.legion/<KEY>/<word>.json` at head as the issue's newest record of the phase when
// Dispatch has none, or when its newest is one the import wrote, under the daemon's session, whose
// body differs from the file. A newest record any other session wrote is a worker's, and is never
// written over. A Dispatch that answers the handoff routes 404 without HANDOFF_NOT_FOUND has no
// such route, and stops the run.
func (r *runner) phase(ctx context.Context, b branch, head, word string) (result, error) {
	path := handoffDir + "/" + b.key + "/" + word + ".json"
	text, err := b.file(ctx, path, head)
	if err != nil {
		return failure(err), nil
	}
	latest, found, err := r.Dispatch.LatestHandoff(ctx, b.key, word)
	if dispatch.Missing(err) {
		return result{}, noRoute("GET /api/v1/issues/"+b.key+"/handoffs/"+word+"/latest", err)
	}
	if err != nil {
		return result{failed, fmt.Sprintf("failed: read the newest %s handoff of %s from Dispatch: %v", word, b.key, err)}, nil
	}
	if found && !latest.Author.IsSession(dispatch.DaemonSession(b.key)) {
		return result{keptWorker, fmt.Sprintf("kept worker record v%d", latest.Version)}, nil
	}
	if found && latest.Body == text {
		return result{unchanged, "unchanged"}, nil
	}
	if r.DryRun {
		return result{imported, fmt.Sprintf("would import v%d", latest.Version+1)}, nil
	}
	written, err := r.Dispatch.WriteHandoff(ctx, b.key, word, text)
	if dispatch.Missing(err) {
		return result{}, noRoute("POST /api/v1/issues/"+b.key+"/handoffs/"+word, err)
	}
	if err != nil {
		return result{failed, fmt.Sprintf("failed: write %s to Dispatch: %v", path, err)}, nil
	}
	return result{imported, fmt.Sprintf("imported v%d", written.Version)}, nil
}

// noRoute is the stop of a run against a Dispatch that answered route 404.
func noRoute(route string, err error) error {
	return fmt.Errorf("Dispatch answered %s with 404 (%v): it has no handoff routes; deploy Dispatch with them before the import (docs/kubernetes.md)", route, err)
}

// remove makes one commit on head that deletes every path under .legion/ and moves the branch to
// it with a non-forced ref update. moved says GitHub refused the update as not a fast-forward.
func (r *runner) remove(ctx context.Context, b branch, head string) (last result, moved bool, err error) {
	rootTree, deletions, err := b.removalSet(ctx, head)
	if err != nil {
		return failure(err), false, nil
	}
	if r.DryRun {
		paths := fmt.Sprintf("%d paths", len(deletions))
		if len(deletions) == 1 {
			paths = "1 path"
		}
		return result{removed, fmt.Sprintf("would remove .legion/ (%s) above %s", paths, head)}, false, nil
	}
	commit, err := b.removalCommit(ctx, head, rootTree, deletions)
	if err != nil {
		return failure(err), false, nil
	}
	moved, err = b.move(ctx, commit)
	if err != nil {
		return failure(err), false, nil
	}
	if moved {
		return result{}, true, nil
	}
	return result{removed, "removed .legion/ at " + commit}, false, nil
}

// print writes one line, `<KEY> <word>: <text>` for a phase and `<KEY>: <text>` for the branch,
// and counts its outcome.
func (r *runner) print(key, word string, line result) {
	subject := key
	if word != "" {
		subject += " " + word
	}
	fmt.Fprintf(r.out, "%s: %s\n", subject, line.text)
	r.counts[line.outcome]++
}

// printCounts writes the closing line: how many lines of each outcome the run printed.
func (r *runner) printCounts() {
	parts := make([]string, 0, len(countLabels))
	for _, entry := range countLabels {
		label := entry.label
		if r.DryRun {
			label = entry.dry
		}
		parts = append(parts, fmt.Sprintf("%s %d", label, r.counts[entry.outcome]))
	}
	prefix := ""
	if r.DryRun {
		prefix = "dry run, nothing written: "
	}
	fmt.Fprintf(r.out, "%s%s\n", prefix, strings.Join(parts, ", "))
}

// branch is an issue's branch legion/<KEY> on its repository, read and written through GitHub's
// REST API as the implement App.
type branch struct {
	key    string
	repo   ghrepo.Repository
	github githubrest.Client
}

// ref is the branch's name.
func (b branch) ref() string { return claim.IssueBranch(b.key) }

// head is the commit the branch names, and whether GitHub has the branch.
func (b branch) head(ctx context.Context) (string, bool, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := b.github.Get(ctx, "/git/ref/heads/"+b.ref(), &ref)
	if notFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s of %s: %w", b.ref(), b.repo, err)
	}
	if ref.Object.SHA == "" {
		return "", false, fmt.Errorf("read %s of %s: GitHub named no commit", b.ref(), b.repo)
	}
	return ref.Object.SHA, true, nil
}

// contentEntry is one entry of a directory GitHub's contents API lists.
type contentEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// list is the directory at path in commit head through GitHub's contents API, empty when the
// commit has no such path.
func (b branch) list(ctx context.Context, path, head string) ([]contentEntry, error) {
	var entries []contentEntry
	err := b.github.Get(ctx, "/contents/"+path+"?ref="+url.QueryEscape(head), &entries)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s/ at %s of %s: %w", path, head, b.repo, err)
	}
	return entries, nil
}

// file is the text of the file at path in commit head, exactly as committed.
func (b branch) file(ctx context.Context, path, head string) (string, error) {
	var file struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := b.github.Get(ctx, "/contents/"+path+"?ref="+url.QueryEscape(head), &file); err != nil {
		return "", fmt.Errorf("read %s at %s of %s: %w", path, head, b.repo, err)
	}
	if file.Encoding != "base64" {
		return "", fmt.Errorf("read %s at %s of %s: GitHub answered its content in encoding %q, not base64", path, head, b.repo, file.Encoding)
	}
	text, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode %s at %s of %s: %w", path, head, b.repo, err)
	}
	return string(text), nil
}

// treeEntry is one entry of a git tree as GitHub's Git Data API reads and writes it. A nil SHA in
// a tree written with a base tree deletes the path from it.
type treeEntry struct {
	Path string  `json:"path"`
	Mode string  `json:"mode"`
	Type string  `json:"type"`
	SHA  *string `json:"sha"`
}

// tree is a git tree GitHub's Git Data API reads.
type tree struct {
	SHA       string      `json:"sha"`
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// removalSet is head's root tree and an entry deleting every path under .legion/ at head: every
// file, symlink and submodule its recursive tree lists, wherever under .legion/ it sits.
func (b branch) removalSet(ctx context.Context, head string) (string, []treeEntry, error) {
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := b.github.Get(ctx, "/git/commits/"+head, &commit); err != nil {
		return "", nil, fmt.Errorf("read commit %s of %s: %w", head, b.repo, err)
	}
	var root tree
	if err := b.github.Get(ctx, "/git/trees/"+commit.Tree.SHA, &root); err != nil {
		return "", nil, fmt.Errorf("read the tree of %s of %s: %w", head, b.repo, err)
	}
	at := slices.IndexFunc(root.Tree, func(e treeEntry) bool { return e.Path == handoffDir && e.Type == "tree" && e.SHA != nil })
	if at < 0 {
		return "", nil, fmt.Errorf("the tree of %s of %s has no %s/ directory, which GitHub's contents API listed", head, b.repo, handoffDir)
	}
	var below tree
	if err := b.github.Get(ctx, "/git/trees/"+*root.Tree[at].SHA+"?recursive=1", &below); err != nil {
		return "", nil, fmt.Errorf("read %s/ at %s of %s: %w", handoffDir, head, b.repo, err)
	}
	if below.Truncated {
		return "", nil, fmt.Errorf("GitHub truncated its list of %s/ at %s of %s", handoffDir, head, b.repo)
	}
	var deletions []treeEntry
	for _, entry := range below.Tree {
		if entry.Type != "tree" {
			deletions = append(deletions, treeEntry{Path: handoffDir + "/" + entry.Path, Mode: entry.Mode, Type: entry.Type})
		}
	}
	if len(deletions) == 0 {
		return "", nil, fmt.Errorf("GitHub listed nothing under %s/ at %s of %s", handoffDir, head, b.repo)
	}
	return commit.Tree.SHA, deletions, nil
}

// removalCommit writes head's tree without deletions' paths and a commit of it whose one parent is
// head, and answers the commit. Its message carries no trailer and no bracket keyword, so GitHub
// runs every check on it as on any push.
func (b branch) removalCommit(ctx context.Context, head, rootTree string, deletions []treeEntry) (string, error) {
	var written tree
	if err := b.github.Post(ctx, "/git/trees", map[string]any{"base_tree": rootTree, "tree": deletions}, &written); err != nil {
		return "", fmt.Errorf("write the tree of %s without %s/ on %s: %w", head, handoffDir, b.repo, err)
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	message := "chore(legion): " + b.key + "'s handoffs are on its Dispatch issue now"
	if err := b.github.Post(ctx, "/git/commits", map[string]any{"message": message, "tree": written.SHA, "parents": []string{head}}, &commit); err != nil {
		return "", fmt.Errorf("write the removal commit above %s on %s: %w", head, b.repo, err)
	}
	if commit.SHA == "" {
		return "", fmt.Errorf("write the removal commit above %s on %s: GitHub named no commit", head, b.repo)
	}
	return commit.SHA, nil
}

// move updates the branch to commit without force. moved says GitHub refused it with 422, which it
// answers an update that is not a fast-forward with: the branch moved since its read.
func (b branch) move(ctx context.Context, commit string) (moved bool, err error) {
	err = b.github.Patch(ctx, "/git/refs/heads/"+b.ref(), map[string]any{"sha": commit, "force": false}, nil)
	var answer *githubrest.Answer
	if errors.As(err, &answer) && answer.Status == http.StatusUnprocessableEntity {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("move %s of %s to %s: %w", b.ref(), b.repo, commit, err)
	}
	return false, nil
}

// notFound says whether err is GitHub's 404.
func notFound(err error) bool {
	var answer *githubrest.Answer
	return errors.As(err, &answer) && answer.Status == http.StatusNotFound
}
