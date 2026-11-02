package classify

import (
	"sort"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// Arrival is what a synchronize says of the move a head arrives by: Before, the head GitHub says
// the branch moved from, and Sender, the account whose push moved it (the webhook's sender), each
// empty when the fact names none - a reopen, a close at another head, the daemon's own read
// of the head, a listener that predates the field; and At, GitHub's clock of the synchronize (the
// pull request's updated_at), zero when unknown. Since is GitHub's clock of the head the record
// held, zero when unknown: the moves a skipped synchronize named between the two are this
// arrival's (arrivalPushes).
type Arrival struct {
	Before, Sender string
	At, Since      time.Time
}

// AdvancePullRequestHead moves the pull request to a new observed head (advance), arriving by the
// move arrival names. A head that arrives before every push of its
// move is recorded - no chain of recorded pushes leads to it from the recorded head
// (arrivalPushes), or its synchronize names a move the pushes cannot yet tell - opens a head run,
// and a head that arrives while one is open joins it: each push, and each move a late synchronize
// names (NameMove), replays the run (replayRun), so the moves made and the CI settlement that
// stands are those every push and synchronize arriving first would have given.
func AdvancePullRequestHead(pr record.PullRequest, arrival Arrival, headSHA string) record.PullRequest {
	if pr.Run == nil {
		if read := arrivalPushes(pr, arrival, headSHA, nil); (read.chained(pr.HeadSHA, headSHA) && !read.waits) || pr.HeadSHA == "" {
			pr, _, _ = advance(pr, arrival, headSHA, nil)
			return pr
		}
		pr.Run = &record.HeadRun{Anchor: judgementOf(pr), Pushes: withoutApprovalCarriers(pr.Pushes), Since: pr.HeadUpdatedAt}
	}
	pr = runStep(pr, record.RunStep{Head: headSHA, Before: arrival.Before, Sender: arrival.Sender, SyncedAt: arrival.At})
	pr.Pushes = unspentPushes(pr.Pushes, pr.Moves, true)
	return pr
}

// MovesBy is whether a synchronize to head is a move of the head the record holds (pr.HeadSHA): a
// head other than it. A synchronize to the recorded head is no move, as the release before read
// it, whatever it names: a copy of the synchronize that reached the head (GitHub can send two for
// one push), or the branch back at the head after moves whose synchronizes were skipped - a loop in
// a skipped gap (the loop floor, unspentPushes), which the release before reads as no move and so
// does this one. It still names its move (NameMove).
func MovesBy(pr record.PullRequest, head string) bool {
	return head != "" && head != pr.HeadSHA
}

// NameMove records the move a synchronize names that moves no head - one older than the newest
// applied (LateLifecycle), or one that returned the branch to the recorded head - while a head run
// is open and an arrival in it waits for it (inAGap): which push, by its sender, the move from
// named.Before to named.SHA took. It is a fact about the branch, as a push is, so the run replays
// with it (replayRun), and an arrival whose synchronize the daemon skipped is judged by its own
// push. It moves no head and opens no run, and changes nothing - pr is returned as it was - with no
// run open, when no arrival of the run waits for it, when it was named already, or when it names no
// sender.
func NameMove(pr record.PullRequest, named record.Named) record.PullRequest {
	if pr.Run == nil || named.Before == "" || named.SHA == "" || named.Sender == "" || named.Before == named.SHA ||
		!inAGap(*pr.Run, named) || namedAlready(pr.Run.Names, named) {
		return pr
	}
	run := *pr.Run
	run.Names = append(clone(run.Names), named)
	if len(run.Names) > maxMoves {
		run.Names = run.Names[len(run.Names)-maxMoves:]
	}
	pr.Run = &run
	return replayRun(pr)
}

// namedAlready is whether names holds named (sameName): a synchronize delivered again names
// nothing new.
func namedAlready(names []record.Named, named record.Named) bool {
	for _, n := range names {
		if sameName(n, named) {
			return true
		}
	}
	return false
}

// inAGap is whether named can be a move of an arrival of run whose synchronize skipped it: a head
// step naming its move by its sender whose synchronize GitHub's clock puts named no later than, and
// no earlier than the head before it (record.HeadRun.Since for the first) - GitHub's clock is to
// the second, so a skipped move can share the second of either - unless named is that head's own
// move, at its own clock (inWindow). A clock unknown on either side fences nothing.
func inAGap(run record.HeadRun, named record.Named) bool {
	since, prev := run.Since, run.Anchor.HeadSHA
	for _, s := range run.Steps {
		if s.Head == "" {
			continue
		}
		if s.Before != "" && s.Sender != "" && !LateLifecycle(s.SyncedAt, named.At) && inWindow(named, prev, since) {
			return true
		}
		since, prev = s.SyncedAt, s.Head
	}
	return false
}

// inWindow is whether named comes no earlier than since, GitHub's clock of the head the window
// starts at (start), and is not that head's own move at that clock: a copy of the synchronize that
// reached it.
func inWindow(named record.Named, start string, since time.Time) bool {
	if named.At.IsZero() || since.IsZero() {
		return true
	}
	return !named.At.Before(since) && !(named.At.Equal(since) && named.SHA == start)
}

// advance moves the pull request to a new head, by the move arrival names, and says whether a
// chain of recorded pushes led to it from the recorded head (chained) with nothing left for it to
// wait for (arrivalPushes), and returns the named moves its judgement left (names, the run's). The
// recorded CI settlement stays, as CheckedHead's: it is the new head's too once every push between
// them is known to change only .legion/ (Settled), a settlement of another head of that same tree
// merges into it, and one of a head holding other code replaces it
// (SettlementFor). GitHub's own verdict on merging the head - Mergeability and MergeState - stays
// only while the recorded pushes say the move changed nothing GitHub checks: every push between
// the two heads changed only the issue's own handoffs, the same test that carries an approval
// across (carriedBack). A handoff-only push changes nothing GitHub would check, so the verdict
// read at the head before it still describes the tree being merged.
//
// Every other move takes the verdict with it, a move the recorded pushes cannot link yet among
// them - a synchronize that arrived before the push webhook of its own move. The alternative was
// to record the head GitHub answered for beside the answer and read a verdict of another head as
// unread; this is the same rule with no second field to keep true, since a verdict is kept only
// where the pushes prove the two heads hold one tree, and a kept verdict of another head is
// exactly what moved an approval of a new head on the old head's `clean`
// (dispatch://INFRA-714 R2). It costs nothing in the end and at most one read meanwhile: the
// arrival opens a head run whose anchor keeps the verdict (record.Judgement), so the push that
// links the move replays the arrival and gives the verdict back when the push says the move
// carried it, and the arrival is itself a fact that asks GitHub for the new head's state
// (workflow's askMergeState) for as long as it does not. The move and
// the chain's pushes join the moves made (madeMoves), and the pushes they spend go
// (unspentPushes). Nothing here judges the settlement: a CI result moves no issue
// (dispatch://INFRA-714/spec R1), so a head arrives counting nothing.
func advance(pr record.PullRequest, arrival Arrival, headSHA string, names []record.Named) (record.PullRequest, bool, []record.Named) {
	read := arrivalPushes(pr, arrival, headSHA, names)
	var chain []record.ClassifiedPush
	chained := read.path
	if read.path {
		chain = read.pushes
	} else {
		chain = chainOf(read.pushes, pr.HeadSHA, headSHA)
		chained = chain != nil
	}
	// The verdict is read against the pull request's own pushes, before the move spends them, as
	// the reader of an approval reads it (ApprovalStands).
	if !carriedBack(pr.Pushes, headSHA, pr.HeadSHA) {
		pr.Mergeability, pr.MergeState = "", record.MergeStateUnread
	}
	if !read.waits {
		// An arrival that waits is judged for now by pushes that may not be its own (provisional),
		// so it makes no move and spends no push: each stays for the arrival whose own it is.
		pr.Moves = madeMoves(pr.Moves, pr.HeadSHA, headSHA, chain, pr.Pushes)
	}
	pr.Pushes = unspentPushes(pr.Pushes, pr.Moves, true)
	pr.HeadSHA = headSHA
	return pr, chained && !read.waits, read.names
}

// maxMoves bounds the moves a pull request remembers (record.PullRequest.Moves): a push for a move
// older than that arrives late enough to wait as a push not yet linked, until boundedPushes lets
// it go.
const maxMoves = 32

// madeMoves is moves with the move from previous to head and the moves of chain's pushes made,
// each pair of heads once and the newest last, at most maxMoves of them. A chain that ends at head
// has the move from previous to head as its last push's, and that move is made once. A move of the
// chain is pushed (record.Move.Pushed) unless its push carries an approval across, which is never
// spent; the move from previous to head, when no push of its own made it, is pushed when a chain
// of recorded pushes made it, since GitHub sends no push of those two heads for it, and otherwise
// waits for its push.
//
// A move made again keeps the repeats its earlier record queued, less those the chain took
// (arrivalPushes); and every other code push of recorded, the record's pushes, whose two heads a
// move now has - a later push of the same two heads than the one the chain took - is queued on
// that move (pushedMoves), in the order it was delivered.
func madeMoves(moves []record.Move, previous, head string, chain, recorded []record.ClassifiedPush) []record.Move {
	made := make([]record.Move, 0, len(chain)+1)
	for _, p := range chain {
		if moveIndex(made, p.Before, p.SHA) < 0 {
			made = append(made, record.Move{Before: p.Before, SHA: p.SHA, Pushed: !carriesApproval(p)})
		}
	}
	if previous != "" && previous != head && moveIndex(made, previous, head) < 0 {
		made = append(made, record.Move{Before: previous, SHA: head, Pushed: chain != nil})
	}
	for i := range made {
		earlier := moveIndex(moves, made[i].Before, made[i].SHA)
		if earlier < 0 || len(moves[earlier].Repeats) == 0 {
			continue
		}
		if repeats := withoutCopies(moves[earlier].Repeats, chain); len(repeats) > 0 {
			made[i].Repeats = clone(repeats)
		}
	}
	kept := make([]record.Move, 0, len(moves)+len(made))
	for _, m := range moves {
		if moveIndex(made, m.Before, m.SHA) < 0 {
			kept = append(kept, m)
		}
	}
	kept = append(kept, made...)
	if len(kept) > maxMoves {
		kept = kept[len(kept)-maxMoves:]
	}
	taken := clone(chain)
	for _, p := range recorded {
		if k := indexOf(taken, p); k >= 0 {
			taken = append(taken[:k:k], taken[k+1:]...)
			continue
		}
		if !carriesApproval(p) && moveIndex(kept, p.Before, p.SHA) >= 0 {
			kept, _ = pushedMoves(kept, p)
		}
	}
	return kept
}

// moveIndex is the index of the move from before to sha among moves, -1 when it is not there.
func moveIndex(moves []record.Move, before, sha string) int {
	for i, m := range moves {
		if m.Before == before && m.SHA == sha {
			return i
		}
	}
	return -1
}

// indexOf is the index of the first push of pushes equal to p, -1 when none is.
func indexOf(pushes []record.ClassifiedPush, p record.ClassifiedPush) int {
	for i, q := range pushes {
		if q == p {
			return i
		}
	}
	return -1
}

// maxRepeats bounds the repeats a move queues (record.Move.Repeats): past it the oldest goes, as
// the oldest push waiting for a link goes past maxWaitingPushes.
const maxRepeats = maxWaitingPushes

// pushedMoves is moves once push arrives, and the repeat the move let go past maxRepeats, if any:
// a move of push's two heads made before its push was recorded takes push as that push, and is
// pushed from then on; one already pushed queues push as its newest repeat (record.Move.Repeats) -
// the push delivered again, or the branch moving between the two heads again, which only the
// head's next arrival by that move tells apart. Either way the move spends push (unspentPushes). A
// push that carries an approval across is never spent, and changes no move.
func pushedMoves(moves []record.Move, push record.ClassifiedPush) ([]record.Move, *record.ClassifiedPush) {
	i := moveIndex(moves, push.Before, push.SHA)
	if i < 0 || carriesApproval(push) {
		return moves, nil
	}
	moves = clone(moves)
	if !moves[i].Pushed {
		moves[i].Pushed = true
		return moves, nil
	}
	repeats := append(clone(moves[i].Repeats), push)
	var gone *record.ClassifiedPush
	if len(repeats) > maxRepeats {
		gone, repeats = &repeats[0], repeats[1:]
	}
	moves[i].Repeats = repeats
	return moves, gone
}

// arrivalPushes is the pushes the arrival of head, by the move arrival names, is judged with;
// whether the arrival waits for what its synchronize names to be recorded (waits), which keeps a
// head run open; and the named moves of a run (names) its judgement leaves. Its three readings:
//
//   - A synchronize that names no move (no before) takes no repeat and the newest recorded push of
//     each two heads (newestOfEachMove), as the release before this one did, which kept the newest
//     push of each two heads alone: behind a listener that sends no before, the daemon judges a
//     history as that release did (see unspentPushes for the one change).
//   - One that names the head it moved from but not its sender takes the oldest recorded push of
//     each two heads (oldestOfEachMove) and, when no chain of them leads to head, a chain through
//     before that takes the oldest repeat of each move (beforeChain).
//   - One that names both takes the push of its own move by its sender (pushOf), and, when the
//     record's head is not before - the synchronizes of the moves between were skipped - the pushes
//     their own late synchronizes named (NameMove): the run's named moves GitHub's clock puts no
//     earlier than the record's head (Since, inWindow) and no later than this synchronize, in that
//     clock's order, which must lead one by one from the record's head to before (gapPath), each
//     push by its sender. Those named moves are spent by the
//     arrival. Until its own push, that path and each of its pushes are recorded it waits, judged
//     meanwhile by the pushes it has (provisional): a named move's push, and for two heads no
//     synchronize named, the newest recorded push (the release before's). A synchronize lost for
//     good leaves it so until the run's bound cuts it off.
func arrivalPushes(pr record.PullRequest, arrival Arrival, head string, names []record.Named) arrivalRead {
	before := arrival.Before
	if before == "" {
		return arrivalRead{pushes: newestOfEachMove(pr.Pushes), names: names}
	}
	if arrival.Sender == "" || pr.HeadSHA == "" {
		return arrivalRead{pushes: beforeChain(pr, before, head), names: names}
	}
	var required, optional []record.Named
	later := func(n record.Named) bool { return !arrival.At.IsZero() && !n.At.IsZero() && n.At.After(arrival.At) }
	for _, n := range names {
		switch {
		case later(n) || !inWindow(n, pr.HeadSHA, arrival.Since):
		case !n.At.IsZero() && (n.At.Equal(arrival.At) || n.At.Equal(arrival.Since)):
			optional = append(optional, n)
		default:
			required = append(required, n)
		}
	}
	seen := append(clone(required), optional...)
	waiting := func(own *record.ClassifiedPush) arrivalRead {
		return arrivalRead{pushes: provisional(pr, seen, own), waits: true, names: names}
	}
	own := pushOf(pr, before, head, arrival.Sender)
	if own == nil {
		return waiting(nil)
	}
	path, ok := gapPath(required, optional, pr.HeadSHA, before)
	if !ok {
		return waiting(own)
	}
	var gap []record.ClassifiedPush
	for _, n := range path {
		p := pushOf(pr, n.Before, n.SHA, n.Sender)
		if p == nil {
			return waiting(own)
		}
		gap = append(gap, *p)
	}
	// The arrival spends the moves on its path and every earlier one; a move of its own second it
	// did not take is the next arrival's, as is every later move.
	var left []record.Named
	for _, n := range names {
		if later(n) || (!arrival.At.IsZero() && n.At.Equal(arrival.At) && !namedAlready(path, n)) {
			left = append(left, n)
		}
	}
	// A path that reaches a head twice made a loop in the skipped gap; the loop is erased, as the
	// release before's shortest chain reads every move, so its pushes decide nothing (the loop
	// floor, unspentPushes). A move back to the record's head (MovesBy) is no loop.
	return arrivalRead{pushes: withoutLoops(pr.HeadSHA, append(gap, *own)), path: true, names: left}
}

// sameName is whether a and b name the same move by the same sender at the same clock.
func sameName(a, b record.Named) bool {
	return a.Before == b.Before && a.SHA == b.SHA && a.Sender == b.Sender && a.At.Equal(b.At)
}

// withoutLoops is path, the pushes of a move from from in the order the branch made them, with
// each loop erased: a push that reaches a head the path already reached drops the pushes since
// that head, itself with them, unless it is the path's last and reaches from - a move back to the
// head the move left.
func withoutLoops(from string, path []record.ClassifiedPush) []record.ClassifiedPush {
	kept := make([]record.ClassifiedPush, 0, len(path))
	reached := map[string]int{from: 0}
	for i, p := range path {
		if k, ok := reached[p.SHA]; ok && (i < len(path)-1 || p.SHA != from) {
			for _, q := range kept[k:] {
				delete(reached, q.SHA)
			}
			reached[p.SHA] = k
			kept = kept[:k]
			continue
		}
		kept = append(kept, p)
		reached[p.SHA] = len(kept)
	}
	return kept
}

// arrivalRead is what an arrival is judged with (arrivalPushes): pushes, which are the move's own
// path, in the order the branch made them, when path is set, and pushes to find a chain in
// otherwise; whether the arrival waits for a push or a named move still to be recorded (waits); and
// the named moves it leaves for the arrivals after it.
type arrivalRead struct {
	pushes      []record.ClassifiedPush
	path, waits bool
	names       []record.Named
}

// chained is whether read leads from previous to head: its path, or a chain of its pushes.
func (read arrivalRead) chained(previous, head string) bool {
	return read.path || chainOf(read.pushes, previous, head) != nil
}

// gapPath is the moves the branch made from from to before whose synchronizes were skipped, and
// whether they lead one by one from from to before - each move from the head the one before it
// reached - in the order GitHub's clock puts them: every move of required, and those of optional
// that lead there. Optional are the moves in the second the window starts or ends in, which can be
// the window's before them or after them: GitHub's clock is to the second. Of the paths that lead,
// the one taking the fewest optional moves is the path, its moves of one second in the one order
// of them that so leads; none, or more than one such path, leaves the path unled (as does trying
// more than maxTiedOrders). Named moves of unknown clock are
// taken as the shortest chain of the newest of each two heads. A move missing from the middle, the
// start or the end of the path leaves it unled, so the arrival waits for it; a loop the branch made
// inside the path, a head left and returned to, whose synchronizes are still to come, is not seen
// missing (TestAMoveMadeAgainInTheGapOfASkippedSynchronize).
func gapPath(required, optional []record.Named, from, before string) ([]record.Named, bool) {
	all := append(clone(required), optional...)
	for _, n := range all {
		if n.At.IsZero() {
			byKey := map[[2]string]record.Named{}
			var named []record.ClassifiedPush
			for _, m := range newestOfEachName(all) {
				byKey[[2]string{m.Before, m.SHA}] = m
				named = append(named, record.ClassifiedPush{Before: m.Before, SHA: m.SHA})
			}
			chain := chainOf(named, from, before)
			path := make([]record.Named, 0, len(chain))
			for _, p := range chain {
				path = append(path, byKey[pushKey(p)])
			}
			return path, chain != nil || from == before
		}
	}
	sorted := make([]candidate, 0, len(all))
	for i, n := range all {
		sorted = append(sorted, candidate{n, i >= len(required)})
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].named.At.Before(sorted[j].named.At) })
	// Of the paths that lead, the one that takes the fewest optional moves: a move of the window's
	// own second it need not take is the neighbour window's, as it is when it is a second apart.
	var best []record.Named
	bestOptional, ties, tries := -1, 0, 0
	var walk func(start int, at string, acc []record.Named, optional int)
	walk = func(start int, at string, acc []record.Named, optional int) {
		if tries > maxTiedOrders || (bestOptional >= 0 && optional > bestOptional) {
			return
		}
		if start == len(sorted) {
			if at != before {
				return
			}
			if bestOptional < 0 || optional < bestOptional {
				best, bestOptional, ties = acc, optional, 1
			} else {
				ties++
			}
			return
		}
		end := start
		for end < len(sorted) && sorted[end].named.At.Equal(sorted[start].named.At) {
			end++
		}
		var order func(left []candidate, at string, acc []record.Named, optional int)
		order = func(left []candidate, at string, acc []record.Named, optional int) {
			if tries++; tries > maxTiedOrders {
				return
			}
			if onlyOptional(left) {
				walk(end, at, acc, optional)
			}
			for k, c := range left {
				if c.named.Before == at {
					rest := append(append(make([]candidate, 0, len(left)-1), left[:k]...), left[k+1:]...)
					took := optional
					if c.optional {
						took++
					}
					order(rest, c.named.SHA, append(clone(acc), c.named), took)
				}
			}
		}
		order(sorted[start:end], at, acc, optional)
	}
	walk(0, from, nil, 0)
	if bestOptional < 0 || ties != 1 || tries > maxTiedOrders {
		return nil, false
	}
	return best, true
}

// candidate is a move gapPath may put on a path: one the path must take, or an optional one.
type candidate struct {
	named    record.Named
	optional bool
}

// onlyOptional is whether every move of left is optional, so a path may leave them all.
func onlyOptional(left []candidate) bool {
	for _, c := range left {
		if !c.optional {
			return false
		}
	}
	return true
}

// maxTiedOrders bounds the orders gapPath tries of moves GitHub's clock puts in one second.
const maxTiedOrders = 4096

// provisional is the pushes an arrival that still waits is judged with: one push of each two heads
// recorded, waiting or a repeat - own for its own move when it is recorded, the push a named move
// took (seen) when it is recorded, and otherwise the newest recorded.
func provisional(pr record.PullRequest, seen []record.Named, own *record.ClassifiedPush) []record.ClassifiedPush {
	all := clone(pr.Pushes)
	for _, m := range pr.Moves {
		all = append(all, m.Repeats...)
	}
	pick := map[[2]string]record.ClassifiedPush{}
	for _, p := range all {
		pick[pushKey(p)] = p
	}
	for _, n := range newestOfEachName(seen) {
		if p := pushOf(pr, n.Before, n.SHA, n.Sender); p != nil {
			pick[pushKey(*p)] = *p
		}
	}
	if own != nil {
		pick[pushKey(*own)] = *own
	}
	kept := make([]record.ClassifiedPush, 0, len(pick))
	for _, p := range newestOfEachMove(all) {
		kept = append(kept, pick[pushKey(p)])
	}
	return kept
}

// beforeChain is the pushes an arrival by the move from before, its sender not named, is judged
// with: the oldest recorded push of each two heads (oldestOfEachMove), which a move takes before a
// later one; and, when no chain of them leads to head from pr's head, a chain that also takes the
// oldest repeat of each move (record.Move.Repeats) and passes through before: from pr's head to
// before - moves whose synchronizes the record missed - and on from before to head.
func beforeChain(pr record.PullRequest, before, head string) []record.ClassifiedPush {
	pushes := oldestOfEachMove(pr.Pushes)
	if pr.HeadSHA == "" || chainOf(pushes, pr.HeadSHA, head) != nil {
		return pushes
	}
	with := clone(pushes)
	for _, m := range pr.Moves {
		if len(m.Repeats) > 0 {
			with = append(with, m.Repeats[0])
		}
	}
	if len(with) == len(pushes) {
		return pushes
	}
	var first []record.ClassifiedPush
	if before != pr.HeadSHA {
		if first = chainOf(with, pr.HeadSHA, before); first == nil {
			return pushes
		}
	}
	second := chainOf(with, before, head)
	if second == nil {
		return pushes
	}
	return append(first, second...)
}

// pushOf is the recorded push of the move from before to head by sender: the oldest such push
// waiting, else the push of those two heads that carries an approval across - who pushed it decides
// nothing of it (sparesAttempt, MayChangeCode) - else the oldest such repeat of the move. A push
// that names no sender, recorded from a listener that predates the field, is taken for any
// sender's. Two pushes of the same two heads by the same sender change the same paths in the same
// way and are interchangeable. Nil when none is recorded: the push is on its way.
func pushOf(pr record.PullRequest, before, head, sender string) *record.ClassifiedPush {
	by := func(p record.ClassifiedPush) bool {
		return p.Before == before && p.SHA == head && (p.Sender == sender || p.Sender == "")
	}
	for _, p := range pr.Pushes {
		if by(p) && !carriesApproval(p) {
			return &p
		}
	}
	for _, p := range pr.Pushes {
		if p.Before == before && p.SHA == head && carriesApproval(p) {
			return &p
		}
	}
	if i := moveIndex(pr.Moves, before, head); i >= 0 {
		for _, p := range pr.Moves[i].Repeats {
			if by(p) {
				return &p
			}
		}
	}
	return nil
}

// newestOfEachName is names with only the newest by GitHub's clock, then by when it was recorded,
// of the names of each two heads: a move the branch made twice between two arrivals was made last
// by the newer.
func newestOfEachName(names []record.Named) []record.Named {
	newest := map[[2]string]int{}
	for i, n := range names {
		key := [2]string{n.Before, n.SHA}
		if j, ok := newest[key]; !ok || !names[j].At.After(n.At) {
			newest[key] = i
		}
	}
	kept := make([]record.Named, 0, len(newest))
	for i, n := range names {
		if newest[[2]string{n.Before, n.SHA}] == i {
			kept = append(kept, n)
		}
	}
	return kept
}

// oldestOfEachMove is pushes with only the first, in delivery order, of the pushes of each two
// heads: a move takes the oldest, and a later one is queued as its repeat (madeMoves).
func oldestOfEachMove(pushes []record.ClassifiedPush) []record.ClassifiedPush {
	seen := map[[2]string]bool{}
	kept := make([]record.ClassifiedPush, 0, len(pushes))
	for _, p := range pushes {
		if !seen[pushKey(p)] {
			seen[pushKey(p)] = true
			kept = append(kept, p)
		}
	}
	return kept
}

// newestOfEachMove is pushes with only the last, in delivery order, of the pushes of each two
// heads, each where it was delivered: what the release before this one recorded, which replaced a
// push with a later one of the same two heads.
func newestOfEachMove(pushes []record.ClassifiedPush) []record.ClassifiedPush {
	last := map[[2]string]int{}
	for i, p := range pushes {
		last[pushKey(p)] = i
	}
	kept := make([]record.ClassifiedPush, 0, len(last))
	for i, p := range pushes {
		if last[pushKey(p)] == i {
			kept = append(kept, p)
		}
	}
	return kept
}

// chainOf is the pushes of the shortest chain of recorded pushes from from to to, each of which
// said which head it replaced, oldest first; nil when no chain leads there.
func chainOf(pushes []record.ClassifiedPush, from, to string) []record.ClassifiedPush {
	if from == "" || from == to {
		return nil
	}
	type reach struct {
		head string
		via  []record.ClassifiedPush
	}
	reached := map[string]bool{to: true}
	for frontier := []reach{{head: to}}; len(frontier) > 0; {
		current := frontier[0]
		frontier = frontier[1:]
		for _, p := range pushes {
			if p.SHA != current.head || p.Before == "" || reached[p.Before] {
				continue
			}
			via := append([]record.ClassifiedPush{p}, current.via...)
			if p.Before == from {
				return via
			}
			reached[p.Before] = true
			frontier = append(frontier, reach{head: p.Before, via: via})
		}
	}
	return nil
}

// Settled says whether a CI settlement stands for the pull request's current head: one recorded
// for the head itself, or for a head the current one replaced through pushes that each changed
// only .legion/ and said which head they replaced (a handoff push can carry GitHub's skip-checks
// trailer and start no CI of its own). A checked head is recorded only with a settlement
// (SettlementFor, then ApplySettlement), and cleared with it (a new generation, migration 0028).
//
// A settlement carried back this way is an account of the code the head carries, which is what
// SettlementFor reads it for. It is no account of the current head's own CI: the push that
// carried it can have run checks of its own, and what they came to is in no settlement of the
// head it replaced. A reader that needs the current head's own checks asks
// ChecksConcludedOnHead.
func Settled(pr record.PullRequest) bool {
	return pr.CheckedHead != "" && carriedBack(pr.Pushes, pr.HeadSHA, pr.CheckedHead)
}

// ChecksConcludedOnHead says whether GitHub has reported a check run on the pull request's
// current head itself and every check run reported there has concluded. The fence holds the
// newest run of each check name with the head GitHub reported it on (record.AttemptRun), and a
// run reaches the fence only through a settlement, which the listener publishes for a commit only
// once every check it has seen there is terminal: so a run of the fence stamped with the current
// head is a run that concluded there, and its settlement is the account of every check GitHub
// had reported on that head having concluded.
//
// It is what an approval waiting on GitHub's merge state reads before it reports the head one
// GitHub will not merge (workflow's reviewRound). A check the base requires holds the head
// `blocked` while it runs, which says nothing about the round, and a settlement carried back
// from a lower head never answers for the current head's own checks: the handoff push that left
// this head carries no skip-checks trailer unless its author wrote one, so GitHub does run CI
// here and answers `blocked` while it does. A head GitHub reports no check run on at all - one
// whose push carried the trailer, so no workflow started there - never satisfies this, so such a
// round is never reported stuck. That is the rule's cost, and it is the smaller one: GitHub's
// verdict for the head below is carried across such a push (advance), so an approval carrying a
// verdict GitHub would merge reaches accepting without asking this at all, and one carrying a
// verdict GitHub would not merge waits on the reads that follow rather than telling a finding
// the daemon cannot tell apart from a check it has not been told of yet.
//
// A run stored before the fence carried the head it ran on names no head, so it is no run on the
// current head and such a record waits for the head's next settlement (record.AttemptRun).
func ChecksConcludedOnHead(pr record.PullRequest) bool {
	for _, run := range pr.CheckRuns {
		if run.Head != "" && run.Head == pr.HeadSHA {
			return true
		}
	}
	return false
}

// SettlementFor says whether a CI settlement of candidate.Head may stand for the pull request's
// current head, and returns the pull request ready to apply it. It may when head is the current
// head, or a head the current one replaced through pushes that each changed only .legion/.
// Every other settlement - an earlier code head's, a head a force push left - stands for nothing.
//
// A settlement of another head than the recorded one, with the recorded one standing for the
// current head (Settled), keeps the fence rather than starting it afresh: both heads carry back
// to the current head through pushes that changed only .legion/, so they hold one tree, and a run
// on either is a run on the same code. What each check name comes to is then the newest run of it
// across those heads, which the attempt set decides (ApplySettlement, over the fence
// mergeAttemptSets keeps): a check that runs per pull request - a title check, anything on a
// pull_request edited trigger - runs on whatever head is current and never on the code head
// again, so its run on a handoff head is the newer one, and a check that ran only on the code
// head keeps the conclusion it got there. The settlement then stands at the head nearer the
// current one, which is the candidate's when the candidate carries back to the recorded head;
// the listener's generation and snapshot number one head's own settlements, so moving the
// recorded head starts them afresh (ApplySettlement takes them only for the head they number).
//
// A settlement whose head holds other code than the recorded one - the recorded settlement no
// longer stands for the current head, a code push between them - starts the fence afresh, since
// check runs, generations and snapshots are each tree's own.
func SettlementFor(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	head := candidate.Head
	if !carriedBack(pr.Pushes, pr.HeadSHA, head) {
		return pr, false
	}
	if head == pr.CheckedHead {
		return pr, true
	}
	if Settled(pr) {
		if carriedBack(pr.Pushes, head, pr.CheckedHead) {
			pr.CheckedHead = head
			pr.Generation = 0
			pr.Snapshot = ""
		}
		return pr, true
	}
	pr.CheckedHead = head
	pr.Failing = []string{}
	pr.Cancelled = []string{}
	pr.CheckRuns = nil
	pr.Generation = 0
	pr.Snapshot = ""
	return pr, true
}

// headPush is the push that left head: the one that replaced previous when the branch has one,
// else any push that left head - a head a force push returned to can have been left before.
func headPush(pushes []record.ClassifiedPush, previous, head string) *record.ClassifiedPush {
	var found *record.ClassifiedPush
	for i := range pushes {
		if pushes[i].SHA != head {
			continue
		}
		if pushes[i].Before == previous {
			return &pushes[i]
		}
		if found == nil {
			found = &pushes[i]
		}
	}
	return found
}

// HeadPushKnown says whether the push that left head is recorded (headPush), as the push that
// replaced pr's head when the branch has one, so the pushes can say what the move from pr's head to
// head changed. Until it is, the head's code is unknown: its push may be on its way, with the paths
// an approval of pr's head is judged against (ApprovalStands).
func HeadPushKnown(pr record.PullRequest, head string) bool {
	return headPush(pr.Pushes, pr.HeadSHA, head) != nil
}

// unspentPushes is what the moves made leave of pushes: every push that changed only .legion/, did
// not rewrite history and said which head it replaced - a fact about two commits, true whenever it
// is learned, which an approval is carried across - and every other push whose own move, its two
// heads, the record has not made: a push still on its way, a link of a chain still to complete, or
// one that returns the branch to a commit it held before. A push is spent by its own move alone,
// never by the head it leaves: a branch reset to an earlier commit arrives there by a push of its
// own; the move keeps what it spent as its own push or a repeat (pushedMoves). A push that did not
// say which head it replaced is never a link: it stays on its way until a head arrives (arrival),
// and then goes.
//
// A push is matched to a move by its two heads, so the same two heads pushed again - h0..h1, a
// forced return h1..h0, then h0..h1 once more - are spent as the first push delivered twice is:
// neither is code on its way (CodeOnItsWay), and neither plans anything until a head arrives by its
// move. The record keeps every code push apart, in delivery order; a move takes the oldest of its
// two heads, and each later one is queued on the move (record.Move.Repeats, oldest first, at most
// maxRepeats). What an arrival takes for its push depends on what its synchronize names
// (arrivalPushes):
//
//   - Before and sender (the listener's synchronize carries both): the push of its own move by its
//     sender - a push of the same two heads by someone else is not it - and the moves between the
//     record's head and before, which late synchronizes name (NameMove), in GitHub's clock order,
//     each by its sender. Every move is judged by the push that made it, as a move to a fresh commit
//     is (TestAMoveMadeAgainIsJudgedByThePushThatMadeItAgain), and the two pushes of the same two
//     heads that only their order told apart (R1, below) are told apart by their senders
//     (TestASynchronizesSenderTellsTwoPushesOfTheSameTwoHeadsApart). GitHub's clock is to the
//     second, so a move in the second a window starts or ends in may be that window's or its
//     neighbour's (gapPath). A loop on the path - a head reached twice - is erased (withoutLoops),
//     and a synchronize to the recorded head is no move (MovesBy), each as the release before reads
//     it.
//   - Before alone: the oldest push of each two heads, and the oldest repeat on a chain through
//     before (beforeChain). R1 is the floor there: two pushes of the same two heads that differ -
//     in who pushed them - delivered in the reverse of the order they were made, which nothing but
//     the sender orders. The replay's shortest chain (chainOf) is a limit there too, shared with the
//     release before: pushes of another path delivered early can stand for a path the branch did
//     not take.
//   - Neither (an older listener, a reopen, a close at another head, the daemon's own read
//     of the head): no repeat and the newest push of each two heads, as the release before did
//     (TestAnArrivalThatNamesNoMoveIsJudgedAsTheReleaseBefore), a head run that a lost push opened
//     closing at its bound the one change; so this daemon may deploy before the listener.
//
// The rule over before and sender has three floors, each pinned by the fuzz of a sender's moves
// (TestEveryOrderOfASendersMovesIsJudgedAsTheOracleJudgesThem), which delivers every synchronize as
// the workflow does - late only when GitHub's clock puts it before the newest applied
// (LateLifecycle), exactly as the release before - and outside which every order of delivery is
// judged alike and as the fresh-commit oracle judges it. A synchronize is never dropped that the
// release before would apply.
//
//   - A loop in a skipped gap: the moves whose synchronizes are late reach a head twice (the record's
//     head, or one between it and before). The late names of a loop that ends where it began are
//     not missed by the path that leads from the record's head to before without them, so an
//     arrival can close its head run before they arrive, and a late synchronize opens no run; which
//     of them it saw then depends on delivery order. No rule avoids it: until a loop's facts arrive
//     they are the facts of a branch that made no loop, where the run must close. Inside this floor
//     the loop is read as the release before reads it - erased, and a return to the recorded head
//     no move - because the fresh-commit oracle judged by this rule and the same oracle judged by
//     the release before then agree that this rule is right where the release before is wrong more
//     often than the reverse; counting the loop's pushes, they did not agree. In the fuzz's long
//     mode about 119,500 of 200,000 histories have such a loop, and about 78,500 of those are judged
//     unlike their reference order or the oracle, or leave a run open.
//   - Two moves in one GitHub second whose synchronizes arrive reversed: the older is not late by
//     the clock, so it moves the record's head back, as on the release before, and the pull request
//     is judged from there. About 40,400 of 200,000 histories deliver two synchronizes of one
//     second reversed with no loop, and about 36,700 of those are judged unlike the oracle or leave
//     a run open. Three rules that read such a synchronize as late from what the record knew of its
//     heads each dropped a real move the release before applies; this floor is the release
//     before's. With no synchronize lost, about 108,500 of the 200,000 histories leave a head run
//     open to its bound, every one in this floor or the one before.
//   - A synchronize never delivered: the arrival after it waits for the move it names, judged for
//     now by the newest push of each two heads (provisional, the release before's), until the run's
//     bound cuts it off. About 169,300 to 170,300 of 200,000 histories with one lost leave a run
//     open to its bound.
//
// Outside those floors the rule is never wrong where the release before is right, by the
// fresh-commit oracle judged by this rule, in the gates' comparison (the undo and any generators,
// the reference, random and bounded orders, seeds 11, 20261028 and 4242, 40,000 histories each,
// synchronizes on time, late or never delivered, clocks a minute apart or in one second).
//
// A head whose push is lost for good is judged unchained and holds its head run open, and every
// head after it joins the run. The run drops exactly the pushes the record's own bound lets go and
// the repeat a move lets go past maxRepeats (ApplyPush), never one the record still holds, so each
// later head whose push arrives is judged with its chain, and the run closes at the step past its
// bound (maxRunSteps), which cuts the waiting head off (TestARunOneLostPushOpenedClosesAtItsBound).
func unspentPushes(pushes []record.ClassifiedPush, moves []record.Move, arrival bool) []record.ClassifiedPush {
	made := map[[2]string]bool{}
	for _, m := range moves {
		made[[2]string{m.Before, m.SHA}] = true
	}
	kept := make([]record.ClassifiedPush, 0, len(pushes))
	for _, p := range pushes {
		switch {
		case carriesApproval(p):
		case p.Before == "" && arrival, made[pushKey(p)]:
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// carriesApproval is whether an approval of the head a push replaced also approves the head it
// left: the push changed only the issue's own .legion/<issue>/ (HandoffPath), did not rewrite
// history, and said which head it replaced.
func carriesApproval(p record.ClassifiedPush) bool {
	return !p.MayChangeCode() && p.Before != ""
}

// ApplyPush records a push. A push that carries an approval across replaces an earlier record of
// the same two heads, so a redelivered handoff push - the webhook's, or the daemon's own read of
// GitHub's compare (daemon's handoffPush) - is recorded once; every other push is recorded in
// delivery order beside any earlier push of the same two heads, a delivery being applied once
// (intake.ApplyFact). A push whose changed paths are unknown takes those of a recorded push of the
// same two heads that knows them (knownPaths), so the webhook's copy of a push the daemon rebuilt
// with its whole list, arriving later cut short, changes nothing that list showed. A push whose
// own move the record has made is spent as it arrives (unspentPushes), the move keeping it as its
// own push or as a repeat (pushedMoves); and the pushes waiting for a link are bounded
// (boundedPushes). A push delivered late, after its head arrived and the branch moved on, is that
// move's: it is taken for the same two heads pushed again only by a head's next arrival by that
// move (arrivalPushes). While a head run is open - a head arrived before every push of its move
// was recorded - the push joins the run's pushes, the run drops what the record's bound let go, and
// the run is replayed (replayRun): the moves made and the CI settlement that stands become those
// every push arriving before every head would have given. With no run open no head waits for a
// push of its move - or the run that held one was cut to its bound (maxRunSteps) - and a push
// changes no judgement until a head arrives.
func ApplyPush(pr record.PullRequest, push record.ClassifiedPush) record.PullRequest {
	pr, _ = ApplyPushDropping(pr, push)
	return pr
}

// Dropped is what a push lets go (ApplyPushDropping): Waiting, the pushes waiting for a link past
// the bound (boundedPushes), oldest first; Repeat, the repeat a move let go past maxRepeats, if
// any; and Copy, a push of two heads on their way past maxCopies of them, if any (cappedCopies).
type Dropped struct {
	Waiting []record.ClassifiedPush
	Repeat  *record.ClassifiedPush
	Copy    *record.ClassifiedPush
}

// ApplyPushDropping is ApplyPush, and what it let go (Dropped).
func ApplyPushDropping(pr record.PullRequest, push record.ClassifiedPush) (record.PullRequest, Dropped) {
	push = knownPaths(pr.Pushes, push)
	var dropped Dropped
	pr.Moves, dropped.Repeat = pushedMoves(pr.Moves, push)
	unspent, copied := cappedCopies(unspentPushes(withPush(pr.Pushes, push), pr.Moves, false), push)
	dropped.Copy = copied
	var gone map[[2]string]bool
	pr.Pushes, gone = boundedPushes(unspent, pr.HeadSHA)
	dropped.Waiting = newestOfKeys(unspent, gone)
	if pr.Run == nil {
		return pr, dropped
	}
	run := *pr.Run
	run.Pushes = clone(run.Pushes)
	if !carriesApproval(push) {
		run.Pushes = append(run.Pushes, push)
	}
	run.Pushes, _ = cappedCopies(withoutKeys(run.Pushes, gone), push)
	if dropped.Repeat != nil {
		run.Pushes = withoutCopies(run.Pushes, []record.ClassifiedPush{*dropped.Repeat})
	}
	pr.Run = &run
	return replayRun(pr), dropped
}

// maxCopies bounds the code pushes of the same two heads a pull request keeps on their way, before
// a move of those heads is made: the one the move will take and the maxRepeats it will queue as
// repeats (madeMoves).
const maxCopies = maxRepeats + 1

// cappedCopies is pushes with at most maxCopies code pushes of push's two heads, and the one it let
// go, if any: past the bound the oldest after the first goes, as the move made of them would keep
// the first for its own push and let the oldest repeat go (pushedMoves).
func cappedCopies(pushes []record.ClassifiedPush, push record.ClassifiedPush) ([]record.ClassifiedPush, *record.ClassifiedPush) {
	if carriesApproval(push) {
		return pushes, nil
	}
	var at []int
	for i, p := range pushes {
		if pushKey(p) == pushKey(push) && !carriesApproval(p) {
			at = append(at, i)
		}
	}
	if len(at) <= maxCopies {
		return pushes, nil
	}
	gone := pushes[at[1]]
	return append(append(make([]record.ClassifiedPush, 0, len(pushes)-1), pushes[:at[1]]...), pushes[at[1]+1:]...), &gone
}

// knownPaths is push with the changed paths of a recorded push of the same two heads when push's
// own are unknown (Unknown: GitHub's limit cut the list, the push listed none, or the listener sent
// none) and the recorded push's are known and describe the two heads - it did not rewrite history,
// whose paths list only the commits it added. What a push changed between two commits is a fact
// about them, true whenever it is learned: the push webhook arriving after the daemon's per-issue
// read rebuilt the same push from GitHub's compare (daemon's handoffPush) must not unlearn a
// handoff-only push, which an approval and a CI settlement are carried across. Only the paths are
// taken; the rest of push is its own, so a forced copy may still change code, and a push whose
// paths are known is recorded as it came.
func knownPaths(pushes []record.ClassifiedPush, push record.ClassifiedPush) record.ClassifiedPush {
	if push.Unknown == "" {
		return push
	}
	for _, p := range pushes {
		if pushKey(p) == pushKey(push) && p.Unknown == "" && !p.Forced {
			push.HandoffOnly, push.Unknown = p.HandoffOnly, ""
			return push
		}
	}
	return push
}

// withPush is pushes with push recorded, after every earlier push. A push that carries an approval
// across replaces every earlier record of the same two heads, and any push replaces an earlier
// one of the same two heads that carries an approval: those are kept once, as the release before
// kept every push. Code pushes of the same two heads are each kept, in delivery order.
func withPush(pushes []record.ClassifiedPush, push record.ClassifiedPush) []record.ClassifiedPush {
	recorded := make([]record.ClassifiedPush, 0, len(pushes)+1)
	for _, p := range pushes {
		if pushKey(p) != pushKey(push) || (!carriesApproval(push) && !carriesApproval(p)) {
			recorded = append(recorded, p)
		}
	}
	return append(recorded, push)
}

// withoutCopies is pushes less gone, copy by copy: each push of gone takes away the oldest push of
// pushes equal to it that no earlier one took.
func withoutCopies(pushes, gone []record.ClassifiedPush) []record.ClassifiedPush {
	if len(gone) == 0 {
		return pushes
	}
	left := clone(gone)
	kept := make([]record.ClassifiedPush, 0, len(pushes))
	for _, p := range pushes {
		if k := indexOf(left, p); k >= 0 {
			left = append(left[:k:k], left[k+1:]...)
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// withoutKeys is pushes less every push of two heads gone holds.
func withoutKeys(pushes []record.ClassifiedPush, gone map[[2]string]bool) []record.ClassifiedPush {
	if len(gone) == 0 {
		return pushes
	}
	kept := make([]record.ClassifiedPush, 0, len(pushes))
	for _, p := range pushes {
		if !gone[pushKey(p)] {
			kept = append(kept, p)
		}
	}
	return kept
}

// newestOfKeys is, for each two heads keys holds, the newest push of pushes with them, in the
// order those newest pushes stand.
func newestOfKeys(pushes []record.ClassifiedPush, keys map[[2]string]bool) []record.ClassifiedPush {
	if len(keys) == 0 {
		return nil
	}
	var newest []record.ClassifiedPush
	for _, p := range newestOfEachMove(pushes) {
		if keys[pushKey(p)] {
			newest = append(newest, p)
		}
	}
	return newest
}

// maxWaitingPushes bounds the pushes a pull request keeps that wait for a link: a push that
// neither carries an approval across nor lies on the path forward from the head, and that no move
// spent (unspentPushes) - one of a line of commits the branch never takes, or one behind a webhook
// that never arrives. The bound counts the two heads of such pushes, each pair once however many
// of its pushes wait, as the release before kept one push of each two heads. Past the bound the
// pushes of the two heads whose newest push is oldest go, and a move they would have linked into is
// judged without them; an open head run lets the same pushes go (ApplyPush).
const maxWaitingPushes = 16

// boundedPushes is pushes with at most maxWaitingPushes pairs of heads of waiting pushes, and the
// pairs it let go, oldest first by their newest push.
func boundedPushes(pushes []record.ClassifiedPush, head string) ([]record.ClassifiedPush, map[[2]string]bool) {
	onPath := onPathFrom(pushes, head)
	newest := map[[2]string]int{}
	for i, p := range pushes {
		if !carriesApproval(p) && !onPath[pushKey(p)] {
			newest[pushKey(p)] = i
		}
	}
	if len(newest) <= maxWaitingPushes {
		return pushes, nil
	}
	gone := map[[2]string]bool{}
	for i, p := range pushes {
		if newest[pushKey(p)] == i && !carriesApproval(p) && !onPath[pushKey(p)] && len(newest)-len(gone) > maxWaitingPushes {
			gone[pushKey(p)] = true
		}
	}
	return withoutKeys(pushes, gone), gone
}

// ApplySettlement applies a listener CI settlement whenever ClassifySettlement says it is newer,
// merging its check runs into the fence per check name. It returns false for stale, duplicate
// and conflicting observations.
//
// Each run is stamped with the head it was reported on and the conclusion the candidate gives it
// (stampedRuns) before it is merged, since the fence keeps the newest run of each name over
// several heads and those facts are that run's. What the pull request then reports failing and
// cancelled follows from the fence: each name the fence holds a concluded run for comes to that
// run's own conclusion, and a name with no run anywhere keeps what the lists say of it
// (concludedBy over EffectiveOutcome). So a check re-run and passed is no longer failing however
// the older run ended, and one that failed on the head whose settlement arrived last is failing
// even when the settlement applied after it reported nothing of that name.
//
// The listener's generation and snapshot number the settlements of one head's own record, so they
// are taken only from a settlement of the head the fence stands at (SettlementFor, which moves
// that head to the candidate's whenever the candidate is the nearer one to the pull request's
// head). A settlement of a head below it merges its runs and the conclusions of them, and leaves
// the recorded head's numbering as it was.
func ApplySettlement(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	if ClassifySettlement(pr, candidate) != SettlementNewer {
		return pr, false
	}
	fence := mergeAttemptSets(pr.CheckRuns, stampedRuns(candidate))
	outcome := concludedBy(fence, EffectiveOutcome(pr, candidate))
	pr.CheckRuns = fence
	if candidate.Head == pr.CheckedHead {
		pr.Generation = candidate.Generation
		pr.Snapshot = candidate.Snapshot
	}
	pr.Failing = append([]string(nil), outcome.Failing...)
	pr.Cancelled = append([]string(nil), outcome.Cancelled...)
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

// RepeatedClose reports whether a close at observed repeats the close already applied to pr: pr is
// recorded closed and the close's clock is not after the recorded one, or the close has none. A
// later close is a second one, whose reopen between the two has not been delivered yet. Unlike
// LateLifecycle, an equal clock is the same event here: a second close in the same second as the
// first goes unreported only when the reopen between them was never observed.
func RepeatedClose(pr record.PullRequest, observed time.Time) bool {
	return pr.State == record.PullRequestClosed && !observed.After(pr.HeadUpdatedAt)
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

// pathFrom is the pushes that lie on the path forward from head: each that replaced head or the
// head a push already on the path left - also one that returns the branch to a head the path
// already passed, a push of its own whatever the head it leaves. With unplaced, a push that did
// not say which head it replaced is on the path too, the reading that never lets it pass unseen.
func pathFrom(pushes []record.ClassifiedPush, head string, unplaced bool) []record.ClassifiedPush {
	var path []record.ClassifiedPush
	reached, included := map[string]bool{head: true}, make([]bool, len(pushes))
	for grew := true; grew; {
		grew = false
		for i, p := range pushes {
			if !included[i] && ((unplaced && p.Before == "") || reached[p.Before]) {
				path = append(path, p)
				included[i], reached[p.SHA] = true, true
				grew = true
			}
		}
	}
	return path
}

// pushKey is a push's identity: the head it replaced and the head it left.
func pushKey(p record.ClassifiedPush) [2]string {
	return [2]string{p.Before, p.SHA}
}

// onPathFrom is the identities of the pushes on the path forward from head (pathFrom).
func onPathFrom(pushes []record.ClassifiedPush, head string) map[[2]string]bool {
	onPath := map[[2]string]bool{}
	for _, p := range pathFrom(pushes, head, false) {
		onPath[pushKey(p)] = true
	}
	return onPath
}

// ApprovalStands says whether an approval of reviewed approves the pull request's current head:
// walking back from the current head through pushes that carry an approval across
// (carriesApproval) reaches reviewed, so every push between them changed only .legion/. Nothing
// stands while a push that may change code is on its way (CodeOnItsWay): the push event can come
// first, and the approval is then of a head already on its way out.
func ApprovalStands(pr record.PullRequest, reviewed string) bool {
	if reviewed == "" || CodeOnItsWay(pr) {
		return false
	}
	return carriedBack(pr.Pushes, pr.HeadSHA, reviewed)
}

// ApprovalSetAside is the one spelling of the state an approval of reviewed is in when it neither
// stands at the pull request's head nor waits on anything the record already holds: it does not
// carry up to the head (ApprovalStands), and no push that may change code is on its way to
// replace that head (CodeOnItsWay), whose arrival would decide it. A recorded push carries an
// approval only from the head it replaced, so an approval is set aside when the webhook of the
// push above it was lost, still waits behind the GitHub consumer's backlog, or came from a
// listener that could not say what it changed - and GitHub's compare of the two heads is the only
// thing that settles it (daemon's readApprovalCarry, the outbox's approvalCarry, workflow's
// carryApproval and stuckApprovedOtherCode).
func ApprovalSetAside(pr record.PullRequest, reviewed string) bool {
	return reviewed != "" && !ApprovalStands(pr, reviewed) && !CodeOnItsWay(pr)
}

// CodeOnItsWay says whether a push that may change code lies on the path forward from the pull
// request's current head, its new head not arrived yet. A push delivered late, after its new head
// arrived and the branch returned to this one, is spent by the move the record made (unspentPushes),
// so it is not on its way; the same two heads pushed again are spent as its repeat
// (record.Move.Repeats), and look the same until their synchronize moves the head. The daemon
// lands no merge meanwhile: the merger merges, and it merges the head its review approved.
func CodeOnItsWay(pr record.PullRequest) bool {
	for _, p := range pathFrom(pr.Pushes, pr.HeadSHA, true) {
		if p.MayChangeCode() {
			return true
		}
	}
	return false
}

// carriedBack is whether earlier is head, or walking back from head through pushes that carry an
// approval across (carriesApproval) reaches it: every push between them changed only .legion/, so
// the two heads' code is the same.
func carriedBack(pushes []record.ClassifiedPush, head, earlier string) bool {
	return linkedBack(pushes, head, earlier, carriesApproval)
}

// linkedBack is whether earlier is head, or walking back from head through pushes link accepts,
// each from the head it left to the head it replaced, reaches it.
func linkedBack(pushes []record.ClassifiedPush, head, earlier string, link func(record.ClassifiedPush) bool) bool {
	if head == earlier {
		return true
	}
	reached := map[string]bool{head: true}
	for frontier := []string{head}; len(frontier) > 0; {
		current := frontier[0]
		frontier = frontier[1:]
		for _, p := range pushes {
			if p.SHA != current || !link(p) || reached[p.Before] {
				continue
			}
			if p.Before == earlier {
				return true
			}
			reached[p.Before] = true
			frontier = append(frontier, p.Before)
		}
	}
	return false
}

// HeadConflicts says whether an open pull request's head has started conflicting with its base:
// the daemon's last read of GitHub's mergeability found MergeabilityConflicting. The read stands
// for whatever head GitHub last computed it on, and the daemon's own read is the only path to it,
// so there is no older read to prefer. GitHub runs no checks at all on a conflicting head - there
// is no merge ref to run them against - and no CI result moves an issue in any case
// (dispatch://INFRA-714/spec R1), so a conflict is the one thing a pull request's own state still
// moves an issue for (workflow's mergeability handler).
func HeadConflicts(pr record.PullRequest) bool {
	return pr.State == record.PullRequestOpen && pr.Mergeability == record.MergeabilityConflicting
}

// DesignGateEventKind is the Dispatch artifact observation that changes a registered gate.
type DesignGateEventKind string

const (
	DesignGateApproved         DesignGateEventKind = "approved"
	DesignGateChangesRequested DesignGateEventKind = "changes_requested"
	DesignGateVersion          DesignGateEventKind = "version"
)

// ApplyDesignGateEvent updates a registered gate from one artifact observation, at mark: where
// Dispatch recorded the review, zero for a version event and for a read that gave none. Approval
// opens only when it names the latest known version, and the gate keeps that approval's mark; a
// newer document version and a changes request close it. A newer version keeps the approval it
// closes, with its mark, as the version a human approved below the latest; a changes request
// clears both. An approval also clears the changes request the architect was told of, which
// RecordChangesTold writes when it is told, so the approval and the told request never stand
// together on the gate. A newer version keeps the told record, at the version it was told at, so a
// late live event of that request is still recognised as told. An approval the gate already records
// at that version, read again (record.EventMark.Same), keeps the fuller of the two marks
// (record.EventMark.Merged), so a read that gave only its time never takes the sequence the live
// event gave.
func ApplyDesignGateEvent(gate record.DesignGate, kind DesignGateEventKind, version int, mark record.EventMark) record.DesignGate {
	if version > gate.LatestVersion {
		gate.LatestVersion = version
	}
	switch kind {
	case DesignGateApproved:
		if gate.ApprovedVersion != nil && *gate.ApprovedVersion == version && gate.ApprovedMark.Same(mark) {
			mark = mark.Merged(gate.ApprovedMark)
		}
		approved := version
		gate.ApprovedVersion, gate.ApprovedMark = &approved, mark
		gate = clearChangesTold(gate)
	case DesignGateChangesRequested:
		gate.ApprovedVersion, gate.ApprovedMark = nil, record.EventMark{}
	}
	return gate
}

func clearChangesTold(gate record.DesignGate) record.DesignGate {
	gate.ToldChangesVersion, gate.ToldChangesReason, gate.ToldChangesMark = nil, "", record.EventMark{}
	return gate
}

// LatestReview is the review the gate has applied last and the document version it reviewed: the
// approval it records, else the changes request it records as told. The two never stand together
// (ApplyDesignGateEvent), so whichever is set is the gate's newest applied review. ok is false
// when the gate records no review at all.
func LatestReview(gate record.DesignGate) (record.EventMark, int, bool) {
	switch {
	case gate.ApprovedVersion != nil:
		return gate.ApprovedMark, *gate.ApprovedVersion, true
	case gate.ToldChangesVersion != nil:
		return gate.ToldChangesMark, *gate.ToldChangesVersion, true
	}
	return record.EventMark{}, 0, false
}

// StaleReview reports whether a review of version at mark is older than the review the gate has
// already applied at that version: the person reviewed again after it, so this one changes no gate
// state. It covers an approval delivered after the changes request that superseded it, and a
// changes request delivered after a newer approval. A review Dispatch placed neither by sequence
// nor by time against the recorded one is not stale (record.EventMark.After), which keeps the
// earlier behaviour for marks a Dispatch did not give, and neither is the recorded review itself
// (record.EventMark.Same): a read that gave only its time and the live event that carries its
// sequence are one review, and the sequenced one completes what the gate records of it.
func StaleReview(gate record.DesignGate, version int, mark record.EventMark) bool {
	recorded, reviewed, ok := LatestReview(gate)
	return ok && reviewed == version && !mark.Same(recorded) && !mark.After(recorded)
}

// ReviewAfter reports whether the review the gate has applied at version was recorded after since.
// A registration whose read of Dispatch began at since is then older than that review: the person
// acted after the read, and the gate keeps what it already applied.
func ReviewAfter(gate record.DesignGate, version int, since record.EventMark) bool {
	recorded, reviewed, ok := LatestReview(gate)
	return ok && reviewed == version && recorded.After(since)
}

// DesignGateChangesTold reports whether the gate already records this changes request as one the
// architect was told of: the same document version, the same reason, and the same review in the
// issue's Dispatch event log (mark, record.EventMark.Same: its event's sequence, else Dispatch's
// time of it). A person who repeats a review's version and wording later has made a new review,
// with an event of its own, and is not this one. A read that carries neither the sequence nor the
// time is matched on version and reason alone.
func DesignGateChangesTold(gate record.DesignGate, version int, reason string, mark record.EventMark) bool {
	return gate.ToldChangesVersion != nil && *gate.ToldChangesVersion == version && gate.ToldChangesReason == reason &&
		gate.ToldChangesMark.Same(mark)
}

// RecordChangesTold records on the gate the changes request the architect is being told of, or has
// been, so a later read of the same review (a registration's or the gate seed's) tells it nothing
// again. A review the gate already records keeps its record, completed with what this read adds of
// its mark (record.EventMark.Merged), so the review's live event and a read that showed only its
// time are one review, recorded with both.
func RecordChangesTold(gate record.DesignGate, version int, reason string, mark record.EventMark) record.DesignGate {
	if DesignGateChangesTold(gate, version, reason, mark) {
		gate.ToldChangesMark = gate.ToldChangesMark.Merged(mark)
		return gate
	}
	told := version
	gate.ToldChangesVersion, gate.ToldChangesReason, gate.ToldChangesMark = &told, reason, mark
	return gate
}

// ApprovalFollows reports whether the gate records an approval given after a review of version at
// mark: an approval of a later version, or one of that version the review does not come after
// (record.EventMark.After). A changes request that such an approval followed was answered by the
// person's own approval, so nothing of it is left to tell.
func ApprovalFollows(gate record.DesignGate, version int, mark record.EventMark) bool {
	if gate.ApprovedVersion == nil {
		return false
	}
	approved := *gate.ApprovedVersion
	return approved > version || approved == version && !mark.After(gate.ApprovedMark)
}

// DesignGateOpen reports whether a human approved the gate's current document version.
func DesignGateOpen(gate record.DesignGate) bool {
	return gate.ApprovedVersion != nil && *gate.ApprovedVersion == gate.LatestVersion
}
