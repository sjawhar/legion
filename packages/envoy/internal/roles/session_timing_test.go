package roles

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// millisText renders d as whole milliseconds, grouped in threes: 300000 reads as "300,000 ms".
func millisText(d time.Duration) string {
	digits := strconv.FormatInt(int64(d/time.Millisecond), 10)
	var grouped strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	return grouped.String() + " ms"
}

// TestROLESTESTTheFallbackWindowIsTheDeclaredRecordLifetime reads the window Buckets expires a
// session record against before it has opened the session bucket, or when the bucket's status
// reports no TTL: the field itself, which is what every read falls back to, not the constant it is
// set from. It must be the declared record lifetime, the window the listener creates the bucket
// with, or Dispatch's outbox would read a live holder as expired, or a gone one as live.
//
// This file is in package roles, unlike the package's other tests, because that field is
// unexported: a test outside the package could only read the constant beside it.
func TestROLESTESTTheFallbackWindowIsTheDeclaredRecordLifetime(t *testing.T) {
	fallback := NewBuckets(nil).ttl
	if fallback != contracts.SessionRecordLifetime {
		t.Errorf("NewBuckets falls back to a window of %s, but contracts.SessionRecordLifetime is %s",
			millisText(fallback), millisText(contracts.SessionRecordLifetime))
	}
	t.Logf("NewBuckets falls back to a window of %s (contracts.SessionRecordLifetime)", millisText(fallback))
}
