package webhook

import (
	"log"
	"net/http"
	"sync"
	"time"
)

// githubFailuresKept and githubFailureAge bound the GitHub deliveries a handler remembers having
// logged as failed: the newest githubFailuresKept, for githubFailureAge after their first logged
// failure, which is as long as Dispatch's redelivery sweep asks GitHub for a failed delivery again.
const (
	githubFailuresKept = 4096
	githubFailureAge   = 72 * time.Hour
)

// githubDeliveryKeyMax caps the X-GitHub-Delivery a failure is remembered under: GitHub's delivery
// GUID is 36 characters, so a header longer than this is none GitHub sent, and one that is empty
// names no delivery at all. Such a header is answered and logged exactly as any other failed
// delivery, and remembered as nothing (first), rather than cut down to a key: two headers sharing
// their first githubDeliveryKeyMax bytes would share one key, and the second delivery's first
// failure - the line the listener's `webhook-refused` and `publish-failure` alarms count - would be
// logged as the first delivery's redelivery, which no alarm counts.
const githubDeliveryKeyMax = 64

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
// counted line (failureLine) only if this is the first time it has logged this failure of this
// delivery; every later one logs that same line behind a `redelivery <delivery>: ` prefix, which
// no alarm counts.
func (f *failedDeliveries) deliveryFailed(w http.ResponseWriter, delivery, action string, err error) {
	result, status, answer := failureOutcome(err)
	line := failureLine(action, result, err)
	if !f.first(failedDelivery{delivery, action + " " + result}) {
		line = "redelivery " + delivery + ": " + line
	}
	log.Print(line)
	http.Error(w, answer, status)
}

// first records failure and reports whether it was not already recorded. It first forgets the
// failures older than f.age, and the oldest one when f.kept are held. A delivery this memory holds
// no key for (githubDeliveryKeyMax) records nothing and is always the first failure of its
// delivery, so its counted line is logged every time.
func (f *failedDeliveries) first(failure failedDelivery) bool {
	if failure.delivery == "" || len(failure.delivery) > githubDeliveryKeyMax {
		return true
	}
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
