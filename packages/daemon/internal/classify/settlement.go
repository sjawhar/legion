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

// PassGeneration is the generation of a settlement the daemon's two-minute pass built from its own
// read of a head's check runs, for a head no settlement stood for (the daemon's readSettlement).
// Every settlement Envoy's listener publishes is at generation 0 or above, so this one is told
// apart from them, and it is the weakest (ClassifySettlement): the listener's settlement of the
// same head is merged into it check by check, and never refused as a conflict with it.
const PassGeneration int64 = -1

// SettlementCandidate is a proposed CI outcome for one commit and its ordering identity. Envoy's
// listener publishes one for every settled commit of a pull request, its current head or not, at
// generation 0 or above; the daemon's two-minute pass builds one for a head no settlement stands
// for, from GitHub's check runs on it, as the listener would have built it, at PassGeneration.
// What a settlement comes to is the base branch's required set's to say
// (record.PullRequest.Required, HeadVerdict); the runs of a workflow the base branch requires are
// read apart (record.PullRequest.Workflows), since no settlement carries them by workflow. The
// daemon takes a settlement only for the head itself (SettlementFor); a settlement of another head
// than the recorded one starts the fence afresh. A check_run delivery the listener lost is
// recovered by Dispatch's webhook redelivery sweep (packages/envoy/internal/dispatch/redeliver: it
// runs only where Dispatch has the App key and NATS, every two minutes, and asks GitHub again for
// each delivery GitHub recorded as failed until GitHub records it OK or 72 hours after its first
// failed attempt; its listing starts at the last hour while it runs without a gap, at its stored
// cursor after one, and 72 hours back on a first sweep with no cursor), by the check's next run,
// or, while no settlement stands for the head, by the pass's read, which also sees the runs with
// no pull_requests the listener never records. Nothing corrects the rest: a deleted check's
// failure stands for the life of the head, and a rerun on a head a settlement stands for whose
// completion is never observed holds the last verdict until the check runs again.
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

// ClassifySettlement decides whether a settlement may update the stored CI fence. Between two of
// the listener's settlements, the per-name attempt sets order them (CompareAttemptSets), then the
// generation at an equal set. When either is the pass's (PassGeneration), the check names decide
// alone (classifyWithPass), so the listener's later settlement of the same head never undoes what
// the pass learned, and a newer run on either side is never lost to a conflict.
func ClassifySettlement(pr record.PullRequest, in SettlementCandidate) SettlementClassification {
	if pr.CheckRuns == nil {
		return SettlementNewer
	}
	if pr.Generation == PassGeneration || in.Generation == PassGeneration {
		return classifyWithPass(pr, in)
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

// classifyWithPass classifies in against pr's recorded settlement when either is the pass's: newer
// when in has a higher run of some check, or a check pr has no run of, whatever it says of the
// others (mergeWithPass takes those checks alone); otherwise stale when in has a lower run of some
// check or leaves out a check pr has a run of, since it knows nothing pr does not; and a duplicate
// when it has the same run of every check pr has, and of no other.
func classifyWithPass(pr record.PullRequest, in SettlementCandidate) SettlementClassification {
	switch order := CompareAttemptSets(pr.CheckRuns, in.CheckRuns); {
	case order == AttemptSetNewer || order == AttemptSetMixed:
		return SettlementNewer
	case order == AttemptSetOlder || uncovered(pr.CheckRuns, in.CheckRuns):
		return SettlementStale
	default:
		return SettlementDuplicate
	}
}

// uncovered says whether stored has a run of a check incoming does not report.
func uncovered(stored, incoming []record.AttemptRun) bool {
	reported := make(map[string]bool, len(incoming))
	for _, run := range incoming {
		reported[run.Name] = true
	}
	for _, run := range stored {
		if !reported[run.Name] {
			return true
		}
	}
	return false
}

// adoptsFence says whether in, a duplicate of pr's recorded settlement (ClassifySettlement), is the
// listener's settlement of the same runs the pass's settlement recorded: its generation and
// snapshot then become the fence the listener's later settlements are ordered against, and nothing
// is decided again, since no run and no outcome changed.
func adoptsFence(pr record.PullRequest, in SettlementCandidate) bool {
	return pr.Generation == PassGeneration && in.Generation != PassGeneration
}

// mergeWithPass is pr with in, a newer settlement (classifyWithPass) when either is the pass's,
// merged check by check: a check in has a higher run of than pr, or one pr has no run of, takes in's
// run and in's outcome (failing, cancelled or neither); every other check keeps pr's run and
// outcome. The merged settlement is the listener's own - its generation and snapshot the fence -
// when in is the listener's and has a run, as high as pr's or higher, of every check pr has a run
// of; otherwise it stays the pass's, so the listener's next settlement is merged again rather than
// ordered against a fence holding a run it has not seen.
func mergeWithPass(pr record.PullRequest, in SettlementCandidate) record.PullRequest {
	known := make(map[string]int64, len(pr.CheckRuns))
	for _, run := range pr.CheckRuns {
		known[run.Name] = run.ID
	}
	reported := make(map[string]bool, len(in.CheckRuns))
	taken := make(map[string]bool, len(in.CheckRuns))
	for _, run := range in.CheckRuns {
		reported[run.Name] = true
		if storedID, found := known[run.Name]; !found || run.ID > storedID {
			taken[run.Name] = true
		}
	}
	// A check in names failing or cancelled with no run of its own is taken only when pr knows no
	// run of it either: there is nothing to order the two by.
	for _, names := range [][]string{in.Failing, in.Cancelled} {
		for _, name := range names {
			if _, found := known[name]; !found && !reported[name] {
				taken[name] = true
			}
		}
	}
	outcome := func(incoming, stored []string) []string {
		names := make([]string, 0, len(incoming)+len(stored))
		for _, name := range incoming {
			if taken[name] {
				names = append(names, name)
			}
		}
		for _, name := range stored {
			if !taken[name] {
				names = append(names, name)
			}
		}
		return names
	}
	order := CompareAttemptSets(pr.CheckRuns, in.CheckRuns)
	lower := order == AttemptSetOlder || order == AttemptSetMixed
	covered := !uncovered(pr.CheckRuns, in.CheckRuns)
	pr.Failing, pr.Cancelled = outcome(in.Failing, pr.Failing), outcome(in.Cancelled, pr.Cancelled)
	pr.CheckRuns = mergeAttemptSets(pr.CheckRuns, in.CheckRuns)
	switch {
	case in.Generation != PassGeneration && !lower && covered:
		pr.Generation, pr.Snapshot = in.Generation, in.Snapshot
	case in.Generation == PassGeneration:
		pr.Generation, pr.Snapshot = PassGeneration, in.Snapshot
	}
	return pr
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
