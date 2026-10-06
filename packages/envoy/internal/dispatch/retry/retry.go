// Package retry is the shared backoff schedule for Dispatch's durable retry queues (the events
// outbox and embedqueue): doubling from one second, capped at five minutes, and a shared
// dead-letter threshold after which a queue may stop retrying a row or event automatically.
package retry

import "time"

const (
	// BaseDelay is the first retry's wait.
	BaseDelay = time.Second
	// MaxDelay is Delay's ceiling: no caller waits longer than this between attempts.
	MaxDelay = 5 * time.Minute
	// DeadLetterAttempts is how many consecutive failures a row or event accumulates before a
	// caller that tracks permanence (embedqueue does, for a row's own content; the events outbox
	// does not, since nothing about a publish failure is ever permanent the way a row's content
	// can be) may stop retrying it automatically.
	DeadLetterAttempts = 10
)

// Delay doubles from BaseDelay, capped at MaxDelay: the nth attempt (1-indexed) waits
// min(2^(n-1) * BaseDelay, MaxDelay).
func Delay(attempts int) time.Duration {
	delay := BaseDelay
	for attempt := 1; attempt < attempts && delay < MaxDelay; attempt++ {
		delay *= 2
	}
	if delay > MaxDelay {
		return MaxDelay
	}
	return delay
}
