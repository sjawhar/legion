package bus

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
)

// publishRefusal reads the subject of the server's refusal of a publish: `Permissions Violation
// for Publish to "<subject>"` (nats-server v2.15.2 client.go:5917), which it reports on the
// connection and never in the publish's own answer. The server writes the subject with %q, as it
// writes the names of a refused subscription (subscriptionRefusal), so it is matched as a whole
// Go-quoted string, escapes included, and unquoted before it is recorded: a subject holding a
// double quote or a backslash reaches the text escaped, and is recorded under the name the
// publisher sent.
var publishRefusal = regexp.MustCompile(`Publish to ("(?:[^"\\]|\\.)*")`)

// publishDenialRetention is how long a recorded violation can still be read (deniedPublish). It
// outlasts, with room to spare, every wait a publish of this package makes for the answer a denied
// publish never gets - publishAcknowledgementTimeout for a window's send, publishBound for
// publishJetStream - so a violation recorded the moment the server refused the message is still
// there when the publish that earned it gives up. It is short enough that a record no publish ever
// claims, because its caller stopped waiting, is dropped long before the same subject is published
// again, so a stale violation never answers a later publish on its own: the publish's own start
// time (deniedPublish) is what rules that out within the retention.
const publishDenialRetention = time.Minute

// publishDenialDepth bounds how many violations of one subject the record keeps, the oldest
// dropped when one past it is recorded. It is measured against how many messages of one subject
// one publisher can have in flight at once: the outbox sends at most one batch per publish window
// and its batch is batchSize (100) events, so a bound comfortably above that cannot drop a
// violation a live publish is still owed, while a server refusing a subject nobody publishes
// again holds no more than this many records of it.
const publishDenialDepth = 256

// publishViolation is the server's refusal of a publish to one subject: the violation it reported
// and the time the client recorded it (recordPublishDenial).
type publishViolation struct {
	violation error
	at        time.Time
}

// recordPublishDenial records violation, a permissions violation nc reported, under the subject it
// names a refused publish to, and reports whether it was a refusal of a publish at all - a refusal
// of a subscription is not, and is the caller's to read (onAsyncError). It goes to the end of that
// subject's queue of violations, which holds them in arrival order: nats-server reports one
// violation per refused message, so one window's several messages to one subject earn a violation
// each, and a record of one violation per subject would answer one of those sends and leave every
// other to its acknowledgement timeout. Each send of the subject takes the violation that answers
// it (deniedPublish).
//
// Nothing is recorded for a connection the client has since replaced, as nothing is marked for one
// (onAsyncError): its publishes are answered already, and the record belongs to the connection in
// Conn. c.mu is held across the check and the record, so a replacement cannot be installed between
// them. Each record carries the time it was made, which bounds how long it can be read, and
// recording one drops every record that retention has spent and, past publishDenialDepth, the
// oldest of the subject's own, so a connection refused publishes to ever new subjects, or ever
// more of one subject, cannot grow the record without bound.
func (c *Client) recordPublishDenial(nc *nats.Conn, violation error) bool {
	match := publishRefusal.FindStringSubmatch(violation.Error())
	if match == nil {
		return false
	}
	subject, err := strconv.Unquote(match[1])
	if err != nil {
		return false
	}
	at := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if nc != c.Conn {
		return true
	}
	if c.deniedPublishes == nil {
		c.deniedPublishes = make(map[string][]publishViolation)
	}
	for recorded, queue := range c.deniedPublishes {
		spent := 0
		for spent < len(queue) && at.Sub(queue[spent].at) > publishDenialRetention {
			spent++
		}
		switch {
		case spent == len(queue):
			delete(c.deniedPublishes, recorded)
		case spent > 0:
			c.deniedPublishes[recorded] = slices.Delete(queue, 0, spent)
		}
	}
	queue := append(c.deniedPublishes[subject], publishViolation{violation: violation, at: at})
	if len(queue) > publishDenialDepth {
		queue = slices.Delete(queue, 0, len(queue)-publishDenialDepth)
	}
	c.deniedPublishes[subject] = queue
	return true
}

// deniedPublish is the server's refusal of the caller's own publish to subject: the oldest
// violation of that subject recorded at or after sentAt, named as ErrPublishDenied
// (deniedPublishError) and taken out of the subject's queue, and nil when the connection reported
// no such refusal. It is what a JetStream publish reads to learn that a denial, and not the
// connection, is what it came to: the stream acknowledges a denied publish with nothing
// (window.acknowledged, publishJetStream).
//
// sentAt is the time the caller put its own message on the wire, which is what keeps a record from
// naming another publish: a violation recorded before this publish was sent belongs to an earlier
// publish of the subject, whose caller may have stopped waiting, so it is skipped and left where
// it is, for retention to drop, rather than answering this publish, whose own failure then has
// another reason. Taking the oldest violation recorded since, and only that one entry, is what
// answers each refused message with a violation of its own: the server reports one per message, so
// every send of the subject still waiting is owed one of those the queue holds. The subject leaves
// the record once its last violation is read.
func (c *Client) deniedPublish(subject string, sentAt time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	queue := c.deniedPublishes[subject]
	for index, denial := range queue {
		if denial.at.Before(sentAt) {
			continue
		}
		if len(queue) == 1 {
			delete(c.deniedPublishes, subject)
		} else {
			c.deniedPublishes[subject] = slices.Delete(queue, index, index+1)
		}
		return deniedPublishError(subject, denial.violation)
	}
	return nil
}

// clearPublishDenials drops every recorded violation, because they were the previous session's:
// the grant the server applies to the session a reconnect opened is its own, and a publish on it
// is answered by it. Its caller holds c.mu, where it learns of the reconnect (onReconnect) or
// installs the constructor's connection (install), where the record is still empty.
func (c *Client) clearPublishDenials() {
	clear(c.deniedPublishes)
}

// deniedPublishError names violation, the server's refusal of a publish to subject under the
// connection's own publish permissions, as ErrPublishDenied. It is the one spelling of that error:
// the core path's read of the connection's last error (publishDenial) and the JetStream paths'
// read of the per-subject record (deniedPublish) both return it, so the text an operator greps and
// the errors a caller matches are the same whichever path named the denial.
func deniedPublishError(subject string, violation error) error {
	return fmt.Errorf("bus: %w to %q: %w", ErrPublishDenied, subject, violation)
}
