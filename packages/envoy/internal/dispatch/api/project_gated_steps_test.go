package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// gatedStepsAnswer is what PUT /api/v1/projects/{key}/gated-steps answers.
type gatedStepsAnswer struct {
	Project string   `json:"project"`
	Steps   []string `json:"steps"`
}

// publishGatedSteps publishes a deployment's list the way its daemon does: over the credential
// the deployment configured, which acts for a machine and holds no personal scope.
func publishGatedSteps(t *testing.T, handler http.Handler, project string, steps []string) gatedStepsAnswer {
	t.Helper()
	response := agentRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/gated-steps",
		map[string]any{"steps": steps, "actor": sessionActor()}, "agent-token")
	if response.Code != http.StatusOK {
		t.Fatalf("publish %v on %s: status=%d body=%s", steps, project, response.Code, response.Body.String())
	}
	return decodeBody[gatedStepsAnswer](t, response)
}

// listedGatedSteps reads one project's published list back off GET /api/v1/projects, which is
// where a reader of Dispatch finds what a deployment gates.
func listedGatedSteps(t *testing.T, handler http.Handler, project string) []string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list projects: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, row := range decodeBody[[]model.Project](t, response) {
		if row.Key == project {
			return row.GatedSteps
		}
	}
	t.Fatalf("project %s is not in the list: %s", project, response.Body.String())
	return nil
}

// projectUpdates counts the project events the publish appends, so a republish that changes
// nothing can be told from one that changes the list.
func projectUpdates(t *testing.T, database *store.Store, project string) int {
	t.Helper()
	var total int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from events where project_key = $1 and type = 'project.updated'`,
		project).Scan(&total); err != nil {
		t.Fatalf("count project events of %s: %v", project, err)
	}
	return total
}

// A deployment's design-gate steps reach the project row through Dispatch's own API, and a
// person reading that row can tell, step by step, which of the root issue's documents is waiting
// for them. The daemon is not in this test: the route is called with the credential class a
// daemon holds and the list is read back off the project list.
func TestGatedStepsOnTheProjectRow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "GATED", "The root", "A spec\n")
	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "UNGATED", "name": "Ungated project",
	}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create the ungated project: status=%d body=%s", created.Code, created.Body.String())
	}

	// A deployment that gates its Verification document and then its spec carries the two in
	// that order, and one that gates nothing carries the empty list. The two are read off the
	// project list, not off the answer the publish gave back.
	if answer := publishGatedSteps(t, handler, "GATED", []string{"criteria", "spec"}); answer.Project != "GATED" ||
		!slices.Equal(answer.Steps, []string{"criteria", "spec"}) {
		t.Fatalf("publish answered %#v, want GATED gating criteria then spec", answer)
	}
	publishGatedSteps(t, handler, "UNGATED", []string{})
	if listed := listedGatedSteps(t, handler, "GATED"); !slices.Equal(listed, []string{"criteria", "spec"}) {
		t.Fatalf("GATED gates %v, want criteria then spec", listed)
	}
	if listed := listedGatedSteps(t, handler, "UNGATED"); len(listed) != 0 {
		t.Fatalf("UNGATED gates %v, want nothing", listed)
	}

	// A publish Dispatch cannot read leaves the stored list where it was: a body naming no list
	// is not the empty list, so a malformed publish cannot clear a gate, a step Legion does not
	// know is refused here rather than stored for a reader to puzzle over, and a step named
	// twice is refused as the deployment's own loader refuses it, since one gate record is kept
	// per step.
	for _, refused := range []struct {
		why  string
		body map[string]any
	}{
		{why: "a body naming no list", body: map[string]any{"actor": sessionActor()}},
		{why: "a list naming the plan", body: map[string]any{"steps": []string{"spec", "plan"}, "actor": sessionActor()}},
		{why: "a list naming the spec twice", body: map[string]any{"steps": []string{"spec", "criteria", "spec"}, "actor": sessionActor()}},
	} {
		response := agentRequest(t, handler, http.MethodPut, "/api/v1/projects/GATED/gated-steps", refused.body, "agent-token")
		code, _ := refusalOf(t, response)
		if response.Code != http.StatusBadRequest || code != "INVALID_PROJECT" {
			t.Fatalf("%s: status=%d body=%s, want 400 INVALID_PROJECT", refused.why, response.Code, response.Body.String())
		}
	}
	if listed := listedGatedSteps(t, handler, "GATED"); !slices.Equal(listed, []string{"criteria", "spec"}) {
		t.Fatalf("a refused publish left GATED gating %v, want criteria then spec", listed)
	}

	// A boot republishes, and a boot on an unchanged list must leave the project's log alone:
	// the update is conditional on the list differing. A changed list is one event, and the
	// order it names is data, so a list nobody would have sorted that way reads back as it was
	// written.
	published := projectUpdates(t, database, "GATED")
	if published != 1 {
		t.Fatalf("the first publish appended %d project events, want one", published)
	}
	publishGatedSteps(t, handler, "GATED", []string{"criteria", "spec"})
	if again := projectUpdates(t, database, "GATED"); again != published {
		t.Fatalf("republishing the same list appended %d project events, want the %d already there", again, published)
	}
	gated := []string{"spec", "decisions", "criteria"}
	publishGatedSteps(t, handler, "GATED", gated)
	if changed := projectUpdates(t, database, "GATED"); changed != published+1 {
		t.Fatalf("a changed list appended %d project events, want %d", changed, published+1)
	}
	if listed := listedGatedSteps(t, handler, "GATED"); !slices.Equal(listed, gated) {
		t.Fatalf("GATED gates %v, want %v in the order the deployment named them", listed, gated)
	}
	var payload []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select payload from events where project_key = 'GATED' and type = 'project.updated'
		order by id desc limit 1
	`).Scan(&payload); err != nil {
		t.Fatalf("read the project event: %v", err)
	}
	var event struct {
		Key        string   `json:"key"`
		GatedSteps []string `json:"gated_steps"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("decode the project event %s: %v", payload, err)
	}
	if event.Key != "GATED" || !slices.Equal(event.GatedSteps, gated) {
		t.Fatalf("the project event carries %#v, want GATED gating %v", event, gated)
	}

	// The published list and the approval records of the root issue's own documents are enough
	// to say where each step stands. The three states a reader must tell apart are set up on one
	// issue: its spec approved, changes asked for on its Verification document, and its
	// decisions document still with the person it was sent to.
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", "# Verification\n\nOne.\n")
	decisions := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "decisions.md", "# Decisions\n\nOne.\n")
	approved := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if approved.Code != http.StatusCreated {
		t.Fatalf("request approval of the spec: status=%d body=%s", approved.Code, approved.Body.String())
	}
	askID := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, approved).Ask.ID
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		map[string]any{"selected": []string{"Approve"}}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("approve the spec: status=%d body=%s", answered.Code, answered.Body.String())
	}
	for _, document := range []string{verification.Artifact.ID, decisions.Artifact.ID} {
		if requested := sessionRequest(t, handler, http.MethodPost,
			"/api/v1/artifacts/"+document+"/approval-requests", map[string]any{"actor": sessionActor()}); requested.Code != http.StatusCreated {
			t.Fatalf("request approval of %s: status=%d body=%s", document, requested.Code, requested.Body.String())
		}
	}
	if review := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+verification.Artifact.ID+"/reviews",
		map[string]any{"state": "changes_requested", "reason": "Name the case nobody checks."}, "alice"); review.Code != http.StatusCreated {
		t.Fatalf("ask for changes on the Verification document: status=%d body=%s", review.Code, review.Body.String())
	}

	// The join a reader makes: each published step names one of the issue's documents, and that
	// document's approval says where the step stands.
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the issue: status=%d body=%s", read.Code, read.Body.String())
	}
	documents := decodeBody[struct {
		Artifacts []model.Artifact `json:"artifacts"`
	}](t, read).Artifacts
	stepDocument := func(step string) (model.Artifact, bool) {
		for _, document := range documents {
			switch step {
			case model.GatedStepSpec:
				if document.Primary {
					return document, true
				}
			case model.GatedStepCriteria:
				if document.Role == model.ArtifactRoleVerification {
					return document, true
				}
			case model.GatedStepDecisions:
				if document.Slug == "decisions-md" {
					return document, true
				}
			}
		}
		return model.Artifact{}, false
	}
	state := map[string]string{}
	for _, step := range listedGatedSteps(t, handler, "GATED") {
		document, found := stepDocument(step)
		if !found {
			t.Fatalf("the %s step names no document of %s: %#v", step, issue.Key, documents)
		}
		approval := readApproval(t, handler, document.ID)
		if approval.Approval == nil {
			t.Fatalf("the %s step's document %s carries no approval", step, document.Name)
		}
		state[step] = approval.Approval.State
	}
	if state[model.GatedStepSpec] != "approved" ||
		state[model.GatedStepCriteria] != "changes_requested" ||
		state[model.GatedStepDecisions] != "awaiting" {
		t.Fatalf("the steps stand at %#v, want spec approved, criteria changes_requested and decisions awaiting", state)
	}
}
