package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const askPartServer = "https://dispatch.example"

// askPartFixture opens a database, begins the transaction every assertion of a test reads, rolled
// back when the test ends, and creates the issue its asks hang on.
func askPartFixture(t *testing.T) pgx.Tx {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	return tx
}

// insertAsk inserts an ask of issueKey and returns it as its writer holds it.
func insertAsk(t *testing.T, tx pgx.Tx, issueKey, question string, options ...model.AskOption) model.Ask {
	t.Helper()
	ask := model.Ask{
		IssueKey: &issueKey,
		Author:   model.Actor{Kind: "user", ID: "alice"},
		Question: question,
		Options:  options,
		State:    "open",
	}
	encoded, err := json.Marshal(ask.Options)
	if err != nil {
		t.Fatalf("encode the ask's options: %v", err)
	}
	if err := tx.QueryRow(context.Background(), `
		insert into asks (issue_key, author, question, options)
		values ($1, '{"kind":"user","id":"alice"}', $2, $3) returning id::text
	`, issueKey, question, encoded).Scan(&ask.ID); err != nil {
		t.Fatalf("insert the ask: %v", err)
	}
	return ask
}

// writeAskOptions writes the ask's options as an edit of its row does.
func writeAskOptions(t *testing.T, tx pgx.Tx, ask *model.Ask, options ...model.AskOption) {
	t.Helper()
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatalf("encode the ask's options: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `update asks set options = $2 where id = $1::uuid`, ask.ID, encoded); err != nil {
		t.Fatalf("write the ask's options: %v", err)
	}
	ask.Options = options
}

// writeAskAnswer answers the ask as an answer write does.
func writeAskAnswer(t *testing.T, tx pgx.Tx, ask *model.Ask, user string, selected []string, body string) {
	t.Helper()
	answer := model.AskAnswer{User: user, Selected: selected, Text: &body, At: time.Now().UTC()}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode the ask's answer: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		update asks set answer = $2, state = 'answered' where id = $1::uuid
	`, ask.ID, encoded); err != nil {
		t.Fatalf("write the ask's answer: %v", err)
	}
	ask.Answer, ask.State = &answer, "answered"
}

// indexAskPartsAt indexes the ask's parts at the index-th second of a fixed clock, so each indexing
// of one test has a time of its own.
func indexAskPartsAt(t *testing.T, tx pgx.Tx, ask model.Ask, index int) []Target {
	t.Helper()
	at := time.Date(2026, 10, 30, 12, 0, index, 0, time.UTC)
	moved, err := IndexAskParts(context.Background(), tx, ask, askPartServer, at)
	if err != nil {
		t.Fatalf("index the parts of ask %s: %v", ask.ID, err)
	}
	return moved
}

// askPartRow is one row of ask_blocks: a removed part's position is -1 and a part the ask's
// author made has no maker.
type askPartRow struct {
	blockID, nodeID, partType, excerpt, madeBy string
	position, revision                         int
}

// summary is the part as "<block id> <type> <position> r<revision> <excerpt>", with its maker
// when it records one.
func (row askPartRow) summary() string {
	summary := fmt.Sprintf("%s %s %d r%d %s", row.blockID, row.partType, row.position, row.revision, row.excerpt)
	if row.madeBy != "" {
		summary += " by " + row.madeBy
	}
	return summary
}

// readAskParts reads every part row of the ask, current ones in order, then removed ones.
func readAskParts(t *testing.T, q Queryer, askID string) []askPartRow {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select block_id, node_id, type, excerpt, coalesce(position, -1), changed_revision,
		       coalesce(made_by->>'kind' || ':' || (made_by->>'id'), '')
		from ask_blocks where ask_id = $1::uuid
		order by position nulls last, length(block_id), block_id
	`, askID)
	if err != nil {
		t.Fatalf("read the parts of ask %s: %v", askID, err)
	}
	parts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (askPartRow, error) {
		var part askPartRow
		err := row.Scan(&part.blockID, &part.nodeID, &part.partType, &part.excerpt, &part.position, &part.revision, &part.madeBy)
		return part, err
	})
	if err != nil {
		t.Fatalf("scan the parts of ask %s: %v", askID, err)
	}
	return parts
}

// askPartSummaries is every part row of the ask as summary writes it.
func askPartSummaries(t *testing.T, q Queryer, askID string) []string {
	t.Helper()
	parts := readAskParts(t, q, askID)
	summaries := make([]string, len(parts))
	for i, part := range parts {
		summaries[i] = part.summary()
	}
	return summaries
}

// askPartTexts is every ask_block_texts row of the ask as "<block id> r<revision> <text>", a
// removed part's text written as "-".
func askPartTexts(t *testing.T, q Queryer, askID string) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select block_id || ' r' || revision || ' ' || coalesce(text, '-')
		from ask_block_texts where ask_id = $1::uuid
		order by revision, length(block_id), block_id
	`, askID)
	if err != nil {
		t.Fatalf("read the part texts of ask %s: %v", askID, err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the part texts of ask %s: %v", askID, err)
	}
	return texts
}

// askPartState is the ask's indexed revision and the number of the next part id it mints.
func askPartState(t *testing.T, q Queryer, askID string) (revision, next int) {
	t.Helper()
	if err := q.QueryRow(context.Background(), `
		select revision, next_block from ask_block_state where ask_id = $1::uuid and owed_at is null
	`, askID).Scan(&revision, &next); err != nil {
		t.Fatalf("read the part state of ask %s: %v", askID, err)
	}
	return revision, next
}

// An ask's parts are its question, then each option in its row order, then its answer. An option
// reads "<label> — <description>", or its label alone, and the answer reads as the selected
// labels, ", " between them, then " - " and the answer's text, with either half alone when the
// other is empty. Each part's identity hash is salted with its type, so an option whose text is
// the question's is another part.
func TestAskPartsAreTheQuestionEachOptionAndTheAnswer(t *testing.T) {
	ask := model.Ask{
		ID:       "11111111-1111-4111-8111-111111111111",
		Question: "Which transport?",
		Options: []model.AskOption{
			{Label: "REST", Description: "the simple one"},
			{Label: "gRPC"},
		},
	}
	parts := AskParts(ask)
	got := make([]string, len(parts))
	for i, part := range parts {
		got[i] = part.Type + ": " + part.Text
	}
	want := []string{"question: Which transport?", "option: REST — the simple one", "option: gRPC"}
	if !slices.Equal(got, want) {
		t.Fatalf("AskParts of an unanswered ask = %v, want %v", got, want)
	}
	for _, part := range parts {
		if part.Hash != itemblocks.Hash(part.Type+"\n"+part.Text) {
			t.Errorf("the hash of the %s part is not salted with its type", part.Type)
		}
		if part.From != 0 || part.To != 0 {
			t.Errorf("the %s part spans [%d, %d); an ask has no body to take offsets into", part.Type, part.From, part.To)
		}
	}

	text := "Ship it."
	ask.Answer = &model.AskAnswer{User: "bob", Selected: []string{"REST", "gRPC"}, Text: &text}
	if answered := AskParts(ask); len(answered) != 4 ||
		answered[3].Type != "answer" || answered[3].Text != "REST, gRPC - Ship it." {
		t.Fatalf("the answer part of AskParts = %+v, want the selected labels and the text", answered[len(answered)-1:])
	}
	only := "Ship it."
	ask.Answer = &model.AskAnswer{User: "bob", Text: &only}
	if answered := AskParts(ask); answered[3].Text != "Ship it." {
		t.Errorf("the part of an answer that selected nothing = %q, want its text alone", answered[3].Text)
	}
	ask.Answer = &model.AskAnswer{User: "bob", Selected: []string{"REST"}}
	if answered := AskParts(ask); answered[3].Text != "REST" {
		t.Errorf("the part of an answer with no text = %q, want its label alone", answered[3].Text)
	}
}

// The first indexing of an ask writes one part row per part at revision 1, each with the node id
// `ask/<ask uuid>#<part id>`, its type and its text, and records each text at that revision.
func TestIndexingAnAskWritesItsPartsAtRevisionOne(t *testing.T) {
	tx := askPartFixture(t)
	ask := insertAsk(t, tx, "CORE-1", "Which transport?",
		model.AskOption{Label: "REST", Description: "the simple one"},
		model.AskOption{Label: "gRPC"})
	indexAskPartsAt(t, tx, ask, 1)

	want := []string{
		"b1 question 0 r1 Which transport?",
		"b2 option 1 r1 REST — the simple one",
		"b3 option 2 r1 gRPC",
	}
	if got := askPartSummaries(t, tx, ask.ID); !slices.Equal(got, want) {
		t.Fatalf("the parts of the ask = %v, want %v", got, want)
	}
	for _, part := range readAskParts(t, tx, ask.ID) {
		if node := "ask/" + ask.ID + "#" + part.blockID; part.nodeID != node {
			t.Errorf("the node id of part %s = %q, want %q", part.blockID, part.nodeID, node)
		}
	}
	if revision, next := askPartState(t, tx, ask.ID); revision != 1 || next != 4 {
		t.Errorf("the ask's state = revision %d, next_block %d; want 1 and 4", revision, next)
	}
	wantTexts := []string{"b1 r1 Which transport?", "b2 r1 REST — the simple one", "b3 r1 gRPC"}
	if got := askPartTexts(t, tx, ask.ID); !slices.Equal(got, wantTexts) {
		t.Errorf("the part texts of the ask = %v, want %v", got, wantTexts)
	}
}

// Answering an ask adds a fourth part, whose maker is the answerer and not the ask's author, at
// the next revision; the question and the options keep their ids and their revision.
func TestAnsweringAnAskAddsAPartMadeByTheAnswerer(t *testing.T) {
	tx := askPartFixture(t)
	ask := insertAsk(t, tx, "CORE-1", "Which transport?",
		model.AskOption{Label: "REST", Description: "the simple one"},
		model.AskOption{Label: "gRPC"})
	indexAskPartsAt(t, tx, ask, 1)
	writeAskAnswer(t, tx, &ask, "bob", []string{"gRPC"}, "Ship it.")
	indexAskPartsAt(t, tx, ask, 2)

	want := []string{
		"b1 question 0 r1 Which transport?",
		"b2 option 1 r1 REST — the simple one",
		"b3 option 2 r1 gRPC",
		"b4 answer 3 r2 gRPC - Ship it. by user:bob",
	}
	if got := askPartSummaries(t, tx, ask.ID); !slices.Equal(got, want) {
		t.Fatalf("the parts of the answered ask = %v, want %v", got, want)
	}
	if revision, next := askPartState(t, tx, ask.ID); revision != 2 || next != 5 {
		t.Errorf("the answered ask's state = revision %d, next_block %d; want 2 and 5", revision, next)
	}
	if got := askPartTexts(t, tx, ask.ID); !slices.Contains(got, "b4 r2 gRPC - Ship it.") {
		t.Errorf("the part texts of the answered ask = %v, want the answer's text at revision 2", got)
	}
}

// A reworded option keeps its part id and takes the new revision; no other part moves.
func TestRewordingAnAskOptionKeepsItsPartID(t *testing.T) {
	tx := askPartFixture(t)
	ask := insertAsk(t, tx, "CORE-1", "Which transport?",
		model.AskOption{Label: "REST", Description: "the simple one"},
		model.AskOption{Label: "gRPC"})
	indexAskPartsAt(t, tx, ask, 1)
	writeAskOptions(t, tx, &ask,
		model.AskOption{Label: "REST", Description: "the one every client speaks"},
		model.AskOption{Label: "gRPC"})
	indexAskPartsAt(t, tx, ask, 2)

	want := []string{
		"b1 question 0 r1 Which transport?",
		"b2 option 1 r2 REST — the one every client speaks",
		"b3 option 2 r1 gRPC",
	}
	if got := askPartSummaries(t, tx, ask.ID); !slices.Equal(got, want) {
		t.Fatalf("the parts of the reworded ask = %v, want %v", got, want)
	}
	if revision, next := askPartState(t, tx, ask.ID); revision != 2 || next != 4 {
		t.Errorf("the reworded ask's state = revision %d, next_block %d; want 2 and 4", revision, next)
	}
}

// An option the ask no longer holds keeps its row with no position and the last excerpt it held,
// so a reference made while it was live still resolves and reads removed, and the revision that
// dropped it records that it holds no text.
func TestDroppingAnAskOptionKeepsItsPartRemoved(t *testing.T) {
	tx := askPartFixture(t)
	ask := insertAsk(t, tx, "CORE-1", "Which transport?",
		model.AskOption{Label: "REST", Description: "the simple one"},
		model.AskOption{Label: "gRPC"})
	indexAskPartsAt(t, tx, ask, 1)
	writeAskOptions(t, tx, &ask, model.AskOption{Label: "gRPC"})
	indexAskPartsAt(t, tx, ask, 2)

	want := []string{
		"b1 question 0 r1 Which transport?",
		"b3 option 1 r1 gRPC",
		"b2 option -1 r1 REST — the simple one",
	}
	if got := askPartSummaries(t, tx, ask.ID); !slices.Equal(got, want) {
		t.Fatalf("the parts of the ask that dropped an option = %v, want %v", got, want)
	}
	if got := askPartTexts(t, tx, ask.ID); !slices.Contains(got, "b2 r2 -") {
		t.Errorf("the part texts of the ask that dropped an option = %v, want b2 holding no text at revision 2", got)
	}
}

// A later answer is another answer, not a rewording of the first: the answer it replaces keeps
// its row as a removed part, and the new one takes an id of its own.
func TestASecondAnswerReplacesTheAnswerPart(t *testing.T) {
	tx := askPartFixture(t)
	ask := insertAsk(t, tx, "CORE-1", "Which transport?", model.AskOption{Label: "REST"})
	indexAskPartsAt(t, tx, ask, 1)
	writeAskAnswer(t, tx, &ask, "alice", nil, "Wait for the review.")
	indexAskPartsAt(t, tx, ask, 2)
	writeAskAnswer(t, tx, &ask, "bob", []string{"REST"}, "Ship it.")
	indexAskPartsAt(t, tx, ask, 3)

	want := []string{
		"b1 question 0 r1 Which transport?",
		"b2 option 1 r1 REST",
		"b4 answer 2 r3 REST - Ship it. by user:bob",
		"b3 answer -1 r2 Wait for the review. by user:alice",
	}
	if got := askPartSummaries(t, tx, ask.ID); !slices.Equal(got, want) {
		t.Fatalf("the parts of the twice-answered ask = %v, want %v", got, want)
	}
	if revision, next := askPartState(t, tx, ask.ID); revision != 3 || next != 5 {
		t.Errorf("the twice-answered ask's state = revision %d, next_block %d; want 3 and 5", revision, next)
	}
}

// A part's identity hash is salted with its type, so an option whose text is the question's takes
// a part id of its own rather than the question's.
func TestAnOptionWhoseTextIsTheQuestionsTakesItsOwnPartID(t *testing.T) {
	tx := askPartFixture(t)
	ask := insertAsk(t, tx, "CORE-1", "Which transport?", model.AskOption{Label: "Which transport?"})
	indexAskPartsAt(t, tx, ask, 1)

	want := []string{"b1 question 0 r1 Which transport?", "b2 option 1 r1 Which transport?"}
	if got := askPartSummaries(t, tx, ask.ID); !slices.Equal(got, want) {
		t.Fatalf("the parts of the ask = %v, want %v", got, want)
	}
}

// A link written in an option is an edge out of that option: a part is its own text, so the
// mention is read from the part and paired to it, with no offset into a body.
func TestALinkInAnAskOptionStartsAtThatPart(t *testing.T) {
	tx := askPartFixture(t)
	ask := insertAsk(t, tx, "CORE-1", "Which transport?",
		model.AskOption{Label: "REST", Description: "as dispatch://CORE-1/spec reads it"},
		model.AskOption{Label: "gRPC"})
	moved := indexAskPartsAt(t, tx, ask, 1)
	if want := []Target{{Kind: "artifact", ID: "CORE-1/spec"}}; !slices.Equal(moved, want) {
		t.Errorf("the targets the indexing moved = %v, want %v", moved, want)
	}

	rows, err := tx.Query(context.Background(), `
		select block_id || ' ' || from_node || ' ' || kind || ' ' || to_kind || ':' || to_id
		from ask_block_refs where ask_id = $1::uuid order by block_id
	`, ask.ID)
	if err != nil {
		t.Fatalf("read the ask's part mentions: %v", err)
	}
	links, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the ask's part mentions: %v", err)
	}
	want := []string{"b2 ask/" + ask.ID + "#b2 mentions artifact:CORE-1/spec"}
	if !slices.Equal(links, want) {
		t.Fatalf("the ask's part mentions = %v, want %v", links, want)
	}
}

// An indexing that finds the parts it last indexed writes no revision and no text row.
func TestIndexingAnUnchangedAskWritesNoRevision(t *testing.T) {
	tx := askPartFixture(t)
	ask := insertAsk(t, tx, "CORE-1", "Which transport?", model.AskOption{Label: "REST"})
	indexAskPartsAt(t, tx, ask, 1)
	before := askPartSummaries(t, tx, ask.ID)
	texts := askPartTexts(t, tx, ask.ID)
	indexAskPartsAt(t, tx, ask, 2)

	if got := askPartSummaries(t, tx, ask.ID); !slices.Equal(got, before) {
		t.Errorf("the parts after a second indexing = %v, want %v", got, before)
	}
	if got := askPartTexts(t, tx, ask.ID); !slices.Equal(got, texts) {
		t.Errorf("the part texts after a second indexing = %v, want %v", got, texts)
	}
	if revision, _ := askPartState(t, tx, ask.ID); revision != 1 {
		t.Errorf("the revision after a second indexing = %d, want 1", revision)
	}
}

// An ask any build writes is owed a block index of its parts (migration 0151's trigger), and the
// block-index reconciliation drains it: it is listed beside the comments and messages owed, and
// indexed in a transaction of its own, which leaves the ask indexed and owing nothing. A pass
// that finds the parts already indexed indexes nothing.
func TestTheReconciliationIndexesAnAskOwedItsParts(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	for _, statement := range []string{
		`insert into projects (key, name) values ('CORE', 'CORE project')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
		 values ('CORE-1', 'CORE', 1, 'Reference issue', '{"kind":"user","id":"alice"}', 'U')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the ask's issue: %v", err)
		}
	}
	var askID string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, options)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Which transport?', '[{"label":"REST"}]')
		returning id::text
	`).Scan(&askID); err != nil {
		t.Fatalf("insert the ask: %v", err)
	}

	owed, err := OwedItemBlocks(ctx, database.Pool, 10)
	if err != nil {
		t.Fatalf("list the items owed a block index: %v", err)
	}
	if want := []OwedItemBlock{{Kind: ItemAsk, ID: askID}}; !slices.Equal(owed, want) {
		t.Fatalf("the items owed a block index = %v, want %v", owed, want)
	}
	indexed, err := IndexOwedItemBlocks(ctx, database.Pool, owed[0], askPartServer)
	if err != nil || !indexed {
		t.Fatalf("index the owed ask = %v, %v; want it indexed", indexed, err)
	}
	want := []string{"b1 question 0 r1 Which transport?", "b2 option 1 r1 REST"}
	if got := askPartSummaries(t, database.Pool, askID); !slices.Equal(got, want) {
		t.Fatalf("the parts of the reconciled ask = %v, want %v", got, want)
	}
	if again, err := OwedItemBlocks(ctx, database.Pool, 10); err != nil || len(again) != 0 {
		t.Fatalf("the items owed after the index = %v, %v; want none", again, err)
	}
	if indexed, err := IndexOwedItemBlocks(ctx, database.Pool, owed[0], askPartServer); err != nil || indexed {
		t.Fatalf("index the ask again = %v, %v; want nothing indexed", indexed, err)
	}
}
