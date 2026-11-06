// going_catalogue_test.go holds the catalogue to the tool specs it is built from and to the
// route table it is served beside: every row a tool names is a write of that table, and every
// write no tool names is reported for follow-up rather than failing the build
// (TestTheMutatingRoutesNoToolSpecNames).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// goingMutatingRoutesWithNoTool is every write of the served table no dispatch tool performs,
// which is the catalogue's gap and this tree's follow-up work: each of these is an operation the
// bar cannot yet propose. TestTheMutatingRoutesNoToolSpecNames holds the list to the table, so a
// write added later is named here or in goingToolRoutes and never silently missing.
var goingMutatingRoutesWithNoTool = []string{
	"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}",
	"DELETE /api/v1/delivery/session-groups/{id}",
	"DELETE /api/v1/issues/{key}/subscribers/{session_id}",
	"DELETE /api/v1/me/agent-tokens/{id}",
	"DELETE /api/v1/me/asks/{id}/snooze",
	"DELETE /api/v1/projects/{key}/architecture-sources/{owner}/{repo}",
	"DELETE /api/v1/settings/repo-projects/{owner}/{repo}",
	"PATCH /api/v1/comments/{id}",
	"PATCH /api/v1/delivery/session-groups/{id}",
	"PATCH /api/v1/me/agent-tokens/{id}",
	"POST /api/v1/agents/{session_id}/messages",
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep",
	"POST /api/v1/artifacts/{id}/rebuild",
	"POST /api/v1/artifacts/{id}/reviews",
	"POST /api/v1/artifacts/{id}/versions",
	"POST /api/v1/asks/{id}/answer",
	"POST /api/v1/broadcasts",
	"POST /api/v1/comments/{id}/accept",
	"POST /api/v1/comments/{id}/deliveries",
	"POST /api/v1/comments/{id}/reject",
	"POST /api/v1/comments/{id}/reopen",
	"POST /api/v1/comments/{id}/reply",
	"POST /api/v1/credential-grants/{id}/revoke",
	"POST /api/v1/credential-requests/machine-lookup",
	"POST /api/v1/credential-requests/{id}/approve",
	"POST /api/v1/credential-requests/{id}/deny",
	"POST /api/v1/delivery/session-groups",
	"POST /api/v1/going/requests",
	"POST /api/v1/issues/{key}/artifacts/{slug}/versions",
	"POST /api/v1/machine-logins/{id}/revoke",
	"POST /api/v1/me/agent-tokens",
	"POST /api/v1/messages/{id}/deliveries",
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept",
	"POST /api/v1/projects/{key}/architecture-source/sync",
	"POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync",
	"POST /api/v1/projects/{key}/artifacts/{slug}/versions",
	"POST /api/v1/projects/{key}/related",
	"POST /api/v1/signins/{id}/revoke",
	"PUT /api/v1/me/agents/{session_id}/state",
	"PUT /api/v1/me/asks/{id}/snooze",
	"PUT /api/v1/me/documents/{id}/read",
	"PUT /api/v1/me/issues/{key}/state",
	"PUT /api/v1/me/threads/{id}/read",
	"PUT /api/v1/projects/{key}/architecture-sources/{owner}/{repo}",
	"PUT /api/v1/projects/{key}/default-assignee",
	"PUT /api/v1/settings/delivery",
	"PUT /api/v1/settings/repo-projects/{owner}/{repo}",
}

// withThrowawayToolRoutes gives one tool a set of rows for the length of a test, so a build is
// shown failing on a row that is no write of the table without touching the map the server
// mounts.
func withThrowawayToolRoutes(t *testing.T, tool string, rows ...goingToolRoute) {
	t.Helper()
	if _, held := goingToolRoutes[tool]; held {
		t.Fatalf("%q already names rows; name a tool the catalogue does not hold", tool)
	}
	goingToolRoutes[tool] = rows
	t.Cleanup(func() { delete(goingToolRoutes, tool) })
}

// TestTheCatalogueIsTheDispatchToolsThatWrite: the catalogue is the mutating tool specs and
// nothing else — each entry carries that spec's own description and its own argument schema,
// unchanged — and every row an entry names is a write of the table the server mounts.
func TestTheCatalogueIsTheDispatchToolsThatWrite(t *testing.T) {
	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	catalogue, err := buildGoingCatalogue(routes)
	if err != nil {
		t.Fatalf("build the catalogue of the served table: %v", err)
	}
	actions := catalogue.Actions()
	t.Logf("the catalogue holds %d actions, one per mutating tool spec", len(actions))
	if len(actions) != len(goingToolRoutes) {
		t.Fatalf("the catalogue holds %d actions, want the %d tool specs that write", len(actions), len(goingToolRoutes))
	}

	specs := make(map[string]contracts.DispatchTool, len(contracts.DispatchTools))
	for _, spec := range contracts.DispatchTools {
		specs[spec.Name] = spec
	}
	mutating := mutatingRoutes(routes)
	held := map[string]bool{}
	for _, action := range actions {
		if held[action.Action] {
			t.Errorf("the catalogue holds %q twice", action.Action)
		}
		held[action.Action] = true
		spec, isTool := specs[action.Action]
		if !isTool {
			t.Errorf("the catalogue holds %q, which is no dispatch tool", action.Action)
			continue
		}
		if action.Description != spec.Description {
			t.Errorf("%q serves a description the tool spec does not: %q", action.Action, action.Description)
		}
		if !reflect.DeepEqual(action.Request, spec.Arguments) {
			t.Errorf("%q serves arguments the tool spec does not", action.Action)
		}
		if string(action.Example) != string(spec.Example) {
			t.Errorf("%q serves the example %s, want the tool spec's %s", action.Action, action.Example, spec.Example)
		}
		if len(action.Routes) == 0 {
			t.Errorf("%q names no route, so nothing performs it", action.Action)
		}
		for _, route := range action.Routes {
			if !mutating[route.Method+" "+route.Pattern] {
				t.Errorf("%q names %s %s, which is no write of the route table", action.Action, route.Method, route.Pattern)
			}
		}
	}

	t.Run("a tool naming a row the table does not hold fails the build", func(t *testing.T) {
		withThrowawayToolRoutes(t, "dispatch_whoami", goingToolRoute{
			Method: http.MethodPost, Pattern: "/api/v1/widgets/{id}/paint",
			Path: map[string]string{"id": "widget"},
		})
		_, err := buildGoingCatalogue(routes)
		if err == nil || !strings.Contains(err.Error(), "dispatch_whoami: POST /api/v1/widgets/{id}/paint is no row of the route table") {
			t.Fatalf("the build answered %v, want the row named as no row of the table", err)
		}
	})

	t.Run("a tool naming a read row fails the build", func(t *testing.T) {
		withThrowawayToolRoutes(t, "dispatch_whoami", goingToolRoute{
			Method: http.MethodGet, Pattern: "/api/v1/projects", Path: map[string]string{},
		})
		_, err := buildGoingCatalogue(routes)
		if err == nil || !strings.Contains(err.Error(), "dispatch_whoami: GET /api/v1/projects is no write of the route table") {
			t.Fatalf("the build answered %v, want the row named as no write", err)
		}
	})

	t.Run("rows named for a tool that does not exist fail the build", func(t *testing.T) {
		withThrowawayToolRoutes(t, "dispatch_paint_widget", goingToolRoute{
			Method: http.MethodPost, Pattern: "/api/v1/projects", Path: map[string]string{},
		})
		_, err := buildGoingCatalogue(routes)
		if err == nil || !strings.Contains(err.Error(), "dispatch_paint_widget: no dispatch tool of that name") {
			t.Fatalf("the build answered %v, want the tool name refused", err)
		}
	})

	t.Run("a placeholder no argument of the tool fills fails the build", func(t *testing.T) {
		withThrowawayToolRoutes(t, "dispatch_whoami", goingToolRoute{
			Method: http.MethodPost, Pattern: "/api/v1/issues/{key}/claim",
			Path: map[string]string{"key": "issue"},
		})
		_, err := buildGoingCatalogue(routes)
		if err == nil || !strings.Contains(err.Error(), `dispatch_whoami: {key} is bound to "issue", which is no argument of the tool`) {
			t.Fatalf("the build answered %v, want the binding refused", err)
		}
	})
}

// TestTheMutatingRoutesNoToolSpecNames: the catalogue's gap, named rather than refused. A write
// of the served table no dispatch tool performs is an operation the bar cannot yet propose, and
// the list is committed here so a write added later is either given a tool or added to it.
func TestTheMutatingRoutesNoToolSpecNames(t *testing.T) {
	routes := (&server{}).routes()
	performed := map[string]bool{}
	for _, rows := range goingToolRoutes {
		for _, row := range rows {
			performed[row.Method+" "+row.Pattern] = true
		}
	}
	gap := make([]string, 0, len(routes))
	for name := range mutatingRoutes(routes) {
		if !performed[name] {
			gap = append(gap, name)
		}
	}
	sort.Strings(gap)
	t.Logf("%d writes of the route table no dispatch tool performs:\n%s", len(gap), strings.Join(gap, "\n"))
	if !slices.Equal(gap, goingMutatingRoutesWithNoTool) {
		t.Fatalf("the writes no tool performs are:\n%s\n\nupdate goingMutatingRoutesWithNoTool, or give the new write a tool in goingToolRoutes", strings.Join(gap, "\n"))
	}
}

// TestEveryGoingActionAcceptsItsExampleAndRefusesAMissingArgument: each entry's schema accepts
// the call the tool spec carries, and refuses that call with any one required argument left out.
func TestEveryGoingActionAcceptsItsExampleAndRefusesAMissingArgument(t *testing.T) {
	catalogue, err := buildGoingCatalogue((&server{}).routes())
	if err != nil {
		t.Fatalf("build the catalogue: %v", err)
	}
	for _, action := range catalogue.Actions() {
		t.Run(action.Action, func(t *testing.T) {
			var example map[string]any
			if err := json.Unmarshal(action.Example, &example); err != nil {
				t.Fatalf("decode the example call: %v", err)
			}
			if refusal := checkGoingArguments(action.Request, example); refusal != "" {
				t.Fatalf("the schema refused its own example %s: %s", action.Example, refusal)
			}
			for _, field := range action.Request {
				if !field.Required {
					continue
				}
				short := maps.Clone(example)
				delete(short, field.Name)
				refusal := checkGoingArguments(action.Request, short)
				if !strings.Contains(refusal, `argument "`+field.Name+`" is required`) {
					t.Errorf("a call without %q was refused %q, want it named as required", field.Name, refusal)
				}
			}
		})
	}
}

// countingDoer is an HTTP client that records what it was asked to send, so a test can prove
// nothing was sent at all.
type countingDoer struct {
	requests []*http.Request
}

func (d *countingDoer) Do(request *http.Request) (*http.Response, error) {
	d.requests = append(d.requests, request)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: request}, nil
}

// TestAGoingActionTheCatalogueDoesNotHoldIsRefusedWithNoRequestMade: a proposal naming an action
// nobody serves is refused with the caller's own error, and the client is never asked to send
// anything. The action it does hold is the control: that one is sent.
func TestAGoingActionTheCatalogueDoesNotHoldIsRefusedWithNoRequestMade(t *testing.T) {
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatalf("build the catalogue: %v", err)
	}
	doer := &countingDoer{}
	caller := GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Doer: doer}

	_, err = caller.Do(context.Background(), "dispatch_paint_widget", map[string]any{"id": "7"})
	var unknown GoingUnknownActionError
	if !errors.As(err, &unknown) || unknown.Action != "dispatch_paint_widget" {
		t.Fatalf("the caller answered %v, want the unknown action refused by name", err)
	}
	if len(doer.requests) != 0 {
		t.Fatalf("the caller sent %d request(s) for an action the catalogue does not hold", len(doer.requests))
	}

	// A read tool is no action of the catalogue either: the catalogue is the tools that write.
	_, err = caller.Do(context.Background(), "dispatch_search", map[string]any{"query": "astrolabe"})
	if !errors.As(err, &unknown) || unknown.Action != "dispatch_search" {
		t.Fatalf("the caller answered %v, want the read tool refused by name", err)
	}

	// A call the action's own schema refuses is the same: refused before anything is sent.
	_, err = caller.Do(context.Background(), "dispatch_issue", map[string]any{"project": "INFRA"})
	var refused GoingArgumentError
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, `argument "title" is required`) {
		t.Fatalf("the caller answered %v, want the missing title refused", err)
	}
	if len(doer.requests) != 0 {
		t.Fatalf("the caller sent %d request(s) for a call the schema refuses", len(doer.requests))
	}

	response, err := caller.Do(context.Background(), "dispatch_issue", map[string]any{"project": "INFRA", "title": "A new issue"})
	if err != nil {
		t.Fatalf("perform an action the catalogue holds: %v", err)
	}
	if response.StatusCode != http.StatusOK || len(doer.requests) != 1 {
		t.Fatalf("the caller sent %d request(s), want the one action it holds", len(doer.requests))
	}
	sent := doer.requests[0]
	if sent.Method != http.MethodPost || sent.URL.String() != "https://dispatch.invalid/api/v1/issues" {
		t.Fatalf("the caller sent %s %s", sent.Method, sent.URL)
	}

	t.Run("a placeholder is filled from the arguments rather than sent as a body field", func(t *testing.T) {
		doer := &countingDoer{}
		caller := GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Doer: doer}
		if _, err := caller.Do(context.Background(), "dispatch_claim", map[string]any{"issue": "INFRA-1042"}); err != nil {
			t.Fatalf("claim an issue: %v", err)
		}
		sent := doer.requests[0]
		if sent.Method != http.MethodPost || sent.URL.Path != "/api/v1/issues/INFRA-1042/claim" {
			t.Fatalf("the caller sent %s %s", sent.Method, sent.URL.Path)
		}
	})

	t.Run("the argument that tells two rows apart picks the row", func(t *testing.T) {
		doer := &countingDoer{}
		caller := GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Doer: doer}
		if _, err := caller.Do(context.Background(), "dispatch_claim", map[string]any{"issue": "INFRA-1042", "release": true}); err != nil {
			t.Fatalf("release a claim: %v", err)
		}
		sent := doer.requests[0]
		if sent.Method != http.MethodDelete || sent.URL.Path != "/api/v1/issues/INFRA-1042/claim" {
			t.Fatalf("the caller sent %s %s", sent.Method, sent.URL.Path)
		}
	})

	t.Run("a row addressing the calling session takes it from the caller", func(t *testing.T) {
		doer := &countingDoer{}
		caller := GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Session: "sess-7", Doer: doer}
		arguments := map[string]any{"ask": "01234567-0000-4000-8000-000000000001", "action": "follow"}
		if _, err := caller.Do(context.Background(), "dispatch_follow", arguments); err != nil {
			t.Fatalf("follow an ask: %v", err)
		}
		sent := doer.requests[0]
		if sent.Method != http.MethodPut || sent.URL.Path != "/api/v1/asks/01234567-0000-4000-8000-000000000001/followers/sess-7" {
			t.Fatalf("the caller sent %s %s", sent.Method, sent.URL.Path)
		}

		// A caller that is no session fills no such row, and the refusal says what each row
		// wanted rather than sending a request to a path with a hole in it.
		doer = &countingDoer{}
		caller = GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Doer: doer}
		_, err := caller.Do(context.Background(), "dispatch_follow", arguments)
		var refused GoingArgumentError
		if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "the calling session") {
			t.Fatalf("the caller answered %v, want the rows' needs named", err)
		}
		if len(doer.requests) != 0 {
			t.Fatalf("the caller sent %d request(s) for a call no row takes", len(doer.requests))
		}
	})
}

// TestAGoingRelationOutsideBothEdgeFamiliesFails: a catalogue entry names the relations its
// verbs write, and a name in neither edge family — the global relation names
// (model.GraphEdgeKinds) and the `block[]` attributes of the typed blocks (pmdoc.LinkKinds) — is
// refused, so no entry can promise an edge the graph has no kind for.
func TestAGoingRelationOutsideBothEdgeFamiliesFails(t *testing.T) {
	families := edgeFamilies()
	for _, relation := range append(slices.Clone(model.GraphEdgeKinds), pmdoc.LinkKinds()...) {
		if !slices.Contains(families, relation) {
			t.Errorf("the catalogue reads no %q, which one of the two edge families declares", relation)
		}
	}
	if slices.Contains(families, "reminds_of") {
		t.Fatal("reminds_of is a relation one of the edge families declares; pick a name neither has")
	}

	withThrowawayToolRoutes(t, "dispatch_whoami", goingToolRoute{
		Method: http.MethodPost, Pattern: "/api/v1/projects", Path: map[string]string{},
		Relations: []string{"mentions", "reminds_of"},
	})
	_, err := buildGoingCatalogue((&server{}).routes())
	if err == nil || !strings.Contains(err.Error(), `relation "reminds_of" is neither a structural edge kind`) {
		t.Fatalf("the build answered %v, want the relation in neither family named", err)
	}
	if strings.Contains(err.Error(), `relation "mentions"`) {
		t.Fatalf("the build refused a relation one family declares: %v", err)
	}
}

// TestTheGoingCatalogueIsMountedOnTheServedTable: the server builds its catalogue from the table
// it mounts, so the route serves what the tests above check rather than a copy built elsewhere.
func TestTheGoingCatalogueIsMountedOnTheServedTable(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/going/catalogue", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET the catalogue: status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeBody[goingCatalogueBody](t, response)
	if body.Total != len(body.Actions) || body.Total != len(goingToolRoutes) {
		t.Fatalf("the catalogue served %d of %d actions, want the %d tool specs that write", len(body.Actions), body.Total, len(goingToolRoutes))
	}
	for _, action := range body.Actions {
		if action.Action != "dispatch_issue" {
			continue
		}
		if !strings.HasPrefix(action.Description, "Create a native Dispatch issue") {
			t.Fatalf("the issue create's description is %q, want the tool spec's own", action.Description)
		}
		if len(action.Routes) != 1 || action.Routes[0].Method != http.MethodPost || action.Routes[0].Pattern != "/api/v1/issues" {
			t.Fatalf("the issue create names the routes %v", action.Routes)
		}
		return
	}
	t.Fatalf("the served catalogue holds no issue create: %d actions", len(body.Actions))
}
