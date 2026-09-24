package classify

import "github.com/sjawhar/legion/daemon/internal/record"

// AdvancePullRequestHead applies the shipped resetPrHead decision to a new observed head. A red
// prior verdict counts once unless the matching push was handoff-only; all head-scoped state is
// reset regardless of its source.
func AdvancePullRequestHead(pr record.PullRequest, headSHA string) record.PullRequest {
	handoffOnly := pr.PendingPush != nil && pr.PendingPush.SHA == headSHA && pr.PendingPush.HandoffOnly
	if pr.PendingPush != nil && pr.PendingPush.SHA == headSHA {
		pr.PendingPush = nil
	}
	if pr.Verdict == "red" && !handoffOnly {
		pr.FixAttempts++
		pr.HeadCounted = headSHA
	} else {
		pr.HeadCounted = ""
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

// ApplyPush stores a new-head push classification or takes back exactly the current head's
// counted fix attempt when its late push is handoff-only.
func ApplyPush(pr record.PullRequest, after string, classification PushClassification) record.PullRequest {
	if pr.HeadSHA == after {
		if classification.HandoffOnly && pr.HeadCounted == after {
			if pr.BlockedAttempts == pr.FixAttempts {
				pr.BlockedAttempts = 0
			}
			pr.FixAttempts--
			pr.HeadCounted = ""
		}
		return pr
	}
	pr.PendingPush = &record.PendingPush{SHA: after, HandoffOnly: classification.HandoffOnly, Unknown: classification.Unknown}
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

// ApplyReview stores a changes-requested review from any reviewed head, or an approval only when
// it is pinned to the current head. A new head clears the decision through AdvancePullRequestHead.
func ApplyReview(pr record.PullRequest, state, commitID string) record.PullRequest {
	if state == "changes_requested" || (state == "approved" && commitID != "" && commitID == pr.HeadSHA) {
		pr.ReviewDecision = state
	}
	return pr
}

// BlockFixAttempt marks and reports one exhausted fix-attempt count. A zero BlockedAttempts means
// no count has been reported, because fix attempts begin at one before they can exhaust a positive cap.
func BlockFixAttempt(pr record.PullRequest, cap int) (record.PullRequest, bool) {
	if cap <= 0 || pr.FixAttempts < cap || pr.BlockedAttempts == pr.FixAttempts {
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
