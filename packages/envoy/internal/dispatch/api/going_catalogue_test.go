// going_catalogue_test.go holds the catalogue to the route table it is generated from: the
// eligible set is computed from the tables themselves, as TestEveryMutatingRouteIsClassed
// computes the mutating set (project_scope.go), and every walk below runs over a scratch copy of
// the table rather than over anything this workspace shares.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// throwawayRow is a route no deployment serves, added to a scratch copy of the table so a walk
// is shown failing on a route added without a catalogue entry of its own.
var throwawayRow = routeRow{Method: http.MethodPost, Pattern: "/api/v1/widgets/{id}/paint", Auth: authAny, Description: "Paint a widget."}

// scratchTable is the served table with rows appended, which no mounted server reads.
func scratchTable(rows ...apiRoute) []apiRoute {
	return append((&server{deps: Deps{TestHooksEnabled: true}}).routes(), rows...)
}

// withThrowawayRequest installs a request type for one route for the length of a test, so the
// walk's other direction — a request type naming a route the catalogue does not hold — is shown
// without touching the generated file.
func withThrowawayRequest(t *testing.T, name string, request contracts.GoingRequest) {
	t.Helper()
	if _, held := contracts.GoingRequests[name]; held {
		t.Fatalf("%q already has a request type; name a route the catalogue does not hold", name)
	}
	contracts.GoingRequests[name] = request
	t.Cleanup(func() { delete(contracts.GoingRequests, name) })
}

// TestTheGoingCatalogueHoldsEveryEligibleRoute: the catalogue holds exactly one entry for each
// mutating route a person's credential may call, and that set is computed from the tables rather
// than from a list written beside them. The sub-tests are the negative control: a route added
// without declaring its request type, and one added that declares a request type without
// reaching the catalogue, each fail the same walk.
func TestTheGoingCatalogueHoldsEveryEligibleRoute(t *testing.T) {
	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	catalogue, err := buildGoingCatalogue(routes)
	if err != nil {
		t.Fatalf("build the catalogue of the served table: %v", err)
	}

	// The eligible set, computed here the way TestEveryMutatingRouteIsClassed reads the table:
	// every row that is not a GET, less the test hooks and the rows only a bearer may call.
	eligible := map[string]bool{}
	for _, route := range routes {
		name := route.Method + " " + route.Pattern
		if route.Method == http.MethodGet || strings.Contains(route.Pattern, "/_test/") || route.Auth == authBearer {
			continue
		}
		eligible[name] = true
	}
	held := map[string]bool{}
	for _, action := range catalogue.Actions() {
		if held[action.Action] {
			t.Errorf("the catalogue holds %q twice", action.Action)
		}
		held[action.Action] = true
		if !eligible[action.Action] {
			t.Errorf("the catalogue holds %q, which is no mutating route a person's credential may call", action.Action)
		}
	}
	for name := range eligible {
		if !held[name] {
			t.Errorf("the catalogue holds no entry for %q", name)
		}
	}
	if len(catalogue.Actions()) != len(eligible) {
		t.Fatalf("the catalogue holds %d actions, want the %d eligible routes", len(catalogue.Actions()), len(eligible))
	}

	// Every entry says what the table says about the row: its own method and pattern, a
	// description of one sentence, and the write classification the mutating set makes.
	for _, action := range catalogue.Actions() {
		if action.Method+" "+action.Pattern != action.Action {
			t.Errorf("%q names method %q and pattern %q", action.Action, action.Method, action.Pattern)
		}
		if action.Description == "" {
			t.Errorf("%q has no description", action.Action)
		}
		if strings.Contains(strings.TrimSuffix(action.Description, "."), ". ") {
			t.Errorf("%q has a description of more than one sentence: %q", action.Action, action.Description)
		}
		if !action.Writes {
			t.Errorf("%q is in the mutating set, so the catalogue must call it a write", action.Action)
		}
	}

	t.Run("a route added without a request type fails the walk", func(t *testing.T) {
		_, err := buildGoingCatalogue(scratchTable(apiRoute{routeRow: throwawayRow, Scope: scopeProject}))
		if err == nil || !strings.Contains(err.Error(), "POST /api/v1/widgets/{id}/paint: no request type") {
			t.Fatalf("the walk answered %v, want the throwaway row named as declaring no request type", err)
		}
	})

	t.Run("a route added that declares a request type without reaching the catalogue fails it too", func(t *testing.T) {
		// A row only a bearer may call is no action a person's credential may propose, so it
		// never reaches the catalogue; its request type then names a route the catalogue does
		// not hold.
		bearerRow := throwawayRow
		bearerRow.Auth = authBearer
		withThrowawayRequest(t, bearerRow.Method+" "+bearerRow.Pattern, contracts.GoingRequest{
			Fields:  []contracts.GoingField{{Name: "id", Kind: contracts.GoingFieldString, Required: true}},
			Example: json.RawMessage(`{"id":"1"}`),
		})
		_, err := buildGoingCatalogue(scratchTable(apiRoute{routeRow: bearerRow, Scope: scopeProject}))
		if err == nil || !strings.Contains(err.Error(), "POST /api/v1/widgets/{id}/paint: a request type for a route the catalogue is not eligible to hold") {
			t.Fatalf("the walk answered %v, want the throwaway row named as reaching no entry", err)
		}
	})
}

// TestEveryGoingActionAcceptsItsExampleAndRefusesAMissingArgument: each entry's schema accepts
// the call it carries, and refuses that call with any one required argument left out. Every
// placeholder of the action's own pattern is one of those required arguments, since an action
// that cannot say which issue it addresses is no action a caller can perform.
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
			declared := map[string]bool{}
			for _, field := range action.Request {
				declared[field.Name] = field.Required
			}
			for _, segment := range strings.Split(action.Pattern, "/") {
				if !strings.HasPrefix(segment, "{") {
					continue
				}
				placeholder := strings.Trim(segment, "{}")
				if !declared[placeholder] {
					t.Errorf("the pattern names %q, which the schema does not require", placeholder)
				}
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

// TestTheGoingCatalogueIsSearchedByMeaning: a search over the catalogue finds the action a
// question is about even when the words asked share no term with the action's own name, because
// what is searched is the sentence saying what the action does.
func TestTheGoingCatalogueIsSearchedByMeaning(t *testing.T) {
	handler := newTestHandler(t)
	for _, probe := range []struct {
		query string
		want  string
	}{
		{query: "repair a broken document history", want: "POST /api/v1/artifacts/{id}/rebuild"},
		{query: "hide a conversation I have finished reading", want: "PUT /api/v1/me/agents/{session_id}/state"},
		{query: "stop waiting on a question that turned out moot", want: "POST /api/v1/asks/{id}/resolve"},
	} {
		t.Run(probe.query, func(t *testing.T) {
			// The query's words share no term with the action's own name: not its method, not a
			// segment of its pattern, not a placeholder in it.
			named := goingTerms(strings.ReplaceAll(probe.want, "/", " "))
			for term := range goingTerms(probe.query) {
				if _, shared := named[term]; shared {
					t.Fatalf("the query shares the term %q with the action's own name %q", term, probe.want)
				}
			}
			response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/going/catalogue?q="+strings.ReplaceAll(probe.query, " ", "+")+"&limit=3", nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("GET the catalogue: status=%d body=%s", response.Code, response.Body.String())
			}
			body := decodeBody[goingCatalogueBody](t, response)
			found := make([]string, 0, len(body.Actions))
			for _, action := range body.Actions {
				found = append(found, action.Action)
			}
			if len(found) == 0 || found[0] != probe.want {
				t.Fatalf("the search answered %v, want %q first", found, probe.want)
			}
		})
	}

	t.Run("the whole catalogue is served without a query", func(t *testing.T) {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/going/catalogue?limit=50", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET the catalogue: status=%d body=%s", response.Code, response.Body.String())
		}
		body := decodeBody[goingCatalogueBody](t, response)
		catalogue, err := buildGoingCatalogue((&server{}).routes())
		if err != nil {
			t.Fatalf("build the catalogue: %v", err)
		}
		if body.Total != len(catalogue.Actions()) {
			t.Fatalf("the catalogue served %d actions, want %d", body.Total, len(catalogue.Actions()))
		}
	})

	t.Run("a limit outside its bounds is refused", func(t *testing.T) {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/going/catalogue?limit=0", nil, "alice")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("GET the catalogue with limit=0: status=%d body=%s", response.Code, response.Body.String())
		}
	})
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

	_, err = caller.Do(context.Background(), "POST /api/v1/widgets/{id}/paint", map[string]any{"id": "7"})
	var unknown GoingUnknownActionError
	if !errors.As(err, &unknown) || unknown.Action != "POST /api/v1/widgets/{id}/paint" {
		t.Fatalf("the caller answered %v, want the unknown action refused by name", err)
	}
	if len(doer.requests) != 0 {
		t.Fatalf("the caller sent %d request(s) for an action the catalogue does not hold", len(doer.requests))
	}

	// A call the action's own schema refuses is the same: refused before anything is sent.
	_, err = caller.Do(context.Background(), "POST /api/v1/issues", map[string]any{"project": "INFRA"})
	var refused GoingArgumentError
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, `argument "title" is required`) {
		t.Fatalf("the caller answered %v, want the missing title refused", err)
	}
	if len(doer.requests) != 0 {
		t.Fatalf("the caller sent %d request(s) for a call the schema refuses", len(doer.requests))
	}

	response, err := caller.Do(context.Background(), "POST /api/v1/issues", map[string]any{"project": "INFRA", "title": "A new issue"})
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
		if _, err := caller.Do(context.Background(), "POST /api/v1/issues/{key}/claim", map[string]any{"key": "INFRA-1042"}); err != nil {
			t.Fatalf("claim an issue: %v", err)
		}
		if got := doer.requests[0].URL.Path; got != "/api/v1/issues/INFRA-1042/claim" {
			t.Fatalf("the caller sent %q", got)
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

	withThrowawayRequest(t, throwawayRow.Method+" "+throwawayRow.Pattern, contracts.GoingRequest{
		Fields:    []contracts.GoingField{{Name: "id", Kind: contracts.GoingFieldString, Required: true}},
		Example:   json.RawMessage(`{"id":"1"}`),
		Relations: []string{"mentions", "reminds_of"},
	})
	_, err := buildGoingCatalogue(scratchTable(apiRoute{routeRow: throwawayRow, Scope: scopeProject}))
	if err == nil || !strings.Contains(err.Error(), `relation "reminds_of" is neither a structural edge kind`) {
		t.Fatalf("the walk answered %v, want the relation in neither family named", err)
	}
	if strings.Contains(err.Error(), `relation "mentions"`) {
		t.Fatalf("the walk refused a relation one family declares: %v", err)
	}
}

// TestTheGoingCatalogueIsMountedOnTheServedTable: the server builds its catalogue from the table
// it mounts, so the route serves what the walk above checks rather than a copy built elsewhere.
func TestTheGoingCatalogueIsMountedOnTheServedTable(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/going/catalogue", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET the catalogue: status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeBody[goingCatalogueBody](t, response)
	for _, action := range body.Actions {
		if action.Action == "POST /api/v1/issues" {
			if action.Tool != "dispatch_issue" {
				t.Fatalf("the issue create names tool %q", action.Tool)
			}
			if !strings.HasPrefix(action.Description, "Create a native Dispatch issue") {
				t.Fatalf("the issue create's description is %q, want the tool's first sentence", action.Description)
			}
			return
		}
	}
	t.Fatalf("the served catalogue holds no issue create: %d actions", len(body.Actions))
}
