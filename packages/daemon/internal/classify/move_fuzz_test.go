package classify

import (
	"fmt"
	"math/rand/v2"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// moveStep is one push of a random history with moves made again, and whether its head arrives:
// a head the daemon never sees synchronize is skipped, and the next arrival's synchronize names
// the move from it. These histories name no sender; they are the ones the release before's digests
// were made from (TestAnArrivalThatNamesNoMoveIsJudgedAsTheReleaseBefore), so they stay as they are.
type moveStep struct {
	push   record.ClassifiedPush
	arrive bool
}

// movesMadeAgain is one random history: fresh commits, forced returns that undo the move before
// them, and moves made again, each push a random pusher's, each pair of heads changing the same
// paths whenever it is pushed.
func movesMadeAgain(rng *rand.Rand) []moveStep {
	var steps []moveStep
	cur, visited := "h0", []string{"h0"}
	flags := map[[2]string][2]bool{}
	n := 3 + rng.IntN(6)
	for k := 1; k <= n; k++ {
		var to string
		forced := false
		var again []string
		for _, s := range steps {
			if s.push.Before == cur {
				again = append(again, s.push.SHA)
			}
		}
		switch r := rng.IntN(3); {
		case r == 0 && len(again) > 0:
			to = again[rng.IntN(len(again))]
		case r == 1 && len(steps) > 0:
			to, forced = steps[len(steps)-1].push.Before, true
		default:
			to, forced = fmt.Sprintf("c%d", k), rng.IntN(4) == 0
		}
		if to == cur {
			to, forced = fmt.Sprintf("c%d", k), false
		}
		key := [2]string{cur, to}
		f, ok := flags[key]
		if !ok {
			f = [2]bool{forced, rng.IntN(3) == 0}
			flags[key] = f
		}
		steps = append(steps, moveStep{push: record.ClassifiedPush{Before: cur, SHA: to, Forced: f[0], HandoffOnly: f[1], ByReviewApp: rng.IntN(2) == 0},
			arrive: k == n || rng.IntN(4) != 0})
		cur = to
		if !containsHead(visited, to) {
			visited = append(visited, to)
		}
	}
	return steps
}

func containsHead(heads []string, head string) bool {
	for _, h := range heads {
		if h == head {
			return true
		}
	}
	return false
}

// moveDeliveries is a history's facts: the reference (each push just before its head), one order
// of delivery, and the oracle's (the reference with each head a move made again reaches renamed
// as a fresh commit, named its head back by named). r1 is whether two pushes of the same two heads
// differ; noop whether a head arrives at the head before it.
type moveDeliveries struct {
	reference, order, oracle []runFact
	named                    map[string]string
	r1, noop                 bool
}

func deliveries(rng *rand.Rand, steps []moveStep, bounded bool) moveDeliveries {
	var d moveDeliveries
	for i := range steps {
		for j := i + 1; j < len(steps); j++ {
			if pushKey(steps[i].push) == pushKey(steps[j].push) && steps[i].push != steps[j].push {
				d.r1 = true
			}
		}
	}
	labels := make([]string, len(steps))
	latest, used := map[string]string{"h0": "h0"}, map[[2]string]bool{}
	label := func(i int) string {
		if i < 0 {
			return "h0"
		}
		return labels[i]
	}
	for i, s := range steps {
		name, ok := latest[s.push.SHA]
		if !ok {
			name = s.push.SHA
		}
		if used[[2]string{label(i - 1), name}] {
			name = fmt.Sprintf("%s'%d", s.push.SHA, i)
		}
		labels[i], latest[s.push.SHA] = name, name
		used[[2]string{label(i - 1), name}] = true
	}
	d.named = map[string]string{"h0": "h0", "": ""}
	var facts []runFact
	var pushes []record.ClassifiedPush
	var after, earlier []int // per push: the facts before it it may follow, and the earlier push of its two heads
	var pending, oraclePending []runFact
	id, lastArrival, lastHead := int64(2), 0, "h0"
	for i, s := range steps {
		d.named[label(i)] = s.push.SHA
		renamed := s.push
		renamed.Before, renamed.SHA = label(i-1), label(i)
		same := -1
		for k := len(pushes) - 1; k >= 0 && same < 0; k-- {
			if pushKey(pushes[k]) == pushKey(s.push) {
				same = k
			}
		}
		pushes, after, earlier = append(pushes, s.push), append(after, lastArrival), append(earlier, same)
		pending, oraclePending = append(pending, pushed(s.push)), append(oraclePending, pushed(renamed))
		if !s.arrive {
			continue
		}
		d.noop = d.noop || s.push.SHA == lastHead
		lastHead = s.push.SHA
		failing := []string{}
		if rng.IntN(2) == 0 {
			failing = []string{"ci"}
		}
		head, settle := arrivesFrom(s.push.Before, s.push.SHA), settles(s.push.SHA, id, failing...)
		d.reference = append(append(d.reference, pending...), head, settle)
		d.oracle = append(append(d.oracle, oraclePending...), arrivesFrom(renamed.Before, renamed.SHA), settles(renamed.SHA, id, failing...))
		facts = append(facts, head, settle)
		pending, oraclePending, lastArrival, id = nil, nil, len(facts)-1, id+1
	}
	d.reference, d.oracle = append(d.reference, pending...), append(d.oracle, oraclePending...)
	if bounded {
		placed := make([]bool, len(pushes))
		for f := 0; ; {
			var eligible []int
			for k := range pushes {
				if !placed[k] && after[k] <= f && (earlier[k] < 0 || placed[earlier[k]]) {
					eligible = append(eligible, k)
				}
			}
			if f == len(facts) && len(eligible) == 0 {
				break
			}
			if len(eligible) > 0 && (f == len(facts) || rng.IntN(2) == 0) {
				k := eligible[rng.IntN(len(eligible))]
				placed[k] = true
				d.order = append(d.order, pushed(pushes[k]))
			} else {
				d.order, f = append(d.order, facts[f]), f+1
			}
		}
		return d
	}
	shuffled := append([]record.ClassifiedPush{}, pushes...)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	for f, p := 0, 0; f < len(facts) || p < len(shuffled); {
		if p < len(shuffled) && (f == len(facts) || rng.IntN(2) == 0) {
			d.order, p = append(d.order, pushed(shuffled[p])), p+1
		} else {
			d.order, f = append(d.order, facts[f]), f+1
		}
	}
	return d
}
