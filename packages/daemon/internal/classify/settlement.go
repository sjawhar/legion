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
//
// A set that is newer for some check names and older for others is applied rather than thrown
// away. It is the ordinary set across two heads of one tree, not a disagreement: the code head's
// settlement reports the title check at the run it had there while the handoff head above it has
// run that check again, and the handoff head's settlement reports no run at all of the checks
// that ran only on the code head. Each name is then decided on its own by the newest run of it
// (mergeAttemptSets, and the conclusion that run came to), so neither head's facts are lost; the
// rule this replaced called such a set a conflict and dropped the whole settlement, which lost
// the code head's red for as long as the handoff heads stood above it. Every other
// classification is as it was: a set older for every name it reports is stale, and a settlement
// of the same generation and snapshot is the same observation delivered again.
func ClassifySettlement(pr record.PullRequest, in SettlementCandidate) SettlementClassification {
	if pr.CheckRuns == nil {
		return SettlementNewer
	}

	switch CompareAttemptSets(pr.CheckRuns, in.CheckRuns) {
	case AttemptSetNewer, AttemptSetMixed:
		return SettlementNewer
	case AttemptSetOlder:
		return SettlementStale
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
//
// It is kept exported and unchanged now that each name the fence holds a run for is decided by
// that run's own conclusion (concludedBy, which ApplySettlement layers over this). The two
// answer different questions and only one of them could be folded into the other: a settlement
// may name a check failing and report no run of it - an aggregate job whose needs: never ran, so
// GitHub started nothing - and no run means no conclusion to read, so what the lists say is all
// there is for such a name. Keeping it apart also keeps it the one place that rule lives, which
// the recorded contract fixtures replay by name (contracts/fixtures/classification).
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

// stampedRuns is in's check runs with the two facts the listener's report leaves implicit: the
// head GitHub reported each run on, which is the head the settlement is of, and what the run
// concluded, which the settlement's own lists give - a name in Failing failed, one in Cancelled
// was cancelled, and one in neither concluded without either, since the listener settles a
// commit only once every check it has seen there is terminal. They travel with the run into the
// fence, where a name merged over two heads needs them to say which head its newest run was on
// and what that run came to (mergeAttemptSets, concludedBy).
func stampedRuns(in SettlementCandidate) []record.AttemptRun {
	concluded := make(map[string]record.RunConclusion, len(in.Failing)+len(in.Cancelled))
	for _, name := range in.Cancelled {
		concluded[name] = record.RunCancelled
	}
	// The listener keeps the two lists apart (intake's PullRequestChecks), so a name in both is a
	// report no listener writes; it is read as failed, which is what the roles are told for such
	// a name (workflow's settledChecks) and the conclusion a reader acts on. One run has one
	// conclusion, so that name is failing and not cancelled.
	for _, name := range in.Failing {
		concluded[name] = record.RunFailed
	}
	runs := make([]record.AttemptRun, 0, len(in.CheckRuns))
	for _, run := range in.CheckRuns {
		run.Head = in.Head
		if run.Conclusion = concluded[run.Name]; run.Conclusion == "" {
			run.Conclusion = record.RunSucceeded
		}
		runs = append(runs, run)
	}
	return runs
}

// concludedBy is what outcome comes to once the fence has spoken: a name the fence holds a
// concluded run for is failing or cancelled exactly as that run concluded, whatever an older run
// of it concluded and whatever list the settlements put it in, and every other name keeps the
// conclusion outcome gives it. The fence is the newest run of each name over the code head and
// the handoff-only heads above it (mergeAttemptSets), so this is where a check re-run and passed
// on either head stops being red and one that failed there starts being red, whichever head's
// settlement arrived last.
//
// A name with no run in the fence at all keeps outcome's reading: a settlement can name a check
// failing and report no run of it (EffectiveOutcome), and there is no conclusion to read for
// such a name. So does a name whose newest run is a row stored before runs carried a conclusion,
// which says nothing and so decides nothing (record.AttemptRun).
func concludedBy(fence []record.AttemptRun, outcome CiOutcome) CiOutcome {
	decided := make(map[string]record.RunConclusion, len(fence))
	for _, run := range fence {
		if run.Conclusion != "" {
			decided[run.Name] = run.Conclusion
		}
	}
	names := func(listed []string, conclusion record.RunConclusion) []string {
		var kept []string
		seen := make(map[string]bool, len(listed))
		for _, name := range listed {
			if got, found := decided[name]; found && got != conclusion {
				continue
			}
			if !seen[name] {
				seen[name], kept = true, append(kept, name)
			}
		}
		// The fence is sorted by name, so a name the lists never carried joins in name order.
		for _, run := range fence {
			if run.Conclusion == conclusion && !seen[run.Name] {
				seen[run.Name], kept = true, append(kept, run.Name)
			}
		}
		return kept
	}
	return CiOutcome{Failing: names(outcome.Failing, record.RunFailed), Cancelled: names(outcome.Cancelled, record.RunCancelled)}
}
