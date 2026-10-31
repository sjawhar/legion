package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestShortIDRangeReadsTheUUIDsAPrefixNames(t *testing.T) {
	for _, test := range []struct {
		written string
		low     string
		high    string
	}{
		{"9b31964e", "9b31964e-0000-0000-0000-000000000000", "9b31964e-ffff-ffff-ffff-ffffffffffff"},
		{"9B31964E", "9b31964e-0000-0000-0000-000000000000", "9b31964e-ffff-ffff-ffff-ffffffffffff"},
		{"9b31964e-dfdb", "9b31964e-dfdb-0000-0000-000000000000", "9b31964e-dfdb-ffff-ffff-ffffffffffff"},
		{"9b31964ed", "9b31964e-d000-0000-0000-000000000000", "9b31964e-dfff-ffff-ffff-ffffffffffff"},
	} {
		low, high, ok := shortIDRange(test.written)
		if !ok || low.String() != test.low || high.String() != test.high {
			t.Errorf("shortIDRange(%q) = %s..%s ok=%v, want %s..%s", test.written, low, high, ok, test.low, test.high)
		}
	}
	// A whole uuid is no short id: it is read as itself, with no range scan at all. Nor is
	// anything shorter than eight hex digits, or holding a character that is not one.
	for _, written := range []string{
		"9b31964e-dfdb-4fc3-882d-8875f4fce861",
		"9b31964edfdb4fc3882d8875f4fce861",
		"{9b31964e-dfdb-4fc3-882d-8875f4fce861}",
		"9b31964", "9b31-964", "", "spec", "9b31964g", "CORE-1",
		"9b31964e-dfdb-4fc3-882d-8875f4fce8611",
	} {
		if _, _, ok := shortIDRange(written); ok {
			t.Errorf("shortIDRange(%q) read a short id; want none", written)
		}
	}
}

// askRead is the shape the single-ask read answers.
type askRead struct {
	Ask model.Ask `json:"ask"`
}

// openShortIDAsk opens one ask on an issue and answers its id.
func openShortIDAsk(t *testing.T, handler http.Handler, issueKey, question string) string {
	t.Helper()
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": question,
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("open ask: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[model.Ask](t, created).ID
}

// insertAsk writes one open ask under an id of the test's choosing, with the ask.opened event
// every read resolves an ask's identity through. It is how a test makes two asks share the
// first eight hex digits of their ids: gen_random_uuid() will not.
func insertAsk(t *testing.T, database *store.Store, id, issueKey, question string) string {
	t.Helper()
	const actor = `{"kind":"session","id":"session-0123456789abcdef"}`
	if _, err := database.Pool.Exec(context.Background(),
		`insert into asks (id, issue_key, author, question) values ($1, $2, $3::jsonb, $4)`,
		id, issueKey, actor, question); err != nil {
		t.Fatalf("insert ask %s: %v", id, err)
	}
	if _, err := database.Pool.Exec(context.Background(),
		`insert into events (issue_key, seq, type, actor, payload, notify)
		 select $1, coalesce(max(seq), 0) + 1, 'ask.opened', $2::jsonb, $3::jsonb, false
		 from events where issue_key = $1`,
		issueKey, actor, `{"id":"`+id+`"}`); err != nil {
		t.Fatalf("insert ask.opened for %s: %v", id, err)
	}
	return id
}

// secondIssue creates another issue in a project some other issue already made.
func secondIssue(t *testing.T, handler http.Handler, project, title string) string {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": title, "spec": "A spec",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
}

// TestAShortIDOpensTheRecordItsFullIDOpens covers every kind a link addresses by uuid, on the
// route the dashboard and the agent client read it through.
func TestAShortIDOpensTheRecordItsFullIDOpens(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Short ids", "A spec passage")

	askID := openShortIDAsk(t, handler, issue.Key, "Which transport?")
	comment := decodeBody[model.Comment](t, dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Please clarify."}, "alice"))
	message := decodeBody[model.Message](t, sessionRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+issue.Key+"/messages", map[string]any{"body": "Shipped.", "actor": sessionActor()}))

	for _, test := range []struct {
		name   string
		target string
		full   string
	}{
		{"ask", "/api/v1/asks/", askID},
		{"ask followers", "/api/v1/asks/%s/followers", askID},
		{"comment", "/api/v1/comments/", comment.ID},
		{"comment blocks", "/api/v1/comments/%s/blocks", comment.ID},
		{"issue message", "/api/v1/issues/" + issue.Key + "/messages/", message.ID},
		{"document", "/api/v1/artifacts/", issue.PrimaryArtifactID},
		{"document blocks", "/api/v1/artifacts/%s/blocks", issue.PrimaryArtifactID},
	} {
		whole := shortIDTarget(test.target, test.full)
		short := shortIDTarget(test.target, test.full[:8])
		wholeAnswer := sessionRequest(t, handler, http.MethodGet, whole, nil)
		if wholeAnswer.Code != http.StatusOK {
			t.Fatalf("%s by its full id: status=%d body=%s", test.name, wholeAnswer.Code, wholeAnswer.Body.String())
		}
		shortAnswer := sessionRequest(t, handler, http.MethodGet, short, nil)
		if shortAnswer.Code != http.StatusOK {
			t.Errorf("%s by its short id %s: status=%d body=%s, want 200", test.name, short, shortAnswer.Code, shortAnswer.Body.String())
			continue
		}
		// The record read is the same record, not merely a request that was accepted.
		if shortAnswer.Body.String() != wholeAnswer.Body.String() {
			t.Errorf("%s by its short id answered a different record:\n short: %s\n whole: %s",
				test.name, shortAnswer.Body.String(), wholeAnswer.Body.String())
		}
	}
}

// shortIDTarget puts id into a target, which is either a prefix to append to or holds one %s.
func shortIDTarget(target, id string) string {
	if !strings.Contains(target, "%s") {
		return target + id
	}
	return strings.Replace(target, "%s", id, 1)
}

// A short id that two asks share must never open one of them: the reader is told which asks it
// could mean and nothing is guessed.
func TestAnAmbiguousShortIDIsRefusedAndNamesItsCandidates(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ambiguous short ids", "A spec")
	const shared = "5c0113de"
	firstID := insertAsk(t, database, shared+"-0000-4000-8000-000000000001", issue.Key, "Which transport?")
	secondID := insertAsk(t, database, shared+"-0000-4000-8000-000000000002", issue.Key, "Which region?")

	response := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+shared, nil)

	if response.Code != http.StatusConflict {
		t.Fatalf("ambiguous short id: status=%d body=%s, want 409", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{`"code":"AMBIGUOUS_ID"`, "2 asks", firstID, secondID} {
		if !strings.Contains(body, want) {
			t.Errorf("ambiguous refusal %s does not name %q", body, want)
		}
	}
	// Each ask still opens under its own full id.
	for _, id := range []string{firstID, secondID} {
		if answer := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+id, nil); answer.Code != http.StatusOK {
			t.Errorf("ask %s by its full id: status=%d", id, answer.Code)
		}
	}
}

// The same short id under two issues resolves under the issue the link names, and is refused
// when the caller names none.
func TestAShortIDIsResolvedWithinTheIssueTheLinkNames(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	first := createInteractionIssue(t, handler, "TEST", "First owner", "A spec")
	second := secondIssue(t, handler, "TEST", "Second owner")
	const shared = "7a2f90bc"
	firstID := insertAsk(t, database, shared+"-0000-4000-8000-00000000000a", first.Key, "First?")
	secondID := insertAsk(t, database, shared+"-0000-4000-8000-00000000000b", second, "Second?")

	for issueKey, want := range map[string]string{first.Key: firstID, second: secondID} {
		answer := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+shared+"?issue="+issueKey, nil)
		if answer.Code != http.StatusOK {
			t.Fatalf("short id under %s: status=%d body=%s", issueKey, answer.Code, answer.Body.String())
		}
		if got := decodeBody[askRead](t, answer).Ask.ID; got != want {
			t.Errorf("short id under %s opened %s, want %s", issueKey, got, want)
		}
	}
	unscoped := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+shared, nil)
	if unscoped.Code != http.StatusConflict {
		t.Errorf("the same short id with no issue named: status=%d body=%s, want 409", unscoped.Code, unscoped.Body.String())
	}
	// An issue that holds no ask under the prefix is a 404 naming that issue, never a fall back
	// to the one the other issue holds.
	elsewhere := secondIssue(t, handler, "TEST", "Third owner")
	missing := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+shared+"?issue="+elsewhere, nil)
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), elsewhere) {
		t.Errorf("short id under an issue without it: status=%d body=%s, want 404 naming %s",
			missing.Code, missing.Body.String(), elsewhere)
	}
}

// The bound on the rows a short id's range reads is a bound on the scope's own candidates: a
// crowd of asks on other issues, all sharing the prefix, must neither hide the one the link
// names nor let one of two asks on that issue read as unique.
func TestTheCandidateBoundIsAppliedWithinTheScope(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	mine := createInteractionIssue(t, handler, "TEST", "The issue the link names", "A spec")
	crowd := secondIssue(t, handler, "TEST", "Another issue entirely")
	const shared = "1d7e45aa"
	// Enough rows under the prefix, sorting before the one the link names, to fill the scan's
	// bound several times over.
	for index := range 30 {
		insertAsk(t, database, fmt.Sprintf("%s-0000-4000-8000-0000000000%02x", shared, index), crowd, "Not this one?")
	}
	wanted := insertAsk(t, database, shared+"-ffff-4000-8000-000000000001", mine.Key, "This one?")

	answer := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+shared+"?issue="+mine.Key, nil)

	if answer.Code != http.StatusOK {
		t.Fatalf("the scoped short id behind a crowd: status=%d body=%s, want 200", answer.Code, answer.Body.String())
	}
	if got := decodeBody[askRead](t, answer).Ask.ID; got != wanted {
		t.Fatalf("the scoped short id opened %s, want %s", got, wanted)
	}
	// A second ask on that issue under the same prefix, still behind the crowd, makes it
	// ambiguous within the scope rather than unique.
	insertAsk(t, database, shared+"-ffff-4000-8000-000000000002", mine.Key, "Or this one?")
	ambiguous := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+shared+"?issue="+mine.Key, nil)
	if ambiguous.Code != http.StatusConflict {
		t.Fatalf("two scoped asks behind a crowd: status=%d body=%s, want 409", ambiguous.Code, ambiguous.Body.String())
	}
}

// A route's own authorization decides before any lookup: a bearer on a human-only route reads
// its 403, never a 404 or a list of candidate ids it may not see.
func TestAShortIDOnARouteTheCallerMayNotUseIsRefusedByTheRoute(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Human-only routes", "A spec")
	const shared = "6b0c31fe"
	askID := insertAsk(t, database, shared+"-0000-4000-8000-000000000001", issue.Key, "Which transport?")
	insertAsk(t, database, shared+"-0000-4000-8000-000000000002", issue.Key, "Which region?")

	// PUT /api/v1/me/asks/{id}/snooze is human-only.
	refused := sessionRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+shared+"/snooze", map[string]any{
		"snoozed_until": "2099-01-01T00:00:00Z",
	})

	if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("a bearer on a human-only route: status=%d body=%s, want 403 HUMAN_ONLY", refused.Code, refused.Body.String())
	}
	if strings.Contains(refused.Body.String(), askID) {
		t.Fatalf("the refusal named a candidate id: %s", refused.Body.String())
	}
	// The same short id resolves for a person, whom the route admits.
	allowed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+shared+"/snooze", map[string]any{
		"snoozed_until": "2099-01-01T00:00:00Z",
	}, "alice")
	if allowed.Code != http.StatusConflict || !strings.Contains(allowed.Body.String(), `"code":"AMBIGUOUS_ID"`) {
		t.Fatalf("a person on the same route: status=%d body=%s, want 409 AMBIGUOUS_ID", allowed.Code, allowed.Body.String())
	}
}

// A short id nothing carries is the 404 the full id would have answered, and an ask on another
// issue is not reached by a short id written under this one.
func TestAShortIDThatNamesNothingIsNotFound(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Missing short ids", "A spec")
	openShortIDAsk(t, handler, issue.Key, "Which transport?")

	response := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/ffffffff", nil)

	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"NOT_FOUND"`) {
		t.Fatalf("short id naming nothing: status=%d body=%s, want 404 NOT_FOUND", response.Code, response.Body.String())
	}
}

// A write that names an ask by a short id writes to the ask that id names, and a write under an
// ambiguous one writes nothing.
func TestAWriteTakesAShortAskID(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Short ids on writes", "A spec")
	askID := openShortIDAsk(t, handler, issue.Key, "Which transport?")

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID[:8], map[string]any{
		"question": "Which transport ships first?", "actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit by short id: status=%d body=%s", edited.Code, edited.Body.String())
	}
	if got := decodeBody[model.Ask](t, edited); got.ID != askID || got.Question != "Which transport ships first?" {
		t.Fatalf("edit by short id wrote %#v, want the reworded ask %s", got, askID)
	}

	const shared = "3ce40a17"
	firstID := insertAsk(t, database, shared+"-0000-4000-8000-000000000001", issue.Key, "Which transport?")
	secondID := insertAsk(t, database, shared+"-0000-4000-8000-000000000002", issue.Key, "Which region?")
	refused := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+shared, map[string]any{
		"question": "Written to neither.", "actor": sessionActor(),
	})
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"AMBIGUOUS_ID"`) {
		t.Fatalf("edit by an ambiguous short id: status=%d body=%s, want 409 AMBIGUOUS_ID", refused.Code, refused.Body.String())
	}
	for _, id := range []string{firstID, secondID} {
		read := decodeBody[askRead](t, sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+id, nil))
		if read.Ask.Question == "Written to neither." {
			t.Errorf("the ambiguous edit reached ask %s", id)
		}
	}
}

// A reply that names its ask by a short id answers that ask: the thread's owner scopes it.
func TestACommentTakesAShortAskID(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Short ask ids on replies", "A spec")
	askID := openShortIDAsk(t, handler, issue.Key, "Which transport?")

	replied := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"ask_id": askID[:8], "body": "REST."}, "alice")

	if replied.Code != http.StatusCreated {
		t.Fatalf("reply by short ask id: status=%d body=%s", replied.Code, replied.Body.String())
	}
	if got := decodeBody[model.Comment](t, replied).AskID; got == nil || *got != askID {
		t.Fatalf("reply by short ask id hangs under %v, want %s", got, askID)
	}
}

// A block of a document opens by the first characters of its id, as an ask does, and an exact
// id a document carries always wins over a longer id it is the start of.
func TestAShortBlockIDOpensItsBlock(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Short block ids", "A heading\n\nA paragraph.")
	blocks := decodeBody[[]model.ArtifactBlock](t, sessionRequest(t, handler, http.MethodGet,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil))
	if len(blocks) == 0 {
		t.Fatalf("the spec has no blocks")
	}
	blockID := blocks[0].ID
	if _, err := uuid.Parse(blockID); err != nil {
		t.Fatalf("block id %q is not a uuid; this test's short form assumes one", blockID)
	}

	short := sessionRequest(t, handler, http.MethodGet,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks/"+blockID[:8], nil)

	if short.Code != http.StatusOK {
		t.Fatalf("block by its short id: status=%d body=%s", short.Code, short.Body.String())
	}
	if got := decodeBody[model.BlockPath](t, short).ID; got != blockID {
		t.Fatalf("block by its short id opened %s, want %s", got, blockID)
	}
	if missing := sessionRequest(t, handler, http.MethodGet,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks/ffffffff", nil); missing.Code != http.StatusNotFound {
		t.Errorf("a block id naming nothing: status=%d, want 404", missing.Code)
	}
}
