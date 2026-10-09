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

// copiedSourceStates are the openers of a copy of copiedAsksDocument showing the states
// copiedAskSource leaves its sources in, each naming its source ask.
func copiedSourceStates(asks map[string]string) []string {
	return []string{
		`:::ask{#copy-answered urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-08T12:00:00Z" selected="[&#x22;Yes&#x22;]" copied_from="` + asks["copy-answered"] + `"}`,
		`:::ask{#copy-resolved urgency="med" multiple="false" state="resolved" copied_from="` + asks["copy-resolved"] + `"}`,
		`:::ask{#copy-open urgency="med" multiple="false" state="open" copied_from="` + asks["copy-open"] + `"}`,
	}
}

// A second document of the same issue written with the first's ask blocks (the spec seed's write,
// SeedText) opens no ask: each block shows the state and answer of the ask it was copied from and
// names it, and a later settlement of the copy shows a later answer to its source.
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
	requireCopiedStates(t, service, copied, copiedSourceStates(asks)...)

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
		`:::ask{#copy-open urgency="med" multiple="false" state="answered" answered_by="bob" answered_at="2026-10-09T09:00:00Z" selected="[]" answer="us-east-1" copied_from="`+asks["copy-open"]+`"}`)
	if count := asksOn(t, service, copied); count != 0 {
		t.Fatalf("the copy opened %d asks after its source was answered, want none", count)
	}
}

// openAskBlocksOn is the block ids of document's open asks, in order.
func openAskBlocksOn(t *testing.T, service *Service, document string) string {
	t.Helper()
	var blocks *string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select string_agg(block_id, ',' order by block_id) from asks where block_artifact_id = $1 and state = 'open'
	`, document).Scan(&blocks); err != nil {
		t.Fatalf("read the document's open asks: %v", err)
	}
	if blocks == nil {
		return ""
	}
	return *blocks
}

// A second document of an issue that uses the first's block id and question for a question of its
// own, with other options, is no copy: it asks something else, and opens its own ask.
func TestABlockAskingTheSameQuestionWithOtherOptionsUnderTheSameIDOpensItsOwnAsk(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, original, ":::ask{#decision urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Matches the platform.\n- gRPC: Streams.\n:::\n")
	settleCurrentGeneration(t, service, original)
	second := createIssueDocument(t, service.store, 1, "# Second")
	seedServiceText(t, service, second, ":::ask{#decision urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- NATS: Already deployed.\n- Kafka: Durable.\n:::\n")
	settleCurrentGeneration(t, service, second)
	if blocks := openAskBlocksOn(t, service, second); blocks != "decision" {
		t.Fatalf("the second document's open asks = %q, want its own decision", blocks)
	}
	text, err := service.Text(context.Background(), second)
	if err != nil {
		t.Fatalf("read the second document: %v", err)
	}
	if strings.Contains(text, "copied_from") {
		t.Fatalf("the second document names a source it was not copied from:\n%s", text)
	}
}

// A block cut from one document and pasted into another is no copy: settlement retracted the ask
// its first document indexed when the block left it, so the second opens an ask of its own.
func TestABlockMovedFromADocumentWhoseAskSettlementRetractedOpensItsOwnAsk(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	moved := ":::ask{#moved urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	seedServiceText(t, service, original, "Context\n\n"+moved)
	settleCurrentGeneration(t, service, original)
	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = tree.Children[:1]
		return tree
	})
	settleCurrentGeneration(t, service, original)
	var resolution string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select state || ' ' || coalesce(resolution->>'kind', '') from asks where block_artifact_id = $1 and block_id = 'moved'
	`, original).Scan(&resolution); err != nil {
		t.Fatalf("read the moved block's first ask: %v", err)
	}
	if resolution != "resolved retracted" {
		t.Fatalf("the first document's ask = %q, want it retracted by settlement", resolution)
	}
	pasted := createIssueDocument(t, service.store, 1, "# Pasted")
	seedServiceText(t, service, pasted, moved)
	settleCurrentGeneration(t, service, pasted)
	if blocks := openAskBlocksOn(t, service, pasted); blocks != "moved" {
		t.Fatalf("the pasted document's open asks = %q, want its own moved", blocks)
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
	if blocks := openAskBlocksOn(t, service, copied); blocks != "copy-open" || asksOn(t, service, copied) != 1 {
		t.Fatalf("the copy opened asks for %q, want only the reworded copy-open", blocks)
	}
}

// A project document copied from another of its project's documents opens no ask, while a
// document of another issue carrying the same block ids is no copy and opens its own.
func TestCopiedAskSourcesAreTheDocumentOwnersOwn(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	original := createProjectDocument(t, service.store, "# Original")
	asks := copiedAskSource(t, service, original)
	copied := createProjectDocument(t, service.store, "# Copy")
	seedServiceText(t, service, copied, copiedAsksDocument)
	settleCurrentGeneration(t, service, copied)
	if count := asksOn(t, service, copied); count != 0 {
		t.Fatalf("the project copy opened %d asks, want none", count)
	}
	requireCopiedStates(t, service, copied, copiedSourceStates(asks)...)

	elsewhere := createIssueDocument(t, service.store, 2, "# Elsewhere")
	seedServiceText(t, service, elsewhere, copiedAsksDocument)
	settleCurrentGeneration(t, service, elsewhere)
	if count := asksOn(t, service, elsewhere); count != 3 {
		t.Fatalf("another issue's document opened %d asks, want its own three", count)
	}
}

// openAsksUnder is the documents holding an open ask under blockID on issue DOC-1.
func openAsksUnder(t *testing.T, service *Service, blockID string) []string {
	t.Helper()
	rows, err := service.store.Pool.Query(context.Background(), `
		select block_artifact_id::text from asks
		where issue_key = 'DOC-1' and block_id = $1 and state = 'open' order by block_artifact_id
	`, blockID)
	if err != nil {
		t.Fatalf("read open asks under %s: %v", blockID, err)
	}
	defer rows.Close()
	var documents []string
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			t.Fatalf("scan open ask under %s: %v", blockID, err)
		}
		documents = append(documents, document)
	}
	return documents
}

// requireOneOpenAskOn waits for the settlement a retraction arms on document: exactly one open ask
// under blockID on the issue, on document.
func requireOneOpenAskOn(t *testing.T, service *Service, blockID, document string) {
	t.Helper()
	var open []string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if open = openAsksUnder(t, service, blockID); len(open) == 1 && open[0] == document {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("open asks under %s = %v, want one, on the copy %s", blockID, open, document)
}

// A block pasted into a second document and then cut from its first, the destination settling
// first, is a copy until the cut: settlement of the first retracts its ask and settles the copy,
// which opens the block's own ask. The question is open in exactly one place throughout.
func TestABlockPastedThenCutOpensItsAskOnTheDocumentItWasPastedInto(t *testing.T) {
	service, original := newTestService(t)
	moved := ":::ask{#moved urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	seedServiceText(t, service, original, "Context\n\n"+moved)
	settleCurrentGeneration(t, service, original)
	pasted := createIssueDocument(t, service.store, 1, "# Pasted")
	seedServiceText(t, service, pasted, moved)
	settleCurrentGeneration(t, service, pasted)
	if count := asksOn(t, service, pasted); count != 0 {
		t.Fatalf("the pasted block opened %d asks while its first document held it, want none", count)
	}
	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = tree.Children[:1]
		return tree
	})
	settleCurrentGeneration(t, service, original)
	requireOneOpenAskOn(t, service, "moved", pasted)
}

// A copy of a document whose open ask's block later leaves the source opens that block's own ask
// once settlement retracts the source's, while its answered and resolved blocks, whose sources are
// closed and not retracted, go on showing them and open none.
func TestACopyOpensItsOwnAskWhenItsOpenSourceLeavesItsDocument(t *testing.T) {
	service, original := newTestService(t)
	asks := copiedAskSource(t, service, original)
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, copiedAsksDocument)
	settleCurrentGeneration(t, service, copied)
	requireCopiedStates(t, service, copied, copiedSourceStates(asks)...)
	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = tree.Children[:3]
		return tree
	})
	settleCurrentGeneration(t, service, original)
	requireOneOpenAskOn(t, service, "copy-open", copied)
	if count := asksOn(t, service, copied); count != 1 {
		t.Fatalf("the copy holds %d asks, want only the open block's own", count)
	}
	requireCopiedStates(t, service, copied, copiedSourceStates(asks)[:2]...)
}

// Of two asks under one block id asking the same thing, a copy shows the earlier asked: here an
// answered one, though the later one, which a second document reworded into the same question,
// is open.
func TestACopyOfTwoMatchingAsksShowsTheEarlierAsked(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	block := func(question string) string {
		return ":::ask{#tie urgency=\"med\" multiple=\"false\"}\n" + question + "\n:::\n"
	}
	seedServiceText(t, service, original, block("Which region?"))
	settleCurrentGeneration(t, service, original)
	var earlier string
	if err := service.store.Pool.QueryRow(context.Background(), `select id::text from asks where block_artifact_id = $1`, original).Scan(&earlier); err != nil {
		t.Fatalf("read the earlier ask: %v", err)
	}
	answer, _ := json.Marshal(model.AskAnswer{User: "alice", Selected: []string{}, Text: new("eu-west-1"), At: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)})
	if _, err := service.store.Pool.Exec(context.Background(), `update asks set state = 'answered', answer = $2 where id = $1`, earlier, answer); err != nil {
		t.Fatalf("answer the earlier ask: %v", err)
	}
	later := createIssueDocument(t, service.store, 1, "# Later")
	seedServiceText(t, service, later, block("Which regions?"))
	settleCurrentGeneration(t, service, later)
	editLiveTree(t, service, later, replaceRun("Which regions?", "Which region?"))
	settleCurrentGeneration(t, service, later)
	if blocks := openAskBlocksOn(t, service, later); blocks != "tie" {
		t.Fatalf("the later document's open asks = %q, want its own tie, reworded", blocks)
	}
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, block("Which region?"))
	settleCurrentGeneration(t, service, copied)
	if count := asksOn(t, service, copied); count != 0 {
		t.Fatalf("the copy opened %d asks, want none", count)
	}
	requireCopiedStates(t, service, copied,
		`:::ask{#tie urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-08T12:00:00Z" selected="[]" answer="eu-west-1" copied_from="`+earlier+`"}`)
}

// A copy taken before its source was reworded asks what the source asked then, which the source's
// ask.edited event records: it is a copy of that ask, opens none, and shows its state.
func TestACopyOfASourcesEarlierWordingShowsItsStateAndOpensNoAsk(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	asks := copiedAskSource(t, service, original)
	editLiveTree(t, service, original, replaceRun("Which region?", "Which regions?"))
	settleCurrentGeneration(t, service, original)
	var question string
	if err := service.store.Pool.QueryRow(context.Background(), `select question from asks where id = $1`, asks["copy-open"]).Scan(&question); err != nil || question != "Which regions?" {
		t.Fatalf("the source's question = %q (%v), want it reworded", question, err)
	}
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, copiedAsksDocument)
	settleCurrentGeneration(t, service, copied)
	if count := asksOn(t, service, copied); count != 0 {
		t.Fatalf("the copy of the earlier wording opened %d asks, want none", count)
	}
	requireCopiedStates(t, service, copied, copiedSourceStates(asks)...)
}
