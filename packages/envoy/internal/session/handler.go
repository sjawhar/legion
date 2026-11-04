package session

import (
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

<<<<<<< conflict 1 of 1
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
 // HandleAgentReason names why a delivery attempt did not land, for the one question the caller
 // cannot answer from Delivered alone: whether waiting could change the answer. The
 // three causes reach the caller as one undelivered result otherwise, and it would have to
 // re-derive them from the error text to tell them apart.
 type HandleAgentReason uint8
 
 const (
 	// ReasonNone is every outcome that is none of the three causes below: a push that landed, a
-	// self-subscribed skip (the session reads the message off its own subscription), and a
-	// session another listener owns (ErrWrongMachine), which both paths answer as OwnedElsewhere:
+	// skip for a message the session's own consumer holds, which the relay of that consumer
+	// delivers (DeliveryResult.Relayed), and a session another listener owns (ErrWrongMachine),
+	// which both paths answer as OwnedElsewhere:
 	// that listener delivers the message, so this one has no cause to report.
 	ReasonNone HandleAgentReason = iota
 	// ReasonNoSessionRecord is a session the resolution found no record of. A caller holding a
 	// record source that trails the store - or one whose read did not answer - may ask again.
 	ReasonNoSessionRecord
 	// ReasonNoPushTarget is a session whose record exists and names nothing to push to: it
 	// registered before its server bound a serve port. That record is rewritten when the port
 	// is bound, so asking again is not asking the same question - a caller may wait the port
 	// out, and the listener's delivery does, deferring such a message for its whole retry
 	// window before it gives up (cmd/listener/unplaceable_delivery.go).
 	ReasonNoPushTarget
 	// ReasonPushFailed is a record that named a serve port the push did not land on: an
 	// unreachable peer, a session process restarting, a connection refused, or a server that
 	// answered with anything but 204. The attempt failed and cannot tell whether the session
 	// took the prompt, so a caller may ask again only at the cost of repeating it. The listener's
 	// delivery never reaches its unplaceable policy with it: on the durable path a serving-port
 	// session's push is its worker's, which pushes until the port accepts it
 	// (cmd/listener/serve_port_workers.go), and on the live and core lanes a failed push comes
 	// back through the interest path, which reports it delivery_failed and NAKs.
 	ReasonPushFailed
 )
 
 // reasonFor maps the delivery sentinels onto the reason the caller acts on. Every other error -
 // a refused push, an unreachable machine - is ReasonPushFailed: it is a failure of this attempt,
 // not a statement about the session's record, and a later attempt may well land. ErrWrongMachine
 // never reaches here: both call sites answer it as OwnedElsewhere before they map.
 func reasonFor(err error) HandleAgentReason {
 	switch {
 	case errors.Is(err, ErrNoSessionRecord):
 		return ReasonNoSessionRecord
 	case errors.Is(err, ErrNoPushTarget):
 		return ReasonNoPushTarget
 	default:
 		return ReasonPushFailed
 	}
 }
 
+++++++ lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
>>>>>>> conflict 1 of 1 ends
// HandleAgentResult captures the outcome of agent message handling.
type HandleAgentResult struct {
	Delivered bool
	Skipped   bool
	// Relayed is set with Delivered and Skipped for a message the session's own consumer holds,
	// which the relay of that consumer delivers (DeliveryResult.Relayed): the caller records
	// nothing about it, neither an attempt nor a dedupe key.
	Relayed bool
	// OwnedElsewhere reports a session whose stored subscription names another listener. That
	// listener reads the same inbox message from its own durable and delivers it, so this one
	// acknowledges it with nothing to report: no delivery, no exception, no NAK.
	OwnedElsewhere bool
	ShouldNAK      bool
	Err            error
}

// HandleAgentMessage handles delivery of an agent-to-agent message.
// It tries the interest path first (KV subscription match), then falls back
// to the session entry its caller resolved. Returns the result so the caller
// can decide ACK vs NAK.
//
// entry is that resolved session record, nil when none was resolved; both paths deliver through
// it (Resolve) rather than resolving it a second time, so the record the caller read - the
// store's own word for it, not just this listener's cache - is the one delivery acts on.
//
// storedSeq is the stream sequence the delivery resolves under: the durable path's copy of a
// stored message carries its own, and a core path's message, which is not the durable's copy of
// anything, resolves under 0, which leaves a session on consumer delivery its consumer's copy
// whatever that copy's sequence is. An agent message is stored on the subject it names as its
// topic, the session's own agent subject, so Resolve reads that topic for both paths below.
func HandleAgentMessage(
	item contracts.Envelope,
	sessionID string,
	machineID string,
	interest *store.Interest,
	entry *SessionEntry,
	storedSeq uint64,
) HandleAgentResult {
	// Interest path: the stored subscription names this listener, which owns the session.
	if interest != nil && interest.MachineID == machineID {
		delivery, err := Resolve(item, sessionID, entry, storedSeq)
		if err != nil {
			return HandleAgentResult{ShouldNAK: true, Err: err}
		}
		return delivered(delivery)
	}
	// A stored subscription naming another listener makes the session that listener's: the
	// stored MachineID is the one its topics are fanned out on (store.Registry.Match), and that
	// listener reads this message from its own durable too. The row names the listener the
	// session registered through most recently (store's mergeForUpsert), so that is also the
	// listener it is registered through, give or take a registration still in flight.
	if interest != nil && interest.MachineID != "" {
		return HandleAgentResult{OwnedElsewhere: true}
	}

	// No stored subscription names an owner, so this listener answers for the session: there is
	// nobody else the message can be left to. The entry is the one the caller resolved, delivered
	// through directly rather than read a second time.
	delivery, err := Resolve(item, sessionID, entry, storedSeq)
	if err != nil {
		// Resolve fails only with ErrNoSessionRecord. The result carries no error, so the
		// listener defers the message for its retry window (cmd/listener/unplaceable_delivery.go)
		// rather than NAK it.
		return HandleAgentResult{Delivered: false}
	}
	return delivered(delivery)
}

// delivered is the result of a delivery that succeeded.
func delivered(delivery DeliveryResult) HandleAgentResult {
	return HandleAgentResult{Delivered: true, Skipped: delivery.Skipped, Relayed: delivery.Relayed}
}
