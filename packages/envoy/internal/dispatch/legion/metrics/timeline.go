package metrics

import (
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// Span is the half-open interval [From, To). Every rule here is written over spans rather than
// over clock hours: "each hour of a phase" is the hour as a measure, so a segment that holds for
// ten minutes claims ten minutes.
type Span struct {
	From time.Time
	To   time.Time
}

// Duration is the span's length, zero for an empty or inverted one.
func (s Span) Duration() time.Duration {
	if !s.To.After(s.From) {
		return 0
	}
	return s.To.Sub(s.From)
}

// The phases a timeline names. Twelve are the daemon's own, named once in internal/dispatch/legion
// (packages/daemon/internal/phase), and two are this package's reading of `admitted`, which the
// daemon does not split: an admitted issue holding a slot is its architect's design ("Design:
// roots that hold a slot while their phase is still `admitted`"), and an admitted issue holding
// none is the waiting line, whose time is on no bar ("Time in the queue is on no bar").
const (
	phaseAdmitted       = legion.PhaseAdmitted
	phaseAwaitingPerson = legion.PhaseAwaitingPerson
	phasePlanning       = legion.PhasePlanning
	phaseImplementing   = legion.PhaseImplementing
	phaseTesting        = legion.PhaseTesting
	phaseReviewing      = legion.PhaseReviewing
	phaseAccepting      = legion.PhaseAccepting
	phaseRetro          = legion.PhaseRetro
	phaseMerging        = legion.PhaseMerging
	phaseAwaitingMerge  = legion.PhaseAwaitingMerge
	phaseDone           = legion.PhaseDone
	phaseHeld           = legion.PhaseHeld

	// queued and design are this package's own split of `admitted`, which no daemon reports.
	phaseQueued = "queued"
	phaseDesign = "design"
)

// childWorkingPhases are the phases "from planning to awaiting merge": a root with a child in one
// of them is waiting on its children (segment 4).
var childWorkingPhases = []string{
	phasePlanning, phaseImplementing, phaseTesting, phaseReviewing, phaseAccepting,
	phaseRetro, phaseMerging, phaseAwaitingMerge,
}

// parkedStatuses are the statuses whose time the work time subtracts: the daemon writes `backlog`
// when it parks a root, and the gate treats both as parked (daemon workflow/gate.go).
var parkedStatuses = []string{"backlog", "icebox"}

// handoffWords is the handoff each phase ends with and the phase it therefore starts, as the
// daemon's phase.Handoff pairs them: the planner's `plan`, the implementer's `implement`, the
// tester's `test` and the reviewer's `review`. Retro, the merger's READY and the architect's
// sign-off write none.
var handoffWords = map[string]string{
	phasePlanning:     "plan",
	phaseImplementing: "implement",
	phaseTesting:      "test",
	phaseReviewing:    "review",
}

// PhaseSpan is one stretch of one phase.
type PhaseSpan struct {
	Phase string
	Span  Span
}

// Phases is the issue's phase timeline: the daemon's own where the sampler observed it, and the
// event log's fallback where it did not. The fallback is the spec's "Before observations exist,
// each role's phase starts at the first event of a session titled `Legion <role> · <KEY>` for the
// issue, or at the status or handoff that marks it, and ends where the next role's phase starts".
func (p *Population) Phases(issue *Issue) []PhaseSpan {
	if observed := p.observedPhases(issue); len(observed) > 0 {
		return observed
	}
	return p.fallbackPhases(issue)
}

// observedPhases is the timeline the sampler's `phase` observations draw, with each admitted
// stretch split into the design the issue held a slot through and the queue it did not.
func (p *Population) observedPhases(issue *Issue) []PhaseSpan {
	marks := issue.Observed[legion.FieldPhase]
	if len(marks) == 0 {
		return nil
	}
	end := p.issueEnd(issue)
	var timeline []PhaseSpan
	for index, mark := range marks {
		phase := legion.JSONString([]byte(mark.Value))
		if phase == "" {
			continue
		}
		span := Span{From: mark.At, To: end}
		if index+1 < len(marks) {
			span.To = marks[index+1].At
		}
		if span.Duration() == 0 {
			continue
		}
		if phase != phaseAdmitted {
			timeline = append(timeline, PhaseSpan{Phase: phase, Span: span})
			continue
		}
		held := intersect([]Span{span}, p.slotSpans(issue, end))
		for _, holding := range held {
			timeline = append(timeline, PhaseSpan{Phase: phaseDesign, Span: holding})
		}
		for _, waiting := range subtract([]Span{span}, held) {
			timeline = append(timeline, PhaseSpan{Phase: phaseQueued, Span: waiting})
		}
	}
	slices.SortFunc(timeline, func(a, b PhaseSpan) int { return a.Span.From.Compare(b.Span.From) })
	return timeline
}

// slotSpans are the stretches the issue held an admission slot, from its own `slot` observations:
// never from the daemon's `admission.active`, which dispatch://INFRA-980 widens to every issue
// holding one, children included.
func (p *Population) slotSpans(issue *Issue, end time.Time) []Span {
	return markSpans(issue.Observed[legion.FieldSlot], end, func(value string) bool {
		return value != "" && value != "null"
	})
}

// fallbackPhases is the timeline the event log draws for an issue the sampler never observed: one
// mark per role's phase, each phase ending where the next starts, and a handoff ending its own
// phase where it comes before that next start.
func (p *Population) fallbackPhases(issue *Issue) []PhaseSpan {
	root := p.isRoot(issue.Key)
	type mark struct {
		phase string
		at    time.Time
		order int
	}
	var marks []mark
	add := func(phase string, order int, at time.Time, ok bool) {
		if ok {
			marks = append(marks, mark{phase: phase, at: at, order: order})
		}
	}
	// A root's architect titles its session by the root's key, so only a root has a design
	// phase; a child's architect bar is its accepting phase alone.
	if root {
		at, ok := issue.FirstRoleEvent["architect"]
		add(phaseDesign, 0, at, ok)
	}
	at, ok := issue.FirstRoleEvent["planner"]
	add(phasePlanning, 1, at, ok)
	at, ok = p.firstOf(issue, "implementer", "plan", "in_progress")
	add(phaseImplementing, 2, at, ok)
	at, ok = p.firstOf(issue, "tester", "implement", "testing")
	add(phaseTesting, 3, at, ok)
	at, ok = p.firstOf(issue, "reviewer", "test", "needs_review")
	add(phaseReviewing, 4, at, ok)
	at, ok = handoffAt(issue, "review")
	add(phaseAccepting, 5, at, ok)
	at, ok = p.firstOf(issue, "merger", "", "retro")
	add(phaseRetro, 6, at, ok)

	slices.SortFunc(marks, func(a, b mark) int {
		if at := a.at.Compare(b.at); at != 0 {
			return at
		}
		return a.order - b.order
	})
	end := p.issueEnd(issue)
	var timeline []PhaseSpan
	for index, one := range marks {
		span := Span{From: one.at, To: end}
		if index+1 < len(marks) {
			span.To = marks[index+1].at
		}
		// A handoff ends its phase only where it comes before the next phase's start: the time
		// between a finished handoff and the next role's first event is on no bar.
		if word, written := handoffWords[one.phase]; written {
			if at, found := handoffAfter(issue, word, span.From); found && at.Before(span.To) {
				span.To = at
			}
		}
		if span.Duration() == 0 {
			continue
		}
		timeline = append(timeline, PhaseSpan{Phase: one.phase, Span: span})
	}
	return timeline
}

// firstOf is where a phase starts in the fallback: the first event of its role's session, else
// the handoff that marks it (the one the phase before it ends with), else the issue's first entry
// into the status the workflow sets with it.
func (p *Population) firstOf(issue *Issue, role, handoffWord, status string) (time.Time, bool) {
	if at, ok := issue.FirstRoleEvent[role]; ok {
		return at, true
	}
	if handoffWord != "" {
		if at, ok := handoffAt(issue, handoffWord); ok {
			return at, true
		}
	}
	return firstStatusEntry(issue, status)
}

// handoffAt is when the issue's first handoff of word was recorded.
func handoffAt(issue *Issue, word string) (time.Time, bool) {
	return handoffAfter(issue, word, time.Time{})
}

// handoffAfter is the issue's first handoff of word at or after from.
func handoffAfter(issue *Issue, word string, from time.Time) (time.Time, bool) {
	for _, handoff := range issue.Handoffs {
		if handoff.Phase == word && !handoff.At.Before(from) {
			return handoff.At, true
		}
	}
	return time.Time{}, false
}

// enteredStatus is whether the mark at index is where the issue's status became status: the mark
// holds it and the one before does not.
func enteredStatus(issue *Issue, index int, status string) bool {
	return issue.StatusMarks[index].Value == status &&
		(index == 0 || issue.StatusMarks[index-1].Value != status)
}

// firstStatusEntry is when the issue's status first became status.
func firstStatusEntry(issue *Issue, status string) (time.Time, bool) {
	for index, mark := range issue.StatusMarks {
		if enteredStatus(issue, index, status) {
			return mark.At, true
		}
	}
	return time.Time{}, false
}

// statusEntries counts how many times the issue's status became status.
func statusEntries(issue *Issue, status string) int {
	entries := 0
	for index := range issue.StatusMarks {
		if enteredStatus(issue, index, status) {
			entries++
		}
	}
	return entries
}

// parkedSpans are the stretches the issue sat in a parked status (backlog or icebox), from the
// statuses its own events snapshotted.
func (p *Population) parkedSpans(issue *Issue) []Span {
	return markSpans(issue.StatusMarks, p.issueEnd(issue), func(value string) bool {
		return slices.Contains(parkedStatuses, value)
	})
}

// queuedSpans is the time an issue spent back in the queue: "any time the issue's observed phase
// is `admitted` after it first left `admitted`". Before observations it is none, which is what
// this answers for an issue the sampler never saw, since the parked subtraction already covers
// the daemon's own parking.
func (p *Population) queuedSpans(issue *Issue) []Span {
	marks := issue.Observed[legion.FieldPhase]
	end := p.issueEnd(issue)
	var left time.Time
	var departed bool
	var spans []Span
	for index, mark := range marks {
		phase := legion.JSONString([]byte(mark.Value))
		if phase == "" {
			continue
		}
		if !departed {
			if phase != phaseAdmitted {
				left, departed = mark.At, true
			}
			continue
		}
		if phase != phaseAdmitted {
			continue
		}
		span := Span{From: mark.At, To: end}
		if index+1 < len(marks) {
			span.To = marks[index+1].At
		}
		if span.From.Before(left) {
			span.From = left
		}
		if span.Duration() > 0 {
			spans = append(spans, span)
		}
	}
	return normalize(spans)
}

// markSpans is the stretches in which a field's recorded value satisfies holds, from the marks the
// sampler wrote for it: each mark runs until the next, and the last until end.
func markSpans(marks []Mark, end time.Time, holds func(value string) bool) []Span {
	var spans []Span
	for index, mark := range marks {
		if !holds(mark.Value) {
			continue
		}
		span := Span{From: mark.At, To: end}
		if index+1 < len(marks) {
			span.To = marks[index+1].At
		}
		if span.Duration() > 0 {
			spans = append(spans, span)
		}
	}
	return normalize(spans)
}

// phaseSpansOf is every span of the timeline whose phase is one of phases.
func phaseSpansOf(timeline []PhaseSpan, phases ...string) []Span {
	var spans []Span
	for _, one := range timeline {
		if slices.Contains(phases, one.Phase) {
			spans = append(spans, one.Span)
		}
	}
	return normalize(spans)
}

// isNormalized is whether spans are already what normalize would return: every span positive and
// each one starting strictly after the one before ends. An empty set is not, so normalize keeps
// answering nil for it.
func isNormalized(spans []Span) bool {
	if len(spans) == 0 {
		return false
	}
	for index, span := range spans {
		if span.Duration() <= 0 {
			return false
		}
		if index > 0 && !span.From.After(spans[index-1].To) {
			return false
		}
	}
	return true
}

// normalize drops the empty spans, orders what is left and merges what overlaps, so the set
// algebra below needs no special cases. A set already in that shape — which is what every
// caller here hands it, since each one is itself the answer of a normalize — is returned as it
// is, rather than sorted and copied again.
func normalize(spans []Span) []Span {
	if isNormalized(spans) {
		return spans
	}
	kept := make([]Span, 0, len(spans))
	for _, span := range spans {
		if span.Duration() > 0 {
			kept = append(kept, span)
		}
	}
	slices.SortFunc(kept, func(a, b Span) int { return a.From.Compare(b.From) })
	merged := kept[:0]
	for _, span := range kept {
		if last := len(merged) - 1; last >= 0 && !span.From.After(merged[last].To) {
			if span.To.After(merged[last].To) {
				merged[last].To = span.To
			}
			continue
		}
		merged = append(merged, span)
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// intersect is the time both sets cover.
func intersect(left, right []Span) []Span {
	left, right = normalize(left), normalize(right)
	var shared []Span
	for _, one := range left {
		for _, other := range right {
			if !other.To.After(one.From) {
				continue
			}
			if !other.From.Before(one.To) {
				break
			}
			span := Span{From: one.From, To: one.To}
			if other.From.After(span.From) {
				span.From = other.From
			}
			if other.To.Before(span.To) {
				span.To = other.To
			}
			if span.Duration() > 0 {
				shared = append(shared, span)
			}
		}
	}
	return normalize(shared)
}

// subtract is the time left carries that right does not.
func subtract(left, right []Span) []Span {
	left, right = normalize(left), normalize(right)
	var rest []Span
	for _, one := range left {
		from := one.From
		for _, other := range right {
			if !other.To.After(from) {
				continue
			}
			if !other.From.Before(one.To) {
				break
			}
			if other.From.After(from) {
				rest = append(rest, Span{From: from, To: other.From})
			}
			if other.To.After(from) {
				from = other.To
			}
		}
		if one.To.After(from) {
			rest = append(rest, Span{From: from, To: one.To})
		}
	}
	return normalize(rest)
}

// totalOf is how long a set of spans covers.
func totalOf(spans []Span) time.Duration {
	var total time.Duration
	for _, span := range normalize(spans) {
		total += span.Duration()
	}
	return total
}
