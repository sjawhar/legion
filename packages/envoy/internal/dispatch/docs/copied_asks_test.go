package docs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

const copiedAsksDocument = "Context\n\n" +
	":::ask{#copy-answered urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n\n" +
	":::ask{#copy-resolved urgency=\"med\" multiple=\"false\"}\nWrite the changelog?\n:::\n\n" +
	":::ask{#copy-open urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"

// copiedAskSource settles document's three ask blocks, answers the first and resolves the second
// as their routes store them, and returns the block ids' asks.
func copiedAskSource(t *testing.T, service *Service, document string) map[string]string {
	t.Helper()
	seedServiceText(t, service, document, copiedAsksDocument)
	settleCurrentGeneration(t, service, document)
	ctx := context.Background()
	asks := map[string]string{}
	rows, err := service.store.Pool.Query(ctx, `select block_id, id::text from asks where block_artifact_id = $1`, document)
	if err != nil {
		t.Fatalf("read source asks: %v", err)
	}
	for rows.Next() {
		var block, id string
		if err := rows.Scan(&block, &id); err != nil {
			t.Fatalf("scan source ask: %v", err)
		}
		asks[block] = id
	}
	rows.Close()
	if len(asks) != 3 {
		t.Fatalf("source asks = %v, want three", asks)
	}
	answer, _ := json.Marshal(model.AskAnswer{User: "alice", Selected: []string{"Yes"}, At: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)})
	if _, err := service.store.Pool.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, asks["copy-answered"], answer); err != nil {
		t.Fatalf("answer source ask: %v", err)
	}
	resolution, _ := json.Marshal(model.AskResolution{Kind: "resolved", Reason: "Waived.", Actor: model.Actor{Kind: "user", ID: "alice"}, At: time.Now().UTC()})
	if _, err := service.store.Pool.Exec(ctx, `update asks set state = 'resolved', resolution = $2 where id = $1`, asks["copy-resolved"], resolution); err != nil {
		t.Fatalf("resolve source ask: %v", err)
	}
	return asks
}

func asksOn(t *testing.T, service *Service, document string) int {
	t.Helper()
	var count int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from asks where block_artifact_id = $1`, document).Scan(&count); err != nil {
		t.Fatalf("count asks: %v", err)
	}
	return count
}

func requireCopiedStates(t *testing.T, service *Service, document string, want ...string) {
	t.Helper()
	text, err := service.Text(context.Background(), document)
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	for _, opener := range want {
		if !strings.Contains(text, opener) {
			t.Fatalf("copy text lacks %q:\n%s", opener, text)
		}
	}
}

var copiedSourceStates = []string{
	`:::ask{#copy-answered urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-08T12:00:00Z" selected="[&#x22;Yes&#x22;]"}`,
	`:::ask{#copy-resolved urgency="med" multiple="false" state="resolved"}`,
	`:::ask{#copy-open urgency="med" multiple="false" state="open"}`,
}

// A second document of the same issue written with the first's ask blocks (the spec seed's write,
// SeedText) opens no ask: each block shows the state and answer of the ask it was copied from, and
// a later settlement of the copy shows a later answer to its source.
func TestADocumentCopiedOnItsIssueShowsItsSourcesAsksAndOpensNone(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	asks := copiedAskSource(t, service, original)
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, copiedAsksDocument)
	settleCurrentGeneration(t, service, copied)
	if count := asksOn(t, service, copied); count != 0 {
		t.Fatalf("the copy opened %d asks, want none", count)
	}
	requireCopiedStates(t, service, copied, copiedSourceStates...)

	region := "us-east-1"
	answer, _ := json.Marshal(model.AskAnswer{User: "bob", Selected: []string{}, Text: &region, At: time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)})
	if _, err := service.store.Pool.Exec(context.Background(), `update asks set state = 'answered', answer = $2 where id = $1`, asks["copy-open"], answer); err != nil {
		t.Fatalf("answer the open source ask: %v", err)
	}
	editLiveTree(t, service, copied, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Children[0].Text = "Context, revisited"
		return tree
	})
	settleCurrentGeneration(t, service, copied)
	requireCopiedStates(t, service, copied,
		`:::ask{#copy-open urgency="med" multiple="false" state="answered" answered_by="bob" answered_at="2026-10-09T09:00:00Z" selected="[]" answer="us-east-1"}`)
	if count := asksOn(t, service, copied); count != 0 {
		t.Fatalf("the copy opened %d asks after its source was answered, want none", count)
	}
}

// A copied block whose question the copy rewords is a new question, and opens an ask.
func TestACopiedAskBlockRewordedOnTheCopyOpensAnAsk(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	copiedAskSource(t, service, original)
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, strings.Replace(copiedAsksDocument, "Which region?", "Which regions?", 1))
	settleCurrentGeneration(t, service, copied)
	var block string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select string_agg(block_id, ',') from asks where block_artifact_id = $1 and state = 'open'
	`, copied).Scan(&block); err != nil {
		t.Fatalf("read the copy's asks: %v", err)
	}
	if block != "copy-open" || asksOn(t, service, copied) != 1 {
		t.Fatalf("the copy opened asks for %q, want only the reworded copy-open", block)
	}
}

// A project document copied from another of its project's documents opens no ask, while a
// document of another issue carrying the same block ids is no copy and opens its own.
func TestCopiedAskSourcesAreTheDocumentOwnersOwn(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	original := createProjectDocument(t, service.store, "# Original")
	copiedAskSource(t, service, original)
	copied := createProjectDocument(t, service.store, "# Copy")
	seedServiceText(t, service, copied, copiedAsksDocument)
	settleCurrentGeneration(t, service, copied)
	if count := asksOn(t, service, copied); count != 0 {
		t.Fatalf("the project copy opened %d asks, want none", count)
	}
	requireCopiedStates(t, service, copied, copiedSourceStates...)

	elsewhere := createIssueDocument(t, service.store, 2, "# Elsewhere")
	seedServiceText(t, service, elsewhere, copiedAsksDocument)
	settleCurrentGeneration(t, service, elsewhere)
	if count := asksOn(t, service, elsewhere); count != 3 {
		t.Fatalf("another issue's document opened %d asks, want its own three", count)
	}
}
