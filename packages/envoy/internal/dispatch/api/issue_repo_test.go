package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// lastIssueEventPayload reads the payload of the issue's newest event.
func lastIssueEventPayload(t *testing.T, database *store.Store, key string) map[string]any {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select payload from events where issue_key = $1 order by id desc limit 1
	`, key).Scan(&raw); err != nil {
		t.Fatalf("read %s's newest event: %v", key, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode %s's newest event: %v", key, err)
	}
	return payload
}

// An issue's repository: its own when set, else its nearest ancestor's, else the project's default,
// else none; every list row, the detail and every issue event carry both the own field and the
// resolved one. A repository not mapped to the project is refused by name.
func TestIssueRepoResolvesThroughParentsThenTheProjectDefault(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE", "default": true}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets", map[string]any{"project": "CORE"}, "alice")

	root := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Root"}, "alice"))
	if root.Repo != nil || root.ResolvedRepo == nil || *root.ResolvedRepo != "acme/widgets" {
		t.Fatalf("root repo=%v resolved=%v, want none and the project default acme/widgets", root.Repo, root.ResolvedRepo)
	}
	created := lastIssueEventPayload(t, database, root.Key) // the issue.created the create wrote
	if created["repo"] != nil || created["resolved_repo"] != "acme/widgets" {
		t.Fatalf("issue.created payload repo=%v resolved_repo=%v, want null and acme/widgets", created["repo"], created["resolved_repo"])
	}
	mid := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Mid", "parent": root.Key, "repo": "acme/gadgets", "force": true}, "alice"))
	if mid.Repo == nil || *mid.Repo != "acme/gadgets" {
		t.Fatalf("mid repo=%v, want its own acme/gadgets", mid.Repo)
	}
	leaf := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Leaf", "parent": mid.Key, "force": true}, "alice"))
	if leaf.Repo != nil || leaf.ResolvedRepo == nil || *leaf.ResolvedRepo != "acme/gadgets" {
		t.Fatalf("leaf repo=%v resolved=%v, want none and the parent's acme/gadgets", leaf.Repo, leaf.ResolvedRepo)
	}
	listed := decodeBody[[]model.IssueSummary](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice"))
	resolved := map[string]string{}
	own := map[string]string{}
	for _, issue := range listed {
		if issue.ResolvedRepo != nil {
			resolved[issue.Key] = *issue.ResolvedRepo
		}
		if issue.Repo != nil {
			own[issue.Key] = *issue.Repo
		}
	}
	if resolved[root.Key] != "acme/widgets" || resolved[mid.Key] != "acme/gadgets" || resolved[leaf.Key] != "acme/gadgets" {
		t.Fatalf("listed resolved repos = %v", resolved)
	}
	if len(own) != 1 || own[mid.Key] != "acme/gadgets" {
		t.Fatalf("listed own repos = %v, want only %s's acme/gadgets", own, mid.Key)
	}
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+leaf.Key, map[string]any{"repo": "Acme/Widgets.git"}, "alice")
	if patched.Code != http.StatusOK || decodeBody[model.Issue](t, patched).Repo == nil || *decodeBody[model.Issue](t, patched).Repo != "acme/widgets" {
		t.Fatalf("set the leaf's own repo: status=%d body=%s, want it canonical", patched.Code, patched.Body.String())
	}
	detail := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+leaf.Key, nil, "alice"))
	if detail.Repo == nil || *detail.Repo != "acme/widgets" || detail.ResolvedRepo == nil || *detail.ResolvedRepo != "acme/widgets" {
		t.Fatalf("leaf detail repo=%v resolved=%v, want its own acme/widgets", detail.Repo, detail.ResolvedRepo)
	}
	cleared := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+leaf.Key, map[string]any{"repo": nil}, "alice")
	if cleared.Code != http.StatusOK || decodeBody[model.Issue](t, cleared).Repo != nil {
		t.Fatalf("clear the leaf's own repo: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	payload := lastIssueEventPayload(t, database, leaf.Key) // the issue.updated the clear wrote
	if payload["repo"] != nil || payload["resolved_repo"] != "acme/gadgets" {
		t.Fatalf("issue.updated payload repo=%v resolved_repo=%v, want null and acme/gadgets", payload["repo"], payload["resolved_repo"])
	}
	for _, bad := range []struct{ repo, code string }{
		{"acme/unmapped", "REPO_NOT_MAPPED"},
		{"not-a-repo", "INVALID_ISSUE"},
	} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+leaf.Key, map[string]any{"repo": bad.repo}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), bad.code) {
			t.Fatalf("patch repo %q: status=%d body=%s, want 400 %s", bad.repo, response.Code, response.Body.String(), bad.code)
		}
		response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Bad " + bad.repo, "repo": bad.repo, "force": true}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), bad.code) {
			t.Fatalf("create with repo %q: status=%d body=%s, want 400 %s", bad.repo, response.Code, response.Body.String(), bad.code)
		}
	}
}

// A project with no default and an issue chain naming none resolves to none: null on list, detail and
// events, never an error.
func TestIssueRepoResolvesToNoneWithoutADefault(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE"}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Alone"}, "alice"))
	if issue.Repo != nil || issue.ResolvedRepo != nil {
		t.Fatalf("repo=%v resolved=%v, want both null", issue.Repo, issue.ResolvedRepo)
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")
	if !strings.Contains(listed.Body.String(), `"repo":null`) || !strings.Contains(listed.Body.String(), `"resolved_repo":null`) {
		t.Fatalf("listed issue = %s, want repo and resolved_repo both null", listed.Body.String())
	}
	payload := lastIssueEventPayload(t, database, issue.Key)
	if value, ok := payload["resolved_repo"]; !ok || value != nil {
		t.Fatalf("issue.created payload resolved_repo=%v (present %t), want null", value, ok)
	}
}

// createChain creates issues under parent, each the parent of the next, and returns their keys:
// keys[i] is i+1 links below parent.
func createChain(t *testing.T, handler http.Handler, project, parent string, length int) []string {
	t.Helper()
	keys := make([]string, 0, length)
	for index := range length {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": project, "title": fmt.Sprintf("%s link %d", parent, index+1), "parent": parent, "force": true}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create link %d under %s: status=%d body=%s", index+1, parent, response.Code, response.Body.String())
		}
		parent = decodeBody[model.Issue](t, response).Key
		keys = append(keys, parent)
	}
	return keys
}

// An issue may sit at most parentDepthCap (32) links below its root. At that depth it still
// resolves to the repository its root names, in its detail, every list row and its creation
// event; an issue one link deeper is refused, 409 PARENT_INPUT.
func TestIssueCreateRefusesAParentPastTheDepthCap(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE", "default": true}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets", map[string]any{"project": "CORE"}, "alice")
	root := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Root", "repo": "acme/gadgets"}, "alice"))
	keys := append([]string{root.Key}, createChain(t, handler, "CORE", root.Key, parentDepthCap)...) // keys[d] is d links below the root
	listed := map[string]model.IssueSummary{}
	for _, issue := range decodeBody[[]model.IssueSummary](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")) {
		listed[issue.Key] = issue
	}
	for _, depth := range []int{parentDepthCap - 1, parentDepthCap} {
		key := keys[depth]
		detail := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
		if detail.ResolvedRepo == nil || *detail.ResolvedRepo != "acme/gadgets" {
			t.Errorf("depth %d detail resolved_repo=%v, want the root's acme/gadgets", depth, detail.ResolvedRepo)
		}
		if row := listed[key]; row.ResolvedRepo == nil || *row.ResolvedRepo != "acme/gadgets" {
			t.Errorf("depth %d list row resolved_repo=%v, want acme/gadgets", depth, row.ResolvedRepo)
		}
		if created := lastIssueEventPayload(t, database, key); created["resolved_repo"] != "acme/gadgets" {
			t.Errorf("depth %d issue.created resolved_repo=%v, want acme/gadgets", depth, created["resolved_repo"])
		}
	}
	deeper := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "One link too deep", "parent": keys[parentDepthCap], "force": true}, "alice")
	if deeper.Code != http.StatusConflict || !strings.Contains(deeper.Body.String(), "PARENT_INPUT") || !strings.Contains(deeper.Body.String(), "at most 32 links") {
		t.Fatalf("create %d links below the root: status=%d body=%s, want 409 PARENT_INPUT naming the 32-link limit", parentDepthCap+1, deeper.Code, deeper.Body.String())
	}
}

// A reparent counts the moved issue's subtree: it is refused when the subtree's deepest issue
// would sit more than 32 links below its new root, even though neither chain alone is that deep.
func TestIssuePatchRefusesAReparentPastTheDepthCap(t *testing.T) {
	handler := newTestHandler(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	top := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Top"}, "alice"))
	upper := append([]string{top.Key}, createChain(t, handler, "CORE", top.Key, 20)...) // upper[d] is d links below Top
	moved := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Moved", "force": true}, "alice"))
	createChain(t, handler, "CORE", moved.Key, 12) // Moved's subtree is 12 links deep
	refused := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moved.Key, map[string]any{"parent": upper[20]}, "alice")
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), "PARENT_INPUT") || !strings.Contains(refused.Body.String(), "at most 32 links") {
		t.Fatalf("move a 12-deep subtree 21 links below Top: status=%d body=%s, want 409 PARENT_INPUT naming the 32-link limit", refused.Code, refused.Body.String())
	}
	if accepted := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moved.Key, map[string]any{"parent": upper[19]}, "alice"); accepted.Code != http.StatusOK {
		t.Fatalf("move a 12-deep subtree 20 links below Top, its deepest issue 32 links down: status=%d body=%s, want 200", accepted.Code, accepted.Body.String())
	}
}

// A parent already past the cap is past the walk's bound too, so the refusal says "more than 32
// links" rather than the bound's number. An earlier release's API could build such a chain;
// migration 0113 refuses to apply while one exists, so after it only a direct database write, or an
// earlier release left serving against the rollout rule, makes one. This test writes one directly.
func TestIssueParentRefusalPastTheCapSaysMoreThan32Links(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "LEG", "name": "Legacy"}, "alice")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, parent_key, created_by, rank)
		select 'LEG-' || n, 'LEG', n, 'Legacy ' || n, case when n = 1 then null else 'LEG-' || (n - 1) end,
		       '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0')
		from generate_series(1, 41) n
		order by n;
		update projects set next_number = 42 where key = 'LEG'
	`); err != nil {
		t.Fatalf("insert a 41-issue chain: %v", err)
	}
	loose := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "LEG", "title": "Loose", "force": true}, "alice"))
	for name, response := range map[string]*httptest.ResponseRecorder{
		"create under LEG-41, 40 links down":     dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "LEG", "title": "Under", "parent": "LEG-41", "force": true}, "alice"),
		"move Loose under LEG-36, 35 links down": dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+loose.Key, map[string]any{"parent": "LEG-36"}, "alice"),
	} {
		body := response.Body.String()
		if response.Code != http.StatusConflict || !strings.Contains(body, "more than 32 links below its root") || strings.Contains(body, "33 links") {
			t.Errorf("%s: status=%d body=%s, want 409 saying more than 32 links and no bound's number", name, response.Code, body)
		}
	}
}

// The repository walk stops at the depth cap, so a list costs at most parentDepthCap+1 steps per
// issue however deep a chain is. A chain past the cap fails its issues' reads by name, never giving
// the project's default, and fails them fast. An earlier release's API could build one; migration
// 0113 refuses to apply while one exists, so after it only a direct database write, or an earlier
// release left serving against the rollout rule, makes one. This test writes one directly.
func TestIssueRepoWalkStopsAtTheDepthCap(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "DEEP", "name": "Deep"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "DEEP", "default": true}, "alice")
	const length = 2000 // DEEP-1 is the root; DEEP-n is n-1 links below it
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, created_by, rank)
		select 'DEEP-' || n, 'DEEP', n, 'Deep ' || n, case when n = 1 then null else 'DEEP-' || (n - 1) end,
		       '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0')
		from generate_series(1, $1::int) n
		order by n
	`, length); err != nil {
		t.Fatalf("insert a %d-issue chain: %v", length, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local statement_timeout = '10s'`); err != nil {
		t.Fatalf("set statement_timeout: %v", err)
	}
	started := time.Now()
	rows, err := tx.Query(ctx, `select i.key, `+issueRepoColumns+` from issues i `+issueRepoLateral+` where i.project_key = 'DEEP'`)
	if err != nil {
		t.Fatalf("walk %d issues: %v", length, err)
	}
	resolved, refused := 0, 0
	for rows.Next() {
		var key string
		var repo repoScan
		if err := rows.Scan(append([]any{&key}, repo.targets()...)...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, resolvedRepo, err := repo.resolve(key); err != nil {
			if !strings.Contains(err.Error(), "deeper than 32 links") {
				t.Fatalf("%s: %v, want a refusal naming the 32-link limit", key, err)
			}
			refused++
		} else if resolvedRepo == nil || *resolvedRepo != "acme/widgets" {
			t.Fatalf("%s resolved_repo=%v, want the project's default acme/widgets", key, resolvedRepo)
		} else {
			resolved++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("walk %d issues in a %d-link chain within 10s: %v", length, length-1, err)
	}
	t.Logf("walked %d issues of a %d-link chain in %s", length, length-1, time.Since(started))
	if resolved != parentDepthCap+1 || refused != length-parentDepthCap-1 {
		t.Fatalf("resolved %d and refused %d, want the %d issues within 32 links of the root resolved and the other %d refused", resolved, refused, parentDepthCap+1, length-parentDepthCap-1)
	}
	started = time.Now()
	if listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=DEEP", nil, "alice"); listed.Code != http.StatusInternalServerError {
		t.Fatalf("list a project with a chain past the cap: status=%d, want 500", listed.Code)
	}
	t.Logf("listed DEEP (refused) in %s", time.Since(started))
}

// A parent chain that loops (which no API write can make) has no answer: the read fails loudly
// rather than giving the project's default.
func TestIssueRepoRefusesToResolveALoopedParentChain(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE", "default": true}, "alice")
	top := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Top"}, "alice"))
	below := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Below", "parent": top.Key, "force": true}, "alice"))
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, below.Key, top.Key); err != nil {
		t.Fatalf("loop the chain: %v", err)
	}
	for _, target := range []string{"/api/v1/issues/" + top.Key, "/api/v1/issues?project=CORE"} {
		if response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); response.Code != http.StatusInternalServerError {
			t.Errorf("GET %s with a looped chain: status=%d body=%s, want 500", target, response.Code, response.Body.String())
		}
	}
}

// The claim and its release are issue events too, and carry the repository as the others do.
func TestIssueRepoRidesTheClaimEvents(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE", "default": true}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Claimed"}, "alice"))
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if response := dispatchRequest(t, handler, method, "/api/v1/issues/"+issue.Key+"/claim", map[string]any{}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("%s claim: status=%d body=%s", method, response.Code, response.Body.String())
		}
		payload := lastIssueEventPayload(t, database, issue.Key)
		if value, ok := payload["repo"]; !ok || value != nil || payload["resolved_repo"] != "acme/widgets" {
			t.Fatalf("%s claim event payload = %v, want repo null and resolved_repo acme/widgets", method, payload)
		}
	}
}

// A closed issue takes no repository write, as it takes no other field but its rank and components.
func TestIssueRepoIsRefusedOnAClosedIssue(t *testing.T) {
	handler := newTestHandler(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE"}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Done"}, "alice"))
	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"repo": "acme/widgets"}, "alice")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "ISSUE_CLOSED") {
		t.Fatalf("set repo on a closed issue: status=%d body=%s, want 409 ISSUE_CLOSED", response.Code, response.Body.String())
	}
}
