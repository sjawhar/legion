package docs

import (
	"context"
	"encoding/json"
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
	source, err := ScanAsk(service.store.Pool.QueryRow(ctx, `select `+AskColumns+` from asks a where a.block_artifact_id = $1`, original))
	if err != nil {
		t.Fatalf("read the source ask: %v", err)
	}
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

	alice := model.Actor{Kind: "user", ID: "alice"}
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the answer: %v", err)
	}
	defer tx.Rollback(context.Background())
	answering, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := tx.Exec(ctx, `select 1 from artifacts where id = $1 for no key update`, original); err != nil {
		t.Fatalf("lock the source's owner row: %v", err)
	}
	if _, err := tx.Exec(ctx, `select 1 from asks where id = $1 for no key update`, source.ID); err != nil {
		t.Fatalf("lock the source ask: %v", err)
	}
	if err := service.SettleCopiesOf(answering, source); err != nil {
		t.Fatalf("mark the copies owed: %v", err)
	}

	settled := make(chan error, 1)
	go func() {
		service.settleRoom(copied, generation)
		settled <- nil
	}()
	waitForLockWait(t, ctx, service.store, "%delete from doc_settlements_pending%", settled)

	region := "eu-west-1"
	answer := model.AskAnswer{User: alice.ID, Selected: []string{}, Text: &region, At: time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)}
	if err := service.SetBlockAttributes(answering, original, "shared", map[string]any{
		"state": "answered", "answered_by": alice.ID, "answered_at": answer.At.Format(time.RFC3339Nano),
		"selected": answer.Selected, "answer": region,
	}, alice); err != nil {
		t.Fatalf("write the answer into the source's block: %v", err)
	}
	encoded, _ := json.Marshal(answer)
	if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, source.ID, encoded); err != nil {
		t.Fatalf("answer the source ask: %v", err)
	}
	source.State, source.Answer = "answered", &answer
	if _, err := service.events.Append(ctx, tx, model.Event{
		ArtifactID: &original, Type: "ask.answered", Actor: alice, Payload: model.NewAskEventPayload(source, model.ReferenceChanges{}),
	}); err != nil {
		t.Fatalf("append the answer's event: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the answer: %v", err)
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
