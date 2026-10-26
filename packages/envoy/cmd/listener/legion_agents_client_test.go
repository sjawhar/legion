package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// GET /api/v1/issues/{key}/collaborators on the real Dispatch mux, its ENVOY_URL the real listener
// mux over real NATS: the registry entries the daemon's batch write recorded for the issue come
// back as the issue's agents, merged by session id with a session that also commented, the
// replaced one naming its successor, beside the people the issue's own rows name.
func TestDispatchIssueCollaboratorsReadTheListenersLegionAgentRegistry(t *testing.T) {
	l := newLegionListener(t, setupPublishTestClient(t))
	listener := httptest.NewServer(l.mux)
	t.Cleanup(listener.Close)
	rig := &dispatchRig{database: migratedDispatchDatabase(t)}
	rig.dispatch = dispatchMux(t, rig.database, listener.URL)
	key := rig.issueKey(t, "Collaborators")

	l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents", `{"entries":[`+
		legionEntry("ses-old-reviewer", key, "reviewer", "2026-10-26T01:00:00Z")+`,`+
		legionEntry("ses-new-reviewer", key, "reviewer", "2026-10-26T02:00:00Z")+`]}`)
	if commented := rig.asSession(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body":  "Reviewed",
		"actor": map[string]any{"kind": "session", "id": "ses-new-reviewer", "origin": map[string]any{"session_title": "stamped title"}},
	}); commented.Code != http.StatusCreated {
		t.Fatalf("the reviewer's comment: status=%d body=%s", commented.Code, commented.Body.String())
	}

	response := rig.asHuman(t, http.MethodGet, "/api/v1/issues/"+key+"/collaborators", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("read collaborators: status=%d body=%s", response.Code, response.Body.String())
	}
	answer := decodeRigBody[struct {
		People []struct {
			Login   string   `json:"login"`
			Reasons []string `json:"reasons"`
		} `json:"people"`
		Agents []struct {
			SessionID string   `json:"session_id"`
			Title     *string  `json:"title"`
			Reasons   []string `json:"reasons"`
			Legion    *struct {
				Role       string  `json:"role"`
				RoleToken  string  `json:"role_token"`
				StartedAt  string  `json:"started_at"`
				ReplacedBy *string `json:"replaced_by"`
				ReplacedAt *string `json:"replaced_at"`
			} `json:"legion"`
		} `json:"agents"`
		LegionRegistry string `json:"legion_registry"`
	}](t, response)
	if answer.LegionRegistry != "ok" {
		t.Fatalf("legion_registry = %q, want ok: %s", answer.LegionRegistry, response.Body.String())
	}
	if len(answer.People) != 1 || answer.People[0].Login != rigHuman || !slices.Equal(answer.People[0].Reasons, []string{"assignee"}) {
		t.Fatalf("people = %+v, want %s as the assignee", answer.People, rigHuman)
	}
	if len(answer.Agents) != 2 {
		t.Fatalf("agents = %s, want the two reviewers", response.Body.String())
	}
	current, replaced := answer.Agents[0], answer.Agents[1]
	token := "legion-infra-" + strings.ToLower(key) + "-reviewer"
	if current.SessionID != "ses-new-reviewer" || current.Legion == nil || current.Legion.Role != "reviewer" ||
		current.Legion.RoleToken != token || current.Legion.StartedAt != "2026-10-26T02:00:00Z" ||
		current.Legion.ReplacedBy != nil || current.Legion.ReplacedAt != nil ||
		!slices.Equal(current.Reasons, []string{"posted"}) ||
		current.Title == nil || *current.Title != "Legion reviewer · "+key {
		t.Fatalf("the current reviewer = %s, want the newest reviewer, who commented, under the registry's title", response.Body.String())
	}
	if replaced.SessionID != "ses-old-reviewer" || replaced.Legion == nil ||
		deref(replaced.Legion.ReplacedBy) != "ses-new-reviewer" || deref(replaced.Legion.ReplacedAt) != "2026-10-26T02:00:00Z" ||
		len(replaced.Reasons) != 0 {
		t.Fatalf("the replaced reviewer = %s, want it replaced by ses-new-reviewer", response.Body.String())
	}
}
