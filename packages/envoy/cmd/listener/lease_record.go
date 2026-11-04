package main

import (
	"encoding/json"
	"reflect"
	"time"

	"github.com/sjawhar/envoy/internal/store"
)

// A lease record names the boots that held the lease before its holder (former_boots), so the
// delivery ledger takes a former holder's unresolved attempt over without waiting out the
// ledger's takeover age (deliveryHandlingLimit and ledgerTakeoverMargin): at once for a holder
// taken over by force, and once it has had ledgerFormerSettle to settle for one that released
// the record and is still alive (FormerHolder). A relay reads the same record for its own,
// shorter wait (relayFormerSettle, lease.FormerSettleWait).
// An acquisition keeps the ones that stopped less than leaseFormerBootAge, twice that age, before
// it, and the newest leaseFormerBootsMax of those.
const (
	leaseFormerBootAge  = 70 * time.Second
	leaseFormerBootsMax = 16
)

// leaseFields are the lease record's fields this build knows (leaseRecordKeys names their JSON
// keys). holder is the holding pod's instance (ENVOY_INSTANCE_ID or its hostname), so a standby
// names the active pod; boot is random per process start, so two starts of one instance differ;
// deliver_subject is the durable's deliver subject the holder receives on; service_since is when
// this machine's pods started serving without a gap, the server's timestamp of the write that
// began it; acquired_at is the server's timestamp of the holder's acquiring write (informational);
// released marks a record its holder gave up on a graceful stop.
//
// former_boots names the boots that held the lease before holder, each with the server time it
// stopped holding (formerBoots): every acquisition over a record adds that record's boot, its
// holder taken over or released, to that record's own former_boots. A renewal, a release and a
// resume after a lapse leave it as it stands. A former holder delivers nothing more: it was killed,
// lost its node, lost the lease to a take-over, or released the lease after settling or cancelling
// the message it had in hand.
//
// stale_binding and unsettled tell the next holder that a pod of this build may still hold
// messages or a push binding, so that a binding it finds is not a pre-lease build's and its term
// must settle (leaseAcquisition). stale_binding is set by a holder that subscribed while the
// durable was push-bound by a binding it did not wait out (an expired holder's lingering
// connection, or a stopping holder's linger it has no clean-hand-back mark for yet), cleared by
// one that finds the durable not push-bound before it subscribes, and cleared by a term whose
// settle hold read the mark of the release it replaced (HandBackMarked): the binding beside it was
// that release's linger, drained since. Every other write carries it on. unsettled is set
// from the acquiring write of a term that must settle until that term's settle hold ends, and a
// release keeps it as it stands, so a holder stopped mid-hold hands the duty on; a graceful stop
// that hands messages back sets it before its release (Unsettle), so a release after one always
// carries it, and the release of a term whose validity had run out sets it too (Release).
//
// ack_wait_owed tells the next holder that its settle hold must wait as long as an ack wait: the
// term that released the record still owed one, so a message it, a dead holder before it, or a
// lost node's connection had may come back only at its ack wait. The debt is the term's own state
// (lease.ackWaitOwed), so every release writes it as the term stands and every acquiring write
// writes it false: a term that must settle owes an ack wait from its acquisition until its settle
// hold ends (Settled), a term taken from a released record included, whatever its settle hold
// learns of the stop before it. The release of a term whose validity had run out writes it
// whatever the term owed, and so does one that outlasted its bound and left the record to a later
// release (Release): what the lapse dropped without an ack, and what the deliver subject lost
// once that stop let go of it, come back only at their ack wait. A released record carrying
// unsettled with neither it nor stale_binding makes the take a hand-back candidate
// (leaseStatus.HandBack), whose settle hold ends at the hand-back delay only once it reads the
// stopping pod's clean-hand-back mark for that release (lease_handback.go).
//
// receiving says the holder's subscription to the durable's deliver subject is up: every
// acquiring write and every release writes it false, the holder sets it once it has subscribed
// (SetReceiving), and its renewals keep it. A pod that stopped gracefully keeps its own
// subscription until a record names another holder that is receiving (or until no other pod has
// taken the record, when there is none to wait for), so the deliver subject does not lose its last
// subscriber while the server still pushes to it and another pod is about to subscribe.
type leaseFields struct {
	Holder         string       `json:"holder"`
	Boot           string       `json:"boot"`
	DeliverSubject string       `json:"deliver_subject"`
	ServiceSince   time.Time    `json:"service_since,omitzero"`
	AcquiredAt     time.Time    `json:"acquired_at,omitzero"`
	Released       bool         `json:"released"`
	StaleBinding   bool         `json:"stale_binding"`
	Unsettled      bool         `json:"unsettled"`
	AckWaitOwed    bool         `json:"ack_wait_owed"`
	Receiving      bool         `json:"receiving"`
	FormerBoots    []formerBoot `json:"former_boots,omitempty"`
}

// formerBoot is one entry of a lease record's former_boots: a boot that held the lease, the
// server time it stopped, and whether the record it stopped on was released rather than taken
// over expired. A released holder is alive: its role lane is finishing the forward it had in
// flight and recording, giving up or reporting its ledger attempt (roleReceiptTimeout), so the
// ledger defers those attempts for ledgerFormerSettle rather than taking them over at once,
// while a holder that was killed or lost its node writes nothing more and is taken over at once.
type formerBoot struct {
	Boot     string    `json:"boot"`
	Stopped  time.Time `json:"stopped"`
	Released bool      `json:"released,omitempty"`
}

// formerBoots is the former_boots of a record that boot acquires at stopped, a server time, over
// previous: previous's own former_boots and previous's boot, stopped then, less boot itself, those
// stopped leaseFormerBootAge or more before stopped, and all but the newest leaseFormerBootsMax.
// It returns a new slice and leaves previous's as it is, which copies of the record may share.
func formerBoots(previous leaseRecord, boot string, stopped time.Time) []formerBoot {
	boots := make([]formerBoot, 0, len(previous.FormerBoots)+1)
	for _, former := range previous.FormerBoots {
		if former.Boot != boot && former.Boot != previous.Boot && stopped.Sub(former.Stopped) < leaseFormerBootAge {
			boots = append(boots, former)
		}
	}
	if previous.Boot != "" && previous.Boot != boot {
		boots = append(boots, formerBoot{Boot: previous.Boot, Stopped: stopped, Released: previous.Released})
	}
	return boots[max(0, len(boots)-leaseFormerBootsMax):]
}

// leaseRecordKeys are leaseFields' JSON keys; any other key a record carries is a newer build's,
// kept across every rewrite.
var leaseRecordKeys = store.JSONKeys(reflect.TypeFor[leaseFields]())

// leaseRecord is the lease's value: the fields this build knows and the JSON of every key it does
// not, which encode writes back unchanged. Its unknown map is never modified once decoded, so
// copies of a record may share it.
type leaseRecord struct {
	leaseFields
	unknown map[string]json.RawMessage
}

func decodeLeaseRecord(data []byte) (leaseRecord, error) {
	var record leaseRecord
	unknown, err := store.DecodeKeeping(data, &record.leaseFields, leaseRecordKeys)
	if err != nil {
		return leaseRecord{}, err
	}
	record.unknown = unknown
	return record, nil
}

func (r leaseRecord) encode() ([]byte, error) {
	return store.EncodeKeeping(r.leaseFields, r.unknown)
}

// relayFormerSettle is how long a relay waits, from this pod's acquisition, before it restarts a
// session's consumer at its ack floor while a holder that released the lease may still be
// settling the frames it had unanswered (FormerSettleWait, sessionRelays.relay).
//
// A release is a soft hand-over: the pod is alive and its session-consumer connection is up, so
// its stopped relays have relaySettleGrace to hear the session's reply to the frame they had
// unanswered and ack it (sessionRelays.settleStoppedFrames). A successor that restarted the
// consumer before that ended would publish a frame the session may already have taken. This is
// that grace and a margin: the term's end reaches the relays a moment after the release the
// successor takes, so the successor must wait a little longer than they hold.
//
// It is a wait every message of the session pays once per soft take-over, so it is kept as short
// as the grace allows rather than tied to the ledger's own wait below, which bounds something
// else and is longer.
const relayFormerSettle = relaySettleGrace + 250*time.Millisecond

// ledgerFormerSettle is how long the delivery ledger defers the unresolved attempts of a holder
// that released the lease, from this pod's acquisition (FormerHolder,
// dedupe.KVConfig.GoneBoot).
//
// The one attempt a released pod can still be holding is a role-lane forward: it published the
// forward and waits roleReceiptTimeout for the holder's receipt before it records, clears or
// reports it (finishRoleForward). The role lane follows no lease, so that wait runs on past the
// release, and taking the attempt over before its answer would forward a message the holder may
// already have. Every other writer settles inside the call that took the attempt - the fan-out's
// refusal and its record of the copy a self-subscribed session read, the agent path's record and
// its unplaceable give-up, and the publish route's confirmation - and the kept lane's drain
// forwards under the same roleReceiptTimeout, so none of them outlives a release by longer than
// this.
//
// Taking either settle over early is a repeat only a hard take-over may make
// (cmd/listener/README.md). A holder killed or cut off from the bucket releases nothing and is
// taken over at once, so the 15 s hard take-over bound is untouched.
const ledgerFormerSettle = roleReceiptTimeout + 250*time.Millisecond

// FormerHolder reports whether boot is one of the boots that held the lease before the holder of
// the record as this process last wrote or read it (former_boots), and how long that process may
// still be settling a delivery it had in hand. A boot taken over by force - killed, its node
// lost, its record left to expire - writes nothing more, so it is reported with no time at all
// and the ledger takes its unresolved attempt over at once. One that released the record is
// alive and its role lane has roleReceiptTimeout to finish the forward it had in flight and
// record it, give its attempt up, or report it failed, so what is left of ledgerFormerSettle
// since this term's acquisition is reported and the ledger defers that attempt until then. A pod
// holding no term reports no time: it delivers nothing, so nothing of its own waits on the
// answer.
func (l *lease) FormerHolder(boot string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, former := range l.record.FormerBoots {
		if former.Boot != boot {
			continue
		}
		if !former.Released || l.termStarted.IsZero() {
			return true, 0
		}
		return true, max(0, ledgerFormerSettle-l.now().Sub(l.termStarted))
	}
	return false, 0
}

// FormerSettleWait is how long any former holder that released the lease may still be settling
// what it had in hand, counted from this term's acquisition: what is left of relayFormerSettle,
// and zero when every former boot was taken over by force, which settles nothing, and zero for a
// pod holding no term. A relay waits it out before it restarts a session's consumer at its ack
// floor, so the acks the stopping pod is still writing have moved that floor as far as it will
// go before the restart reads it; a restart sends again every frame left above it (the README's
// "When a session can see a message twice", sessionRelays.relay).
func (l *lease) FormerSettleWait() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.termStarted.IsZero() {
		return 0
	}
	for _, former := range l.record.FormerBoots {
		if former.Released {
			return max(0, relayFormerSettle-l.now().Sub(l.termStarted))
		}
	}
	return 0
}
