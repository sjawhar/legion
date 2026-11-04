package metrics

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// Segment names one way an hour of a phase was spent.
type Segment string

const (
	SegmentWaitingOnPerson   Segment = "waiting_on_person"
	SegmentInATurn           Segment = "in_a_turn"
	SegmentCIWait            Segment = "ci_wait"
	SegmentWaitingOnChildren Segment = "waiting_on_children"
	SegmentIdle              Segment = "idle"
)

// segmentRule is one row of the precedence: a segment and every span of the issue's life in which
// it holds.
type segmentRule struct {
	Segment Segment
	Spans   func(view *issueView) []Span
}

// precedence is the order of the five segments, and the only place it is written (the spec's
// "Rule details — Precedence": "The order the phase-time section gives is exact: waiting on a
// person, in a turn, CI wait, waiting on children, idle. Each hour goes to the first that
// holds."). Each rule is one function below; idle is no rule, it is what no rule claimed.
var precedence = []segmentRule{
	{SegmentWaitingOnPerson, waitingOnPersonSpans},
	{SegmentInATurn, inATurnSpans},
	{SegmentCIWait, ciWaitSpans},
	{SegmentWaitingOnChildren, waitingOnChildrenSpans},
}

// Segments is one bar's hours, one field per segment, in the precedence's order.
type Segments struct {
	WaitingOnPerson   float64 `json:"waiting_on_person"`
	InATurn           float64 `json:"in_a_turn"`
	CIWait            float64 `json:"ci_wait"`
	WaitingOnChildren float64 `json:"waiting_on_children"`
	Idle              float64 `json:"idle"`
}

// add books a segment's time onto the bar.
func (s *Segments) add(segment Segment, spent time.Duration) {
	switch segment {
	case SegmentWaitingOnPerson:
		s.WaitingOnPerson += hours(spent)
	case SegmentInATurn:
		s.InATurn += hours(spent)
	case SegmentCIWait:
		s.CIWait += hours(spent)
	case SegmentWaitingOnChildren:
		s.WaitingOnChildren += hours(spent)
	case SegmentIdle:
		s.Idle += hours(spent)
	}
}

// total is every segment's hours.
func (s Segments) total() float64 {
	return s.WaitingOnPerson + s.InATurn + s.CIWait + s.WaitingOnChildren + s.Idle
}

// addSegments adds every segment of other.
func (s *Segments) addSegments(other Segments) {
	s.WaitingOnPerson += other.WaitingOnPerson
	s.InATurn += other.InATurn
	s.CIWait += other.CIWait
	s.WaitingOnChildren += other.WaitingOnChildren
	s.Idle += other.Idle
}

// issueView is one issue with everything its segments read: its tree's root and children, its own
// phase timeline and theirs, and the spans each rule claims, computed once.
type issueView struct {
	population *Population
	issue      *Issue
	root       *Issue
	children   []*Issue
	timeline   []PhaseSpan
	childPhase map[string][]PhaseSpan
	end        time.Time

	claimed []([]Span)
}

// split shares a phase's spans between the segments, each span's time going to the first rule of
// the precedence that holds through it and the rest to idle.
func (v *issueView) split(spans []Span) Segments {
	var segments Segments
	rest := normalize(spans)
	for index, rule := range precedence {
		if len(rest) == 0 {
			break
		}
		taken := intersect(rest, v.claims(index))
		segments.add(rule.Segment, totalOf(taken))
		rest = subtract(rest, taken)
	}
	segments.add(SegmentIdle, totalOf(rest))
	return segments
}

// claims is the spans the rule at index holds through, computed once per issue: a bar asks for
// them once per phase stretch, and a rule that reads the whole tree is not worth recomputing.
func (v *issueView) claims(index int) []Span {
	if v.claimed == nil {
		v.claimed = make([]([]Span), len(precedence))
	}
	if v.claimed[index] == nil {
		v.claimed[index] = normalize(precedence[index].Spans(v))
		if v.claimed[index] == nil {
			v.claimed[index] = []Span{}
		}
	}
	return v.claimed[index]
}

// waitingOnPersonSpans is segment 1: "the daemon recorded the root in `awaiting_person`, or an ask
// a session of the tree opened was on the person's turn (`ask.opened` or `ask.handed_back`, until
// `ask.answered`, `ask.resolved`, or `waitingOn.answeredAt`)".
func waitingOnPersonSpans(view *issueView) []Span {
	spans := markSpans(view.root.Observed[legion.FieldPhase], view.population.issueEnd(view.root), func(value string) bool {
		return legion.JSONString([]byte(value)) == phaseAwaitingPerson
	})
	answered := answeredAts(view.root)
	for _, issue := range view.tree() {
		for _, turn := range issue.AskTurns {
			if at, found := answeredWithin(answered, turn); found {
				turn.To = at
			}
			spans = append(spans, turn)
		}
	}
	return spans
}

// answeredWithin is the first of answered strictly inside turn, which is where that turn ends.
func answeredWithin(answered []time.Time, turn Span) (time.Time, bool) {
	for _, at := range answered {
		if at.After(turn.From) && at.Before(turn.To) {
			return at, true
		}
	}
	return time.Time{}, false
}

// inATurnSpans is segment 2: "a role of the issue had claim state `working`".
func inATurnSpans(view *issueView) []Span {
	var spans []Span
	for field, marks := range view.issue.Observed {
		if !strings.HasPrefix(field, legion.ClaimFieldPrefix) {
			continue
		}
		spans = append(spans, markSpans(marks, view.end, func(value string) bool {
			return legion.JSONString([]byte(value)) == "working"
		})...)
	}
	return spans
}

// ciWaitSpans is segment 3: "the pull request was open, its base branch requires a check, and the
// daemon had no CI verdict for its head (`checksVerdict` empty)". Each of the three is read from
// the issue's own observations, and a field no read ever carried holds through nothing: a daemon
// that sends no `requiresChecks` leaves this segment empty rather than guessing a check.
func ciWaitSpans(view *issueView) []Span {
	issue, end := view.issue, view.end
	open := markSpans(issue.Observed[legion.FieldPullRequestState], end, func(value string) bool {
		return legion.JSONString([]byte(value)) == "open"
	})
	required := markSpans(issue.Observed[legion.FieldPullRequestRequiresChecks], end, requiresChecks)
	judged := markSpans(issue.Observed[legion.FieldPullRequestChecksVerdict], end, func(value string) bool {
		return legion.JSONString([]byte(value)) != ""
	})
	return subtract(intersect(open, required), judged)
}

// waitingOnChildrenSpans is segment 4: "the issue is a root with a child in a phase from planning
// to awaiting merge".
func waitingOnChildrenSpans(view *issueView) []Span {
	if view.root.Key != view.issue.Key {
		return nil
	}
	var spans []Span
	for _, child := range view.children {
		spans = append(spans, phaseSpansOf(view.childPhase[child.Key], childWorkingPhases...)...)
	}
	return spans
}

// tree is the issue's root and every child of it, which is whose asks stand for the whole tree.
func (v *issueView) tree() []*Issue {
	tree := make([]*Issue, 0, len(v.children)+1)
	tree = append(tree, v.root)
	// No child is its own root: linkTrees records a key under its root only where the two differ.
	tree = append(tree, v.children...)
	if !slices.ContainsFunc(tree, func(one *Issue) bool { return one.Key == v.issue.Key }) {
		tree = append(tree, v.issue)
	}
	return tree
}

// requiresChecks reads the daemon's `requiresChecks` without assuming its shape: no daemon
// released today carries the field, and the one that will may send a boolean, a list of contexts
// or an object. Anything that is not absent, null, false, zero or empty says the base branch
// requires a check.
func requiresChecks(value string) bool {
	switch strings.TrimSpace(value) {
	case "", "null", "false", "0", `""`, "[]", "{}":
		return false
	}
	return true
}

// answeredAts are the moments a root's recorded wait named as answered, from the `waitingOn` the
// daemon sent, read for that one field and no other.
func answeredAts(root *Issue) []time.Time {
	var answered []time.Time
	for _, mark := range root.Observed[legion.FieldWaitingOn] {
		var wait struct {
			AnsweredAt string `json:"answeredAt"`
		}
		if err := json.Unmarshal([]byte(mark.Value), &wait); err != nil || wait.AnsweredAt == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, wait.AnsweredAt)
		if err != nil {
			continue
		}
		answered = append(answered, at)
	}
	slices.SortFunc(answered, func(a, b time.Time) int { return a.Compare(b) })
	return answered
}
