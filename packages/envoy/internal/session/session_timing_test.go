package session

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

// TestSESSIONTESTTheRegistryResolvesTheDeclaredWindows reads the two windows the listener holds a
// session's registration by, as a registry opened with no lifetime of its own resolves them: the
// record lifetime a refresh renews, and the window after which a reader treats the process that
// wrote a role claim as gone. Both are declared once in packages/contracts and generated here, and
// the pane's refresh interval is set against the lifetime, so a registry that resolved another
// number would drop live panes however the declaration reads.
//
// The lifetime is read from the registry rather than from the option, because OpenSessionRegistry
// adopts whatever MaxAge the live bucket reports: against a bucket no earlier test made, which is
// what the package's NATS hands each test, that is the lifetime it created the bucket with.
func TestSESSIONTESTTheRegistryResolvesTheDeclaredWindows(t *testing.T) {
	registry, err := OpenSessionRegistry(setupNATS(t).Conn)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}

	if got := registry.TTL(); got != contracts.SessionRecordLifetime {
		t.Errorf("the registry resolved a record lifetime of %s, but contracts.SessionRecordLifetime is %s",
			millisText(got), millisText(contracts.SessionRecordLifetime))
	}
	if ClaimStaleAfter != contracts.SessionClaimStale {
		t.Errorf("ClaimStaleAfter is %s, but contracts.SessionClaimStale is %s",
			millisText(ClaimStaleAfter), millisText(contracts.SessionClaimStale))
	}

	t.Logf("the registry resolved a record lifetime of %s (contracts.SessionRecordLifetime)", millisText(registry.TTL()))
	t.Logf("ClaimStaleAfter is %s (contracts.SessionClaimStale)", millisText(ClaimStaleAfter))
}
