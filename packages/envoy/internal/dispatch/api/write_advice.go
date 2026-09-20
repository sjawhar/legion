package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

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
	DecisionBlocks          *int        `json:"decision_blocks,omitempty"`
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

func countAskBlocks(markdown string) int {
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		slog.Warn("dispatch: decision block count failed", "error", err)
		return 0
	}
	count := 0
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			count++
		}
		return true
	})
	return count
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

func withDecisionBlockAdvice(payload any, decisionBlocks int) any {
	return advisedResponse{payload: payload, advice: map[string]int{"decision_blocks": decisionBlocks}}
}
