package webhook

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Publisher publishes a webhook's envelopes, each within the context its handler gives it, which
// carries the request's own end and the route's budget for its caller (*bus.Client.PublishWithin,
// which also keeps the bus's own five-second bound).
type Publisher interface {
	PublishWithin(context.Context, contracts.Envelope) error
}

// deliveryFailed answers a delivery whose envelope the listener did not publish, and logs
// `<action> refused` or `<action> failed` (failureOutcome). Slack and Ghost Wispr deliveries log
// it on every attempt; a GitHub delivery logs it once (failedDeliveries).
func deliveryFailed(w http.ResponseWriter, action string, err error) {
	result, status, answer := failureOutcome(err)
	log.Printf("%s %s: %v", action, result, err)
	http.Error(w, answer, status)
}

// failureOutcome is how a delivery whose envelope, or CI observation, the listener did not publish
// is logged and answered. A refusal (bus.ErrRefused: too large to publish whole, or a subject NATS
// does not accept) is refused the same way on every redelivery while the server's limits stand,
// so it is a 422 `refused`, and its answer is the error, which says why; Dispatch's redelivery
// sweep asks for it again like any failed delivery. Any other failure may pass on a redelivery,
// so it is a 503 `failed`. The listener's `webhook-refused` and `publish-failure` alarms count
// these lines.
func failureOutcome(err error) (result string, status int, answer string) {
	if errors.Is(err, bus.ErrRefused) {
		return "refused", http.StatusUnprocessableEntity, err.Error()
	}
	return "failed", http.StatusServiceUnavailable, "service unavailable"
}

// githubFailuresKept and githubFailureAge bound the GitHub deliveries a handler remembers having
// logged as failed: the newest githubFailuresKept, for githubFailureAge after their first logged
// failure, which is as long as Dispatch's redelivery sweep asks GitHub for a failed delivery again.
const (
	githubFailuresKept = 4096
	githubFailureAge   = 72 * time.Hour
)

// failedDeliveries remembers which GitHub deliveries' failures this process has logged, so a
// delivery that fails on every redelivery pages once. Dispatch's redelivery sweep asks GitHub for
// a failed delivery again for 72 hours, about 76 times, and a redelivery carries the same
// X-GitHub-Delivery: the first failure of a delivery logs `<action> refused` or `<action> failed`,
// the line the alarms count, and every later one of the same action and result logs
// `redelivery <delivery>: <action> <result>: <error>`, which no alarm counts. The memory is the
// process's own, so each listener task, and each start of one, logs a delivery's first failure
// once.
type failedDeliveries struct {
	kept int
	age  time.Duration
	now  func() time.Time

	mu     sync.Mutex
	logged map[failedDelivery]struct{}
	order  []loggedFailure // oldest first: an entry is added once and never refreshed
}

// failedDelivery is a delivery's failure as its counted line names it: the delivery and its
// `<action> <result>`.
type failedDelivery struct{ delivery, line string }

type loggedFailure struct {
	failedDelivery
	at time.Time
}

func newFailedDeliveries(kept int, age time.Duration) *failedDeliveries {
	return &failedDeliveries{kept: kept, age: age, now: time.Now, logged: make(map[failedDelivery]struct{})}
}

// deliveryFailed answers delivery's failure as the package's deliveryFailed does, and logs the
// counted line only if this is the first time it has logged this failure of this delivery.
func (f *failedDeliveries) deliveryFailed(w http.ResponseWriter, delivery, action string, err error) {
	result, status, answer := failureOutcome(err)
	if f.first(failedDelivery{delivery, action + " " + result}) {
		log.Printf("%s %s: %v", action, result, err)
	} else {
		log.Printf("redelivery %s: %s %s: %v", delivery, action, result, err)
	}
	http.Error(w, answer, status)
}

// first records failure and reports whether it was not already recorded. It first forgets the
// failures older than f.age, and the oldest one when f.kept are held.
func (f *failedDeliveries) first(failure failedDelivery) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	for len(f.order) > 0 && now.Sub(f.order[0].at) >= f.age {
		f.forgetOldest()
	}
	if _, ok := f.logged[failure]; ok {
		return false
	}
	if len(f.order) >= f.kept {
		f.forgetOldest()
	}
	f.logged[failure] = struct{}{}
	f.order = append(f.order, loggedFailure{failure, now})
	return true
}

func (f *failedDeliveries) forgetOldest() {
	delete(f.logged, f.order[0].failedDelivery)
	f.order[0] = loggedFailure{}
	f.order = f.order[1:]
}
