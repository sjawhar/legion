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
// So claiming an issue never moves its status, and the only thing that creates a claim is
// POST /api/v1/issues/{key}/claim (the `dispatch_claim` tool). Messages, comments, asks,
// labels, links, priority, route and component writes, and every read, leave the claim alone;
// closing an issue is the one status move that touches it, clearing it because the work is
// over.
//
// The claimant is the request's own actor, the same identity every other write carries: a
// human's login from their signed cookie (a body that names a session is ignored for a cookie
// caller), or, for a bearer, the session the caller declares in `actor` — what the token
// proves is its owner or its service subject, not which session it is running. So a bearer
// could name another session here exactly as it could on any other write; what makes a claim
// trustworthy is that `dispatch_claim` fills `actor` from the host's own runtime, so no model
// chooses it, and that the request has no second parameter for claiming on someone's behalf.
// The body is closed to unknown fields, so an invented one is refused rather than ignored.
//
// claimBlockedBy is the whole taking rule, in one place: a claim whose session the Envoy
// listener still lists as live belongs to that session until it releases it or a human forces
// it; a claim whose session is gone may be taken by any agent (Sami, 2026-09-24, on whether
// that needs asking: "Yes, automatically"), and the takeover is recorded on the issue and
// delivered to the session that lost it.
//
// The listener lookup never runs inside a transaction. It is a cross-service HTTP call of up
// to five seconds, and production runs four pool connections, so holding one — plus the issue
// row's lock — across it stalls unrelated requests. Every claim write is therefore two
// phases: read the holder and take the liveness snapshot with nothing held (judgeHolder),
// then lock the row, re-read it, and decide with claimBlockedBy under that lock (claimWrite);
// a holder that changed in between sends the write around the cycle once more.

// claimHolder names a holder in one phrase: a human by login, a session by its id and the
// title it is running under, the way the dashboard labels a session. liveTitle is the
// listener's own title for that session when the caller has it; it wins over the title the
// session stamped on the claim, which may be hours old.
func claimHolder(actor model.Actor, liveTitle string) string {
	if actor.Kind != "session" {
		return actor.Kind + " " + actor.ID
	}
	title := strings.TrimSpace(liveTitle)
	if title == "" && actor.Origin != nil {
		title = strings.TrimSpace(actor.Origin.SessionTitle)
	}
	if title == "" {
		return "session " + actor.ID
	}
	return "session " + actor.ID + " (" + title + ")"
}

// claimConflict refuses to take a claim its live holder still has. It carries the claim so
// the 409 names the holder, its title, and when it claimed, and a client renders that without
// a second read.
type claimConflict struct {
	claim model.IssueClaim
	// liveTitle is the title the listener reported for the holder, when this refusal came from
	// a liveness lookup. It is empty when the refusal came from a holder that changed under
	// the lock, where naming it would mean another listener call; the stamped title is used
	// then, so this label is the dashboard's rule only where the live title is in hand.
	liveTitle string
}

func (c *claimConflict) Error() string {
	return fmt.Sprintf(
		"%s claimed this issue at %s and is still running; ask that session to release it, or a human can force the claim",
		claimHolder(c.claim.Actor, c.liveTitle), c.claim.At.UTC().Format(time.RFC3339),
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

// liveSessions is a snapshot of the Envoy listener's live sessions, fetched with no
// transaction open and no pooled connection held. `fetched` is false when the caller never
// needed it, which is every claim of an issue nobody else holds.
type liveSessions struct {
	fetched bool
	byID    map[string]envoy.Session
}

// errHolderUnjudged says the decision needs a liveness snapshot this caller did not take,
// because the holder appeared (or changed) after the pre-lock read. The write redoes its
// cycle rather than deciding from a snapshot that does not describe the holder.
var errHolderUnjudged = errors.New("claim holder was not covered by the liveness snapshot")

// fetchLiveSessions asks the listener who is running. It must be called with nothing held: it
// is a cross-service HTTP call of up to five seconds, and production runs four pool
// connections, so a held connection or row lock across it stalls unrelated requests. An
// unreachable or unconfigured listener is a 503, never "unknown, therefore free" — Dispatch
// does not hand one session's work to another on a transport error.
func (s *server) fetchLiveSessions(ctx context.Context, holder model.IssueClaim) (liveSessions, error) {
	if s.deps.Envoy == nil {
		return liveSessions{}, errorf(http.StatusServiceUnavailable, "ENVOY_UNAVAILABLE",
			"ENVOY_URL is not configured, so Dispatch cannot tell whether %s is still working", claimHolder(holder.Actor, ""))
	}
	sessions, err := s.deps.Envoy.Sessions(ctx)
	if err != nil {
		if errors.Is(err, envoy.ErrUnavailable) {
			return liveSessions{}, errorf(http.StatusServiceUnavailable, "ENVOY_UNAVAILABLE", "%s", err.Error())
		}
		return liveSessions{}, err
	}
	byID := make(map[string]envoy.Session, len(sessions))
	for _, session := range sessions {
		byID[session.SessionID] = session
	}
	return liveSessions{fetched: true, byID: byID}, nil
}

// claimBlockedBy is the whole taking rule, and it runs under the issue's row lock: the caller
// brings the liveness snapshot, this decides. It returns the conflict that stops actor from
// taking or clearing current, nil when nothing does, or errHolderUnjudged when the snapshot
// cannot speak for this holder.
//
// A human's claim has no session to end, so it is held until that human or another releases
// it. A session's claim is held only while the listener still lists that session — the same
// live list GET /api/v1/agents and the issue subscribers read. humanOverride is the one
// difference between the callers: releasing a claim is any human's to do, while claiming over
// a live holder takes an explicit force.
func claimBlockedBy(
	current *model.IssueClaim, actor model.Actor, humanOverride bool, live liveSessions,
) error {
	if current == nil || current.Actor.SameAs(actor) {
		return nil
	}
	if humanOverride && actor.Kind == "user" {
		return nil
	}
	if current.Actor.Kind != "session" {
		return &claimConflict{claim: *current}
	}
	if !live.fetched {
		return errHolderUnjudged
	}
	session, live_ := live.byID[current.Actor.ID]
	if !live_ {
		return nil
	}
	return &claimConflict{claim: *current, liveTitle: session.Title}
}

// judgeHolder is the pre-lock half: read who holds the issue on a pooled connection, return
// that connection, and — only when somebody else holds it — take the liveness snapshot with
// nothing held. It decides nothing; claimBlockedBy does, under the lock.
func (s *server) judgeHolder(
	ctx context.Context, key string, actor model.Actor, humanOverride bool,
) (*model.IssueClaim, liveSessions, error) {
	holder, err := s.readIssueClaim(ctx, key)
	if err != nil {
		return nil, liveSessions{}, err
	}
	if holder == nil || holder.Actor.SameAs(actor) || (humanOverride && actor.Kind == "user") ||
		holder.Actor.Kind != "session" {
		return holder, liveSessions{}, nil
	}
	live, err := s.fetchLiveSessions(ctx, *holder)
	if err != nil {
		return nil, liveSessions{}, err
	}
	return holder, live, nil
}

// readIssueClaim reads one issue's claim on a pooled connection.
func (s *server) readIssueClaim(ctx context.Context, key string) (*model.IssueClaim, error) {
	var claim claimScan
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select `+issueClaimColumns+` from issues i where i.key = $1
	`, key).Scan(claim.targets()...); err != nil {
		return nil, err
	}
	return claim.resolve(key)
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

// claimEvent is the issue.claimed or issue.released record: what the claim became, whose claim
// it replaced or cleared, and why.
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

// claimChange is what the locked decision asks for: the claim to store (nil clears it), the
// event to append, whose claim it replaced, and why. changed false means there is nothing to
// write; retry means the holder is one the liveness snapshot cannot speak for, so the write
// goes around the cycle again.
type claimChange struct {
	claim     *model.IssueClaim
	eventType string
	previous  *model.IssueClaim
	reason    string
	changed   bool
	retry     bool
}

// claimApplied is the outcome of one locked attempt.
type claimApplied struct {
	issue     model.Issue
	event     model.Event
	retry     bool
	unchanged bool
}

// claimWrite is the locked half of every claim write, and the only place that opens a
// transaction: lock the issue, re-read it, hand the decision to `decide` (which calls the one
// taking rule, claimBlockedBy, with the snapshot its caller fetched), and commit. No listener call happens inside this function, so no pool
// connection is ever held across one.
func (s *server) claimWrite(
	ctx context.Context,
	key string,
	actor model.Actor,
	decide func(before model.Issue) (claimChange, error),
) (claimApplied, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return claimApplied{}, err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `select key from issues where key = $1 for update`, key).Scan(new(string)); err != nil {
		return claimApplied{}, err
	}
	before, err := s.loadIssue(ctx, tx, key)
	if err != nil {
		return claimApplied{}, err
	}
	if before.ClosedAt != nil {
		return claimApplied{}, errorf(http.StatusConflict, "ISSUE_CLOSED", "issue is closed")
	}
	change, err := decide(before)
	if err != nil {
		return claimApplied{}, err
	}
	if change.retry {
		return claimApplied{retry: true}, nil
	}
	if !change.changed {
		if err := tx.Commit(ctx); err != nil {
			return claimApplied{}, err
		}
		return claimApplied{issue: before, unchanged: true}, nil
	}
	if err := writeIssueClaim(ctx, tx, key, change.claim); err != nil {
		return claimApplied{}, err
	}
	after, err := s.loadIssue(ctx, tx, key)
	if err != nil {
		return claimApplied{}, err
	}
	after.LastSeq++
	event, err := s.appendEvent(ctx, tx, claimEvent(change.eventType, key, actor, after, change.previous, change.reason))
	if err != nil {
		return claimApplied{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return claimApplied{}, err
	}
	return claimApplied{issue: after, event: event}, nil
}

// claimAttempts is the judge-then-lock cycle: one redo when the holder changes under the lock,
// because a second change means a genuinely contended issue whose current holder the caller
// should simply be told about.
const claimAttempts = 2

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
	key := r.PathValue("key")
	for attempt := 1; attempt <= claimAttempts; attempt++ {
		_, live, err := s.judgeHolder(r.Context(), key, actor, input.Force)
		if err != nil {
			s.writeClaimError(w, err)
			return
		}
		applied, err := s.claimWrite(r.Context(), key, actor, func(before model.Issue) (claimChange, error) {
			blocked := claimBlockedBy(before.Claim, actor, input.Force, live)
			if errors.Is(blocked, errHolderUnjudged) {
				return claimChange{retry: true}, nil
			}
			if blocked != nil {
				return claimChange{}, blocked
			}
			if before.Claim != nil && before.Claim.Actor.SameAs(actor) {
				// This session already holds it: the claim keeps the time work started, and
				// the repeated claim stays out of the issue's log.
				return claimChange{}, nil
			}
			reason := "claimed"
			previous := before.Claim
			if previous != nil {
				reason = "takeover"
				if input.Force {
					reason = "forced"
				}
			}
			return claimChange{
				claim:     &model.IssueClaim{Actor: actor, At: time.Now().UTC()},
				eventType: "issue.claimed",
				previous:  previous,
				reason:    reason,
				changed:   true,
			}, nil
		})
		if err != nil {
			s.writeClaimError(w, err)
			return
		}
		if applied.retry {
			if attempt == claimAttempts {
				s.answerContendedClaim(w, r, key)
				return
			}
			continue
		}
		if applied.event.ID != 0 {
			s.publish(applied.event)
		}
		WriteJSON(w, http.StatusOK, applied.issue)
		return
	}
}

// answerContendedClaim ends the bounded cycle: after one redo the holder has changed twice, so
// the caller is told who holds it now. The holder comes from the row, not another listener
// lookup, so this answer costs no cross-service call.
func (s *server) answerContendedClaim(w http.ResponseWriter, r *http.Request, key string) {
	current, err := s.readIssueClaim(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if current == nil {
		writeError(w, "CLAIM_CONTENDED", http.StatusConflict,
			"the claim on "+key+" changed twice while this request ran; try again")
		return
	}
	s.writeClaimError(w, &claimConflict{claim: *current})
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
	key := r.PathValue("key")
	for attempt := 1; attempt <= claimAttempts; attempt++ {
		_, live, err := s.judgeHolder(r.Context(), key, actor, true)
		if err != nil {
			s.writeClaimError(w, err)
			return
		}
		applied, err := s.claimWrite(r.Context(), key, actor, func(before model.Issue) (claimChange, error) {
			if before.Claim == nil {
				return claimChange{}, nil
			}
			blocked := claimBlockedBy(before.Claim, actor, true, live)
			if errors.Is(blocked, errHolderUnjudged) {
				return claimChange{retry: true}, nil
			}
			if blocked != nil {
				return claimChange{}, blocked
			}
			return claimChange{
				eventType: "issue.released",
				previous:  before.Claim,
				reason:    "released",
				changed:   true,
			}, nil
		})
		if err != nil {
			s.writeClaimError(w, err)
			return
		}
		if applied.retry {
			if attempt == claimAttempts {
				s.answerContendedClaim(w, r, key)
				return
			}
			continue
		}
		if applied.event.ID != 0 {
			s.publish(applied.event)
		}
		WriteJSON(w, http.StatusOK, applied.issue)
		return
	}
}
