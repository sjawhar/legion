// id_prefix_test.go drives the prefix layer (resolveIDPrefixes) and the two reads that resolve a
// prefix of their own, `?to=` and the question map, against ids chosen by raw SQL: no caller may
// choose an id, and a prefix two items share is the whole point of the refusal under test.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// chosenID is a uuid whose first eight characters are prefix, told from its siblings by nth.
// Every id a prefix test reads is written with it, so which prefixes name one item, which name
// two and which name none is stated by the test rather than drawn.
func chosenID(prefix string, nth int) string {
	return fmt.Sprintf("%s-0000-4000-8000-%012d", prefix, nth)
}

// ambiguousIDAnswer is the 409 of an id prefix two items or more carry.
type ambiguousIDAnswer struct {
	Error      string           `json:"error"`
	Code       string           `json:"code"`
	Candidates []refs.Candidate `json:"candidates"`
}

// refusedAnswer is the error and code of any refusal.
type refusedAnswer struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// idPrefixFixture is one project with, for each kind an id prefix names, an item whose
// eight-character prefix names it alone and two that share another.
type idPrefixFixture struct {
	handler  http.Handler
	database *store.Store
	issue    string
	spec     string
}

// seedExec runs one seeding statement.
func seedExec(t *testing.T, database *store.Store, what, query string, args ...any) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("seed %s: %v", what, err)
	}
}

const idPrefixAuthor = `{"kind":"user","id":"alice"}`

// seedAskOpenedEvent writes the ask.opened event every ask read looks the row up through
// (events.OpenedEventIDs). The API appends it with the row; raw SQL has to write it too, or the
// read that follows fails on an ask no event opened. The sequence comes from issues.last_seq,
// which events.Append draws every event's from, so the next event the server appends to this
// issue does not land on a sequence this seed already used.
func seedAskOpenedEvent(t *testing.T, database *store.Store, issueKey, id string) {
	t.Helper()
	seedExec(t, database, "an ask's opened event", `
		with bumped as (
			update issues set last_seq = last_seq + 1 where key = $1 returning key, last_seq
		)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select key, last_seq, 'ask.opened', $2::jsonb, jsonb_build_object('id', $3::text), false
		from bumped
	`, issueKey, idPrefixAuthor, id)
}

// seedAsk writes one ask with a chosen id, as a caller cannot choose one, with its opened event.
func seedAsk(t *testing.T, database *store.Store, issueKey, id, question string) {
	t.Helper()
	seedExec(t, database, "an ask", `
		insert into asks (id, issue_key, author, question) values ($1::uuid, $2, $3::jsonb, $4)
	`, id, issueKey, idPrefixAuthor, question)
	seedAskOpenedEvent(t, database, issueKey, id)
}

// newIDPrefixFixture seeds PRE-1 with three asks, three comments, three issue messages and
// three documents: one of each under the prefix its kind's test resolves, and two under the
// prefix that names two. The ids are written here, not drawn, so "a0000001" names exactly one
// ask and "a0000002" exactly two.
func newIDPrefixFixture(t *testing.T) idPrefixFixture {
	t.Helper()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "PRE", "Short ids", "# Spec\n\nA passage.")
	fixture := idPrefixFixture{handler: handler, database: database, issue: issue.Key, spec: issue.PrimaryArtifactID}

	for nth, id := range []string{chosenID("a0000001", 1), chosenID("a0000002", 1), chosenID("a0000002", 2)} {
		seedAsk(t, database, issue.Key, id, fmt.Sprintf("Which way %d?", nth))
	}
	for nth, id := range []string{chosenID("c0000001", 1), chosenID("c0000002", 1), chosenID("c0000002", 2)} {
		seedExec(t, database, "a comment", `
			insert into comments (id, issue_key, author, body) values ($1::uuid, $2, $3::jsonb, $4)
		`, id, issue.Key, idPrefixAuthor, fmt.Sprintf("A remark %d.", nth))
	}
	for nth, id := range []string{chosenID("d0000001", 1), chosenID("d0000002", 1), chosenID("d0000002", 2)} {
		seedExec(t, database, "a message", `
			insert into messages (id, issue_key, author, body) values ($1::uuid, $2, $3::jsonb, $4)
		`, id, issue.Key, idPrefixAuthor, fmt.Sprintf("A note %d.", nth))
	}
	for nth, id := range []string{chosenID("f0000001", 1), chosenID("f0000002", 1), chosenID("f0000002", 2)} {
		slug := fmt.Sprintf("notes-%d-md", nth)
		seedExec(t, database, "a document", `
			insert into artifacts (id, issue_key, project_key, slug, name, kind, created_by)
			values ($1::uuid, $2, 'PRE', $3, $4, 'doc', $5::jsonb)
		`, id, issue.Key, slug, fmt.Sprintf("notes-%d.md", nth), idPrefixAuthor)
		seedExec(t, database, "a document version", `
			insert into artifact_versions (artifact_id, number, markdown) values ($1::uuid, 1, $2)
		`, id, fmt.Sprintf("# Notes %d\n\nA document.", nth))
	}
	return fixture
}

// idPrefixRoute is one owner-free route that takes an id of one kind in its path, with what
// its answer carries and the two answers a value that is no item of its kind gets.
type idPrefixRoute struct {
	kind string
	path func(id string) string
	// wholeID is the id the 200 carries, which proves the route acted on the resolved item.
	wholeID func(t *testing.T, response *httptest.ResponseRecorder) string
	// shortStatus and shortCode are the answer for a path value that is no id written short:
	// a seven-character stub reaches the handler as written and gets what it always got,
	// which on a route that parses its id is a 400.
	shortStatus int
	shortCode   string
	// notFoundCode is the 404 the route answers for a whole id of its kind that names no
	// item, which is what the prefix layer answers for a prefix that names none (idRoutes).
	notFoundCode string
}

func idPrefixRoutes() []idPrefixRoute {
	return []idPrefixRoute{
		{
			kind: "ask",
			path: func(id string) string { return "/api/v1/asks/" + id },
			wholeID: func(t *testing.T, response *httptest.ResponseRecorder) string {
				return decodeBody[struct {
					Ask struct {
						ID string `json:"id"`
					} `json:"ask"`
				}](t, response).Ask.ID
			},
			shortStatus:  http.StatusBadRequest,
			shortCode:    "ASK_ID_INPUT",
			notFoundCode: "NOT_FOUND",
		},
		{
			kind: "comment",
			path: func(id string) string { return "/api/v1/comments/" + id },
			wholeID: func(t *testing.T, response *httptest.ResponseRecorder) string {
				return decodeBody[struct {
					Comment struct {
						ID string `json:"id"`
					} `json:"comment"`
				}](t, response).Comment.ID
			},
			shortStatus:  http.StatusBadRequest,
			shortCode:    "COMMENT_ID_INPUT",
			notFoundCode: "NOT_FOUND",
		},
		{
			kind: "message",
			path: func(id string) string { return "/api/v1/messages/" + id },
			wholeID: func(t *testing.T, response *httptest.ResponseRecorder) string {
				return decodeBody[messageRead](t, response).Message.ID
			},
			shortStatus:  http.StatusBadRequest,
			shortCode:    "MESSAGE_ID_INPUT",
			notFoundCode: "NOT_FOUND",
		},
		{
			kind: "artifact",
			path: func(id string) string { return "/api/v1/artifacts/" + id },
			wholeID: func(t *testing.T, response *httptest.ResponseRecorder) string {
				return decodeBody[struct {
					ID string `json:"id"`
				}](t, response).ID
			},
			shortStatus:  http.StatusNotFound,
			shortCode:    "ARTIFACT_NOT_FOUND",
			notFoundCode: "ARTIFACT_NOT_FOUND",
		},
	}
}

// idPrefixKindPrefixes is each kind's three prefixes: the one that names one item, the one two
// items carry, and one nothing carries.
var idPrefixKindPrefixes = map[string]struct{ one, two, none string }{
	"ask":      {one: "a0000001", two: "a0000002", none: "a0000009"},
	"comment":  {one: "c0000001", two: "c0000002", none: "c0000009"},
	"message":  {one: "d0000001", two: "d0000002", none: "d0000009"},
	"artifact": {one: "f0000001", two: "f0000002", none: "f0000009"},
}

// TestAnIDPrefixNamesOneItemOnEveryOwnerFreeRoute: for an ask, a comment, a message and a
// document, the route that takes that kind's id in its path reads the item an eight-character
// prefix names, refuses one two items carry with 409 ID_AMBIGUOUS listing both, and splits the
// rest three ways: a seven-character value is no id written short, so it reaches the handler as
// written and gets today's answer; an eight-character prefix nothing carries gets the route's
// own 404; and a whole uuid nothing carries gets that same 404, which is the control proving
// the prefix is answered as an unknown whole id is and not by a 404 this test merely asserts.
func TestAnIDPrefixNamesOneItemOnEveryOwnerFreeRoute(t *testing.T) {
	fixture := newIDPrefixFixture(t)
	for _, route := range idPrefixRoutes() {
		prefixes := idPrefixKindPrefixes[route.kind]
		t.Run(route.kind, func(t *testing.T) {
			read := dispatchRequest(t, fixture.handler, http.MethodGet, route.path(prefixes.one), nil, "alice")
			if read.Code != http.StatusOK {
				t.Fatalf("read by the prefix %s: status=%d body=%s, want 200", prefixes.one, read.Code, read.Body.String())
			}
			if whole := route.wholeID(t, read); whole != chosenID(prefixes.one, 1) {
				t.Fatalf("the answer carries id %q, want the whole id %q", whole, chosenID(prefixes.one, 1))
			}

			shared := dispatchRequest(t, fixture.handler, http.MethodGet, route.path(prefixes.two), nil, "alice")
			if shared.Code != http.StatusConflict {
				t.Fatalf("read by the prefix %s: status=%d body=%s, want 409", prefixes.two, shared.Code, shared.Body.String())
			}
			ambiguous := decodeBody[ambiguousIDAnswer](t, shared)
			if ambiguous.Code != contracts.IDAmbiguous {
				t.Fatalf("refusal = %+v, want %s", ambiguous, contracts.IDAmbiguous)
			}
			if !strings.Contains(ambiguous.Error, prefixes.two) {
				t.Fatalf("the refusal %q does not name the prefix %s", ambiguous.Error, prefixes.two)
			}
			if len(ambiguous.Candidates) != 2 {
				t.Fatalf("candidates = %+v, want the two items %s names", ambiguous.Candidates, prefixes.two)
			}
			for nth, candidate := range ambiguous.Candidates {
				want := chosenID(prefixes.two, nth+1)
				if candidate.Kind != route.kind || candidate.ID != want {
					t.Fatalf("candidate %d = %+v, want kind %s id %s", nth, candidate, route.kind, want)
				}
				// A document is addressed by its slug, every other kind by its id.
				wantRef := "dispatch://" + fixture.issue + "/" + route.kind + "/" + want
				if route.kind == "artifact" {
					wantRef = fmt.Sprintf("dispatch://%s/artifact/notes-%d-md", fixture.issue, nth+1)
				}
				if candidate.Ref != wantRef {
					t.Fatalf("candidate %d = %+v, want the reference %s", nth, candidate, wantRef)
				}
				if candidate.IssueKey != fixture.issue || candidate.Document != "" {
					t.Fatalf("candidate %d = %+v, want the issue %s as its owner", nth, candidate, fixture.issue)
				}
				if candidate.FirstLine == "" {
					t.Fatalf("candidate %d = %+v, want the line that tells it from the other", nth, candidate)
				}
			}

			unknown := chosenID(prefixes.none, 7)
			for _, unresolved := range []struct {
				name, value string
				status      int
				code        string
			}{
				{"a seven-character value", prefixes.one[:7], route.shortStatus, route.shortCode},
				{"a prefix nothing carries", prefixes.none, http.StatusNotFound, route.notFoundCode},
				{"a whole uuid nothing carries", unknown, http.StatusNotFound, route.notFoundCode},
			} {
				answer := dispatchRequest(t, fixture.handler, http.MethodGet, route.path(unresolved.value), nil, "alice")
				if answer.Code != unresolved.status {
					t.Fatalf("%s (%s): status=%d body=%s, want %d",
						unresolved.name, unresolved.value, answer.Code, answer.Body.String(), unresolved.status)
				}
				if code := decodeBody[refusedAnswer](t, answer).Code; code != unresolved.code {
					t.Fatalf("%s (%s): code=%q, want %q", unresolved.name, unresolved.value, code, unresolved.code)
				}
			}
		})
	}
}

// TestAnAskIsAnsweredByAnIDPrefix: a write route resolves its prefix and writes the item it
// named, so the ask the prefix shortens is the ask that ends up answered.
func TestAnAskIsAnsweredByAnIDPrefix(t *testing.T) {
	fixture := newIDPrefixFixture(t)
	answered := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/asks/a0000001/answer",
		map[string]any{"text": "This way."}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer by prefix: status=%d body=%s, want 200", answered.Code, answered.Body.String())
	}
	read := dispatchRequest(t, fixture.handler, http.MethodGet, "/api/v1/asks/"+chosenID("a0000001", 1), nil, "alice")
	ask := decodeBody[struct {
		Ask struct {
			ID     string `json:"id"`
			State  string `json:"state"`
			Answer *struct {
				Text string `json:"text"`
			} `json:"answer"`
		} `json:"ask"`
	}](t, read)
	if ask.Ask.State != "answered" || ask.Ask.Answer == nil || ask.Ask.Answer.Text != "This way." {
		t.Fatalf("the ask the prefix named = %+v, want it answered \"This way.\"", ask.Ask)
	}
}

// TestABlockIDPrefixIsMatchedWithinTheDocumentItsRouteNames: the {block_id} of
// /api/v1/artifacts/{id}/blocks/{block_id} is resolved among the blocks of the document its
// {id} names, which is itself resolved from a prefix first — so a block prefix two of that
// document's blocks carry is 409 ID_AMBIGUOUS, the same prefix under another document names
// nothing, and a block id that is no id written short is matched exactly, as it always was.
func TestABlockIDPrefixIsMatchedWithinTheDocumentItsRouteNames(t *testing.T) {
	fixture := newIDPrefixFixture(t)
	document := chosenID("f0000001", 1)
	for _, block := range []struct{ document, id string }{
		{document, "ab12cd34ef"},
		{document, "ab12cd34ff"},
		{document, "b1"},
	} {
		seedExec(t, fixture.database, "an indexed block", `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, $2, $1::text || '#' || $2, 'paragraph', '', $3, 1, now())
		`, block.document, block.id, "The block "+block.id+".")
	}

	path := func(document, block string) string {
		return "/api/v1/artifacts/" + document + "/blocks/" + block
	}
	shared := dispatchRequest(t, fixture.handler, http.MethodGet, path("f0000001", "ab12cd34"), nil, "alice")
	if shared.Code != http.StatusConflict {
		t.Fatalf("a block prefix two blocks carry: status=%d body=%s, want 409", shared.Code, shared.Body.String())
	}
	ambiguous := decodeBody[ambiguousIDAnswer](t, shared)
	if ambiguous.Code != contracts.IDAmbiguous || len(ambiguous.Candidates) != 2 {
		t.Fatalf("refusal = %+v, want %s naming both blocks", ambiguous, contracts.IDAmbiguous)
	}
	for nth, candidate := range ambiguous.Candidates {
		want := []string{"ab12cd34ef", "ab12cd34ff"}[nth]
		if candidate.Kind != "block" || candidate.ID != want || candidate.FirstLine != "The block "+want+"." {
			t.Fatalf("candidate %d = %+v, want the block %s of this document", nth, candidate, want)
		}
	}

	// The same block prefix, asked of a document whose blocks do not carry it: the block scope
	// is the document the route names, so nothing resolves and the read answers as it does for
	// a block that document lacks.
	elsewhere := dispatchRequest(t, fixture.handler, http.MethodGet, path(fixture.spec, "ab12cd34"), nil, "alice")
	if elsewhere.Code != http.StatusNotFound {
		t.Fatalf("a block prefix of another document: status=%d body=%s, want 404", elsewhere.Code, elsewhere.Body.String())
	}
	exact := dispatchRequest(t, fixture.handler, http.MethodGet, path("f0000001", "b1"), nil, "alice")
	if exact.Code != http.StatusNotFound {
		t.Fatalf("a block id that is no short id: status=%d body=%s, want the 404 it always gave",
			exact.Code, exact.Body.String())
	}
}

// TestABlockPrefixUnderAUUIDSpellingTheRouteReadsIsResolvedToo: the {id} of
// /api/v1/artifacts/{id}/blocks/{block_id} is read with uuid.Parse (parseArtifactID), which
// reads `urn:uuid:<uuid>` as the document it names, so the document a block prefix is matched
// within is read the same way: that spelling answers exactly what the plain id answers - the 409
// of a prefix two of its blocks carry and the route's own 404 for the one block the live
// document does not hold - and never the 500 of a value Postgres refuses as a uuid.
func TestABlockPrefixUnderAUUIDSpellingTheRouteReadsIsResolvedToo(t *testing.T) {
	fixture := newIDPrefixFixture(t)
	document := chosenID("f0000001", 1)
	for _, blockID := range []string{"ab12cd34ef", "ab12cd34ff"} {
		seedExec(t, fixture.database, "an indexed block", `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, $2, $1::text || '#' || $2, 'paragraph', '', $3, 1, now())
		`, document, blockID, "The block "+blockID+".")
	}

	path := func(document, block string) string {
		return "/api/v1/artifacts/" + document + "/blocks/" + block
	}
	for _, prefix := range []string{"ab12cd34", "ab12cd34e"} {
		plain := dispatchRequest(t, fixture.handler, http.MethodGet, path(document, prefix), nil, "alice")
		urn := dispatchRequest(t, fixture.handler, http.MethodGet, path("urn:uuid:"+document, prefix), nil, "alice")
		if urn.Code == http.StatusInternalServerError {
			t.Fatalf("the urn spelling with the block prefix %q: status=500 body=%s", prefix, urn.Body.String())
		}
		if urn.Code != plain.Code || urn.Body.String() != plain.Body.String() {
			t.Fatalf("the urn spelling with the block prefix %q = status=%d body=%s, want the plain id's status=%d body=%s",
				prefix, urn.Code, urn.Body.String(), plain.Code, plain.Body.String())
		}
	}
}

// TestAWriteRouteThatParsesNoIDActsOnTheResolvedItem: POST /api/v1/comments/{id}/resolve reads
// its id out of the path and parses none of it, so the whole id the layer left there is the
// comment it resolves.
func TestAWriteRouteThatParsesNoIDActsOnTheResolvedItem(t *testing.T) {
	fixture := newIDPrefixFixture(t)
	resolved := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/comments/c0000001/resolve",
		map[string]any{}, "alice")
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve by prefix: status=%d body=%s, want 200", resolved.Code, resolved.Body.String())
	}
	read := dispatchRequest(t, fixture.handler, http.MethodGet, "/api/v1/comments/"+chosenID("c0000001", 1), nil, "alice")
	comment := decodeBody[struct {
		Comment struct {
			ID       string `json:"id"`
			Resolved bool   `json:"resolved"`
		} `json:"comment"`
	}](t, read)
	if !comment.Comment.Resolved {
		t.Fatalf("the comment the prefix named = %+v, want it resolved", comment.Comment)
	}
}

// TestAProjectScopedTokenIsRefusedAPrefixOfAnotherProjectsAsk: the project check runs behind the
// prefix layer, so it judges the item the prefix named. A credential scoped to one project is
// refused another project's ask by its prefix exactly as it is by its whole id, and the refusal
// names both projects.
func TestAProjectScopedTokenIsRefusedAPrefixOfAnotherProjectsAsk(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-scoped-to-one-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTH", "Other")

	const prefix = "b0000001"
	id := chosenID(prefix, 1)
	seedAsk(t, rig.database, other.issue, id, "Which way?")

	body := map[string]any{"kind": "resolved", "reason": "Answered elsewhere."}
	for _, spelling := range []struct{ name, written string }{{"the whole id", id}, {"its prefix", prefix}} {
		name, written := spelling.name, spelling.written
		refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/asks/"+written+"/resolve", token, body))
		if refused.Code != http.StatusForbidden {
			t.Fatalf("%s: status=%d body=%s, want 403", name, refused.Code, refused.Body.String())
		}
		refusal := decodeBody[refusalAnswer](t, refused)
		if refusal.Code != codeProjectScope {
			t.Fatalf("%s: refusal = %+v, want %s", name, refusal, codeProjectScope)
		}
		if !strings.Contains(refusal.Error, brokerTestProject) || !strings.Contains(refusal.Error, other.key) {
			t.Fatalf("%s: refusal %q names neither %s nor %s", name, refusal.Error, brokerTestProject, other.key)
		}
	}
}

// TestADirectMessagePrefixKeepsTheConversationGuard: a prefix is looked for among the same
// messages a whole id is, and the route then applies its own rule to what it found — so a
// session not in the conversation is refused by the prefix exactly as by the whole id, and a
// refusal listing such a message carries no line of its text for that caller.
func TestADirectMessagePrefixKeepsTheConversationGuard(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	for nth, id := range []string{chosenID("e0000001", 1), chosenID("e0000002", 1), chosenID("e0000002", 2)} {
		seedExec(t, database, "a direct message", `
			insert into messages (id, issue_key, author, body, target)
			values ($1::uuid, null, $2::jsonb, $3, 'session:s1')
		`, id, idPrefixAuthor, fmt.Sprintf("Where is the dashboard %d?", nth))
	}

	for _, spelling := range []struct{ name, written string }{{"the whole id", chosenID("e0000001", 1)}, {"its prefix", "e0000001"}} {
		name, written := spelling.name, spelling.written
		refused := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/"+written+"?session=s2", nil)
		if refused.Code != http.StatusForbidden || decodeBody[refusedAnswer](t, refused).Code != "THREAD_FORBIDDEN" {
			t.Fatalf("s2 reading s1's conversation by %s: status=%d body=%s, want 403 THREAD_FORBIDDEN",
				name, refused.Code, refused.Body.String())
		}
		if strings.Contains(refused.Body.String(), "Where is the dashboard") {
			t.Fatalf("s2 reading s1's conversation by %s carried its text: %s", name, refused.Body.String())
		}
	}

	outside := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/e0000002?session=s2", nil)
	if outside.Code != http.StatusConflict {
		t.Fatalf("an ambiguous prefix as s2: status=%d body=%s, want 409", outside.Code, outside.Body.String())
	}
	ambiguous := decodeBody[ambiguousIDAnswer](t, outside)
	if ambiguous.Code != contracts.IDAmbiguous || len(ambiguous.Candidates) != 2 {
		t.Fatalf("refusal = %+v, want %s with both messages", ambiguous, contracts.IDAmbiguous)
	}
	for nth, candidate := range ambiguous.Candidates {
		if candidate.FirstLine != "" {
			t.Fatalf("candidate %d = %+v, want no first line for a conversation s2 is not in", nth, candidate)
		}
		if candidate.Ref != "" || candidate.IssueKey != "" || candidate.Document != "" {
			t.Fatalf("candidate %d = %+v, want a direct message's empty address and owner", nth, candidate)
		}
	}

	person := dispatchRequest(t, handler, http.MethodGet, "/api/v1/messages/e0000002", nil, "alice")
	if person.Code != http.StatusConflict {
		t.Fatalf("an ambiguous prefix as a person: status=%d body=%s, want 409", person.Code, person.Body.String())
	}
	for nth, candidate := range decodeBody[ambiguousIDAnswer](t, person).Candidates {
		if candidate.FirstLine == "" {
			t.Fatalf("candidate %d = %+v, want the line a person reads to tell them apart", nth, candidate)
		}
	}
}

// TestAReferenceResolvesAPrefixWithinTheOwnerItNames: `?to=` reads a short id as the one item of
// the owner the reference names, and the same prefix under another owner names nothing — while a
// whole id is read whatever owner the reference names, as this route has always read one.
func TestAReferenceResolvesAPrefixWithinTheOwnerItNames(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	first := createInteractionIssue(t, handler, "REF", "Short ids in references", "# Spec\n\nA passage.")
	second := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "REF", "title": "The other issue", "spec": "# Spec\n\nAnother passage.",
	}, "alice")
	if second.Code != http.StatusCreated {
		t.Fatalf("create the second issue: status=%d body=%s", second.Code, second.Body.String())
	}
	other := decodeBody[struct {
		Key string `json:"key"`
	}](t, second).Key

	id := chosenID("ab000001", 1)
	seedAsk(t, database, first.Key, id, "Which way?")

	node := func(reference string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?to="+reference, nil, "alice")
	}
	owned := node("dispatch://" + first.Key + "/ask/ab000001")
	if owned.Code != http.StatusOK {
		t.Fatalf("a prefix within its owner: status=%d body=%s, want 200", owned.Code, owned.Body.String())
	}
	if answered := decodeBody[struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
	}](t, owned).Node.ID; answered != id {
		t.Fatalf("the node carries id %q, want the whole id %q", answered, id)
	}

	elsewhere := node("dispatch://" + other + "/ask/ab000001")
	if elsewhere.Code != http.StatusNotFound {
		t.Fatalf("the same prefix under another owner: status=%d body=%s, want 404", elsewhere.Code, elsewhere.Body.String())
	}

	whole := node("dispatch://" + other + "/ask/" + id)
	if whole.Code != http.StatusOK {
		t.Fatalf("the whole id under another owner: status=%d body=%s, want the 200 this route has always given",
			whole.Code, whole.Body.String())
	}
}

// TestAReferenceResolvesAPrefixWithinTheProjectDocumentItNames: an ask and a comment written on
// a project document are owned by that document, and the reference names it
// (`dispatch://<PROJECT>/artifact/<slug>/ask/<id>`), so `?to=` reads a short id as the one item
// of that document carrying it. The same prefix on another document of the project, and under
// the project's issue, names that owner's item or nothing at all.
func TestAReferenceResolvesAPrefixWithinTheProjectDocumentItNames(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "PDR", "Short ids on a document", "# Spec\n\nA passage.")
	handbook := createProjectDocument(t, handler, "PDR", "Handbook", "# Handbook\n")
	notes := createProjectDocument(t, handler, "PDR", "Notes", "# Notes\n")

	// One ask under the prefix on each document, so the prefix names one item within either and
	// two across the project; one comment on the handbook, and one ask of the issue under the
	// comment's prefix, which no document owns.
	handbookAsk, notesAsk := chosenID("ba000001", 1), chosenID("ba000001", 2)
	handbookComment, issueAsk := chosenID("bc000001", 1), chosenID("bc000001", 2)
	for _, item := range []struct{ id, artifact, question string }{
		{handbookAsk, handbook.ID, "Which vendor?"},
		{notesAsk, notes.ID, "Which region?"},
	} {
		seedExec(t, database, "a document ask", `
			insert into asks (id, artifact_id, author, question) values ($1::uuid, $2::uuid, $3::jsonb, $4)
		`, item.id, item.artifact, idPrefixAuthor, item.question)
	}
	seedExec(t, database, "a document comment", `
		insert into comments (id, artifact_id, author, body) values ($1::uuid, $2::uuid, $3::jsonb, $4)
	`, handbookComment, handbook.ID, idPrefixAuthor, "A remark on the handbook.")
	seedAsk(t, database, issue.Key, issueAsk, "Which way?")

	node := func(reference string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?to="+reference, nil, "alice")
	}
	answeredID := func(response *httptest.ResponseRecorder) string {
		t.Helper()
		return decodeBody[struct {
			Node struct {
				ID string `json:"id"`
			} `json:"node"`
		}](t, response).Node.ID
	}
	for _, test := range []struct {
		name, reference, want string
	}{
		{"an ask of the handbook", "dispatch://PDR/artifact/" + handbook.Slug + "/ask/ba000001", handbookAsk},
		{"an ask of the notes", "dispatch://PDR/artifact/" + notes.Slug + "/ask/ba000001", notesAsk},
		{"a comment of the handbook", "dispatch://PDR/artifact/" + handbook.Slug + "/comment/bc000001", handbookComment},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := node(test.reference)
			if response.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s, want 200", test.name, response.Code, response.Body.String())
			}
			if answered := answeredID(response); answered != test.want {
				t.Fatalf("%s: the node carries id %q, want the whole id %q", test.name, answered, test.want)
			}
		})
	}

	// The same prefix under another owner: the issue's ask carries `bc000001`, and the handbook
	// holds no ask of it.
	elsewhere := node("dispatch://PDR/artifact/" + handbook.Slug + "/ask/bc000001")
	if elsewhere.Code != http.StatusNotFound {
		t.Fatalf("a prefix only the issue's ask carries: status=%d body=%s, want 404",
			elsewhere.Code, elsewhere.Body.String())
	}
	// And the issue owns neither document's item, so the document's prefix names nothing there.
	onTheIssue := node("dispatch://" + issue.Key + "/ask/ba000001")
	if onTheIssue.Code != http.StatusNotFound {
		t.Fatalf("a document's prefix under the issue: status=%d body=%s, want 404",
			onTheIssue.Code, onTheIssue.Body.String())
	}
	// A whole id is read whatever owner the reference names, as this route has always read one.
	whole := node("dispatch://PDR/artifact/" + notes.Slug + "/ask/" + handbookAsk)
	if whole.Code != http.StatusOK || answeredID(whole) != handbookAsk {
		t.Fatalf("the whole id under another document: status=%d body=%s, want the 200 this route has always given",
			whole.Code, whole.Body.String())
	}
}

// TestTheQuestionMapRouteAnswersEverySourceKind: a `#id` in one source's text resolves against
// the question map of the document that text belongs to, and the route answers that map for a
// source of each of the four kinds, by whole id and by prefix; a source whose text belongs to no
// document is 404.
func TestTheQuestionMapRouteAnswersEverySourceKind(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "QMP", "A spec with a question", "# Spec\n\nA passage.")
	// The create path seeds the spec through the document service and queues that document's
	// settlement once its transaction commits, after the response (issue_create.go,
	// Docs.ScheduleSettlement); a settlement writes a version of its own wherever the document
	// reads differently from the newest one. Wait for it here, because without this wait it can
	// commit after the version seeded below, which the document does not hold: it then writes
	// the create path's spec over that version, and the newest version is a spec with no
	// `:::ask{#q1}` block: the question map is empty and every read in this test answers 200
	// with no questions.
	awaitSettled(t, database, issue.PrimaryArtifactID)
	seedExec(t, database, "a version declaring a question block", `
		insert into artifact_versions (artifact_id, number, markdown)
		values ($1::uuid, (select coalesce(max(number), 0) + 1 from artifact_versions where artifact_id = $1::uuid), $2)
	`, issue.PrimaryArtifactID, "# Spec\n\n:::ask{#q1}\nWhich way?\n:::\n")

	blockAsk := chosenID("ba000001", 1)
	seedExec(t, database, "the ask settled for the question block", `
		insert into asks (id, issue_key, author, question, block_artifact_id, block_id)
		values ($1::uuid, $2, $3::jsonb, 'Which way?', $4::uuid, 'q1')
	`, blockAsk, issue.Key, idPrefixAuthor, issue.PrimaryArtifactID)
	seedAskOpenedEvent(t, database, issue.Key, blockAsk)
	comment := chosenID("bc000001", 1)
	seedExec(t, database, "a comment of the issue", `
		insert into comments (id, issue_key, author, body) values ($1::uuid, $2, $3::jsonb, 'A remark.')
	`, comment, issue.Key, idPrefixAuthor)
	message := chosenID("bd000001", 1)
	seedExec(t, database, "a message of the issue", `
		insert into messages (id, issue_key, author, body) values ($1::uuid, $2, $3::jsonb, 'A note.')
	`, message, issue.Key, idPrefixAuthor)

	type questionsAnswer struct {
		Document  string `json:"document"`
		Questions []struct {
			BlockID   string `json:"block_id"`
			Ask       string `json:"ask"`
			Reference string `json:"reference"`
		} `json:"questions"`
	}
	askReference := "dispatch://" + issue.Key + "/ask/" + blockAsk
	for _, source := range []struct{ kind, id, prefix string }{
		{"artifact", issue.PrimaryArtifactID, issue.PrimaryArtifactID[:8]},
		{"ask", blockAsk, "ba000001"},
		{"comment", comment, "bc000001"},
		{"message", message, "bd000001"},
	} {
		for _, spelling := range []struct{ name, written string }{{"the whole id", source.id}, {"its prefix", source.prefix}} {
			name, written := spelling.name, spelling.written
			read := dispatchRequest(t, handler, http.MethodGet,
				"/api/v1/references/questions?kind="+source.kind+"&id="+written, nil, "alice")
			if read.Code != http.StatusOK {
				t.Fatalf("%s by %s: status=%d body=%s, want 200", source.kind, name, read.Code, read.Body.String())
			}
			answer := decodeBody[questionsAnswer](t, read)
			if answer.Document != issue.PrimaryArtifactID {
				t.Fatalf("%s by %s: document=%q, want the issue's spec %q", source.kind, name, answer.Document, issue.PrimaryArtifactID)
			}
			if len(answer.Questions) != 1 || answer.Questions[0].BlockID != "q1" || answer.Questions[0].Ask != blockAsk ||
				answer.Questions[0].Reference != askReference {
				t.Fatalf("%s by %s: questions=%+v, want q1 naming %s at %s", source.kind, name, answer.Questions, blockAsk, askReference)
			}
		}
	}

	direct := chosenID("be000001", 1)
	seedExec(t, database, "a direct message", `
		insert into messages (id, issue_key, author, body, target)
		values ($1::uuid, null, $2::jsonb, 'Where is the dashboard?', 'session:s1')
	`, direct, idPrefixAuthor)
	missing := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/references/questions?kind=message&id=be000001", nil, "alice")
	if missing.Code != http.StatusNotFound || decodeBody[refusedAnswer](t, missing).Code != "NOT_FOUND" {
		t.Fatalf("a source whose text belongs to no document: status=%d body=%s, want 404 NOT_FOUND",
			missing.Code, missing.Body.String())
	}

	wrongKind := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/references/questions?kind=issue&id="+issue.PrimaryArtifactID, nil, "alice")
	if wrongKind.Code != http.StatusBadRequest || decodeBody[refusedAnswer](t, wrongKind).Code != "INVALID_KIND" {
		t.Fatalf("a kind that is no reference source: status=%d body=%s, want 400 INVALID_KIND",
			wrongKind.Code, wrongKind.Body.String())
	}
}

// The id the question map takes arrives in the query string, which the prefix layer never
// touches: that layer rewrites path values alone (resolveIDPrefixes), so the canonical
// spelling of one written another of uuid.Parse's ways is the read's own to bind
// (refs.ScopeOf, canonicalUUID), the one place it is canonicalized. Without it,
// `urn:uuid:<uuid>` reaches a uuid column as written, Postgres refuses it ("invalid input
// syntax for type uuid", SQLSTATE 22P02) and the read answers 500 where the canonical id
// answers the document; the braced spelling, which Postgres does read, answers that document
// under a name in braces rather than the id every other read of it carries. Every spelling of
// one id answers what the canonical id answers, and one nothing carries this route's own 404.
func TestTheQuestionMapRouteReadsEveryUuidSpellingAsTheCanonicalID(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "QMU", "A spec with a question", "# Spec\n\nA passage.")
	// Wait for the settlement the create path queues for this spec, as the seed below writes a
	// version over it; TestTheQuestionMapRouteAnswersEverySourceKind's comment above says why a
	// settlement that commits later empties the question map this test reads.
	awaitSettled(t, database, issue.PrimaryArtifactID)
	seedExec(t, database, "a version declaring a question block", `
		insert into artifact_versions (artifact_id, number, markdown)
		values ($1::uuid, (select coalesce(max(number), 0) + 1 from artifact_versions where artifact_id = $1::uuid), $2)
	`, issue.PrimaryArtifactID, "# Spec\n\n:::ask{#q1}\nWhich way?\n:::\n")
	blockAsk := chosenID("ca000001", 1)
	seedExec(t, database, "the ask settled for the question block", `
		insert into asks (id, issue_key, author, question, block_artifact_id, block_id)
		values ($1::uuid, $2, $3::jsonb, 'Which way?', $4::uuid, 'q1')
	`, blockAsk, issue.Key, idPrefixAuthor, issue.PrimaryArtifactID)
	seedAskOpenedEvent(t, database, issue.Key, blockAsk)

	type questionsAnswer struct {
		Document  string `json:"document"`
		Questions []struct {
			BlockID string `json:"block_id"`
			Ask     string `json:"ask"`
		} `json:"questions"`
	}
	questions := func(kind, id string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodGet,
			"/api/v1/references/questions?kind="+kind+"&id="+url.QueryEscape(id), nil, "alice")
	}
	for _, test := range []struct{ name, kind, id string }{
		{"the spec written urn:uuid:", "artifact", "urn:uuid:" + issue.PrimaryArtifactID},
		{"the spec written in braces", "artifact", "{" + issue.PrimaryArtifactID + "}"},
		{"the block's ask written urn:uuid:", "ask", "urn:uuid:" + blockAsk},
		{"the block's ask written in braces", "ask", "{" + blockAsk + "}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			read := questions(test.kind, test.id)
			if read.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s, want the 200 the canonical id answers",
					test.name, read.Code, read.Body.String())
			}
			answer := decodeBody[questionsAnswer](t, read)
			if answer.Document != issue.PrimaryArtifactID {
				t.Fatalf("%s: document=%q, want the canonical id of the issue's spec %q",
					test.name, answer.Document, issue.PrimaryArtifactID)
			}
			if len(answer.Questions) != 1 || answer.Questions[0].BlockID != "q1" || answer.Questions[0].Ask != blockAsk {
				t.Fatalf("%s: questions=%+v, want q1 naming %s", test.name, answer.Questions, blockAsk)
			}
		})
	}

	const unknown = "00000000-0000-4000-8000-000000000000"
	for _, written := range []string{unknown, "urn:uuid:" + unknown, "{" + unknown + "}"} {
		missing := questions("ask", written)
		if missing.Code != http.StatusNotFound || decodeBody[refusedAnswer](t, missing).Code != "NOT_FOUND" {
			t.Fatalf("an ask nothing carries, written %q: status=%d body=%s, want 404 NOT_FOUND",
				written, missing.Code, missing.Body.String())
		}
	}
}

// `?to=` and `?from=` carry the other query-string id of these two routes, an item id inside a
// dispatch:// reference. A spelling other than the canonical one is no 500 there: the graph
// keys an item node by the id as text (loadAskNodes matches on `id::text`), so nothing binds
// the value to a uuid column, and a reference naming no node gets the 404 this route answers
// for one. The canonical id, which the graph does hold, answers its node.
func TestAReferenceWritingAnIDAnotherWayIsNoFiveHundred(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "URN", "An ask a reference names", "# Spec\n\nA passage.")
	id := chosenID("cb000001", 1)
	seedAsk(t, database, issue.Key, id, "Which way?")
	for _, direction := range []string{"to", "from"} {
		for _, test := range []struct {
			written string
			want    int
		}{
			{written: id, want: http.StatusOK},
			{written: "urn:uuid:" + id, want: http.StatusNotFound},
			{written: "{" + id + "}", want: http.StatusNotFound},
		} {
			reference := "dispatch://" + issue.Key + "/ask/" + test.written
			response := dispatchRequest(t, handler, http.MethodGet,
				"/api/v1/references?"+direction+"="+url.QueryEscape(reference), nil, "alice")
			if response.Code != test.want {
				t.Errorf("?%s=%s: status=%d body=%s, want %d",
					direction, reference, response.Code, response.Body.String(), test.want)
			}
		}
	}
}

// unknownIDRouteProbe is what one resolving route needs beyond its path to reach its read of
// the id: the body it refuses a request without, and whether the route is an agent's alone, so
// a person's cookie is turned away before the row is read. The body is written here rather than
// drawn from routeProbeBody because what this test needs is the opposite of that one's probe: a
// request complete in everything but the id it names.
type unknownIDRouteProbe struct {
	body   any
	bearer bool
}

// unknownIDRouteProbes is each resolving route whose default probe - routeProbeBody as a cookie
// caller - is refused before the route reads the id, with what it takes to reach that read.
// Every other row is driven with that default, and no row is left out of the test.
var unknownIDRouteProbes = map[string]unknownIDRouteProbe{
	"PATCH /api/v1/asks/{id}":                                {body: map[string]any{"question": "Which way?"}},
	"POST /api/v1/asks/{id}/resolve":                         {body: map[string]any{"kind": "resolved", "reason": "Settled elsewhere."}},
	"PUT /api/v1/me/asks/{id}/snooze":                        {body: map[string]any{"snoozed_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}},
	"PATCH /api/v1/comments/{id}":                            {body: map[string]any{"body": "A remark."}},
	"POST /api/v1/comments/{id}/deliveries":                  {body: map[string]any{"actor": sessionActor(), "delivery": "btw"}},
	"POST /api/v1/comments/{id}/reply":                       {body: map[string]any{"actor": sessionActor(), "attempt": 1, "body": "A reply."}, bearer: true},
	"POST /api/v1/messages/{id}/deliveries":                  {body: map[string]any{"actor": sessionActor(), "delivery": "btw"}},
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept": {bearer: true},
	"POST /api/v1/messages/{id}/reply":                       {body: map[string]any{"actor": sessionActor(), "attempt": 1, "body": "A reply."}, bearer: true},
	"POST /api/v1/artifacts/{id}/rebuild":                    {body: map[string]any{}},
	"POST /api/v1/artifacts/{id}/versions":                   {body: map[string]any{"summary": "A named version"}},
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep":     {body: map[string]any{"text_sha256": strings.Repeat("0", 64)}},
	"PUT /api/v1/me/documents/{id}/read":                     {body: map[string]any{"version": 1}},
}

// unknownIDRouteRequest sends one probe of a resolving route: a cookie caller, or the agent
// bearer the three routes a person may not call need (routes_table_test's "bearer" credential).
func unknownIDRouteRequest(t *testing.T, handler http.Handler, method, path string, probe unknownIDRouteProbe, fallback string) *httptest.ResponseRecorder {
	t.Helper()
	if !probe.bearer {
		if probe.body != nil {
			return dispatchRequest(t, handler, method, path, probe.body, "alice")
		}
		return dispatchRequest(t, handler, method, path, json.RawMessage(fallback), "alice")
	}
	encoded := []byte(fallback)
	if probe.body != nil {
		var err error
		if encoded, err = json.Marshal(probe.body); err != nil {
			t.Fatalf("encode the probe of %s %s: %v", method, path, err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// idRoutes names, per route, the 404 that route answers for an id of its kind it does not hold,
// and the prefix layer answers a prefix that names none with that code, copied there by hand
// (resolvePathValue). This drives every resolving row with a whole uuid nothing carries - which
// the layer leaves as written (text.IsIDPrefix), so the answer is the handler's own - and holds
// what the handler writes to what the table claims, the {block_id} of the two block routes with
// a document that exists and a block id nothing carries. A row whose code is wrong fails by its
// own name. The one route that answers no 404 for an id it does not hold (notFoundNone) is held
// to that: its answer is anything but a 404.
func TestEveryResolvingIDRouteAnswersTheNotFoundItNames(t *testing.T) {
	// Every route the table mounts, the test hooks' one that takes a document id included.
	handler, _ := newTestHandlerWithTestHooks(t)
	issue := createInteractionIssue(t, handler, "TEST", "Unknown ids", "# Spec\n\nA passage.")
	unknown := chosenID("e0000009", 7)
	fill := func(pattern, id, blockID string) string {
		path := strings.ReplaceAll(pattern, "/issues/{key}", "/issues/"+issue.Key)
		path = strings.ReplaceAll(path, "{id}", id)
		path = strings.ReplaceAll(path, "{block_id}", blockID)
		path = strings.ReplaceAll(path, "{session_id}", "s1")
		// A grant id that is a whole uuid and no grant, so the delete reads the {id} beside it
		// rather than answering GRANT_NOT_FOUND for a value uuid.Parse refuses.
		path = strings.ReplaceAll(path, "{grant_id}", "00000000-0000-4000-8000-00000000000a")
		path = strings.ReplaceAll(path, "{number}", "1")
		return strings.ReplaceAll(path, "{attempt}", "1")
	}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		name := route.Method + " " + route.Pattern
		entry, named := idRoutes[name]
		if !named || !entry.resolves() {
			continue
		}
		t.Run(name, func(t *testing.T) {
			probe := unknownIDRouteProbes[name]
			// Each wildcard the row resolves, with every other value naming something that
			// exists: the {id} against a document that is there for the {block_id}'s probe.
			probes := []struct{ what, path, want string }{}
			if entry.Kind != "" {
				probes = append(probes, struct{ what, path, want string }{
					"an id", fill(route.Pattern, unknown, "b1"), entry.NotFound,
				})
			}
			if entry.Blocks {
				probes = append(probes, struct{ what, path, want string }{
					"a block id", fill(route.Pattern, issue.PrimaryArtifactID, unknown), entry.BlockNotFound,
				})
			}
			for _, each := range probes {
				answer := unknownIDRouteRequest(t, handler, route.Method, each.path, probe, routeProbeBody(route))
				if each.want == notFoundNone {
					if answer.Code == http.StatusNotFound {
						t.Fatalf("%s answers 404 %s for %s nothing carries; idRoutes says it answers none: %s",
							name, decodeBody[refusedAnswer](t, answer).Code, each.what, answer.Body.String())
					}
					continue
				}
				if answer.Code != http.StatusNotFound {
					t.Fatalf("%s answers %d for %s nothing carries, not the 404 idRoutes names (%s): %s",
						name, answer.Code, each.what, each.want, answer.Body.String())
				}
				if code := decodeBody[refusedAnswer](t, answer).Code; code != each.want {
					t.Fatalf("%s answers 404 %q for %s nothing carries; idRoutes names %q, which is what a prefix nothing carries gets",
						name, code, each.what, each.want)
				}
			}
		})
	}
}
