package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/session"
)

// Dispatch's listener client (internal/dispatch/envoy) reads the Legion agent registry through the
// real listener mux over real NATS, so the two sides' shapes are one: what the daemon's batch write
// records, the client's Agent, RoleAgents and IssueAgents read back, with live and the replacement
// the listener derives, an unknown session as ErrNotFound and an issue with no agents as none.
func TestDispatchClientReadsTheLegionAgentRegistry(t *testing.T) {
	l := newLegionListener(t, setupPublishTestClient(t))
	server := httptest.NewServer(l.mux)
	t.Cleanup(server.Close)
	l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents", `{"entries":[`+
		legionEntry("ses-old-reviewer", "INFRA-448", "reviewer", "2026-10-26T01:00:00Z")+`,`+
		legionEntry("ses-new-reviewer", "INFRA-448", "reviewer", "2026-10-26T02:00:00Z")+`,`+
		legionEntry("ses-tester", "INFRA-448", "tester", "2026-10-26T00:30:00Z")+`]}`)
	if err := l.deps.sessions.Put("ses-new-reviewer", session.SessionEntry{Port: 1, MachineID: "test-machine"}); err != nil {
		t.Fatalf("register ses-new-reviewer: %v", err)
	}
	client := dispatchenvoy.New(server.URL)
	ctx := context.Background()

	old, err := client.Agent(ctx, "ses-old-reviewer")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if old.LegionProject != "infra" || old.Issue != "INFRA-448" || old.Role != "reviewer" ||
		old.RoleToken != "legion-infra-infra-448-reviewer" || old.Title != "Legion reviewer · INFRA-448" ||
		old.Harness != "omp" || old.StartedAt != "2026-10-26T01:00:00Z" || old.RecordedAt == "" || old.Live ||
		deref(old.ReplacedBy) != "ses-new-reviewer" || deref(old.ReplacedAt) != "2026-10-26T02:00:00Z" {
		t.Fatalf("agent = %#v", old)
	}
	if _, err := client.Agent(ctx, "ses-unknown"); !errors.Is(err, dispatchenvoy.ErrNotFound) {
		t.Fatalf("an unknown session: %v, want ErrNotFound", err)
	}

	reviewers, err := client.RoleAgents(ctx, "legion-infra-infra-448-reviewer")
	if err != nil || len(reviewers) != 2 {
		t.Fatalf("role agents = %#v, %v", reviewers, err)
	}
	if current := reviewers[0]; current.SessionID != "ses-new-reviewer" || !current.Live || current.ReplacedBy != nil || current.ReplacedAt != nil {
		t.Fatalf("the current reviewer = %#v, want the live new reviewer, not replaced", current)
	}
	if reviewers[1].SessionID != "ses-old-reviewer" || reviewers[1].Live {
		t.Fatalf("the replaced reviewer = %#v", reviewers[1])
	}

	agents, err := client.IssueAgents(ctx, "INFRA-448")
	if err != nil {
		t.Fatalf("issue agents: %v", err)
	}
	var ids []string
	for _, agent := range agents {
		ids = append(ids, agent.SessionID)
	}
	if len(ids) != 3 || ids[0] != "ses-tester" || ids[1] != "ses-new-reviewer" || ids[2] != "ses-old-reviewer" {
		t.Fatalf("issue agents = %v, want the tester, then the reviewers newest first", ids)
	}
	if none, err := client.IssueAgents(ctx, "INFRA-1"); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("an issue with no agents = %#v, %v; want none", none, err)
	}
	if _, err := client.IssueAgents(ctx, "infra-448"); !errors.Is(err, dispatchenvoy.ErrUnavailable) {
		t.Fatalf("a malformed issue key: %v, want ErrUnavailable carrying the listener's 400", err)
	}
}
