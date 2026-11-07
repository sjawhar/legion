package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// `legion claims compact --claim <token>` posts to the claim's compact route with the operator's
// bearer and no body at all, and prints what the daemon answered: the session was compacted, and
// whatever task the claim owed went to it again.
func TestClaimsCompactPostsToTheClaimsCompactRoute(t *testing.T) {
	d := newOperatorDaemon(t)
	if code, _, _ := d.claims(d.spawnArgs()...); code != 0 {
		t.Fatalf("spawn exited %d, want 0", code)
	}
	d.conns.Register(architectClaim, fake.NewConn())
	d.ready(architectClaim)

	out := d.succeed(append(append([]string{"compact"}, d.reach()...), "--claim", string(architectClaim))...)

	seen := d.lastOperatorRequest()
	wantOperatorRequest(t, seen, http.MethodPost, "/legion/v1/operator/claims/"+string(architectClaim)+"/compact")
	if len(seen.body) != 0 {
		t.Fatalf("compact sent the body %q, want none", seen.body)
	}
	if want := "compacted; whatever task the claim owed has gone again"; !strings.Contains(out, want) {
		t.Fatalf("compact stdout = %q, want it to say %q", out, want)
	}
}

// A compaction Oh My Pi refused or skipped is printed with its reason and the command succeeds:
// the daemon did the whole of what the command asks — it tried, and whatever task the claim
// owed went again — so a session too small to compact is news rather than a failure.
func TestClaimsCompactPrintsWhyASessionWasNotCompacted(t *testing.T) {
	d := newOperatorDaemon(t)
	if code, _, _ := d.claims(d.spawnArgs()...); code != 0 {
		t.Fatalf("spawn exited %d, want 0", code)
	}
	conn := fake.NewConn()
	conn.RefuseCompact("Nothing to compact (session too small)")
	d.conns.Register(architectClaim, conn)
	d.ready(architectClaim)

	out := d.succeed(append(append([]string{"compact"}, d.reach()...), "--claim", string(architectClaim))...)

	if want := "not compacted: Nothing to compact (session too small); whatever task the claim owed has gone again"; !strings.Contains(out, want) {
		t.Fatalf("compact stdout = %q, want it to say %q", out, want)
	}
}

// --claim is required, and nothing is reached without it.
func TestClaimsCompactRequiresAClaim(t *testing.T) {
	d := newOperatorDaemon(t)

	code, _, stderr := d.claims(append([]string{"compact"}, d.reach()...)...)

	if code != 2 || !strings.Contains(stderr, "--claim is required") {
		t.Fatalf("compact with no claim exited %d, stderr %q; want 2 naming --claim", code, stderr)
	}
	d.wantNothingReached()
}
