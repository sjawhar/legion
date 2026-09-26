package classify

import "github.com/sjawhar/legion/daemon/internal/record"

// AdvancePullRequestHead applies the shipped resetPrHead fix-attempt decision to a new observed
// head. A red prior verdict counts once, unless the red was planned (PlannedRed: the review App's
// red tests) or the head's own push was handoff-only or the review App's. A code-changing head
// decides the planned mark (the review App's sets it, anyone else's clears it); a handoff-only
// head, or one whose push has not arrived (ApplyPush settles it), carries it. All other
// head-scoped state is reset regardless of its source. The pushes it keeps are keptPushes'.
func AdvancePullRequestHead(pr record.PullRequest, headSHA string) record.PullRequest {
	pending := headPush(pr.Pushes, pr.HeadSHA, headSHA)
	pr.Pushes = keptPushes(pr.Pushes, headSHA)
	if pr.Verdict == "red" && !pr.PlannedRed && (pending == nil || (!pending.HandoffOnly && !pending.ByReviewApp)) {
		pr.FixAttempts++
		pr.HeadCounted = headSHA
	} else {
		pr.HeadCounted = ""
	}
	if pending != nil && !pending.HandoffOnly {
		pr.PlannedRed = pending.ByReviewApp
	}
	pr.HeadSHA = headSHA
	pr.Verdict = ""
	pr.Failing = []string{}
	pr.FailingStatuses = []string{}
	pr.CheckRuns = nil
	pr.Generation = 0
	pr.Snapshot = ""
	pr.Reconciled = false
	return pr
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
// it is newer or a refresh. It returns false for stale, duplicate, and conflicting observations.
func ApplySettlement(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	classification := ClassifySettlement(pr, candidate)
	if classification != SettlementNewer && classification != SettlementRefresh {
		return pr, false
	}
	outcome := EffectiveOutcome(pr, candidate)
	pr.CheckRuns = append([]record.AttemptRun(nil), candidate.CheckRuns...)
	pr.Generation = candidate.Generation
	pr.Snapshot = candidate.Snapshot
	pr.Verdict = outcome.Verdict
	pr.Failing = append([]string(nil), outcome.Failing...)
	pr.FailingStatuses = append([]string(nil), outcome.FailingStatuses...)
	pr.Reconciled = classification == SettlementRefresh
	return pr, true
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
// stands while a push that may change code lies on the path forward from the current head and its
// new head has not arrived: the push event can come first, and the approval is then of a head
// already on its way out. That includes a push delivered late, after its new head arrived and the
// branch returned to this one: nothing tells it from the same two heads pushed again, so it blocks
// until the next head arrives, by design. In Legion's flow one always follows an approval (the
// reviewer's handoff push).
func ApprovalStands(pr record.PullRequest, reviewed string) bool {
	if reviewed == "" {
		return false
	}
	for _, p := range pathFrom(pr.Pushes, pr.HeadSHA, true) {
		if p.MayChangeCode() {
			return false
		}
	}
	reached := map[string]bool{pr.HeadSHA: true}
	for frontier := []string{pr.HeadSHA}; len(frontier) > 0; {
		head := frontier[0]
		frontier = frontier[1:]
		if head == reviewed {
			return true
		}
		for _, p := range pr.Pushes {
			if p.SHA == head && carriesApproval(p) && !reached[p.Before] {
				reached[p.Before] = true
				frontier = append(frontier, p.Before)
			}
		}
	}
	return false
}

// BlockFixAttempt marks and reports one exhausted fix-attempt count when the settlement just
// applied is red, as the shipped reducer decides pr-blocked on ci-settled-red alone: a green head
// at an exhausted count is the fix that worked. A zero BlockedAttempts means no count has been
// reported, because fix attempts begin at one before they can exhaust a positive cap.
func BlockFixAttempt(pr record.PullRequest, cap int) (record.PullRequest, bool) {
	if cap <= 0 || pr.Verdict != "red" || pr.FixAttempts < cap || pr.BlockedAttempts == pr.FixAttempts {
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
