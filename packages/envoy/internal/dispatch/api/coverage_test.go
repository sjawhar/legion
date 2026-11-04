package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// twoRequirements is a spec marking two requirements, r1 and r2.
const twoRequirements = ":::requirement{#r1}\nThe issue page lists every open ask.\n:::\n\n" +
	":::requirement{#r2}\nAn answered ask leaves the list.\n:::\n"

// checkServing is a Verification document holding one check that serves the named requirements.
func checkServing(id, serves string) string {
	return ":::check{#" + id + " serves=\"" + serves + "\" surface=\"cli\" run=\"go test ./...\"}\n" +
		"The check that proves it.\n:::\n"
}

// planTaskServing is a plan document holding one plan task that serves the named requirements.
func planTaskServing(id, serves string) string {
	return ":::plan_task{#" + id + " serves=\"" + serves + "\"}\nDo the work.\n:::\n"
}

func readCoverage(t *testing.T, handler http.Handler, issueKey string) model.IssueCoverage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/coverage", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the coverage of %s: status=%d body=%s", issueKey, response.Code, response.Body.String())
	}
	return decodeBody[model.IssueCoverage](t, response)
}

// requirementOf is the coverage read's requirement with that block id.
func requirementOf(t *testing.T, coverage model.IssueCoverage, blockID string) model.RequirementCoverage {
	t.Helper()
	for _, requirement := range coverage.Requirements {
		if requirement.BlockID == blockID {
			return requirement
		}
	}
	t.Fatalf("the coverage read holds no requirement %s: %+v", blockID, coverage.Requirements)
	return model.RequirementCoverage{}
}

// A requirement the spec marks is flagged until some check serves it, and the read names each
// requirement in document order with its reference and its words.
func TestCoverageFlagsARequirementWithNoCheckUntilACheckServesIt(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "COV", "Coverage", twoRequirements)

	before := readCoverage(t, handler, issue.Key)
	if before.SpecArtifactID != issue.PrimaryArtifactID || before.HasPlan {
		t.Fatalf("coverage = %+v, want the spec's id %s and no plan", before, issue.PrimaryArtifactID)
	}
	if len(before.Requirements) != 2 || before.Requirements[0].BlockID != "r1" || before.Requirements[1].BlockID != "r2" {
		t.Fatalf("requirements = %+v, want r1 then r2 in document order", before.Requirements)
	}
	if got := before.Requirements[0]; got.Ref != "dispatch://"+issue.Key+"/spec#b-r1" || got.Text != "The issue page lists every open ask." {
		t.Fatalf("r1 = %+v, want its spec block reference and its words", got)
	}
	wantGaps := []string{"r1 has no check", "r2 has no check"}
	if !slices.Equal(before.Gaps, wantGaps) {
		t.Fatalf("gaps = %q, want %q", before.Gaps, wantGaps)
	}

	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		checkServing("k1", "dispatch://"+issue.Key+"/spec#b-r1"))

	after := readCoverage(t, handler, issue.Key)
	if got := requirementOf(t, after, "r1"); got.Checks != 1 || got.PlanTasks != 0 {
		t.Fatalf("r1 after the check = %+v, want one check and no plan task", got)
	}
	if !slices.Equal(after.Gaps, []string{"r2 has no check"}) {
		t.Fatalf("gaps after the check = %q, want r2's alone", after.Gaps)
	}
}

// A check of a descendant issue, at any status, covers the root's requirement, and deleting that
// check brings the flag back.
func TestCoverageCountsADescendantsCheckAndFlagsAgainWhenItIsDeleted(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	root := createInteractionIssue(t, handler, "COV", "Root", twoRequirements)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "COV", "title": "Child", "parent": root.Key, "force": true}, "alice"))
	grandchild := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "COV", "title": "Grandchild", "parent": child.Key, "force": true}, "alice"))

	uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "verification.md",
		checkServing("k1", "dispatch://"+root.Key+"/spec#b-r1"))
	uploadDocument(t, handler, "/api/v1/issues/"+grandchild.Key, "verification.md",
		checkServing("k2", "dispatch://"+root.Key+"/spec#b-r2"))
	// A closed descendant's check counts as much as an open one's.
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+grandchild.Key,
		map[string]any{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close the grandchild: status=%d body=%s", response.Code, response.Body.String())
	}

	covered := readCoverage(t, handler, root.Key)
	if len(covered.Gaps) != 0 {
		t.Fatalf("gaps with a child's and a closed grandchild's check = %q, want none", covered.Gaps)
	}
	if got := requirementOf(t, covered, "r1"); got.Checks != 1 {
		t.Fatalf("r1 = %+v, want the child's one check", got)
	}

	// The child's next version holds no check at all: its requirement is uncovered again.
	uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "verification.md", "No check here any more.\n")
	after := readCoverage(t, handler, root.Key)
	if !slices.Equal(after.Gaps, []string{"r1 has no check"}) {
		t.Fatalf("gaps after the child's check was deleted = %q, want r1's alone", after.Gaps)
	}
	if got := requirementOf(t, after, "r2"); got.Checks != 1 {
		t.Fatalf("r2 = %+v, want the grandchild's check still counted", got)
	}
}

// A spec that marks no requirement answers the one line that says so, and so does an issue
// created without a spec.
func TestCoverageSaysWhenTheSpecMarksNoRequirements(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "COV", "Unmarked", "A spec with prose and no requirement block.\n")

	coverage := readCoverage(t, handler, issue.Key)
	if len(coverage.Requirements) != 0 || !slices.Equal(coverage.Gaps, []string{noRequirementsGap}) {
		t.Fatalf("coverage of an unmarked spec = %+v, want no requirements and %q", coverage, noRequirementsGap)
	}

	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/NOPE-1/coverage", nil, "alice"); response.Code != http.StatusNotFound {
		t.Fatalf("coverage of an issue nobody has: status=%d body=%s, want 404", response.Code, response.Body.String())
	}
}

// A missing plan task is no gap until the issue has a plan document; once plan.md is there, a
// requirement no plan task serves is flagged for it.
func TestCoverageFlagsAMissingPlanTaskOnlyOnceTheIssueHasAPlan(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "COV", "Planned", twoRequirements)
	spec := "dispatch://" + issue.Key + "/spec"
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		checkServing("k1", spec+"#b-r1")+"\n"+checkServing("k2", spec+"#b-r2"))

	before := readCoverage(t, handler, issue.Key)
	if before.HasPlan || len(before.Gaps) != 0 {
		t.Fatalf("coverage before the plan = %+v, want no plan and no gap", before)
	}

	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", planTaskServing("t1", spec+"#b-r1"))

	after := readCoverage(t, handler, issue.Key)
	if !after.HasPlan {
		t.Fatalf("coverage after plan.md = %+v, want has_plan", after)
	}
	if got := requirementOf(t, after, "r1"); got.PlanTasks != 1 {
		t.Fatalf("r1 = %+v, want the plan task counted", got)
	}
	if !slices.Equal(after.Gaps, []string{"r2 has no plan task"}) {
		t.Fatalf("gaps with a plan = %q, want r2's missing plan task alone", after.Gaps)
	}
}

// approvalAnswer is what POST /api/v1/artifacts/{id}/approval-requests answers, as far as the
// coverage lines and the ask it records.
type approvalAnswer struct {
	Ask            *model.Ask `json:"ask"`
	Coverage       []string   `json:"coverage"`
	CoverageUnread bool       `json:"coverage_unread"`
}

// requestSpecApproval asks for the document's approval and answers what came back; 200 is the
// repeat an already-open request answers, 201 a request this call wrote.
func requestSpecApproval(t *testing.T, handler http.Handler, artifactID string) approvalAnswer {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests",
		map[string]any{"actor": sessionActor()})
	if response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("request approval: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[approvalAnswer](t, response)
}

// An approval request's answer names each requirement with no check, never a missing plan task,
// and the request is recorded as it always was. A spec that marks no requirements says so, and a
// spec every requirement of which has a check gets no gap line.
func TestApprovalRequestNamesEachRequirementWithNoCheck(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "COV", "Approving", twoRequirements)
	spec := "dispatch://" + issue.Key + "/spec"
	// A plan whose only task serves r1 leaves r2 with no plan task, which the answer never names.
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", planTaskServing("t1", spec+"#b-r1"))
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checkServing("k1", spec+"#b-r1"))

	answer := requestSpecApproval(t, handler, issue.PrimaryArtifactID)
	if answer.Ask == nil {
		t.Fatalf("the request opened no ask: %+v", answer)
	}
	if !slices.Equal(answer.Coverage, []string{"r2 has no check"}) {
		t.Fatalf("coverage = %q, want r2's missing check alone and no plan-task line", answer.Coverage)
	}
	// The gaps the route reports are the same, with the plan task named there and not here.
	if gaps := readCoverage(t, handler, issue.Key).Gaps; !slices.Equal(gaps, []string{"r2 has no check", "r2 has no plan task"}) {
		t.Fatalf("the route's gaps = %q, want the missing check and the missing plan task", gaps)
	}

	// A check for r2 covers the spec; the next request carries no gap line.
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		checkServing("k1", spec+"#b-r1")+"\n"+checkServing("k2", spec+"#b-r2"))
	covered := requestSpecApproval(t, handler, issue.PrimaryArtifactID)
	if len(covered.Coverage) != 0 {
		t.Fatalf("coverage of a covered spec = %q, want none", covered.Coverage)
	}
}

// An approval request on a spec that marks no requirements says so, and still opens the request.
func TestApprovalRequestSaysWhenTheSpecMarksNoRequirements(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "COV", "Unmarked", "A spec with prose and no requirement block.\n")

	answer := requestSpecApproval(t, handler, issue.PrimaryArtifactID)
	if answer.Ask == nil {
		t.Fatalf("the request opened no ask: %+v", answer)
	}
	if !slices.Equal(answer.Coverage, []string{noRequirementsGap}) {
		t.Fatalf("coverage = %q, want %q", answer.Coverage, noRequirementsGap)
	}
}

// unreadableSpecStore fails the next read of one document's stored state, whichever way the
// document service reads it: its stamp, its history or its rendering.
type unreadableSpecStore struct {
	docs.VersionedStore
	mu     sync.Mutex
	broken map[string]bool
}

// failNextRead arms the next read of room to fail, once.
func (s *unreadableSpecStore) failNextRead(room string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broken[room] = true
}

func (s *unreadableSpecStore) take(room string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	broken := s.broken[room]
	delete(s.broken, room)
	return broken
}

func (s *unreadableSpecStore) DocumentStamp(ctx context.Context, room string) (docs.DocumentStamp, error) {
	if s.take(room) {
		return docs.DocumentStamp{}, errors.New("the document store this test stands on is unavailable")
	}
	return s.VersionedStore.DocumentStamp(ctx, room)
}

func (s *unreadableSpecStore) LoadDocument(ctx context.Context, room string) (docs.LoadedDocument, error) {
	if s.take(room) {
		return docs.LoadedDocument{}, errors.New("the document store this test stands on is unavailable")
	}
	return s.VersionedStore.LoadDocument(ctx, room)
}

// A coverage read that fails refuses nothing: the request is recorded as it always was — it
// never read the live spec before the answer carried coverage — and the answer says the
// coverage was not read rather than naming gaps it could not count.
func TestApprovalRequestGoesThroughWhenTheCoverageReadFails(t *testing.T) {
	var broken *unreadableSpecStore
	handler, _, _ := newTestServer(t, testServerOptions{
		persistence: func(database *store.Store) docs.VersionedStore {
			broken = &unreadableSpecStore{VersionedStore: docs.NewPgVersioned(database), broken: map[string]bool{}}
			return broken
		},
	})
	issue := createInteractionIssue(t, handler, "COVF", "Unread coverage", twoRequirements)

	// The next read of the spec's stored state fails, which is the read the coverage folds.
	broken.failNextRead(issue.PrimaryArtifactID)
	answer := requestSpecApproval(t, handler, issue.PrimaryArtifactID)
	if answer.Ask == nil {
		t.Fatalf("the request opened no ask when the coverage read failed: %+v", answer)
	}
	if !answer.CoverageUnread || len(answer.Coverage) != 0 {
		t.Fatalf("answer = %+v, want coverage_unread with no gap lines", answer)
	}
	if read := readApproval(t, handler, issue.PrimaryArtifactID); read.Approval == nil || read.Approval.State != "awaiting" {
		t.Fatalf("spec approval after the request = %#v, want awaiting", read.Approval)
	}

	// The read that follows works, so the same request's next answer names the spec's gaps.
	again := requestSpecApproval(t, handler, issue.PrimaryArtifactID)
	if again.CoverageUnread || !slices.Equal(again.Coverage, []string{"r1 has no check", "r2 has no check"}) {
		t.Fatalf("answer after the read recovered = %+v, want both requirements' missing checks", again)
	}
}
