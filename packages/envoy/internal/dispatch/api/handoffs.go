package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// handoffPhasePattern is a handoff's phase, the routes' {phase}: a lowercase slug. Legion writes
// plan, implement, test and review. The handoffs.phase check (migration 0120) holds the same rule,
// generated from contracts' HANDOFF_PHASE_PATTERN, which the dispatch command checks too.
var handoffPhasePattern = regexp.MustCompile(contracts.HandoffPhasePattern)

// handoffRequestLimit is the most a handoff write's request may hold. The body travels as a JSON
// string, whose escaping can make each byte of the object up to six on the wire (a client that
// writes < as \u003c, or a two-byte character as \u00e9), so a body at the cap must fit six times
// over beside the actor; the cap itself is checked on the decoded body.
const handoffRequestLimit = 6*contracts.DispatchHandoffBodyMax + 4<<10

// handoffColumns is the handoffs select list scanHandoff reads, in scan order.
const handoffColumns = `id::text, issue_key, phase, version, author, created_at`

type createHandoffInput struct {
	Actor *model.Actor `json:"actor"`
	Body  *string      `json:"body"`
}

// createHandoff records one phase handoff: POST /api/v1/issues/{key}/handoffs/{phase}. Only a
// bearer naming its session writes one, since the Legion daemon accepts a handoff only from the
// session that holds the phase's claim. The version is the next of the issue's phase, taken under
// the issue's row lock, and handoff.recorded is appended in the same transaction. A handoff gets
// no write advice and no reference edges, and its event notifies no one (events.Broker.Notify
// wakes on a person's writes only).
func (s *server) createHandoff(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	if human {
		writeError(w, "HANDOFF_FORBIDDEN", http.StatusForbidden, "a handoff is written by an agent session under a bearer token, naming itself in actor")
		return
	}
	phase := r.PathValue("phase")
	if !handoffPhasePattern.MatchString(phase) {
		writeHandoffPhaseError(w, phase)
		return
	}
	var input createHandoffInput
	if err := decodeJSONWithin(r, &input, handoffRequestLimit); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusRequestEntityTooLarge {
			writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes: a handoff body is at most %d bytes", handoffRequestLimit, contracts.DispatchHandoffBodyMax))
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	if actor, err = bearerSessionActor(actor, input.Actor); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Body == nil {
		writeError(w, "HANDOFF_INPUT", http.StatusBadRequest, "body is required: the handoff, one JSON object")
		return
	}
	body := []byte(*input.Body)
	if length := len(body); length > contracts.DispatchHandoffBodyMax {
		writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge,
			fmt.Sprintf("body is %d bytes over the %d-byte limit (%d/%d)", length-contracts.DispatchHandoffBodyMax, contracts.DispatchHandoffBodyMax, length, contracts.DispatchHandoffBodyMax))
		return
	}
	// One valid JSON value whose first token opens an object is one JSON object. Valid JSON holds
	// no raw control character, so a body that passes holds nothing a text column refuses.
	if !json.Valid(body) || !bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("{")) {
		writeError(w, "HANDOFF_INPUT", http.StatusBadRequest, "body is not one JSON object: a handoff body is a JSON object")
		return
	}
	handoff, err := s.recordHandoff(r.Context(), r.PathValue("key"), phase, *input.Body, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, handoff)
}

// recordHandoff writes the handoff as the next version of the issue's phase and appends its
// handoff.recorded, in one transaction that first locks the open issue.
func (s *server) recordHandoff(ctx context.Context, issueKey, phase, body string, actor model.Actor) (model.HandoffSummary, error) {
	author, err := json.Marshal(actor)
	if err != nil {
		return model.HandoffSummary{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return model.HandoffSummary{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := s.requireOpenIssue(ctx, tx, issueKey); err != nil {
		return model.HandoffSummary{}, err
	}
	handoff, err := scanHandoff(tx.QueryRow(ctx, `
		insert into handoffs (issue_key, phase, version, body, author)
		select $1::text, $2::text, coalesce(max(version), 0) + 1, $3::text, $4::jsonb
		from handoffs where issue_key = $1::text and phase = $2::text
		returning `+handoffColumns, issueKey, phase, body, author))
	if err != nil {
		return model.HandoffSummary{}, err
	}
	event, err := s.appendEvent(ctx, tx, issueOwner(issueKey).event("handoff.recorded", actor, model.HandoffRecordedEventPayload{
		HandoffID: handoff.ID, Phase: handoff.Phase, Version: handoff.Version,
	}))
	if err != nil {
		return model.HandoffSummary{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.HandoffSummary{}, err
	}
	s.publish(event)
	return handoff, nil
}

// listHandoffs is every handoff of one issue without its body, newest first: GET
// /api/v1/issues/{key}/handoffs. A closed issue's handoffs read as an open one's.
func (s *server) listHandoffs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+handoffColumns+`
		from handoffs
		where issue_key = $1
		order by created_at desc, phase, version desc
	`, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	handoffs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.HandoffSummary, error) {
		return scanHandoff(row)
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, handoffs)
}

// getLatestHandoff is the newest handoff of one phase of an issue, with its body: GET
// /api/v1/issues/{key}/handoffs/{phase}/latest.
func (s *server) getLatestHandoff(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	issueKey, phase := r.PathValue("key"), r.PathValue("phase")
	if !handoffPhasePattern.MatchString(phase) {
		writeHandoffPhaseError(w, phase)
		return
	}
	s.writeHandoff(w, r, fmt.Sprintf("issue %s has no %s handoff", issueKey, phase), `
		select `+handoffColumns+`, body
		from handoffs
		where issue_key = $1 and phase = $2
		order by version desc
		limit 1
	`, issueKey, phase)
}

// getHandoff is one handoff by id, with its body: GET /api/v1/handoffs/{id}.
func (s *server) getHandoff(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	if !requireUUIDPath(w, r, "handoff") {
		return
	}
	id := r.PathValue("id")
	s.writeHandoff(w, r, "handoff "+id+" not found", `
		select `+handoffColumns+`, body from handoffs where id = $1
	`, id)
}

// writeHandoff answers the one handoff query selects (handoffColumns, then body), or 404
// HANDOFF_NOT_FOUND with missing when it selects none.
func (s *server) writeHandoff(w http.ResponseWriter, r *http.Request, missing, query string, args ...any) {
	var handoff model.Handoff
	summary, err := scanHandoff(s.deps.Store.Pool.QueryRow(r.Context(), query, args...), &handoff.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "HANDOFF_NOT_FOUND", http.StatusNotFound, missing)
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	handoff.HandoffSummary = summary
	WriteJSON(w, http.StatusOK, handoff)
}

// writeHandoffPhaseError refuses a phase that is not a handoff phase with the rule, worded once in
// contracts (HANDOFF_PHASE_RULE_WORDS) so the dispatch command and the server say the same thing.
func writeHandoffPhaseError(w http.ResponseWriter, phase string) {
	writeError(w, "HANDOFF_INPUT", http.StatusBadRequest,
		fmt.Sprintf("%q is not a handoff phase: %s", phase, contracts.HandoffPhaseRule))
}

// scanHandoff decodes one handoffColumns row; extra receives any columns selected after them.
func scanHandoff(row pgx.Row, extra ...any) (model.HandoffSummary, error) {
	var handoff model.HandoffSummary
	var author []byte
	dest := append([]any{
		&handoff.ID, &handoff.IssueKey, &handoff.Phase, &handoff.Version, &author, &handoff.CreatedAt,
	}, extra...)
	if err := row.Scan(dest...); err != nil {
		return model.HandoffSummary{}, err
	}
	if err := json.Unmarshal(author, &handoff.Author); err != nil {
		return model.HandoffSummary{}, fmt.Errorf("decode handoff author: %w", err)
	}
	return handoff, nil
}
