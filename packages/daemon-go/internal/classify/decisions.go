package classify

import "github.com/sjawhar/legion/daemon/internal/record"

// AdvancePullRequestHead applies the shipped resetPrHead fix-attempt decision to a new observed
// head. A red prior verdict counts once, unless the red was planned (PlannedRed: the review App's
// red tests) or the matching push was handoff-only or the review App's. A code-changing head
// decides the planned mark (the review App's sets it, anyone else's clears it); a handoff-only
// head, or one whose push has not arrived (ApplyPush settles it), carries it. All other
// head-scoped state is reset regardless of its source. The code chain (CodeHeads) grows by a
// head whose push changed only .legion/; any other head, or one whose push is still to come,
// leaves it ending at the head replaced, which is then no longer the current one.
func AdvancePullRequestHead(pr record.PullRequest, headSHA string) record.PullRequest {
	var pending *record.PendingPush
	for i, p := range pr.PendingPushes {
		if p.SHA == headSHA {
			pending = &p
			pr.PendingPushes = append(append([]record.PendingPush(nil), pr.PendingPushes[:i]...), pr.PendingPushes[i+1:]...)
			break
		}
	}
	// A push that did not say which head it replaced claimed the one just replaced, unless it is
	// the push this head's arrival consumed: it is dropped with the late ones.
	pr.PendingPushes = pathFrom(pr.PendingPushes, headSHA, false)
	pr.CodeHeads = chainAt(pr.CodeHeads, pr.HeadSHA)
	if pending != nil && pending.HandoffOnly {
		pr.CodeHeads = append(pr.CodeHeads, headSHA)
	}
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
	pr.ReviewDecision = ""
	return pr
}

// ApplyPush stores a new-head push classification or, for the current head, settles its planned
// mark and takes back exactly its counted fix attempt when the late push is handoff-only or the
// review App's (byReviewApp: its pusher is the review App's bot login).
func ApplyPush(pr record.PullRequest, before, after string, classification PushClassification, byReviewApp bool) record.PullRequest {
	if pr.HeadSHA == after {
		if !classification.HandoffOnly {
			pr.PlannedRed = byReviewApp
		} else if !holds(pr.CodeHeads, after) {
			pr.CodeHeads = append(append([]string(nil), pr.CodeHeads...), after)
		}
		if (classification.HandoffOnly || byReviewApp) && pr.HeadCounted == after {
			if pr.BlockedAttempts == pr.FixAttempts {
				pr.BlockedAttempts = 0
			}
			pr.FixAttempts--
			pr.HeadCounted = ""
		}
		return pr
	}
	pushes := make([]record.PendingPush, 0, len(pr.PendingPushes)+1)
	for _, p := range pr.PendingPushes {
		if p.SHA != after {
			pushes = append(pushes, p)
		}
	}
	pr.PendingPushes = append(pushes, record.PendingPush{SHA: after, Before: before, HandoffOnly: classification.HandoffOnly,
		Unknown: classification.Unknown, ByReviewApp: byReviewApp})
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

// chainAt is the code chain ending at head, the head a new one replaces: the recorded chain when
// it already reaches head, else head alone - a head carries its own code, and nothing proves the
// heads before it carried the same.
func chainAt(heads []string, head string) []string {
	if n := len(heads); n > 0 && heads[n-1] == head {
		return append([]string(nil), heads...)
	}
	if head == "" {
		return nil
	}
	return []string{head}
}

// pathFrom is the pending pushes that lie on the path from head: each replaced head or the head a
// push already on the path left. With unplaced, a push that did not say which head it replaced is
// on the path too, the reading that never lets it pass unseen. Any other pending push arrived late
// for a head already gone.
func pathFrom(pushes []record.PendingPush, head string, unplaced bool) []record.PendingPush {
	var path []record.PendingPush
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
// it names that head, or the code chain ends at the current head and holds reviewed, so every
// push since reviewed changed only .legion/. A chain ending anywhere else says nothing: the pushes
// after its end changed code, or have not been classified yet. Nothing stands while a push that
// may change code lies on the path from the current head (pathFrom) and its new head has not
// arrived: the push event can come first, and the approval is then of a head already on its way
// out. A pending push that replaced some other head arrived late for a head already gone, and
// changes nothing.
func ApprovalStands(pr record.PullRequest, reviewed string) bool {
	if reviewed == "" {
		return false
	}
	for _, p := range pathFrom(pr.PendingPushes, pr.HeadSHA, true) {
		if !p.HandoffOnly {
			return false
		}
	}
	if reviewed == pr.HeadSHA {
		return true
	}
	n := len(pr.CodeHeads)
	return n > 0 && pr.CodeHeads[n-1] == pr.HeadSHA && holds(pr.CodeHeads, reviewed)
}

func holds(heads []string, head string) bool {
	for _, h := range heads {
		if h == head {
			return true
		}
	}
	return false
}

// ApplyReview stores a changes-requested review from any reviewed head, or an approval only when
// it is pinned to the current head. A new head clears the decision through AdvancePullRequestHead.
func ApplyReview(pr record.PullRequest, state, commitID string) record.PullRequest {
	if state == "changes_requested" || (state == "approved" && commitID != "" && commitID == pr.HeadSHA) {
		pr.ReviewDecision = state
	}
	return pr
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
