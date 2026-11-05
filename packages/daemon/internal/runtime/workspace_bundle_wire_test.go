package runtime_test

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The release's own maximum has to fit the sequence that carries it, header and separator
// included: a release stops above runtime.MaxWorkspaceBundleBytes (outbox's release), and a body
// over shimwire.MaxWorkspaceBundleBodyBytes is refused by the wire, so a policy raised past the
// wire's bound would save work the daemon could then never send. The two live in different
// packages because the wire imports nothing of the runtime's, which is what lets this drift.
func TestTheReleasesBundleFitsTheSequenceThatCarriesIt(t *testing.T) {
	// The header is one JSON line, far under a kibibyte for the commits a release names, plus the
	// newline that separates it from the bundle's own bytes.
	const headerAndSeparator = 1 << 10
	if runtime.MaxWorkspaceBundleBytes+headerAndSeparator > shimwire.MaxWorkspaceBundleBodyBytes {
		t.Fatalf("a release carries %d bytes and the launcher_bundle sequence %d: a saved bundle could not be sent",
			runtime.MaxWorkspaceBundleBytes, shimwire.MaxWorkspaceBundleBodyBytes)
	}
}
