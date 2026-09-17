package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// componentTreeFiles is the fixture model: legion contains daemon (which contains workspace)
// and envoy (which contains listener and server); github is external.
func componentTreeFiles() map[string]string {
	return map[string]string{
		"legion.md":    "---\ntitle: Legion\n---\nThe swarm.\n",
		"daemon.md":    "---\ntitle: Daemon\nparent: legion\ndepends_on: [envoy]\npaths: [packages/daemon]\n---\nThe daemon.\n",
		"workspace.md": "---\ntitle: Workspace\nparent: daemon\n---\nIssue workspaces.\n",
		"envoy.md":     "---\ntitle: Envoy\nparent: legion\n---\nRouting.\n",
		"listener.md":  "---\ntitle: Listener\nparent: envoy\n---\nThe listener.\n",
		"server.md":    "---\ntitle: Server\nparent: envoy\n---\nThe server.\n",
		"github.md":    "---\ntitle: GitHub\nexternal: true\n---\nGitHub.\n",
	}
}

// newComponentTreeServer imports the fixture model into project CORE and returns the handler.
func newComponentTreeServer(t *testing.T) (http.Handler, *fakeGitHubApp) {
	t.Helper()
	fake := &fakeGitHubApp{
		installations: map[string]string{"legion/arch": "read"},
		commit:        "sha-one",
		files:         componentTreeFiles(),
	}
	handler, _ := newArchitectureSourceServer(t, fake)
	createTestProject(t, handler, "CORE")
	if saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/architecture-source", map[string]string{
		"repo": "legion/arch", "branch": "main",
	}, "alice"); saved.Code != http.StatusOK {
		t.Fatalf("put source: status=%d body=%s", saved.Code, saved.Body.String())
	}
	syncComponentTree(t, handler)
	return handler, fake
}

func syncComponentTree(t *testing.T, handler http.Handler) {
	t.Helper()
	synced := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CORE/architecture-source/sync", nil, "alice")
	if synced.Code != http.StatusOK {
		t.Fatalf("sync: status=%d body=%s", synced.Code, synced.Body.String())
	}
	if body := decodeBody[map[string]any](t, synced); body["last_error"] != nil {
		t.Fatalf("sync rejected the model: %v", body["last_error"])
	}
}

func createComponentIssue(t *testing.T, handler http.Handler, title string, parent *string, components any) model.Issue {
	t.Helper()
	body := map[string]any{"project": "CORE", "title": title, "force": true}
	if parent != nil {
		body["parent"] = *parent
	}
	if components != nil {
		body["components"] = components
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

func patchComponents(t *testing.T, handler http.Handler, key string, components any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"components": components}, "alice")
}

func readTree(t *testing.T, handler http.Handler) model.ArchitectureTree {
	t.Helper()
	response := agentRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/architecture", nil, "agent-token")
	if response.Code != http.StatusOK {
		t.Fatalf("read tree: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.ArchitectureTree](t, response)
}

func treeComponent(t *testing.T, tree model.ArchitectureTree, id string) model.ArchitectureTreeComponent {
	t.Helper()
	for _, component := range tree.Components {
		if component.ID == id {
			return component
		}
	}
	t.Fatalf("tree has no component %q", id)
	return model.ArchitectureTreeComponent{}
}

func componentCounts(component model.ArchitectureTreeComponent) [4]int {
	return [4]int{component.Done, component.Total, component.OwnDone, component.OwnTotal}
}

func issueAttachments(component model.ArchitectureTreeComponent) map[string]string {
	attachments := map[string]string{}
	for _, issue := range component.Issues {
		how := issue.Attached
		if issue.Via != nil {
			how += " via " + *issue.Via
		}
		attachments[issue.Key] = how
	}
	return attachments
}

// The counting rule: a component counts every distinct issue whose effective set names it or
// a component it contains, parents included, each once, with direct > inherited > contained
// when several ways apply; own_* keeps only the issues naming this component itself. A `none`
// row is inherited like an explicit one, and a closed issue can still be classified.
func TestArchitectureTreeCountsAttachedWork(t *testing.T) {
	handler, fake := newComponentTreeServer(t)

	root := createComponentIssue(t, handler, "Root", nil, map[string]any{"mode": "explicit", "ids": []string{"legion"}})
	child := createComponentIssue(t, handler, "Child inherits", &root.Key, nil)
	grandchild := createComponentIssue(t, handler, "Grandchild on daemon", &child.Key, map[string]any{"mode": "explicit", "ids": []string{"daemon"}})
	none := createComponentIssue(t, handler, "Process work", nil, map[string]any{"mode": "none", "reason": "hiring, not code"})
	noneChild := createComponentIssue(t, handler, "Under process work", &none.Key, nil)
	unattached := createComponentIssue(t, handler, "Nobody chose", nil, nil)
	done := createComponentIssue(t, handler, "Shipped", &root.Key, nil)

	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+done.Key, map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	// Classified after closing, without reopening.
	if attached := patchComponents(t, handler, done.Key, map[string]any{"mode": "explicit", "ids": []string{"workspace", "daemon", "daemon"}}); attached.Code != http.StatusOK {
		t.Fatalf("attach closed issue: status=%d body=%s", attached.Code, attached.Body.String())
	} else if got := decodeBody[model.Issue](t, attached); got.Status != "done" || !reflect.DeepEqual(got.Components.IDs, []string{"daemon", "workspace"}) || got.Components.InheritedFrom != nil {
		t.Fatalf("closed issue after attach = %+v", got.Components)
	}

	// The effective attachment on read: the nearest ancestor's row of either mode.
	inherited := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+child.Key, nil, "alice"))
	if inherited.Components.Mode != "explicit" || !reflect.DeepEqual(inherited.Components.IDs, []string{"legion"}) || inherited.Components.InheritedFrom == nil || *inherited.Components.InheritedFrom != root.Key {
		t.Fatalf("child components = %+v, want legion inherited from %s", inherited.Components, root.Key)
	}
	inheritedNone := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+noneChild.Key, nil, "alice"))
	if inheritedNone.Components.Mode != "none" || inheritedNone.Components.Reason == nil || *inheritedNone.Components.Reason != "hiring, not code" || inheritedNone.Components.InheritedFrom == nil || *inheritedNone.Components.InheritedFrom != none.Key {
		t.Fatalf("none child components = %+v, want none inherited from %s", inheritedNone.Components, none.Key)
	}
	if unattached.Components.Mode != "inherit" || len(unattached.Components.IDs) != 0 || unattached.Components.InheritedFrom != nil {
		t.Fatalf("unattached components = %+v, want inherit with nothing", unattached.Components)
	}
	listed := decodeBody[[]model.IssueSummary](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice"))
	summaries := map[string]model.IssueComponents{}
	for _, summary := range listed {
		summaries[summary.Key] = summary.Components
	}
	if got := summaries[grandchild.Key]; got.Mode != "explicit" || !reflect.DeepEqual(got.IDs, []string{"daemon"}) || got.InheritedFrom != nil {
		t.Fatalf("listed grandchild components = %+v", got)
	}
	if got := summaries[child.Key]; got.InheritedFrom == nil || *got.InheritedFrom != root.Key {
		t.Fatalf("listed child components = %+v", got)
	}

	tree := readTree(t, handler)
	if tree.Source.Repo != "legion/arch" || tree.Source.LastCommit == nil || *tree.Source.LastCommit != "sha-one" {
		t.Fatalf("tree source = %+v", tree.Source)
	}
	wantCounts := map[string][4]int{
		"legion":    {1, 4, 0, 2}, // root direct, child inherited, grandchild + done contained
		"daemon":    {1, 2, 1, 2}, // grandchild direct, done direct (direct beats contained via workspace)
		"workspace": {1, 1, 1, 1},
		"envoy":     {0, 0, 0, 0},
		"listener":  {0, 0, 0, 0},
		"server":    {0, 0, 0, 0},
		"github":    {0, 0, 0, 0},
	}
	if len(tree.Components) != len(wantCounts) {
		t.Fatalf("tree lists %d components, want %d", len(tree.Components), len(wantCounts))
	}
	for id, want := range wantCounts {
		if got := componentCounts(treeComponent(t, tree, id)); got != want {
			t.Errorf("%s counts (done, total, own_done, own_total) = %v, want %v", id, got, want)
		}
	}
	if got := issueAttachments(treeComponent(t, tree, "legion")); !reflect.DeepEqual(got, map[string]string{
		root.Key: "direct", child.Key: "inherited", grandchild.Key: "contained via daemon", done.Key: "contained via daemon",
	}) {
		t.Fatalf("legion attachments = %v", got)
	}
	if got := issueAttachments(treeComponent(t, tree, "daemon")); !reflect.DeepEqual(got, map[string]string{
		grandchild.Key: "direct", done.Key: "direct",
	}) {
		t.Fatalf("daemon attachments = %v", got)
	}
	daemon := treeComponent(t, tree, "daemon")
	if daemon.Parent == nil || *daemon.Parent != "legion" || !reflect.DeepEqual(daemon.DependsOn, []string{"envoy"}) || !reflect.DeepEqual(daemon.Paths, []string{"packages/daemon"}) || daemon.Prose != "The daemon.\n" {
		t.Fatalf("daemon definition = %+v", daemon)
	}
	for _, row := range daemon.Issues {
		if row.Key == done.Key && (row.Status != "done" || row.Parent == nil || *row.Parent != root.Key || row.ExternalLinks == nil) {
			t.Fatalf("done row = %+v, want status done, parent %s, external_links []", row, root.Key)
		}
	}
	if !treeComponent(t, tree, "github").External {
		t.Fatal("github must be external")
	}

	if tree.Totals != (model.ArchitectureTreeTotals{IssuesDone: 1, IssuesTotal: 7, Unassigned: 1, NotArchitectural: 2, ComponentsWithoutWork: 3, RetiredLinks: 0}) {
		t.Fatalf("totals = %+v", tree.Totals)
	}
	if len(tree.Unassigned) != 1 || tree.Unassigned[0].Key != unattached.Key {
		t.Fatalf("unassigned = %+v, want %s", tree.Unassigned, unattached.Key)
	}
	notArchitectural := map[string]*string{}
	for _, row := range tree.NotArchitectural {
		if row.Reason != "hiring, not code" {
			t.Fatalf("not_architectural row %s reason = %q", row.Key, row.Reason)
		}
		notArchitectural[row.Key] = row.InheritedFrom
	}
	if len(notArchitectural) != 2 || notArchitectural[none.Key] != nil || notArchitectural[noneChild.Key] == nil || *notArchitectural[noneChild.Key] != none.Key {
		t.Fatalf("not_architectural = %+v", tree.NotArchitectural)
	}

	// The explicit attachment is an affects edge whose other end is the component node.
	edges := graphEdges(t, handler, url.Values{"from": {"dispatch://" + grandchild.Key}, "kind": {"affects"}})
	if len(edges.Edges) != 1 || edges.Edges[0].Kind != "affects" || edges.Edges[0].Node.Kind != "component" || edges.Edges[0].Node.ID != "CORE/daemon" ||
		edges.Edges[0].Node.Ref != "dispatch://CORE/component/daemon" || edges.Edges[0].Excerpt == nil || edges.Edges[0].Excerpt.Text != "Daemon" {
		t.Fatalf("affects edges = %+v", edges.Edges)
	}
	backlinks := graphEdges(t, handler, url.Values{"to": {"dispatch://CORE/component/legion"}, "kind": {"affects"}})
	if len(backlinks.Edges) != 1 || backlinks.Edges[0].Node.ID != root.Key || backlinks.Node.Project != "CORE" {
		t.Fatalf("legion backlinks = %+v", backlinks)
	}

	// Retire workspace by re-import: the closed issue keeps the link as unknown, appears in
	// retired_links, and its live component still counts it.
	fake.commit = "sha-two"
	delete(fake.files, "workspace.md")
	syncComponentTree(t, handler)
	retired := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+done.Key, nil, "alice"))
	if !reflect.DeepEqual(retired.Components.IDs, []string{"daemon"}) || !reflect.DeepEqual(retired.Components.Unknown, []string{"workspace"}) {
		t.Fatalf("components after retiring workspace = %+v", retired.Components)
	}
	tree = readTree(t, handler)
	if len(tree.RetiredLinks) != 1 || tree.RetiredLinks[0].Key != done.Key || !reflect.DeepEqual(tree.RetiredLinks[0].IDs, []string{"workspace"}) || tree.Totals.RetiredLinks != 1 {
		t.Fatalf("retired_links = %+v totals %+v", tree.RetiredLinks, tree.Totals)
	}
	if got := componentCounts(treeComponent(t, tree, "daemon")); got != [4]int{1, 2, 1, 2} {
		t.Fatalf("daemon counts after retire = %v", got)
	}
	if got := componentCounts(treeComponent(t, tree, "legion")); got != [4]int{1, 4, 0, 2} {
		t.Fatalf("legion counts after retire = %v", got)
	}
	if len(tree.Components) != 6 {
		t.Fatalf("tree lists %d components after retire, want 6", len(tree.Components))
	}
	// An edge at a retired component has no other end and is omitted.
	if edges := graphEdges(t, handler, url.Values{"from": {"dispatch://" + done.Key}, "kind": {"affects"}}); len(edges.Edges) != 1 || edges.Edges[0].Node.ID != "CORE/daemon" {
		t.Fatalf("affects edges after retire = %+v", edges.Edges)
	}

	// Back to inheriting: null deletes the row and the closed issue takes it.
	if reset := patchComponents(t, handler, done.Key, nil); reset.Code != http.StatusOK {
		t.Fatalf("reset components: status=%d body=%s", reset.Code, reset.Body.String())
	} else if got := decodeBody[model.Issue](t, reset).Components; got.Mode != "explicit" || !reflect.DeepEqual(got.IDs, []string{"legion"}) || got.InheritedFrom == nil || *got.InheritedFrom != root.Key {
		t.Fatalf("components after reset = %+v, want legion inherited from %s", got, root.Key)
	}
}

// Every refused shape names what was wrong with COMPONENTS_INPUT; other closed-issue writes
// still answer ISSUE_CLOSED.
func TestIssueComponentsInputValidation(t *testing.T) {
	handler, _ := newComponentTreeServer(t)
	createReferenceAPIProject(t, handler, "OPS")
	issue := createComponentIssue(t, handler, "Validate", nil, nil)

	for name, tc := range map[string]struct {
		components any
		message    string
	}{
		"unknown id":         {map[string]any{"mode": "explicit", "ids": []string{"web"}}, "CORE has no component web"},
		"external component": {map[string]any{"mode": "explicit", "ids": []string{"github"}}, "external components cannot be attached: github"},
		"mixed unknown":      {map[string]any{"mode": "explicit", "ids": []string{"daemon", "nope", "web"}}, "CORE has no component nope, web"},
		"empty ids":          {map[string]any{"mode": "explicit", "ids": []string{}}, "at least one component"},
		"bad slug":           {map[string]any{"mode": "explicit", "ids": []string{"Daemon"}}, "not a component id"},
		"none without why":   {map[string]any{"mode": "none", "reason": "  "}, "components.reason"},
		"unknown mode":       {map[string]any{"mode": "maybe"}, "mode must be inherit, explicit, or none"},
		"not an object":      {"legion", "components must be null or an object"},
	} {
		response := patchComponents(t, handler, issue.Key, tc.components)
		body := response.Body.String()
		if response.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"COMPONENTS_INPUT"`) {
			t.Fatalf("%s: status=%d body=%s", name, response.Code, body)
		}
		if !strings.Contains(body, tc.message) {
			t.Fatalf("%s: body %s does not name %q", name, body, tc.message)
		}
	}
	// A component from another project's model is unknown here even when its id is taken.
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OPS", "title": "Other project", "components": map[string]any{"mode": "explicit", "ids": []string{"daemon"}},
	}, "alice"); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "OPS has no component daemon") {
		t.Fatalf("other project: status=%d body=%s", response.Code, response.Body.String())
	}
	if untouched := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")); untouched.Components.Mode != "inherit" {
		t.Fatalf("refused writes changed the attachment: %+v", untouched.Components)
	}

	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	if refused := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"components": map[string]any{"mode": "none", "reason": "process"}, "title": "Renamed",
	}, "alice"); refused.Code != http.StatusConflict || decodeBody[map[string]string](t, refused)["code"] != "ISSUE_CLOSED" {
		t.Fatalf("components beside a title on a closed issue: status=%d body=%s", refused.Code, refused.Body.String())
	}
	if accepted := patchComponents(t, handler, issue.Key, map[string]any{"mode": "none", "reason": "process"}); accepted.Code != http.StatusOK {
		t.Fatalf("none on a closed issue: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
}

// Without a source the tree is 404 SOURCE_NOT_FOUND, and like every read it needs a caller.
func TestArchitectureTreeWithoutSource(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/CORE/architecture", nil, "alice")
	if response.Code != http.StatusNotFound || decodeBody[map[string]string](t, response)["code"] != "SOURCE_NOT_FOUND" {
		t.Fatalf("tree without source: status=%d body=%s", response.Code, response.Body.String())
	}
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/v1/projects/CORE/architecture", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous tree read: status=%d", anonymous.Code)
	}
}
