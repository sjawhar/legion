package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

type adviceAsk struct {
	ID       string `json:"id"`
	Question string `json:"question"`
}

type writeAdvice struct {
	IssueStatus             string      `json:"issue_status"`
	SessionWritesSinceHuman int         `json:"session_writes_since_human"`
	YourOpenAsks            []adviceAsk `json:"your_open_asks"`
	// LegionManagedStatus is whether the Legion daemon owns the issue's status (the issue, or
	// an issue above it, carries the legion label). A pointer, omitted when nil, because it is
	// resolved only where a line depends on it - a session's write on an issue in triage, the
	// one case where the client has advice to drop - and a plain bool would serialise false
	// everywhere else and state, wrongly, that Legion does not own the status. Absent means
	// "not asked", which is what @legion/contracts' optional WriteAdvice.legion_managed_status
	// already expresses.
	LegionManagedStatus *bool `json:"legion_managed_status,omitempty"`
	// documentBlocks is set by a document write whose document reads; nil omits its fields.
	*documentBlocks
	// editBlocks is set by a document edit; nil omits its field.
	*editBlocks
	// SupersededApproval is set by a document write that stored a version replacing the one a
	// person had approved; nil (omitted) everywhere else.
	SupersededApproval *supersededApproval `json:"superseded_approval,omitempty"`
	// Suggestions is the related-item check's result, set only on an issue or ask
	// creation; nil (omitted) everywhere else.
	Suggestions *model.Suggestions `json:"suggestions,omitempty"`
}

// suggestionsOnlyAdvice is the advice of a write with no issue state to report: an ask on an
// unlinked project document, which has no issue status, no writes-since-human count and no
// issue-scoped open-ask list, or an issue or ask write whose write-advice read failed. It still
// carries the related-item check's result (withSuggestions).
type suggestionsOnlyAdvice struct {
	Suggestions *model.Suggestions `json:"suggestions"`
}

type adviceQueryError struct {
	query string
	err   error
}

func (e *adviceQueryError) Error() string {
	return fmt.Sprintf("%s: %v", e.query, e.err)
}

func (e *adviceQueryError) Unwrap() error {
	return e.err
}

// sessionWriteTypes are the events session_writes_since_human counts; its query binds them.
var sessionWriteTypes = []string{"message.created", "comment.created", "ask.opened"}

// computeWriteAdvice runs the issue-scoped advice queries for a session's write. Two of them are
// about the calling session itself - its own writes since the last human event, its own open
// asks - so writeAdvice calls this only for a session actor. The third, whether Legion owns the
// issue's status, runs only on an issue whose status is triage: that is the only line the answer
// decides, so an ordinary write on an ordinary issue gains no query at all. status is the
// issue's status as the write committed it, the same value writeAdvice reports. route names the
// caller for the one query whose failure is logged rather than returned
// (legionManagedStatusAdvice). appending is the event the write appends after the advice is read
// (writeAdvice). row is the issue row as the write leaves it when the write reads its advice
// before it writes that row (writeAdviceBefore), nil to read the stored one.
func (s *server) computeWriteAdvice(
	ctx context.Context,
	tx pgx.Tx,
	route string,
	issueKey string,
	actor model.Actor,
	excludeAskID string,
	status string,
	appending string,
	row *adviceIssueRow,
) (*writeAdvice, error) {
	advice := &writeAdvice{YourOpenAsks: []adviceAsk{}, IssueStatus: status}
	if err := s.runAdviceQueryHook(ctx, tx, "session_writes_since_human"); err != nil {
		return nil, &adviceQueryError{query: "session_writes_since_human", err: err}
	}
	if err := tx.QueryRow(ctx, `
		with last_human as materialized (
			select coalesce(max(id), 0) as last_id from events
			where issue_key = $1 and actor->>'kind' = 'user'
		)
		select count(*) from events, last_human
		where issue_key = $1 and actor->>'kind' = 'session' and actor->>'id' = $2
		  and type = any($3::text[])
		  and id > last_human.last_id
	`, issueKey, actor.ID, sessionWriteTypes).Scan(&advice.SessionWritesSinceHuman); err != nil {
		return nil, &adviceQueryError{query: "session_writes_since_human", err: err}
	}
	// The write's own event is not appended yet (writeAdvice), and it is the session's newest
	// write: under the issue's owner lock no other event of the issue commits before it, and the
	// write appends no human event, so the count it would have read after the append is one more.
	if slices.Contains(sessionWriteTypes, appending) {
		advice.SessionWritesSinceHuman++
	}
	if err := s.runAdviceQueryHook(ctx, tx, "your_open_asks"); err != nil {
		return nil, &adviceQueryError{query: "your_open_asks", err: err}
	}
	rows, err := tx.Query(ctx, `
		select id::text, question from asks
		where issue_key = $1 and state = 'open'
		  and author->>'kind' = 'session' and author->>'id' = $2
		  and ($3 = '' or id::text <> $3)
		order by created_at, id
		limit 2
	`, issueKey, actor.ID, excludeAskID)
	if err != nil {
		return nil, &adviceQueryError{query: "your_open_asks", err: err}
	}
	defer rows.Close()
	for rows.Next() {
		var ask adviceAsk
		if err := rows.Scan(&ask.ID, &ask.Question); err != nil {
			return nil, &adviceQueryError{query: "your_open_asks", err: err}
		}
		advice.YourOpenAsks = append(advice.YourOpenAsks, ask)
	}
	if err := rows.Err(); err != nil {
		return nil, &adviceQueryError{query: "your_open_asks", err: err}
	}
	if status == model.IssueStatusTriage {
		advice.LegionManagedStatus = s.legionManagedStatusAdvice(ctx, tx, route, issueKey, row)
	}
	return advice, nil
}

// legionManagedStatusAdvice answers whether Legion owns the issue's status for a write's advice,
// or nil when the walk failed. It is the last step of computeWriteAdvice and the only one whose
// failure is not fatal to the advice: the two before it are what a session asked for, while this
// one decides a single line, and every issue is created in triage, so returning an
// adviceQueryError here would put a third failure mode on the path that runs for every
// session-authored `dispatch issue`. What that would cost a creation is its issue state alone: the
// related-item check runs after the write commits whatever the advice answered, and rides a
// suggestions-only advice when it failed (withSuggestions). Nothing runs after this step, so the
// advice already assembled is complete without it.
//
// The walk takes a savepoint of its own inside the advice savepoint, because a failed statement
// aborts the transaction it ran in: run directly on the advice savepoint, its failure would also
// take the statement_timeout reset and the release that follow it (writeAdvice), and the advice
// would be dropped anyway. The failure is logged once, in the shape every omitted advice query
// takes. The 500 ms bound writeAdvice set still covers it: SET LOCAL holds across a savepoint
// opened under it.
func (s *server) legionManagedStatusAdvice(ctx context.Context, tx pgx.Tx, route, issueKey string, row *adviceIssueRow) *bool {
	walk, err := tx.Begin(ctx)
	if err != nil {
		s.logAdviceError(route, issueKey, "legion_managed_status", err)
		return nil
	}
	if err := s.runAdviceQueryHook(ctx, walk, "legion_managed_status"); err != nil {
		_ = walk.Rollback(ctx)
		s.logAdviceError(route, issueKey, "legion_managed_status", err)
		return nil
	}
	var managed bool
	if row != nil {
		managed, err = legionManagedStatusOfRow(ctx, walk, issueKey, row.parent, row.labels)
	} else {
		managed, err = legionManagedStatus(ctx, walk, issueKey)
	}
	if err != nil {
		_ = walk.Rollback(ctx)
		s.logAdviceError(route, issueKey, "legion_managed_status", err)
		return nil
	}
	if err := walk.Commit(ctx); err != nil {
		s.logAdviceError(route, issueKey, "legion_managed_status", err)
		return nil
	}
	return &managed
}

func (s *server) runAdviceQueryHook(ctx context.Context, tx pgx.Tx, query string) error {
	if s.adviceQueryHook == nil {
		return nil
	}
	return s.adviceQueryHook(ctx, tx, query)
}

// writeAdvice answers what a write tells its caller about the issue it wrote. A write joined to a
// document ledger reads it in its read phase, before Commit runs its writes, so its queries hold no
// transaction id (INFRA-636), and its own event is not in the table yet. appending names the event
// the write appends for actor after this read - "" for none, or for a write that appends before
// it reads its advice - and a counted one (sessionWriteTypes) is counted as if appended, so the
// answer is the one the same read gave after the append.
func (s *server) writeAdvice(
	ctx context.Context,
	tx pgx.Tx,
	route string,
	issueKey string,
	actor model.Actor,
	excludeAskID string,
	status string,
	appending string,
) *writeAdvice {
	return s.adviseWrite(ctx, tx, route, issueKey, actor, excludeAskID, status, appending, nil)
}

// adviceIssueRow is the part of an issue row a write's advice reads - its parent and its labels,
// which the walk for who owns the issue's status starts from (legionManagedStatusOfRow) - as the
// write leaves the row.
type adviceIssueRow struct {
	parent *string
	labels []string
}

// writeAdviceBefore is writeAdvice for a write that reads its advice before it writes the issue
// row the advice describes, so that the advice runs before the write's first statement and holds
// no transaction id (INFRA-636): a create, whose row does not exist yet, and a patch, which may
// change the row's labels, its parent or its status. status and row are the row as the write
// leaves it, so the answer is the one the same read gave after the write; the write appends no
// counted event, and a new issue has neither events nor asks.
func (s *server) writeAdviceBefore(
	ctx context.Context,
	tx pgx.Tx,
	route string,
	issueKey string,
	actor model.Actor,
	status string,
	row adviceIssueRow,
) *writeAdvice {
	return s.adviseWrite(ctx, tx, route, issueKey, actor, "", status, "", &row)
}

// adviseWrite is writeAdvice and writeAdviceBefore: row nil reads the stored issue row.
func (s *server) adviseWrite(
	ctx context.Context,
	tx pgx.Tx,
	route string,
	issueKey string,
	actor model.Actor,
	excludeAskID string,
	status string,
	appending string,
	row *adviceIssueRow,
) *writeAdvice {
	// A human write has no advice query to run - every one of them is about the calling session,
	// and the triage line this answers is about what a session is told - so it is answered here,
	// outside the savepoint and the two statement_timeout statements that would otherwise run
	// inside the write's own transaction while it holds the issue's owner lock. Such a write still gets
	// the triage advice, which is true for a reader who may move the status. A human is the only
	// non-session actor that reaches a write: optionalActor returns an actor only for the cookie
	// identity, which is always Kind "user", and every bearer goes through bearerSessionActor,
	// which refuses an actor that is not a session (server.go). The guard is on the kind rather
	// than on "is a human" so an actor kind the server does not produce today cannot silently
	// acquire session-shaped advice.
	if actor.Kind != "session" {
		return &writeAdvice{YourOpenAsks: []adviceAsk{}, IssueStatus: status}
	}
	adviceTx, err := tx.Begin(ctx)
	if err != nil {
		s.logAdviceError(route, issueKey, "savepoint", err)
		return nil
	}
	// Healthy advice queries complete in single-digit milliseconds. Bound them at 500 ms so a
	// future plan regression cannot hold the issue's owner lock and a pool connection indefinitely.
	if _, err := adviceTx.Exec(ctx, "set local statement_timeout = '500ms'"); err != nil {
		_ = adviceTx.Rollback(ctx)
		s.logAdviceError(route, issueKey, "statement_timeout", err)
		return nil
	}
	advice, err := s.computeWriteAdvice(ctx, adviceTx, route, issueKey, actor, excludeAskID, status, appending, row)
	if err != nil {
		_ = adviceTx.Rollback(ctx)
		query := "unknown"
		if queryErr, ok := err.(*adviceQueryError); ok {
			query = queryErr.query
		}
		s.logAdviceError(route, issueKey, query, err)
		return nil
	}
	// SET LOCAL crosses a released savepoint, so restore the outer transaction's default before
	// releasing this one. An error path rolls back the savepoint and clears the setting.
	if _, err := adviceTx.Exec(ctx, "set local statement_timeout = default"); err != nil {
		_ = adviceTx.Rollback(ctx)
		s.logAdviceError(route, issueKey, "statement_timeout_reset", err)
		return nil
	}
	if err := adviceTx.Commit(ctx); err != nil {
		s.logAdviceError(route, issueKey, "savepoint", err)
		return nil
	}
	return advice
}

func (s *server) logAdviceError(route, issueKey, query string, err error) {
	slog.Warn("dispatch: write advice omitted", "route", route, "issue", issueKey, "query", query, "error", err)
}

// documentBlocks is what a document write's advice reports about the typed blocks in the document
// it stored: how many ask blocks it holds, and the typed block openings it holds as text.
type documentBlocks struct {
	DecisionBlocks  int              `json:"decision_blocks"`
	UnparsedOpeners *unparsedOpeners `json:"unparsed_openers,omitempty"`
}

// unparsedOpeners is the typed block openings (`:::ask{…}`) a document holds as text rather than
// as blocks, outside code: written inside a line, or escaped. A writer who
// meant a block would otherwise hear only that the document holds none (LEGION-416). A mention of
// the syntax in prose is reported too, and harmlessly: it is never refused.
type unparsedOpeners struct {
	Count    int      `json:"count"`
	Examples []string `json:"examples"`
}

// editBlocks is what a document edit's advice reports about the typed blocks it wrote: how many ask
// blocks the edited document holds that the document before the edit did not
// (docs.EditOutcome.AddedAskBlocks), as the server's parser reads them, and the related-item check
// each of them got (decisionBlockChecks), omitted when the edit added none.
type editBlocks struct {
	DecisionBlocksAdded int                  `json:"decision_blocks_added"`
	DecisionBlockChecks []decisionBlockCheck `json:"decision_block_checks,omitempty"`
}

// decisionBlockCheck is one added decision block's related-item check: the block, its
// question, and the check's result.
type decisionBlockCheck struct {
	BlockID     string             `json:"block_id"`
	Question    string             `json:"question"`
	Suggestions *model.Suggestions `json:"suggestions"`
}

// supersededApproval is what a document write reports when the version it stored replaced the
// version a person had approved: that approval is now stale, and nothing but a person's review of
// the new version restores it. Legion's design gate is exactly such an approval - a person's
// approval of a root issue's spec at a version - so a write that supersedes it closes that gate,
// and every READY in the tree is refused until a person approves again. The writer is the only
// one who can be told in time, and only the write that superseded the approval is told: a write
// on a document whose approval was already stale closed nothing.
type supersededApproval struct {
	// ApprovedVersion is the version a person approved, the one this write superseded.
	ApprovedVersion int `json:"approved_version"`
	// Version is the version this write stored.
	Version int `json:"version"`
	// ApprovedBy is who gave that approval, as its review records the actor's id.
	ApprovedBy string `json:"approved_by"`
}

// supersededApprovalAdvice reports the approval the write of version just made stale, or nil when
// it made none, which is every write of a document nobody has approved. It runs in the write's
// read phase, where the version row and the approval carry (docs.carryApproval) are queued rather
// than written, so the carry's own review comes off the ledger (docs.Ledger.PendingReview) and
// everything else from the table: a carried approval is pinned to version itself and reports
// nothing, which is right - the gate reopens on that carry's own artifact.approved event.
//
// The read is the newest review of the document (docs.NewestReview), one entry of
// artifact_reviews_by_artifact_event_seq in docs.ReviewOrder, the order the reviews' events
// committed: artifact_reviews.created_at is its transaction's start and its id is random, so an
// approval committed while this write was open can carry an earlier pair than an older one, and
// ordering by those two alone would report a gate this write did not close. It reports only when
// that newest review approves the version right below this one: an older approval was already
// stale before this write, and a newer changes request had already closed the gate.
//
// Like every other advice query this runs behind a savepoint, because a failed statement aborts
// the transaction it ran in and this must never fail the write it describes. It writes nothing,
// so the savepoint is always rolled back, which also restores the statement_timeout it set.
func (s *server) supersededApprovalAdvice(
	ctx context.Context,
	tx pgx.Tx,
	ledger *docs.Ledger,
	route string,
	issueKey string,
	artifactID string,
	version int,
) *supersededApproval {
	// A review this transaction queued is newer than every stored one and no read of the table
	// can see it before the write phase runs, so it answers for the newest review by itself.
	if review, queued := ledger.PendingReview(artifactID); queued {
		return supersededBy(&review, version)
	}
	adviceTx, err := tx.Begin(ctx)
	if err != nil {
		s.logAdviceError(route, issueKey, "superseded_approval", err)
		return nil
	}
	defer func() { _ = adviceTx.Rollback(ctx) }()
	if _, err := adviceTx.Exec(ctx, "set local statement_timeout = '500ms'"); err != nil {
		s.logAdviceError(route, issueKey, "superseded_approval", err)
		return nil
	}
	approval, err := newestReviewAsApproval(ctx, adviceTx, artifactID, version)
	if err != nil {
		s.logAdviceError(route, issueKey, "superseded_approval", err)
		return nil
	}
	return approval
}

// newestReviewAsApproval is supersededApprovalAdvice's rule, over whatever queryer the caller
// holds: the document's newest review in docs.ReviewOrder, reported when it approves the version
// right below version. A document with no review at all, an approval of an older version, an
// approval of version itself (the approval carry's own review) and a changes request each report
// nothing.
func newestReviewAsApproval(ctx context.Context, q docs.Queryer, artifactID string, version int) (*supersededApproval, error) {
	review, err := docs.NewestReview(ctx, q, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the document's newest review: %w", err)
	}
	return supersededBy(review, version), nil
}

// supersededBy is that rule over one review, whichever read answered it: the document's newest,
// reported when it approves the version right below version and nothing otherwise.
func supersededBy(review *model.ArtifactReview, version int) *supersededApproval {
	if review == nil || review.State != "approved" || review.Version != version-1 {
		return nil
	}
	return &supersededApproval{
		ApprovedVersion: review.Version,
		Version:         version,
		ApprovedBy:      review.Actor.ID,
	}
}

// documentWriteAdvice is the whole advice of a document write with no issue state to report: a
// project document's write, or an issue document's whose bounded issue-advice read failed. It
// carries the document half of writeAdvice - the block report its route produces, and the
// approval the write superseded - so neither depends on the issue advice beside it.
type documentWriteAdvice struct {
	*documentBlocks
	*editBlocks
	SupersededApproval *supersededApproval `json:"superseded_approval,omitempty"`
}

const (
	// unparsedOpenerExamples is how many openings the advice quotes.
	unparsedOpenerExamples = 3
	// unparsedOpenerContext is how many bytes of the text before an opening its example quotes,
	// and unparsedOpenerSpan how far past its start the example runs looking for its closing brace.
	unparsedOpenerContext = 16
	unparsedOpenerSpan    = 80
)

// typedOpening matches a typed block's opening as text: three or more colons, a name, and a brace.
var typedOpening = regexp.MustCompile(`:{3,}[A-Za-z0-9_-]+\{`)

// readDocumentBlocks reads markdown, a document's canonical markdown - the rendering a write
// stored, read back (pmdoc.ParseRendering) - for its advice, or nil when it does not read.
func readDocumentBlocks(markdown string) *documentBlocks {
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		slog.Warn("dispatch: decision block count failed", "error", err)
		return nil
	}
	blocks := &documentBlocks{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		switch node.Type {
		case "ask":
			blocks.DecisionBlocks++
		case "code_block":
			// Code is text on purpose, and its children are text nodes, whose own visits read
			// nothing, so the walk moves on past it: pmdoc.Walk's false stops the whole
			// traversal, which would leave every block after the code uncounted.
			return true
		}
		blocks.readOpenersAsText(node)
		return true
	})
	return blocks
}

// readOpenersAsText records the typed block openings in node's own text: its text children outside
// inline code, read as runs that any other inline node ends.
func (blocks *documentBlocks) readOpenersAsText(node *pmdoc.Node) {
	var run strings.Builder
	flush := func() {
		text := run.String()
		run.Reset()
		for _, match := range typedOpening.FindAllStringIndex(text, -1) {
			if blocks.UnparsedOpeners == nil {
				blocks.UnparsedOpeners = &unparsedOpeners{Examples: []string{}}
			}
			blocks.UnparsedOpeners.Count++
			if len(blocks.UnparsedOpeners.Examples) < unparsedOpenerExamples {
				blocks.UnparsedOpeners.Examples = append(blocks.UnparsedOpeners.Examples, openerExample(text, match[0], match[1]))
			}
		}
	}
	for _, child := range node.Children {
		if child.Type == "text" && !slices.ContainsFunc(child.Marks, func(mark pmdoc.Mark) bool { return mark.Type == "inlineCode" }) {
			run.WriteString(child.Text)
			continue
		}
		flush()
	}
	flush()
}

// openerExample quotes the opening text[start:end] with a little of the text before it and the
// rest of its attributes, up to its closing brace within unparsedOpenerSpan bytes, on rune
// boundaries.
func openerExample(text string, start, end int) string {
	from := max(0, start-unparsedOpenerContext)
	for from > 0 && !utf8.RuneStart(text[from]) {
		from--
	}
	to := min(len(text), max(end, start+unparsedOpenerSpan))
	if brace := strings.IndexByte(text[end:to], '}'); brace >= 0 {
		to = end + brace + 1
	}
	for to < len(text) && !utf8.RuneStart(text[to]) {
		to++
	}
	example := text[from:to]
	if from > 0 {
		example = "…" + example
	}
	return example
}

// advisedResponse keeps advice on the HTTP response: the underlying models are also event
// payloads, where response-only guidance must not appear.
type advisedResponse struct {
	payload any
	advice  any
}

func (response advisedResponse) MarshalJSON() ([]byte, error) {
	payload, err := json.Marshal(response.payload)
	if err != nil {
		return nil, err
	}
	if len(payload) < 2 || payload[0] != '{' || payload[len(payload)-1] != '}' {
		return nil, fmt.Errorf("write advice payload must be a JSON object")
	}
	advice, err := json.Marshal(response.advice)
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, len(payload)+len(advice)+11)
	encoded = append(encoded, payload[:len(payload)-1]...)
	if len(payload) > 2 {
		encoded = append(encoded, ',')
	}
	encoded = append(encoded, `"advice":`...)
	encoded = append(encoded, advice...)
	encoded = append(encoded, '}')
	return encoded, nil
}

func withAdvice(payload any, advice *writeAdvice) any {
	if advice == nil {
		return payload
	}
	return advisedResponse{payload: payload, advice: advice}
}

// withRawAdvice wraps payload with whatever advice shape the caller already decided is worth
// sending, with no nil-check (contrast withAdvice, which treats a nil *writeAdvice as "send
// nothing). Two kinds of shape ride it: a document write's block report (*documentBlocks/*editBlocks,
// when a project document has no issue state to report, or an issue document's bounded
// issue-advice query failed), and suggestionsOnlyAdvice (withSuggestions): an ask on an unlinked
// project document, or an issue or ask write whose write advice failed.
func withRawAdvice(payload any, advice any) any {
	return advisedResponse{payload: payload, advice: advice}
}
