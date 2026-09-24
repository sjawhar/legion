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
// The taking rule lives in claimBlockedBy alone: a claim whose session the Envoy listener
// still lists as live belongs to that session until it releases it or a human forces it; a
// claim whose session is gone may be taken by any agent (Sami, 2026-09-24, on whether that
// needs asking: "Yes, automatically"), and the takeover is recorded on the issue and
// delivered to the session that lost it.
//
// Every claim write is the resolve-then-lock pair envoy_resolve.go owns, because the listener
// lookup never runs inside a transaction (fetchLiveSessions carries the reason): judgeHolder
// reads the holder and takes the liveness snapshot with nothing held, then claimWrite locks
// the row, re-reads it, and decides with claimBlockedBy under that lock; a holder that
// changed in between sends the write around the cycle once more. claimCycle is that pair and
// the answer both routes make of it.

// claimHolder names a holder in one phrase for an error message: a human by login, a session
// by its id and the title it is running under. It is not the dashboard's label (which shows
// the title alone, through @legion/contracts' actorLabel) — an API refusal names the session
// id too, because that is what the refused caller needs to reach it. liveTitle is the
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

// claimConflict refuses to take or clear a claim its holder still holds. It carries the claim
// so the 409 names the holder, its title and when it claimed, and so a caller that wants the
// claim itself has it without a second read.
type claimConflict struct {
	claim model.IssueClaim
	// liveTitle is the title the listener reported for the holder, and only a refusal that
	// came from a liveness lookup has one. A human holder is refused without any lookup and
	// leaves it empty, which costs nothing: claimHolder names a non-session by kind and id
	// and reads no title at all.
	liveTitle string
	// forcible is humanRule.forcible, carried through so the refusal offers only what its
	// caller can use.
	forcible bool
}

// Error is the refusal a caller reads, and it never says more than was established. A live
// session is named with the title it is running under and can be asked to release the issue;
// a human holder had no liveness to check and no session to message, so the text names the
// person to ask instead.
func (c *claimConflict) Error() string {
	at := c.claim.At.UTC().Format(time.RFC3339)
	alternative := "or any human can"
	if c.forcible {
		alternative = "or a human can force the claim"
	}
	if c.claim.Actor.Kind != "session" {
		return fmt.Sprintf("%s claimed this issue at %s; ask %s to release it, %s",
			claimHolder(c.claim.Actor, ""), at, c.claim.Actor.ID, alternative)
	}
	return fmt.Sprintf(
		"%s claimed this issue at %s and is still running; ask that session to release it, %s",
		claimHolder(c.claim.Actor, c.liveTitle), at, alternative,
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

// humanRule is how a route treats a human caller, the one difference between claiming and
// releasing. override is whether being human is itself enough to act over a live holder:
// releasing a claim is any human's to do, while claiming over a live holder takes an explicit
// {force: true}. forcible is whether the route has that force at all, so a refusal offers only
// what its caller can use — POST /api/v1/issues/{key}/claim has one, DELETE has none.
type humanRule struct {
	override bool
	forcible bool
}

// judgement is what the pre-lock half read: the holder it found and the liveness snapshot it
// took about that holder. The two travel together because the snapshot speaks for that holder
// alone; apart, they are two loose arguments a caller can pair wrongly.
type judgement struct {
	holder *model.IssueClaim
	live   liveSessions
}

// claimBlockedBy is the whole taking rule, and it runs under the issue's row lock: the caller
// brings what it judged before the lock, this decides against the row. It returns the conflict
// that stops actor from taking or clearing current, nil when nothing does, or errHolderUnjudged
// when the judgement cannot speak for the holder the row shows.
//
// A liveness snapshot speaks only for the holder that was read before it was taken, so the
// first check is that the row still shows that holder: a session-to-session change in between
// would otherwise be judged against a snapshot that predates the new holder, read as "not
// listed", and let a live session's claim be taken without force.
//
// A human's claim has no session to end, so it is held until that human or another releases
// it. A session's claim is held only while the listener still lists that session — the same
// live list GET /api/v1/agents and the issue subscribers read.
func claimBlockedBy(
	current *model.IssueClaim, actor model.Actor, rule humanRule, judged judgement,
) error {
	if judged.live.fetched && !sameHolder(judged.holder, current) {
		return errHolderUnjudged
	}
	if current == nil || current.Actor.SameAs(actor) {
		return nil
	}
	if rule.override && actor.Kind == "user" {
		return nil
	}
	if current.Actor.Kind != "session" {
		return &claimConflict{claim: *current, forcible: rule.forcible}
	}
	if !judged.live.fetched {
		return errHolderUnjudged
	}
	session, listed := judged.live.byID[current.Actor.ID]
	if !listed {
		return nil
	}
	return &claimConflict{claim: *current, liveTitle: session.Title, forcible: rule.forcible}
}

// sameHolder reports whether two nullable claims name the same holder.
func sameHolder(left, right *model.IssueClaim) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Actor.SameAs(right.Actor)
}

// judgeHolder is the pre-lock half: read who holds the issue on a pooled connection, release
// that connection, and take the liveness snapshot with nothing held — but only when the taking
// rule says it needs one, which it establishes by asking that rule about what it just read
// rather than by re-deriving when a holder must be judged. It decides nothing; claimBlockedBy
// does, under the lock, against the row as it stands then.
func (s *server) judgeHolder(
	ctx context.Context, key string, actor model.Actor, rule humanRule,
) (judgement, error) {
	holder, err := s.readIssueClaim(ctx, key)
	if err != nil {
		return judgement{}, err
	}
	judged := judgement{holder: holder}
	if !errors.Is(claimBlockedBy(holder, actor, rule, judged), errHolderUnjudged) {
		return judged, nil
	}
	judged.live, err = s.fetchLiveSessions(ctx, *holder)
	if err != nil {
		return judgement{}, err
	}
	return judged, nil
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
// write.
type claimChange struct {
	claim     *model.IssueClaim
	eventType string
	previous  *model.IssueClaim
	reason    string
	changed   bool
}

// claimApplied is the outcome of one locked attempt: the issue as it stands and the event to
// publish, which is the zero event when nothing was written.
type claimApplied struct {
	issue model.Issue
	event model.Event
}

// claimWrite is the locked half of every claim write - resolveThenLock's `write` - and the
// only place that opens a transaction: lock the issue, re-read it, hand the decision to
// `decide` (which calls the one taking rule, claimBlockedBy, with what its caller judged),
// and commit. No listener call happens inside this function, so no pool connection is ever
// held across one. A decision the judgement cannot make is errHolderUnjudged, which this
// reports as errStaleResolution once the rollback has run, for the cycle to redo.
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
	if err := tx.QueryRow(ctx, `select key from issues where key = $1 for no key update`, key).Scan(new(string)); err != nil {
		return claimApplied{}, err
	}
	before, err := s.loadIssue(ctx, tx, key)
	if err != nil {
		return claimApplied{}, err
	}
	change, err := decide(before)
	if errors.Is(err, errHolderUnjudged) {
		return claimApplied{}, errStaleResolution
	}
	if err != nil {
		return claimApplied{}, err
	}
	if !change.changed {
		if err := tx.Commit(ctx); err != nil {
			return claimApplied{}, err
		}
		return claimApplied{issue: before}, nil
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

// errClaimContended is what the bounded cycle answers with when the holder changed under
// every attempt. It never reaches a client as itself: claimCycle turns it into the
// CLAIM_CONTENDED answer answerContendedClaim writes.
var errClaimContended = errors.New("the issue's claim changed under every attempt")

// claimCycle is the whole of both claim routes past their own decision: resolve the holder
// and its liveness with nothing held, decide under the issue's row lock, and answer. The
// cycle is bounded at one redo (resolutionAttempts) because a holder that changes twice means
// a genuinely contended issue whose current holder the caller should simply be told about.
func (s *server) claimCycle(
	w http.ResponseWriter,
	r *http.Request,
	key string,
	actor model.Actor,
	rule humanRule,
	decide func(before model.Issue, judged judgement) (claimChange, error),
) {
	applied, err := resolveThenLock(r.Context(),
		func(ctx context.Context) (judgement, error) {
			return s.judgeHolder(ctx, key, actor, rule)
		},
		func(ctx context.Context, judged judgement) (claimApplied, error) {
			return s.claimWrite(ctx, key, actor, func(before model.Issue) (claimChange, error) {
				return decide(before, judged)
			})
		},
		errClaimContended,
	)
	if errors.Is(err, errClaimContended) {
		s.answerContendedClaim(w, r, key)
		return
	}
	if err != nil {
		s.writeClaimError(w, err)
		return
	}
	if applied.event.ID != 0 {
		s.publish(applied.event)
	}
	WriteJSON(w, http.StatusOK, applied.issue)
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
	key := r.PathValue("key")
	rule := humanRule{override: input.Force, forcible: true}
	s.claimCycle(w, r, key, actor, rule, func(before model.Issue, judged judgement) (claimChange, error) {
		if before.ClosedAt != nil {
			return claimChange{}, errorf(http.StatusConflict, "ISSUE_CLOSED", "issue is closed")
		}
		if blocked := claimBlockedBy(before.Claim, actor, rule, judged); blocked != nil {
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
}

// answerContendedClaim ends the bounded cycle: the holder changed twice while this request
// ran, so no verdict was applied. It is never ISSUE_CLAIMED — nothing on this path established
// that the holder is running, and naming a live title would mean another listener call — so it
// has its own code, and carries the claim the row shows for any caller that wants it.
func (s *server) answerContendedClaim(w http.ResponseWriter, r *http.Request, key string) {
	current, err := s.readIssueClaim(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	message := "the claim on " + key + " changed twice while this request ran, so nothing was applied; read the issue and try again"
	if current != nil {
		message = claimHolder(current.Actor, "") + " holds the claim on " + key +
			" as of this read; it changed twice while this request ran, so nothing was applied — read the issue and try again"
	}
	WriteJSON(w, http.StatusConflict, map[string]any{
		"error": message,
		"code":  "CLAIM_CONTENDED",
		"claim": current,
	})
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
	// Releasing a claim is any human's to do, and this route has no force to point a refused
	// caller at.
	rule := humanRule{override: true}
	s.claimCycle(w, r, key, actor, rule, func(before model.Issue, judged judgement) (claimChange, error) {
		if before.Claim == nil {
			// Nothing to release, including on a closed issue, whose close already cleared
			// the claim: the answer is the issue, so an agent releasing what it stopped
			// working never has to care which happened first.
			return claimChange{}, nil
		}
		if blocked := claimBlockedBy(before.Claim, actor, rule, judged); blocked != nil {
			return claimChange{}, blocked
		}
		return claimChange{
			eventType: "issue.released",
			previous:  before.Claim,
			reason:    "released",
			changed:   true,
		}, nil
	})
}
