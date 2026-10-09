package docs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// The reads a settlement makes for its copied ask blocks run at every settlement of a copy, which
// never gets an ask row of its own. Each must reach the owner's documents through an index - the
// issue's through artifacts_issue_key, a project's unlinked documents through
// artifacts_project_documents - and read events through events_ask_payload_id, never scanning a
// table that grows with every document or event.
func TestCopiedAskQueriesAvoidSequentialScans(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	issue, project := "DOC-1", "DOC"
	sourcesOfIssue, issueKey := copiedAskSourcesQuery(artifactOwner{IssueKey: &issue, Project: project})
	sourcesOfProject, projectKey := copiedAskSourcesQuery(artifactOwner{Project: project})
	copiesOnIssue, _ := owedCopiesQuery(artifactOwner{IssueKey: &issue, Project: project})
	copiesOnProject, _ := owedCopiesQuery(artifactOwner{Project: project})
	document := "00000000-0000-4000-8000-000000000000"
	blocks := []string{"decision"}
	patterns := []string{"%ask{#decision %"}
	for _, query := range []struct {
		name     string
		sql      string
		relation string
		index    string
		args     []any
	}{
		{"an issue document's copy sources", sourcesOfIssue, "artifacts", "artifacts_issue_key", []any{blocks, document, issueKey}},
		{"a project document's copy sources", sourcesOfProject, "artifacts", "artifacts_project_documents", []any{blocks, document, projectKey}},
		{"the copies of an issue document's retracted ask", copiesOnIssue, "artifacts", "artifacts_issue_key", []any{blocks, document, issueKey, patterns}},
		{"the copies of a project document's retracted ask", copiesOnProject, "artifacts", "artifacts_project_documents", []any{blocks, document, projectKey, patterns}},
		// The lateral read of each candidate's latest version: its unique (artifact_id, number).
		{"the latest versions of an issue's copies", copiesOnIssue, "artifact_versions", "artifact_versions_artifact_id_number_key", []any{blocks, document, issueKey, patterns}},
		{"the latest versions of a project's copies", copiesOnProject, "artifact_versions", "artifact_versions_artifact_id_number_key", []any{blocks, document, projectKey, patterns}},
		{"what a copied ask asked before an edit", copiedAskContentsQuery, "events", "events_ask_payload_id", []any{[]string{"ask-id"}}},
	} {
		t.Run(query.name, func(t *testing.T) {
			var planJSON []byte
			if err := tx.QueryRow(ctx, "explain (format json) "+query.sql, query.args...).Scan(&planJSON); err != nil {
				t.Fatalf("explain query: %v", err)
			}
			var plans []struct {
				Plan json.RawMessage `json:"Plan"`
			}
			if err := json.Unmarshal(planJSON, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("decode explain output (%v): %s", err, planJSON)
			}
			if explaintest.SeqScansRelation(t, plans[0].Plan, query.relation) {
				t.Fatalf("query plan sequentially scans %s; want %s:\n%s", query.relation, query.index, planJSON)
			}
			if !planUsesIndex(t, plans[0].Plan, query.index) {
				t.Fatalf("query plan does not read %s:\n%s", query.index, planJSON)
			}
		})
	}
}

// planUsesIndex reports whether a plan node, at any depth, reads index.
func planUsesIndex(t *testing.T, planJSON json.RawMessage, index string) bool {
	t.Helper()
	var node struct {
		IndexName string            `json:"Index Name"`
		Plans     []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	if node.IndexName == index {
		return true
	}
	for _, child := range node.Plans {
		if planUsesIndex(t, child, index) {
			return true
		}
	}
	return false
}
