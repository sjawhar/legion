package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// progressOf reads an issue's progress object off GET /api/v1/issues/{key}.
func progressOf(t *testing.T, handler http.Handler, key string) model.IssueProgress {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read issue %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Progress model.IssueProgress `json:"progress"`
	}](t, response).Progress
}

func wantCount(t *testing.T, what string, got *model.ProgressCount, done, total int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = null, want %d/%d", what, done, total)
	}
	if got.Done != done || got.Total != total {
		t.Fatalf("%s = %d/%d, want %d/%d", what, got.Done, got.Total, done, total)
	}
}

func wantNull(t *testing.T, what string, got *model.ProgressCount) {
	t.Helper()
	if got != nil {
		t.Fatalf("%s = %d/%d, want null", what, got.Done, got.Total)
	}
}

// The issue read carries the task counts of the spec seeded at creation, nested items included,
// and null for a spec with no task list; a spec with no task item at all counts the same as one
// never counted.
func TestIssueProgressCountsSeededSpecTasks(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	withTasks := createInteractionIssue(t, handler, "TEST", "With tasks",
		"Plan\n\n- [x] one\n- [ ] two\n  - [x] nested\n- plain item\n")
	progress := progressOf(t, handler, withTasks.Key)
	wantCount(t, "tasks", progress.Tasks, 2, 3)
	wantNull(t, "children", progress.Children)

	noTasks := createInteractionIssue(t, handler, "PLAIN", "No tasks", "Just prose.\n\n- a list\n- without boxes\n")
	progress = progressOf(t, handler, noTasks.Key)
	wantNull(t, "tasks", progress.Tasks)
	wantNull(t, "children", progress.Children)
}

// A document edit that ticks an item through `replace` with a leading checkbox sets the item's
// box instead of writing `[x]` as text, and the settlement that versions the edit recounts the
// issue; a `[x] ` written over a plain list item or a paragraph stays literal text.
func TestIssueProgressFollowsReplaceThatTicksAnItem(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ticking", "- [ ] write it\n- [ ] test it\n\nA paragraph.\n\n- plain\n")
	wantCount(t, "tasks before", progressOf(t, handler, issue.Key).Tasks, 0, 2)

	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{
			{"op": "replace", "find": "write it", "with": "[x] write it"},
			{"op": "replace", "find": "A paragraph.", "with": "[x] A paragraph."},
			{"op": "replace", "find": "plain", "with": "[x] plain"},
		},
		"actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("tick item: status=%d body=%s", edited.Code, edited.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if text.Code != http.StatusOK {
		t.Fatalf("read text: status=%d body=%s", text.Code, text.Body.String())
	}
	const want = "- [x] write it\n- [ ] test it\n\n[x] A paragraph.\n\n- \\[x] plain\n"
	if got := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, text).Markdown; got != want {
		t.Fatalf("document after the tick = %q, want %q", got, want)
	}
	wantCount(t, "tasks after the tick", progressOf(t, handler, issue.Key).Tasks, 1, 2)

	unticked := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": "write it", "with": "[ ] write it again"}},
		"actor": sessionActor(),
	})
	if unticked.Code != http.StatusOK {
		t.Fatalf("untick item: status=%d body=%s", unticked.Code, unticked.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	wantCount(t, "tasks after the untick", progressOf(t, handler, issue.Key).Tasks, 0, 2)

	// A box with no text behind it would tick the item and empty it, which renders as a plain
	// item and loses the task; it is refused before anything is written.
	for _, with := range []string{"[x] ", "[ ]\t", "[x]   "} {
		refused := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops":   []map[string]string{{"op": "replace", "find": "write it again", "with": with}},
			"actor": sessionActor(),
		})
		if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"INVALID_OP"`) || !strings.Contains(refused.Body.String(), "leaves it no text") {
			t.Fatalf("with %q: status=%d body=%s, want 400 INVALID_OP naming the emptied item", with, refused.Code, refused.Body.String())
		}
	}
	wantCount(t, "tasks after the refusals", progressOf(t, handler, issue.Key).Tasks, 0, 2)
}

// An upload that replaces the spec recounts the issue in the upload's own transaction.
func TestIssueProgressFollowsSpecUpload(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Uploaded", "- [ ] one\n")
	wantCount(t, "tasks before", progressOf(t, handler, issue.Key).Tasks, 0, 1)
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "spec.md", "content": "- [x] one\n- [x] two\n- [ ] three\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated && uploaded.Code != http.StatusOK {
		t.Fatalf("upload spec: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	wantCount(t, "tasks after the upload", progressOf(t, handler, issue.Key).Tasks, 2, 3)

	cleared := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "spec.md", "content": "No more tasks.\n",
	}, "alice")
	if cleared.Code != http.StatusCreated && cleared.Code != http.StatusOK {
		t.Fatalf("upload spec: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	wantNull(t, "tasks after the list is removed", progressOf(t, handler, issue.Key).Tasks)
}

// Children progress counts an issue's direct children, every status, done being status done,
// and follows a child's status at once; a grandchild counts toward its parent alone. The list,
// the page and the pinned list carry the same object.
func TestIssueProgressCountsDirectChildren(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	root := createInteractionIssue(t, handler, "TEST", "Root", "- [ ] root task\n")
	createChild := func(title, parent string) string {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": "TEST", "title": title, "parent": parent,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			Key string `json:"key"`
		}](t, response).Key
	}
	first := createChild("First child", root.Key)
	second := createChild("Second child", root.Key)
	createChild("Grandchild", first)
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+second, map[string]any{"status": "icebox"}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("icebox second child: status=%d body=%s", patched.Code, patched.Body.String())
	}

	progress := progressOf(t, handler, root.Key)
	wantCount(t, "root tasks", progress.Tasks, 0, 1)
	wantCount(t, "root children", progress.Children, 0, 2)
	wantCount(t, "first child's children", progressOf(t, handler, first).Children, 0, 1)
	wantNull(t, "second child's children", progressOf(t, handler, second).Children)

	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+first, map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close first child: status=%d body=%s", closed.Code, closed.Body.String())
	}
	wantCount(t, "root children after a close", progressOf(t, handler, root.Key).Children, 1, 2)

	if pinned := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+root.Key+"/state", map[string]bool{"pinned": true}, "alice"); pinned.Code != http.StatusOK {
		t.Fatalf("pin root: status=%d body=%s", pinned.Code, pinned.Body.String())
	}
	for _, target := range []string{
		"/api/v1/issues?project=TEST",
		"/api/v1/issues?project=TEST&limit=10",
		"/api/v1/issues?pinned=true",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
		var rows []model.IssueSummary
		if target == "/api/v1/issues?project=TEST&limit=10" {
			rows = decodeBody[model.IssueSummaryPage](t, response).Issues
		} else {
			rows = decodeBody[[]model.IssueSummary](t, response)
		}
		found := false
		for _, row := range rows {
			if row.Key != root.Key {
				continue
			}
			found = true
			wantCount(t, target+" root tasks", row.Progress.Tasks, 0, 1)
			wantCount(t, target+" root children", row.Progress.Children, 1, 2)
		}
		if !found {
			t.Fatalf("GET %s: root %s missing from %d rows", target, root.Key, len(rows))
		}
	}
}

// The reconciliation counts the spec of every issue whose stored count is not its latest
// version's: never counted (as the migration leaves every row), or versioned by a server that
// wrote no count (the task a deploy replaces); it leaves current rows alone and finds nothing on
// a second pass.
func TestIssueProgressReconciliationCountsDriftedIssues(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	counted := createInteractionIssue(t, handler, "TEST", "Counted", "- [x] a\n- [ ] b\n")
	uncounted := createInteractionIssue(t, handler, "SECOND", "Uncounted", "- [x] c\n- [x] d\n- [ ] e\n")
	empty := createInteractionIssue(t, handler, "THIRD", "Empty", "prose\n")
	stale := createInteractionIssue(t, handler, "FOURTH", "Stale", "- [ ] one\n")
	ctx := context.Background()
	// The deploy leaves every existing row with the columns null; these two were counted at
	// creation, so the test clears them to stand for rows the migration found.
	if _, err := database.Pool.Exec(ctx, `update issues set tasks_done = null, tasks_total = null, tasks_version = null where key in ($1, $2)`, uncounted.Key, empty.Key); err != nil {
		t.Fatalf("clear counts: %v", err)
	}
	// The old task, which records no count, wrote this issue's version 2 during the deploy: the
	// row keeps version 1's count and names version 1. The row carries the document's update
	// cursor, as every version write does, so the room's settlement finds nothing past it to
	// version; without it the settlement would write version 3 from the live tree first.
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors, doc_update_version)
		values ($1::uuid, 2, '- [x] one' || chr(10) || '- [x] two' || chr(10) || '- [ ] three' || chr(10), '[]',
		        coalesce((select max(version) from doc_updates where artifact_id = $1::uuid), 0))
	`, stale.PrimaryArtifactID); err != nil {
		t.Fatalf("write a version the old task left uncounted: %v", err)
	}
	wantNull(t, "uncounted tasks before the pass", progressOf(t, handler, uncounted.Key).Tasks)
	wantCount(t, "stale tasks before the pass", progressOf(t, handler, stale.Key).Tasks, 0, 1)

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	deps.Docs.(*docs.Service).ReconcileTaskProgress(runCtx)

	wantCount(t, "counted", progressOf(t, handler, counted.Key).Tasks, 1, 2)
	wantCount(t, "uncounted", progressOf(t, handler, uncounted.Key).Tasks, 2, 3)
	wantNull(t, "empty", progressOf(t, handler, empty.Key).Tasks)
	wantCount(t, "stale, recounted from version 2", progressOf(t, handler, stale.Key).Tasks, 2, 3)
	var remaining int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from issues i where i.tasks_version is distinct from (
			select coalesce(max(v.number), 0) from artifacts a join artifact_versions v on v.artifact_id = a.id
			where a.issue_key = i.key and a.is_primary)
	`).Scan(&remaining); err != nil {
		t.Fatalf("count drift: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("drifted issues after the pass = %d, want 0", remaining)
	}
	deps.Docs.(*docs.Service).ReconcileTaskProgress(runCtx)
	wantCount(t, "uncounted after a second pass", progressOf(t, handler, uncounted.Key).Tasks, 2, 3)
}
