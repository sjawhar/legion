package api

import (
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// groupsTimeline is the part of the timeline's answer the session-group tests read.
type groupsTimeline struct {
	PRs []struct {
		ID          string  `json:"id"`
		ParentAgent *string `json:"parent_agent"`
	} `json:"prs"`
	FacetCounts     map[string]map[string]int `json:"facet_counts"`
	ColorCounts     map[string]map[string]int `json:"color_counts"`
	SessionGroups   []sessionGroupBody        `json:"session_groups"`
	SessionTitles   map[string]string         `json:"session_titles"`
	SessionPRCounts map[string]int            `json:"session_pr_counts"`
	Measures        struct {
		Measures struct {
			ReworkShare float64 `json:"rework_share"`
		} `json:"measures"`
	} `json:"measures"`
}

const groupsWindow = "from=2024-03-01T00:00:00Z&to=2024-03-02T00:00:00Z"

// seedGroupsTimeline writes the delivery settings and, inside groupsWindow, one population pull
// request per entry of firstSessions (#1 first, a "" entry naming no session; rework marks a
// rework pull request), and records the given session titles.
func seedGroupsTimeline(t *testing.T, database *store.Store, firstSessions []string, rework map[int]bool, titles map[string]string) {
	t.Helper()
	ctx := t.Context()
	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	merged := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	for index, session := range firstSessions {
		number := index + 1
		at := merged.Add(time.Duration(number) * time.Minute)
		sessions := []string{}
		if session != "" {
			sessions = []string{session}
		}
		if err := delivery.UpsertPullRequest(ctx, database.Pool, delivery.DeliveryPullRequest{
			Repo: "acme/widgets", Number: number, Title: "change " + strconv.Itoa(number),
			URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(number), Author: "octocat",
			MergedAt: &at, CreatedAt: &at, Sessions: sessions, Rework: rework[number],
		}); err != nil {
			t.Fatalf("seed PR #%d: %v", number, err)
		}
	}
	recordSessionTitles(t, database, merged.Add(-time.Hour), titles)
}

func readGroupsTimeline(t *testing.T, handler http.Handler, query string) groupsTimeline {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET timeline?%s: status = %d, body = %s", query, response.Code, response.Body.String())
	}
	var body groupsTimeline
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode timeline?%s: %v", query, err)
	}
	return body
}

func (body groupsTimeline) parentAgents() map[string]string {
	agents := map[string]string{}
	for _, pr := range body.PRs {
		if pr.ParentAgent == nil {
			agents[pr.ID] = "<null>"
			continue
		}
		agents[pr.ID] = *pr.ParentAgent
	}
	return agents
}

// TestTimelineShowsAGroupAsTheParentAgentOfItsSessionsPRs: a saved group becomes the Parent agent
// of every pull request whose first session it holds, in the pull requests, the facet counts, the
// colours, the facet filter and the measures, while the Session facet keeps each session apart.
func TestTimelineShowsAGroupAsTheParentAgentOfItsSessionsPRs(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedGroupsTimeline(t, database, []string{"s1", "s2", "s3", ""}, map[int]bool{2: true}, map[string]string{
		"s1": "Planner one", "s2": "Planner two", "s3": "Reviewer",
	})
	group := createSessionGroup(t, handler, "Planners", "s1", "s2")
	value := "group:" + group.ID

	body := readGroupsTimeline(t, handler, groupsWindow)
	wantAgents := map[string]string{
		"acme/widgets#1": value, "acme/widgets#2": value, "acme/widgets#3": "Reviewer", "acme/widgets#4": "<null>",
	}
	if got := body.parentAgents(); !reflect.DeepEqual(got, wantAgents) {
		t.Errorf("parent_agent by PR = %v, want %v", got, wantAgents)
	}
	wantCounts := map[string]int{value: 2, "Reviewer": 1, deliveryNoSession: 1}
	if got := body.FacetCounts["parent_agent"]; !reflect.DeepEqual(got, wantCounts) {
		t.Errorf("facet_counts.parent_agent = %v, want %v", got, wantCounts)
	}
	if got := body.ColorCounts["parent_agent"]; !reflect.DeepEqual(got, wantCounts) {
		t.Errorf("color_counts.parent_agent = %v, want %v", got, wantCounts)
	}
	if got, want := body.FacetCounts["session"], map[string]int{"s1": 1, "s2": 1, "s3": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("facet_counts.session = %v, want each session apart %v", got, want)
	}
	if len(body.SessionGroups) != 1 || body.SessionGroups[0].ID != group.ID || body.SessionGroups[0].Name != "Planners" {
		t.Errorf("session_groups = %+v, want the one group", body.SessionGroups)
	}
	if want := map[string]string{"s1": "Planner one", "s2": "Planner two", "s3": "Reviewer"}; !reflect.DeepEqual(body.SessionTitles, want) {
		t.Errorf("session_titles = %v, want %v", body.SessionTitles, want)
	}
	if want := map[string]int{"s1": 1, "s2": 1, "s3": 1}; !reflect.DeepEqual(body.SessionPRCounts, want) {
		t.Errorf("session_pr_counts = %v, want %v", body.SessionPRCounts, want)
	}

	filtered := readGroupsTimeline(t, handler, groupsWindow+"&parent_agent="+value)
	if got := deliveryPRIDsOf(filtered); !reflect.DeepEqual(got, []string{"acme/widgets#1", "acme/widgets#2"}) {
		t.Errorf("parent_agent=%s prs = %v, want #1 and #2", value, got)
	}
	if share := filtered.Measures.Measures.ReworkShare; math.Abs(share-0.5) > 1e-9 {
		t.Errorf("rework_share under parent_agent=%s = %v, want 0.5 (#2 of #1 and #2)", value, share)
	}
}

// TestDeletingAGroupRestoresItsSessionsOwnValues: once its group is gone, each session's pull
// request shows the session by its title again.
func TestDeletingAGroupRestoresItsSessionsOwnValues(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedGroupsTimeline(t, database, []string{"s1", "s2"}, nil, map[string]string{"s1": "Planner one", "s2": "Planner two"})
	group := createSessionGroup(t, handler, "Planners", "s1", "s2")
	if response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/delivery/session-groups/"+group.ID, nil, "alice"); response.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, body = %s", response.Code, response.Body.String())
	}
	want := map[string]string{"acme/widgets#1": "Planner one", "acme/widgets#2": "Planner two"}
	if got := readGroupsTimeline(t, handler, groupsWindow).parentAgents(); !reflect.DeepEqual(got, want) {
		t.Errorf("parent_agent by PR after delete = %v, want %v", got, want)
	}
}

// TestALaterSessionTakingAGroupsNameStandsApart: a session that takes a group's name as its title
// after the group was named, without being in it, shows with its id beside the title, so the
// group and the session count apart.
func TestALaterSessionTakingAGroupsNameStandsApart(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedGroupsTimeline(t, database, []string{"s1", "s5"}, nil, map[string]string{"s1": "Planner", "s5": "Builder"})
	group := createSessionGroup(t, handler, "Planner", "s1")
	recordSessionTitles(t, database, time.Date(2024, 3, 1, 13, 0, 0, 0, time.UTC), map[string]string{"s5": "Planner"})

	body := readGroupsTimeline(t, handler, groupsWindow)
	value := "group:" + group.ID
	want := map[string]string{"acme/widgets#1": value, "acme/widgets#2": "Planner (s5)"}
	if got := body.parentAgents(); !reflect.DeepEqual(got, want) {
		t.Errorf("parent_agent by PR = %v, want %v", got, want)
	}
	if got, want := body.FacetCounts["parent_agent"], map[string]int{value: 1, "Planner (s5)": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("facet_counts.parent_agent = %v, want %v", got, want)
	}
}

// TestGroupedMeasuresMatchTheTimelines: the measures route, read for a group's filter, answers what
// the timeline carries for the same query, as for every other facet.
func TestGroupedMeasuresMatchTheTimelines(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedGroupsTimeline(t, database, []string{"s1", "s2", "s3"}, map[int]bool{2: true}, map[string]string{"s1": "One", "s2": "Two", "s3": "Three"})
	group := createSessionGroup(t, handler, "Planners", "s1", "s2")
	query := groupsWindow + "&parent_agent=group:" + group.ID
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?"+query, nil, "alice")
	var timeline struct {
		Measures deliveryMeasuresResponse `json:"measures"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &timeline); err != nil {
		t.Fatalf("decode timeline: %v", err)
	}
	got, want := timeline.Measures, getDeliveryMeasures(t, handler, query)
	got.ComputedAt, want.ComputedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("timeline measures = %+v, want the measures route's %+v", got, want)
	}
	if math.Abs(want.Measures.ReworkShare-0.5) > 1e-9 {
		t.Errorf("measures rework_share = %v, want 0.5 over the group's two pull requests", want.Measures.ReworkShare)
	}
}

func deliveryPRIDsOf(body groupsTimeline) []string {
	ids := make([]string, len(body.PRs))
	for i, pr := range body.PRs {
		ids[i] = pr.ID
	}
	return ids
}
