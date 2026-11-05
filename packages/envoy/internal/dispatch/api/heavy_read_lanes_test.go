package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
)

// The routes whose statements the database spends its processors on carry a lane of their own,
// and no other route does. The four statements the load test measured in the seconds where every
// request stalls - the search union, the reference closure, an issue's artifacts and the issue
// list - are 101 of the 124 seconds of statement time a burst's aftermath spends (INFRA-592), and
// a lane is what keeps three to five of one of them from running at once. Both reference routes
// run the same closure, so both are in that lane.
func TestTheHeavyReadRoutesCarryTheirLanes(t *testing.T) {
	want := map[string]admission.Lane{
		"GET /api/v1/search":                  admission.Search,
		"GET /api/v1/issues/{key}/references": admission.References,
		"GET /api/v1/references":              admission.References,
		"GET /api/v1/issues/{key}/artifacts":  admission.IssueArtifacts,
		"GET /api/v1/issues":                  admission.IssueList,
	}
	for _, route := range (&server{}).routes() {
		key := route.Method + " " + route.Pattern
		lane, heavy := want[key]
		switch {
		case heavy && route.Lane != lane:
			t.Errorf("%s is in the %q lane, want %q", key, route.Lane, lane)
		case !heavy && route.Lane != admission.NoLane:
			t.Errorf("%s is in the %q lane, want no lane: only the measured heavy reads take one", key, route.Lane)
		case heavy && route.Method != http.MethodGet:
			t.Errorf("%s is in a lane and is not a read; a lane is a read's own limit", key)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("no route %s in the table, so its lane is on nothing", key)
	}
}
