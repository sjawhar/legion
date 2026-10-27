// Package dedupe is the listener's delivery ledger: what it knows of each (dedupe key, session)
// pair it prompts over HTTP or forwards on the role lane, so a message already delivered to a
// session, or being delivered to it, is not delivered again. Every pod of one machine shares one
// ledger (KV, over the listener state bucket), so a pod that takes the delivery lease over does not
// prompt a session again for a message the pod before it delivered. A listener that cannot open
// that bucket keeps its ledger in process (Memory), as one listener process alone always did.
package dedupe

import "time"

// The ages a record counts for. A delivered record stops repeats for DeliveredAge; an attempted
// one, which a prompt whose answer never came leaves (a slow server can accept prompt_async and
// answer 204 after the HTTP client gave up, #389), for AttemptedAge.
const (
	DeliveredAge = 10 * time.Minute
	AttemptedAge = 5 * time.Minute
)

// Key identifies one delivery: an envelope's dedupe key to one session.
type Key struct {
	DedupeKey string
	SessionID string
}

// Verdict is the ledger's answer to an Attempt.
type Verdict uint8

const (
	// Proceed: this process now holds the pair's attempt and prompts or forwards it, then
	// settles the attempt with Record or Clear.
	Proceed Verdict = iota + 1
	// Delivered: the pair was delivered within DeliveredAge; the message is skipped and acked.
	Delivered
	// InFlight: this process attempted the pair within AttemptedAge and has not resolved it (a
	// prompt whose answer never came); the message is skipped and acked.
	InFlight
	// Deferred: another process attempted the pair less than its takeover age ago and may still
	// be prompting. The durable lane retries the message once RetryIn has passed, when the
	// attempt is old enough to take over; a core lane skips it.
	Deferred
)

func (v Verdict) String() string {
	switch v {
	case Proceed:
		return "proceed"
	case Delivered:
		return "delivered"
	case InFlight:
		return "in_flight"
	case Deferred:
		return "deferred"
	default:
		return "none"
	}
}

// Attempt is the ledger's answer for one pair. The zero Attempt holds nothing: Record and Clear
// of it touch nothing, which is how a lane that delivers without the ledger settles its pairs.
type Attempt struct {
	Verdict Verdict
	// RetryIn is, for Deferred, how long until the other process's attempt is old enough to
	// take over.
	RetryIn time.Duration

	pair     Key
	key      string
	revision uint64
}

// Ledger is the delivery ledger every lane of the listener asks before it prompts a session over
// HTTP or forwards a role-lane message to it. KV and Memory are its two implementations.
type Ledger interface {
	// Attempt claims (dedupeKey, sessionID) for a delivery by this process, or says why not.
	// An error means the ledger could not be read or written.
	Attempt(dedupeKey, sessionID string) (Attempt, error)
	// Record marks the pair of a Proceed attempt delivered.
	Record(attempt Attempt) error
	// Clear gives up a Proceed attempt that failed in a way worth retrying, so a retry of the
	// message, by this process or another, prompts the pair again. An attempt another process
	// has since taken over or recorded is left as it is.
	Clear(attempt Attempt) error
}

var (
	_ Ledger = (*KV)(nil)
	_ Ledger = (*Memory)(nil)
)
