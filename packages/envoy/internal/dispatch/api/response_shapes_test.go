package api

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// Every response body of the Dispatch HTTP API is a named Go type, so an OpenAPI document can be
// reflected out of the route table (INFRA-1406). Many of those types replaced a map literal, and
// encoding/json marshals a map with its keys sorted and a struct in field declaration order: a
// type whose fields are not in the map keys' sorted order would move bytes on the wire. Each row
// below marshals the named type beside the literal the handler wrote before it was named, and
// the two byte slices must be identical; a later reordering of a field fails the row.
func TestResponseShapes(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	artifact := model.Artifact{ID: "art-1", Project: "TEST", Slug: "spec", Name: "Spec", Kind: "doc"}
	version := model.Version{Number: 7, Named: true, Summary: new("a named version"), CreatedAt: at}
	checks := []model.CheckBlock{{BlockID: "b-1", Ref: "check-1", IssueKey: "TEST-1", Text: "go build"}}
	result := model.CheckResult{ID: "res-1", IssueKey: "TEST-1", CheckBlockID: "b-1", Verdict: "pass"}
	ask := model.Ask{ID: "ask-1", Question: "Approve?"}
	comment := model.Comment{ID: "com-1", Body: "a comment"}
	replies := []model.Comment{{ID: "com-2", Body: "a reply"}}
	followers := []model.AskFollower{{SessionID: "s-1", Since: at}}
	edits := []model.AskEdit{{Previous: model.AskEditPrevious{Question: "Approve it?"}, At: "2026-03-04T05:06:07Z"}}
	answers := []model.AskAnswer{{ID: "ans-1", User: "alice", Selected: []string{"yes"}, At: at}}
	reach := askReach{Claimant: new("alice"), Route: new("implementing")}
	approval := model.ArtifactApproval{State: "awaiting", LatestVersion: 7}
	issueState := userIssueState{Pinned: true, LastReadSeq: 11, Dismissed: []string{"d-1"}, Seq: 12}
	agentState := userAgentState{ClearedBefore: new("2026-03-04T05:06:07Z"), ReadThrough: new("2026-03-04T05:06:08Z"), UnreadReplies: 3}
	claim := model.IssueClaim{Actor: model.Actor{Kind: "session", ID: "s-1"}, At: at}
	candidates := []model.DuplicateCandidate{{Key: "TEST-2", Title: "A duplicate", Status: "open", SharedTerms: 3}}
	mismatches := []model.EditPreconditionMismatch{{Scope: "block", BlockID: "b-1", Expected: "t-1", Current: new("t-2")}}
	currentBlocks := []model.EditBlockPrecondition{{ID: "b-1", Token: "t-2"}}
	targets := []pmdoc.Candidate{{Range: pmdoc.Range{From: 3, To: 9}, Context: "the quick brown fox"}}
	participants := []model.Actor{{Kind: "session", ID: "s-2"}}
	lost := []int{1, 4}
	suggestion := "com-3"
	routes := []routeIndexEntry{{Method: "GET", Path: "/api/v1", Auth: "none", Description: "the route index"}}
	users := []dispatchUser{{Login: "alice"}}
	scopes := []string{"person", "approve"}

	// approvalRequestResponseBefore is POST /api/v1/artifacts/{id}/approval-requests's own
	// handler struct, kept here as the field order the named type must marshal in.
	type approvalRequestResponseBefore struct {
		Ask            *model.Ask             `json:"ask"`
		ArtifactID     string                 `json:"artifact_id"`
		Version        int                    `json:"version"`
		Approval       model.ArtifactApproval `json:"approval"`
		Coverage       []string               `json:"coverage"`
		CoverageUnread bool                   `json:"coverage_unread,omitempty"`
	}

	for _, shape := range []struct {
		// name is the Go type the row proves, as the failure names it.
		name string
		// named is a value of that type, every field distinctive.
		named any
		// before is the literal the handler wrote before the type was declared.
		before any
	}{
		{
			name:   "routeIndex",
			named:  routeIndex{Docs: routeIndexDocs, Routes: routes},
			before: map[string]any{"routes": routes, "docs": routeIndexDocs},
		},
		{
			name:   "artifactUploadResponse",
			named:  artifactUploadResponse{Artifact: artifact, Version: version},
			before: map[string]any{"artifact": artifact, "version": version},
		},
		{
			name:   "listUsersResponse",
			named:  listUsersResponse{Users: users},
			before: map[string]any{"users": users},
		},
		{
			name:   "issueRefResolved",
			named:  issueRefResolved{Key: "TEST-1"},
			before: map[string]string{"key": "TEST-1"},
		},
		{
			name:  "issueCheckResults",
			named: issueCheckResults{Checks: checks, Issue: "TEST-1", VerificationArtifactID: "art-2"},
			before: map[string]any{
				"issue": "TEST-1", "verification_artifact_id": "art-2", "checks": checks,
			},
		},
		{
			name:   "checkResultRecorded",
			named:  checkResultRecorded{Line: "- Check b-1 (before_merge): pass.", Result: result},
			before: map[string]any{"result": result, "line": "- Check b-1 (before_merge): pass."},
		},
		{
			name:   "artifactText",
			named:  artifactText{Markdown: "# Spec", Token: "tok-1", Version: new(7)},
			before: map[string]any{"markdown": "# Spec", "version": new(7), "token": "tok-1"},
		},
		{
			name: "editArtifactResponse",
			named: editArtifactResponse{
				Applied:      2,
				Changed:      true,
				LostOps:      lost,
				Token:        "tok-1",
				UnchangedOps: []int{3},
				Version:      &version,
			},
			before: map[string]any{
				"applied":       2,
				"version":       &version,
				"changed":       true,
				"unchanged_ops": []int{3},
				"lost_ops":      lost,
				"token":         "tok-1",
			},
		},
		{
			name:   "userState",
			named:  userState{"TEST-1": issueState, "TEST-2": issueState},
			before: map[string]userIssueState{"TEST-1": issueState, "TEST-2": issueState},
		},
		{
			name:   "userAgentStates",
			named:  userAgentStates{"s-1": agentState, "s-2": agentState},
			before: map[string]userAgentState{"s-1": agentState, "s-2": agentState},
		},
		{
			name: "askRead",
			named: askRead{Ask: ask, askThread: askThread{
				Replies: replies, Edits: edits, Answers: answers, Followers: followers, askReach: reach,
			}},
			before: struct {
				Ask model.Ask `json:"ask"`
				askThread
			}{Ask: ask, askThread: askThread{
				Replies: replies, Edits: edits, Answers: answers, Followers: followers, askReach: reach,
			}},
		},
		{
			name:  "askFollowersRead",
			named: askFollowersRead{Followers: followers},
			before: struct {
				Followers []model.AskFollower `json:"followers"`
			}{Followers: followers},
		},
		{
			name:  "commentRead",
			named: commentRead{Comment: comment, Replies: replies},
			before: struct {
				Comment model.Comment   `json:"comment"`
				Replies []model.Comment `json:"replies"`
			}{Comment: comment, Replies: replies},
		},
		{
			name:  "artifactVersionText",
			named: artifactVersionText{Markdown: "# Spec", Version: version},
			before: struct {
				Markdown string `json:"markdown"`
				model.Version
			}{Markdown: "# Spec", Version: version},
		},
		{
			name: "approvalRequestResponse",
			named: approvalRequestResponse{
				Ask: &ask, ArtifactID: "art-1", Version: 7,
				Approval: approval, Coverage: []string{"a requirement with no check"}, CoverageUnread: true,
			},
			before: approvalRequestResponseBefore{
				Ask: &ask, ArtifactID: "art-1", Version: 7,
				Approval: approval, Coverage: []string{"a requirement with no check"}, CoverageUnread: true,
			},
		},
		{
			name: "userStateStaleError",
			named: userStateStaleError{
				Code: "STATE_STALE", Error: "user state was updated by another write", State: issueState,
			},
			before: map[string]any{
				"code":  "STATE_STALE",
				"error": "user state was updated by another write",
				"state": issueState,
			},
		},
		{
			name:  "issueClaimedError (ISSUE_CLAIMED)",
			named: issueClaimedError{Claim: &claim, Code: "ISSUE_CLAIMED", Error: "s-1 claimed this issue"},
			before: map[string]any{
				"error": "s-1 claimed this issue", "code": "ISSUE_CLAIMED", "claim": &claim,
			},
		},
		{
			name:  "issueClaimedError (CLAIM_CONTENDED)",
			named: issueClaimedError{Claim: nil, Code: "CLAIM_CONTENDED", Error: "the claim changed twice"},
			before: map[string]any{
				"error": "the claim changed twice", "code": "CLAIM_CONTENDED", "claim": (*model.IssueClaim)(nil),
			},
		},
		{
			name: "issueDuplicateError",
			named: issueDuplicateError{
				Candidates: candidates, Code: "POSSIBLE_DUPLICATE", Error: "possible duplicate of TEST-2: A duplicate",
			},
			before: map[string]any{
				"error":      "possible duplicate of TEST-2: A duplicate",
				"code":       "POSSIBLE_DUPLICATE",
				"candidates": candidates,
			},
		},
		{
			name:   "editPreconditionCurrent",
			named:  editPreconditionCurrent{Blocks: currentBlocks, Document: "tok-1"},
			before: map[string]any{"document": "tok-1", "blocks": currentBlocks},
		},
		{
			name: "editPreconditionError",
			named: editPreconditionError{
				Code:       "PRECONDITION_FAILED",
				Current:    editPreconditionCurrent{Blocks: currentBlocks, Document: "tok-1"},
				Error:      "the document moved under this write",
				Mismatches: mismatches,
			},
			before: map[string]any{
				"error":      "the document moved under this write",
				"code":       "PRECONDITION_FAILED",
				"current":    map[string]any{"document": "tok-1", "blocks": currentBlocks},
				"mismatches": mismatches,
			},
		},
		{
			name:  "ambiguousTargetError",
			named: ambiguousTargetError{Candidates: targets, Code: "TARGET_AMBIGUOUS", Error: "the quote resolves in 2 places"},
			before: map[string]any{
				"error": "the quote resolves in 2 places", "code": "TARGET_AMBIGUOUS", "candidates": targets,
			},
		},
		{
			name: "editLostError (lost_ops)",
			named: editLostError{
				Code:         "EDIT_LOST_TO_CONCURRENT_CHANGE",
				Error:        "a concurrent change removed this write's text",
				LostOps:      &lost,
				Participants: participants,
			},
			before: map[string]any{
				"error":        "a concurrent change removed this write's text",
				"code":         "EDIT_LOST_TO_CONCURRENT_CHANGE",
				"participants": participants,
				"lost_ops":     lost,
			},
		},
		{
			name: "editLostError (suggestion)",
			named: editLostError{
				Code:         "EDIT_LOST_TO_CONCURRENT_CHANGE",
				Error:        "a concurrent change removed this write's text",
				Participants: participants,
				Suggestion:   &suggestion,
			},
			before: map[string]any{
				"error":        "a concurrent change removed this write's text",
				"code":         "EDIT_LOST_TO_CONCURRENT_CHANGE",
				"participants": participants,
				"suggestion":   suggestion,
			},
		},
		{
			name:   "errorBody",
			named:  errorBody{Code: "NOT_FOUND", Error: "not found"},
			before: map[string]string{"error": "not found", "code": "NOT_FOUND"},
		},
		{
			name:   "whoamiUser",
			named:  whoamiUser{Kind: "user", Login: "alice", Scopes: scopes},
			before: map[string]any{"kind": "user", "login": "alice", "scopes": scopes},
		},
		{
			name: "whoamiAgent",
			named: whoamiAgent{
				Kind:    "agent",
				Owner:   new("alice"),
				Project: new("TEST"),
				Scopes:  scopes,
				Service: new("legion"),
				Session: new("s-1"),
			},
			before: map[string]any{
				"kind":    "agent",
				"owner":   new("alice"),
				"service": new("legion"),
				"session": new("s-1"),
				"project": new("TEST"),
				"scopes":  scopes,
			},
		},
		{
			name:   "testHookOK",
			named:  testHookOK{OK: true},
			before: map[string]bool{"ok": true},
		},
		{
			name:   "residentRooms",
			named:  residentRooms{Rooms: []string{"art-1", "art-2"}},
			before: map[string][]string{"rooms": {"art-1", "art-2"}},
		},
		{
			name:   "liveWritePauseState",
			named:  liveWritePauseState{Parked: true},
			before: map[string]bool{"parked": true},
		},
		{
			name: "issueComponentsInput",
			named: issueComponentsInput{
				Mode: new("explicit"), IDs: new([]string{"web", "daemon"}), Reason: new("not architectural"),
			},
			before: struct {
				Mode   *string   `json:"mode"`
				IDs    *[]string `json:"ids"`
				Reason *string   `json:"reason"`
			}{
				Mode: new("explicit"), IDs: new([]string{"web", "daemon"}), Reason: new("not architectural"),
			},
		},
	} {
		t.Run(shape.name, func(t *testing.T) {
			named, err := json.Marshal(shape.named)
			if err != nil {
				t.Fatalf("marshal %s: %v", shape.name, err)
			}
			before, err := json.Marshal(shape.before)
			if err != nil {
				t.Fatalf("marshal the literal behind %s: %v", shape.name, err)
			}
			if !bytes.Equal(named, before) {
				t.Fatalf("%s serialises as\n\t%s\nand the literal it replaced as\n\t%s", shape.name, named, before)
			}
		})
	}
}
