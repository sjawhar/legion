package approval_test

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// kernelHeading is the heading of the group the page gives a record's peer claim, and
// kernelSentence's first words are the sentence under it that says where those values came from.
const (
	kernelHeading  = "What the kernel says about the asking process"
	kernelSentence = "Read from the kernel through the handle the agent-secrets helper pinned, at "
)

// peerFacts is a reading of a live agent session as the helper signs one, read five seconds after
// the process started. The instants are whole seconds because the claim carries Unix seconds.
func peerFacts(now time.Time) record.PeerFacts {
	return record.PeerFacts{
		PID:        3224579,
		Executable: "/home/person/.local/share/mise/installs/omp/1.2.3/bin/omp",
		WorkingDir: "/home/person/src/oss-verifier",
		Cgroup:     "0::/user.slice/user-1000.slice/session-3.scope",
		StartedAt:  now.Add(-5 * time.Second).Unix(),
		ReadAt:     now.Unix(),
	}
}

// TestTheApprovalPageSaysWhatTheKernelSaysAboutTheAskingProcess holds the page to the whole group:
// its heading, its sentence with the record's own read instant, and each of the five values the
// helper read through the handle it had pinned. The request's own session id stays where it was,
// under the heading that says the process stated it, because the two are worth different amounts
// of trust and a reader who cannot tell them apart has only one of them.
func TestTheApprovalPageSaysWhatTheKernelSaysAboutTheAskingProcess(t *testing.T) {
	r := newRig(t)
	enrollmentID, key := r.enroll(t, "ben-box", "ben-box-runtime", "enrollment-session-1")
	now := time.Now()
	facts := peerFacts(now)
	p := r.requestPeer(t, enrollmentID, key, facts, "TEST_SECRET")

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	res := b.Page(t, p.recordID)
	block := pageText(t, recordBlock(t, res.Body, p.recordID))
	readAt := time.Unix(facts.ReadAt, 0).UTC().Format("2006-01-02 15:04:05 UTC")
	for _, want := range []string{
		kernelHeading,
		kernelSentence + readAt + ", and signed with the session's key. On a machine session the helper runs, the asking process never holds that key.",
		"Process ID 3224579",
		"Executable " + facts.Executable,
		"Working directory " + facts.WorkingDir,
		"Started " + time.Unix(facts.StartedAt, 0).UTC().Format("2006-01-02 15:04:05 UTC"),
		"Control group " + facts.Cgroup,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the record's block does not say %q:\n%s", want, block)
		}
	}

	at := strings.Index(block, kernelHeading)
	if at < 0 {
		t.Fatalf("the record's block has no kernel group:\n%s", block)
	}
	kernel := block[at:]
	if strings.Contains(kernel, "enrollment-session-1") {
		t.Errorf("the kernel group carries the session id the enrollment stated, which the kernel did not answer:\n%s", kernel)
	}
	if !strings.Contains(block, "Session (as the enrollment stated it) enrollment-session-1") {
		t.Errorf("the record's block no longer says the session the enrollment stated:\n%s", block)
	}
}

// TestAnApprovalPageShowsNoKernelGroupWithoutAPeerClaim is the other half: a request no helper
// signed a claim into — every request made before the claim existed, and every one from a session
// whose key signs for no pinned process — renders no kernel group at all, rather than a group of
// empty rows a reader would read as the kernel's answers.
func TestAnApprovalPageShowsNoKernelGroupWithoutAPeerClaim(t *testing.T) {
	r := newRig(t)
	enrollmentID, key := r.enroll(t, "ben-box", "ben-box-runtime", "enrollment-session-1")
	p := r.request(t, enrollmentID, key, "rotate the billing key", nil, "", "TEST_SECRET")

	b := r.browser(t)
	b.SignIn(t, p.recordID, person(ada))
	res := b.Page(t, p.recordID)
	block := recordBlock(t, res.Body, p.recordID)
	for _, unwanted := range []string{kernelHeading, kernelSentence, "Process ID", "Control group"} {
		if strings.Contains(pageText(t, block), unwanted) {
			t.Errorf("a record with no peer claim says %q:\n%s", unwanted, block)
		}
	}
}
