package metrics

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// Scope is which of a project's Legion issues an answer counts, the Legion section's own toggle.
type Scope string

const (
	ScopeAll      Scope = "all"
	ScopeRoots    Scope = "roots"
	ScopeChildren Scope = "children"
)

// ParseScope reads the scope parameter, defaulting to all.
func ParseScope(raw string) (Scope, error) {
	switch Scope(raw) {
	case "", ScopeAll:
		return ScopeAll, nil
	case ScopeRoots:
		return ScopeRoots, nil
	case ScopeChildren:
		return ScopeChildren, nil
	}
	return "", fmt.Errorf("scope must be one of all, roots or children")
}

// The bars of metric 2: the six roles, each holding the phases that role works, and the seventh
// for the phases no role works. "A seventh bar, 'no role', holds the phases no role works:
// `awaiting_person` and `held`", and the spec's "Phase boundaries before observations" holds it
// to those two alone — a handoff starts the next phase, so no hour falls between two phases for
// it to hold. Time in the queue is on no bar, and neither is an hour the work time leaves out,
// so the seven bars of an issue add up to its work time (phaseTime).
const barNoRole = "no_role"

// bars are the seven stacked bars in the order the card draws them.
var bars = []struct {
	Role   string
	Phases []string
}{
	{"architect", []string{phaseDesign, phaseAccepting}},
	{"planner", []string{phasePlanning}},
	{"implementer", []string{phaseImplementing}},
	{"tester", []string{phaseTesting}},
	{"reviewer", []string{phaseReviewing}},
	{"merger", []string{phaseRetro, phaseMerging, phaseAwaitingMerge}},
	{barNoRole, []string{phaseAwaitingPerson, phaseHeld}},
}

// The four classes of issue a Legion session filed (metric 4).
const (
	ClassInTreeAtDesign  = "in_tree_at_design"
	ClassInTreeLater     = "in_tree_later"
	ClassOutOfTree       = "out_of_tree"
	ClassByTheController = "by_the_controller"
)

// classes is every class, in the order the card lists them.
var classes = []string{ClassInTreeAtDesign, ClassInTreeLater, ClassOutOfTree, ClassByTheController}

// Report is one answer of the Legion section: the window, the issues that finished in it, and the
// four metrics.
type Report struct {
	Project    string    `json:"project"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Scope      Scope     `json:"scope"`
	ComputedAt time.Time `json:"computed_at"`
	// Finished is how many issues finished in the window under this scope: every metric's
	// population.
	Finished  int       `json:"finished"`
	WorkTime  WorkTime  `json:"work_time"`
	PhaseTime PhaseTime `json:"phase_time"`
	Rework    Rework    `json:"rework"`
	Splitting Splitting `json:"splitting"`
}

// WorkTime is metric 1: hours from an issue's first worker event to its close, less its parked
// and queued time.
type WorkTime struct {
	MedianHours float64         `json:"median_hours"`
	P90Hours    float64         `json:"p90_hours"`
	Issues      []WorkTimeIssue `json:"issues"`
}

// WorkTimeIssue is one finished issue's work time with what was taken out of it.
type WorkTimeIssue struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Priority    *int       `json:"priority"`
	Root        bool       `json:"root"`
	StartedAt   time.Time  `json:"started_at"`
	ClosedAt    time.Time  `json:"closed_at"`
	MergedAt    *time.Time `json:"merged_at"`
	Hours       float64    `json:"hours"`
	ParkedHours float64    `json:"parked_hours"`
	QueuedHours float64    `json:"queued_hours"`
}

// PhaseTime is metric 2: the seven bars, each split into the five segments.
type PhaseTime struct {
	Bars []RoleBar `json:"bars"`
	// IdleShare is the idle hours of every bar over their whole time.
	IdleShare float64 `json:"idle_share"`
}

// RoleBar is one worker type's bar.
type RoleBar struct {
	Role string `json:"role"`
	// Hours is the bar's whole time across the finished issues.
	Hours float64 `json:"hours"`
	// Issues is how many finished issues spent time on this bar, which is what the median is
	// taken over.
	Issues              int      `json:"issues"`
	MedianHoursPerIssue float64  `json:"median_hours_per_issue"`
	Segments            Segments `json:"segments"`
}

// Rework is metric 3.
type Rework struct {
	MedianReviewRounds float64 `json:"median_review_rounds"`
	// SentBackShare is the share of finished issues moved from testing, reviewing or accepting
	// back to implementing at least once.
	SentBackShare float64 `json:"sent_back_share"`
	// HoursAfterFirstReview is the implementing and testing hours after the first review, across
	// the finished issues, and its median per issue.
	HoursAfterFirstReview       float64       `json:"hours_after_first_review"`
	MedianHoursAfterFirstReview float64       `json:"median_hours_after_first_review"`
	Issues                      []ReworkIssue `json:"issues"`
}

// ReworkIssue is one finished issue's rework numbers.
type ReworkIssue struct {
	Key                         string  `json:"key"`
	ReviewRounds                int     `json:"review_rounds"`
	SentBack                    bool    `json:"sent_back"`
	ImplementingHoursAfterFirst float64 `json:"implementing_hours_after_first_review"`
	TestingHoursAfterFirst      float64 `json:"testing_hours_after_first_review"`
}

// Splitting is metric 4: what Legion sessions filed in the window, by class and week, against the
// issues that finished per week.
type Splitting struct {
	Weeks []SplittingWeek `json:"weeks"`
	Filed []FiledIssue    `json:"filed"`
	// FinishedRoots is how many roots finished in the window, ChildrenPerRoot the mean number of
	// children of those roots, and the two medians are their work time with and without children.
	FinishedRoots                int     `json:"finished_roots"`
	ChildrenPerRoot              float64 `json:"children_per_root"`
	MedianHoursRootsWithChildren float64 `json:"median_hours_roots_with_children"`
	MedianHoursRootsNoChildren   float64 `json:"median_hours_roots_without_children"`
}

// SplittingWeek is one UTC week: each class's filings and how many issues finished.
type SplittingWeek struct {
	Week     time.Time      `json:"week"`
	Filed    map[string]int `json:"filed"`
	Finished int            `json:"finished"`
}

// FiledIssue is one issue a Legion session created in the window.
type FiledIssue struct {
	Key   string    `json:"key"`
	Title string    `json:"title"`
	At    time.Time `json:"at"`
	Class string    `json:"class"`
	// Filer is the role that filed it and the issue that role was working, or the controller.
	Filer      string `json:"filer"`
	FilerIssue string `json:"filer_issue"`
	// FilerPhase is the phase the filer's issue was in when it filed, which the table names for
	// an issue filed in the tree after design.
	FilerPhase string `json:"filer_phase"`
}

// Compute is the metrics answer for a population. It reads nothing: the route and the live hub
// both call it over a population Read gathered, so a live push and a GET answer the same numbers.
func Compute(population *Population, scope Scope, now time.Time) Report {
	computed := &computation{
		population: population,
		scope:      scope,
		phases:     map[string]phaseTimeline{},
		views:      map[string]*issueView{},
		roots:      map[string]string{},
		workTimes:  map[string]workTimeRow{},
	}
	finished := computed.finished()
	return Report{
		Project:    population.Project,
		From:       population.From,
		To:         population.To,
		Scope:      scope,
		ComputedAt: now,
		Finished:   len(finished),
		WorkTime:   computed.workTime(finished),
		PhaseTime:  computed.phaseTime(finished),
		Rework:     computed.rework(finished),
		Splitting:  computed.splitting(finished),
	}
}

// computation is one answer being computed, with the timelines and views each metric shares.
type computation struct {
	population *Population
	scope      Scope
	phases     map[string]phaseTimeline
	views      map[string]*issueView
	// roots is each key's tree root and workTimes each issue's work-time row, each computed
	// once: TreeRoot walks a parent chain allocating a set as it goes, workTimeOf runs the whole
	// span algebra, and the four metrics ask for each of them three or four times per issue.
	roots     map[string]string
	workTimes map[string]workTimeRow
}

// phaseTimeline is one issue's phase timeline with where it came from: the daemon's own
// observations, or the event log's fallback (Population.Phases).
type phaseTimeline struct {
	spans    []PhaseSpan
	observed bool
}

// workTimeRow is one memoised answer of workTimeOf: the row, the spans its hours are the measure
// of, and whether the issue has a work time at all.
type workTimeRow struct {
	row    WorkTimeIssue
	worked []Span
	ok     bool
}

// finished is the population every metric counts: "issues of the picked project that finished in
// the window. An issue finished when it closed with Legion phase work: an event by a session
// titled `Legion <role> · <KEY>` for its own key, or a handoff", narrowed to the scope.
func (c *computation) finished() []*Issue {
	var finished []*Issue
	for _, key := range c.population.Keys {
		issue := c.population.Issues[key]
		if !c.finishedInWindow(issue) {
			continue
		}
		switch c.scope {
		case ScopeRoots:
			if !c.isRoot(issue) {
				continue
			}
		case ScopeChildren:
			if c.isRoot(issue) {
				continue
			}
		}
		finished = append(finished, issue)
	}
	return finished
}

// finishedInWindow is the finished rule itself, before any scope.
func (c *computation) finishedInWindow(issue *Issue) bool {
	if issue.ClosedAt == nil || issue.ClosedAt.Before(c.population.From) || !issue.ClosedAt.Before(c.population.To) {
		return false
	}
	return len(issue.FirstRoleEvent) > 0 || len(issue.Handoffs) > 0
}

// treeRoot is the issue's tree root (Population.TreeRoot), computed once per key: the walk is a
// pure function of a population Compute never writes to, so one answer stands for the whole
// computation.
func (c *computation) treeRoot(key string) string {
	if root, held := c.roots[key]; held {
		return root
	}
	root := c.population.TreeRoot(key)
	c.roots[key] = root
	return root
}

func (c *computation) isRoot(issue *Issue) bool {
	return c.treeRoot(issue.Key) == issue.Key
}

// timelineOf is the issue's phase timeline with where it came from, computed once.
func (c *computation) timelineOf(issue *Issue) phaseTimeline {
	if timeline, held := c.phases[issue.Key]; held {
		return timeline
	}
	spans, observed := c.population.Phases(issue)
	timeline := phaseTimeline{spans: spans, observed: observed}
	c.phases[issue.Key] = timeline
	return timeline
}

// phasesOf is the issue's phase timeline alone, for the rules that read the spans and not where
// they came from.
func (c *computation) phasesOf(issue *Issue) []PhaseSpan {
	return c.timelineOf(issue).spans
}

// viewOf is the issue with its tree and the timelines its segments read, computed once.
func (c *computation) viewOf(issue *Issue) *issueView {
	if view, held := c.views[issue.Key]; held {
		return view
	}
	root := c.population.Issues[c.treeRoot(issue.Key)]
	if root == nil {
		root = issue
	}
	view := &issueView{
		population:  c.population,
		issue:       issue,
		root:        root,
		timeline:    c.phasesOf(issue),
		childPhase:  map[string][]PhaseSpan{},
		childWorked: map[string][]Span{},
		end:         c.population.issueEnd(issue),
	}
	for _, key := range root.Children {
		child := c.population.Issues[key]
		if child == nil || child.Key == issue.Key {
			continue
		}
		view.children = append(view.children, child)
		if root.Key != issue.Key {
			// Only a root's view reads its children's timelines: the waiting-on-children rule
			// answers nothing for a child, and a child's view takes its tree's asks alone.
			continue
		}
		view.childPhase[child.Key] = c.phasesOf(child)
		view.childWorked[child.Key] = c.barSpansOf(child)
	}
	c.views[issue.Key] = view
	return view
}

// workTime is metric 1: "Hours from the issue's first worker event to its close, minus any time in
// `backlog` or `icebox` and any time back in the queue after it."
func (c *computation) workTime(finished []*Issue) WorkTime {
	metric := WorkTime{Issues: make([]WorkTimeIssue, 0, len(finished))}
	hoursPer := make([]float64, 0, len(finished))
	for _, issue := range finished {
		row, ok := c.workTimeOf(issue)
		if !ok {
			continue
		}
		metric.Issues = append(metric.Issues, row)
		hoursPer = append(hoursPer, row.Hours)
	}
	slices.Sort(hoursPer)
	metric.MedianHours, metric.P90Hours = quantile(hoursPer, 0.5), quantile(hoursPer, 0.9)
	return metric
}

// workTimeOf is one finished issue's work time, computed once: the work-time metric asks for
// every finished issue's row, and the splitting metric asks again for every finished root.
func (c *computation) workTimeOf(issue *Issue) (WorkTimeIssue, bool) {
	held := c.workedOf(issue)
	return held.row, held.ok
}

// workedSpansOf is the time the issue worked: the very spans computeWorkTime totalled for the
// hours of its row. Metric 2 shares a bar out over them and nothing else, so an hour the work
// time left out cannot reach a bar and no second subtraction can drift from this one.
func (c *computation) workedSpansOf(issue *Issue) []Span {
	return c.workedOf(issue).worked
}

// barSpansOf is the hours of an issue its seven bars share out: the work time metric 1 reports,
// less the time it sat in the queue, which is on no bar. Metric 2's bars and metric 3's hours
// after the first review are both the measure of these spans, so no table of a card counts an
// hour the card's own bars leave out.
func (c *computation) barSpansOf(issue *Issue) []Span {
	return subtract(c.workedSpansOf(issue), phaseSpansOf(c.phasesOf(issue), phaseQueued))
}

// workedOf is the memoised answer of computeWorkTime.
func (c *computation) workedOf(issue *Issue) workTimeRow {
	if held, computed := c.workTimes[issue.Key]; computed {
		return held
	}
	held := c.computeWorkTime(issue)
	c.workTimes[issue.Key] = held
	return held
}

// computeWorkTime is the work time itself: the issue's life, less the time it sat parked and the
// time it sat back in the queue. The spans that are left are the answer's second half, because
// they are what the issue's bars of phase time are shared out over.
//
// Every issue the metric reports a row for has closed (finishedInWindow), and the row's ClosedAt
// is that close. A child still open has a work time too, which its root's waiting-on-children
// rule reads: its life runs to the window's end, as every other span of a live issue does
// (issueEnd).
func (c *computation) computeWorkTime(issue *Issue) workTimeRow {
	start, ok := firstWorkerEvent(issue)
	if !ok {
		return workTimeRow{}
	}
	end := c.population.issueEnd(issue)
	// A life of no length leaves no span to park, to queue or to put on a bar, which the span
	// algebra below answers on its own.
	life := []Span{{From: start, To: end}}
	parked := intersect(c.population.parkedSpans(issue), life)
	queued := intersect(c.population.queuedSpans(issue), life)
	worked := subtract(subtract(life, parked), queued)
	return workTimeRow{
		row: WorkTimeIssue{
			Key: issue.Key, Title: issue.Title, Priority: issue.Priority, Root: c.isRoot(issue),
			StartedAt: start, ClosedAt: end, MergedAt: issue.MergedAt,
			Hours:       hours(totalOf(worked)),
			ParkedHours: hours(totalOf(parked)),
			QueuedHours: hours(totalOf(queued)),
		},
		worked: worked,
		ok:     true,
	}
}

// firstWorkerEvent is where an issue's work time starts: the first event of any session titled
// `Legion <role> · <its key>`, and its first handoff for an issue whose Legion work is a handoff
// alone.
func firstWorkerEvent(issue *Issue) (time.Time, bool) {
	var first time.Time
	for _, at := range issue.FirstRoleEvent {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	for _, handoff := range issue.Handoffs {
		if first.IsZero() || handoff.At.Before(first) {
			first = handoff.At
		}
	}
	return first, !first.IsZero()
}

// phaseTime is metric 2: one stacked bar per worker type, each hour of each phase going to the
// first segment of the precedence that holds.
//
// The hours shared out are the issue's work time and nothing else (the spec's "Rule details —
// Parked and pre-work hours in phase time": "An hour that work time to merge leaves out is on no
// bar of phase time ... So the bars of an issue add up to its work time, and the two cards
// agree"). Each bar takes the phases it holds and the queue is on none of them, so the seven are
// a partition of the work time: the timeline leaves no hour between two phases (the spec's
// "Phase boundaries before observations"), and every phase an issue can be in is on one of the
// seven bars.
func (c *computation) phaseTime(finished []*Issue) PhaseTime {
	metric := PhaseTime{Bars: make([]RoleBar, len(bars))}
	perIssue := make([][]float64, len(bars))
	for index, bar := range bars {
		metric.Bars[index].Role = bar.Role
		perIssue[index] = make([]float64, 0, len(finished))
	}
	for _, issue := range finished {
		view := c.viewOf(issue)
		shared := c.barSpansOf(issue)
		for index, bar := range bars {
			spans := intersect(phaseSpansOf(view.timeline, bar.Phases...), shared)
			segments := view.split(spans)
			// Every issue of the population counts towards each bar's median, one the role
			// never worked as a zero, so the seven medians describe one population and not
			// seven.
			perIssue[index] = append(perIssue[index], segments.total())
			if len(spans) == 0 {
				continue
			}
			metric.Bars[index].Segments.addSegments(segments)
			metric.Bars[index].Issues++
		}
	}
	var whole, idle float64
	for index := range metric.Bars {
		row := &metric.Bars[index]
		row.Hours = round(row.Segments.total())
		row.MedianHoursPerIssue = median(perIssue[index])
		row.Segments = rounded(row.Segments)
		whole += row.Hours
		idle += row.Segments.Idle
	}
	if whole > 0 {
		metric.IdleShare = round(idle / whole)
	}
	return metric
}

// rework is metric 3: review rounds, the share of issues sent back, and the implementing and
// testing hours after the first review.
func (c *computation) rework(finished []*Issue) Rework {
	metric := Rework{Issues: make([]ReworkIssue, 0, len(finished))}
	rounds, afterReview := make([]float64, 0, len(finished)), make([]float64, 0, len(finished))
	var sentBack int
	for _, issue := range finished {
		phases := c.timelineOf(issue)
		timeline := phases.spans
		row := ReworkIssue{Key: issue.Key, ReviewRounds: reviewRounds(issue, phases), SentBack: sentBackOnce(issue, phases)}
		if at, reviewed := firstReview(issue, phases); reviewed {
			after := intersect(c.barSpansOf(issue), []Span{{From: at, To: c.population.issueEnd(issue)}})
			row.ImplementingHoursAfterFirst = hours(totalOf(intersect(phaseSpansOf(timeline, phaseImplementing), after)))
			row.TestingHoursAfterFirst = hours(totalOf(intersect(phaseSpansOf(timeline, phaseTesting), after)))
		}
		if row.SentBack {
			sentBack++
		}
		rounds = append(rounds, float64(row.ReviewRounds))
		hoursAfter := row.ImplementingHoursAfterFirst + row.TestingHoursAfterFirst
		afterReview = append(afterReview, hoursAfter)
		metric.HoursAfterFirstReview += hoursAfter
		metric.Issues = append(metric.Issues, row)
	}
	metric.MedianReviewRounds = median(rounds)
	metric.MedianHoursAfterFirstReview = median(afterReview)
	metric.HoursAfterFirstReview = round(metric.HoursAfterFirstReview)
	if len(finished) > 0 {
		metric.SentBackShare = round(float64(sentBack) / float64(len(finished)))
	}
	return metric
}

// reviewRounds is "entries into `reviewing`, or into status `needs_review` before observations".
// Before observations is where the sampler drew no timeline of the issue's own life: an issue
// whose only `phase` mark is the one the sampler's first pass left after it closed has no
// observed round of any hour it lived, so its rounds are its status entries.
func reviewRounds(issue *Issue, timeline phaseTimeline) int {
	if !timeline.observed {
		return statusEntries(issue, statusNeedsReview)
	}
	rounds := 0
	for index, span := range timeline.spans {
		if span.Phase == phaseReviewing && (index == 0 || timeline.spans[index-1].Phase != phaseReviewing) {
			rounds++
		}
	}
	return rounds
}

// sentBackOnce is "a move from `testing`, `reviewing` or `accepting` to `implementing`": off the
// observed timeline where the sampler drew one, and off the status history before observations,
// as the review rounds beside it are, since `needs_review` is the status of both reviewing and
// accepting and `in_progress` the status of implementing.
func sentBackOnce(issue *Issue, timeline phaseTimeline) bool {
	if !timeline.observed {
		return statusMoved(issue, sentBackStatuses, statusImplementing)
	}
	for index := 1; index < len(timeline.spans); index++ {
		if timeline.spans[index].Phase != phaseImplementing {
			continue
		}
		switch timeline.spans[index-1].Phase {
		case phaseTesting, phaseReviewing, phaseAccepting:
			return true
		}
	}
	return false
}

// firstEntry is when the timeline first entered a phase.
func firstEntry(timeline []PhaseSpan, phase string) (time.Time, bool) {
	for _, span := range timeline {
		if span.Phase == phase {
			return span.Span.From, true
		}
	}
	return time.Time{}, false
}

// firstReview is when the issue was first reviewed: the first `reviewing` stretch of the
// timeline the sampler drew, and the first entry into status `needs_review` before observations,
// where the rounds and the send-back beside it are read off the same status history ("Rule
// details — Review rounds and send-backs before observations").
func firstReview(issue *Issue, timeline phaseTimeline) (time.Time, bool) {
	if !timeline.observed {
		return firstStatusEntry(issue, statusNeedsReview)
	}
	return firstEntry(timeline.spans, phaseReviewing)
}

// splitting is metric 4: each issue a Legion session created in the window, by the parent it had
// at its `issue.created` event, counted per week against the issues that finished per week.
func (c *computation) splitting(finished []*Issue) Splitting {
	metric := Splitting{Filed: make([]FiledIssue, 0, len(c.population.Filed))}
	weeks := map[time.Time]*SplittingWeek{}
	week := func(at time.Time) *SplittingWeek {
		start := weekStart(at)
		if held, ok := weeks[start]; ok {
			return held
		}
		fresh := &SplittingWeek{Week: start, Filed: map[string]int{}}
		for _, class := range classes {
			fresh.Filed[class] = 0
		}
		weeks[start] = fresh
		return fresh
	}
	for _, filed := range c.population.Filed {
		row := c.classify(filed)
		metric.Filed = append(metric.Filed, row)
		week(filed.At).Filed[row.Class]++
	}
	for _, issue := range finished {
		week(*issue.ClosedAt).Finished++
	}
	metric.Weeks = make([]SplittingWeek, 0, len(weeks))
	for _, one := range weeks {
		metric.Weeks = append(metric.Weeks, *one)
	}
	slices.SortFunc(metric.Weeks, func(a, b SplittingWeek) int { return a.Week.Compare(b.Week) })

	// The roots that finished in the window, whatever scope the reader picked: the table's last
	// two numbers are about roots by name, so the toggle does not change them.
	var withChildren, withoutChildren []float64
	var children int
	for _, key := range c.population.Keys {
		issue := c.population.Issues[key]
		if !c.finishedInWindow(issue) || !c.isRoot(issue) {
			continue
		}
		metric.FinishedRoots++
		children += len(issue.Children)
		row, ok := c.workTimeOf(issue)
		if !ok {
			continue
		}
		if len(issue.Children) > 0 {
			withChildren = append(withChildren, row.Hours)
		} else {
			withoutChildren = append(withoutChildren, row.Hours)
		}
	}
	if metric.FinishedRoots > 0 {
		metric.ChildrenPerRoot = round(float64(children) / float64(metric.FinishedRoots))
	}
	metric.MedianHoursRootsWithChildren = median(withChildren)
	metric.MedianHoursRootsNoChildren = median(withoutChildren)
	return metric
}

// classify is one filing's class: by the controller, in the filer's tree at design or later, or
// out of the tree.
func (c *computation) classify(filed Filed) FiledIssue {
	row := FiledIssue{
		Key: filed.Key, Title: filed.Title, At: filed.At,
		Filer: filed.Filer.Role, FilerIssue: filed.Filer.Issue,
	}
	if filed.Filer.Controller {
		row.Class = ClassByTheController
		return row
	}
	row.Class = ClassOutOfTree
	filer := c.population.Issues[filed.Filer.Issue]
	if filer == nil {
		return row
	}
	// The tree of a filed issue: walk up from the parent it had at its `issue.created` event,
	// through each issue's parent as Dispatch records it, and take the top it reaches; the filer
	// walks the same way. The new issue is in the filer's tree when both walks reach the same
	// issue, at any depth. The event's own `ancestors` list is read by nothing here: Dispatch
	// leaves it empty, so a filing under a child of the filer read as out of the tree (the
	// spec's "Rule details — The tree of a filed issue").
	root := c.treeRoot(filer.Key)
	if filed.Parent == "" || c.treeRoot(filed.Parent) != root {
		return row
	}
	row.FilerPhase = phaseAt(c.phasesOf(filer), filed.At)
	// "created before the filer's issue had its first implementer event": an issue whose filer
	// never reached an implementer never passed that moment, so its filings are at design.
	implementer, started := filer.FirstRoleEvent["implementer"]
	if !started || filed.At.Before(implementer) {
		row.Class = ClassInTreeAtDesign
		return row
	}
	row.Class = ClassInTreeLater
	return row
}

// phaseAt is the phase a timeline holds at a moment, or "" where it holds none.
func phaseAt(timeline []PhaseSpan, at time.Time) string {
	for _, span := range timeline {
		if !at.Before(span.Span.From) && at.Before(span.Span.To) {
			return span.Phase
		}
	}
	return ""
}

// weekStart is the UTC Monday the moment's week begins on.
func weekStart(at time.Time) time.Time {
	day := at.UTC().Truncate(24 * time.Hour)
	back := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -back)
}

// quantile is the q-th quantile of a sorted list, "linear interpolation between the closest
// ranks", and zero for an empty one: the delivery measures' own rule (measures.Quantile),
// rounded as every number of a report is.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return round(measures.Quantile(sorted, q))
}

// median sorts values and is their middle. Every caller builds the slice for its median alone
// and reads it no further, so the sort in place is unobservable.
func median(values []float64) float64 {
	slices.Sort(values)
	return quantile(values, 0.5)
}

// hours is a duration in hours, rounded as every number of a report is.
func hours(spent time.Duration) float64 {
	return round(spent.Hours())
}

// round keeps four decimals: enough that a sum of minutes reads exactly, and few enough that no
// answer carries floating-point noise.
func round(value float64) float64 {
	return math.Round(value*1e4) / 1e4
}

// rounded rounds each of a bar's segments.
func rounded(segments Segments) Segments {
	return Segments{
		WaitingOnPerson:   round(segments.WaitingOnPerson),
		InATurn:           round(segments.InATurn),
		CIWait:            round(segments.CIWait),
		WaitingOnChildren: round(segments.WaitingOnChildren),
		Idle:              round(segments.Idle),
	}
}
