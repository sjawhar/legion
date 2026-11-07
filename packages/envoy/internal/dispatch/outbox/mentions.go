package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
)

// fallbackClaim is how long one fallback pass's claim keeps a comment_fallback_copies row out of
// every other pass: the claim is a next_attempt_at of its own (claimFallbackCopies), so a pass
// that dies mid-decision leaves its rows claimed for no longer than this before the next pass
// takes them. A pass decides at most batchSize rows, each a read, at most a short transaction and
// one publish, so a healthy one finishes well inside it.
const fallbackClaim = time.Minute

// mentionAbandonedError is the error the outbox records on a mention attempt it settles failed
// because its send was abandoned (mentionComment.settleAbandoned).
const mentionAbandonedError = "the outbox published the comment's own copy instead; the send was abandoned"

// mentionDeliveries is what a new comment's mentions did, read from comment_deliveries when the
// event is published (loadMentionDeliveries). A mention resolved at write time names only a
// candidate: its send runs after the comment commits and can fail - the listener does not know the
// session, the receipt times out, the handler is cancelled, the session lacks the delivery mode -
// and a session whose mention failed has no copy but the outbox's.
type mentionDeliveries struct {
	// sessions is keyed by the session each attempt counts under (model.MentionSessionSQL).
	sessions mentionOutcomes
	// targets is keyed by each attempt's target as written, for a route suppressed by that string
	// alone (payloadSuppressedRouteTarget).
	targets mentionOutcomes
	// fallbackKeys is keyed like sessions, and holds for each session with a failed attempt - or
	// an abandoned one, which the outbox settles failed before it publishes the copy - the dedupe
	// key that attempt's send went under (contracts.AgentSendDedupeKey of
	// model.CommentMentionIdempotencyKey), which the session's copy carries
	// (destinations.addSession, publishFallbackCopies). A send that landed before its attempt
	// failed - a receipt timeout, or a send handed the listener before a kill whose resend then
	// failed - has already given the host that key, so the host drops the copy; a session no
	// mention reached shows it. Every attempt of one mention sends under that one key, which
	// carries the mention's own mode and no attempt's, so which of a target's failed attempts is
	// the latest, and what mode it went in, do not change it. A session whose failed attempts
	// span several targets keys by the lowest target. One key is enough: each target is a send of
	// its own, so a comment naming one session under two targets gives it one copy per target
	// that lands, and a copy shown beside another target's landed mention leaves the session no
	// more copies than had every one of its mentions landed.
	fallbackKeys map[string]string
	// settledKeys is keyed like sessions, and holds for each session a mention of which has
	// settled - one of its attempts sent, or every one of them failed or abandoned - the dedupe
	// key that mention's send went under (contracts.AgentSendDedupeKey of
	// model.CommentMentionIdempotencyKey), the key its host claimed when a send of the mention
	// landed and the key the session's own copy carries when none did (fallbackKeys). It is
	// written by loadMentionDeliveries, carried by a role route alone (destination.settledKeys),
	// and read where the session a role copy reaches is keyed by its own mention of this
	// comment: the forward to a live holder (roleFallbackKey) and the copy kept for a role no
	// live holder can take, which carries the whole map for whoever forwards it to the holder
	// that claims the role next (roleForward.keep, contracts.Envelope.MentionDedupeKeys).
	//
	// Only a settled mention is named. A mention still in flight, and one the comment names that
	// no attempt was recorded for, is left out: a copy carrying its key would claim that key at
	// the host, and the mention's own send, which goes under that same key, would then be
	// dropped and its mode - a steer among them - lost with it. A keep, whose copy carries the
	// whole map and whose holder is unknown, is therefore held back while an attempt of the
	// comment is still live, so that the map it stores names every mention that settled within
	// the attempt's own lease (anyLive, resolveRoleHolders). A forward is held back the same way
	// while the holder it reaches has a mention of its own in flight, so that the key chosen for
	// it here is that mention's rather than none (mentionHolds.live,
	// roleCopyWaitsForAMention).
	//
	// A session mentioned under several targets keys by a delivered mention of it, the lowest
	// such target, and by the lowest failed target only when no mention of it delivered: the key
	// of a mention that never landed is one that host never claimed, so a copy carrying it would
	// be shown beside the mention it stands for (settledMention.beats).
	settledKeys map[string]string
	// targetFallbackKeys is keyed like targets, and holds for each target with a failed or
	// abandoned attempt that names a session the dedupe key of that target's mention (of the
	// lowest session, should its attempts name several), which a fallback copy keyed by that
	// target carries (publishFallbackCopies).
	targetFallbackKeys map[string]string
	// held holds each destination of the comment a fallback row owns (comment_fallback_copies):
	// the fallback pass publishes or settles it, and the event's own publish skips it
	// (destinations.addDestination).
	held map[string]struct{}
	// unpinned is what the attempts of the comment that name no session yet and whose send is
	// still coming decide (unpinnedRoleMentions): the roles they target and the state each of
	// those attempts is in, read here, and the holders those roles resolve to, which the batch
	// writes before it reads any destination (resolveUnpinnedRoleMentions). It is a pointer so
	// that the resolution reaches every reader of this reading, which is held by value.
	unpinned *unpinnedRoleMentions
}

// newMentionDeliveries is the empty reading of one comment's mentions, which every read fills.
func newMentionDeliveries() mentionDeliveries {
	return mentionDeliveries{
		sessions:           mentionOutcomes{kind: sessionMentions, states: map[string]mentionState{}},
		targets:            mentionOutcomes{kind: targetMentions, states: map[string]mentionState{}},
		fallbackKeys:       map[string]string{},
		settledKeys:        map[string]string{},
		targetFallbackKeys: map[string]string{},
		held:               map[string]struct{}{},
		unpinned:           newUnpinnedRoleMentions(),
	}
}

// of is the holds of kind's keys and the dedupe keys a copy each of them decides carries. A
// session's keys carry the comment's unpinned role mentions beside their own attempts' states,
// a target's its attempts' states alone: a target is a key, not a session (mentionHolds).
func (d mentionDeliveries) of(kind mentionKind) (mentionHolds, map[string]string) {
	if kind == targetMentions {
		return mentionHolds{outcomes: d.targets}, d.targetFallbackKeys
	}
	return d.holds(), d.fallbackKeys
}

// holds is the question every copy of the comment bound for a session asks of that session
// (mentionHolds): the states of its own attempts, and the hold of an attempt that names no
// session yet whose role it turns out to hold.
func (d mentionDeliveries) holds() mentionHolds {
	return mentionHolds{outcomes: d.sessions, unpinned: d.unpinned}
}

// anyLive reports whether an attempt of any of the comment's mentions is still live
// (mentionSending, model.MentionLiveSQL): a send within model.DeliveryClaimLapse of the attempt's
// creation that neither settled nor lapsed into the delivery sweep's hands. Both keyings are
// read, a session's attempts and a target's: an attempt whose session the sender has not resolved
// yet counts under its target alone, and names a session in settledKeys once it settles.
//
// It is what holds a role route's keep back for the comment's own mentions
// (resolveRoleHolders): the copy a keep stores carries every settled mention of the comment
// (settledKeys), and the session that claims the role next - any mentioned session among them -
// is keyed by the map on it, so a keep made while a mention is in flight stores a map that names
// that mention's session nowhere and shows it the comment a second time. The wait is the
// attempt's own lease and no longer: an attempt past it reads mentionResending or
// mentionAbandoned rather than live, and the keep proceeds with whatever has settled by then.
func (d mentionDeliveries) anyLive() bool {
	for _, outcomes := range [...]mentionOutcomes{d.sessions, d.targets} {
		for _, state := range outcomes.states {
			if state == mentionSending {
				return true
			}
		}
	}
	return false
}

// liveFor reports whether a copy of the comment bound for sessionID waits for a mention of it
// (mentionSending, which model.MentionLiveSQL bounds by the attempt's lease): one of that
// session's own attempts, one that names no session yet whose role this read resolved to it
// (mentionHolds.live), or one that names no session yet whose holder this read could name at all
// while no one key stands for that mention (mentionHolds.waitsForAnUnnamedMention). It is what
// holds a fallback pass's forward back from a live holder (decideFallbackCopy): the copy would
// carry the row's key under contracts.RoleForwardDedupePrefix, a key that host never claimed,
// while a mention of the same comment is on its way to that session - so the host is shown the
// comment twice, once for the copy and once for the mention that lands after it. The wait is
// narrow on purpose: a holder no mention of the comment can reach takes the copy at once, one the
// comment's single unpinned mention may yet be pinned to takes it under that mention's own key
// (roleFallbackKey), and the wait is the attempt's own lease and no longer, past which the
// forward goes with whatever has settled.
//
// A batch's own forward of a role route waits on the same rule, asked of the states its role
// destination carries rather than of this reading (destination.mentionStates,
// roleCopyWaitsForAMention): the batch holds one reading of the mentions for every event of the
// batch, and a destination carries the maps it is keyed from already.
func (d mentionDeliveries) liveFor(sessionID string) bool {
	holds := d.holds()
	return holds.live(sessionID) || holds.waitsForAnUnnamedMention(sessionID)
}

// mentionComment is the comment whose mention attempts a mentionDeliveries read, the
// comment.created event that created it, and the context and store a settle or a hold of one of
// its keys runs on (settleAbandoned, holdFallbackCopy). It lives as long as the one batch or
// fallback pass that read it.
type mentionComment struct {
	ctx   context.Context
	deps  Deps
	event model.Event
	id    string
}

// mentionKind is the key a comment's mention attempts are grouped under: the session each counts
// under (model.MentionSession), or its target as written.
type mentionKind int

const (
	sessionMentions mentionKind = iota
	targetMentions
)

// attemptKey is the SQL expression of a comment_deliveries row's key of this kind.
func (k mentionKind) attemptKey() string {
	if k == targetMentions {
		return "target"
	}
	return model.MentionSessionSQL("comment_deliveries")
}

// fallbackColumn is the comment_fallback_copies column that names a row's key of this kind.
func (k mentionKind) fallbackColumn() string {
	if k == targetMentions {
		return "target"
	}
	return "session_id"
}

// mentionState is what the attempts under one key of mentionDeliveries came to. The states are
// in the order a key whose attempts came to several counts as the first of them.
type mentionState int

const (
	// mentionSent: an attempt reached the session, so the mention delivered the comment.
	mentionSent mentionState = iota
	// mentionResending: an attempt past the lease the outbox waits a send out for
	// (model.MentionResumedSQL): the delivery sweep's to resend, or a queued send's to make, or
	// one a later attempt overtook, which its sender sends when its lane reaches it, or settles
	// superseded once a later attempt has delivered the comment.
	mentionResending
	// mentionSending: an attempt whose send is live (model.MentionLiveSQL).
	mentionSending
	// mentionAbandoned: an attempt whose send was abandoned (model.MentionAbandonedSQL).
	mentionAbandoned
	// mentionFailed: every attempt failed, or none was recorded.
	mentionFailed
)

// mentionStateOf is the state of a key whose attempts hold a sent one, one being resent, one
// being sent and an abandoned one, as each is true.
func mentionStateOf(sent, resending, sending, abandoned bool) mentionState {
	switch {
	case sent:
		return mentionSent
	case resending:
		return mentionResending
	case sending:
		return mentionSending
	case abandoned:
		return mentionAbandoned
	}
	return mentionFailed
}

// mentionOutcomes is what the attempts under each key of one kind came to.
type mentionOutcomes struct {
	kind mentionKind
	// states holds the state of each key with an attempt; a key with none is mentionFailed.
	states map[string]mentionState
}

// state is the state of key's attempts.
func (o mentionOutcomes) state(key string) mentionState {
	if state, ok := o.states[key]; ok {
		return state
	}
	return mentionFailed
}

// unpinnedRoleMentions is what the attempts of one comment that name no session yet decide. The
// attempt of an @role: mention whose listener lookup went unanswered within the comment's 250 ms
// window is written with its target alone and session_id null, and its sender pins the session
// on the row a round trip later (coalesce(session_id, $6) in
// internal/dispatch/api/comment_delivery.go), so model.MentionSession counts it under no session
// and the attempts name the host that mention is on its way to nowhere. The outbox resolves the
// role itself instead, through the resolution its role routes already run
// (resolveUnpinnedRoleMentions, resolveRole), and the copies bound for that holder follow that
// attempt's own state, exactly as the copies of a session the attempts do name follow theirs.
//
// roles is what the unpinned attempts of each role came to, read with the attempts themselves
// (loadMentionDeliveries): their state, and the key the mention's own send carries at whichever
// session its sender pins (unpinnedRole). holders is that state under the session each role
// resolved to, whose copies read it. unnamed is the state of the roles whose holder the
// resolution could not name at all - no claim stands for one there, or its claim could not be
// read - which the copies bound for the sessions the owner's own work names read, and they
// alone (destinations.addWorkingSession). mentionFailed is this reading's "no such attempt":
// the state of a key with no attempt at all, which decides nothing, and what unnamed holds
// until a role goes unnamed.
//
// Only an attempt whose send is still coming is recorded (mentionSending, mentionResending): a
// live one, whose copies wait for it as a live pinned attempt's copies do, and one past the
// lease the outbox waits a send out for, whose copies are kept as comment_fallback_copies rows
// and published by the pass that reads the comment's attempts as they stand when it publishes
// them (destinations.leftTo, publishFallbackCopies). An attempt that settled names the session
// it reached itself, so no role is needed to say whose copies it decides.
//
// The resolution is a guess at the session the sender is about to pin, and the two are made by
// different readers a round trip apart - the outbox reads the role bucket itself, the sender
// asks the listener - so a role this reading names no holder for is not a role nobody holds,
// and a role it does name a holder for is not a role that holder keeps: the claim can move
// between the two readings. What the outbox does about each is on resolveUnpinnedRoleMentions,
// whose doc holds the measurements that chose it.
//
// The wait a live attempt puts on a copy is that attempt's own lease and nothing longer
// (model.MentionLiveSQL, model.DeliveryClaimLapse): past it the attempt reads resumed or
// abandoned rather than live, and the copy is the fallback row's or the event's own, never a
// wait of the event's.
type unpinnedRoleMentions struct {
	roles   map[string]unpinnedRole
	holders map[string]mentionState
	unnamed mentionState
}

// unpinnedRole is what one comment's attempts of one role that name no session yet came to: the
// first state they are in, by the rule every other keying of them follows
// (mentionOutcomes.record), and the idempotency key every send of that mention goes under
// (model.CommentMentionIdempotencyKey, the comment, the target as written and the mention's own
// mode), which names no session and so is the same key wherever the sender pins it. That is
// what lets a copy the outbox publishes to a session carry the key the mention would carry if
// it were pinned there (unpinnedRoleMentions.keyFor).
type unpinnedRole struct {
	state          mentionState
	idempotencyKey string
}

// newUnpinnedRoleMentions is the empty reading, which loadMentionDeliveries fills with the roles
// and resolveUnpinnedRoleMentions with their holders. unnamed starts at mentionFailed, the
// state that decides nothing, which is what a reading whose every role resolved holds.
func newUnpinnedRoleMentions() *unpinnedRoleMentions {
	return &unpinnedRoleMentions{
		roles:   map[string]unpinnedRole{},
		holders: map[string]mentionState{},
		unnamed: mentionFailed,
	}
}

// record names target's role as one an attempt of the comment in state and naming no session
// targets, in mode, counting it under the first state that role's attempts came to, the rule
// every other keying of them follows (mentionOutcomes.record). commentID, target and mode are
// the mention's own idempotency key, which every send of it goes under whichever session its
// sender pins (unpinnedRole). A target that is no role is not recorded: a session:<id> target
// names its session already (model.MentionSession), and nothing else a mention is written for
// resolves to one.
func (u *unpinnedRoleMentions) record(commentID, target, mode string, state mentionState) {
	parsed, err := model.ParseRoute(target)
	if err != nil || parsed.Kind != "role" {
		return
	}
	if current, ok := u.roles[parsed.ID]; ok && current.state <= state {
		return
	}
	u.roles[parsed.ID] = unpinnedRole{
		state:          state,
		idempotencyKey: model.CommentMentionIdempotencyKey(commentID, target, mode),
	}
}

// state is what the attempts of the comment that name no session yet say about the copies bound
// for sessionID: the state of the attempts of the roles this reading resolved to sessionID. A
// reading that resolved none of its roles to it answers mentionFailed, which decides nothing:
// that is every comment whose mentions the sender resolved, every event that is no comment, and
// every session but the holders this reading could name.
//
// The roles this reading could name no holder for are not in it: no copy bound for a session's
// own topic can be told from that holder's by this state, so the copies that read it are the
// ones the owner's own work names and nothing else (unnamedState, destinations.leftTo).
func (u *unpinnedRoleMentions) state(sessionID string) mentionState {
	if u == nil {
		return mentionFailed
	}
	if held, ok := u.holders[sessionID]; ok {
		return held
	}
	return mentionFailed
}

// unnamedState is the state of the comment's unpinned attempts whose role this read could name
// no holder for, mentionFailed when it named one for every role. The copies that read it are
// the ones bound for a session the owner's own work names - the session its route names and the
// session that claims it - because a role token names a role of that owner's own tree and the
// session working the owner is the session the listener resolves such a role to
// (destinations.addWorkingSession, resolveUnpinnedRoleMentions).
func (u *unpinnedRoleMentions) unnamedState() mentionState {
	if u == nil {
		return mentionFailed
	}
	return u.unnamed
}

// keyFor is the dedupe key the one unpinned mention of this comment carries at sessionID: the
// key contracts.AgentSendDedupeKey mints of that session and the mention's own idempotency key,
// which is the key its send goes under wherever its sender pins it. A copy published under it
// claims it at that session's host, so whichever of the copy and the mention lands second is
// dropped there and the session is shown the comment once.
//
// The copy that carries it is a role route's forward to the holder of a role this read can name
// (roleFallbackKey), which is a session such a mention may yet be pinned to and the one the
// outbox can mint a key for rather than wait on. A copy bound for a session's own topic carries
// the event's own key or its own mention's and never this one: the session the mention's own
// role resolves to waits for that mention instead, mode and all, and a session the comment's
// thread names is one no mention of it is on its way to (destinations.addSession,
// resolveUnpinnedRoleMentions).
//
// It is "" for a comment with no live unpinned attempt and for one whose live unpinned attempts
// target two or more roles: one key cannot serve two mentions, since each carries a target of
// its own, and a copy keyed by one of them would stand beside the other. Those copies wait as
// they did before this key (mentionHolds.waitsForAnUnnamedMention).
func (u *unpinnedRoleMentions) keyFor(sessionID string) string {
	if u == nil || len(u.roles) != 1 || sessionID == "" {
		return ""
	}
	for _, role := range u.roles {
		return contracts.AgentSendDedupeKey(sessionID, role.idempotencyKey)
	}
	return ""
}

// mentionHolds is the one question both roads that hold a copy of a new comment back for a
// mention ask of one session: the states that session's own attempts came to
// (mentionDeliveries.sessions), and the hold of the comment's attempts that name no session yet
// (unpinnedRoleMentions). The copies bound for a session's own topic read it through
// destinations.leftTo (destinations.leftToMention), and the role forward that resolves a role
// topic to its holder's agent subject reads it through live (roleCopyWaitsForAMention), so that
// the two agree about an attempt whose session is not written yet: it leaves the session keying
// empty for the very holder it is on its way to, and a road that read that keying alone would
// send its copy beside the mention.
//
// The hold such an attempt puts on a session is the one its role resolved to and no other
// (unpinnedRoleMentions.state). What a role this read could name no holder for holds back is a
// second question, asked by the copies of the sessions the owner's own work names alone
// (unnamedState, destinations.addWorkingSession) and by a role route's forward while no one key
// stands for that mention (waitsForAnUnnamedMention); where one does, the forward carries it
// and goes at once (unpinnedRoleMentions.keyFor, roleFallbackKey).
//
// A target's keys carry no such hold (mentionDeliveries.of): a target names a key, not a
// session, and the copy it decides is the route's own topic (suppressesCurrentRoute).
type mentionHolds struct {
	outcomes mentionOutcomes
	unpinned *unpinnedRoleMentions
}

// state is the state key's copies of the comment follow: the first state key's own attempts and
// the comment's attempts that name no session yet whose role this read resolved to key came to,
// by the rule a key whose attempts came to several states counts under the first of them
// (mentionStateOf, mentionOutcomes.record). So a mention of that session that was sent or is
// being resent stands - the copy the one decides was delivered by the mention, and the copy the
// other decides is the resumed send's - and a key whose own attempts failed, or that has none at
// all, reads whatever the attempt that may yet be pinned to it reads: live, so its copy waits;
// past the lease, so its copy is held as a fallback row and the pass that publishes it reads the
// comment's attempts again (destinations.leftTo, publishFallbackCopies).
//
// An abandoned key held back this way is not settled while it waits (destinations.leftTo): the
// settle is the outbox's only once no send is coming, and an attempt that may yet be pinned to
// this very session is a send that is coming. It is settled by the pass that reads the key once
// that attempt has settled or been abandoned itself.
func (h mentionHolds) state(key string) mentionState {
	state := h.outcomes.state(key)
	if held := h.unpinned.state(key); held < state {
		return held
	}
	return state
}

// unnamedState is the state of the comment's unpinned attempts whose role this read could name
// no holder for (unpinnedRoleMentions.unnamedState), which the copies bound for a session the
// owner's own work names read beside their own key's state.
func (h mentionHolds) unnamedState() mentionState {
	return h.unpinned.unnamedState()
}

// waitsForAnUnnamedMention reports whether a copy of the comment bound for key's session, which
// its own attempts leave to this event, waits for an attempt that names no session yet and whose
// role this read could name no holder for: one is live, and no one key stands for it, which is
// the comment whose live unpinned attempts target two or more roles
// (unpinnedRoleMentions.keyFor). With one target the copy carries that mention's own key and
// goes in this pass instead (roleFallbackKey), and the copies bound for a session the owner's
// own work names wait whichever it is, no key of theirs being the mention's
// (destinations.addWorkingSession, destinations.leftTo).
func (h mentionHolds) waitsForAnUnnamedMention(key string) bool {
	return h.unnamedState() == mentionSending && h.unpinned.keyFor(key) == ""
}

// live reports whether a mention of the comment bound for key's session is still being sent
// (mentionSending): the one rule every copy that waits for a mention of its own key asks
// (mentionDeliveries.liveFor, roleCopyWaitsForAMention). A key with no attempt at all and no
// unpinned attempt of the comment resolved to it is mentionFailed and never live, as is one
// whose attempts are past the lease the outbox waits a send out for.
func (h mentionHolds) live(key string) bool {
	return h.state(key) == mentionSending
}

// record adds what the attempts of one row of mentionDeliveryQuery came to under key: the first
// state of those key's rows came to.
func (o mentionOutcomes) record(key string, state mentionState) {
	if current, ok := o.states[key]; !ok || state < current {
		o.states[key] = state
	}
}

// copyHold is what the mention rule does with one copy of a new comment bound for a session's
// own topic (destinations.leftTo).
type copyHold int

const (
	// copyGoes: no mention of the comment decides this copy, so the event publishes it, under
	// the event's own key or the failed mention's (destinations.addSession).
	copyGoes copyHold = iota
	// copyLeftToTheMention: the mention delivers the comment to that session, or the fallback
	// row this left behind does, so the copy is left out of the event's list and holds none of
	// the owner's later events back (batchEvent.mentionPending, planChains).
	copyLeftToTheMention
	// copyWaitsForAnUnnamedMention: an attempt of the comment that names no session yet, and
	// whose role this read could name no holder for, may yet be pinned to this session. Nothing
	// says that mention delivers the comment here, so the copy stays in the event's list marked
	// waiting, as a role route's copy does: it makes no send this read and holds its key against
	// the owner's later events, so none of them reaches that session before it
	// (destination.waiting, planChains).
	copyWaitsForAnUnnamedMention
)

// leftToMention is the one rule every copy of a new comment bound for a session's own topic
// applies (destinations.addSession): it reports whether that copy, on that session's own topic,
// is left to the comment's mention of the session (destinations.leftTo).
//
// The comment's attempt rows decide it, never the payload's mentions. An unanswered @role:
// mention names no session in the payload - its listener lookup went unanswered within the
// 250 ms window - and its sender pins the session on the comment_deliveries row after the
// comment commits (coalesce(session_id, $6), internal/dispatch/api/comment_delivery.go), so a
// rule read from the payload alone leaves that session's route, claimant, author and follower
// copies in the event's list beside the mention, under the event's own key, and its host is
// shown the comment twice. The attempts name it either way (model.MentionSessionSQL), and they
// are what the keys of the copies that do go out are read from already
// (mentionDeliveries.fallbackKeys, settledKeys).
//
// A session no attempt of the comment names reads mentionFailed, the state of a key with no
// attempt at all, so it is published to as if it had not been mentioned - unless an attempt of
// the comment that names no session yet resolves to it, which names that session where the
// attempts cannot (mentionHolds, unpinnedRoleMentions): its copy then follows that attempt as a
// copy of a session the attempts do name follows its own. A live one holds the copy back, for
// that attempt's own lease and no longer; one past that lease leaves the copy to the fallback
// pass, which reads the comment's attempts as they stand when it publishes the copy, by then
// naming the session the sender pinned.
//
// What a role no holder could be named for holds back is asked by the copies of the sessions the
// owner's own work names alone (destinations.workingSessionHold): this one asks nothing of it,
// so a session the comment reaches by its thread - an author, a follower, a document's author -
// is published to in the pass that reads it.
func (d *destinations) leftToMention(sessionID string) bool {
	return d.leftTo(d.mentions.holds(), sessionID, contracts.AgentSubject(sessionID), false) != copyGoes
}

// workingSessionHold is that same rule asked of a session the owner's own work names - the
// session its route names and the session that claims it (destinations.addWorkingSession) -
// which reads what the comment's unpinned attempts whose holder this read could not name hold
// back beside its own key's attempts.
func (d *destinations) workingSessionHold(sessionID string) copyHold {
	return d.leftTo(d.mentions.holds(), sessionID, contracts.AgentSubject(sessionID), true)
}

// leftTo reports what becomes of the copy of the comment at destination, which key's attempts
// decide. A destination a fallback row owns is the fallback pass's, so the copy is skipped. A
// mention that reached the session delivered the comment, so the copy is skipped. A
// mention still being sent may yet, so the copy is held back and the event marked mentionPending,
// to be decided again when it is next read (settleEvent blocks it for mentionPendingDelay). A
// mention past the lease that wait is bounded by (model.MentionResumedSQL: the delivery sweep
// resends it, a sender holds it queued still, or a later attempt overtook it) is that send's: the
// copy is skipped and held as a fallback row (mentionComment.holdFallbackCopy), which the fallback
// pass settles with nothing sent once that send lands and publishes under the mention's own key
// should it fail (publishFallbackCopies); an insert that fails leaves the
// copy out of the list and backs the event off, so it is read again. An attempt past that lease
// that names no session yet decides the copies of the session its role resolves to the same way,
// which is the one reading that names them at all (unpinnedRoleMentions): the row it holds is
// keyed by that session, and the pass reads the attempts again rather than the keys this batch
// read, which name no session while the sender has pinned none. A destination this event
// already reached gave the session a copy, so it needs no row. A mention whose send was abandoned
// is settled by the outbox first (mentionComment.settleAbandoned), and the copy follows what key's
// attempts are once that settle commits: one a sender claimed before the settle took its row is
// pending, so the copy is held back and the sender owns the outcome; one the settle took is
// failed, and the copy is the outbox's. Only one of the two acts on an attempt, so the session
// gets the mention or the copy, not both. A settle that fails holds the copy back and backs the
// event off, so it is read again. Any other key - every attempt failed, or none was recorded - is
// published to as if it had not been mentioned, except that a session copy for a failed attempt
// carries that attempt's dedupe key (destinations.addSession): a send that landed before its
// attempt failed has given the host the comment already, and the host drops the copy.
//
// unnamed is asked by the copies of a session the owner's own work names and by no other
// (destinations.workingSessionHold): a live attempt whose role this read could name no holder
// for may yet be pinned to that session, and the copy waits in the event's list for it rather
// than being left out of the list, since nothing here says that mention delivers the comment to
// this very session. Past that attempt's lease the copy is the fallback row's, exactly as a
// named holder's is, and the pass that publishes it reads the attempts as they stand then.
//
// A copy left to the mention is the one copy of the comment keyed by its own session, and the
// one copy left out of the event's destinations while it waits: the mention delivers the comment
// to that very session, so a copy so left out holds none of the owner's later events back. A
// copy that waits for an attempt no holder could be named for, and a role route's copy, stay in
// the list instead, holding their key, nothing else delivering them (destination.waiting,
// resolveRoleHolders, planChains): a keep for a role nobody holds waits on every mention of the
// comment, no key of the session it reaches being known yet (mentionDeliveries.anyLive), and a
// forward on the one mention of the holder it reaches (mentionHolds.live).
func (d *destinations) leftTo(holds mentionHolds, key, destination string, unnamed bool) copyHold {
	if _, ok := d.mentions.held[destination]; ok {
		return copyLeftToTheMention
	}
	state := holds.state(key)
	if state == mentionAbandoned {
		settled, err := d.comment.settleAbandoned(holds.outcomes.kind, key)
		if err != nil {
			d.failed = append(d.failed, err)
			holds.outcomes.states[key] = mentionSending
			return copyLeftToTheMention
		}
		state = settled
		holds.outcomes.states[key] = settled
	}
	if unnamed {
		if held := holds.unnamedState(); held < state {
			if held == mentionSending {
				d.mentionPending = true
				return copyWaitsForAnUnnamedMention
			}
			state = held
		}
	}
	switch state {
	case mentionSent:
		return copyLeftToTheMention
	case mentionSending:
		d.mentionPending = true
		return copyLeftToTheMention
	case mentionResending:
		if _, ok := d.delivered[destination]; ok {
			return copyLeftToTheMention
		}
		if err := d.comment.holdFallbackCopy(holds.outcomes.kind, key, destination); err != nil {
			d.failed = append(d.failed, err)
			return copyLeftToTheMention
		}
		d.mentions.held[destination] = struct{}{}
		return copyLeftToTheMention
	}
	return copyGoes
}

// suppressesCurrentRoute applies a create-time suppression to the route that was resolved
// alongside the mention, including a later direct route to that same resolved session. A route
// edit to any other target must still wake its new destination. A suppression that names the
// session the route resolved to when the comment was written (suppressed_route_session_id) holds
// only once the mention of that session delivered the comment, by the rule destinations.leftTo
// applies. One that names the route string alone, the target of a mention whose lookup went
// unanswered (payloadSuppressedRouteTarget), holds while the route is still that string and only
// once the mention of that target delivered the comment, the same rule keyed by the target. Either
// way the copy leftTo holds back is the route's own topic (routeTopic). A route whose mention
// failed is published to.
//
// This is what a role route's copy has: a route whose topic is a role's is published to that
// topic and forwarded from there, so addSession never sees it and the mention rule it applies to
// a session's own topic cannot hold it back. A session route needs no suppression of its own -
// addWorkingSession leaves that session's copy to its mention by the comment's attempts, whatever
// the payload records (destinations.leftToMention) - and this one is read for it all the same,
// since the attempts of a target no session is pinned to are keyed by that target alone.
//
// What a role no holder could be named for holds back is not read here: the suppression says the
// mention of that session, or of that target, delivered the comment, which only an attempt that
// names it can say. The route's own copy asks that question where it is added, and waits in the
// event's list rather than being suppressed out of it (destinations.addWorkingSession).
func suppressesCurrentRoute(payload any, current *string, d *destinations) bool {
	if !payloadBool(payload, "suppress_route") || current == nil || *current == "" {
		return false
	}
	topic := routeTopic(*current)
	if topic == "" {
		return false
	}
	sessionID := payloadString(payload, "suppressed_route_session_id")
	if sessionID == "" {
		target := payloadSuppressedRouteTarget(payload)
		return target != "" && *current == target &&
			d.leftTo(mentionHolds{outcomes: d.mentions.targets}, target, topic, false) != copyGoes
	}
	if *current != payloadString(payload, "suppressed_route") && *current != "session:"+sessionID {
		return false
	}
	return d.leftTo(d.mentions.holds(), sessionID, topic, false) != copyGoes
}

// routeTopic is the subject addRoute gives route's copy, "" for a route that does not parse or
// names neither a role nor a session.
func routeTopic(route string) string {
	parsed, err := model.ParseRoute(route)
	if err != nil {
		return ""
	}
	switch parsed.Kind {
	case "role":
		return contracts.RoleTopicPrefix + parsed.ID
	case "session":
		return contracts.AgentSubject(parsed.ID)
	}
	return ""
}

// payloadSuppressedRouteTarget is the mention target a comment payload suppresses the route copy
// by, string alone: suppressed_route, when suppress_route is set, no suppressed_route_session_id
// names a session, and one of the mentions' targets (mentions[].target) is that string - the route
// was that mention's target and its lookup went unanswered. It is "" for every other payload.
func payloadSuppressedRouteTarget(payload any) string {
	route := payloadString(payload, "suppressed_route")
	if !payloadBool(payload, "suppress_route") || route == "" || payloadString(payload, "suppressed_route_session_id") != "" {
		return ""
	}
	values, _ := payload.(map[string]any)
	mentions, _ := values["mentions"].([]any)
	for _, entry := range mentions {
		if payloadString(entry, "target") == route {
			return route
		}
	}
	return ""
}

// payloadHasMentions reports whether a comment payload names a mention at all (mentions[]), as
// the batch decodes the payload from JSON. It is the one reading of the payload the destinations
// make of a comment's mentions: it says whether the comment can have a comment_deliveries row at
// all, so a comment with none is read for none (eventDestinations). Which session each mention
// counts under the payload does not say - an unanswered @role: mention names none there, and its
// sender pins one on the attempt row after the commit - so the attempts decide every copy left to
// a mention (destinations.leftToMention).
func payloadHasMentions(payload any) bool {
	values, _ := payload.(map[string]any)
	mentions, _ := values["mentions"].([]any)
	return len(mentions) > 0
}

// holdFallbackCopy records the fallback row of the copy of the comment at destination, decided by
// the attempts under key of kind, unless the destination has one already (publishFallbackCopies).
// It commits before the batch that holds the copy back can mark the event published.
//
// Its insert runs inside the statement gate the batches of one scan share (lanes.statement): the
// scan's batches read and write on one tracked context, so a statement run beside another's would
// be refused a second connection (store.ErrNestedAcquire) and fail whichever of the two asked for
// it. The gate is a no-op for a batch run by itself and for the fallback pass, neither of which
// has lanes.
func (c mentionComment) holdFallbackCopy(kind mentionKind, key, destination string) error {
	if err := c.deps.lanes.statement(func() error {
		_, err := c.deps.Store.Pool.Exec(c.ctx, `
		insert into comment_fallback_copies (comment_id, destination, event_id, `+kind.fallbackColumn()+`)
		values ($1, $2, $3, $4)
		on conflict (comment_id, destination) do nothing
	`, c.id, destination, c.event.ID, key)
		return err
	}); err != nil {
		return fmt.Errorf("hold the fallback copy of comment %q at %s: %w", c.id, destination, err)
	}
	return nil
}

// settleAbandoned settles failed, with mentionAbandonedError, every attempt of the comment whose
// key of kind is key and whose send was abandoned (model.MentionAbandonedSQL), appends each one's
// comment.delivery receipt as the API's sender would (the comment's author its actor), and reports
// the state key's attempts are in then: sent, past the lease a send is waited out for, being
// sent, or failed. It is one transaction, and it takes the comment's owner lock before any
// attempt row, the order the API's claim and settle transactions take them in
// (claimCommentDeliveryAttempt, settleDeliveryAttempt in internal/dispatch/api), so it and a
// sender's claim of the same attempt run one after the other. A claim that commits first leaves
// the row claimed just now, so the update's guard no longer matches it and the attempt reads
// pending; an attempt this settles first is no longer pending when the claim reads it, and the
// claim takes it as settled and sends nothing. The receipts reach the event log in the same
// commit, so a reader sees the attempt fail, and the outbox publishes them like any event.
//
// The transaction runs inside the statement gate the batches of one scan share (lanes.statement):
// it waits on the owner's lock for as long as whoever holds that lock takes, and a batch beside it
// reading or recording meanwhile on the scan's one tracked context would be refused a second
// connection (store.ErrNestedAcquire), which fails that batch and the scan with it - or, the other
// way round, fails this settle and backs its event off. The gate is a no-op for a batch run by
// itself and for the fallback pass, neither of which has lanes. The receipts are published once
// the transaction has committed, outside the gate.
func (c mentionComment) settleAbandoned(kind mentionKind, key string) (state mentionState, err error) {
	ctx, deps, event, commentID := c.ctx, c.deps, c.event, c.id
	defer func() {
		if err != nil {
			err = fmt.Errorf("settle the abandoned mention attempts of comment %q for %q: %w", commentID, key, err)
		}
	}()
	var receipts []model.Event
	if err := deps.lanes.statement(func() error {
		tx, err := deps.Store.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		owner := model.Event{IssueKey: event.IssueKey, ArtifactID: event.ArtifactID}
		if err := deps.Broker.LockOwners(ctx, tx, owner); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
		update comment_deliveries set state = 'failed', error = $3
		where comment_id = $1 and `+kind.attemptKey()+` = $2 and state = 'pending' and `+model.MentionAbandonedSQL("comment_deliveries")+`
		returning target, attempt, delivery, session_id, reply_id::text
	`, commentID, key, mentionAbandonedError)
		if err != nil {
			return err
		}
		var askID *string
		if id := payloadString(event.Payload, "ask_id"); id != "" {
			askID = &id
		}
		settled, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.CommentDeliveryEventPayload, error) {
			receipt := model.CommentDeliveryEventPayload{
				CommentID: commentID, AskID: askID, State: "failed", Error: mentionAbandonedError,
			}
			err := row.Scan(&receipt.Target, &receipt.Attempt, &receipt.Delivery, &receipt.SessionID, &receipt.ReplyID)
			return receipt, err
		})
		if err != nil {
			return err
		}
		receipts = make([]model.Event, 0, len(settled))
		for _, payload := range settled {
			receipt := owner
			receipt.Type, receipt.Actor, receipt.Payload = "comment.delivery", event.Actor, payload
			appended, err := deps.Broker.Append(ctx, tx, receipt)
			if err != nil {
				return err
			}
			receipts = append(receipts, appended)
		}
		var sent, resending, pending bool
		if err := tx.QueryRow(ctx, `
		select coalesce(bool_or(state = 'sent'), false),
		       coalesce(bool_or(state = 'pending' and `+model.MentionResumedSQL("comment_deliveries")+`), false),
		       coalesce(bool_or(state = 'pending'), false)
		from comment_deliveries
		where comment_id = $1 and `+kind.attemptKey()+` = $2
	`, commentID, key).Scan(&sent, &resending, &pending); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		state = mentionStateOf(sent, resending, pending, false)
		return nil
	}); err != nil {
		return 0, err
	}
	for _, receipt := range receipts {
		deps.Broker.Publish(receipt)
	}
	return state, nil
}

// fallbackCopy is one comment_fallback_copies row a fallback pass claimed: the copy of the comment
// at destination, the comment.created event eventID's envelope, decided by the attempts under key
// of kind. attempts is how many of the row's passes have failed so far (attempt_count), and claim
// the next_attempt_at the pass's claim set, which every write of the pass's record is guarded on
// (recordFallbackCopies).
type fallbackCopy struct {
	commentID   string
	destination string
	eventID     int64
	key         string
	kind        mentionKind
	attempts    int
	claim       time.Time
}

// fallbackEvent is the comment.created event a fallback copy is the envelope of, and the slug of
// its artifact (envelope).
type fallbackEvent struct {
	event model.Event
	slug  string
}

// fallbackRecord is what a fallback pass came to for one row: settled, and published when it
// published the copy; failed when its decision or publish failed, which counts an attempt on the
// row and backs it off (failedFallbackCopy). A record neither settled nor failed releases the
// row's claim for a later pass, counting no attempt, and delay is how long the row waits before
// it is due again: mentionPendingDelay while its mention is still being sent, retry.BaseDelay
// while NATS is unreachable, and none at all when the publisher hold's deadline fenced the send
// or the process is stopping, where whatever pass runs next takes the row at once.
type fallbackRecord struct {
	held      fallbackCopy
	settled   bool
	published bool
	failed    bool
	delay     time.Duration
}

// failedFallbackCopy is the record of a row whose decision or publish failed: one more attempt
// counted on it, and its next attempt retry.Delay of that count away, the schedule an event backs
// off by (backOff). Nothing about a publish failing is permanent, so the row is never given up:
// the attempt that brings its count to retry.DeadLetterAttempts, the threshold the event scan logs
// at, is logged at Error, and the row is retried on the same schedule after it.
func failedFallbackCopy(held fallbackCopy) fallbackRecord {
	if attempts := held.attempts + 1; attempts == retry.DeadLetterAttempts {
		slog.Error("dispatch outbox: fallback copy reached dead-letter threshold",
			"comment_id", held.commentID, "destination", held.destination, "attempts", attempts)
	}
	return fallbackRecord{held: held, failed: true}
}

// publishFallbackCopies is the fallback pass. This is the one description of how a mentioned
// session gets a new comment once - by its mention, or by the outbox's copy - however the
// mention's send ends; every other description points here.
//
// A comment's mention is sent by the API after the comment commits (mentionSender in
// internal/dispatch/api), and every destination of the comment.created event that would reach a
// mentioned session - the route, the claimant, an author, a follower - leaves that session's copy
// to the mention by the rule destinations.leftTo applies to the attempts the session counts under
// (model.MentionSession). A sent attempt delivered the comment, so the copy is skipped; a live
// one (model.MentionLiveSQL) holds the copy back and blocks the event for mentionPendingDelay, for
// the model.DeliveryClaimLapse after its comment committed and no longer; a failed one leaves the
// copy to the outbox, under the failed send's dedupe key. An abandoned attempt
// (model.MentionAbandonedSQL) is settled failed by the outbox, with its comment.delivery receipt,
// before it publishes the copy (mentionComment.settleAbandoned); a sender's claim of it runs
// before or after that settle under the comment's owner lock, so whichever commits first acts on
// the attempt and the other finds it taken.
//
// A mention attempt past that lease (model.MentionResumedSQL) is a send the outbox waits no
// longer on: the delivery sweep resends it (resumeOwedDeliveries in internal/dispatch/api) under
// the attempt's own idempotency key once no sender holds it - the process that queued it died
// with the send in flight, or never started it, perhaps after the listener took it, and the
// listener drops the resend as a repeat when it did - or a live sender holds it, queued behind a
// lane its recipient has held longer than a lease, and sends it when that lane reaches it, or
// settles it superseded without sending when a Retry from the card has delivered the comment
// meanwhile (supersedeCommentDelivery). The outbox cannot tell which, and holding the comment's
// event for a lane's minutes would be no bound at all, so it neither publishes that copy nor
// waits on the send. It records the copy it held back as a comment_fallback_copies row instead,
// keyed by the comment and the exact destination topic the copy would have gone to and naming the
// key whose attempts decide it (the session, or the target of a route suppressed by that string
// alone), inserted idempotently before the batch can mark the event published; an insert that
// fails backs the event off, so it is read again. From then on the row owns that destination: the
// event's own publish skips it on every retry (mentionDeliveries.held,
// destinations.addDestination), so the event never sends it too, and the event is published and
// recorded as it would be had the mention been sent.
//
// That is the whole of how a copy published past that lease is keyed, and it is why the key is
// the pass's to read and not the batch's. An attempt past the lease that names no session yet -
// an unanswered @role: mention whose sender has not reached its lane - names the host it is on
// its way to nowhere, so the batch's own reading keys that host's copy by nothing and would
// publish it under the event's own key, beside the mention that lands after it. The row the
// batch holds instead is keyed by the session the attempt's role resolves to
// (unpinnedRoleMentions, resolveUnpinnedRoleMentions), and the pass reads the comment's attempts
// again when it publishes it, by then naming the session the sender pinned: the row is settled
// with nothing sent when that send landed, and published under that mention's own key when it
// failed. Nothing waits longer for it than the event did: the event publishes at the lease, as
// it always has, and the row is the copy's, not the event's.
//
// The pass runs on every scan the publisher hold's holder makes, after the event scan
// (Outbox.serve), and under that hold: it claims a bounded batch of unsettled rows, oldest first
// (next_attempt_at pushed out by fallbackClaim under for update skip locked, so a second process
// skips them), and decides each by its key's attempts, read as leftTo reads them
// (mentionDeliveriesOf): a sent attempt delivered the comment, so the row is settled with nothing
// sent; an attempt still being sent or resent leaves the row to a later pass, its claim released
// for mentionPendingDelay, so the pass of every wake does not read it again; an abandoned attempt
// is settled failed first (mentionComment.settleAbandoned, which a sender's claim of it runs after
// or before under the owner lock) and the row decided again; and when no attempt was sent and none
// is pending, the comment.created event's envelope is published to the row's destination alone,
// carrying the dedupe key of the failed attempt's send (mentionDeliveries.fallbackKeys for a
// session, targetFallbackKeys for a target), and the row is marked published. A row whose
// destination is a role topic is not published there - the stream captures no role subject - but
// resolved and forwarded to the role's holder, as a batch's role route is (fallbackRoleForward,
// roleForward.send): a role no live holder can take has its copy kept on the role's kept lane for
// the holder that claims it next, and the stream's acknowledgement of that keep marks the row
// published as a holder's receipt of a forward does, and the copy it keeps carries every settled
// mention of the comment by session, so that whoever forwards it to the role's next holder keys
// it by that holder's own mention (roleForward.keep, contracts.Envelope.MentionDedupeKeys); a
// holder whose own mention of the comment settled - the session the row's key names, which a
// role fallback row's holder usually is, and any other session the comment mentions that holds
// the role by now alike - is forwarded to under that mention's own dedupe key rather than a role
// forward's prefixed one, which is what makes that session's host drop a copy of a mention it
// was given, and that is the one rule every forwarder of a role message follows
// (contracts.RoleForwardDedupeKey, roleFallbackKey, roleForward.forward); a resolution that
// reaches the destination with nothing to send at all - the holder is the event's own actor, or
// the role token is one no claim can ever name - settles
// the row unpublished, and one that leaves it pending counts an attempt and backs the row off, as
// a failed publish does. The pass never writes the events row:
// the event's published_at, published_destinations and payload stay as its own publish left
// them, and no destination but the row's is reached, whatever the issue's route,
// claimant, followers or authors are by then. A decision or a publish that fails counts an attempt
// on the row (attempt_count) and moves its next_attempt_at out by retry.Delay of that count, the
// schedule an event backs off by, so it is decided again once that passes; nothing about a publish
// failing is permanent, so the row is never given up, and the attempt that brings the count to
// retry.DeadLetterAttempts is logged at Error (failedFallbackCopy). One cut short by NATS being
// unreachable counts nothing - the outage is no fault of the row, as it is none of an event whose
// send met the connection (settleEvent) - and releases the claim for retry.BaseDelay, the wait a
// scan that failed is tried again after: the holder gives the hold up for NATS lost, and the
// first pass of whichever holder follows, armed to that wait (Outbox.nextRetry), takes the row.
// One cut short by the process stopping counts nothing and releases it at once too, for whatever
// process runs next, and a pass that starts once the process is already stopping claims nothing
// at all; a process that dies between the publish
// and the mark leaves the claim, and the repeat carries the same dedupe key, which the host, and
// the stream within its duplicate window, drop.
//
// The pass is fenced by the publisher hold as a batch is: it claims nothing once the hold's loss
// deadline has passed, sends through the hold's window (Publisher.WindowUntil), and leaves its
// record unwritten when that deadline passed during it, the record being the last act checked
// against it - the rows it claimed are then left claimed until fallbackClaim lapses, which the
// next holder's pass waits out. Every write of a pass's record is guarded on the claim the pass
// took (recordFallbackCopies), so a pass that outlasts fallbackClaim writes nothing to a row a
// later pass has claimed since.
//
// Races. A person's Retry of the mention opens or resumes an attempt the pass reads as being
// sent, so the row waits for it, and a Retry that is sent settles the row with nothing sent. A
// Retry that starts once the pass has decided to publish can deliver the mention and the copy
// both, under one dedupe key when it is sent in the failed attempt's mode, so the host shows one.
// A send the killed process had handed the listener before it died, whose resend then fails for
// good (the listener down after the restart), gives the host that mention and then the copy under
// the mention's own dedupe key, which the host drops. The API's failed settle appends a
// comment.delivery receipt, an event insert that wakes the holder, and the pass runs on the scan
// that wake starts.
//
// It reports what the holder does next, as a batch reports it (Outbox.scan): full, the claim
// filled its batch, so rows it did not reach are due now and the holder scans and passes again
// rather than waiting for a wake of its own - a fresh row's next_attempt_at is null, which no
// retry timer is armed to (Outbox.nextRetry); and failed, the claim itself failed, so the holder
// waits retry.BaseDelay before its next pass rather than spinning on a claim that keeps failing.
// A row the pass decided and failed needs neither: its record moves its next_attempt_at out, and
// the timer is armed to that.
func publishFallbackCopies(ctx context.Context, deps Deps, hold fence) (full, failed bool) {
	if ctx.Err() != nil {
		// Nothing this pass claimed could be decided: every row would be read and handed straight
		// back, and the events behind them read on a context that is already done. The rows are
		// left for whatever process runs next, as a pass that claimed them would leave them.
		return false, false
	}
	if !time.Now().Before(hold.Deadline()) {
		// Past the publisher hold's loss deadline this process publishes nothing: the hold is
		// another's to take, and its pass decides these rows.
		return false, false
	}
	copies, err := claimFallbackCopies(ctx, deps)
	if err != nil {
		slog.Error("dispatch outbox: claim fallback copies", "error", err)
		return false, true
	}
	if len(copies) == 0 {
		return false, false
	}
	full = len(copies) == batchSize
	var records []fallbackRecord
	// Written even when ctx has ended: a process stopping mid-pass records what NATS accepted and
	// releases what it did not decide, rather than leave those rows claimed for fallbackClaim.
	defer func() {
		if !time.Now().Before(hold.Deadline()) {
			slog.Error("dispatch outbox: the publisher hold's deadline passed before the fallback pass was recorded, so it is left unrecorded",
				"copies", len(records))
			return
		}
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		if err := recordFallbackCopies(record, deps, records); err != nil {
			slog.Error("dispatch outbox: record fallback copies", "error", err)
		}
	}()
	// release leaves the row to a later pass, counting no attempt: it is due again once delay has
	// passed, or at once when delay is none.
	release := func(held fallbackCopy, delay time.Duration) {
		records = append(records, fallbackRecord{held: held, delay: delay})
	}
	fail := func(held fallbackCopy) {
		if ctx.Err() != nil {
			release(held, 0)
			return
		}
		records = append(records, failedFallbackCopy(held))
	}
	events, err := loadFallbackEvents(ctx, deps, copies)
	if err != nil {
		slog.Error("dispatch outbox: read the events of fallback copies", "error", err)
		for _, held := range copies {
			fail(held)
		}
		return full, false
	}
	type sentCopy struct {
		held   fallbackCopy
		out    fallbackSend
		answer <-chan error
	}
	sender := deps.Publisher.WindowUntil(hold.Deadline)
	// One record for the pass, as a batch keeps one for its own window of sends (sendChains):
	// what the handovers behind this pass's keeps have already handed a role's holder.
	handed := newKeptHandovers()
	var sent []sentCopy
	for _, held := range copies {
		if ctx.Err() != nil {
			release(held, 0)
			continue
		}
		event, ok := events[held.eventID]
		if !ok {
			slog.Error("dispatch outbox: fallback copy names no event", "comment_id", held.commentID, "event_id", held.eventID)
			fail(held)
			continue
		}
		decision, out, err := decideFallbackCopy(ctx, deps, held, event)
		switch {
		case err != nil:
			slog.Error("dispatch outbox: decide fallback copy", "comment_id", held.commentID, "destination", held.destination, "error", err)
			fail(held)
		case decision == fallbackPublishes:
			sent = append(sent, sentCopy{held: held, out: out, answer: out.send(ctx, deps, sender, handed)})
		case decision == fallbackSettles:
			records = append(records, fallbackRecord{held: held, settled: true})
		default:
			// The key's mention is still being sent or resent: the row waits for it as an event
			// whose copy that mention holds back waits, rather than being claimed and read again
			// by every pass until the mention ends.
			release(held, mentionPendingDelay)
		}
	}
	for _, item := range sent {
		var err error
		// An answer already in is read before ctx is: a stopping process records what NATS
		// accepted.
		select {
		case err = <-item.answer:
		default:
			select {
			case err = <-item.answer:
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, bus.ErrRefused):
			deps.countPublishFailure("refused")
			slog.Error("dispatch outbox: fallback copy refused", "comment_id", item.held.commentID, "destination", item.held.destination, "error", err)
		case errors.Is(err, bus.ErrWindowDeadline):
			// The hold's deadline refused the send: it never left this process, so it is no
			// failure of the row and counts nothing. The record below is left unwritten too, so
			// the row keeps its claim until it lapses.
			release(item.held, 0)
			continue
		default:
			// Counted as a batch counts a destination that did not reach NATS (sendChains): a
			// role forward its holder's resolution held pending sent nothing at all and is
			// counted under that holder, a forward NATS took whose holder sent no receipt is
			// unanswered, and a send the server refused under this connection's own publish
			// permissions is a denial. An answer a stopping process stopped waiting for says
			// nothing of NATS and counts nothing.
			switch forward := item.out.forward; {
			case errors.Is(err, bus.ErrPublishDenied):
				deps.countPublishFailure("denied")
			case forward != nil && forward.err != nil:
				deps.countPublishFailure("holder")
			case errors.Is(err, bus.ErrReceiptTimeout) && !errors.Is(err, bus.ErrPublishUnconfirmed):
				deps.countPublishFailure("unanswered")
			case !lostToContextEnd(ctx, err):
				deps.countPublishFailure("failed")
			}
			slog.Error("dispatch outbox: publish fallback copy", "comment_id", item.held.commentID, "destination", item.held.destination, "error", err)
			if errors.Is(err, bus.ErrUnavailable) {
				release(item.held, retry.BaseDelay)
			} else {
				fail(item.held)
			}
			continue
		}
		records = append(records, fallbackRecord{held: item.held, settled: true, published: true})
	}
	return full, false
}

// fallbackDecision is what a fallback pass does with one row (decideFallbackCopy).
type fallbackDecision int

const (
	// fallbackWaits: the key's mention is still being sent or resent; the row waits for a later
	// pass.
	fallbackWaits fallbackDecision = iota
	// fallbackSettles: the key's mention was sent; the row is settled with nothing sent.
	fallbackSettles
	// fallbackPublishes: no attempt of the key was sent and none is pending; the copy is published.
	fallbackPublishes
)

// fallbackSend is the copy a pass publishes for one row: the envelope, whose dedupe key is the
// failed mention's own, and the forward that carries it when the row's destination is a role
// topic, nil for every other destination (fallbackRoleForward, decideFallbackCopy).
type fallbackSend struct {
	item    contracts.Envelope
	forward *roleForward
}

// send starts the copy's send through sender, within ctx: a role destination forwarded to the
// role's holder with a Request, or kept on the role's kept lane when no live holder can take it
// (roleForward.send), and every other destination published to its topic. handed is the pass's
// record of what the handovers behind its keeps have delivered, which a keep reads and writes
// (keptHandovers). A forward that resolved to no holder it could reach answers its own error, as
// a batch's chain does (sendChains), so the row counts an attempt and backs off.
//
// The copy carries the mention's own dedupe key, and a forward of it keys by the one rule both
// forwarders of a role message follow (contracts.RoleForwardDedupeKey, roleForward.forward): a
// holder whose own mention of this comment settled - the very session the row's key names, which
// a role fallback row's holder usually is, the row being held for a route a mention of the
// session that route resolved to suppressed, and any other mentioned session that holds the role
// by now alike - is forwarded that mention's key as it is, and its host drops a copy of a mention
// it was given, while a holder no mention of the comment settled for is forwarded the role
// forward's prefixed key, standing for the role's copy rather than that session's mention. A keep
// stores the copy under the row's own key and every settled mention's key beside it, for the
// role's next holder, where whoever forwards it on applies the same rule.
func (s fallbackSend) send(ctx context.Context, deps Deps, sender bus.Sender, handed *keptHandovers) <-chan error {
	switch {
	case s.forward == nil:
		return sender.PublishAsync(s.item)
	case s.forward.err != nil:
		return failedAnswer(s.forward.err)
	}
	return s.forward.send(ctx, deps, sender, s.item, handed)
}

// decideFallbackCopy decides one claimed row by its key's attempts (publishFallbackCopies), and
// returns, when the copy is to be published, the send to make: the envelope, under the dedupe
// key of the failed mention its key names (mentionDeliveries.fallbackKeys for a row keyed by a
// session, targetFallbackKeys for one keyed by a target), and the role forward that carries it
// to a role destination's holder. A role no live holder can take is published like any other
// destination: its forward keeps the copy on the role's kept lane for the role's next holder
// (forwardOf, keptOrSkipped).
// Only a resolution that reaches the destination with nothing to send at all - the holder is the
// event's own actor, or the role token is one no claim can ever name - settles the row as a sent
// mention does, with nothing published.
//
// A role destination's forward is given the comment's mentions by session, which this read holds
// already, as a batch gives them to the role route of its own event (resolveRoleHolders): the
// settled ones, which a keep stores on the copy it keeps, and the failed ones, which say what a
// session with no settled mention is owed. The copy a live holder is forwarded is keyed by that
// holder's own settled mention in place of the row's key, by the rule the event's own role route
// follows (roleFallbackKey), so a mentioned session that holds the role although the row's key
// names another - a second session this comment mentions, whose own mention landed - is shown
// the comment once rather than again under a key its host never claimed. A keep stays under the
// row's own key, the id every writer of that role's kept lane holds the copy under
// (bus.KeptMsgID), and carries the map instead.
func decideFallbackCopy(
	ctx context.Context, deps Deps, held fallbackCopy, event fallbackEvent,
) (fallbackDecision, fallbackSend, error) {
	deliveries, err := mentionDeliveriesOf(ctx, deps.Store.Pool, held.commentID)
	if err != nil {
		return 0, fallbackSend{}, err
	}
	// Whose copies the comment's attempts that name no session yet hold back, as the batch
	// reads it for its own events (resolveUnpinnedRoleMentions): this row's destination is one
	// of those copies when the key it names is such a holder.
	resolveUnpinnedRoleMentions(ctx, deps, deliveries)
	holds, keys := deliveries.of(held.kind)
	state := holds.state(held.key)
	if state == mentionAbandoned {
		comment := mentionComment{ctx: ctx, deps: deps, event: event.event, id: held.commentID}
		if state, err = comment.settleAbandoned(held.kind, held.key); err != nil {
			return 0, fallbackSend{}, err
		}
	}
	switch state {
	case mentionSent:
		return fallbackSettles, fallbackSend{}, nil
	case mentionResending, mentionSending:
		return fallbackWaits, fallbackSend{}, nil
	}
	item, err := envelope(event.event, event.slug)
	if err != nil {
		return 0, fallbackSend{}, err
	}
	item.Topic = held.destination
	if key, ok := keys[held.key]; ok {
		item.DedupeKey = key
	}
	if err := item.Validate(); err != nil {
		return 0, fallbackSend{}, fmt.Errorf("validate envelope: %w", err)
	}
	forward := fallbackRoleForward(ctx, deps, item, held.destination)
	if forward != nil {
		forward.mentionKeys, forward.settledKeys = deliveries.fallbackKeys, deliveries.settledKeys
		if forward.keptFor == "" {
			if deliveries.liveFor(forward.holder) {
				// The holder's own mention of this comment is still being sent, so the comment
				// is already on its way to that session and this copy would be the duplicate:
				// it would go under the row's key prefixed, which that host never claimed, and
				// the mention would land beside it. The row waits, bounded by that attempt's
				// own lease, and a holder with no live mention of its own takes the copy at
				// once, as a batch's own forward does (roleCopyWaitsForAMention).
				return fallbackWaits, fallbackSend{}, nil
			}
			if key := roleFallbackKey(forward.mentionKeys, forward.settledKeys, deliveries.unpinned, forward); key != "" {
				item.DedupeKey = key
			}
		} else if deliveries.anyLive() {
			// A keep of this pass waits on every mention of the comment, as a batch's own keep
			// does (roleCopyWaitsForAMention): the row's own key has lapsed past its lease,
			// which is what made the row, but another session's mention of the same comment can
			// still be live - a Retry opens a fresh attempt at any time - and a copy kept while
			// it is shows that session the comment a second time when it claims the role. The row
			// waits, bounded by that attempt's own lease, and is decided again when it is due.
			return fallbackWaits, fallbackSend{}, nil
		}
		if forward.skip != "" {
			slog.Info("dispatch outbox: a fallback copy's role destination is reached with nothing sent",
				"comment_id", held.commentID, "destination", held.destination, "reason", forward.skip)
			return fallbackSettles, fallbackSend{}, nil
		}
	}
	return fallbackPublishes, fallbackSend{item: item, forward: forward}, nil
}

// claimFallbackCopies claims at most batchSize unsettled fallback rows that are due - never
// claimed, or past the claim, the wait or the backoff their next_attempt_at holds - oldest first,
// in one statement: for update skip locked, so a second process's pass skips them, and
// next_attempt_at pushed out by fallbackClaim in the same breath, which each row returns as its
// claim. It reads the partial index of unsettled rows, so a pass with nothing to decide reads no
// row.
//
// The statement runs outside the pass's context, under claimTimeout of its own, as the record
// step does: a context that ends once it has committed would answer context canceled and lose the
// rows it claimed, which would then sit claimed for fallbackClaim with no pass deciding them. The
// pass reads its own ctx instead and releases every row it claimed at once (publishFallbackCopies).
func claimFallbackCopies(ctx context.Context, deps Deps) ([]fallbackCopy, error) {
	claim, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimTimeout)
	defer cancel()
	rows, err := deps.Store.Pool.Query(claim, `
		update comment_fallback_copies set next_attempt_at = now() + $2
		where (comment_id, destination) in (
			select comment_id, destination from comment_fallback_copies
			where settled_at is null and (next_attempt_at is null or next_attempt_at <= now())
			order by created_at
			limit $1
			for update skip locked
		)
		returning comment_id::text, destination, event_id, session_id, target, attempt_count, next_attempt_at
	`, batchSize, fallbackClaim)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (fallbackCopy, error) {
		var held fallbackCopy
		var sessionID, target *string
		if err := row.Scan(
			&held.commentID, &held.destination, &held.eventID, &sessionID, &target, &held.attempts, &held.claim,
		); err != nil {
			return fallbackCopy{}, err
		}
		if target != nil {
			held.key, held.kind = *target, targetMentions
		} else if sessionID != nil {
			held.key, held.kind = *sessionID, sessionMentions
		}
		return held, nil
	})
}

// loadFallbackEvents reads, by id, the comment.created events copies are the envelopes of, with
// the artifact slug a document's envelope needs.
func loadFallbackEvents(ctx context.Context, deps Deps, copies []fallbackCopy) (map[int64]fallbackEvent, error) {
	ids := make([]int64, 0, len(copies))
	for _, held := range copies {
		ids = append(ids, held.eventID)
	}
	rows, err := deps.Store.Pool.Query(ctx, `
		select e.id, e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''), e.seq,
		       e.type, e.actor, e.notify, e.created_at, e.payload, coalesce(ar.slug, '')
		from events e
		left join issues i on i.key = e.issue_key
		left join artifacts ar on ar.id = e.artifact_id
		where e.id = any($1)
	`, ids)
	if err != nil {
		return nil, err
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (fallbackEvent, error) {
		var item fallbackEvent
		var actor, payload []byte
		if err := row.Scan(
			&item.event.ID, &item.event.IssueKey, &item.event.ArtifactID, &item.event.Project, &item.event.Seq,
			&item.event.Type, &actor, &item.event.Notify, &item.event.CreatedAt, &payload, &item.slug,
		); err != nil {
			return fallbackEvent{}, err
		}
		if err := json.Unmarshal(actor, &item.event.Actor); err != nil {
			return fallbackEvent{}, fmt.Errorf("decode event %d actor: %w", item.event.ID, err)
		}
		if err := json.Unmarshal(payload, &item.event.Payload); err != nil {
			return fallbackEvent{}, fmt.Errorf("decode event %d payload: %w", item.event.ID, err)
		}
		return item, nil
	})
	if err != nil {
		return nil, err
	}
	events := make(map[int64]fallbackEvent, len(read))
	for _, item := range read {
		events[item.event.ID] = item
	}
	return events, nil
}

// recordFallbackCopies writes what a fallback pass came to, in one statement: each row settled is
// marked so, published when it published the copy; each failed row counts one more attempt and
// waits retry.Delay of its new count from now (failedFallbackCopy); and each row left for a later
// pass has its claim replaced by the wait it was released with, which counts no attempt, or
// cleared when that wait is none (fallbackRecord). Every write is guarded on the claim the pass
// took (fallbackCopy's claim): a row whose next_attempt_at is no longer that claim was claimed
// again by a later pass, once the claim lapsed, and is that pass's, so it is not written. A row a
// pass did not reach is not written either and keeps its claim.
func recordFallbackCopies(ctx context.Context, deps Deps, records []fallbackRecord) error {
	if len(records) == 0 {
		return nil
	}
	commentIDs := make([]string, 0, len(records))
	destinations := make([]string, 0, len(records))
	claims := make([]time.Time, 0, len(records))
	settled := make([]bool, 0, len(records))
	published := make([]bool, 0, len(records))
	counted := make([]bool, 0, len(records))
	retryAfter := make([]*time.Duration, 0, len(records))
	for _, record := range records {
		commentIDs = append(commentIDs, record.held.commentID)
		destinations = append(destinations, record.held.destination)
		claims = append(claims, record.held.claim)
		settled = append(settled, record.settled)
		published = append(published, record.published)
		counted = append(counted, record.failed)
		switch {
		case record.failed:
			retryAfter = append(retryAfter, new(retry.Delay(record.held.attempts+1)))
		case record.delay > 0:
			retryAfter = append(retryAfter, new(record.delay))
		default:
			retryAfter = append(retryAfter, nil)
		}
	}
	_, err := deps.Store.Pool.Exec(ctx, `
		update comment_fallback_copies c set
			settled_at = case when o.settled then now() end,
			published = o.published,
			attempt_count = c.attempt_count + case when o.counted then 1 else 0 end,
			next_attempt_at = now() + o.retry_after
		from unnest($1::uuid[], $2::text[], $3::timestamptz[], $4::bool[], $5::bool[], $6::interval[], $7::bool[])
			as o(comment_id, destination, claim, settled, published, retry_after, counted)
		where c.comment_id = o.comment_id and c.destination = o.destination and c.settled_at is null
			and c.next_attempt_at = o.claim
	`, commentIDs, destinations, claims, settled, published, retryAfter, counted)
	return err
}
