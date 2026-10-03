package classify

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// AdvancePullRequestHead applies the shipped resetPrHead fix-attempt decision to a new observed
// head. A red prior verdict counts once, unless the red was planned (PlannedRed: the review App's
// red tests) or the head's own push was handoff-only or the review App's. A code-changing head
// decides the planned mark (the review App's sets it, anyone else's clears it); a handoff-only
// head, or one whose push has not arrived (ApplyPush settles it), carries it. The recorded CI
// settlement stays, as CheckedHead's: it is the new head's too once every push between them is
// known to change only .legion/ (HeadVerdict), and a settlement for another head replaces it
// (SettlementFor). The pushes it keeps are keptPushes'.
func AdvancePullRequestHead(pr record.PullRequest, headSHA string) record.PullRequest {
	pending := headPush(pr.Pushes, pr.HeadSHA, headSHA)
	pr.Pushes = keptPushes(pr.Pushes, headSHA)
	if HeadVerdict(pr) == "red" && !pr.PlannedRed && (pending == nil || (!pending.HandoffOnly && !pending.ByReviewApp)) {
		pr.FixAttempts++
		pr.HeadCounted = headSHA
	} else {
		pr.HeadCounted = ""
	}
	if pending != nil && !pending.HandoffOnly {
		pr.PlannedRed = pending.ByReviewApp
	}
	pr.HeadSHA = headSHA
	return pr
}

// HeadVerdict is the CI verdict that stands for the pull request's current head: the recorded
// settlement's, when it is the head's own or a head the current one replaced through pushes that
// each changed only .legion/ and said which head they replaced (a handoff push can carry GitHub's
// skip-checks trailer and start no CI of its own); otherwise none.
func HeadVerdict(pr record.PullRequest) string {
	if pr.CheckedHead == "" || !carriedBack(pr.Pushes, pr.HeadSHA, pr.CheckedHead) {
		return ""
	}
	return pr.Verdict
}

// SettlementFor says whether a CI settlement of candidate.Head, recording candidate.Verdict, may
// stand for the pull request's current head, and returns the pull request ready to apply it. It may when head is the
// current head, or a head the current one replaced through pushes that each changed only .legion/,
// unless a recorded verdict already stands for the current head from a head nearer it on that
// path, which outranks it.
// Every other settlement - an earlier code head's, a head a force push left - stands for nothing.
// An absence of information never displaces information: a settlement that records no verdict
// (its checks ended cancelled with none failed) is refused while a verdict stands, whichever head
// it is for, since "some runs were cancelled" is not a result, and letting it overwrite one would
// make the outcome depend on the order the two arrive in.
// A settlement of a head other than the recorded one starts that head's fence afresh, since
// check runs, generations and snapshots are each head's own.
func SettlementFor(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	head := candidate.Head
	if !carriedBack(pr.Pushes, pr.HeadSHA, head) {
		return pr, false
	}
	if candidate.Verdict == "" && HeadVerdict(pr) != "" {
		return pr, false
	}
	if head == pr.CheckedHead {
		return pr, true
	}
	if head != pr.HeadSHA && HeadVerdict(pr) != "" && carriedBack(pr.Pushes, pr.CheckedHead, head) {
		return pr, false
	}
	pr.CheckedHead = head
	pr.Verdict = ""
	pr.Failing = []string{}
	pr.FailingStatuses = []string{}
	pr.CheckRuns = nil
	pr.Generation = 0
	pr.Snapshot = ""
	return pr, true
}

// headPush is the push that left head: the one that replaced previous when the branch has one,
// else any push that left head - a head a force push returned to can have been left before.
func headPush(pushes []record.ClassifiedPush, previous, head string) *record.ClassifiedPush {
	var found *record.ClassifiedPush
	for i := range pushes {
		if pushes[i].SHA != head {
			continue
		}
		if pushes[i].Before == previous {
			return &pushes[i]
		}
		if found == nil {
			found = &pushes[i]
		}
	}
	return found
}

// keptPushes is what a head's arrival keeps: every push that changed only .legion/, did not
// rewrite history and said which head it replaced - a fact about two commits, true whenever it is
// learned, which an approval is carried across - and the pushes still on their way from the new
// head. Any other push is spent once its head arrives, or was late for a head already gone.
func keptPushes(pushes []record.ClassifiedPush, head string) []record.ClassifiedPush {
	onPath := map[string]bool{}
	for _, p := range pathFrom(pushes, head, false) {
		onPath[p.SHA] = true
	}
	var kept []record.ClassifiedPush
	for _, p := range pushes {
		if carriesApproval(p) || onPath[p.SHA] {
			kept = append(kept, p)
		}
	}
	return kept
}

// carriesApproval is whether an approval of the head a push replaced also approves the head it
// left: the push changed only .legion/, did not rewrite history, and said which head it replaced.
func carriesApproval(p record.ClassifiedPush) bool {
	return !p.MayChangeCode() && p.Before != ""
}

// ApplyPush records a push and, for the current head, settles its planned mark and takes back
// exactly its counted fix attempt when the late push is handoff-only or the review App's. A push
// replaces an earlier record of the same two heads, so a redelivered push is recorded once.
func ApplyPush(pr record.PullRequest, push record.ClassifiedPush) record.PullRequest {
	pushes := make([]record.ClassifiedPush, 0, len(pr.Pushes)+1)
	for _, p := range pr.Pushes {
		if p.SHA != push.SHA || p.Before != push.Before {
			pushes = append(pushes, p)
		}
	}
	pr.Pushes = append(pushes, push)
	if pr.HeadSHA != push.SHA {
		return pr
	}
	if !push.HandoffOnly {
		pr.PlannedRed = push.ByReviewApp
	}
	if (push.HandoffOnly || push.ByReviewApp) && pr.HeadCounted == push.SHA {
		if pr.BlockedAttempts == pr.FixAttempts {
			pr.BlockedAttempts = 0
		}
		pr.FixAttempts--
		pr.HeadCounted = ""
	}
	return pr
}

// ApplySettlement applies a listener CI settlement only when the exported fence classifiers say
// it is newer, merging its attempt set into the fence. It returns false for stale, duplicate, and
// conflicting observations.
func ApplySettlement(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	if ClassifySettlement(pr, candidate) != SettlementNewer {
		return pr, false
	}
	outcome := EffectiveOutcome(pr, candidate)
	pr.CheckRuns = mergeAttemptSets(pr.CheckRuns, candidate.CheckRuns)
	pr.Generation = candidate.Generation
	pr.Snapshot = candidate.Snapshot
	pr.Verdict = outcome.Verdict
	pr.Failing = append([]string(nil), outcome.Failing...)
	pr.FailingStatuses = append([]string(nil), outcome.FailingStatuses...)
	return pr, true
}

// LateLifecycle reports whether a pull request lifecycle observation (opened, reopened,
// synchronize or closed) at observed is older than the newest one applied, at applied: a late
// redelivery, which must change nothing. Both are the pull request's updated_at. An equal clock is
// not late, since GitHub's clock is to the second and two real events can share one; a missing
// clock on either side fences nothing.
func LateLifecycle(observed, applied time.Time) bool {
	return !observed.IsZero() && !applied.IsZero() && observed.Before(applied)
}

// RepeatedClose reports whether a close at observed repeats the close already applied to pr: pr is
// recorded closed and the close's clock is not after the recorded one, or the close has none. A
// later close is a second one, whose reopen between the two has not been delivered yet. Unlike
// LateLifecycle, an equal clock is the same event here: a second close in the same second as the
// first goes unreported only when the reopen between them was never observed.
func RepeatedClose(pr record.PullRequest, observed time.Time) bool {
	return pr.State == record.PullRequestClosed && !observed.After(pr.HeadUpdatedAt)
}

// LatestClock is the clock a pull request keeps after applying an observation at observed: the
// later of the two. An observation with no clock is applied but never lowers the stored one, which
// would let an older observation redelivered after it pass LateLifecycle.
func LatestClock(applied, observed time.Time) time.Time {
	if observed.After(applied) {
		return observed
	}
	return applied
}

// pathFrom is the pushes that lie on the path forward from head: each that replaced head or the
// head a push already on the path left. With unplaced, a push that did not say which head it
// replaced is on the path too, the reading that never lets it pass unseen.
func pathFrom(pushes []record.ClassifiedPush, head string, unplaced bool) []record.ClassifiedPush {
	var path []record.ClassifiedPush
	reached := map[string]bool{head: true}
	for grew := true; grew; {
		grew = false
		for _, p := range pushes {
			if !reached[p.SHA] && ((unplaced && p.Before == "") || reached[p.Before]) {
				path = append(path, p)
				reached[p.SHA] = true
				grew = true
			}
		}
	}
	return path
}

// ApprovalStands says whether an approval of reviewed approves the pull request's current head:
// walking back from the current head through pushes that carry an approval across
// (carriesApproval) reaches reviewed, so every push between them changed only .legion/. Nothing
// stands while a push that may change code is on its way (CodeOnItsWay): the push event can come
// first, and the approval is then of a head already on its way out.
func ApprovalStands(pr record.PullRequest, reviewed string) bool {
	if reviewed == "" || CodeOnItsWay(pr) {
		return false
	}
	return carriedBack(pr.Pushes, pr.HeadSHA, reviewed)
}

// CodeOnItsWay says whether a push that may change code lies on the path forward from the pull
// request's current head, its new head not arrived yet. That includes a push delivered late, after
// its new head arrived and the branch returned to this one: nothing tells it from the same two
// heads pushed again, so it counts until the next head arrives, by design. In Legion's flow one
// always follows an approval (the reviewer's handoff push).
func CodeOnItsWay(pr record.PullRequest) bool {
	for _, p := range pathFrom(pr.Pushes, pr.HeadSHA, true) {
		if p.MayChangeCode() {
			return true
		}
	}
	return false
}

// carriedBack is whether earlier is head, or walking back from head through pushes that carry an
// approval across (carriesApproval) reaches it: every push between them changed only .legion/, so
// the two heads' code is the same.
func carriedBack(pushes []record.ClassifiedPush, head, earlier string) bool {
	if head == earlier {
		return true
	}
	reached := map[string]bool{head: true}
	for frontier := []string{head}; len(frontier) > 0; {
		current := frontier[0]
		frontier = frontier[1:]
		for _, p := range pushes {
			if p.SHA != current || !carriesApproval(p) || reached[p.Before] {
				continue
			}
			if p.Before == earlier {
				return true
			}
			reached[p.Before] = true
			frontier = append(frontier, p.Before)
		}
	}
	return false
}

// RedSendsBack says whether the verdict that stands for the pull request's head (HeadVerdict) sends
// an issue in testing or reviewing back to implementing: it is red, the review App did not plan it
// (its failing tests), and the head was not reached by a push that carries an approval across
// (carriesApproval). Such a push changed only .legion/, so the head's code is that of the head it
// replaced, and a red there is either that code's second run or the code head's own, carried to
// the handoff head (SettlementFor). The round in progress decides it, since the reviewer waits for
// the settled verdict at its own handoff head; acting on it would stop a reviewer mid-round and
// spend a fix attempt with no code changed. A head whose push is not recorded yet is read as one
// that may change code.
func RedSendsBack(pr record.PullRequest) bool {
	if HeadVerdict(pr) != "red" || pr.PlannedRed {
		return false
	}
	for _, p := range pr.Pushes {
		if p.SHA == pr.HeadSHA && carriesApproval(p) {
			return false
		}
	}
	return true
}

// BlockFixAttempt marks and reports one exhausted fix-attempt count when the settlement just
// applied is red, as the shipped reducer decides pr-blocked on ci-settled-red alone: a green head
// at an exhausted count is the fix that worked. A zero BlockedAttempts means no count has been
// reported, because fix attempts begin at one before they can exhaust a positive cap.
func BlockFixAttempt(pr record.PullRequest, cap int) (record.PullRequest, bool) {
	if cap <= 0 || HeadVerdict(pr) != "red" || pr.FixAttempts < cap || pr.BlockedAttempts == pr.FixAttempts {
		return pr, false
	}
	pr.BlockedAttempts = pr.FixAttempts
	return pr, true
}

// DesignGateEventKind is the Dispatch artifact observation that changes a registered gate.
type DesignGateEventKind string

const (
	DesignGateApproved         DesignGateEventKind = "approved"
	DesignGateChangesRequested DesignGateEventKind = "changes_requested"
	DesignGateVersion          DesignGateEventKind = "version"
)

// ApplyDesignGateEvent updates a registered gate from one artifact observation. Approval opens
// only when it names the latest known version; a newer document version and changes request close it.
func ApplyDesignGateEvent(gate record.DesignGate, kind DesignGateEventKind, version int) record.DesignGate {
	if version > gate.LatestVersion {
		gate.LatestVersion = version
	}
	switch kind {
	case DesignGateApproved:
		approved := version
		gate.ApprovedVersion = &approved
	case DesignGateChangesRequested:
		gate.ApprovedVersion = nil
	}
	return gate
}

// DesignGateOpen reports whether a human approved the gate's current document version.
func DesignGateOpen(gate record.DesignGate) bool {
	return gate.ApprovedVersion != nil && *gate.ApprovedVersion == gate.LatestVersion
}
