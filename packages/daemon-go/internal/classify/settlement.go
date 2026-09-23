package classify

import "github.com/sjawhar/legion/daemon/internal/record"

// SettlementClassification states whether a CI settlement can update a head's fence.
type SettlementClassification string

const (
	SettlementStale     SettlementClassification = "stale"
	SettlementDuplicate SettlementClassification = "duplicate"
	SettlementConflict  SettlementClassification = "conflict"
	SettlementNewer     SettlementClassification = "newer"
	SettlementRefresh   SettlementClassification = "refresh"
)

// GitHubFenceEffect states what a complete GitHub rollup can do to a stored CI fence.
type GitHubFenceEffect string

const (
	GitHubFenceAdvance  GitHubFenceEffect = "advance"
	GitHubFenceApply    GitHubFenceEffect = "apply"
	GitHubFenceUnfenced GitHubFenceEffect = "unfenced"
	GitHubFenceStale    GitHubFenceEffect = "stale"
	GitHubFenceConflict GitHubFenceEffect = "conflict"
)

// SettlementCandidate is the listener's proposed CI outcome and its ordering identity.
type SettlementCandidate struct {
	CheckRuns  []record.AttemptRun `json:"checkRuns"`
	Generation int64               `json:"generation"`
	Snapshot   string              `json:"snapshot"`
	Verdict    string              `json:"verdict"`
	Failing    []string            `json:"failing"`
}

// CiOutcome is the effective result after combining a partial listener settlement with stored
// failures and GitHub-only failing statuses.
type CiOutcome struct {
	Verdict         string   `json:"verdict"`
	Failing         []string `json:"failing"`
	FailingStatuses []string `json:"failingStatuses"`
}

// ClassifySettlement decides whether a listener settlement may update the stored CI fence.
func ClassifySettlement(pr record.PullRequest, in SettlementCandidate) SettlementClassification {
	if pr.CheckRuns == nil {
		return SettlementNewer
	}

	switch CompareAttemptSets(pr.CheckRuns, in.CheckRuns) {
	case AttemptSetNewer:
		return SettlementNewer
	case AttemptSetOlder:
		return SettlementStale
	case AttemptSetMixed:
		return SettlementConflict
	}

	// Generation zero is a valid listener generation. The record's empty snapshot is the contract's
	// representation of a GitHub-authored fence, which has no listener generation.
	hasListenerIdentity := pr.Snapshot != ""
	if hasListenerIdentity && in.Generation < pr.Generation {
		return SettlementStale
	}
	if hasListenerIdentity && in.Generation == pr.Generation {
		if in.Snapshot == pr.Snapshot {
			return SettlementDuplicate
		}
		return SettlementConflict
	}
	if !pr.Reconciled {
		return SettlementNewer
	}

	effective := EffectiveOutcome(pr, in)
	if effective.Verdict == pr.Verdict && sameStringMultiset(effective.Failing, pr.Failing) {
		return SettlementRefresh
	}
	return SettlementStale
}

// EffectiveOutcome preserves failures omitted by an incomplete listener observation and every
// GitHub-only failing status.
func EffectiveOutcome(pr record.PullRequest, in SettlementCandidate) CiOutcome {
	reported := make(map[string]struct{}, len(in.CheckRuns)+len(in.Failing))
	for _, run := range in.CheckRuns {
		reported[run.Name] = struct{}{}
	}
	for _, name := range in.Failing {
		reported[name] = struct{}{}
	}

	failing := make([]string, 0, len(in.Failing)+len(pr.Failing))
	failing = append(failing, in.Failing...)
	for _, name := range pr.Failing {
		if _, found := reported[name]; !found {
			failing = append(failing, name)
		}
	}
	failingStatuses := make([]string, len(pr.FailingStatuses))
	copy(failingStatuses, pr.FailingStatuses)
	if len(failing) != 0 || len(failingStatuses) != 0 {
		return CiOutcome{Verdict: "red", Failing: failing, FailingStatuses: failingStatuses}
	}
	return CiOutcome{Verdict: in.Verdict, Failing: []string{}, FailingStatuses: failingStatuses}
}

// AcceptGitHubFence decides whether a complete GitHub rollup can update a stored CI fence.
func AcceptGitHubFence(pr record.PullRequest, checkRuns []record.AttemptRun) GitHubFenceEffect {
	fenced := pr.CheckRuns != nil && len(pr.CheckRuns) > 0
	if len(checkRuns) == 0 {
		if fenced {
			return GitHubFenceStale
		}
		return GitHubFenceUnfenced
	}
	if pr.CheckRuns == nil {
		return GitHubFenceAdvance
	}

	switch CompareAttemptSets(pr.CheckRuns, checkRuns) {
	case AttemptSetNewer:
		return GitHubFenceAdvance
	case AttemptSetEqual:
		return GitHubFenceApply
	case AttemptSetOlder:
		return GitHubFenceStale
	default:
		return GitHubFenceConflict
	}
}

func sameStringMultiset(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		if counts[value] == 0 {
			return false
		}
		counts[value]--
	}
	return true
}
