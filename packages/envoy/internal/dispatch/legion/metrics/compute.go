package metrics

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
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
// for the phases no role works. Time in the queue is on no bar, so `queued` is on none of them.
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
	// ChildrenPerRoot is the mean number of children of the roots that finished in the window,
	// and the two medians are their work time with and without children.
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
		phases:     map[string][]PhaseSpan{},
		views:      map[string]*issueView{},
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
	phases     map[string][]PhaseSpan
	views      map[string]*issueView
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

func (c *computation) isRoot(issue *Issue) bool {
	return c.population.TreeRoot(issue.Key) == issue.Key
}

// phasesOf is the issue's phase timeline, computed once.
func (c *computation) phasesOf(issue *Issue) []PhaseSpan {
	if timeline, held := c.phases[issue.Key]; held {
		return timeline
	}
	timeline := c.population.Phases(issue)
	c.phases[issue.Key] = timeline
	return timeline
}

// viewOf is the issue with its tree and the timelines its segments read, computed once.
func (c *computation) viewOf(issue *Issue) *issueView {
	if view, held := c.views[issue.Key]; held {
		return view
	}
	root := c.population.Issues[c.population.TreeRoot(issue.Key)]
	if root == nil {
		root = issue
	}
	view := &issueView{
		population: c.population,
		issue:      issue,
		root:       root,
		timeline:   c.phasesOf(issue),
		childPhase: map[string][]PhaseSpan{},
		end:        c.population.issueEnd(issue),
	}
	for _, key := range root.Children {
		child := c.population.Issues[key]
		if child == nil || child.Key == issue.Key {
			continue
		}
		view.children = append(view.children, child)
		view.childPhase[child.Key] = c.phasesOf(child)
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

// workTimeOf is one finished issue's work time.
func (c *computation) workTimeOf(issue *Issue) (WorkTimeIssue, bool) {
	start, ok := firstWorkerEvent(issue)
	if !ok {
		return WorkTimeIssue{}, false
	}
	life := Span{From: start, To: *issue.ClosedAt}
	if life.Duration() <= 0 {
		return WorkTimeIssue{
			Key: issue.Key, Title: issue.Title, Priority: issue.Priority, Root: c.isRoot(issue),
			StartedAt: start, ClosedAt: *issue.ClosedAt, MergedAt: issue.MergedAt,
		}, true
	}
	whole := []Span{life}
	parked := intersect(c.population.parkedSpans(issue), whole)
	queued := intersect(c.population.queuedSpans(issue), whole)
	worked := subtract(subtract(whole, parked), queued)
	return WorkTimeIssue{
		Key: issue.Key, Title: issue.Title, Priority: issue.Priority, Root: c.isRoot(issue),
		StartedAt: start, ClosedAt: *issue.ClosedAt, MergedAt: issue.MergedAt,
		Hours:       hours(totalOf(worked)),
		ParkedHours: hours(totalOf(parked)),
		QueuedHours: hours(totalOf(queued)),
	}, true
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
func (c *computation) phaseTime(finished []*Issue) PhaseTime {
	metric := PhaseTime{Bars: make([]RoleBar, 0, len(bars))}
	var whole, idle float64
	for _, bar := range bars {
		row := RoleBar{Role: bar.Role}
		var perIssue []float64
		for _, issue := range finished {
			view := c.viewOf(issue)
			spans := phaseSpansOf(view.timeline, bar.Phases...)
			if len(spans) == 0 {
				continue
			}
			segments := view.split(spans)
			row.Segments.addSegments(segments)
			row.Issues++
			perIssue = append(perIssue, segments.total())
		}
		slices.Sort(perIssue)
		row.Hours = round(row.Segments.total())
		row.MedianHoursPerIssue = quantile(perIssue, 0.5)
		row.Segments = rounded(row.Segments)
		whole += row.Hours
		idle += row.Segments.Idle
		metric.Bars = append(metric.Bars, row)
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
	var rounds, afterReview []float64
	var sentBack int
	for _, issue := range finished {
		timeline := c.phasesOf(issue)
		row := ReworkIssue{Key: issue.Key, ReviewRounds: c.reviewRounds(issue, timeline), SentBack: sentBackOnce(timeline)}
		if at, reviewed := firstEntry(timeline, phaseReviewing); reviewed {
			after := []Span{{From: at, To: c.population.issueEnd(issue)}}
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
	slices.Sort(rounds)
	slices.Sort(afterReview)
	metric.MedianReviewRounds = quantile(rounds, 0.5)
	metric.MedianHoursAfterFirstReview = quantile(afterReview, 0.5)
	metric.HoursAfterFirstReview = round(metric.HoursAfterFirstReview)
	if len(finished) > 0 {
		metric.SentBackShare = round(float64(sentBack) / float64(len(finished)))
	}
	return metric
}

// reviewRounds is "entries into `reviewing`, or into status `needs_review` before observations".
func (c *computation) reviewRounds(issue *Issue, timeline []PhaseSpan) int {
	if len(issue.Observed[legion.FieldPhase]) == 0 {
		return statusEntries(issue, "needs_review")
	}
	rounds := 0
	for index, span := range timeline {
		if span.Phase == phaseReviewing && (index == 0 || timeline[index-1].Phase != phaseReviewing) {
			rounds++
		}
	}
	return rounds
}

// sentBackOnce is "a move from `testing`, `reviewing` or `accepting` to `implementing`".
func sentBackOnce(timeline []PhaseSpan) bool {
	for index := 1; index < len(timeline); index++ {
		if timeline[index].Phase != phaseImplementing {
			continue
		}
		switch timeline[index-1].Phase {
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
	slices.Sort(withChildren)
	slices.Sort(withoutChildren)
	metric.MedianHoursRootsWithChildren = quantile(withChildren, 0.5)
	metric.MedianHoursRootsNoChildren = quantile(withoutChildren, 0.5)
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
	root := c.population.TreeRoot(filer.Key)
	inTree := filed.Parent == filer.Key || filed.Parent == root ||
		slices.Contains(filed.Ancestors, filer.Key) || slices.Contains(filed.Ancestors, root)
	if !inTree {
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
// ranks", and zero for an empty one.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return round(sorted[0])
	}
	rank := q * float64(len(sorted)-1)
	lower := int(math.Floor(rank))
	if lower >= len(sorted)-1 {
		return round(sorted[len(sorted)-1])
	}
	return round(sorted[lower] + (rank-float64(lower))*(sorted[lower+1]-sorted[lower]))
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
