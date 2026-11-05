package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func createConversionIssue(t *testing.T, handler http.Handler, title string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": title, "spec": "The issue spec.\n", "force": true}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("conversion fixture: %d %s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

func TestDecisionConversionKeepsIssueAtomicAndRetainsClosedConversations(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createConversionIssue(t, handler, "Atomic conversion")
	first := uploadIssueDocument(t, handler, issue.Key, "decisions.md", "## First choice\n\nFirst reason.\n")
	second := uploadIssueDocument(t, handler, issue.Key, "other.md", "## Second choice\n\nSecond reason.\n")
	if _, err := database.Pool.Exec(t.Context(), `update artifacts set name='decisions.md' where id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	blocked, quote := first, "First reason."
	if second.ID > first.ID {
		blocked, quote = second, "Second reason."
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Still discussing this choice.", "anchor": map[string]any{"artifact": blocked.ID, "quote": quote}}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("conversation: %d %s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	before, err := deps.Docs.Text(t.Context(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	versions := specVersionCount(t, database, issue.PrimaryArtifactID)
	var out bytes.Buffer
	report, err := deps.Docs.(*docs.Service).ConvertDecisionDocuments(t.Context(), true, &out)
	if err == nil || report.Documents != 2 || !strings.Contains(out.String(), comment.ID) {
		t.Fatalf("blocked conversion: %#v %v\n%s", report, err, out.String())
	}
	after, err := deps.Docs.Text(t.Context(), issue.PrimaryArtifactID)
	if err != nil || after != before || specVersionCount(t, database, issue.PrimaryArtifactID) != versions {
		t.Fatalf("failed issue transaction changed spec: %q -> %q (%v)", before, after, err)
	}
	var legacyCount int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from artifacts where issue_key=$1 and name='decisions.md'`, issue.Key).Scan(&legacyCount); err != nil || legacyCount != 2 {
		t.Fatalf("failed issue transaction retired sources: count=%d err=%v", legacyCount, err)
	}
	resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", map[string]any{}, "alice")
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", resolved.Code, resolved.Body.String())
	}
	report, err = deps.Docs.(*docs.Service).ConvertDecisionDocuments(t.Context(), true, &out)
	if err != nil || report.Documents != 2 || report.Moved != 2 {
		t.Fatalf("retry after conversation: %#v %v\n%s", report, err, out.String())
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("retained conversation: %d %s", read.Code, read.Body.String())
	}
	kept := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, read).Comment
	if kept.Body != comment.Body || !kept.Resolved || kept.Anchor == nil || kept.Anchor.ArtifactID != blocked.ID {
		t.Fatalf("closed conversation lost its history identity: %#v", kept)
	}
}

func TestDecisionConversionReportsEveryIssueEvenWhenOneIsBlocked(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	blocked := createConversionIssue(t, handler, "Blocked conversion")
	good := createConversionIssue(t, handler, "Independent conversion")
	first := uploadIssueDocument(t, handler, blocked.Key, "decisions.md", "## First\n\nBlocked reason.\n")
	second := uploadIssueDocument(t, handler, good.Key, "decisions.md", "## Second\n\nIndependent reason.\n")
	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+blocked.Key+"/comments", map[string]any{"body": "Still open.", "anchor": map[string]any{"artifact": first.ID, "quote": "Blocked reason."}}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("conversation: %d %s", comment.Code, comment.Body.String())
	}
	var before int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from artifact_versions`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report, err := deps.Docs.(*docs.Service).ConvertDecisionDocuments(context.Background(), false, &out)
	if err == nil || report.Documents != 2 || !strings.Contains(out.String(), first.ID) || !strings.Contains(out.String(), second.ID) {
		t.Fatalf("incomplete dry-run report: %#v %v\n%s", report, err, out.String())
	}
	var after int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from artifact_versions`).Scan(&after); err != nil || after != before {
		t.Fatalf("dry run wrote versions: %d -> %d (%v)", before, after, err)
	}
}

func TestDecisionConversionPreservesDistinctKeepsWithIdenticalHeadings(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createConversionIssue(t, handler, "Historical receipts")
	legacy := uploadIssueDocument(t, handler, issue.Key, "decisions.md", "## Same title\n\nFirst reason.\n\n## Same title\n\nSecond reason.\n")
	markdown, blocks, err := deps.Docs.TextWithBlocks(t.Context(), legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].From < blocks[j].From })
	headings := []string{}
	for _, block := range blocks {
		if block.Type == "heading" && strings.TrimSpace(markdown[block.From:block.To]) == "## Same title" {
			headings = append(headings, block.ID)
		}
	}
	if len(headings) != 2 {
		t.Fatalf("source headings: %v", headings)
	}
	for i, reason := range []string{"First reason.", "Second reason."} {
		sum := sha256.Sum256([]byte("## Same title\n\n" + reason))
		user := []string{"alice", "bob"}[i]
		if _, err := database.Pool.Exec(t.Context(), `insert into decision_keeps(artifact_id,block_id,text_sha256,changed_version,kept_by) values($1,$2,$3,1,$4)`, legacy.ID, headings[i], hex.EncodeToString(sum[:]), user); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Pool.Exec(t.Context(), `insert into artifact_reviews(artifact_id,version,state,actor) values($1,1,'approved','{"kind":"user","id":"alice"}'::jsonb)`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report, err := deps.Docs.(*docs.Service).ConvertDecisionDocuments(t.Context(), true, &out)
	if err != nil || report.Moved != 2 {
		t.Fatalf("receipt conversion: %#v %v\n%s", report, err, out.String())
	}
	view := readIssueDecisions(t, handler, issue.Key)
	if len(view.Decisions) != 2 {
		t.Fatalf("converted choices: %s", mustJSON(t, view))
	}
	for i, decision := range view.Decisions {
		user := []string{"alice", "bob"}[i]
		if decision.Kept == nil || decision.Kept.KeptBy != user || decision.WaitingReview {
			t.Fatalf("receipt %d reassigned or lost: %#v", i, decision)
		}
	}
	var reviews int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from artifact_reviews where artifact_id=$1 and version=1 and state='approved'`, legacy.ID).Scan(&reviews); err != nil || reviews != 1 {
		t.Fatalf("source review history lost: %d %v", reviews, err)
	}
}
