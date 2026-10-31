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
// identity. Envoy listener settlements are the Go daemon's only input about check runs: it reads
// GitHub's checks nowhere else, and judges none of them - what a settlement names is told to the
// roles that work the issue and read by them (workflow's tellChecks,
// dispatch://INFRA-714/spec R1). The listener publishes a settlement for every settled
// commit of a pull request, its current head or not, and the daemon takes one for the head when it
// is the head's own or a head the current one replaced through pushes that each changed only
// .legion/ (SettlementFor), since a handoff push can carry GitHub's skip-checks trailer and run no
// CI; those heads hold one tree, so their settlements merge per check name and the newest run of
// each name is what the head's CI comes to, while a settlement of a head holding other code
// starts the fence afresh. A check still running is in no settlement at all - the listener
// settles a commit only once every check it has seen there is terminal - so it is no verdict
// rather than a failure. A check_run
// delivery the listener lost is recovered only by Dispatch's webhook redelivery sweep
// (packages/envoy/internal/dispatch/redeliver: it runs only where Dispatch has the App key and
// NATS, every two minutes, and asks GitHub again for each delivery GitHub recorded as failed until
// GitHub records it OK or 72 hours after its first failed attempt; its listing starts at the last
// hour while it runs without a gap, at its stored cursor after one, and 72 hours back on a first
// sweep with no cursor) or by the check's next run. Nothing on the Go path corrects the rest: a
// check_run with no pull_requests yields no observation, a deleted check's failure stands for the
// life of the head and of the handoff heads after it, and a rerun whose completion is never
// observed holds the last verdict until the check runs again.
type SettlementCandidate struct {
	// Head is the commit the settlement is for, which need not be the pull request's head
	// (SettlementFor).
	Head       string              `json:"head"`
	CheckRuns  []record.AttemptRun `json:"checkRuns"`
	Generation int64               `json:"generation"`
	Snapshot   string              `json:"snapshot"`
	Failing    []string            `json:"failing"`
	Cancelled  []string            `json:"cancelled"`
}

// CiOutcome is the effective result of a partial listener settlement combined with the stored
// failures and cancellations it omits.
type CiOutcome struct {
	Failing   []string `json:"failing"`
	Cancelled []string `json:"cancelled"`
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

// EffectiveOutcome preserves the failures and cancellations an incomplete listener observation
// omits: a check the stored settlement names failing or cancelled that the new one does not report
// at all stays as it was.
func EffectiveOutcome(pr record.PullRequest, in SettlementCandidate) CiOutcome {
	reported := make(map[string]struct{}, len(in.CheckRuns)+len(in.Failing)+len(in.Cancelled))
	for _, run := range in.CheckRuns {
		reported[run.Name] = struct{}{}
	}
	for _, names := range [][]string{in.Failing, in.Cancelled} {
		for _, name := range names {
			reported[name] = struct{}{}
		}
	}
	kept := func(incoming, stored []string) []string {
		names := append(make([]string, 0, len(incoming)+len(stored)), incoming...)
		for _, name := range stored {
			if _, found := reported[name]; !found {
				names = append(names, name)
			}
		}
		return names
	}
	return CiOutcome{Failing: kept(in.Failing, pr.Failing), Cancelled: kept(in.Cancelled, pr.Cancelled)}
}
