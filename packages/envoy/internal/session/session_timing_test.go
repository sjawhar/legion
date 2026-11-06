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

// TestSESSIONTESTTheRegistryResolvesTheDeclaredWindows reads the two windows the listener judges
// a session's registration by, as a registry opened with no window of its own resolves them: the
// live window a refresh renews, and the window after which a reader treats the process that
// wrote a role claim as gone. Both are declared once in packages/contracts and generated here,
// and the pane's refresh interval is set against the live window, so a registry that resolved
// another number would read live panes as unresponsive however the declaration reads.
//
// The live window is read from the registry rather than from the option, because it is what
// every reader of the registry judges a record's age by (Windows, registry.go): a registry given
// no WithSessionLiveWindow resolves the package's own declaration.
func TestSESSIONTESTTheRegistryResolvesTheDeclaredWindows(t *testing.T) {
	registry, err := OpenSessionRegistry(setupNATS(t).Conn)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}

	live, _ := registry.Windows()
	if live != contracts.SessionLiveWindow {
		t.Errorf("the registry resolved a live window of %s, but contracts.SessionLiveWindow is %s",
			millisText(live), millisText(contracts.SessionLiveWindow))
	}
	if ClaimStaleAfter != contracts.SessionClaimStale {
		t.Errorf("ClaimStaleAfter is %s, but contracts.SessionClaimStale is %s",
			millisText(ClaimStaleAfter), millisText(contracts.SessionClaimStale))
	}

	// The windows this run read, logged only where both checks above held, so no line here can
	// read as the declared value under an error saying it is not that value. They appear under
	// `go test -v` alone.
	if t.Failed() {
		return
	}

	t.Logf("the registry resolved a live window of %s (contracts.SessionLiveWindow)", millisText(live))
	t.Logf("ClaimStaleAfter is %s (contracts.SessionClaimStale)", millisText(ClaimStaleAfter))
}
