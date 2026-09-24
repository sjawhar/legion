package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An issue's claim is the session that intends to implement it, so two sessions never take
// the same work. It is never a record of contact: one issue legitimately has one implementer
// holding the claim while coordinators gate its pull requests, and those gates must neither
// claim it nor collide with it.
//
// The claim and the issue's status are separate, on Sami's decision (2026-09-24, verbatim:
// "Keep them separate — Separate because humans might be using them to keep track of work").
// So claiming an issue never moves its status and no status change ever claims: the only
// thing that creates a claim is POST /api/v1/issues/{key}/claim (the `dispatch_claim` tool).
// Messages, comments, asks, labels, links, priority, route, component writes, status moves
// and every read leave the claim exactly as it was.
//
// The claimant is always the authenticated caller of the request — a bearer's own session
// (named in `actor`, as every Dispatch write names it) or a human's cookie identity. No
// argument claims on behalf of another session, and the request body is closed to unknown
// fields, so a caller cannot record someone else as the holder.
//
// claimBlockedBy is the whole taking rule, in one place: a claim whose session the Envoy
// listener still lists as live belongs to that session until it releases it or a human forces
// it; a claim whose session is gone may be taken by any agent (Sami, 2026-09-24, on whether
// that needs asking: "Yes, automatically"), and the takeover is recorded on the issue and
// delivered to the session that lost it.

// sameClaimant reports whether two actors are the same claim holder: one session id, or one
// human login.
func sameClaimant(left, right model.Actor) bool {
	return left.Kind == right.Kind && left.ID == right.ID
}

// claimHolder names a holder in one phrase: a human by login, a session by its id and the
// title it was running under, the way the dashboard labels a session.
func claimHolder(actor model.Actor) string {
	if actor.Kind != "session" {
		return actor.Kind + " " + actor.ID
	}
	title := ""
	if actor.Origin != nil {
		title = strings.TrimSpace(actor.Origin.SessionTitle)
	}
	if title == "" {
		return "session " + actor.ID
	}
	return "session " + actor.ID + " (" + title + ")"
}

// claimConflict refuses to take a claim its live holder still has. It carries the claim so
// the 409 names the holder, its title, and when it claimed, and a client can render that
// without a second read.
type claimConflict struct{ claim model.IssueClaim }

func (c *claimConflict) Error() string {
	return fmt.Sprintf(
		"%s claimed this issue at %s and is still running; ask that session to release it, or a human can force the claim",
		claimHolder(c.claim.Actor), c.claim.At.UTC().Format(time.RFC3339),
	)
}

// writeClaimError answers a claim refusal: the 409 ISSUE_CLAIMED body carries the claim
// itself, and every other failure follows the usual handler mapping.
func (s *server) writeClaimError(w http.ResponseWriter, err error) {
	var conflict *claimConflict
	if errors.As(err, &conflict) {
		WriteJSON(w, http.StatusConflict, map[string]any{
			"error": conflict.Error(),
			"code":  "ISSUE_CLAIMED",
			"claim": conflict.claim,
		})
		return
	}
	s.writeHandlerError(w, err)
}

// claimHolderLive reports whether a claim's holder is still working. A human's claim has no
// session to end, so it is always held. A session's claim is held only while the Envoy
// listener still lists that session — the same live list GET /api/v1/agents and the issue
// subscribers read. With no reachable listener Dispatch cannot tell, and says so rather than
// guess one session's work away from it.
func (s *server) claimHolderLive(ctx context.Context, claim model.IssueClaim) (bool, error) {
	if claim.Actor.Kind != "session" {
		return true, nil
	}
	if s.deps.Envoy == nil {
		return false, errorf(http.StatusServiceUnavailable, "ENVOY_UNAVAILABLE",
			"ENVOY_URL is not configured, so Dispatch cannot tell whether %s is still working", claimHolder(claim.Actor))
	}
	sessions, err := s.deps.Envoy.Sessions(ctx)
	if err != nil {
		if errors.Is(err, envoy.ErrUnavailable) {
			return false, errorf(http.StatusServiceUnavailable, "ENVOY_UNAVAILABLE", "%s", err.Error())
		}
		return false, err
	}
	for _, session := range sessions {
		if session.SessionID == claim.Actor.ID {
			return true, nil
		}
	}
	return false, nil
}

// claimBlockedBy returns the conflict that stops actor from taking or clearing current, or
// nil when nothing does. humanOverride is the one difference between the callers: releasing
// a claim is any human's to do, while claiming over a live holder takes an explicit force.
// The liveness lookup runs only when somebody else holds the claim, so the ordinary claim
// costs no listener call.
func (s *server) claimBlockedBy(
	ctx context.Context, current *model.IssueClaim, actor model.Actor, humanOverride bool,
) error {
	if current == nil || sameClaimant(current.Actor, actor) {
		return nil
	}
	if humanOverride && actor.Kind == "user" {
		return nil
	}
	live, err := s.claimHolderLive(ctx, *current)
	if err != nil {
		return err
	}
	if live {
		return &claimConflict{claim: *current}
	}
	return nil
}

// writeIssueClaim sets or clears an issue's claim; a nil claim clears it. The two claim
// columns always move together, and the issue's status is never touched here.
func writeIssueClaim(ctx context.Context, tx pgx.Tx, key string, claim *model.IssueClaim) error {
	if claim == nil {
		_, err := tx.Exec(ctx, `
			update issues set claimed_by = null, claimed_at = null, updated_at = now() where key = $1
		`, key)
		return err
	}
	actor, err := encodeJSON(claim.Actor)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		update issues set claimed_by = $2, claimed_at = $3, updated_at = now() where key = $1
	`, key, actor, claim.At)
	return err
}

// claimEvent is the issue.claimed or issue.released record: what the claim became, whose
// claim it replaced or cleared, and why.
func claimEvent(
	eventType string, key string, actor model.Actor, after model.Issue,
	previous *model.IssueClaim, reason string,
) model.Event {
	return issueOwner(key).event(eventType, actor, model.IssueClaimEventPayload{
		Key:      key,
		Status:   after.Status,
		Claim:    after.Claim,
		Previous: previous,
		Reason:   reason,
	})
}

// claimIssue is POST /api/v1/issues/{key}/claim: this session (or human) takes the issue.
func (s *server) claimIssue(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor *model.Actor `json:"actor"`
		Force bool         `json:"force"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if input.Force && actor.Kind != "user" {
		writeError(w, "HUMAN_ONLY", http.StatusForbidden, "only a human may force a claim away from a live session")
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	key := r.PathValue("key")
	if err := tx.QueryRow(r.Context(), `select key from issues where key = $1 for update`, key).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	before, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if before.ClosedAt != nil {
		writeError(w, "ISSUE_CLOSED", http.StatusConflict, "issue is closed")
		return
	}
	if err := s.claimBlockedBy(r.Context(), before.Claim, actor, input.Force); err != nil {
		s.writeClaimError(w, err)
		return
	}
	if before.Claim != nil && sameClaimant(before.Claim.Actor, actor) {
		// This session already holds it: the claim keeps the time work started, and the
		// repeated claim stays out of the issue's log.
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, before)
		return
	}
	reason := "claimed"
	previous := before.Claim
	if previous != nil {
		reason = "takeover"
		if input.Force {
			reason = "forced"
		}
	}
	if err := writeIssueClaim(r.Context(), tx, key, &model.IssueClaim{Actor: actor, At: time.Now().UTC()}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	after, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	after.LastSeq++
	event, err := s.appendEvent(r.Context(), tx, claimEvent("issue.claimed", key, actor, after, previous, reason))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, after)
}

// releaseIssueClaim is DELETE /api/v1/issues/{key}/claim: the holder gives the issue up, a
// human releases anyone's claim, or anyone clears a claim whose session is gone. Releasing
// never moves the status — the work stays where it got to.
func (s *server) releaseIssueClaim(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	if !human {
		// A bearer names its own session in the body, as it does on every other write.
		var input struct {
			Actor *model.Actor `json:"actor"`
		}
		if err := decodeJSON(r, &input); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		actor, err = bearerSessionActor(actor, input.Actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	key := r.PathValue("key")
	if err := tx.QueryRow(r.Context(), `select key from issues where key = $1 for update`, key).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	before, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if before.Claim == nil {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, before)
		return
	}
	if err := s.claimBlockedBy(r.Context(), before.Claim, actor, true); err != nil {
		s.writeClaimError(w, err)
		return
	}
	previous := before.Claim
	if err := writeIssueClaim(r.Context(), tx, key, nil); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	after, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	after.LastSeq++
	event, err := s.appendEvent(r.Context(), tx, claimEvent("issue.released", key, actor, after, previous, "released"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, after)
}
