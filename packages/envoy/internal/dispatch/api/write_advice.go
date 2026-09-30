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

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

type adviceAsk struct {
	ID       string `json:"id"`
	Question string `json:"question"`
}

type writeAdvice struct {
	IssueStatus             string           `json:"issue_status"`
	SessionWritesSinceHuman int              `json:"session_writes_since_human"`
	YourOpenAsks            []adviceAsk      `json:"your_open_asks"`
	DecisionBlocks          *int             `json:"decision_blocks,omitempty"`
	UnparsedOpeners         *unparsedOpeners `json:"unparsed_openers,omitempty"`
}

// setDocumentBlocks puts what a document write read of its document's typed blocks on the advice.
func (advice *writeAdvice) setDocumentBlocks(blocks *documentBlocks) {
	if blocks == nil {
		return
	}
	advice.DecisionBlocks = &blocks.DecisionBlocks
	advice.UnparsedOpeners = blocks.UnparsedOpeners
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

func (s *server) computeWriteAdvice(
	ctx context.Context,
	tx pgx.Tx,
	issueKey string,
	actor model.Actor,
	excludeAskID string,
) (*writeAdvice, error) {
	advice := &writeAdvice{YourOpenAsks: []adviceAsk{}}
	if err := s.runAdviceQueryHook(ctx, tx, "session_writes_since_human"); err != nil {
		return nil, &adviceQueryError{query: "session_writes_since_human", err: err}
	}
	if err := tx.QueryRow(ctx, `
		with last_human as materialized (
			select coalesce(max(id), 0) as last_id from events
			where issue_key = $1 and actor->>'kind' = 'user'
		)
		select count(*) from events, last_human
		where issue_key = $1 and actor->>'kind' = 'session'
		  and type in ('message.created', 'comment.created', 'ask.opened')
		  and id > last_human.last_id
	`, issueKey).Scan(&advice.SessionWritesSinceHuman); err != nil {
		return nil, &adviceQueryError{query: "session_writes_since_human", err: err}
	}
	if actor.Kind != "session" {
		return advice, nil
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
	return advice, nil
}

func (s *server) runAdviceQueryHook(ctx context.Context, tx pgx.Tx, query string) error {
	if s.adviceQueryHook == nil {
		return nil
	}
	return s.adviceQueryHook(ctx, tx, query)
}

func (s *server) writeAdvice(
	ctx context.Context,
	tx pgx.Tx,
	route string,
	issueKey string,
	actor model.Actor,
	excludeAskID string,
	status string,
) *writeAdvice {
	adviceTx, err := tx.Begin(ctx)
	if err != nil {
		s.logAdviceError(route, issueKey, "savepoint", err)
		return nil
	}
	// Healthy advice queries complete in single-digit milliseconds. Bound them at 500 ms so a
	// future plan regression cannot hold the issue row lock and a pool connection indefinitely.
	if _, err := adviceTx.Exec(ctx, "set local statement_timeout = '500ms'"); err != nil {
		_ = adviceTx.Rollback(ctx)
		s.logAdviceError(route, issueKey, "statement_timeout", err)
		return nil
	}
	advice, err := s.computeWriteAdvice(ctx, adviceTx, issueKey, actor, excludeAskID)
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
	advice.IssueStatus = status
	return advice
}

func (s *server) logAdviceError(route, issueKey, query string, err error) {
	slog.Warn("dispatch: write advice omitted", "route", route, "issue", issueKey, "query", query, "error", err)
}

// documentBlocks is what a document write's advice reports about the typed blocks in the document
// it stored: how many ask blocks it holds, and the typed block openings it holds as text.
type documentBlocks struct {
	DecisionBlocks int
	// UnparsedOpeners is nil when the document holds no opening as text.
	UnparsedOpeners *unparsedOpeners
}

// unparsedOpeners is the typed block openings (`:::ask{…}`) a document holds as text rather than
// as blocks, outside code: written inside a line, or escaped. The parser refuses such an opening
// where it starts a line of a paragraph (pmdoc's typedOpeningAsText), so this reports the ones it
// stores, where a writer who meant a block would otherwise hear only that the document holds none
// (LEGION-416). A mention of the syntax in prose is reported too, and harmlessly: it is never
// refused.
type unparsedOpeners struct {
	Count    int      `json:"count"`
	Examples []string `json:"examples"`
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
	to := min(len(text), start+unparsedOpenerSpan)
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

// documentBlockAdvice is a project document write's advice: only what it read of the document's
// typed blocks, since a project document has no issue state to report.
type documentBlockAdvice struct {
	DecisionBlocks  int              `json:"decision_blocks"`
	UnparsedOpeners *unparsedOpeners `json:"unparsed_openers,omitempty"`
}

func withDocumentBlockAdvice(payload any, blocks documentBlocks) any {
	return advisedResponse{payload: payload, advice: documentBlockAdvice{
		DecisionBlocks:  blocks.DecisionBlocks,
		UnparsedOpeners: blocks.UnparsedOpeners,
	}}
}
