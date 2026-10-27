package handoffimport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

const (
	owner          = "acme"
	repository     = owner + "/widgets"
	repoPrefix     = "/repos/" + repository
	implementToken = "implement-token"
	daemonSession  = "legion-daemon:LEG"
)

// handoffText is a handoff file as a worker committed it: indented, with a trailing newline, so a
// record that re-encoded it would differ.
func handoffText(key, word string) string {
	return fmt.Sprintf("{\n  \"schemaVersion\": 1,\n  \"phase\": %q,\n  \"issue\": %q,\n  \"note\": \"<&> é\"\n}\n", word, key)
}

// seededIssue is one issue of the daemon's state the stand-ins are seeded with: its phase, and the
// files its branch carries above the repository's code, nil for an issue with no branch.
type seededIssue struct {
	phase string
	files []string
}

// seed is the workflow the tests run the import over: a branch in planning, testing, reviewing,
// accepting and retro, each carrying its phases' handoff files, the retro branch also carrying
// another issue's merged .legion/OTHER-1/ directory; an issue in implementing with no branch; and a
// done issue, whose branch the import never reads.
var seed = map[string]seededIssue{
	"LEG-1": {"planning", []string{".legion/LEG-1/plan.json"}},
	"LEG-2": {"testing", []string{".legion/LEG-2/plan.json", ".legion/LEG-2/implement.json"}},
	"LEG-3": {"reviewing", []string{".legion/LEG-3/plan.json", ".legion/LEG-3/implement.json", ".legion/LEG-3/test.json"}},
	"LEG-4": {"accepting", []string{".legion/LEG-4/plan.json", ".legion/LEG-4/implement.json", ".legion/LEG-4/test.json", ".legion/LEG-4/review.json"}},
	"LEG-5": {"retro", []string{
		".legion/LEG-5/plan.json", ".legion/LEG-5/implement.json", ".legion/LEG-5/test.json", ".legion/LEG-5/review.json",
		".legion/OTHER-1/plan.json", ".legion/OTHER-1/implement.json", ".legion/OTHER-1/notes/review-thread.md",
	}},
	"LEG-6": {"implementing", nil},
	"LEG-7": {"done", []string{".legion/LEG-7/plan.json"}},
}

// commit is one commit of the GitHub stand-in: its parent, message, and every file it holds.
type commit struct {
	parent, message string
	files           map[string]string
}

// treeRef is a tree of the GitHub stand-in: the directory dir ("" for the root) of files.
type treeRef struct {
	files map[string]string
	dir   string
}

// refUpdate is one PATCH of a branch the stand-in received.
type refUpdate struct {
	branch, sha string
	force       *bool
}

// world is the stand-in of GitHub's REST API, Dispatch's handoff routes and the daemon's state.
type world struct {
	t  *testing.T
	mu sync.Mutex

	states   map[string]seededIssue
	commits  map[string]commit
	branches map[string]string
	trees    map[string]treeRef
	next     int
	// writes is every GitHub request that writes, as "METHOD path".
	writes  []string
	updates []refUpdate
	// moves is how many more times a branch's ref update finds it moved by another push.
	moves map[string]int

	handoffs map[string]map[string][]handoff
	// dispatchWrites counts handoff writes Dispatch took.
	dispatchWrites int
	noRoutes       bool
	noWriteRoute   bool
}

type handoff struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Author  struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"author"`
	Body string `json:"body"`
}

func newWorld(t *testing.T) *world {
	w := &world{
		t: t, states: map[string]seededIssue{}, commits: map[string]commit{}, branches: map[string]string{},
		trees: map[string]treeRef{}, moves: map[string]int{}, handoffs: map[string]map[string][]handoff{},
	}
	base := w.commit("", "initial", map[string]string{"README.md": "# widgets\n", "src/main.go": "package main\n"})
	for key, issue := range seed {
		w.states[key] = issue
		if issue.files == nil {
			continue
		}
		files := maps.Clone(w.commits[base].files)
		for _, path := range issue.files {
			parts := strings.Split(path, "/")
			files[path] = handoffText(parts[1], strings.TrimSuffix(parts[len(parts)-1], ".json"))
		}
		w.branches["legion/"+key] = w.commit(base, key+": the work, and its handoffs", files)
	}
	return w
}

func (w *world) id(prefix string) string {
	w.next++
	return fmt.Sprintf("%s%04d", prefix, w.next)
}

func (w *world) commit(parent, message string, files map[string]string) string {
	sha := w.id("c")
	w.commits[sha] = commit{parent: parent, message: message, files: files}
	return sha
}

func (w *world) tree(files map[string]string, dir string) string {
	sha := w.id("t")
	w.trees[sha] = treeRef{files: files, dir: dir}
	return sha
}

// serve starts the stand-in and answers the import's options against it.
func (w *world) serve() Options {
	server := httptest.NewServer(http.HandlerFunc(w.handle))
	w.t.Cleanup(server.Close)
	return Options{
		Project: "LEG", DaemonURL: server.URL, GitHubAPI: server.URL,
		Tokens: fakeTokens{t: w.t}, Dispatch: dispatch.New(server.URL, "dispatch-token"),
	}
}

func (w *world) handle(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case r.URL.Path == "/legion/v1/state" && r.Method == http.MethodGet:
		w.state(rw)
	case strings.HasPrefix(r.URL.Path, "/api/v1/"):
		w.dispatch(rw, r)
	case strings.HasPrefix(r.URL.Path, repoPrefix+"/"):
		if got := r.Header.Get("Authorization"); got != "Bearer "+implementToken {
			w.t.Errorf("GitHub %s %s with Authorization %q, want the implement App's token", r.Method, r.URL.Path, got)
		}
		w.github(rw, r, strings.TrimPrefix(r.URL.Path, repoPrefix))
	default:
		w.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(rw, r)
	}
}

// state answers the daemon's state as a contract-39 daemon serves it, with members the import does
// not read.
func (w *world) state(rw http.ResponseWriter) {
	issues := map[string]any{}
	for key, issue := range w.states {
		issues[key] = map[string]any{
			"key": key, "generation": 1, "phase": issue.phase, "status": "in_progress", "repo": repository,
			"defaultBranch": "main", "workers": map[string]any{"implementer": map[string]any{"session": "ses_x", "handoffCommit": "abc"}},
		}
	}
	issues["LEG-99"] = map[string]any{"key": "LEG-99", "phase": "planning", "workers": map[string]any{}}
	writeJSON(rw, http.StatusOK, map[string]any{
		"daemon":    map[string]any{"project": "LEG", "schemaVersion": 55, "boots": 3},
		"admission": map[string]any{"cap": 4, "active": []string{}, "waiting": []string{}},
		"issues":    issues,
	})
}

func (w *world) dispatch(rw http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer dispatch-token" {
		w.t.Errorf("Dispatch %s %s without the daemon's bearer", r.Method, r.URL.Path)
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/issues/")
	parts := strings.Split(rest, "/")
	if w.noRoutes || !ok || len(parts) < 3 || parts[1] != "handoffs" || (w.noWriteRoute && r.Method == http.MethodPost) {
		writeJSON(rw, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "error": "no route for " + r.Method + " " + r.URL.Path})
		return
	}
	key, word := parts[0], parts[2]
	records := w.handoffs[key][word]
	switch {
	case r.Method == http.MethodGet && len(parts) == 4 && parts[3] == "latest":
		if len(records) == 0 {
			writeJSON(rw, http.StatusNotFound, map[string]string{"code": "HANDOFF_NOT_FOUND", "error": "issue " + key + " has no " + word + " handoff"})
			return
		}
		latest := records[len(records)-1]
		writeJSON(rw, http.StatusOK, map[string]any{"id": latest.ID, "issue_key": key, "phase": word, "version": latest.Version, "author": latest.Author, "body": latest.Body})
	case r.Method == http.MethodPost && len(parts) == 3:
		var input struct {
			Actor struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"actor"`
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Actor.Kind != "session" || input.Actor.ID == "" {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"code": "ACTOR_KIND", "error": "a bearer write names its session"})
			return
		}
		record := handoff{ID: w.id("h"), Version: len(records) + 1, Body: input.Body}
		record.Author.Kind, record.Author.ID = input.Actor.Kind, input.Actor.ID
		w.addHandoff(key, word, record)
		w.dispatchWrites++
		writeJSON(rw, http.StatusCreated, map[string]any{"id": record.ID, "issue_key": key, "phase": word, "version": record.Version, "author": record.Author})
	default:
		writeJSON(rw, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "error": "no route for " + r.Method + " " + r.URL.Path})
	}
}

func (w *world) addHandoff(key, word string, record handoff) {
	if w.handoffs[key] == nil {
		w.handoffs[key] = map[string][]handoff{}
	}
	w.handoffs[key][word] = append(w.handoffs[key][word], record)
}

// seedHandoff writes a record to the Dispatch stand-in as session wrote it.
func (w *world) seedHandoff(key, word, session, body string) {
	record := handoff{ID: w.id("h"), Version: len(w.handoffs[key][word]) + 1, Body: body}
	record.Author.Kind, record.Author.ID = "session", session
	w.addHandoff(key, word, record)
}

func (w *world) github(rw http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodGet {
		w.writes = append(w.writes, r.Method+" "+path)
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/git/ref/heads/"):
		head, ok := w.branches[strings.TrimPrefix(path, "/git/ref/heads/")]
		if !ok {
			writeJSON(rw, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(rw, http.StatusOK, map[string]any{"ref": "refs/heads/" + strings.TrimPrefix(path, "/git/ref/heads/"), "object": map[string]string{"sha": head, "type": "commit"}})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/contents/"):
		w.contents(rw, strings.TrimPrefix(path, "/contents/"), r.URL.Query().Get("ref"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/git/commits/"):
		sha := strings.TrimPrefix(path, "/git/commits/")
		c, ok := w.commits[sha]
		if !ok {
			writeJSON(rw, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(rw, http.StatusOK, map[string]any{"sha": sha, "message": c.message, "tree": map[string]string{"sha": w.tree(c.files, "")}})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/git/trees/"):
		w.readTree(rw, strings.TrimPrefix(path, "/git/trees/"), r.URL.Query().Get("recursive") == "1")
	case r.Method == http.MethodPost && path == "/git/trees":
		w.writeTree(rw, r)
	case r.Method == http.MethodPost && path == "/git/commits":
		var input struct {
			Message string   `json:"message"`
			Tree    string   `json:"tree"`
			Parents []string `json:"parents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Parents) != 1 {
			w.t.Errorf("commit create %s: %v, want one parent", path, err)
			writeJSON(rw, http.StatusUnprocessableEntity, map[string]string{"message": "bad commit"})
			return
		}
		tree, ok := w.trees[input.Tree]
		if !ok || tree.dir != "" {
			writeJSON(rw, http.StatusUnprocessableEntity, map[string]string{"message": "Tree SHA does not exist"})
			return
		}
		writeJSON(rw, http.StatusCreated, map[string]string{"sha": w.commit(input.Parents[0], input.Message, tree.files)})
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/git/refs/heads/"):
		w.updateRef(rw, r, strings.TrimPrefix(path, "/git/refs/heads/"))
	default:
		w.t.Errorf("unexpected GitHub request %s %s", r.Method, path)
		writeJSON(rw, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

// contents answers GitHub's contents API: a file's base64 content, broken into lines as GitHub
// breaks it, or a directory's immediate entries, or 404.
func (w *world) contents(rw http.ResponseWriter, path, ref string) {
	c, ok := w.commits[ref]
	if !ok {
		writeJSON(rw, http.StatusNotFound, map[string]string{"message": "No commit found for the ref " + ref})
		return
	}
	if text, ok := c.files[path]; ok {
		encoded := base64.StdEncoding.EncodeToString([]byte(text))
		var lines []string
		for len(encoded) > 60 {
			lines, encoded = append(lines, encoded[:60]), encoded[60:]
		}
		lines = append(lines, encoded)
		writeJSON(rw, http.StatusOK, map[string]any{"type": "file", "path": path, "encoding": "base64", "content": strings.Join(lines, "\n") + "\n"})
		return
	}
	entries := map[string]string{}
	for file := range c.files {
		rest, ok := strings.CutPrefix(file, path+"/")
		if !ok {
			continue
		}
		name, _, nested := strings.Cut(rest, "/")
		if nested {
			entries[name] = "dir"
		} else {
			entries[name] = "file"
		}
	}
	if len(entries) == 0 {
		writeJSON(rw, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	var list []map[string]string
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		list = append(list, map[string]string{"name": name, "path": path + "/" + name, "type": entries[name], "sha": "s-" + name})
	}
	writeJSON(rw, http.StatusOK, list)
}

// readTree answers a tree, its entries relative to its directory; a recursive read lists every file
// and every directory below it, as GitHub's does.
func (w *world) readTree(rw http.ResponseWriter, sha string, recursive bool) {
	tree, ok := w.trees[sha]
	if !ok {
		writeJSON(rw, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	prefix := ""
	if tree.dir != "" {
		prefix = tree.dir + "/"
	}
	entries := map[string]map[string]any{}
	for file := range tree.files {
		rest, ok := strings.CutPrefix(file, prefix)
		if !ok {
			continue
		}
		parts := strings.Split(rest, "/")
		if !recursive {
			if len(parts) == 1 {
				entries[rest] = map[string]any{"path": rest, "mode": "100644", "type": "blob", "sha": "b-" + file}
			} else {
				entries[parts[0]] = map[string]any{"path": parts[0], "mode": "040000", "type": "tree", "sha": w.tree(tree.files, prefix+parts[0])}
			}
			continue
		}
		for i := 1; i < len(parts); i++ {
			dir := strings.Join(parts[:i], "/")
			entries[dir] = map[string]any{"path": dir, "mode": "040000", "type": "tree", "sha": "d-" + prefix + dir}
		}
		entries[rest] = map[string]any{"path": rest, "mode": "100644", "type": "blob", "sha": "b-" + file}
	}
	var list []map[string]any
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		list = append(list, entries[name])
	}
	writeJSON(rw, http.StatusOK, map[string]any{"sha": sha, "tree": list, "truncated": false})
}

// writeTree answers a tree written on a base tree, each entry deleting its path with a null sha.
func (w *world) writeTree(rw http.ResponseWriter, r *http.Request) {
	var input struct {
		BaseTree string                       `json:"base_tree"`
		Tree     []map[string]json.RawMessage `json:"tree"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		w.t.Errorf("tree create: %v", err)
	}
	base, ok := w.trees[input.BaseTree]
	if !ok || base.dir != "" {
		writeJSON(rw, http.StatusUnprocessableEntity, map[string]string{"message": "base_tree is not a root tree"})
		return
	}
	files := maps.Clone(base.files)
	for _, entry := range input.Tree {
		var path, mode, kind string
		_ = json.Unmarshal(entry["path"], &path)
		_ = json.Unmarshal(entry["mode"], &mode)
		_ = json.Unmarshal(entry["type"], &kind)
		sha, present := entry["sha"]
		if !present || string(sha) != "null" || mode != "100644" || kind != "blob" {
			w.t.Errorf("tree entry %s: sha %s mode %q type %q, want a deletion (sha null) of a blob", path, sha, mode, kind)
		}
		if _, ok := files[path]; !ok {
			w.t.Errorf("tree entry deletes %s, which the base tree does not hold", path)
		}
		delete(files, path)
	}
	writeJSON(rw, http.StatusCreated, map[string]string{"sha": w.tree(files, "")})
}

// updateRef answers a branch update: a move another push makes first (moves), answered as GitHub
// answers an update that is no longer a fast-forward, else a fast-forward, else that same 422.
func (w *world) updateRef(rw http.ResponseWriter, r *http.Request, branch string) {
	var input struct {
		SHA   string `json:"sha"`
		Force *bool  `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		w.t.Errorf("ref update: %v", err)
	}
	w.updates = append(w.updates, refUpdate{branch: branch, sha: input.SHA, force: input.Force})
	head := w.branches[branch]
	if w.moves[branch] > 0 {
		w.moves[branch]--
		files := maps.Clone(w.commits[head].files)
		key := strings.TrimPrefix(branch, "legion/")
		files[".legion/"+key+"/implement.json"] = handoffText(key, "implement") + " "
		files[fmt.Sprintf("src/push-%d.go", w.next)] = "package main\n"
		w.branches[branch] = w.commit(head, key+": an old-release worker's push", files)
		writeJSON(rw, http.StatusUnprocessableEntity, map[string]string{"message": "Update is not a fast forward"})
		return
	}
	if input.Force != nil && *input.Force {
		w.branches[branch] = input.SHA
	} else if w.commits[input.SHA].parent != head {
		writeJSON(rw, http.StatusUnprocessableEntity, map[string]string{"message": "Update is not a fast forward"})
		return
	}
	w.branches[branch] = input.SHA
	writeJSON(rw, http.StatusOK, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": input.SHA}})
}

func writeJSON(rw http.ResponseWriter, status int, body any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(body)
}

// fakeTokens mints the implement App's token for the repository's owner and nothing else.
type fakeTokens struct{ t *testing.T }

func (f fakeTokens) Token(_ context.Context, role appauth.AppRole, tokenOwner string) (appauth.Lease, error) {
	if role != appauth.Implement || tokenOwner != owner {
		f.t.Errorf("token for %s on %s, want the implement App on %s", role, tokenOwner, owner)
	}
	return appauth.Lease{Token: implementToken}, nil
}

// run runs the import and answers its exit, stdout and stderr.
func run(t *testing.T, o Options) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), o, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func wantLines(t *testing.T, output string, lines ...string) {
	t.Helper()
	have := strings.Split(strings.TrimRight(output, "\n"), "\n")
	for _, line := range lines {
		if !slices.Contains(have, line) {
			t.Errorf("output has no line %q:\n%s", line, output)
		}
	}
}

// removalOf is the removal commit a branch's head is, checked: the import's message with no trailer
// and no bracket keyword, its parent the head before it, no path under .legion/, and the code
// files as they were.
func (w *world) removalOf(t *testing.T, key, before string) string {
	t.Helper()
	head := w.branches["legion/"+key]
	c := w.commits[head]
	if c.parent != before {
		t.Fatalf("%s: head %s has parent %s, want the removal commit on %s", key, head, c.parent, before)
	}
	if want := "chore(legion): " + key + "'s handoffs are on its Dispatch issue now"; c.message != want {
		t.Errorf("%s: removal message %q, want %q", key, c.message, want)
	}
	// One line holds no trailer (a check-skipping one included), and no bracket keyword.
	if strings.ContainsAny(c.message, "[]\n") {
		t.Errorf("%s: removal message %q carries a trailer or a bracket keyword", key, c.message)
	}
	for path := range c.files {
		if strings.HasPrefix(path, ".legion/") {
			t.Errorf("%s: removal commit %s still holds %s", key, head, path)
		}
	}
	for path, text := range w.commits[before].files {
		if !strings.HasPrefix(path, ".legion/") && c.files[path] != text {
			t.Errorf("%s: removal commit changed %s", key, path)
		}
	}
	return head
}

// Each file of every in-flight branch becomes version 1 of its phase's record under the daemon's
// session, exactly as committed; each branch gets exactly one removal commit, moved there without
// force, and an issue with no branch says so. Another issue's merged .legion/OTHER-1/ is removed
// with the rest and imported nowhere; a done issue's branch is not read.
func TestImportWritesEachFileAsVersionOneAndRemovesLegionInOneCommit(t *testing.T) {
	w := newWorld(t)
	o := w.serve()
	heads := maps.Clone(w.branches)
	for key, issue := range seed {
		files := w.commits[heads["legion/"+key]].files
		if issue.files != nil && !slices.ContainsFunc(slices.Collect(maps.Keys(files)), func(path string) bool { return strings.HasPrefix(path, ".legion/") }) {
			t.Fatalf("control: %s's branch carries no .legion/ before the run", key)
		}
	}

	code, stdout, stderr := run(t, o)

	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q; want 0 and nothing:\n%s", code, stderr, stdout)
	}
	updated := map[string]int{}
	for _, update := range w.updates {
		updated[update.branch]++
		if update.force == nil || *update.force {
			t.Errorf("%s was moved with force %v, want force false", update.branch, update.force)
		}
	}
	for _, key := range []string{"LEG-1", "LEG-2", "LEG-3", "LEG-4", "LEG-5"} {
		for _, path := range seed[key].files {
			parts := strings.Split(path, "/")
			if parts[1] != key {
				continue
			}
			word := strings.TrimSuffix(parts[2], ".json")
			records := w.handoffs[key][word]
			if len(records) != 1 || records[0].Version != 1 || records[0].Author.Kind != "session" || records[0].Author.ID != daemonSession || records[0].Body != handoffText(key, word) {
				t.Errorf("%s %s records = %+v, want version 1 under %s with the file's text", key, word, records, daemonSession)
			}
			wantLines(t, stdout, key+" "+word+": imported v1")
		}
		if updated["legion/"+key] != 1 {
			t.Errorf("%s was updated %d times, want once", key, updated["legion/"+key])
		}
		removal := w.removalOf(t, key, heads["legion/"+key])
		wantLines(t, stdout, key+": removed .legion/ at "+removal)
	}
	if len(w.handoffs["OTHER-1"]) != 0 || len(w.handoffs["LEG-7"]) != 0 || len(w.handoffs["LEG-6"]) != 0 {
		t.Errorf("records of issues the import must not write: %+v", w.handoffs)
	}
	if w.branches["legion/LEG-7"] != heads["legion/LEG-7"] {
		t.Error("the done issue's branch moved")
	}
	wantLines(t, stdout, "LEG-6: no branch", "imported 14, unchanged 0, kept worker record 0, removed .legion/ 5, no branch 1, failed 0")
	if strings.Contains(stdout, "LEG-7") || strings.Contains(stdout, "LEG-99") {
		t.Errorf("output names an issue outside the workflow's branches:\n%s", stdout)
	}
}

// A second run over branches the first emptied writes nothing to Dispatch and pushes nothing.
func TestASecondRunWritesAndPushesNothing(t *testing.T) {
	w := newWorld(t)
	o := w.serve()
	if code, stdout, stderr := run(t, o); code != 0 {
		t.Fatalf("first run exit %d, stderr %q:\n%s", code, stderr, stdout)
	}
	writes, githubWrites := w.dispatchWrites, len(w.writes)
	heads := maps.Clone(w.branches)

	code, stdout, stderr := run(t, o)

	if code != 0 || stderr != "" {
		t.Fatalf("second run exit %d, stderr %q:\n%s", code, stderr, stdout)
	}
	if w.dispatchWrites != writes || len(w.writes) != githubWrites || !maps.Equal(w.branches, heads) {
		t.Fatalf("second run wrote %d records and sent %v to GitHub; want nothing", w.dispatchWrites-writes, w.writes[githubWrites:])
	}
	wantLines(t, stdout, "LEG-1: unchanged", "LEG-5: unchanged", "LEG-6: no branch",
		"imported 0, unchanged 5, kept worker record 0, removed .legion/ 0, no branch 1, failed 0")
}

// The write rule: a phase whose newest record a worker wrote keeps it, and the file is not written
// above it; a newest record the import wrote is written again only when the file differs from it.
// The branch's .legion/ goes either way.
func TestAWorkerRecordIsKeptAndAnImportRecordIsWrittenOnlyWhenItDiffers(t *testing.T) {
	w := newWorld(t)
	o := w.serve()
	w.seedHandoff("LEG-3", "plan", daemonSession, handoffText("LEG-3", "plan"))
	w.seedHandoff("LEG-3", "plan", "ses_planner_3", `{"schemaVersion":1,"phase":"plan","rewritten":true}`)
	w.seedHandoff("LEG-2", "plan", daemonSession, handoffText("LEG-2", "plan"))
	w.seedHandoff("LEG-2", "implement", daemonSession, `{"schemaVersion":1,"stale":true}`)
	before := maps.Clone(w.branches)

	code, stdout, stderr := run(t, o)

	if code != 0 {
		t.Fatalf("exit %d, stderr %q:\n%s", code, stderr, stdout)
	}
	wantLines(t, stdout, "LEG-3 plan: kept worker record v2", "LEG-2 plan: unchanged", "LEG-2 implement: imported v2", "LEG-3 implement: imported v1")
	if records := w.handoffs["LEG-3"]["plan"]; len(records) != 2 || records[1].Author.ID != "ses_planner_3" {
		t.Errorf("LEG-3 plan records = %+v, want the worker's v2 newest and nothing above it", records)
	}
	if records := w.handoffs["LEG-2"]["plan"]; len(records) != 1 {
		t.Errorf("LEG-2 plan records = %+v, want the import's unchanged v1 alone", records)
	}
	if records := w.handoffs["LEG-2"]["implement"]; len(records) != 2 || records[1].Body != handoffText("LEG-2", "implement") || records[1].Author.ID != daemonSession {
		t.Errorf("LEG-2 implement records = %+v, want v2 holding the file under %s", records, daemonSession)
	}
	w.removalOf(t, "LEG-3", before["legion/LEG-3"])
	w.removalOf(t, "LEG-2", before["legion/LEG-2"])
	wantLines(t, stdout, "imported 12, unchanged 1, kept worker record 1, removed .legion/ 5, no branch 1, failed 0")
}

// A branch that moves between its read and the ref update is read again and its issue started
// over: the file the moving push added is imported too, and the branch ends on one removal commit
// above that push, every update without force.
func TestABranchThatMovedIsReadAgainAndRetried(t *testing.T) {
	w := newWorld(t)
	o := w.serve()
	w.moves["legion/LEG-1"] = 1

	code, stdout, stderr := run(t, o)

	if code != 0 {
		t.Fatalf("exit %d, stderr %q:\n%s", code, stderr, stdout)
	}
	var updates []refUpdate
	for _, update := range w.updates {
		if update.branch == "legion/LEG-1" {
			updates = append(updates, update)
		}
		if update.force == nil || *update.force {
			t.Errorf("%s was moved with force %v, want force false", update.branch, update.force)
		}
	}
	if len(updates) != 2 {
		t.Fatalf("LEG-1 updates = %+v, want the refused one and the retry", updates)
	}
	head := w.branches["legion/LEG-1"]
	pushed := w.commits[head].parent
	if !strings.HasSuffix(w.commits[pushed].message, "an old-release worker's push") {
		t.Fatalf("LEG-1's removal sits on %q, want the push that moved the branch", w.commits[pushed].message)
	}
	removal := w.removalOf(t, "LEG-1", pushed)
	if records := w.handoffs["LEG-1"]["implement"]; len(records) != 1 || records[0].Body != handoffText("LEG-1", "implement")+" " {
		t.Errorf("LEG-1 implement records = %+v, want the moving push's file as v1", records)
	}
	wantLines(t, stdout, "LEG-1 plan: imported v1", "LEG-1 implement: imported v1", "LEG-1: removed .legion/ at "+removal)
}

// A branch that moves under every one of the three attempts is reported failed and the run exits
// 1, the branch left where the other pushes put it; every other branch still goes through.
func TestThreeMovesFailTheIssueAndExitOne(t *testing.T) {
	w := newWorld(t)
	o := w.serve()
	w.moves["legion/LEG-1"] = 3

	code, stdout, stderr := run(t, o)

	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s%s", code, stdout, stderr)
	}
	count := 0
	for _, update := range w.updates {
		if update.branch == "legion/LEG-1" {
			count++
		}
	}
	if count != 3 {
		t.Errorf("LEG-1 was updated %d times, want 3", count)
	}
	if head := w.commits[w.branches["legion/LEG-1"]]; !strings.HasSuffix(head.message, "an old-release worker's push") {
		t.Errorf("LEG-1's head is %q, want the last moving push", head.message)
	}
	wantLines(t, stdout, "LEG-1: failed: legion/LEG-1 moved under the removal commit 3 times; run the import again",
		"imported 15, unchanged 0, kept worker record 0, removed .legion/ 4, no branch 1, failed 1")
}

// --dry-run reads everything and says what it would do, writing nothing to Dispatch or GitHub.
func TestADryRunWritesNothing(t *testing.T) {
	w := newWorld(t)
	o := w.serve()
	o.DryRun = true
	heads := maps.Clone(w.branches)

	code, stdout, stderr := run(t, o)

	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q:\n%s", code, stderr, stdout)
	}
	if w.dispatchWrites != 0 || len(w.writes) != 0 || !maps.Equal(w.branches, heads) {
		t.Fatalf("a dry run wrote %d records and sent %v to GitHub", w.dispatchWrites, w.writes)
	}
	wantLines(t, stdout, "LEG-1 plan: would import v1", "LEG-1: would remove .legion/ (1 path) above "+heads["legion/LEG-1"],
		"LEG-5: would remove .legion/ (7 paths) above "+heads["legion/LEG-5"],
		"dry run, nothing written: would import 14, unchanged 0, kept worker record 0, would remove .legion/ 5, no branch 1, failed 0")
}

// A Dispatch without the handoff routes stops the run at its first handoff call, naming the route,
// before anything is written or pushed.
func TestADispatchWithoutTheHandoffRoutesStopsTheRunNamingTheRoute(t *testing.T) {
	w := newWorld(t)
	o := w.serve()
	w.noRoutes = true

	code, stdout, stderr := run(t, o)

	if code != 1 || !strings.Contains(stderr, "GET /api/v1/issues/LEG-1/handoffs/plan/latest") || !strings.Contains(stderr, "404") {
		t.Fatalf("exit %d, stderr %q; want 1 naming the route Dispatch answered 404:\n%s", code, stderr, stdout)
	}
	if w.dispatchWrites != 0 || len(w.writes) != 0 || strings.Contains(stdout, "LEG-2") {
		t.Fatalf("the run went on: %d records, GitHub writes %v:\n%s", w.dispatchWrites, w.writes, stdout)
	}
}

// A Dispatch that answers the newest-record read but not the write stops at the first write,
// naming the write's route, with nothing pushed.
func TestADispatchWithoutTheWriteRouteStopsAtTheFirstWrite(t *testing.T) {
	w := newWorld(t)
	o := w.serve()
	w.noWriteRoute = true

	code, stdout, stderr := run(t, o)

	if code != 1 || !strings.Contains(stderr, "POST /api/v1/issues/LEG-1/handoffs/plan") {
		t.Fatalf("exit %d, stderr %q; want 1 naming the write's route:\n%s", code, stderr, stdout)
	}
	if len(w.writes) != 0 {
		t.Fatalf("the run pushed %v", w.writes)
	}
}

// A daemon serving another project than the configuration names is refused before anything is read
// of GitHub.
func TestADaemonOfAnotherProjectIsRefused(t *testing.T) {
	w := newWorld(t)
	o := w.serve()
	o.Project = "INFRA"

	code, _, stderr := run(t, o)

	if code != 1 || !strings.Contains(stderr, `serves project "LEG", and the configuration names "INFRA"`) {
		t.Fatalf("exit %d, stderr %q; want 1 naming both projects", code, stderr)
	}
}
