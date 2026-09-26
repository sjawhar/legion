package classify

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// AdvancePullRequestHead applies the shipped resetPrHead fix-attempt decision to a new observed
// head. A red prior verdict counts once, unless the red was planned (PlannedRed: the review App's
// red tests) or the matching push was handoff-only or the review App's. A code-changing head
// decides the planned mark (the review App's sets it, anyone else's clears it); a handoff-only
// head, or one whose push has not arrived (ApplyPush settles it), carries it. All other
// head-scoped state is reset regardless of its source.
func AdvancePullRequestHead(pr record.PullRequest, headSHA string) record.PullRequest {
	var pending *record.PendingPush
	if pr.PendingPush != nil && pr.PendingPush.SHA == headSHA {
		pending, pr.PendingPush = pr.PendingPush, nil
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
func ApplyPush(pr record.PullRequest, after string, classification PushClassification, byReviewApp bool) record.PullRequest {
	if pr.HeadSHA == after {
		if !classification.HandoffOnly {
			pr.PlannedRed = byReviewApp
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
	pr.PendingPush = &record.PendingPush{SHA: after, HandoffOnly: classification.HandoffOnly, Unknown: classification.Unknown, ByReviewApp: byReviewApp}
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

// LateLifecycle reports whether a pull request lifecycle observation (opened, reopened,
// synchronize or closed) at observed is older than the newest one applied, at applied: a late
// redelivery, which must change nothing. Both are the pull request's updated_at. An equal clock is
// not late, since GitHub's clock is to the second and two real events can share one; a missing
// clock on either side fences nothing.
func LateLifecycle(observed, applied time.Time) bool {
	return !observed.IsZero() && !applied.IsZero() && observed.Before(applied)
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
