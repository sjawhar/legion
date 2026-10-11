package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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

// documentAddress is the address settlement names a copy's source document by (refs.ArtifactRef).
func documentAddress(t *testing.T, service *Service, document string) string {
	t.Helper()
	var issueKey *string
	var project, slug, kind string
	var primary bool
	if err := service.store.Pool.QueryRow(context.Background(), `
		select issue_key, coalesce(project_key, ''), slug, kind, is_primary from artifacts where id = $1
	`, document).Scan(&issueKey, &project, &slug, &kind, &primary); err != nil {
		t.Fatalf("read the source document's address: %v", err)
	}
	return refs.ArtifactRef(issueKey, project, nil, slug, kind, primary)
}

// copiedFrom is the attributes a copy carries naming source, the ask copied from, on document.
func copiedFrom(t *testing.T, service *Service, source, document string) string {
	t.Helper()
	return `copied_from="` + source + `" copied_from_document="` + documentAddress(t, service, document) + `"`
}

// copiedSourceStates are the openers of a copy of copiedAsksDocument showing the states
// copiedAskSource leaves its sources on original in, each naming its source ask and document.
func copiedSourceStates(t *testing.T, service *Service, original string, asks map[string]string) []string {
	t.Helper()
	return []string{
		`:::ask{#copy-answered urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-08T12:00:00Z" selected="[&#x22;Yes&#x22;]" ` + copiedFrom(t, service, asks["copy-answered"], original) + `}`,
		`:::ask{#copy-resolved urgency="med" multiple="false" state="resolved" ` + copiedFrom(t, service, asks["copy-resolved"], original) + `}`,
		`:::ask{#copy-open urgency="med" multiple="false" state="open" ` + copiedFrom(t, service, asks["copy-open"], original) + `}`,
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
	requireCopiedStates(t, service, copied, copiedSourceStates(t, service, original, asks)...)

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
		`:::ask{#copy-open urgency="med" multiple="false" state="answered" answered_by="bob" answered_at="2026-10-09T09:00:00Z" selected="[]" answer="us-east-1" `+copiedFrom(t, service, asks["copy-open"], original)+`}`)
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
	requireCopiedStates(t, service, copied, copiedSourceStates(t, service, original, asks)...)

	elsewhere := createIssueDocument(t, service.store, 2, "# Elsewhere")
	seedServiceText(t, service, elsewhere, copiedAsksDocument)
	settleCurrentGeneration(t, service, elsewhere)
	if count := asksOn(t, service, elsewhere); count != 3 {
		t.Fatalf("another issue's document opened %d asks, want its own three", count)
	}
}

// openAsksUnder is the documents holding an open ask under blockID, in the test's own database.
func openAsksUnder(t *testing.T, service *Service, blockID string) []string {
	t.Helper()
	rows, err := service.store.Pool.Query(context.Background(), `
		select block_artifact_id::text from asks
		where block_id = $1 and state = 'open' order by block_artifact_id
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
	requireCopiedStates(t, service, copied, copiedSourceStates(t, service, original, asks)...)
	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = tree.Children[:3]
		return tree
	})
	settleCurrentGeneration(t, service, original)
	requireOneOpenAskOn(t, service, "copy-open", copied)
	if count := asksOn(t, service, copied); count != 1 {
		t.Fatalf("the copy holds %d asks, want only the open block's own", count)
	}
	requireCopiedStates(t, service, copied, copiedSourceStates(t, service, original, asks)[:2]...)
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
		`:::ask{#tie urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-08T12:00:00Z" selected="[]" answer="eu-west-1" `+copiedFrom(t, service, earlier, original)+`}`)
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
	requireCopiedStates(t, service, copied, copiedSourceStates(t, service, original, asks)...)
}

// shipBlock is an ask block whose source tests reword: its question and its options both change.
const shipBlock = ":::ask{#ship urgency=\"med\" multiple=\"false\"}\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n"

// blockAskOn reads the ask of blockID on document.
func blockAskOn(t *testing.T, service *Service, document, blockID string) model.Ask {
	t.Helper()
	ask, err := ScanAsk(service.store.Pool.QueryRow(context.Background(),
		`select `+AskColumns+` from asks a where a.block_artifact_id = $1 and a.block_id = $2`, document, blockID))
	if err != nil {
		t.Fatalf("read the ask of %s: %v", blockID, err)
	}
	return ask
}

// rewordShip rewords the ship block on original to a question with other options, settles it, and
// returns its ask as reworded.
func rewordShip(t *testing.T, service *Service, original string) model.Ask {
	t.Helper()
	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		return replaceRun("Yes: Monday.", "Tuesday: Ship it.")(replaceRun("Ship on Monday?", "Ship on Tuesday?")(tree))
	})
	settleCurrentGeneration(t, service, original)
	source := blockAskOn(t, service, original, "ship")
	if source.Question != "Ship on Tuesday?" || len(source.Options) != 2 || source.Options[0].Label != "Tuesday" {
		t.Fatalf("the source asks %q with %v, want it reworded with new options", source.Question, source.Options)
	}
	return source
}

// A copy taken while its source was open asks the source's wording of then. Once the source is
// reworded with other options and then answered or resolved, that state is a decision on a question
// the copy does not ask, so the copy is no copy of it: its next settlement, which the answer or the
// resolution owes it, opens the block's own ask on the copy as for any new block, rather than
// writing a decision on the reworded question into the record.
func TestAnAnswerOrResolutionOfACopysSourceAfterARewordingOpensTheCopysOwnAsk(t *testing.T) {
	for _, closing := range []string{"answered", "resolved"} {
		t.Run(closing, func(t *testing.T) {
			service, original := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, original, "Context\n\n"+shipBlock)
			settleCurrentGeneration(t, service, original)
			source := blockAskOn(t, service, original, "ship")
			copied := createIssueDocument(t, service.store, 1, "# Copy")
			seedServiceText(t, service, copied, "Record\n\n"+shipBlock)
			settleCurrentGeneration(t, service, copied)
			requireCopiedStates(t, service, copied, `:::ask{#ship urgency="med" multiple="false" state="open" `+copiedFrom(t, service, source.ID, original)+`}`)

			source = rewordShip(t, service, original)
			var answer *model.AskAnswer
			if closing == "answered" {
				answer = &model.AskAnswer{User: "alice", Selected: []string{"Tuesday"}, At: time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)}
			}
			if err := closeSource(context.Background(), service, original, "ship", source, answer, nil); err != nil {
				t.Fatal(err)
			}
			if owed := copyOwed(t, service, copied); owed != 1 {
				t.Fatalf("the copy owes %d settlements after its source was %s, want one", owed, closing)
			}
			settleCurrentGeneration(t, service, copied)
			text, err := service.Text(context.Background(), copied)
			if err != nil {
				t.Fatalf("read the copy: %v", err)
			}
			if strings.Contains(text, `state="`+closing+`"`) || strings.Contains(text, "copied_from") {
				t.Fatalf("the copy of the earlier wording shows its source %s on the reworded question:\n%s", closing, text)
			}
			requireCopiedStates(t, service, copied, `:::ask{#ship urgency="med" multiple="false" state="open"}`+"\nShip on Monday?")
			if blocks := openAskBlocksOn(t, service, copied); blocks != "ship" {
				t.Fatalf("the copy's open asks = %q, want its own ship", blocks)
			}
		})
	}
}

// tuesdayBlock is shipBlock reworded, its question and options both changed.
const tuesdayBlock = ":::ask{#ship urgency=\"med\" multiple=\"false\"}\nShip on Tuesday?\n\n- Tuesday: Ship it.\n- No: Later.\n:::\n"

// mondayAnswered is the ship block on its source once its ask was answered Yes at mondayAnswer.
const mondayAnswered = `:::ask{#ship urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-09T09:00:00Z" selected="[&#x22;Yes&#x22;]"}` +
	"\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::"

var mondayAnswer = model.AskAnswer{User: "alice", Selected: []string{"Yes"}, At: time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)}

// decidedShip is how a test closes the ship block's ask, answered with mondayAnswer or resolved as
// closing names (closeSource's answer, nil for a resolution), and the block's opener once its
// source carries that decision on the Monday wording.
func decidedShip(closing string) (*model.AskAnswer, string) {
	if closing == "answered" {
		return &mondayAnswer, mondayAnswered
	}
	return nil, `:::ask{#ship urgency="med" multiple="false" state="resolved"}` + "\nShip on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::"
}

// askEditsOf counts the ask.edited events of ask.
func askEditsOf(t *testing.T, service *Service, ask string) int {
	t.Helper()
	var count int
	if err := service.store.Pool.QueryRow(context.Background(),
		`select count(*) from events where type = 'ask.edited' and payload->>'id' = $1`, ask).Scan(&count); err != nil {
		t.Fatalf("count the ask's edits: %v", err)
	}
	return count
}

// A decided ask's wording is fixed. A document edit or an upload that rewords an answered or
// resolved ask's block leaves its row as it was decided, appends no ask.edited, and settlement
// writes the decided wording back into the block beside its decision, logging that it did.
func TestADocumentEditOfADecidedAsksWordingIsWrittenBackAndLeavesItsRow(t *testing.T) {
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	writes := map[string]func(t *testing.T, service *Service, document string){
		"live edit": func(t *testing.T, service *Service, document string) {
			editLiveTree(t, service, document, func(tree *pmdoc.Node) *pmdoc.Node {
				return replaceRun("Yes: Monday.", "Tuesday: Ship it.")(replaceRun("Ship on Monday?", "Ship on Tuesday?")(tree))
			})
		},
		"upload": func(t *testing.T, service *Service, document string) {
			if _, err := joinedReplaceText(service, document, "Context\n\n"+tuesdayBlock, model.Actor{Kind: "session", ID: "uploader"}); err != nil {
				t.Fatalf("upload the reworded document: %v", err)
			}
		},
	}
	for _, closing := range []string{"answered", "resolved"} {
		for name, write := range writes {
			t.Run(closing+"/"+name, func(t *testing.T) {
				service, original := newTestService(t)
				service.settle = time.Hour
				seedServiceText(t, service, original, "Context\n\n"+shipBlock)
				settleCurrentGeneration(t, service, original)
				source := blockAskOn(t, service, original, "ship")
				answer, opener := decidedShip(closing)
				if err := closeSource(context.Background(), service, original, "ship", source, answer, nil); err != nil {
					t.Fatal(err)
				}
				write(t, service, original)
				settleCurrentGeneration(t, service, original)

				decided := blockAskOn(t, service, original, "ship")
				if decided.State != closing || decided.Question != "Ship on Monday?" || len(decided.Options) != 2 || decided.Options[0].Label != "Yes" {
					t.Fatalf("the %s ask reads %s %q %v after the document reworded it, want it as it was decided", closing, decided.State, decided.Question, decided.Options)
				}
				if edits := askEditsOf(t, service, source.ID); edits != 0 {
					t.Fatalf("the %s ask has %d ask.edited events, want none", closing, edits)
				}
				requireCopiedStates(t, service, original, opener)
				if line := logs.Lines(source.ID); !strings.Contains(line, "wrote a decided ask's wording back into its block") || !strings.Contains(line, original) {
					t.Fatalf("the repair logged %q, want its line naming the ask and the document", line)
				}
			})
		}
	}
}

// rewordAsAnEarlierRelease writes the Tuesday wording into ask's row, as a task of the release
// before LEGION-651 did when its settlement found the ship block reworded on document (that
// release's reconcileAskBlocks), whatever the ask's state: the row takes the block's question and
// options with edited_at, its question is indexed again, and an ask.edited records the wording it
// replaced, under the document's owner row.
func rewordAsAnEarlierRelease(t *testing.T, service *Service, document string, ask model.Ask) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the earlier release's rewording: %v", err)
	}
	defer tx.Rollback(ctx)
	owner, _, err := lockArtifactOwner(ctx, tx, document)
	if err != nil {
		t.Fatalf("lock the document's owner row: %v", err)
	}
	previous := model.NewAskEditPrevious(ask)
	ask.Question = "Ship on Tuesday?"
	ask.Options = []model.AskOption{{Label: "Tuesday", Description: "Ship it."}, {Label: "No", Description: "Later."}}
	options, _ := json.Marshal(ask.Options)
	var editedAt time.Time
	if err := tx.QueryRow(ctx, `update asks set question = $2, options = $3, edited_at = now() where id = $1 returning edited_at`,
		ask.ID, ask.Question, options).Scan(&editedAt); err != nil {
		t.Fatalf("reword the decided ask's row: %v", err)
	}
	ask.EditedAt = askTimestamp(&editedAt)
	changes, err := refs.ReplaceCounted(ctx, tx, "ask", ask.ID, ask.Question, service.serverURL)
	if err != nil {
		t.Fatalf("index the reworded question: %v", err)
	}
	if _, err := service.events.Append(ctx, tx, documentAskEvent(owner, document, "ask.edited", SettlementActor,
		model.NewAskEditEventPayload(ask, previous, SettlementActor, changes))); err != nil {
		t.Fatalf("append the rewording's ask.edited: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the earlier release's rewording: %v", err)
	}
}

// A task of the release before LEGION-651, still running during a rolling deploy after this
// release's boot restored every decided ask, takes a document's rewording of an answered or
// resolved ask into its row: the row asks the new wording beside the decision made on the old. The
// document's next settlement on this release gives the row back the wording the decision was given
// on, writes it back into the block, and logs the ask, its document and the restored question;
// the boot's restoration then finds nothing left to restore.
func TestASettlementRestoresADecidedAskAnEarlierReleaseReworded(t *testing.T) {
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, closing := range []string{"answered", "resolved"} {
		t.Run(closing, func(t *testing.T) {
			service, original := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, original, "Context\n\n"+shipBlock)
			settleCurrentGeneration(t, service, original)
			answer, opener := decidedShip(closing)
			if err := closeSource(context.Background(), service, original, "ship", blockAskOn(t, service, original, "ship"), answer, nil); err != nil {
				t.Fatal(err)
			}
			source := blockAskOn(t, service, original, "ship")
			editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
				return replaceRun("Yes: Monday.", "Tuesday: Ship it.")(replaceRun("Ship on Monday?", "Ship on Tuesday?")(tree))
			})
			rewordAsAnEarlierRelease(t, service, original, source)

			settleCurrentGeneration(t, service, original)
			decided := blockAskOn(t, service, original, "ship")
			if decided.State != closing || decided.Question != "Ship on Monday?" || len(decided.Options) != 2 || decided.Options[0].Label != "Yes" {
				t.Fatalf("the %s ask reads %s %q %v after its document's settlement, want the wording it was decided on", closing, decided.State, decided.Question, decided.Options)
			}
			requireCopiedStates(t, service, original, opener)
			line := logs.Lines(source.ID)
			if !strings.Contains(line, "restored a decided ask's wording") || !strings.Contains(line, original) || !strings.Contains(line, `question="Ship on Monday?"`) {
				t.Fatalf("the settlement logged %q, want a line naming the ask, its document and the restored question", line)
			}
			if strings.Contains(line, "Tuesday") {
				t.Fatalf("the settlement logged the wording it replaced: %q", line)
			}
			restored, copies, err := service.RestoreDecidedWordings(context.Background())
			if err != nil || restored != 0 || copies != 0 {
				t.Fatalf("the boot's restoration after the settlement restored %d asks and marked %d copies owed (%v), want none", restored, copies, err)
			}
		})
	}
}

// A copy that settles while an earlier release's rewording of its decided source is still in the
// source's row matches the reworded question, and shows the decision beside it. Restoring the
// source's decided wording, at its document's settlement or at boot, marks the copy owed, and the
// copy's next settlement opens the block's own ask, the reworded question being one no decision
// answered. A second restoration finds nothing left, and the copy owes nothing more.
func TestARestorationOfACopysSourceSettlesTheCopyAgain(t *testing.T) {
	ctx := context.Background()
	for _, closing := range []string{"answered", "resolved"} {
		for _, restoration := range []string{"settlement", "boot"} {
			t.Run(closing+"/"+restoration, func(t *testing.T) {
				service, original := newTestService(t)
				service.settle = time.Hour
				seedServiceText(t, service, original, "Context\n\n"+shipBlock)
				settleCurrentGeneration(t, service, original)
				answer, _ := decidedShip(closing)
				if err := closeSource(ctx, service, original, "ship", blockAskOn(t, service, original, "ship"), answer, nil); err != nil {
					t.Fatal(err)
				}
				source := blockAskOn(t, service, original, "ship")
				editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
					return replaceRun("Yes: Monday.", "Tuesday: Ship it.")(replaceRun("Ship on Monday?", "Ship on Tuesday?")(tree))
				})
				rewordAsAnEarlierRelease(t, service, original, source)

				copied := createIssueDocument(t, service.store, 1, "# Copy")
				seedServiceText(t, service, copied, "Record\n\n"+tuesdayBlock)
				settleCurrentGeneration(t, service, copied)
				requireCopiedStates(t, service, copied, `:::ask{#ship urgency="med" multiple="false" state="`+closing+`"`)

				switch restoration {
				case "settlement":
					settleCurrentGeneration(t, service, original)
				case "boot":
					if restored, copies, err := service.RestoreDecidedWordings(ctx); err != nil || restored != 1 || copies != 1 {
						t.Fatalf("the boot's restoration restored %d asks and marked %d copies owed (%v), want the source and its copy", restored, copies, err)
					}
				}
				if owed := copyOwed(t, service, copied); owed != 1 {
					t.Fatalf("the copy owes %d settlements once its source's wording was restored at %s, want one", owed, restoration)
				}
				settleCurrentGeneration(t, service, copied)
				text, err := service.Text(ctx, copied)
				if err != nil {
					t.Fatalf("read the copy: %v", err)
				}
				if strings.Contains(text, `state="`+closing+`"`) || strings.Contains(text, "copied_from") {
					t.Fatalf("the copy of the reworded question still shows its source %s once the source was restored:\n%s", closing, text)
				}
				requireCopiedStates(t, service, copied, `:::ask{#ship urgency="med" multiple="false" state="open"}`+"\nShip on Tuesday?")
				if blocks := openAskBlocksOn(t, service, copied); blocks != "ship" {
					t.Fatalf("the copy's open asks = %q, want its own ship", blocks)
				}

				if restored, copies, err := service.RestoreDecidedWordings(ctx); err != nil || restored != 0 || copies != 0 {
					t.Fatalf("a second restoration restored %d asks and marked %d copies owed (%v), want none", restored, copies, err)
				}
				if owed := copyOwed(t, service, copied); owed != 0 {
					t.Fatalf("the copy owes %d settlements after a second restoration, want none", owed)
				}
				if again, err := service.Text(ctx, copied); err != nil || again != text {
					t.Fatalf("the copy changed after a second restoration (%v):\n%s", err, again)
				}
			})
		}
	}
}

// The boot's restoration of a decided ask's wording marks the ask's own document owed, as it marks
// the ask's copies: the document's block still asks the rewording an earlier release took into
// the row, beside the decision, and nothing else settles the document until someone edits it. The
// one settlement the boot arms writes the decided wording back into the block, and a second
// restoration finds nothing to restore and marks nothing.
func TestTheBootsRestorationSettlesTheRestoredAsksOwnDocument(t *testing.T) {
	ctx := context.Background()
	for _, closing := range []string{"answered", "resolved"} {
		t.Run(closing, func(t *testing.T) {
			service, original := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, original, "Context\n\n"+shipBlock)
			settleCurrentGeneration(t, service, original)
			answer, opener := decidedShip(closing)
			if err := closeSource(ctx, service, original, "ship", blockAskOn(t, service, original, "ship"), answer, nil); err != nil {
				t.Fatal(err)
			}
			source := blockAskOn(t, service, original, "ship")
			editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
				return replaceRun("Yes: Monday.", "Tuesday: Ship it.")(replaceRun("Ship on Monday?", "Ship on Tuesday?")(tree))
			})
			rewordAsAnEarlierRelease(t, service, original, source)
			// The earlier release's settlement that took the rewording into the row committed, so
			// the document owed nothing when this release booted: once the browser edit's own
			// appends, which mark the row, have landed, the row goes.
			waitForLandedAppends(t, service, original)
			if _, err := service.store.Pool.Exec(ctx, `delete from doc_settlements_pending where artifact_id = $1`, original); err != nil {
				t.Fatalf("settle the document as the earlier release did: %v", err)
			}

			if restored, copies, err := service.RestoreDecidedWordings(ctx); err != nil || restored != 1 || copies != 0 {
				t.Fatalf("the boot's restoration restored %d asks and marked %d copies owed (%v), want the source and no copy", restored, copies, err)
			}
			if owed := copyOwed(t, service, original); owed != 1 {
				t.Fatalf("the restored ask's own document owes %d settlements after the boot, want one", owed)
			}
			settleCurrentGeneration(t, service, original)
			requireCopiedStates(t, service, original, opener)
			text, err := service.Text(ctx, original)
			if err != nil {
				t.Fatalf("read the document: %v", err)
			}
			if strings.Contains(text, "Tuesday") {
				t.Fatalf("the document's block still asks the rewording after the settlement the boot armed:\n%s", text)
			}
			if owed := copyOwed(t, service, original); owed != 0 {
				t.Fatalf("the document owes %d settlements after it settled, want none", owed)
			}
			if restored, copies, err := service.RestoreDecidedWordings(ctx); err != nil || restored != 0 || copies != 0 {
				t.Fatalf("a second restoration restored %d asks and marked %d copies owed (%v), want none", restored, copies, err)
			}
			if owed := copyOwed(t, service, original); owed != 0 {
				t.Fatalf("the document owes %d settlements after a second restoration, want none", owed)
			}
		})
	}
}

// A block taken for a copy opens no ask and reaches nobody's Inbox, so settlement logs each block
// it writes a copy's attributes into, naming the copy, its source ask, the source's document and
// the state it shows, and never the question. A later settlement that leaves the copy's blocks as
// they are logs nothing more, and one that shows a change of a source's state logs that block
// again.
func TestASettlementLogsEachBlockItTakesForACopyOnce(t *testing.T) {
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	service, original := newTestService(t)
	service.settle = time.Hour
	asks := copiedAskSource(t, service, original)
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, copiedAsksDocument)
	settleCurrentGeneration(t, service, copied)
	classified := func() []string {
		var lines []string
		for line := range strings.Lines(logs.Lines("settled an ask block as a copy")) {
			if strings.Contains(line, "artifact="+copied) {
				lines = append(lines, line)
			}
		}
		return lines
	}
	lines := classified()
	if len(lines) != 3 {
		t.Fatalf("the copy's first settlement logged %d copy lines, want one per copied block:\n%s", len(lines), strings.Join(lines, ""))
	}
	source := documentAddress(t, service, original)
	for block, state := range map[string]string{"copy-answered": "answered", "copy-resolved": "resolved", "copy-open": "open"} {
		found := false
		for _, line := range lines {
			if strings.Contains(line, "block="+block+" ") {
				found = strings.Contains(line, "source_ask="+asks[block]) && strings.Contains(line, source) &&
					strings.Contains(line, "source_state="+state)
			}
		}
		if !found {
			t.Fatalf("no copy line names %s, its source %s on %s and the state %s:\n%s", block, asks[block], source, state, strings.Join(lines, ""))
		}
	}
	for _, question := range []string{"Ship on Monday", "Write the changelog", "Which region"} {
		if strings.Contains(strings.Join(lines, ""), question) {
			t.Fatalf("a copy line logged the question %q:\n%s", question, strings.Join(lines, ""))
		}
	}

	editLiveTree(t, service, copied, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Children[0].Text = "Context, revisited"
		return tree
	})
	settleCurrentGeneration(t, service, copied)
	if again := classified(); len(again) != 3 {
		t.Fatalf("a settlement that left the copy's blocks as they were logged %d copy lines more, want none:\n%s", len(again)-3, strings.Join(again, ""))
	}

	region := "us-east-1"
	answer, _ := json.Marshal(model.AskAnswer{User: "bob", Selected: []string{}, Text: &region, At: time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)})
	if _, err := service.store.Pool.Exec(context.Background(), `update asks set state = 'answered', answer = $2 where id = $1`, asks["copy-open"], answer); err != nil {
		t.Fatalf("answer the open source ask: %v", err)
	}
	editLiveTree(t, service, copied, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Children[0].Text = "Context, again"
		return tree
	})
	settleCurrentGeneration(t, service, copied)
	after := classified()
	if len(after) != 4 || !strings.Contains(after[3], "block=copy-open ") || !strings.Contains(after[3], "source_state=answered") {
		t.Fatalf("the settlement after the open source was answered logged %d copy lines more, want one naming copy-open answered:\n%s", len(after)-3, strings.Join(after, ""))
	}
}

// A copy of a decided ask's source is a copy only of what was decided. Once its source's own
// document rewords the answered ask, a copy of the reworded text holds a question no decision
// answered and opens its own ask, and a copy of the wording that was answered shows the answer.
func TestACopyOfADecidedAsksEditedTextOpensItsOwnAskAndACopyOfItsDecidedWordingShowsTheDecision(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, original, "Context\n\n"+shipBlock)
	settleCurrentGeneration(t, service, original)
	source := blockAskOn(t, service, original, "ship")
	if err := closeSource(context.Background(), service, original, "ship", source, &mondayAnswer, nil); err != nil {
		t.Fatal(err)
	}
	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		return replaceRun("Yes: Monday.", "Tuesday: Ship it.")(replaceRun("Ship on Monday?", "Ship on Tuesday?")(tree))
	})
	settleCurrentGeneration(t, service, original)

	reworded := createIssueDocument(t, service.store, 1, "# Reworded copy")
	seedServiceText(t, service, reworded, "Record\n\n"+tuesdayBlock)
	settleCurrentGeneration(t, service, reworded)
	text, err := service.Text(context.Background(), reworded)
	if err != nil {
		t.Fatalf("read the copy of the reworded text: %v", err)
	}
	if strings.Contains(text, `state="answered"`) || strings.Contains(text, "copied_from") {
		t.Fatalf("the copy of the reworded text shows the decision on the question it replaced:\n%s", text)
	}
	requireCopiedStates(t, service, reworded, `:::ask{#ship urgency="med" multiple="false" state="open"}`+"\nShip on Tuesday?")
	if blocks := openAskBlocksOn(t, service, reworded); blocks != "ship" {
		t.Fatalf("the copy of the reworded text's open asks = %q, want its own ship", blocks)
	}

	decided := createIssueDocument(t, service.store, 1, "# Decided copy")
	seedServiceText(t, service, decided, "Record\n\n"+shipBlock)
	settleCurrentGeneration(t, service, decided)
	if count := asksOn(t, service, decided); count != 0 {
		t.Fatalf("the copy of the decided wording opened %d asks, want none", count)
	}
	requireCopiedStates(t, service, decided,
		`:::ask{#ship urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-09T09:00:00Z" selected="[&#x22;Yes&#x22;]" `+copiedFrom(t, service, source.ID, original)+`}`)
}

// A copy taken while its source was open stays a copy of it across the source's retraction and
// restoration, which reopen the wording it asked, and stops being one once the restored source
// is reworded and the new wording answered: the copy then opens its own ask.
func TestACopyOfARestoredSourceRewordedAndAnsweredOpensItsOwnAsk(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, original, "Context\n\n"+shipBlock)
	settleCurrentGeneration(t, service, original)
	source := blockAskOn(t, service, original, "ship")
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, "Record\n\n"+shipBlock)
	settleCurrentGeneration(t, service, copied)
	shown := `:::ask{#ship urgency="med" multiple="false" state="open" ` + copiedFrom(t, service, source.ID, original) + `}`
	requireCopiedStates(t, service, copied, shown)

	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = tree.Children[:1]
		return tree
	})
	settleCurrentGeneration(t, service, original)
	if retracted := blockAskOn(t, service, original, "ship"); !settlementRetracted(retracted) {
		t.Fatalf("the source reads %s after its block left, want settlement's retraction", retracted.State)
	}
	editLiveTree(t, service, original, appendBlocks(t, shipBlock))
	settleCurrentGeneration(t, service, original)
	if restored := blockAskOn(t, service, original, "ship"); restored.State != "open" {
		t.Fatalf("the source reads %s after its block came back, want it restored open", restored.State)
	}
	settleCurrentGeneration(t, service, copied)
	if count := asksOn(t, service, copied); count != 0 {
		t.Fatalf("the copy opened %d asks beside its restored source, want none", count)
	}
	requireCopiedStates(t, service, copied, shown)

	source = rewordShip(t, service, original)
	answer := model.AskAnswer{User: "alice", Selected: []string{"Tuesday"}, At: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)}
	if err := closeSource(context.Background(), service, original, "ship", source, &answer, nil); err != nil {
		t.Fatal(err)
	}
	settleCurrentGeneration(t, service, copied)
	text, err := service.Text(context.Background(), copied)
	if err != nil {
		t.Fatalf("read the copy: %v", err)
	}
	if strings.Contains(text, `state="answered"`) || strings.Contains(text, "copied_from") {
		t.Fatalf("the copy of the restored source's earlier wording shows the answer to its new one:\n%s", text)
	}
	if blocks := openAskBlocksOn(t, service, copied); blocks != "ship" {
		t.Fatalf("the copy's open asks = %q, want its own ship", blocks)
	}
}

// pendingSettlements counts the documents that owe a settlement no settlement has committed.
func pendingSettlements(t *testing.T, service *Service) int {
	t.Helper()
	var count int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from doc_settlements_pending`).Scan(&count); err != nil {
		t.Fatalf("count pending settlements: %v", err)
	}
	return count
}

// Three copies of one open ask whose block then leaves its document share one ask: retracting the
// source settles every copy, the first to settle opens the block's own ask, and the other two are
// copies of that ask and name it, never the retracted source, so the question waits in one place in
// one person's Inbox. Which copy settles first is not fixed, so the test names none.
func TestThreeCopiesOfARetractedAskShareTheOneAskTheFirstOpens(t *testing.T) {
	service, original := newTestService(t)
	shared := ":::ask{#shared urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	seedServiceText(t, service, original, "Context\n\n"+shared)
	settleCurrentGeneration(t, service, original)
	var source string
	if err := service.store.Pool.QueryRow(context.Background(), `select id::text from asks where block_artifact_id = $1`, original).Scan(&source); err != nil {
		t.Fatalf("read the source ask: %v", err)
	}
	copies := make([]string, 3)
	for index := range copies {
		copies[index] = createIssueDocument(t, service.store, 1, "# Copy")
		seedServiceText(t, service, copies[index], shared)
		settleCurrentGeneration(t, service, copies[index])
		requireCopiedStates(t, service, copies[index], `copied_from="`+source+`"`)
	}
	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = tree.Children[:1]
		return tree
	})
	settleCurrentGeneration(t, service, original)

	var open []string
	var shown map[string]string
	pending := -1
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		open = openAsksUnder(t, service, "shared")
		shown = map[string]string{}
		for _, copy := range copies {
			text, err := service.Text(context.Background(), copy)
			if err != nil {
				t.Fatalf("read a copy: %v", err)
			}
			shown[copy] = text
		}
		pending = pendingSettlements(t, service)
		if len(open) == 1 && pending == 0 && sharesOneAsk(t, service, copies, open[0], shown) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("open asks under shared = %v, pending settlements = %d, want one open ask on a copy that the other two name, none pending; copies:\n%v", open, pending, shown)
}

// sharesOneAsk reports whether opener, a copy holding the block's one open ask, is one of copies
// and every other copy names that ask in copied_from and never the retracted source.
func sharesOneAsk(t *testing.T, service *Service, copies []string, opener string, shown map[string]string) bool {
	t.Helper()
	if !slices.Contains(copies, opener) {
		return false
	}
	var ask string
	if err := service.store.Pool.QueryRow(context.Background(), `select id::text from asks where block_artifact_id = $1 and block_id = 'shared'`, opener).Scan(&ask); err != nil {
		t.Fatalf("read the copy's own ask: %v", err)
	}
	for _, copy := range copies {
		if copy == opener {
			if strings.Contains(shown[copy], "copied_from") {
				return false
			}
			continue
		}
		if !strings.Contains(shown[copy], `copied_from="`+ask+`"`) || asksOn(t, service, copy) != 0 {
			return false
		}
	}
	return true
}

// copyLockWait is the LIKE pattern of a statement waiting on a project's copy lock.
const copyLockWait = "%" + projectCopiesLock + "%"

// settlementBarrier is a settlement hook (afterSettleLock, afterSettleReconcile) that holds each
// settlement calling it until all n have called it or wait on their project's copy lock
// (lockProjectCopies). It runs on settlement's goroutine, so a failed read is reported with
// t.Errorf and lets the settlement go on.
func settlementBarrier(t *testing.T, service *Service, n int) func(string) {
	t.Helper()
	var mu sync.Mutex
	arrived := 0
	return func(string) {
		mu.Lock()
		arrived++
		mu.Unlock()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			waiting, err := storetest.CountLockWaits(context.Background(), service.store, copyLockWait)
			if err != nil {
				t.Errorf("count the settlements waiting on the copy lock: %v", err)
				return
			}
			mu.Lock()
			reached := arrived
			mu.Unlock()
			if reached+waiting >= n {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Errorf("settlements never all reached the barrier or the copy lock")
	}
}

// roomGeneration is document's room generation once its live updates are durable, the generation a
// settlement of it runs at (settleCurrentGeneration).
func roomGeneration(t *testing.T, service *Service, document string) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, document); err != nil {
		t.Fatalf("wait for live updates to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, document); err != nil {
		t.Fatalf("wait for live updates to become durable: %v", err)
	}
	state := service.room(document)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.roomGeneration
}

// Three copies of one open ask on a project's documents, settling at once after settlement retracts
// the ask, open one ask between them. A project document's owner row is the document itself, so no
// row orders its siblings' settlements; the project's copy lock does. Each settlement is held past
// its room lock until all three hold theirs, and past its reconciliation until the others have
// reconciled too or wait on the copy lock: without the lock all three would read their copy sources
// before any committed, and each would open an ask.
func TestThreeProjectCopiesOfARetractedAskSettlingAtOnceShareOneAsk(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	shared := ":::ask{#shared urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	original := createProjectDocument(t, service.store, "# Original")
	seedServiceText(t, service, original, "Context\n\n"+shared)
	settleCurrentGeneration(t, service, original)
	var source string
	if err := service.store.Pool.QueryRow(context.Background(), `select id::text from asks where block_artifact_id = $1`, original).Scan(&source); err != nil {
		t.Fatalf("read the source ask: %v", err)
	}
	copies := make([]string, 3)
	for index := range copies {
		copies[index] = createProjectDocument(t, service.store, "# Copy")
		seedServiceText(t, service, copies[index], shared)
		settleCurrentGeneration(t, service, copies[index])
		requireCopiedStates(t, service, copies[index], `copied_from="`+source+`"`)
	}
	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = tree.Children[:1]
		return tree
	})
	settleCurrentGeneration(t, service, original)
	if pending := pendingSettlements(t, service); pending != 3 {
		t.Fatalf("pending settlements after the retraction = %d, want the three copies'", pending)
	}

	generations := make([]uint64, len(copies))
	for index, copy := range copies {
		generations[index] = roomGeneration(t, service, copy)
	}
	service.afterSettleLock = settlementBarrier(t, service, len(copies))
	service.afterSettleReconcile = settlementBarrier(t, service, len(copies))
	var settling sync.WaitGroup
	for index, copy := range copies {
		settling.Go(func() { service.settleRoom(copy, generations[index]) })
	}
	settling.Wait()
	service.afterSettleLock, service.afterSettleReconcile = nil, nil

	open := openAsksUnder(t, service, "shared")
	shown := map[string]string{}
	for _, copy := range copies {
		text, err := service.Text(context.Background(), copy)
		if err != nil {
			t.Fatalf("read a copy: %v", err)
		}
		shown[copy] = text
	}
	pending := pendingSettlements(t, service)
	if len(open) != 1 || pending != 0 || !sharesOneAsk(t, service, copies, open[0], shown) {
		t.Fatalf("open asks under shared = %v, pending settlements = %d, want one open ask on a copy that the other two name, none pending; copies:\n%v", open, pending, shown)
	}
}

// A copy settling while its source is answered: the answer runs in the answer route's order (the
// source's owner row and ask row, then SettleCopiesOf, which marks the copy owed and holds its
// pending row, then the source's block, then the ask.answered event), and the copy's settlement,
// whose block the copy no longer holds, takes no copy lock and so runs beside it. The settlement
// deletes its own pending row before it appends an event, so it waits for the answer holding no
// lock the answer needs, and both finish. Deleting the row after its events would hold the events'
// commit-order lock, which the answer's event waits for, while waiting on the row the answer holds.
func TestACopySettlingWhileItsSourceIsAnsweredWaitsForTheAnswerAndNeitherFails(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	shared := ":::ask{#shared urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	original := createProjectDocument(t, service.store, "# Original")
	seedServiceText(t, service, original, "Context\n\n"+shared)
	settleCurrentGeneration(t, service, original)
	source := blockAskOn(t, service, original, "shared")
	copied := createProjectDocument(t, service.store, "# Copy")
	seedServiceText(t, service, copied, "Kept\n\n"+shared)
	settleCurrentGeneration(t, service, copied)
	requireCopiedStates(t, service, copied, `copied_from="`+source.ID+`"`)
	// The copy's live text drops the block while its latest version still holds it, so the answer
	// marks the copy owed and the copy's settlement has no copy to read the source of.
	editLiveTree(t, service, copied, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = tree.Children[:1]
		return tree
	})
	generation := roomGeneration(t, service, copied)

	settled := make(chan error, 1)
	if err := answerSource(ctx, service, original, source, func() {
		go func() {
			service.settleRoom(copied, generation)
			settled <- nil
		}()
		waitForLockWait(t, ctx, service.store, "%delete from doc_settlements_pending%", settled)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settled:
	case <-ctx.Done():
		t.Fatal("the copy's settlement never finished")
	}
	state := service.room(copied)
	state.mu.Lock()
	failures := state.settleFailures
	state.mu.Unlock()
	if failures != 0 {
		t.Fatalf("the copy's settlement failed %d time(s) beside the answer, want none", failures)
	}
	var text string
	if err := service.store.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, copied).Scan(&text); err != nil {
		t.Fatalf("read the copy's latest version: %v", err)
	}
	if strings.Contains(text, "#shared") {
		t.Fatalf("the copy's settlement did not version its text without the block:\n%s", text)
	}
}

// answerSource answers source, the ask of the block shared on the project document original, as
// closeSource does, with eu-west-1. between runs right after SettleCopiesOf.
func answerSource(ctx context.Context, service *Service, original string, source model.Ask, between func()) error {
	region := "eu-west-1"
	answer := model.AskAnswer{User: "alice", Selected: []string{}, Text: &region, At: time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)}
	return closeSource(ctx, service, original, "shared", source, &answer, between)
}

// closeSource answers source, the ask of blockID on original, with answer, or resolves it when
// answer is nil, in its route's order (closeAskTx, the answer's WriteBlock, the resolve's Apply):
// the source's owner row and ask row, then SettleCopiesOf, which takes the project's copy lock and
// marks the copies owed, then the source's block, the ask row and the ask.answered or ask.resolved
// event, and commits. between, when set, runs right after SettleCopiesOf, while the transaction
// holds the copy lock and the copies' pending rows. It returns its error rather than failing the
// test, so it can run off the test's goroutine.
func closeSource(ctx context.Context, service *Service, original, blockID string, source model.Ask, answer *model.AskAnswer, between func()) error {
	alice := model.Actor{Kind: "user", ID: "alice"}
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin closing the source: %w", err)
	}
	defer tx.Rollback(context.Background())
	closing, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	owner, _, err := lockArtifactOwner(ctx, tx, original)
	if err != nil {
		return fmt.Errorf("lock the source's owner row: %w", err)
	}
	if _, err := tx.Exec(ctx, `select 1 from asks where id = $1 for no key update`, source.ID); err != nil {
		return fmt.Errorf("lock the source ask: %w", err)
	}
	if err := service.SettleCopiesOf(closing, source); err != nil {
		return fmt.Errorf("mark the copies owed: %w", err)
	}
	if between != nil {
		between()
	}
	eventType, attributes := "ask.resolved", map[string]any{"state": "resolved"}
	if answer != nil {
		var text any
		if answer.Text != nil {
			text = *answer.Text
		}
		eventType, attributes = "ask.answered", map[string]any{
			"state": "answered", "answered_by": answer.User, "answered_at": answer.At.Format(time.RFC3339Nano),
			"selected": answer.Selected, "answer": text,
		}
	}
	if err := service.SetBlockAttributes(closing, original, blockID, attributes, alice); err != nil {
		return fmt.Errorf("write the source's block: %w", err)
	}
	if answer != nil {
		encoded, _ := json.Marshal(answer)
		if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, source.ID, encoded); err != nil {
			return fmt.Errorf("answer the source ask: %w", err)
		}
		source.State, source.Answer = "answered", answer
	} else if source, err = WriteAskResolution(ctx, tx, source, "resolved", "Waived.", alice); err != nil {
		return fmt.Errorf("resolve the source ask: %w", err)
	}
	if _, err := service.events.Append(ctx, tx, documentAskEvent(
		owner, original, eventType, alice, model.NewAskEventPayload(source, model.ReferenceChanges{}),
	)); err != nil {
		return fmt.Errorf("append the source's %s event: %w", eventType, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		return fmt.Errorf("commit closing the source: %w", err)
	}
	return nil
}

// A copy settling while its source is answered, with the copy's block still in it, so both take the
// project's copy lock (the copy's settlement to read its source, the answer to mark the copy owed),
// in either order. Whichever takes it first finishes holding nothing the other holds while it waits
// there: the answer holds the source's owner and ask rows, which settlement only reads, and the
// settlement holds the copy's owner row, which the answer's mark of the copy's pending row reaches
// only through a foreign key's `for key share`. Both finish, the copy shows the answer once the
// settlement after the answer has run, and nothing is owed after that.
func TestACopyAndItsSourcesAnswerTakingTheCopyLockInEitherOrderBothFinish(t *testing.T) {
	for _, order := range []string{"answer first", "settlement first"} {
		t.Run(order, func(t *testing.T) {
			service, _ := newTestService(t)
			service.settle = time.Hour
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			shared := ":::ask{#shared urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
			original := createProjectDocument(t, service.store, "# Original")
			seedServiceText(t, service, original, "Context\n\n"+shared)
			settleCurrentGeneration(t, service, original)
			source := blockAskOn(t, service, original, "shared")
			copied := createProjectDocument(t, service.store, "# Copy")
			seedServiceText(t, service, copied, "Kept\n\n"+shared)
			settleCurrentGeneration(t, service, copied)
			requireCopiedStates(t, service, copied, `state="open" `+copiedFrom(t, service, source.ID, original))
			// The copy owes a settlement and still holds the block, so that settlement reads its source
			// under the copy lock.
			editLiveTree(t, service, copied, replaceRun("Kept", "Kept again"))
			generation := roomGeneration(t, service, copied)

			settled := make(chan error, 1)
			settle := func() {
				service.settleRoom(copied, generation)
				settled <- nil
			}
			switch order {
			case "answer first":
				// The answer holds the copy lock and the copy's pending row while the copy's settlement
				// waits on the lock, then appends its event and commits.
				if err := answerSource(ctx, service, original, source, func() {
					go settle()
					waitForLockWait(t, ctx, service.store, copyLockWait, settled)
				}); err != nil {
					t.Fatal(err)
				}
				awaitDone(t, ctx, "the copy's settlement", settled)
			case "settlement first":
				// The copy's settlement holds the copy lock, reconciled, while the answer waits on it.
				reconciled, proceed := make(chan struct{}), make(chan struct{})
				service.afterSettleReconcile = func(room string) {
					if room == copied {
						close(reconciled)
						<-proceed
					}
				}
				go settle()
				select {
				case <-reconciled:
				case <-ctx.Done():
					t.Fatal("the copy's settlement never reconciled")
				}
				answered := make(chan error, 1)
				go func() { answered <- answerSource(ctx, service, original, source, nil) }()
				waitForLockWait(t, ctx, service.store, copyLockWait, answered)
				close(proceed)
				awaitDone(t, ctx, "the copy's settlement", settled)
				service.afterSettleReconcile = nil
				awaitDone(t, ctx, "the answer", answered)
				// The settlement read the source before the answer, so the copy shows it open and owes
				// the settlement the answer marked.
				requireCopiedStates(t, service, copied, `state="open" `+copiedFrom(t, service, source.ID, original))
				if owed := copyOwed(t, service, copied); owed != 1 {
					t.Fatalf("the copy owes %d settlements after the answer, want the one it marked", owed)
				}
				settleCurrentGeneration(t, service, copied)
			}
			state := service.room(copied)
			state.mu.Lock()
			failures := state.settleFailures
			state.mu.Unlock()
			if failures != 0 {
				t.Fatalf("the copy's settlement failed %d time(s) beside the answer, want none", failures)
			}
			var markdown string
			if err := service.store.Pool.QueryRow(ctx, `
				select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
			`, copied).Scan(&markdown); err != nil {
				t.Fatalf("read the copy's latest version: %v", err)
			}
			if !strings.Contains(markdown, `state="answered" answered_by="alice"`) || !strings.Contains(markdown, `copied_from="`+source.ID+`"`) {
				t.Fatalf("the copy's latest version does not show its source's answer:\n%s", markdown)
			}
			if count, owed := asksOn(t, service, copied), copyOwed(t, service, copied); count != 0 || owed != 0 {
				t.Fatalf("the copy holds %d asks and owes %d settlements, want none of either", count, owed)
			}
		})
	}
}

// awaitDone waits for what, which sends its result on done, failing the test on an error or when
// ctx ends first.
func awaitDone(t *testing.T, ctx context.Context, what string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-ctx.Done():
		t.Fatalf("%s never finished", what)
	}
}

// copyOwed is how many pending settlements document owes: 0 or 1.
func copyOwed(t *testing.T, service *Service, document string) int {
	t.Helper()
	var owed int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from doc_settlements_pending where artifact_id = $1
	`, document).Scan(&owed); err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	return owed
}

// The earliest asked match is the source whichever wording it matched on: ask A asks a question
// first and is reworded twice, ask B is asked later and reworded into A's first wording, and a copy
// asking that wording is a copy of A, though only B asks it now.
func TestACopyNamesTheEarliestAskedMatchThoughALaterAskAsksItsWordingNow(t *testing.T) {
	service, original := newTestService(t)
	service.settle = time.Hour
	block := func(question string) string {
		return ":::ask{#tie urgency=\"med\" multiple=\"false\"}\n" + question + "\n:::\n"
	}
	seedServiceText(t, service, original, block("Question X?"))
	settleCurrentGeneration(t, service, original)
	editLiveTree(t, service, original, replaceRun("Question X?", "Question Y?"))
	settleCurrentGeneration(t, service, original)
	editLiveTree(t, service, original, replaceRun("Question Y?", "Question W?"))
	settleCurrentGeneration(t, service, original)
	var earlier string
	if err := service.store.Pool.QueryRow(context.Background(), `select id::text from asks where block_artifact_id = $1`, original).Scan(&earlier); err != nil {
		t.Fatalf("read the earlier ask: %v", err)
	}
	later := createIssueDocument(t, service.store, 1, "# Later")
	seedServiceText(t, service, later, block("Question Z?"))
	settleCurrentGeneration(t, service, later)
	editLiveTree(t, service, later, replaceRun("Question Z?", "Question X?"))
	settleCurrentGeneration(t, service, later)
	var question string
	if err := service.store.Pool.QueryRow(context.Background(), `select question from asks where block_artifact_id = $1 and state = 'open'`, later).Scan(&question); err != nil || question != "Question X?" {
		t.Fatalf("the later document's own ask asks %q (%v), want A's first wording", question, err)
	}
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, block("Question X?"))
	settleCurrentGeneration(t, service, copied)
	if count := asksOn(t, service, copied); count != 0 {
		t.Fatalf("the copy opened %d asks, want none", count)
	}
	requireCopiedStates(t, service, copied, `:::ask{#tie urgency="med" multiple="false" state="open" `+copiedFrom(t, service, earlier, original)+`}`)
}
