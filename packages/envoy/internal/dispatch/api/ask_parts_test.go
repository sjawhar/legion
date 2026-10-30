package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// askPartLayout reads an ask's parts as one line: its current parts in order, each as
// <id>@<changed_revision>, then each removed part as -<id>@<changed_revision>, by id. It returns
// the ask's revision too, and fails the test while the ask is still owed an index: migration
// 0146's trigger owes one on every write of an ask, and every writer of this build clears it in
// the transaction that wrote the ask.
func askPartLayout(t *testing.T, database *store.Store, askID string) (string, int) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, position is not null, changed_revision from ask_blocks
		where ask_id = $1::uuid order by position nulls last, length(block_id), block_id
	`, askID)
	if err != nil {
		t.Fatalf("read the parts of ask %s: %v", askID, err)
	}
	defer rows.Close()
	parts := []string{}
	for rows.Next() {
		var id string
		var live bool
		var changed int
		if err := rows.Scan(&id, &live, &changed); err != nil {
			t.Fatalf("scan a part of ask %s: %v", askID, err)
		}
		if !live {
			id = "-" + id
		}
		parts = append(parts, fmt.Sprintf("%s@%d", id, changed))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the parts of ask %s: %v", askID, err)
	}
	var revision int
	var owed bool
	var sum *string
	if err := database.Pool.QueryRow(context.Background(), `
		select revision, owed_at is not null, body_sha256 from ask_block_state where ask_id = $1::uuid
	`, askID).Scan(&revision, &owed, &sum); err != nil {
		t.Fatalf("read the block state of ask %s: %v", askID, err)
	}
	if owed || sum == nil {
		t.Fatalf("ask %s is owed a part index after its write (owed=%v, body_sha256=%v)", askID, owed, sum)
	}
	return strings.Join(parts, " "), revision
}

// requireAskParts fails unless the ask's parts read as layout at revision.
func requireAskParts(t *testing.T, database *store.Store, askID, step, layout string, revision int) {
	t.Helper()
	gotLayout, gotRevision := askPartLayout(t, database, askID)
	if gotLayout != layout || gotRevision != revision {
		t.Fatalf("%s: parts = %q at revision %d, want %q at revision %d", step, gotLayout, gotRevision, layout, revision)
	}
}

// askPartRow is one part row as the assertions read it: its id, its node id, its type, the login
// of its maker ("" for a part the ask's author made) and its excerpt.
type askPartRow struct {
	id, node, kind, madeBy, excerpt string
}

// askPartRows is every part row of the ask, current ones in position order, then removed ones.
func askPartRows(t *testing.T, database *store.Store, askID string) []askPartRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, node_id, type, coalesce(made_by->>'id', ''), excerpt from ask_blocks
		where ask_id = $1::uuid order by position nulls last, length(block_id), block_id
	`, askID)
	if err != nil {
		t.Fatalf("read the part rows of ask %s: %v", askID, err)
	}
	defer rows.Close()
	var parts []askPartRow
	for rows.Next() {
		var part askPartRow
		if err := rows.Scan(&part.id, &part.node, &part.kind, &part.madeBy, &part.excerpt); err != nil {
			t.Fatalf("scan a part row of ask %s: %v", askID, err)
		}
		parts = append(parts, part)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the part rows of ask %s: %v", askID, err)
	}
	return parts
}

// askPartText is the text ask_block_texts holds for a part at revision: the latest row at or
// below it, which is null for a part a revision removed.
func askPartText(t *testing.T, database *store.Store, askID, blockID string, revision int) *string {
	t.Helper()
	var text *string
	if err := database.Pool.QueryRow(context.Background(), `
		select text from ask_block_texts where ask_id = $1::uuid and block_id = $2 and revision <= $3
		order by revision desc limit 1
	`, askID, blockID, revision).Scan(&text); err != nil {
		t.Fatalf("read the text of %s of ask %s at revision %d: %v", blockID, askID, revision, err)
	}
	return text
}

// askPartRefs is every link the ask's parts hold, each as "<part id> <to kind> <to id>".
func askPartRefs(t *testing.T, database *store.Store, askID string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, to_kind, to_id from ask_block_refs where ask_id = $1::uuid
		order by block_id, to_kind, to_id
	`, askID)
	if err != nil {
		t.Fatalf("read the part links of ask %s: %v", askID, err)
	}
	defer rows.Close()
	links := []string{}
	for rows.Next() {
		var block, kind, id string
		if err := rows.Scan(&block, &kind, &id); err != nil {
			t.Fatalf("scan a part link of ask %s: %v", askID, err)
		}
		links = append(links, block+" "+kind+" "+id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the part links of ask %s: %v", askID, err)
	}
	return links
}

// askRefs is every link the ask's own row holds, each as "<to kind> <to id>".
func askRefs(t *testing.T, database *store.Store, askID string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select to_kind, to_id from refs where from_kind = 'ask' and from_id = $1 order by to_kind, to_id
	`, askID)
	if err != nil {
		t.Fatalf("read the links of ask %s: %v", askID, err)
	}
	defer rows.Close()
	links := []string{}
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			t.Fatalf("scan a link of ask %s: %v", askID, err)
		}
		links = append(links, kind+" "+id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the links of ask %s: %v", askID, err)
	}
	return links
}

// createPartAsk opens an ask on the issue as the test session, with the options it is given.
func createPartAsk(t *testing.T, handler http.Handler, issueKey, question string, options ...map[string]string) model.Ask {
	t.Helper()
	body := map[string]any{"actor": sessionActor(), "question": question}
	if len(options) > 0 {
		body["options"] = options
	}
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create the ask: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

// editPartAskOptions rewrites the ask's options as its asking session does.
func editPartAskOptions(t *testing.T, handler http.Handler, askID, step string, options ...map[string]string) model.Ask {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"actor": sessionActor(), "options": options,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", step, response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

// answerPartAsk answers the ask as login, with the question revision that login read.
func answerPartAsk(t *testing.T, handler http.Handler, ask model.Ask, login string, selected []string, text string) model.Ask {
	t.Helper()
	body := map[string]any{"selected": selected, "expected_edited_at": ask.EditedAt}
	if text != "" {
		body["text"] = text
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", body, login)
	if response.Code != http.StatusOK {
		t.Fatalf("answer the ask: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

// Every write of an ask's question, options or answer indexes its parts in its own transaction:
// each write is a revision of the ask's blocks, and nothing is left for the reconciliation to
// drain. The order here is the order the routes allow: an answered ask is closed to edits
// (ASK_NOT_OPEN), so the rewording and the dropped option come before the answer.
func TestAnAsksPartsAreIndexedOnEveryWrite(t *testing.T) {
	const (
		monday      = "Monday — Ship at the start of the week."
		tuesday     = "Tuesday — Ship once the review lands."
		reworded    = "Tuesday — Ship once the review lands and the queue is green."
		answerText  = "Monday - Ship it then."
		theQuestion = "Which day do we ship?"
	)
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask parts", "A spec")
	ask := createPartAsk(t, handler, issue.Key, theQuestion,
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."},
		map[string]string{"label": "Tuesday", "description": "Ship once the review lands."})

	requireAskParts(t, database, ask.ID, "create", "b1@1 b2@1 b3@1", 1)
	want := []askPartRow{
		{id: "b1", node: "ask/" + ask.ID + "#b1", kind: "question", excerpt: theQuestion},
		{id: "b2", node: "ask/" + ask.ID + "#b2", kind: "option", excerpt: monday},
		{id: "b3", node: "ask/" + ask.ID + "#b3", kind: "option", excerpt: tuesday},
	}
	if got := askPartRows(t, database, ask.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("the created ask's parts = %#v, want %#v", got, want)
	}

	// A reworded option keeps its part id at the new revision, and no other part moves.
	edited := editPartAskOptions(t, handler, ask.ID, "reword an option",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."},
		map[string]string{"label": "Tuesday", "description": "Ship once the review lands and the queue is green."})
	requireAskParts(t, database, ask.ID, "reword an option", "b1@1 b2@1 b3@2", 2)
	if text := askPartText(t, database, ask.ID, "b3", 2); text == nil || *text != reworded {
		t.Fatalf("the reworded option's text at revision 2 = %v, want %q", text, reworded)
	}

	// The option the ask no longer holds keeps its row, with no position and the last excerpt it
	// held, so a reference made while it was live still resolves and reads removed. Its changed
	// revision stays the one its text last changed in; the revision that dropped it records that
	// it holds no text.
	edited = editPartAskOptions(t, handler, ask.ID, "drop an option",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."})
	requireAskParts(t, database, ask.ID, "drop an option", "b1@1 b2@1 -b3@2", 3)
	dropped := askPartRows(t, database, ask.ID)[2]
	if dropped.id != "b3" || dropped.excerpt != reworded {
		t.Fatalf("the dropped option = %#v, want b3 with the excerpt %q it last held", dropped, reworded)
	}
	if text := askPartText(t, database, ask.ID, "b3", 3); text != nil {
		t.Fatalf("the dropped option's text at revision 3 = %q, want none", *text)
	}

	// The answer is a part of its own, made by the answerer, who is not the ask's author.
	answered := answerPartAsk(t, handler, edited, "alice", []string{"Monday"}, "Ship it then.")
	if answered.Answer == nil {
		t.Fatal("the answered ask carries no answer")
	}
	requireAskParts(t, database, ask.ID, "answer", "b1@1 b2@1 b4@4 -b3@2", 4)
	answer := askPartRows(t, database, ask.ID)[2]
	if answer.id != "b4" || answer.kind != "answer" || answer.madeBy != "alice" || answer.excerpt != answerText {
		t.Fatalf("the answer part = %#v, want b4 made by alice reading %q", answer, answerText)
	}
}

// A decided ask an earlier release reworded gets its decided wording back with no event of any
// kind (restoreWordings), and its parts move with its row: the restoration is a write of the
// ask's question and options, so it indexes them, and leaves nothing owed.
func TestARestoredDecidedWordingMovesTheAsksPartRevision(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, ServerURL: "https://dispatch.example", Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Restored wording", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Ship on Monday?",
		map[string]string{"label": "Yes", "description": "Monday."},
		map[string]string{"label": "No", "description": "Later."})
	answerPartAsk(t, handler, ask, "alice", []string{"Yes"}, "")
	requireAskParts(t, database, ask.ID, "answer", "b1@1 b2@1 b3@1 b4@2", 2)

	// What a task of the release before LEGION-651 leaves: the document's rewording in the
	// decided row, with an ask.edited recording the wording the decision was given on. Its
	// release had no part index, so the parts are owed and the reconciliation indexes them.
	rewordAsAnEarlierRelease(t, database, issue.Key, ask, "Ship on Tuesday?",
		[]model.AskOption{{Label: "Tuesday", Description: "Ship it."}, {Label: "No", Description: "Later."}})
	if indexed, err := refs.IndexOwedItemBlocks(context.Background(), database.Pool,
		refs.OwedItemBlock{Kind: refs.ItemAsk, ID: ask.ID}, "https://dispatch.example"); err != nil || !indexed {
		t.Fatalf("the reconciliation indexed the reworded ask = %v (%v), want it indexed", indexed, err)
	}
	requireAskParts(t, database, ask.ID, "the earlier release's rewording", "b1@3 b2@3 b3@1 b4@2", 3)

	restored, copies, err := documentService.RestoreDecidedWordings(context.Background())
	if err != nil || restored != 1 || copies != 0 {
		t.Fatalf("the boot's restoration restored %d asks and marked %d copies owed (%v), want the one ask", restored, copies, err)
	}
	requireAskParts(t, database, ask.ID, "the restoration", "b1@4 b2@4 b3@1 b4@2", 4)
	if text := askPartText(t, database, ask.ID, "b1", 4); text == nil || *text != "Ship on Monday?" {
		t.Fatalf("the restored question part = %v, want the wording the answer was given on", text)
	}
	if edits := askEditedEvents(t, database, ask.ID); edits != 1 {
		t.Fatalf("the ask has %d ask.edited events after the restoration, want only the earlier release's", edits)
	}
}

// rewordAsAnEarlierRelease writes wording into the decided ask's row, as a task of the release
// before LEGION-651 did when its settlement found the block reworded: the row takes the question
// and the options with edited_at, and an ask.edited records the wording it replaced. That release
// indexed no parts, so none are written here either.
func rewordAsAnEarlierRelease(t *testing.T, database *store.Store, issueKey string, ask model.Ask, question string, options []model.AskOption) {
	t.Helper()
	ctx := context.Background()
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatalf("encode the rewording's options: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		update asks set question = $2, options = $3, edited_at = now() where id = $1::uuid
	`, ask.ID, question, encoded); err != nil {
		t.Fatalf("reword the decided ask's row: %v", err)
	}
	payload, err := json.Marshal(map[string]any{"id": ask.ID, "previous": model.NewAskEditPrevious(ask)})
	if err != nil {
		t.Fatalf("encode the rewording's payload: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, coalesce(max(seq), 0) + 1, 'ask.edited', '{"kind":"session","id":"session-0123456789abcdef"}', $2::jsonb, false
		from events where issue_key = $1
	`, issueKey, payload); err != nil {
		t.Fatalf("append the rewording's ask.edited: %v", err)
	}
}

// askEditedEvents is how many ask.edited events the ask has.
func askEditedEvents(t *testing.T, database *store.Store, askID string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*)::int from events where type = 'ask.edited' and payload->>'id' = $1
	`, askID).Scan(&count); err != nil {
		t.Fatalf("count the ask.edited events of ask %s: %v", askID, err)
	}
	return count
}

// A part is a text of its own, so a link written in an option is an edge out of that option and
// out of nothing else. A link the question alone holds is written by the ask's row and by its
// question part, and the target counts it once either way.
func TestALinkInAnOptionStartsAtThatOption(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	source := createReferenceAPIIssue(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	spec := "dispatch://" + target.Key + "/spec"

	option := createPartAsk(t, handler, source.Key, "Which transport?",
		map[string]string{"label": "REST", "description": "as " + spec + " reads it"},
		map[string]string{"label": "gRPC"})
	if links := askPartRefs(t, database, option.ID); len(links) != 1 || links[0] != "b2 artifact "+target.Key+"/spec" {
		t.Fatalf("the links of the option ask's parts = %#v, want one from its REST option", links)
	}
	if links := askRefs(t, database, option.ID); len(links) != 0 {
		t.Fatalf("the links of the option ask's own row = %#v, want none: its question holds no link", links)
	}

	// The question's own link: the ask's row holds it, as it always has, and so does the
	// question part. The target counts it once.
	question := createPartAsk(t, handler, source.Key, "Does dispatch://"+target.Key+" ship first?")
	if links := askPartRefs(t, database, question.ID); len(links) != 1 || links[0] != "b1 issue "+target.Key {
		t.Fatalf("the links of the question ask's parts = %#v, want one from its question", links)
	}
	if links := askRefs(t, database, question.ID); len(links) != 1 || links[0] != "issue "+target.Key {
		t.Fatalf("the links of the question ask's own row = %#v, want its question's", links)
	}
	// One beside the structural edge of the issue's own spec document.
	requireIssueBacklinkCount(t, handler, target.Key, 2)
}

// An answer is a part, so its text reaches the graph, and the ask.answered event the answer
// appends names what the answer moved: the targets its own text mentions.
func TestAnAnsweredAsksEventNamesWhatItsAnswerMentions(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	source := createReferenceAPIIssue(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	ask := createPartAsk(t, handler, source.Key, "Which day do we ship?",
		map[string]string{"label": "Monday"})
	answerPartAsk(t, handler, ask, "alice", []string{"Monday"}, "Monday, once dispatch://"+target.Key+" lands.")

	requireReferencesChanged(t, handler, source.Key, "ask.answered", ask.ID, []model.ChangedReference{
		{Kind: "issue", ID: target.Key},
	})
	if links := askPartRefs(t, database, ask.ID); len(links) != 1 || links[0] != "b3 issue "+target.Key {
		t.Fatalf("the links of the answered ask's parts = %#v, want one from its answer", links)
	}
}

// askPartRef is the reference naming part blockID of the ask.
func askPartRef(issueKey, askID, blockID string) string {
	return "dispatch://" + issueKey + "/ask/" + askID + "#b-" + blockID
}

// latestEventID is the id of the newest event of the issue, the id a ?since= read counts from.
func latestEventID(t *testing.T, database *store.Store, issueKey string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(max(id), 0) from events where issue_key = $1
	`, issueKey).Scan(&id); err != nil {
		t.Fatalf("read the newest event of %s: %v", issueKey, err)
	}
	return id
}

// askPartTime is when part blockID of the ask was made, which is when it was last changed while
// no later revision reworded it.
func askPartTime(t *testing.T, database *store.Store, askID, blockID string) time.Time {
	t.Helper()
	var made, changed time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select made_at, changed_at from ask_blocks where ask_id = $1::uuid and block_id = $2
	`, askID, blockID).Scan(&made, &changed); err != nil {
		t.Fatalf("read the times of %s of ask %s: %v", blockID, askID, err)
	}
	if !made.Equal(changed) {
		t.Fatalf("part %s of ask %s was made at %v and changed at %v; want one revision's time", blockID, askID, made, changed)
	}
	return made
}

// A part of an ask is a node a reference names: a comment citing an option holds one mention of
// that part and of nothing else, the ask's own read lists the mention at the part holding it, and
// the ask's card counts it. Each current part is part_of the ask, so an answer adds one. A part
// the ask does not hold answers 404 naming the part and the ask.
func TestAnAskOptionIsAddressable(t *testing.T) {
	const option = "Monday — Ship at the start of the week."
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Addressable ask parts", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."},
		map[string]string{"label": "Tuesday"})
	askRef := "dispatch://" + issue.Key + "/ask/" + ask.ID
	optionRef := askPartRef(issue.Key, ask.ID, "b2")
	citer := createThreadComment(t, handler, issue.Key, map[string]any{"body": "We take " + optionRef + "."}, "alice")

	if got := itemBlockRefs(t, database, "comment", citer.ID); strings.Join(got, ",") != "b1->ask_block:ask/"+ask.ID+"#b2" {
		t.Fatalf("the citing comment's block mentions = %v, want b1 -> ask_block ask/%s#b2", got, ask.ID)
	}
	written := readGraph(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/comment/" + citer.ID}}).Edges
	if len(written) != 1 || written[0].Kind != "mentions" {
		t.Fatalf("the citing comment writes %#v, want its one mention of the option", written)
	}
	if node := written[0].Node; node.Kind != "block" || node.ID != "ask/"+ask.ID+"#b2" || node.BlockID != "b2" ||
		node.Ref != optionRef || node.Parent == nil || node.Parent.ID != ask.ID {
		t.Fatalf("the mention's other end = %#v, want part b2 of ask %s", written[0].Node, ask.ID)
	}

	part := readGraph(t, handler, url.Values{"to": {optionRef}})
	if node := part.Node; node.Kind != "block" || node.ID != "ask/"+ask.ID+"#b2" || node.BlockID != "b2" ||
		node.Ref != optionRef || node.Text != option || node.ChangedVersion != 1 {
		t.Fatalf("?to= the option reads the node %#v, want part b2 reading %q", part.Node, option)
	}
	if len(part.Edges) != 1 || part.Edges[0].Node.ID != "comment/"+citer.ID+"#b1" {
		t.Fatalf("?to= the option lists %#v, want the mention from block b1 of comment %s", part.Edges, citer.ID)
	}

	whole := readGraph(t, handler, url.Values{"to": {askRef}})
	if len(whole.Edges) != 1 || whole.Edges[0].Kind != "mentions" || whole.Edges[0].Block == nil ||
		whole.Edges[0].Block.BlockID != "b2" || whole.Edges[0].Node.ID != "comment/"+citer.ID+"#b1" {
		t.Fatalf("?to= the ask lists %#v, want the mention once, at its part b2", whole.Edges)
	}
	askRead := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if askRead.Code != http.StatusOK {
		t.Fatalf("read the ask: status=%d body=%s", askRead.Code, askRead.Body.String())
	}
	counted := decodeBody[struct {
		Ask askBacklinkCount `json:"ask"`
	}](t, askRead).Ask
	requireAskBacklinkCount(t, map[string]*int{counted.ID: counted.ReferencedByCount}, ask.ID, 1)

	missing := graphRequest(t, handler, url.Values{"to": {askPartRef(issue.Key, ask.ID, "b9")}})
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"NOT_FOUND"`) ||
		!strings.Contains(missing.Body.String(), `no part \"b9\" in ask `+ask.ID) {
		t.Fatalf("?to= part b9: status=%d body=%s", missing.Code, missing.Body.String())
	}

	if parts := readGraph(t, handler, url.Values{"to": {askRef}, "kind": {"part_of"}}).Edges; len(parts) != 3 {
		t.Fatalf("?to= the ask with kind=part_of lists %d edges, want its question and two options", len(parts))
	}
	answerPartAsk(t, handler, ask, "alice", []string{"Monday"}, "Ship it then.")
	if parts := readGraph(t, handler, url.Values{"to": {askRef}, "kind": {"part_of"}}).Edges; len(parts) != 4 {
		t.Fatalf("?to= the answered ask with kind=part_of lists %d edges, want its four parts", len(parts))
	}
}

// R12 for an ask's parts: a part carries the six facts of every node, read by its own reference
// and as the other end of an edge. The ask's author made its question and its options, at the
// revision that first held each; the answerer made the answer part, at the answer's time; each is
// part of its ask at the revision its text last changed in.
func TestAnAskPartCarriesTheFactsOfEveryNode(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask part facts", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?", map[string]string{"label": "Monday"})
	askRef := "dispatch://" + issue.Key + "/ask/" + ask.ID
	optionRef := askPartRef(issue.Key, ask.ID, "b2")
	citer := createThreadComment(t, handler, issue.Key, map[string]any{"body": "We take " + optionRef + "."}, "alice")

	opened, answered := 1, 2
	asked := nodeFacts{
		MadeBy: actorNames([]model.Actor{ask.Author}), MadeAt: &ask.CreatedAt,
		ChangedBy: actorNames([]model.Actor{ask.Author}), ChangedAt: &ask.CreatedAt,
		Parent: &model.NodeParent{Kind: "ask", ID: ask.ID, Ref: askRef}, ParentVersion: &opened,
	}
	requireFacts(t, "the option read by its own reference", readNode(t, handler, optionRef), asked)
	written := readGraph(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/comment/" + citer.ID}}).Edges
	if len(written) != 1 {
		t.Fatalf("the citing comment writes %#v, want its one mention of the option", written)
	}
	requireFacts(t, "the option as the other end of the comment's mention", written[0].Node, asked)

	answer := answerPartAsk(t, handler, ask, "alice", []string{"Monday"}, "Ship it then.")
	if answer.Answer == nil {
		t.Fatal("the answered ask carries no answer")
	}
	// The answer part is written at the answer's time, which the row holds to the microsecond
	// Postgres keeps, where the answered ask carries the instant the write made.
	answeredAt := askPartTime(t, database, ask.ID, "b3")
	if !answeredAt.Equal(answer.Answer.At.Truncate(time.Microsecond)) {
		t.Fatalf("the answer part was written at %v, want the answer's time %v", answeredAt, answer.Answer.At)
	}
	requireFacts(t, "the answer part", readNode(t, handler, askPartRef(issue.Key, ask.ID, "b3")), nodeFacts{
		MadeBy: actorNames([]model.Actor{{Kind: "user", ID: "alice"}}), MadeAt: &answeredAt,
		ChangedBy: actorNames([]model.Actor{{Kind: "user", ID: "alice"}}), ChangedAt: &answeredAt,
		Parent: &model.NodeParent{Kind: "ask", ID: ask.ID, Ref: askRef}, ParentVersion: &answered,
	})
}

// A15 for an ask's parts: a link pinned to revision N of an ask reads the part as N recorded it,
// and says how it stands now: unchanged, changed (after an edit rewords the option) or removed
// (after the edit that drops it, with the revision that removed it). The unpinned link reads the
// current text, or 404 once the part is gone; a pin past the ask's revision answers 404.
func TestAPinnedAskPartShowsTheRevisionItNames(t *testing.T) {
	const (
		tuesday  = "Tuesday — Ship once the review lands."
		reworded = "Tuesday — Ship once the review lands and the queue is green."
	)
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Pinned ask parts", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."},
		map[string]string{"label": "Tuesday", "description": "Ship once the review lands."})
	optionRef := askPartRef(issue.Key, ask.ID, "b3")
	createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Now " + optionRef + ", then " + optionRef + "@v1.",
	}, "alice")

	requirePin := func(step, ref, text, state string, removedVersion int, current string) model.GraphReferences {
		t.Helper()
		read := readGraph(t, handler, url.Values{"to": {ref}})
		want := model.BlockPin{Version: 1, Ref: optionRef + "@v1", Text: text, State: state, RemovedVersion: removedVersion}
		if read.Node.Pin == nil || *read.Node.Pin != want {
			t.Fatalf("%s: the pin reads %#v, want %#v", step, read.Node.Pin, want)
		}
		if read.Node.Text != current {
			t.Fatalf("%s: the pinned node's text = %q, want %q", step, read.Node.Text, current)
		}
		return read
	}
	requireUnpinned := func(step, text string) {
		t.Helper()
		if read := readGraph(t, handler, url.Values{"to": {optionRef}}); read.Node.Pin != nil || read.Node.Text != text {
			t.Fatalf("%s: the unpinned read = %#v, want the text %q and no pin", step, read.Node, text)
		}
	}
	requireNotFound := func(step, ref, message string) {
		t.Helper()
		response := graphRequest(t, handler, url.Values{"to": {ref}})
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), message) {
			t.Fatalf("%s: status=%d body=%s, want 404 saying %q", step, response.Code, response.Body.String(), message)
		}
	}

	if read := requirePin("before the edit", optionRef+"@v1", tuesday, "unchanged", 0, tuesday); len(read.Edges) != 1 {
		t.Fatalf("before the edit: the pinned read lists %d edges, want the one mention (pinned and unpinned are one target)", len(read.Edges))
	}
	requireUnpinned("before the edit", tuesday)
	requireNotFound("a pin past the revision", optionRef+"@v2", "no revision 2 of ask "+ask.ID+"; its current revision is 1")

	edited := editPartAskOptions(t, handler, ask.ID, "reword the option",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."},
		map[string]string{"label": "Tuesday", "description": "Ship once the review lands and the queue is green."})
	requirePin("after the reword", optionRef+"@v1", tuesday, "changed", 0, reworded)
	requireUnpinned("after the reword", reworded)

	editPartAskOptions(t, handler, edited.ID, "drop the option",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."})
	removed := requirePin("after the drop", optionRef+"@v1", tuesday, "removed", 3, "")
	if len(removed.Edges) != 0 || removed.Node.ChangedVersion != 0 || removed.Node.ParentVersion == nil ||
		*removed.Node.ParentVersion != 2 || removed.Node.Parent == nil || removed.Node.Parent.ID != ask.ID {
		t.Fatalf("after the drop: the removed part reads %#v with %d edges, want no edge, part of ask %s at revision 2",
			removed.Node, len(removed.Edges), ask.ID)
	}
	if read := readGraph(t, handler, url.Values{"to": {optionRef + "@v2"}}); read.Node.Pin == nil ||
		read.Node.Pin.Text != reworded || read.Node.Pin.State != "removed" {
		t.Fatalf("after the drop: the pin at revision 2 reads %#v, want its reworded text, removed", read.Node.Pin)
	}
	requireNotFound("the unpinned read after the drop", optionRef, `no part \"b3\" in ask `+ask.ID)
	requireNotFound("a pin to the revision that dropped it", optionRef+"@v3", `no part \"b3\" in ask `+ask.ID+" at revision 3")
}

// The answer's part rows are stamped with the ask.answered event that carried them (refs.Stamp),
// so a ?since= read of the ask lists the edges the answer wrote and no older one; an unstamped row
// is invisible to every such read.
func TestAnAnsweredAsksPartEdgesAreListedSince(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	source := createReferenceAPIIssue(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	ask := createPartAsk(t, handler, source.Key, "Does dispatch://"+target.Key+" ship first?",
		map[string]string{"label": "Monday"})
	askRef := "dispatch://" + source.Key + "/ask/" + ask.ID
	beforeTheAnswer := latestEventID(t, database, source.Key)
	answerPartAsk(t, handler, ask, "alice", []string{"Monday"}, "Monday, once dispatch://"+target.Key+" lands.")

	since := url.Values{"from": {askRef}, "since": {strconv.FormatInt(beforeTheAnswer, 10)}}
	edges := readGraph(t, handler, since).Edges
	if len(edges) != 1 || edges[0].Kind != "mentions" || edges[0].Node.ID != target.Key ||
		edges[0].Block == nil || edges[0].Block.BlockID != "b3" {
		t.Fatalf("?from= the ask since the event before the answer lists %#v, want the answer part's mention", edges)
	}
	afterTheAnswer := latestEventID(t, database, source.Key)
	if edges := readGraph(t, handler, url.Values{
		"from": {askRef}, "since": {strconv.FormatInt(afterTheAnswer, 10)},
	}).Edges; len(edges) != 0 {
		t.Fatalf("?from= the ask since the answer's own event lists %#v, want none", edges)
	}
}

// readPartAsk reads GET /api/v1/asks/<id> as a human: the ask as its own read carries it, with
// the part id of every option, of its answer, and the revision of its parts a pin names.
func readPartAsk(t *testing.T, handler http.Handler, askID string) model.Ask {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read ask %s: status=%d body=%s", askID, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, response).Ask
}

// askOptionIDs is the part id each of the ask's options carries, "" for an option carrying none.
func askOptionIDs(ask model.Ask) []string {
	ids := make([]string, len(ask.Options))
	for index, option := range ask.Options {
		ids[index] = option.ID
	}
	return ids
}

// askOptionsJSON is the ask row's options column as it was stored.
func askOptionsJSON(t *testing.T, database *store.Store, askID string) string {
	t.Helper()
	var options string
	if err := database.Pool.QueryRow(context.Background(), `
		select options::text from asks where id = $1::uuid
	`, askID).Scan(&options); err != nil {
		t.Fatalf("read the options of ask %s: %v", askID, err)
	}
	return options
}

// An ask's own read names every part it shows: each option carries the part id its reference
// uses, so the reader writes `dispatch://KEY/ask/<id>#b-<that id>` from the ask's JSON rather
// than guessing it, the answer carries its own id, and the ask carries the revision of its parts
// that `@v<N>` pins.
func TestAnAsksReadCarriesThePartIDsOfItsOptionsAndAnswer(t *testing.T) {
	const monday = "Monday — Ship at the start of the week."
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask part ids", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."},
		map[string]string{"label": "Tuesday"})

	read := readPartAsk(t, handler, ask.ID)
	if got := askOptionIDs(read); !reflect.DeepEqual(got, []string{"b2", "b3"}) {
		t.Fatalf("the read ask's option ids = %v, want b2 and b3", got)
	}
	if read.Revision != 1 {
		t.Fatalf("the read ask's revision = %d, want the revision of its first indexing", read.Revision)
	}
	node := readNode(t, handler, askPartRef(issue.Key, ask.ID, read.Options[0].ID))
	if node.Kind != "block" || node.ID != "ask/"+ask.ID+"#"+read.Options[0].ID || node.Text != monday {
		t.Fatalf("the reference of option %q reads %#v, want the part reading %q", read.Options[0].ID, node, monday)
	}

	answerPartAsk(t, handler, ask, "alice", []string{"Monday"}, "Ship it then.")
	answered := readPartAsk(t, handler, ask.ID)
	if answered.Answer == nil || answered.Answer.ID != "b4" {
		t.Fatalf("the answered ask's answer = %#v, want the part id b4", answered.Answer)
	}
	if got := askOptionIDs(answered); !reflect.DeepEqual(got, []string{"b2", "b3"}) {
		t.Fatalf("the answered ask's option ids = %v, want b2 and b3 still", got)
	}
	if answered.Revision != 2 {
		t.Fatalf("the answered ask's revision = %d, want 2: the answer is a revision of its parts", answered.Revision)
	}
	if answer := readNode(t, handler, askPartRef(issue.Key, ask.ID, answered.Answer.ID)); answer.Text != "Monday - Ship it then." {
		t.Fatalf("the reference of the answer part reads %#v, want the answer's text", answer)
	}
}

// A part id is the index's to mint: an option a write sends back with an id of its own is stored
// without it, so no caller mints a part id or moves one, and the ask's parts keep the ids they
// were minted with.
func TestAWriteCannotMintOrMoveAnAskPartID(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Minted part ids", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?",
		map[string]string{"label": "Monday", "id": "b9"})
	requireAskParts(t, database, ask.ID, "the ask opened with an id of the caller's", "b1@1 b2@1", 1)
	if stored := askOptionsJSON(t, database, ask.ID); strings.Contains(stored, `"id"`) {
		t.Fatalf("the opened ask's options row = %s, want no id of the caller's", stored)
	}
	if got := askOptionIDs(readPartAsk(t, handler, ask.ID)); !reflect.DeepEqual(got, []string{"b2"}) {
		t.Fatalf("the opened ask's option ids = %v, want the minted b2", got)
	}

	editPartAskOptions(t, handler, ask.ID, "send the option back with an id of its own",
		map[string]string{"label": "Monday", "id": "b7"})
	requireAskParts(t, database, ask.ID, "after the write sending an id back", "b1@1 b2@1", 1)
	if stored := askOptionsJSON(t, database, ask.ID); strings.Contains(stored, `"id"`) {
		t.Fatalf("the edited ask's options row = %s, want no id of the caller's", stored)
	}
	if got := askOptionIDs(readPartAsk(t, handler, ask.ID)); !reflect.DeepEqual(got, []string{"b2"}) {
		t.Fatalf("the edited ask's option ids = %v, want the minted b2, unmoved", got)
	}
}

// An ask carries the same part ids wherever it is listed: the issue detail's open_asks and the
// human Inbox name each option as the ask's own read names it, so a reader cites an option from
// the list in front of it.
func TestAListedAskCarriesThePartIDsItsOwnReadCarries(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Listed part ids", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?",
		map[string]string{"label": "Monday"}, map[string]string{"label": "Tuesday"})
	want := readPartAsk(t, handler, ask.ID)
	if got := askOptionIDs(want); !reflect.DeepEqual(got, []string{"b2", "b3"}) {
		t.Fatalf("the ask's own read carries the option ids %v, want b2 and b3", got)
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue %s: status=%d body=%s", issue.Key, detail.Code, detail.Body.String())
	}
	open := decodeBody[struct {
		OpenAsks []model.Ask `json:"open_asks"`
	}](t, detail).OpenAsks
	if len(open) != 1 || !reflect.DeepEqual(askOptionIDs(open[0]), askOptionIDs(want)) || open[0].Revision != want.Revision {
		t.Fatalf("the issue detail's open_asks = %#v, want the ask's option ids %v at revision %d",
			open, askOptionIDs(want), want.Revision)
	}

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	rows := decodeBody[[]model.Ask](t, inbox)
	if len(rows) != 1 || rows[0].ID != ask.ID ||
		!reflect.DeepEqual(askOptionIDs(rows[0]), askOptionIDs(want)) || rows[0].Revision != want.Revision {
		t.Fatalf("the inbox rows = %#v, want the ask's option ids %v at revision %d",
			rows, askOptionIDs(want), want.Revision)
	}
}

// A reply in an ask's thread names the part it answers: `reply_to_block` is checked against the
// ask's own parts, the reply stores that part's node id, every read of the reply carries it and
// ?to= the part lists the reply. The ask's referenced-by count leaves the reply out, since a
// clarification thread is never what "referenced by" means (refs.AskBacklinkExclusions); an edit
// that drops the option leaves the reply reading the part removed, with the last text it held, as
// a removed comment block's reply reads; a part the ask does not hold is refused; and a reply an
// older build wrote under the ask, with no block at all, still reads under it naming none.
func TestAReplyToAnAskNamesThePartItAnswers(t *testing.T) {
	const monday = "Monday — Ship at the start of the week."
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask thread replies", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."},
		map[string]string{"label": "Tuesday"})
	optionRef := askPartRef(issue.Key, ask.ID, "b2")
	option := &model.ReplyBlock{Node: "ask/" + ask.ID + "#b2", Ref: optionRef, BlockID: "b2", Text: monday}

	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Monday works for us.", "ask_id": ask.ID, "reply_to_block": "b2",
	}, "alice")
	if reply.AskID == nil || *reply.AskID != ask.ID {
		t.Fatalf("the reply's ask_id = %v, want %s", reply.AskID, ask.ID)
	}
	requireReplyBlock(t, "the write's answer", reply.ReplyBlock, option)
	var stored *string
	if err := database.Pool.QueryRow(context.Background(), `select replies_to_block from comments where id = $1`, reply.ID).Scan(&stored); err != nil ||
		stored == nil || *stored != option.Node {
		t.Fatalf("replies_to_block = %v (%v), want %s", stored, err, option.Node)
	}
	requireReplyBlock(t, "GET the reply", readCommentThread(t, handler, reply.ID).Comment.ReplyBlock, option)

	// The ask's own read lists the reply under it, carrying the part it answers.
	askReplies := func(step string) []model.Comment {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", step, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			Replies []model.Comment `json:"replies"`
		}](t, response).Replies
	}
	if replies := askReplies("read the ask"); len(replies) != 1 || replies[0].ID != reply.ID {
		t.Fatalf("the ask's replies = %#v, want the reply", replies)
	} else {
		requireReplyBlock(t, "the ask's replies", replies[0].ReplyBlock, option)
	}

	// ?to= the part lists the reply, and the ask's card does not count it as a reference.
	if edges := readGraph(t, handler, url.Values{"to": {optionRef}, "kind": {"replies_to"}}).Edges; len(edges) != 1 ||
		edges[0].Node.Kind != "comment" || edges[0].Node.ID != reply.ID {
		t.Fatalf("?to= the option with kind=replies_to lists %#v, want the reply", edges)
	}
	askRead := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if askRead.Code != http.StatusOK {
		t.Fatalf("read the ask: status=%d body=%s", askRead.Code, askRead.Body.String())
	}
	counted := decodeBody[struct {
		Ask askBacklinkCount `json:"ask"`
	}](t, askRead).Ask
	requireAskBacklinkCount(t, map[string]*int{counted.ID: counted.ReferencedByCount}, ask.ID, 0)

	// The edit drops Monday: the reply still names the part, removed, with its last text.
	editPartAskOptions(t, handler, ask.ID, "drop Monday", map[string]string{"label": "Tuesday"})
	removed := *option
	removed.Removed = true
	requireReplyBlock(t, "GET the reply after the edit dropped the option",
		readCommentThread(t, handler, reply.ID).Comment.ReplyBlock, &removed)
	if replies := askReplies("read the ask after the edit"); len(replies) != 1 {
		t.Fatalf("the ask's replies after the edit = %#v, want the reply", replies)
	} else {
		requireReplyBlock(t, "the ask's replies after the edit", replies[0].ReplyBlock, &removed)
	}

	post := func(input map[string]any) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", input, "alice")
	}
	requireRefusal(t, "a part the ask does not hold", post(map[string]any{"body": "On b9.", "ask_id": ask.ID, "reply_to_block": "b9"}),
		"INVALID_COMMENT", "reply_to_block b9 is not a block of ask "+ask.ID)
	requireRefusal(t, "a part the edit dropped", post(map[string]any{"body": "On Monday.", "ask_id": ask.ID, "reply_to_block": "b2"}),
		"INVALID_COMMENT", "reply_to_block b2 is not a block of ask "+ask.ID)

	// A reply an older build wrote names no block, and still reads under the ask.
	var older string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, ask_id)
		values ($1, '{"kind":"user","id":"alice"}', $2, $3::uuid) returning id::text
	`, issue.Key, "Written before the parts were blocks.", ask.ID).Scan(&older); err != nil {
		t.Fatalf("insert a reply as an older build: %v", err)
	}
	replies := askReplies("read the ask with the older reply")
	if len(replies) != 2 || replies[1].ID != older {
		t.Fatalf("the ask's replies = %#v, want the reply and the older one", replies)
	}
	requireReplyBlock(t, "the older reply", replies[1].ReplyBlock, nil)
	requireReplyBlock(t, "GET the older reply", readCommentThread(t, handler, older).Comment.ReplyBlock, nil)

	// The thread climb reaches the ask too: a reply to a comment under it names a part of the
	// ask, never a block of that comment, which holds no b3 at all.
	climbed := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Tuesday then.", "reply_to": reply.ID, "reply_to_block": "b3",
	}, "bob")
	requireReplyBlock(t, "a reply whose climb reaches the ask", climbed.ReplyBlock,
		&model.ReplyBlock{Node: "ask/" + ask.ID + "#b3", Ref: askPartRef(issue.Key, ask.ID, "b3"), BlockID: "b3", Text: "Tuesday"})
	requireRefusal(t, "a climb to the ask naming no part of it",
		post(map[string]any{"body": "On b9.", "reply_to": reply.ID, "reply_to_block": "b9"}),
		"INVALID_COMMENT", "reply_to_block b9 is not a block of ask "+ask.ID)
}
