package refs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// prefixFixture opens a database, begins the transaction every assertion of a test reads, rolled
// back when the test ends, and creates the project and issue its items hang on.
func prefixFixture(t *testing.T) pgx.Tx {
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

// refusedQueryer fails the test it belongs to if it is asked anything: a prefix that can start no
// id is answered without a query.
type refusedQueryer struct{ t *testing.T }

func (q refusedQueryer) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.t.Helper()
	q.t.Errorf("a prefix that matches nothing asked the database: %s", sql)
	return nil
}

func (q refusedQueryer) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	q.t.Helper()
	q.t.Errorf("a prefix that matches nothing asked the database: %s", sql)
	return nil, errors.New("the database must not be asked")
}

// insertPrefixComment writes a comment of issueKey under the id the test chose, as every fixture
// of this file writes the id it then resolves a prefix of.
func insertPrefixComment(t *testing.T, tx pgx.Tx, id, issueKey, body string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into comments (id, issue_key, author, body)
		values ($1::uuid, $2, '{"kind":"user","id":"alice"}', $3)
	`, id, issueKey, body); err != nil {
		t.Fatalf("insert comment %s: %v", id, err)
	}
}

// insertPrefixDocumentComment writes a comment of a document under the id the test chose, the
// other owner a comment has (comments_one_owner).
func insertPrefixDocumentComment(t *testing.T, tx pgx.Tx, id, artifactID, body string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into comments (id, artifact_id, author, body)
		values ($1::uuid, $2::uuid, '{"kind":"user","id":"alice"}', $3)
	`, id, artifactID, body); err != nil {
		t.Fatalf("insert document comment %s: %v", id, err)
	}
}

// insertPrefixAsk writes an ask under the id the test chose, of an issue or of a document
// (asks_one_owner: one of the two).
func insertPrefixAsk(t *testing.T, tx pgx.Tx, id string, issueKey, artifactID *string, question string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into asks (id, issue_key, artifact_id, author, question)
		values ($1::uuid, $2, $3::uuid, '{"kind":"user","id":"alice"}', $4)
	`, id, issueKey, artifactID, question); err != nil {
		t.Fatalf("insert ask %s: %v", id, err)
	}
}

// insertPrefixMessage writes a message under the id the test chose: of an issue, or, with no
// issue key, of the direct conversation target names, as a reply to inReplyTo when it is set.
func insertPrefixMessage(t *testing.T, tx pgx.Tx, id string, issueKey *string, author, body string, target *string, inReplyTo *string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into messages (id, issue_key, author, body, target, in_reply_to)
		values ($1::uuid, $2, $3::jsonb, $4, $5, $6::uuid)
	`, id, issueKey, author, body, target, inReplyTo); err != nil {
		t.Fatalf("insert message %s: %v", id, err)
	}
}

// insertPrefixDocument writes a document under the id the test chose, of an issue or of a
// project, and returns its ref_key, the address a Scope names it by.
func insertPrefixDocument(t *testing.T, tx pgx.Tx, id string, issueKey *string, project, slug, name string, primary bool) string {
	t.Helper()
	var refKey string
	if err := tx.QueryRow(context.Background(), `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1::uuid, $2, $3, $4, $5, 'doc', $6, '{"kind":"user","id":"alice"}')
		returning ref_key
	`, id, issueKey, project, slug, name, primary).Scan(&refKey); err != nil {
		t.Fatalf("insert document %s: %v", slug, err)
	}
	return refKey
}

// insertPrefixBlock writes one block of a document's latest indexed version, as IndexBlocks
// writes it.
func insertPrefixBlock(t *testing.T, tx pgx.Tx, artifactID, refKey, blockID, excerpt string, version int) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, $2, $3, 'paragraph', '', $4, $5, now())
	`, artifactID, blockID, refKey+"#"+blockID, excerpt, version); err != nil {
		t.Fatalf("insert block %s: %v", blockID, err)
	}
}

// insertPrefixBlockText records a block's text at one version, nil where that version deleted it,
// and records the version indexed, as IndexBlocks records both.
func insertPrefixBlockText(t *testing.T, tx pgx.Tx, artifactID, blockID string, version int, body *string) {
	t.Helper()
	ctx := context.Background()
	if _, err := tx.Exec(ctx, `
		insert into document_block_texts (artifact_id, block_id, version, text) values ($1::uuid, $2, $3, $4)
	`, artifactID, blockID, version, body); err != nil {
		t.Fatalf("record block %s at version %d: %v", blockID, version, err)
	}
	if _, err := tx.Exec(ctx, `
		insert into document_indexed_versions (artifact_id, version) values ($1::uuid, $2) on conflict do nothing
	`, artifactID, version); err != nil {
		t.Fatalf("record version %d indexed: %v", version, err)
	}
}

// resolvedPrefix is the candidate prefix names, failing the test when it names none or several.
func resolvedPrefix(t *testing.T, tx pgx.Tx, kind string, scope Scope, prefix string, viewer Viewer) Candidate {
	t.Helper()
	candidate, err := ResolvePrefix(context.Background(), tx, kind, scope, prefix, viewer)
	if err != nil {
		t.Fatalf("resolve the %s prefix %q in %#v: %v", kind, prefix, scope, err)
	}
	return candidate
}

// requireNoPrefixMatch fails the test unless prefix names nothing, the one no-match signal.
func requireNoPrefixMatch(t *testing.T, q Queryer, kind string, scope Scope, prefix string, viewer Viewer) {
	t.Helper()
	candidate, err := ResolvePrefix(context.Background(), q, kind, scope, prefix, viewer)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the %s prefix %q in %#v = %#v, %v; want pgx.ErrNoRows", kind, prefix, scope, candidate, err)
	}
}

// A prefix of the one id that starts with it names that item, read without case; a prefix no id
// starts with names nothing, and so does one that can start no id at all - shorter than eight
// characters, a hyphen where a uuid has a hex digit, or a whole id, which needs no resolving -
// each answered without a query.
func TestResolvePrefixNamesTheOneItemAndRefusesWhatCanStartNoID(t *testing.T) {
	tx := prefixFixture(t)
	const id = "3b8e2d01-2a5f-4b3c-8d4e-6f70819a2b3c"
	insertPrefixComment(t, tx, id, "CORE-1", "Ship it.\nAnd then the rest.")
	want := Candidate{
		Kind: "comment", ID: id, Ref: "dispatch://CORE-1/comment/" + id,
		IssueKey: "CORE-1", FirstLine: "Ship it.",
	}
	for _, prefix := range []string{"3b8e2d01", "3B8E2D01", "3b8e2d01-2a5f", "3b8e2d01-2a5f-4b3c-8d4e-6f70819a2b3"} {
		if got := resolvedPrefix(t, tx, "comment", Scope{}, prefix, Viewer{}); !reflect.DeepEqual(got, want) {
			t.Fatalf("the comment %q names = %#v, want %#v", prefix, got, want)
		}
	}
	requireNoPrefixMatch(t, tx, "comment", Scope{}, "deadbeef", Viewer{})
	refused := refusedQueryer{t: t}
	for _, prefix := range []string{"3b8e2d0", "3b8e-2d01", "3b8e2d01-2a5f-4b3c-8d4e-6f70819a2b3c", "3b8e2d012a5f4b3c8d4e6f70819a2b3c"} {
		requireNoPrefixMatch(t, refused, "comment", Scope{}, prefix, Viewer{})
	}
}

// A prefix two items carry is refused with both listed, in id order, each with its reference, its
// owner and its first line, so a person can tell them apart and write more of the id.
func TestResolvePrefixRefusesAnAmbiguousPrefixWithItsCandidates(t *testing.T) {
	tx := prefixFixture(t)
	const first = "4c9f3e12-3b60-4c4d-9e5f-70819a2b3c4d"
	const second = "4c9f3e12-3b60-4c4d-9e5f-70819a2b3c9f"
	insertPrefixComment(t, tx, second, "CORE-1", "The second body.")
	insertPrefixComment(t, tx, first, "CORE-1", "The first body.")
	_, err := ResolvePrefix(context.Background(), tx, "comment", Scope{}, "4c9f3e12", Viewer{Human: true})
	var ambiguous *ErrPrefixAmbiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("two comments under one prefix = %v, want ErrPrefixAmbiguous", err)
	}
	want := []Candidate{
		{Kind: "comment", ID: first, Ref: "dispatch://CORE-1/comment/" + first, IssueKey: "CORE-1", FirstLine: "The first body."},
		{Kind: "comment", ID: second, Ref: "dispatch://CORE-1/comment/" + second, IssueKey: "CORE-1", FirstLine: "The second body."},
	}
	if ambiguous.Prefix != "4c9f3e12" || !reflect.DeepEqual(ambiguous.Candidates, want) {
		t.Fatalf("the refusal = %#v, want the prefix with %#v", ambiguous, want)
	}
	if message := ambiguous.Error(); !strings.Contains(message, "4c9f3e12") || !strings.Contains(message, "2") {
		t.Fatalf("the refusal reads %q, want the prefix and the count in it", message)
	}
}

// A lookup that names an owner reads that owner's items alone: an issue's, or a project
// document's, which carries the document instead of an issue key. The same prefix with no owner
// named reads every item of the kind, and is ambiguous where two owners hold one.
func TestResolvePrefixLooksWithinTheOwnerAlone(t *testing.T) {
	tx := prefixFixture(t)
	createReferenceProject(t, tx, "WEB")
	createReferenceIssue(t, tx, "WEB-1", "WEB")
	const core = "7a000000-0000-4000-8000-000000000001"
	const web = "7a000000-0000-4000-8000-000000000002"
	insertPrefixComment(t, tx, core, "CORE-1", "The core comment.")
	insertPrefixComment(t, tx, web, "WEB-1", "The web comment.")
	want := Candidate{
		Kind: "comment", ID: core, Ref: "dispatch://CORE-1/comment/" + core,
		IssueKey: "CORE-1", FirstLine: "The core comment.",
	}
	if got := resolvedPrefix(t, tx, "comment", Scope{IssueKey: "CORE-1"}, "7a000000", Viewer{Human: true}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the comment of CORE-1 = %#v, want %#v", got, want)
	}
	_, err := ResolvePrefix(context.Background(), tx, "comment", Scope{}, "7a000000", Viewer{Human: true})
	var ambiguous *ErrPrefixAmbiguous
	if !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
		t.Fatalf("the same prefix across both issues = %v, want both listed", err)
	}

	document := "8a000000-0000-4000-8000-00000000000a"
	handbook := insertPrefixDocument(t, tx, document, nil, "CORE", "handbook", "Handbook", false)
	if handbook != "CORE/handbook" {
		t.Fatalf("the project document's ref_key = %q, want CORE/handbook", handbook)
	}
	issueKey := "CORE-1"
	const documentAsk = "9b000000-0000-4000-8000-000000000001"
	const issueAsk = "9b000000-0000-4000-8000-000000000002"
	insertPrefixAsk(t, tx, documentAsk, nil, &document, "Which vendor?")
	insertPrefixAsk(t, tx, issueAsk, &issueKey, nil, "Which region?")
	wantAsk := Candidate{
		Kind: "ask", ID: documentAsk, Ref: "dispatch://CORE/artifact/handbook/ask/" + documentAsk,
		Document: "dispatch://CORE/artifact/handbook", FirstLine: "Which vendor?",
	}
	if got := resolvedPrefix(t, tx, "ask", Scope{Document: handbook}, "9b000000", Viewer{Human: true}); !reflect.DeepEqual(got, wantAsk) {
		t.Fatalf("the ask of the project document = %#v, want %#v", got, wantAsk)
	}
	if got := resolvedPrefix(t, tx, "ask", Scope{IssueKey: "CORE-1"}, "9b000000", Viewer{Human: true}); got.ID != issueAsk {
		t.Fatalf("the ask of CORE-1 = %#v, want %s", got, issueAsk)
	}
}

// A block prefix is matched among the block ids of the one document version named: the latest
// indexed version's blocks, or the blocks the pinned version held, and never another document's.
// A block id that is also the start of a longer one still names itself.
func TestResolvePrefixReadsTheBlocksOfTheDocumentVersionNamed(t *testing.T) {
	tx := prefixFixture(t)
	issueKey := "CORE-1"
	const specID = "8a000000-0000-4000-8000-000000000011"
	const notesID = "8a000000-0000-4000-8000-000000000012"
	spec := insertPrefixDocument(t, tx, specID, &issueKey, "CORE", "spec", "Spec", true)
	notes := insertPrefixDocument(t, tx, notesID, &issueKey, "CORE", "notes", "Notes", false)
	insertPrefixBlock(t, tx, specID, spec, "bbbb11110", "The spec block.", 1)
	insertPrefixBlock(t, tx, notesID, notes, "bbbb11111", "The notes block.", 1)
	wantSpec := Candidate{
		Kind: "block", ID: "bbbb11110", Ref: "dispatch://CORE-1/spec#b-bbbb11110",
		IssueKey: "CORE-1", FirstLine: "The spec block.",
	}
	if got := resolvedPrefix(t, tx, "block", Scope{Document: spec}, "bbbb1111", Viewer{Human: true}); !reflect.DeepEqual(got, wantSpec) {
		t.Fatalf("the block of the spec = %#v, want %#v", got, wantSpec)
	}
	wantNotes := Candidate{
		Kind: "block", ID: "bbbb11111", Ref: "dispatch://CORE-1/artifact/notes#b-bbbb11111",
		IssueKey: "CORE-1", FirstLine: "The notes block.",
	}
	if got := resolvedPrefix(t, tx, "block", Scope{Document: notes}, "bbbb1111", Viewer{Human: true}); !reflect.DeepEqual(got, wantNotes) {
		t.Fatalf("the block of the notes = %#v, want %#v", got, wantNotes)
	}
	requireNoPrefixMatch(t, tx, "block", Scope{}, "bbbb1111", Viewer{Human: true})

	// A whole block id that is the start of another's wins over it, as an exact id always does.
	insertPrefixBlock(t, tx, specID, spec, "abcd1234", "The short block.", 1)
	insertPrefixBlock(t, tx, specID, spec, "abcd12345", "The longer block.", 1)
	if got := resolvedPrefix(t, tx, "block", Scope{Document: spec}, "abcd1234", Viewer{Human: true}); got.ID != "abcd1234" {
		t.Fatalf("the whole block id = %#v, want abcd1234 itself", got)
	}

	// The versions the index recorded: the spec held cccc11110 at 1 and deleted it at 2, where it
	// held dddd11110; the notes held a block of the same prefix at 1.
	one, two := "Version one of the spec.", "Version two of the spec."
	insertPrefixBlockText(t, tx, specID, "cccc11110", 1, &one)
	insertPrefixBlockText(t, tx, specID, "cccc11110", 2, nil)
	insertPrefixBlockText(t, tx, specID, "dddd11110", 2, &two)
	notesOne := "Version one of the notes."
	insertPrefixBlockText(t, tx, notesID, "cccc11111", 1, &notesOne)
	wantPinned := Candidate{
		Kind: "block", ID: "cccc11110", Ref: "dispatch://CORE-1/spec#b-cccc11110",
		IssueKey: "CORE-1", FirstLine: one,
	}
	if got := resolvedPrefix(t, tx, "block", Scope{Document: spec, Version: 1}, "cccc1111", Viewer{Human: true}); !reflect.DeepEqual(got, wantPinned) {
		t.Fatalf("the block of the spec at version 1 = %#v, want %#v", got, wantPinned)
	}
	if got := resolvedPrefix(t, tx, "block", Scope{Document: notes, Version: 1}, "cccc1111", Viewer{Human: true}); got.ID != "cccc11111" || got.FirstLine != notesOne {
		t.Fatalf("the block of the notes at version 1 = %#v, want cccc11111", got)
	}
	if got := resolvedPrefix(t, tx, "block", Scope{Document: spec, Version: 2}, "dddd1111", Viewer{Human: true}); got.ID != "dddd11110" || got.FirstLine != two {
		t.Fatalf("the block the spec gained at version 2 = %#v, want dddd11110", got)
	}
	// Deleted at version 2, not yet written at version 1, and no block at all at a version no
	// block-aware build indexed.
	requireNoPrefixMatch(t, tx, "block", Scope{Document: spec, Version: 2}, "cccc1111", Viewer{Human: true})
	requireNoPrefixMatch(t, tx, "block", Scope{Document: spec, Version: 1}, "dddd1111", Viewer{Human: true})
	requireNoPrefixMatch(t, tx, "block", Scope{Document: spec, Version: 3}, "dddd1111", Viewer{Human: true})
	// The latest indexed version's blocks are the ones document_blocks holds, which hold none of
	// these, and a block lookup that names no document names nothing.
	requireNoPrefixMatch(t, tx, "block", Scope{Document: spec}, "cccc1111", Viewer{Human: true})
	requireNoPrefixMatch(t, tx, "block", Scope{}, "cccc1111", Viewer{Human: true})
}

// A block id is text its author wrote, not a uuid: `Deadbeef` and `deadbeef` are two blocks of
// one document, and the id written exactly as the prefix is the one it names, whichever of the
// two the block id order puts first. A prefix neither is written as names both, which is two
// blocks and no match. The case-insensitive reading stays where an id is a uuid, whose text form
// is lowercase hex (the comment prefixes above).
func TestResolvePrefixNamesTheBlockWrittenExactlyAsThePrefix(t *testing.T) {
	tx := prefixFixture(t)
	issueKey := "CORE-1"
	const specID = "8a000000-0000-4000-8000-000000000021"
	spec := insertPrefixDocument(t, tx, specID, &issueKey, "CORE", "spec", "Spec", true)
	insertPrefixBlock(t, tx, specID, spec, "deadbeef", "The lowercase block.", 1)
	insertPrefixBlock(t, tx, specID, spec, "Deadbeef", "The capitalised block.", 1)
	for _, want := range []Candidate{
		{
			Kind: "block", ID: "deadbeef", Ref: "dispatch://CORE-1/spec#b-deadbeef",
			IssueKey: "CORE-1", FirstLine: "The lowercase block.",
		},
		{
			Kind: "block", ID: "Deadbeef", Ref: "dispatch://CORE-1/spec#b-Deadbeef",
			IssueKey: "CORE-1", FirstLine: "The capitalised block.",
		},
	} {
		if got := resolvedPrefix(t, tx, "block", Scope{Document: spec}, want.ID, Viewer{Human: true}); !reflect.DeepEqual(got, want) {
			t.Fatalf("the block %q names = %#v, want %#v", want.ID, got, want)
		}
	}
	var ambiguous *ErrPrefixAmbiguous
	candidate, err := ResolvePrefix(context.Background(), tx, "block", Scope{Document: spec}, "DEADbeef", Viewer{Human: true})
	if !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
		t.Fatalf("the block prefix %q = %#v, %v; want both blocks listed", "DEADbeef", candidate, err)
	}
}

// A direct message's first line is answered to a human and to a session in the conversation - the
// one its root targets, or one that has replied in it - and left out for any other session and
// for the index-time resolver. An issue message's is answered to all of them.
func TestResolvePrefixAnswersADirectMessageFirstLineOnlyToTheConversation(t *testing.T) {
	tx := prefixFixture(t)
	root := "6d000000-0000-4000-8000-000000000001"
	const reply = "9e000000-0000-4000-8000-000000000002"
	const onIssue = "5c000000-0000-4000-8000-000000000003"
	target := "session:sess-one"
	issueKey := "CORE-1"
	insertPrefixMessage(t, tx, root, nil, `{"kind":"user","id":"alice"}`, "The secret plan.", &target, nil)
	insertPrefixMessage(t, tx, reply, nil, `{"kind":"session","id":"sess-two"}`, "On it.", &target, &root)
	insertPrefixMessage(t, tx, onIssue, &issueKey, `{"kind":"user","id":"alice"}`, "Status?", nil, nil)
	for name, viewer := range map[string]Viewer{
		"a human":                 {Human: true},
		"the session it targets":  {Session: "sess-one"},
		"a session that replied":  {Session: "sess-two"},
		"a human with a session":  {Human: true, Session: "sess-nine"},
		"another session":         {Session: "sess-nine"},
		"the index-time resolver": {},
	} {
		want := Candidate{Kind: "message", ID: root, FirstLine: "The secret plan."}
		if name == "another session" || name == "the index-time resolver" {
			want.FirstLine = ""
		}
		if got := resolvedPrefix(t, tx, "message", Scope{}, "6d000000", viewer); !reflect.DeepEqual(got, want) {
			t.Fatalf("the direct message %s reads = %#v, want %#v", name, got, want)
		}
		wantIssue := Candidate{
			Kind: "message", ID: onIssue, Ref: "dispatch://CORE-1/message/" + onIssue,
			IssueKey: "CORE-1", FirstLine: "Status?",
		}
		if got := resolvedPrefix(t, tx, "message", Scope{}, "5c000000", viewer); !reflect.DeepEqual(got, wantIssue) {
			t.Fatalf("the issue message %s reads = %#v, want %#v", name, got, wantIssue)
		}
	}
}

// A document's own id takes a prefix too, and answers the document's address and name.
func TestResolvePrefixNamesADocument(t *testing.T) {
	tx := prefixFixture(t)
	issueKey := "CORE-1"
	const specID = "8a000000-0000-4000-8000-000000000011"
	insertPrefixDocument(t, tx, specID, &issueKey, "CORE", "spec", "The spec", true)
	want := Candidate{
		Kind: "artifact", ID: specID, Ref: "dispatch://CORE-1/spec",
		IssueKey: "CORE-1", FirstLine: "The spec",
	}
	if got := resolvedPrefix(t, tx, "artifact", Scope{}, "8a000000", Viewer{Human: true}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the document 8a000000 names = %#v, want %#v", got, want)
	}
	if got := resolvedPrefix(t, tx, "artifact", Scope{IssueKey: "CORE-1"}, "8a000000", Viewer{Human: true}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the document of CORE-1 = %#v, want %#v", got, want)
	}
	requireNoPrefixMatch(t, tx, "artifact", Scope{IssueKey: "CORE-2"}, "8a000000", Viewer{Human: true})
}
