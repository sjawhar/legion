package classify

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// AdvancePullRequestHead applies the shipped resetPrHead fix-attempt decision to a new observed
// head. A red prior verdict counts once, unless the red was planned (PlannedRed: the review App's
// red tests) or the recorded pushes show the move spares the count (sparedMove): a chain of pushes
// from the recorded head to the new one, each the review App's or an unforced handoff-only push
// (sparesAttempt). The recorded CI settlement stays, as CheckedHead's: it is the new head's too
// once every push between them is known to change only .legion/ (HeadVerdict), and a settlement
// for another head replaces it (SettlementFor). The pushes it keeps are keptPushes'.
//
// The move's planned mark is its last push that may change code (arrivalPlan): the review App's
// sets it, anyone else's clears it, and a move of handoffs alone carries it. The head records the
// head it replaced (HeadArrivedFrom), so a chain whose pushes arrive after the head settles the
// mark when it completes (ApplyPush), as it would have had they arrived first. Until a chain is
// recorded, any push that left the head and may change code decides (headPush). The pushes it keeps
// are keptPushes': a push that may be a link of a chain still to complete stays.
//
// HeadCounted names the head the count last counted, and the attempt it marks runs on through every
// head pushed on top of it until one is pushed onto a green verdict, or onto a settled head whose
// base branch requires no check and no workflow (nothingRequired), which clears it: CI sent nothing
// back from either, so the push is a fresh start. A push onto a head CI has not judged yet, or onto
// a red that counts nothing, is the same attempt, not a new one. Only a counted head is ever named, so
// ApplyPush's take-back never undoes a count it did not make. A count keeps the mark it replaces
// (HeadCountedBefore), which the take-back restores, and the head it moved from (HeadCountedFrom),
// where the chain the take-back looks for starts: a head whose pushes have not all arrived is
// counted, and when the pushes that arrive later complete a chain that spares the move, the attempt
// is left as it would have been had they arrived first.
//
// The count and the planned mark of a move are the same in every order of its pushes and its
// arrival while the move's head is still the recorded head when its last push arrives - also for a
// head the daemon reached past heads it never recorded (a read of GitHub's head, a reopen). A head
// that arrives on top of it first ends that: h2 red, the review App's red tests h2..h3 and a
// handoff h3..h4, both synchronizes first and both pushes late, counts h3 and leaves h4 unplanned,
// while the pushes first count nothing and plan h4. Judging h4's arrival reads h3's verdict, which
// depends on h3's pushes, so only replaying the arrivals and settlements since the last head whose
// chain was known would make that order agree. The code before the chain rule had the same gap.
func AdvancePullRequestHead(pr record.PullRequest, headSHA string) record.PullRequest {
	planned, decided, chained := arrivalPlan(pr.Pushes, pr.HeadSHA, headSHA)
	if pending := headPush(pr.Pushes, pr.HeadSHA, headSHA); !chained && pending != nil && pending.MayChangeCode() {
		planned, decided = pending.ByReviewApp, true
	}
	spared := sparedMove(pr.Pushes, pr.HeadSHA, headSHA)
	pr.Pushes = keptPushes(pr.Pushes, pr.HeadSHA, headSHA, chained)
	switch verdict := HeadVerdict(pr); {
	case verdict == "red" && !pr.PlannedRed && !spared:
		pr.FixAttempts++
		pr.HeadCountedBefore, pr.HeadCounted, pr.HeadCountedFrom = pr.HeadCounted, headSHA, pr.HeadSHA
	case verdict == "green" || nothingRequired(pr):
		pr.HeadCounted, pr.HeadCountedBefore, pr.HeadCountedFrom = "", "", ""
	}
	if decided {
		pr.PlannedRed = planned
	}
	pr.HeadArrivedFrom, pr.HeadSHA = pr.HeadSHA, headSHA
	return pr
}

// settled says whether a CI settlement stands for the pull request's current head: one recorded
// for the head itself, or for a head the current one replaced through pushes that each changed
// only .legion/ and said which head they replaced (a handoff push can carry GitHub's skip-checks
// trailer and start no CI of its own). A checked head is recorded only with a settlement
// (SettlementFor, then ApplySettlement), and cleared with it (a new generation, migration 0028).
func settled(pr record.PullRequest) bool {
	return pr.CheckedHead != "" && carriedBack(pr.Pushes, pr.HeadSHA, pr.CheckedHead)
}

// SettlementFor says whether a CI settlement of candidate.Head may stand for the pull request's
// current head, and returns the pull request ready to apply it. It may when head is the current
// head, or a head the current one replaced through pushes that each changed only .legion/, unless
// a settlement already stands for the current head from a head nearer it on that path, which
// outranks it.
// Every other settlement - an earlier code head's, a head a force push left - stands for nothing.
// A settlement of a head other than the recorded one starts that head's fence afresh, since
// check runs, generations and snapshots are each head's own.
func SettlementFor(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	head := candidate.Head
	if !carriedBack(pr.Pushes, pr.HeadSHA, head) {
		return pr, false
	}
	if head == pr.CheckedHead {
		return pr, true
	}
	if head != pr.HeadSHA && settled(pr) && carriedBack(pr.Pushes, pr.CheckedHead, head) {
		return pr, false
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

// keptPushes is what a head's arrival from previous keeps: every push that changed only .legion/,
// did not rewrite history and said which head it replaced - a fact about two commits, true
// whenever it is learned, which an approval is carried across - the pushes still on their way from
// the new head, and every other push that said which head it replaced and is not yet spent: it may
// be a link of a chain still to complete (ApplyPush), or of a later move whose first link is late.
// A push is spent once it lies behind the head the new one replaced - reached walking back from it
// through recorded pushes - since the moves it made are judged, and, once a chain of recorded
// pushes leads from that head to the new one (chained), once it lies behind the new head. Until
// then a push behind the new head is a link whose chain is still to complete. A push that did not
// say which head it replaced is never a link, and is not kept.
func keptPushes(pushes []record.ClassifiedPush, previous, head string, chained bool) []record.ClassifiedPush {
	onPath := map[string]bool{}
	for _, p := range pathFrom(pushes, head, false) {
		onPath[p.SHA] = true
	}
	from := []string{previous}
	if chained {
		from = append(from, head)
	}
	reached := map[string]bool{}
	for _, h := range from {
		reached[h] = true
	}
	spent := map[int]bool{}
	for frontier := from; len(frontier) > 0; {
		current := frontier[0]
		frontier = frontier[1:]
		for i, p := range pushes {
			if p.SHA != current || p.Before == "" || spent[i] {
				continue
			}
			spent[i] = true
			if !reached[p.Before] {
				reached[p.Before] = true
				frontier = append(frontier, p.Before)
			}
		}
	}
	var kept []record.ClassifiedPush
	for i, p := range pushes {
		if carriesApproval(p) || onPath[p.SHA] || (p.Before != "" && !spent[i]) {
			kept = append(kept, p)
		}
	}
	return kept
}

// carriesApproval is whether an approval of the head a push replaced also approves the head it
// left: the push changed only the issue's own .legion/<issue>/ (HandoffPath), did not rewrite
// history, and said which head it replaced.
func carriesApproval(p record.ClassifiedPush) bool {
	return !p.MayChangeCode() && p.Before != ""
}

// sparesAttempt is whether a push onto a red is no fix attempt: it said which head it replaced,
// and it was the review App's (its red tests, a red it plans) - which who pushed it says, forced
// or not - or it changed only the issue's own handoffs without rewriting history (a forced push's
// paths do not describe what it did to that head, so it may change code).
func sparesAttempt(p record.ClassifiedPush) bool {
	return p.Before != "" && (p.ByReviewApp || (p.HandoffOnly && !p.Forced))
}

// sparedMove is whether the recorded pushes show the move from from to to is no fix attempt: a
// chain of pushes leads from from to to, each sparing the attempt (sparesAttempt). Each such push is
// a fact about two commits, true whenever it is learned, so once every push of the move is recorded
// the answer does not depend on the order the pushes arrived in, or on whether the daemon recorded
// the heads between from and to (AdvancePullRequestHead names the order it does not cover).
func sparedMove(pushes []record.ClassifiedPush, from, to string) bool {
	return from != "" && from != to && linkedBack(pushes, to, from, sparesAttempt)
}

// arrivalPlan reads the move from from to to off a chain of recorded pushes between them, each of
// which said which head it replaced. chained is whether such a chain leads from from to to;
// decided is whether a push on it may change code (MayChangeCode); planned is whether the last such
// push - the one nearest to - was the review App's. A move of handoffs alone decides nothing, and
// the planned mark is carried across it.
func arrivalPlan(pushes []record.ClassifiedPush, from, to string) (planned, decided, chained bool) {
	if from == "" || from == to {
		return false, false, false
	}
	type reach struct {
		head             string
		decided, planned bool
	}
	reached := map[string]bool{to: true}
	for frontier := []reach{{head: to}}; len(frontier) > 0; {
		current := frontier[0]
		frontier = frontier[1:]
		for _, p := range pushes {
			if p.SHA != current.head || p.Before == "" || reached[p.Before] {
				continue
			}
			next := reach{head: p.Before, decided: current.decided, planned: current.planned}
			if !next.decided && p.MayChangeCode() {
				next.decided, next.planned = true, p.ByReviewApp
			}
			if p.Before == from {
				return next.planned, next.decided, true
			}
			reached[p.Before] = true
			frontier = append(frontier, next)
		}
	}
	return false, false, false
}

// ApplyPush records a push and settles the current head's planned mark: once the recorded pushes
// chain from the head it replaced (HeadArrivedFrom) to it, by that chain's last push that may
// change code (arrivalPlan); until then, by the head's own push when it may change code (the
// review App's sets it, anyone else's clears it). The push completing the chain can name a head
// before the current one, so the mark is the same whichever of the pushes and the head arrives
// first, while the head is still the recorded head (AdvancePullRequestHead).
//
// It then takes back exactly the counted fix attempt of the current head once the recorded pushes
// show the counted move - from HeadCountedFrom to HeadCounted - spares it (sparedMove), restoring
// the counted attempt's mark that count replaced (AdvancePullRequestHead). The push that completes
// that chain can be the counted head's own or any push before it on the chain, so it can name a
// head other than the current one. A push that reached the counted head from another head - the
// required-checks pass's compare from an approved head - completes no chain from HeadCountedFrom,
// and takes nothing back: the counted move can carry code that push does not. A push replaces an
// earlier record of the same two heads, so a redelivered push is recorded once. The pushes it
// keeps are boundedPushes'.
func ApplyPush(pr record.PullRequest, push record.ClassifiedPush) record.PullRequest {
	pushes := make([]record.ClassifiedPush, 0, len(pr.Pushes)+1)
	for _, p := range pr.Pushes {
		if p.SHA != push.SHA || p.Before != push.Before {
			pushes = append(pushes, p)
		}
	}
	pr.Pushes = boundedPushes(append(pushes, push), pr.HeadSHA)
	if planned, decided, chained := arrivalPlan(pr.Pushes, pr.HeadArrivedFrom, pr.HeadSHA); chained {
		if decided {
			pr.PlannedRed = planned
		}
	} else if pr.HeadSHA == push.SHA && push.MayChangeCode() {
		pr.PlannedRed = push.ByReviewApp
	}
	if pr.HeadCounted != "" && pr.HeadCounted == pr.HeadSHA && sparedMove(pr.Pushes, pr.HeadCountedFrom, pr.HeadCounted) {
		if pr.BlockedAttempts == pr.FixAttempts {
			pr.BlockedAttempts = 0
		}
		pr.FixAttempts--
		pr.HeadCounted, pr.HeadCountedBefore, pr.HeadCountedFrom = pr.HeadCountedBefore, "", ""
	}
	return pr
}

// maxWaitingPushes bounds the pushes a pull request keeps that wait for a link (keptPushes): a push
// that neither carries an approval across nor lies on the path forward from the head. A push behind
// a webhook that never arrives is spent by no head's arrival, since the walk back from each head
// stops at the gap; past the bound the oldest such push goes, and the move it would have linked
// into is judged without it.
const maxWaitingPushes = 16

// boundedPushes is pushes with at most maxWaitingPushes waiting pushes, the oldest dropped first.
func boundedPushes(pushes []record.ClassifiedPush, head string) []record.ClassifiedPush {
	onPath := map[string]bool{}
	for _, p := range pathFrom(pushes, head, false) {
		onPath[p.SHA] = true
	}
	waiting := 0
	for _, p := range pushes {
		if !carriesApproval(p) && !onPath[p.SHA] {
			waiting++
		}
	}
	if waiting <= maxWaitingPushes {
		return pushes
	}
	kept := make([]record.ClassifiedPush, 0, len(pushes))
	for _, p := range pushes {
		if waiting > maxWaitingPushes && !carriesApproval(p) && !onPath[p.SHA] {
			waiting--
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// DroppedPushes is the pushes before holds that after does not: what a fact let go of.
func DroppedPushes(before, after []record.ClassifiedPush) []record.ClassifiedPush {
	held := map[[2]string]bool{}
	for _, p := range after {
		held[[2]string{p.Before, p.SHA}] = true
	}
	var dropped []record.ClassifiedPush
	for _, p := range before {
		if !held[[2]string{p.Before, p.SHA}] {
			dropped = append(dropped, p)
		}
	}
	return dropped
}

// ApplySettlement applies a listener CI settlement only when ClassifySettlement says it is newer,
// merging its attempt set into the fence. It returns false for stale, duplicate, and conflicting
// observations.
func ApplySettlement(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	if ClassifySettlement(pr, candidate) != SettlementNewer {
		return pr, false
	}
	outcome := EffectiveOutcome(pr, candidate)
	pr.CheckRuns = mergeAttemptSets(pr.CheckRuns, candidate.CheckRuns)
	pr.Generation = candidate.Generation
	pr.Snapshot = candidate.Snapshot
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
// head a push already on the path left. With unplaced, a push that did not say which head it
// replaced is on the path too, the reading that never lets it pass unseen.
func pathFrom(pushes []record.ClassifiedPush, head string, unplaced bool) []record.ClassifiedPush {
	var path []record.ClassifiedPush
	reached := map[string]bool{head: true}
	for grew := true; grew; {
		grew = false
		for _, p := range pushes {
			if !reached[p.SHA] && ((unplaced && p.Before == "") || reached[p.Before]) {
				path = append(path, p)
				reached[p.SHA] = true
				grew = true
			}
		}
	}
	return path
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

// CodeOnItsWay says whether a push that may change code lies on the path forward from the pull
// request's current head, its new head not arrived yet. That includes a push delivered late, after
// its new head arrived and the branch returned to this one: nothing tells it from the same two
// heads pushed again, so it counts until the next head arrives, by design. In Legion's flow one
// always follows an approval (the reviewer's handoff push).
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

// RedSendsBack says whether the verdict that stands for the pull request's head (HeadVerdict) sends
// an issue in testing, reviewing, retro or merging back to implementing: it is red, and the review
// App did not plan it (PlannedRed, the tester's own failing tests, which the implementer is meant
// to make pass). HeadVerdict carries a code commit's settlement to the handoff heads above it
// (SettlementFor, carriedBack): a head a handoff-only push reached changed only .legion/, so its
// code is the head before it, and a red standing for it is that code's red, whether the code head's
// own or the handoff head's second run of it. Such a head is no exception, and neither is a red a
// required workflow alone makes: CI is the daemon's own rule, read beside the review round and
// never by it (workflow's decideChecks), so the reviewer's approval and a red at its head are two
// facts, each acted on as it arrives. The fix-attempt cap is the caller's (FixAttemptCapHolds).
func RedSendsBack(pr record.PullRequest) bool {
	return HeadVerdict(pr) == "red" && !pr.PlannedRed
}

// RedWithdrawsReady says whether the verdict that stands for the pull request's head is red against
// a READY, in awaiting_merge: the pull request is open, and CI is red at the head by its own
// settlement and its own workflow runs (HeadVerdict, with the settlement of the head itself). The
// daemon has submitted the merge of that head, so what takes the submission back is a red of the
// head's own - a required check failing on a rerun, a newly required one, a required workflow
// re-run red - which the implementer's next push replaces, and the submission names the commit
// about to be replaced: Legion withdraws its own request, rather than refusing a merge the
// repository's rules allow. A red carried to the head from the one a .legion/-only push replaced
// (SettlementFor) stood before the READY and says nothing new about that commit, so it sends
// nothing back. As with RedSendsBack
// before READY, a head a .legion/-only push reached is no exception: every worker is suspended in
// awaiting_merge, and only the implementer can change the red. PlannedRed is a red the tester's own
// tests planned for a review round the approval and the acceptance have ended since, so it is not
// read here. The
// caller decides when such a red is read: a red only required workflows make (RedOnlyByWorkflows)
// withdraws the READY on a read of their runs, never on a CI settlement, which reads no run and
// would judge them by a read up to one pass old (workflow's decideChecks).
func RedWithdrawsReady(pr record.PullRequest) bool {
	return pr.State == record.PullRequestOpen && pr.CheckedHead == pr.HeadSHA && HeadVerdict(pr) == "red"
}

// ConflictWithdrawsReady says whether a READY pull request, in awaiting_merge, has started
// conflicting with its base: the pull request is open, and the daemon's last read of GitHub's
// mergeability found MergeabilityConflicting. Unlike RedWithdrawsReady, a conflict is not a
// settlement's to carry across a handoff-only push (SettlementFor) or a read ever to go stale on:
// Mergeability stands for whatever head GitHub last computed it on, and the daemon's own read is
// the only path to it, so there is no older read to prefer. GitHub runs no checks at all on a
// conflicting head - there is no merge ref to run them against - so no verdict of HeadVerdict's
// ever stands beside this one; the two are checked independently (workflow's decideChecks and its
// own mergeability handler).
func ConflictWithdrawsReady(pr record.PullRequest) bool {
	return pr.State == record.PullRequestOpen && pr.Mergeability == record.MergeabilityConflicting
}

// FixAttemptCapHolds says whether the red verdict standing for the head is one the fix-attempt cap
// holds: the pull request's fix attempts are at cap, and the head belongs to the attempt the count
// last counted (HeadCounted): the counted head itself, or a head pushed on top of it before any
// push landed on a green verdict or on a settled head whose base branch requires nothing
// (AdvancePullRequestHead). That attempt is the loop the cap bounds, so its red no longer sends the
// work back from testing, reviewing, retro or merging by itself, however often it returns - a
// re-run failing again, a red after the head read green, a second push before CI settled - and the
// architect decides what follows (workflow's standingRed). A head pushed onto a green verdict, or
// onto a settled head whose base requires nothing, is not that loop: it raised no count and left
// the attempt, so its red is new. A head a handoff-only push left is in the attempt of the code
// head it replaced, as its red is that head's (RedSendsBack). A green head is the fix that worked.
func FixAttemptCapHolds(pr record.PullRequest, cap int) bool {
	return cap > 0 && pr.FixAttempts >= cap && pr.HeadCounted != "" && HeadVerdict(pr) == "red"
}

// BlockFixAttempt marks and reports one exhausted fix-attempt count when the verdict that stands
// for the head is red after a settlement or a new required set, as the shipped reducer decides
// pr-blocked on ci-settled-red alone: once per count, never again on a later red at the same count.
// A green head at an exhausted count is the fix that worked. A zero BlockedAttempts means no count
// has been reported, because fix attempts begin at one before they can exhaust a positive cap. The
// caller asks it only of a red that sent nothing back (workflow's decideChecks).
func BlockFixAttempt(pr record.PullRequest, cap int) (record.PullRequest, bool) {
	if cap <= 0 || HeadVerdict(pr) != "red" || pr.FixAttempts < cap || pr.BlockedAttempts == pr.FixAttempts {
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
