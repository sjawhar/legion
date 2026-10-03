package classify

import "github.com/sjawhar/legion/daemon/internal/record"

// SettlementClassification states whether a CI settlement can update a head's fence.
type SettlementClassification string

const (
	SettlementStale     SettlementClassification = "stale"
	SettlementDuplicate SettlementClassification = "duplicate"
	SettlementConflict  SettlementClassification = "conflict"
	SettlementNewer     SettlementClassification = "newer"
)

// SettlementCandidate is the listener's proposed CI outcome for one commit and its ordering
// identity. Envoy listener settlements are the Go daemon's only CI input: it never reads GitHub's
// checks, only which of them the base branch requires (record.PullRequest.Required), which decides
// what a settlement comes to (HeadVerdict). The listener publishes a settlement for every settled
// commit of a pull request, its current head or not, and the daemon takes one for the head when it
// is the head's own or a head the current one replaced through pushes that each changed only
// .legion/ (SettlementFor), since a handoff push can carry GitHub's skip-checks trailer and run no
// CI; a settlement of another head than the recorded one starts the fence afresh. A check_run
// delivery the listener lost is recovered only by Dispatch's webhook redelivery sweep
// (packages/envoy/internal/dispatch/redeliver: it runs only where Dispatch has the App key and
// NATS, every two minutes, and resends deliveries GitHub recorded as failed since the sweep's
// window starts: the last hour while it runs without a gap, back to its stored cursor after one,
// never past GitHub's three-day horizon, and the last hour on a first sweep with no cursor) or by
// the check's next run. Nothing on the Go path corrects the rest: a check_run with no
// pull_requests yields no observation, a deleted check's failure stands for the life of the head
// and of the handoff heads after it, and a rerun whose completion is never observed holds the last
// verdict until the check runs again.
type SettlementCandidate struct {
	// Head is the commit the settlement is for, which need not be the pull request's head
	// (SettlementFor).
	Head       string              `json:"head"`
	CheckRuns  []record.AttemptRun `json:"checkRuns"`
	Generation int64               `json:"generation"`
	Snapshot   string              `json:"snapshot"`
	Verdict    string              `json:"verdict"`
	Failing    []string            `json:"failing"`
}

// CiOutcome is the effective result of a partial listener settlement combined with the stored
// failures it omits.
type CiOutcome struct {
	Verdict string   `json:"verdict"`
	Failing []string `json:"failing"`
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

	if in.Generation < pr.Generation {
		return SettlementStale
	}
	if in.Generation == pr.Generation {
		if in.Snapshot == pr.Snapshot {
			return SettlementDuplicate
		}
		return SettlementConflict
	}
	return SettlementNewer
}

// EffectiveOutcome preserves failures omitted by an incomplete listener observation.
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
	if len(failing) != 0 {
		return CiOutcome{Verdict: "red", Failing: failing}
	}
	return CiOutcome{Verdict: in.Verdict, Failing: []string{}}
}
