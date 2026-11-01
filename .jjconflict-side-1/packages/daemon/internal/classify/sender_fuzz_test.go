package classify

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// syncKind is how a random history's synchronize of one move is delivered: on time, late - after
// a newer synchronize was applied, so it moves no head (NameMove) - or never.
type syncKind int

const (
	onTime syncKind = iota
	lateSync
	lostSync
)

// sentStep is one push of a random history whose synchronizes name their move and its sender, and
// how that move's synchronize is delivered.
type sentStep struct {
	push record.ClassifiedPush
	sync syncKind
}

// pushSenders are the accounts a random history's pushes come from: the review App's and two
// people's.
var pushSenders = []string{"app", "alice", "bob"}

// sentHistory is one random history with moves made again - fresh commits, forced returns that
// undo the move before them, the same two heads pushed again - each push a random sender's, each
// pair of heads changing the same paths and forced or not whenever it is pushed, as those are facts
// of the two commits. Each synchronize but the last is delivered on time or late; with lose, one of
// them is never delivered.
func sentHistory(rng *rand.Rand, lose bool) []sentStep {
	var steps []sentStep
	cur := "h0"
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
		sender := pushSenders[rng.IntN(len(pushSenders))]
		kind := onTime
		if k < n && rng.IntN(2) == 0 {
			kind = lateSync
		}
		steps = append(steps, sentStep{push: record.ClassifiedPush{Before: cur, SHA: to, Forced: f[0], HandoffOnly: f[1], ByReviewApp: sender == "app", Sender: sender}, sync: kind})
		cur = to
	}
	if lose {
		steps[rng.IntN(len(steps)-1)].sync = lostSync
	}
	return steps
}

// sentDeliveries is a history's facts: the reference - each push before the first synchronize
// delivered on time at or after its move, each late synchronize just after that one's settlement
// - one other order of delivery, and the oracle's: the reference with each head a move made again
// reaches renamed as a fresh commit, named its head back by named.
type sentDeliveries struct {
	reference, order, oracle []runFact
	named                    map[string]string
}

// sentFacts builds a history's deliveries. A random order places each push anywhere and each late
// synchronize anywhere after the synchronize that made it late; a bounded one also delivers no push
// before the synchronize of the head its move left, and the pushes of the same two heads in the
// order they were made.
func sentFacts(rng *rand.Rand, steps []sentStep, bounded bool) sentDeliveries {
	// GitHub's clock is to the second: each synchronize is in the second of the one before it or the
	// next, so moves share a second about half the time.
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ticks := make([]time.Duration, len(steps))
	for k := range ticks {
		ticks[k] = time.Second
		if k > 0 {
			ticks[k] = ticks[k-1] + time.Duration(rng.IntN(2))*time.Second
		}
	}
	clock := func(k int) time.Time { return base.Add(ticks[k]) }
	labels := make([]string, len(steps))
	latest, used := map[string]string{"h0": "h0"}, map[[2]string]bool{}
	label := func(i int) string {
		if i < 0 {
			return "h0"
		}
		return labels[i]
	}
	d := sentDeliveries{named: map[string]string{"h0": "h0", "": ""}}
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
		d.named[name] = s.push.SHA
	}
	renamed := func(i int) record.ClassifiedPush {
		p := steps[i].push
		p.Before, p.SHA = label(i-1), label(i)
		return p
	}
	// next[k] is the first step at or after k whose synchronize is delivered on time.
	next := make([]int, len(steps))
	for k := len(steps) - 1; k >= 0; k-- {
		next[k] = k
		if steps[k].sync != onTime {
			next[k] = next[k+1]
		}
	}
	synced := func(p record.ClassifiedPush, k int, late bool) runFact {
		return runFact{head: p.SHA, before: p.Before, sender: p.Sender, at: clock(k), late: late, made: k + 1}
	}
	// The skeleton is each on-time synchronize and its settlement, in order.
	var skeleton, oracleSkeleton [][2]runFact
	onTimeAt := make([]int, len(steps)) // the skeleton index of each on-time step
	failing := map[int][]string{}
	id := int64(2)
	for k, s := range steps {
		if s.sync != onTime {
			continue
		}
		if rng.IntN(2) == 0 {
			failing[k] = []string{"ci"}
		}
		onTimeAt[k] = len(skeleton)
		skeleton = append(skeleton, [2]runFact{synced(s.push, k, false), settles(s.push.SHA, id, failing[k]...)})
		oracleSkeleton = append(oracleSkeleton, [2]runFact{synced(renamed(k), k, false), settles(label(k), id, failing[k]...)})
		id++
	}
	for j, pair := range skeleton {
		for k := range steps {
			if next[k] == onTimeIndex(onTimeAt, steps, j) {
				d.reference = append(d.reference, pushed(steps[k].push))
				d.oracle = append(d.oracle, pushed(renamed(k)))
			}
		}
		d.reference = append(d.reference, pair[0], pair[1])
		d.oracle = append(d.oracle, oracleSkeleton[j][0], oracleSkeleton[j][1])
		for k := range steps {
			if steps[k].sync == lateSync && next[k] == onTimeIndex(onTimeAt, steps, j) {
				d.reference = append(d.reference, synced(steps[k].push, k, true))
				d.oracle = append(d.oracle, synced(renamed(k), k, true))
			}
		}
	}
	// One other order: a random walk over the skeleton, each extra fact placed once it may be.
	type extra struct {
		fact  runFact
		after int // the skeleton facts that must be delivered first
		key   [2]string
		k     int
	}
	var extras []extra
	for k, s := range steps {
		after := 0
		if bounded {
			for p := k - 1; p >= 0; p-- {
				if steps[p].sync == onTime {
					after = 2*onTimeAt[p] + 1
					break
				}
			}
		}
		extras = append(extras, extra{fact: pushed(s.push), after: after, key: pushKey(s.push), k: k})
		if s.sync == lateSync {
			extras = append(extras, extra{fact: synced(s.push, k, true), after: 2*onTimeAt[next[k]] + 1, k: -1})
		}
	}
	placed := make([]bool, len(extras))
	flat := make([]runFact, 0, 2*len(skeleton))
	for _, pair := range skeleton {
		flat = append(flat, pair[0], pair[1])
	}
	for f := 0; ; {
		var eligible []int
		for e, x := range extras {
			if placed[e] || x.after > f {
				continue
			}
			if bounded && x.k >= 0 {
				earlier := false
				for o, y := range extras {
					if !placed[o] && y.k >= 0 && y.k < x.k && y.key == x.key {
						earlier = true
					}
				}
				if earlier {
					continue
				}
			}
			eligible = append(eligible, e)
		}
		if f == len(flat) && len(eligible) == 0 {
			break
		}
		if len(eligible) > 0 && (f == len(flat) || rng.IntN(2) == 0) {
			e := eligible[rng.IntN(len(eligible))]
			placed[e] = true
			d.order = append(d.order, extras[e].fact)
		} else {
			d.order, f = append(d.order, flat[f]), f+1
		}
	}
	return d
}

// gapLoops is whether a synchronize delivered on time follows skipped ones whose moves, with its
// own, make a loop: a head the branch reached twice between that synchronize and the one before it
// delivered on time (h0 for the first), that head included.
func gapLoops(steps []sentStep) bool {
	at := "h0"
	visited := map[string]bool{at: true}
	for _, s := range steps {
		if s.sync == lostSync {
			continue
		}
		if visited[s.push.SHA] {
			return true
		}
		visited[s.push.SHA] = true
		if s.sync == onTime {
			visited = map[string]bool{s.push.SHA: true}
		}
	}
	return false
}

// sameSecondReversed is whether order delivers a synchronize after one of a move made later in the
// same second of GitHub's clock: two synchronizes the clock cannot order, delivered in the reverse
// of their order.
func sameSecondReversed(order []runFact) bool {
	var seen []runFact
	for _, f := range order {
		if f.head == "" || f.made == 0 {
			continue
		}
		for _, g := range seen {
			if g.at.Equal(f.at) && g.made > f.made {
				return true
			}
		}
		seen = append(seen, f)
	}
	return false
}

// onTimeIndex is the step index of the j-th on-time synchronize.
func onTimeIndex(onTimeAt []int, steps []sentStep, j int) int {
	for k, s := range steps {
		if s.sync == onTime && onTimeAt[k] == j {
			return k
		}
	}
	return -1
}

// Random histories whose synchronizes name their move and its sender (Arrival) - fresh commits,
// forced returns that undo the move before them, the same two heads pushed again by the same or
// another sender, a red or green settlement at every synchronize delivered on time, some
// synchronizes delivered late - are judged alike in every order GitHub may deliver their pushes and
// late synchronizes in, close their head run, and are judged as the oracle judges them: the same
// history with every move made again aimed at a fresh commit. No history is left out. Two pushes of
// the same two heads that differ - the floor of a rule that reads the pushes alone - are told apart
// by their senders. A history one of whose synchronizes is never delivered is the floor of this
// rule: no fact says which push a move the daemon never saw synchronize took; those are judged and
// counted apart, and logged. LEGION_RUN_FUZZ_ITERS sets the number of histories of each order (the
// long mode), LEGION_RUN_FUZZ_SEED the seed, as for the run's own fuzz.
func TestEveryOrderOfASendersMovesIsJudgedAsTheOracleJudgesThem(t *testing.T) {
	iters, seed := 2000, uint64(11)
	if s := os.Getenv("LEGION_RUN_FUZZ_ITERS"); s != "" {
		iters, _ = strconv.Atoi(s)
	}
	if s := os.Getenv("LEGION_RUN_FUZZ_SEED"); s != "" {
		seed, _ = strconv.ParseUint(s, 10, 64)
	}
	for _, bounded := range []bool{false, true} {
		for _, lose := range []bool{false, true} {
			rng := rand.New(rand.NewPCG(seed, 5))
			unlike, unlikeOracle, open, failures, loops, loopFailures, ties, tieFailures := 0, 0, 0, 0, 0, 0, 0, 0
			for it := range iters {
				steps := sentHistory(rng, lose)
				history := sentFacts(rng, steps, bounded)
				loop := gapLoops(steps)
				tie := !loop && sameSecondReversed(history.order)
				if tie {
					ties++
				}
				if loop {
					loops++
				}
				start := redAtH0()
				start.PlannedRed = rng.IntN(2) == 0
				want := delivered(t, start, history.reference...)
				got := delivered(t, start, history.order...)
				truth := delivered(t, start, history.oracle...)
				orderWrong := JudgementChanged(want, got)
				oracleWrong := want.FixAttempts != truth.FixAttempts || want.PlannedRed != truth.PlannedRed ||
					HeadVerdict(want) != HeadVerdict(truth) || want.HeadCounted != history.named[truth.HeadCounted]
				stuck := got.Run != nil || want.Run != nil || truth.Run != nil
				if orderWrong {
					unlike++
				}
				if oracleWrong {
					unlikeOracle++
				}
				if stuck {
					open++
				}
				if !lose && (orderWrong || oracleWrong || stuck) && loop {
					loopFailures++
				}
				if !lose && (orderWrong || oracleWrong || stuck) && tie {
					tieFailures++
				}
				if !lose && (orderWrong || oracleWrong || stuck) && !loop && !tie {
					if failures++; failures > 3 {
						continue
					}
					t.Errorf("bounded %v, history %d:\n reference %v\n order     %v\n oracle    %v\n got   %s\n want  %s\n truth %s", bounded, it,
						history.reference, history.order, history.oracle, judged(got, "", ""), judged(want, "", ""), judged(truth, "", ""))
				}
			}
			t.Logf("bounded %v, a synchronize lost %v: %d histories (%d with a loop in a skipped gap, %d more with synchronizes of one second delivered reversed); judged unlike the reference order %d, unlike the oracle %d, a head run left open %d; misjudged with a loop %d, with reversed synchronizes %d",
				bounded, lose, iters, loops, ties, unlike, unlikeOracle, open, loopFailures, tieFailures)
			if failures > 3 {
				t.Errorf("bounded %v: %d histories with every synchronize delivered misjudged in all", bounded, failures)
			}
		}
	}
}
