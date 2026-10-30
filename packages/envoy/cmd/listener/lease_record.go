package main

import (
	"encoding/json"
	"reflect"
	"time"

	"github.com/sjawhar/envoy/internal/store"
)

// A lease record names the boots that held the lease before its holder (former_boots), so the
// delivery ledger takes a former holder's unresolved attempt over at once (FormerHolder) rather
// than waiting out the ledger's takeover age (the 30 s prompt limit and ledgerTakeoverMargin).
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
// connection), but for one whose term still counts as taken from a graceful hand-back
// (lease.HandBackHolds), beside the stopping holder's linger, and cleared by one that finds the
// durable not push-bound before it subscribes; every other write carries it on. unsettled is set
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
// hold ends (Settled), and one taken from a graceful hand-back until the server has its
// subscription, which it stops owing only if the hand-back still holds then (HandBackHolds,
// SetReceiving), the stopping holder's linger having bridged to it. The release of a term whose
// validity had run out writes it whatever the term owed, and so does one that outlasted its bound
// and left the record to a later release (Release): what the lapse dropped without an ack, and
// what the deliver subject lost once that stop let go of it, come back only at their ack wait. A
// released record carrying unsettled without it, taken while that linger could still have bridged
// to the take-over (leaseHandBackBridge), is a graceful hand-back (leaseStatus.HandedBack).
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

// formerBoot is one entry of a lease record's former_boots: a boot that held the lease and the
// server time it stopped, the timestamp of the acquiring write over its record.
type formerBoot struct {
	Boot    string    `json:"boot"`
	Stopped time.Time `json:"stopped"`
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
		boots = append(boots, formerBoot{Boot: previous.Boot, Stopped: stopped})
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
