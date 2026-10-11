package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// readCountingDocs counts every read of a document's rendering a route can make.
type readCountingDocs struct {
	docs.API
	reads atomic.Int64
}

func (d *readCountingDocs) Text(ctx context.Context, artifactID string) (string, error) {
	d.reads.Add(1)
	return d.API.Text(ctx, artifactID)
}

func (d *readCountingDocs) TextWithToken(ctx context.Context, artifactID string) (string, string, error) {
	d.reads.Add(1)
	return d.API.TextWithToken(ctx, artifactID)
}

func (d *readCountingDocs) Blocks(ctx context.Context, artifactID string) ([]model.ArtifactBlock, error) {
	d.reads.Add(1)
	return d.API.Blocks(ctx, artifactID)
}

func (d *readCountingDocs) TextWithBlocks(ctx context.Context, artifactID string) (string, []model.ArtifactBlock, error) {
	d.reads.Add(1)
	return d.API.TextWithBlocks(ctx, artifactID)
}

func referenceExcerptHandler(t *testing.T) (http.Handler, *store.Store, *docs.Service, *readCountingDocs) {
	t.Helper()
	var service *docs.Service
	var counted *readCountingDocs
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		service = docs.New(docs.Deps{Store: database, ServerURL: "https://dispatch.example", Settle: time.Hour})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		counted = &readCountingDocs{API: service}
		return counted
	})
	return handler, database, service, counted
}

// renderedMentionExcerpt is the excerpt the references route cut before it stored one: the
// document the caller sees rendered with its blocks, and the first block, in their order, whose
// byte range holds the mention.
func renderedMentionExcerpt(t *testing.T, service *docs.Service, artifactID, reference string) model.GraphExcerpt {
	t.Helper()
	markdown, blocks, err := service.TextWithBlocks(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("render %s: %v", artifactID, err)
	}
	offset := strings.Index(markdown, reference)
	if offset < 0 {
		t.Fatalf("%s does not hold %s:\n%s", artifactID, reference, markdown)
	}
	for _, block := range blocks {
		if block.From <= offset && offset < block.To {
			return model.GraphExcerpt{BlockID: block.ID, Text: text.HeadRunes(markdown[block.From:block.To], refs.BlockExcerptRunes)}
		}
	}
	t.Fatalf("no block of %s holds %s at %d: %#v", artifactID, reference, offset, blocks)
	return model.GraphExcerpt{}
}

func documentMention(t *testing.T, handler http.Handler, reference, artifactID string) model.GraphEdge {
	t.Helper()
	for _, edge := range graphEdges(t, handler, url.Values{"to": {reference}, "kind": {"mentions"}}).Edges {
		if edge.Node.Kind == "artifact" && edge.Node.ID == artifactID {
			return edge
		}
	}
	t.Fatalf("no mention of %s from %s", reference, artifactID)
	return model.GraphEdge{}
}

// The references route answers a document's mention with the excerpt stored when the mention was
// indexed, so it reads no document to cut one.
func TestReferencesServeTheStoredExcerptWithoutReadingTheDocument(t *testing.T) {
	handler, _, service, counted := referenceExcerptHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	reference := "dispatch://" + target.Key
	design := createProjectDocument(t, handler, "OPS", "Design notes",
		"# Design\n\nIntroduction paragraph.\n\nThe plan lives in "+reference+" and nowhere else.\n")
	want := renderedMentionExcerpt(t, service, design.ID, reference)
	if want.BlockID == "" || want.Text != "The plan lives in "+reference+" and nowhere else." {
		t.Fatalf("rendered excerpt = %#v; want the paragraph holding the mention", want)
	}

	counted.reads.Store(0)
	mention := documentMention(t, handler, reference, design.ID)
	if reads := counted.reads.Load(); reads != 0 {
		t.Fatalf("the references route read a document %d times; want none, the excerpt is stored", reads)
	}
	if mention.Excerpt == nil || *mention.Excerpt != want {
		t.Fatalf("mention excerpt = %#v; want %#v", mention.Excerpt, want)
	}
}

// Each excerpt indexed when a version is written is the block the route cut from the rendered
// document: a heading, a list item and a paragraph, from a spec seeded at issue creation and
// again from the version an edit writes.
func TestAnIndexedExcerptIsTheBlockTheRouteServedForEachBlockKind(t *testing.T) {
	handler, _, service, counted := referenceExcerptHandler(t)
	for _, project := range []string{"CORE", "OPS", "ACME", "LEGION"} {
		createReferenceAPIProject(t, handler, project)
	}
	heading := "dispatch://" + createReferenceAPIIssue(t, handler, "CORE").Key
	item := "dispatch://" + createReferenceAPIIssue(t, handler, "OPS").Key
	paragraph := "dispatch://" + createReferenceAPIIssue(t, handler, "ACME").Key
	response := createIssueRequest(t, handler, map[string]any{
		"project": "LEGION", "title": "Excerpts",
		"spec": "# Plan for " + heading + "\n\n- Track " + item + " first\n- Then the rest\n\nWe close with " + paragraph + " today.\n",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue with spec: status=%d body=%s", response.Code, response.Body.String())
	}
	citing := decodeBody[model.Issue](t, response).PrimaryArtifactID
	assertExcerpts := func(stage string) {
		t.Helper()
		for _, reference := range []string{heading, item, paragraph} {
			want := renderedMentionExcerpt(t, service, citing, reference)
			counted.reads.Store(0)
			got := documentMention(t, handler, reference, citing).Excerpt
			if counted.reads.Load() != 0 {
				t.Fatalf("%s: the references route read a document for %s", stage, reference)
			}
			if got == nil || *got != want {
				t.Fatalf("%s: excerpt of %s = %#v; want %#v", stage, reference, got, want)
			}
		}
	}
	assertExcerpts("seeded spec")

	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+citing+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "replace", "find": "today", "with": "tomorrow"}},
		"summary": "Reword the close",
		"actor":   sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit the spec: status=%d body=%s", edited.Code, edited.Body.String())
	}
	assertExcerpts("edited version")
	if got := documentMention(t, handler, paragraph, citing).Excerpt; got == nil || !strings.Contains(got.Text, "tomorrow") {
		t.Fatalf("excerpt after the edit = %#v; want the edited paragraph", got)
	}
}

// A mention indexed before excerpts were stored (every row migration 0092 found) holds none, so
// the route answers it with the document's name until the reconciliation stores the excerpt the
// rendered document gives it; once stored, a tick finds nothing to do.
func TestTheReconciliationStoresTheExcerptOfAMentionIndexedWithout(t *testing.T) {
	handler, database, service, _ := referenceExcerptHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	reference := "dispatch://" + createReferenceAPIIssue(t, handler, "CORE").Key
	design := createProjectDocument(t, handler, "OPS", "Design notes", "# Design\n\n- See "+reference+" for the plan\n")
	want := renderedMentionExcerpt(t, service, design.ID, reference)
	if _, err := database.Pool.Exec(context.Background(),
		`update refs set excerpt_block_id = null, excerpt_text = null, excerpt_version = null where from_id = $1`, design.ID); err != nil {
		t.Fatalf("clear the stored excerpts: %v", err)
	}
	if got := documentMention(t, handler, reference, design.ID).Excerpt; got == nil || got.BlockID != "" || got.Text != "Design notes" {
		t.Fatalf("excerpt with none stored = %#v; want the document's name", got)
	}
	documents, full, err := service.ReconcileReferenceExcerpts(context.Background())
	if err != nil || documents < 1 || full {
		t.Fatalf("reconcile = %d documents, full %v, err %v; want the document reconciled", documents, full, err)
	}
	if got := documentMention(t, handler, reference, design.ID).Excerpt; got == nil || *got != want {
		t.Fatalf("excerpt after the reconciliation = %#v; want %#v", got, want)
	}
	if documents, _, err := service.ReconcileReferenceExcerpts(context.Background()); err != nil || documents != 0 {
		t.Fatalf("idle tick = %d documents, err %v; want none", documents, err)
	}
}

// A version written by the binary a deploy replaces stores no excerpt, so its mentions keep the
// previous version's block until MarkStaleExcerpts marks them owed. The references route must not
// serve that stale payload: it falls back to the document's name until the reconciliation records
// the edited document's block.
func TestAnOwedEditedExcerptFallsBackToTheDocumentNameUntilReconciled(t *testing.T) {
	handler, database, service, _ := referenceExcerptHandler(t)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	reference := "dispatch://" + createReferenceAPIIssue(t, handler, "CORE").Key
	design := createProjectDocument(t, handler, "OPS", "Design notes",
		"# Design\n\nThe old plan is "+reference+".\n")
	previous := documentMention(t, handler, reference, design.ID).Excerpt
	if previous == nil || !strings.Contains(previous.Text, "old plan") {
		t.Fatalf("excerpt before the edit = %#v; want the old plan", previous)
	}
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+design.ID+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "replace", "find": "old", "with": "new"}},
		"summary": "Refresh the plan",
		"actor":   sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit the document: status=%d body=%s", edited.Code, edited.Body.String())
	}
	want := renderedMentionExcerpt(t, service, design.ID, reference)
	if !strings.Contains(want.Text, "new plan") {
		t.Fatalf("rendered edited excerpt = %#v; want the new plan", want)
	}
	// The older binary wrote the edit's version without touching the excerpt columns, so the row
	// still holds the version before it.
	if _, err := database.Pool.Exec(ctx, `
		update refs set excerpt_block_id = $2, excerpt_text = $3, excerpt_version = excerpt_version - 1
		where from_kind = 'artifact' and from_id = $1
	`, design.ID, previous.BlockID, previous.Text); err != nil {
		t.Fatalf("leave the previous version's excerpt behind: %v", err)
	}
	if marked, err := refs.MarkStaleExcerpts(ctx, database.Pool); err != nil || marked != 1 {
		t.Fatalf("mark stale excerpts = %d, %v; want the edited document's mention", marked, err)
	}
	if got := documentMention(t, handler, reference, design.ID).Excerpt; got == nil || got.BlockID != "" || got.Text != "Design notes" {
		t.Fatalf("excerpt while owed = %#v; want the document's name", got)
	}
	documents, full, err := service.ReconcileReferenceExcerpts(ctx)
	if err != nil || documents < 1 || full {
		t.Fatalf("reconcile = %d documents, full %v, err %v; want the edited document reconciled", documents, full, err)
	}
	if got := documentMention(t, handler, reference, design.ID).Excerpt; got == nil || *got != want {
		t.Fatalf("excerpt after reconciliation = %#v; want %#v", got, want)
	}
}
