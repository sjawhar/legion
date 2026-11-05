// legion_board.go serves GET /api/v1/legion/board, the Legion view on the project Issues page
// (dispatch://INFRA-1077, the spec's "The Legion view on the Issues page"), and the body the
// fourth live view kind sends (live_frames.go's legionBoardView).
//
// The answer is read from what Dispatch holds, never from the daemon: the sampler reads each
// daemon once a minute under a lease (internal/dispatch/legion) and records what moved, and this
// reads the newest value of each field it recorded, the newest read's own verdict, and the
// project's issues. A board that called the daemon would read it once per open page and once per
// event burst, which is what the per-minute lease exists to prevent.
//
// Two disciplines, as in the metrics package:
//
//   - The daemon's state is read loosely. Every fact here is a field legion/state.go observes, and
//     a field a daemon does not send is a field the board has no observation of. Nothing takes a
//     slot holder for a root: a card's tree is the daemon's own `tree` where a read carried one
//     (dispatch://INFRA-980 adds it) and the parent chain otherwise.
//   - The columns are the spec's, in one ordered list (legionBoardColumnOrder), and a phase maps
//     to exactly one of them (legionPhaseColumns), so the view and metric 2's "no role" bar agree
//     on where `awaiting_person` and `held` go (the plan's D11).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The board's columns. Queue holds the daemon's waiting line and, after it, the issues a blocker
// link holds; Design the issues whose phase is still `admitted` that are on a card at all — the
// slot holders, and the ones no card stands above, which are on no other issue's count
// (legionBoardCompute); Merging `retro`, `merging` and `awaiting_merge`; and Held the phase
// metric 2 puts on the "no role" bar beside `awaiting_person` (the plan's D11).
const (
	legionColumnQueue        = "queue"
	legionColumnDesign       = "design"
	legionColumnPlanning     = "planning"
	legionColumnImplementing = "implementing"
	legionColumnTesting      = "testing"
	legionColumnReviewing    = "reviewing"
	legionColumnAccepting    = "accepting"
	legionColumnMerging      = "merging"
	legionColumnAwaiting     = "awaiting_person"
	legionColumnHeld         = "held"
)

// legionBoardColumnOrder is every column of the board, left to right, with the label the view
// shows. Every answer carries all of them, empty where nothing is in them, so a page draws the
// board from the body alone.
var legionBoardColumnOrder = []struct{ id, label string }{
	{legionColumnQueue, "Queue"},
	{legionColumnDesign, "Design"},
	{legionColumnPlanning, "Planning"},
	{legionColumnImplementing, "Implementing"},
	{legionColumnTesting, "Testing"},
	{legionColumnReviewing, "Reviewing"},
	{legionColumnAccepting, "Accepting"},
	{legionColumnMerging, "Merging"},
	{legionColumnAwaiting, "Waiting on a person"},
	{legionColumnHeld, "Held"},
}

// legionPhaseColumns is the column each phase the daemon reports falls in. A phase this does not
// name (`done`, and any phase a later daemon adds) puts the issue on no card: the board shows the
// work in flight, and a phase Dispatch has never heard of is not something it can place.
var legionPhaseColumns = map[string]string{
	legion.PhaseAdmitted:       legionColumnDesign,
	legion.PhasePlanning:       legionColumnPlanning,
	legion.PhaseImplementing:   legionColumnImplementing,
	legion.PhaseTesting:        legionColumnTesting,
	legion.PhaseReviewing:      legionColumnReviewing,
	legion.PhaseAccepting:      legionColumnAccepting,
	legion.PhaseRetro:          legionColumnMerging,
	legion.PhaseMerging:        legionColumnMerging,
	legion.PhaseAwaitingMerge:  legionColumnMerging,
	legion.PhaseAwaitingPerson: legionColumnAwaiting,
	legion.PhaseHeld:           legionColumnHeld,
}

// legionNotConfiguredCode and legionNotConfiguredMessage are the one refusal a project with no
// Legion daemon gets, whichever route or live view asked (getLegionBoard,
// putLegionProjectSettings, the live hub's worker).
const (
	legionNotConfiguredCode    = "LEGION_NOT_CONFIGURED"
	legionNotConfiguredMessage = "this project has no Legion daemon; set one through PUT /api/v1/settings/legion/{project}"
)

// legionBoardResponse is GET /api/v1/legion/board: the project the board shows, its header, its
// columns, the settings its daemon reports with where each came from, and when it was computed.
// Mirrors packages/contracts/src/dispatch-api.ts's LegionBoardResponse exactly.
type legionBoardResponse struct {
	Project string              `json:"project"`
	Header  legionBoardHeader   `json:"header"`
	Columns []legionBoardColumn `json:"columns"`
	// Settings is the settings the daemon reports, which hold no secret, and SettingSources where
	// each came from — the file or the route, who changed it and when. Both are the daemon's own
	// bytes, key by key, and both are {} until a read records them, so a project whose daemon has
	// never answered still renders. The settings control on the Legion view reads them.
	Settings       map[string]json.RawMessage `json:"settings"`
	SettingSources map[string]json.RawMessage `json:"setting_sources"`
	ComputedAt     time.Time                  `json:"computed_at"`
}

// legionBoardHeader is what the view's header shows: the slots in use against the cap, the live
// trees, the queue's length, and the last read of the daemon — when it was made, whether it
// succeeded and why it did not. SlotCap is null until a read records the daemon's admission
// block, Trees is 0 until a daemon that reports its live trees does (dispatch://INFRA-980), and
// ReadAt is null for a project whose daemon has never answered at all, which shows the board with
// empty columns.
type legionBoardHeader struct {
	SlotsInUse  int        `json:"slots_in_use"`
	SlotCap     *int       `json:"slot_cap"`
	Trees       int        `json:"trees"`
	QueueLength int        `json:"queue_length"`
	ReadAt      *time.Time `json:"read_at"`
	ReadOK      bool       `json:"read_ok"`
	ReadError   *string    `json:"read_error"`
}

// legionBoardColumn is one column: its id, the label the view shows, and its cards in the order
// the view draws them.
type legionBoardColumn struct {
	ID    string            `json:"id"`
	Label string            `json:"label"`
	Cards []legionBoardCard `json:"cards"`
}

// legionBoardCard is one issue on the board. Key, Title and Priority come from Dispatch's own
// issues table, joined on the daemon's key (the plan's D13; the daemon's state carries none of the
// three). Phase, PhaseSince, Slot, Roles, ChecksVerdict and WaitingOn are the newest observations
// of that issue, PhaseSince being when the phase it is in was first recorded — the time in the
// phase the card shows. BlockedBy names the issues a blocker link holds it on, and Children counts
// the issues of its tree that its tree has not started, which are on no card of their own.
type legionBoardCard struct {
	Key           string           `json:"key"`
	Title         string           `json:"title"`
	Priority      *int             `json:"priority"`
	Phase         string           `json:"phase"`
	PhaseSince    *time.Time       `json:"phase_since"`
	Slot          *int             `json:"slot"`
	Roles         []string         `json:"roles"`
	ChecksVerdict *string          `json:"checks_verdict"`
	BlockedBy     []string         `json:"blocked_by"`
	WaitingOn     *legionBoardWait `json:"waiting_on"`
	Tree          string           `json:"tree"`
	Children      int              `json:"children"`
}

// legionBoardWait is what a root in `awaiting_person` waits on, as the daemon recorded it: the
// kind of wait (`ask`, `gate`, or whatever a later daemon names), when it began, and when a person
// answered it — an answered root waits for a slot at the front of the queue rather than for a
// person.
type legionBoardWait struct {
	Kind       string     `json:"kind"`
	Since      *time.Time `json:"since"`
	AnsweredAt *time.Time `json:"answered_at"`
}

// getLegionBoard answers GET /api/v1/legion/board. Its one parameter is `project`; with none it
// reads the first project that names a Legion daemon, as GET /api/v1/legion/metrics does. A
// project that names no daemon is 404 LEGION_NOT_CONFIGURED, which is what shows no view; a
// project whose daemon has never answered is answered the board with empty columns and the read's
// failure in its header.
func (s *server) getLegionBoard(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project, ok := s.legionMetricsProject(w, r)
	if !ok {
		return
	}
	population, err := s.readLegionBoard(r.Context(), project)
	if err != nil {
		if errors.Is(err, legion.ErrNoSettings) {
			writeError(w, legionNotConfiguredCode, http.StatusNotFound, legionNotConfiguredMessage)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, legionBoardCompute(population, time.Now().UTC()))
}

// legionBoardPopulation is every read one board answer makes: the project's Legion setting, the
// newest read of its daemon, the newest value of every field the sampler recorded, and the
// project's own issues in Dispatch's rank order.
type legionBoardPopulation struct {
	project string
	// read is the newest legion_reads row, nil for a project whose daemon was never read.
	read *legionBoardDaemonRead
	// admission is the daemon's admission block as the newest read recorded it, nil while no read
	// has recorded one: before the first read of a daemon, and for a daemon that sends none.
	admission *legion.Admission
	// settings and settingSources are the daemon's own two blocks as the newest read recorded
	// them, nil while no read has recorded either.
	settings, settingSources map[string]json.RawMessage
	// issues is every issue the daemon reported, by key, with what Dispatch holds of it beside the
	// observations.
	issues map[string]*legionBoardIssue
	// parents is the parent Dispatch records for every issue of the project, the chain the tree
	// walk climbs: an issue the daemon reports nothing of can still be the Legion issue above one
	// it does (treeRoot).
	parents map[string]string
	// phaseWork is the server's set of the issues that carry Legion phase work, the other half of
	// what makes an issue Legion's for the walk (legion_phase_work.go); nil is an empty set.
	phaseWork *legionPhaseWork
	// ranked is the project's issue keys in Dispatch's rank order, which is the Queue column's
	// order when no read has recorded a waiting line.
	ranked []string
}

// legionBoardDaemonRead is the newest legion_reads row: when the read was made, whether it
// succeeded, and why it did not.
type legionBoardDaemonRead struct {
	at      time.Time
	ok      bool
	failure *string
}

// legionBoardIssue is one issue the board may show: the newest observation of each field the
// sampler records, and the title, priority and parent Dispatch holds for the same key.
type legionBoardIssue struct {
	key string
	// phase and phaseSince are the newest phase observation and when it was recorded, which is
	// when the issue entered that phase (a row is written only where the value moved).
	phase      string
	phaseSince time.Time
	slot       *int
	// tree is the daemon's own `tree` for the issue, empty where it named none.
	tree          string
	waitingOn     *legionBoardWait
	checksVerdict string
	// roles are the roles of the issue whose claim state is `working`, sorted.
	roles []string
	// title and priority are Dispatch's, empty for an issue Dispatch does not hold; the parent
	// Dispatch records is p.parents, which holds every issue of the project and not only the
	// ones the daemon reports.
	title    string
	priority *int
}

// readLegionBoard reads one project's board. A project with no Legion setting is legion's
// ErrNoSettings, which the route answers 404 and the live hub refuses the subscription with.
func (s *server) readLegionBoard(ctx context.Context, project string) (*legionBoardPopulation, error) {
	pool := s.deps.Store.Pool
	if _, err := legion.GetSettings(ctx, pool, project); err != nil {
		return nil, err
	}
	population := &legionBoardPopulation{
		project: project, issues: map[string]*legionBoardIssue{}, parents: map[string]string{},
		phaseWork: &s.legionPhaseWork,
	}
	// The walk reads the issues that carry Legion phase work beside the daemon's own reports, and
	// the set is read once rather than per board (legion_phase_work.go).
	if err := s.legionPhaseWork.ensure(ctx, pool); err != nil {
		return nil, err
	}
	if err := population.readDaemonRead(ctx, pool); err != nil {
		return nil, err
	}
	if err := population.readObservations(ctx, pool); err != nil {
		return nil, err
	}
	if err := population.readIssues(ctx, pool); err != nil {
		return nil, err
	}
	return population, nil
}

// readDaemonRead reads the newest read of the project's daemon, the freshness the header shows.
func (p *legionBoardPopulation) readDaemonRead(ctx context.Context, pool *store.Pool) error {
	var read legionBoardDaemonRead
	err := pool.QueryRow(ctx, `
		select read_at, ok, error
		from legion_reads
		where project = $1
		order by read_at desc, id desc
		limit 1
	`, p.project).Scan(&read.at, &read.ok, &read.failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	p.read = &read
	return nil
}

// readObservations reads the newest value of every field the sampler recorded for the project: one
// row per issue and field, which the unique index (project, issue_key, field, observed_at) answers
// in order. The rows under legion.AdmissionKey belong to no issue: the admission block, the
// daemon's settings and where each setting came from.
func (p *legionBoardPopulation) readObservations(ctx context.Context, pool *store.Pool) error {
	rows, err := pool.Query(ctx, `
		select distinct on (issue_key, field) issue_key, field, value, observed_at
		from legion_observations
		where project = $1
		order by issue_key, field, observed_at desc
	`, p.project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, field string
		var value []byte
		var at time.Time
		if err := rows.Scan(&key, &field, &value, &at); err != nil {
			return err
		}
		if key == legion.AdmissionKey {
			p.observeProject(field, value)
			continue
		}
		issue := p.issues[key]
		if issue == nil {
			issue = &legionBoardIssue{key: key}
			p.issues[key] = issue
		}
		issue.observe(field, value, at)
	}
	return rows.Err()
}

// observeProject takes one of the project's own newest observations: a value of another shape
// than the field is read as leaves the field unrecorded, since the daemon owns the wire shape.
func (p *legionBoardPopulation) observeProject(field string, value []byte) {
	switch field {
	case legion.FieldAdmission:
		var admission legion.Admission
		if err := json.Unmarshal(value, &admission); err == nil {
			p.admission = &admission
		}
	case legion.FieldSettings:
		_ = json.Unmarshal(value, &p.settings)
	case legion.FieldSettingSources:
		_ = json.Unmarshal(value, &p.settingSources)
	}
}

// observe takes one newest observation into the issue. A value that is not the shape the field is
// read as leaves the field as it stands: the daemon owns the wire shape, and a reader of one names
// what it expects rather than trusting it.
func (issue *legionBoardIssue) observe(field string, value []byte, at time.Time) {
	switch {
	case field == legion.FieldPhase:
		issue.phase, issue.phaseSince = legion.JSONString(value), at
	case field == legion.FieldTree:
		issue.tree = legion.JSONString(value)
	case field == legion.FieldSlot:
		var slot *int
		if err := json.Unmarshal(value, &slot); err == nil {
			issue.slot = slot
		}
	case field == legion.FieldWaitingOn:
		var wait struct {
			Kind       string     `json:"kind"`
			Since      *time.Time `json:"since"`
			AnsweredAt *time.Time `json:"answeredAt"`
		}
		if err := json.Unmarshal(value, &wait); err == nil && wait.Kind != "" {
			issue.waitingOn = &legionBoardWait{Kind: wait.Kind, Since: wait.Since, AnsweredAt: wait.AnsweredAt}
		}
	case field == legion.FieldPullRequestChecksVerdict:
		issue.checksVerdict = legion.JSONString(value)
	case strings.HasPrefix(field, legion.ClaimFieldPrefix):
		role := strings.TrimPrefix(field, legion.ClaimFieldPrefix)
		working := legion.JSONString(value) == "working"
		held := slices.Contains(issue.roles, role)
		switch {
		case working && !held:
			issue.roles = append(issue.roles, role)
			slices.Sort(issue.roles)
		case !working && held:
			issue.roles = slices.DeleteFunc(issue.roles, func(r string) bool { return r == role })
		}
	}
}

// readIssues reads the project's issues in Dispatch's rank order, records the parent of every one
// of them — the chain the tree walk climbs, whatever the daemon reports of each — and fills the
// title, priority and parent of every issue the daemon reported. An issue the daemon reports that
// Dispatch does not hold keeps an empty title: the board shows what the daemon says, and a card
// with no title is better than a card the reader never sees.
func (p *legionBoardPopulation) readIssues(ctx context.Context, pool *store.Pool) error {
	rows, err := pool.Query(ctx, `
		select key, title, priority, coalesce(parent_key, '')
		from issues
		where project_key = $1
		order by rank asc, created_at asc, key asc
	`, p.project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, title, parent string
		var priority *int
		if err := rows.Scan(&key, &title, &priority, &parent); err != nil {
			return err
		}
		p.ranked = append(p.ranked, key)
		p.parents[key] = parent
		if issue := p.issues[key]; issue != nil {
			issue.title, issue.priority = title, priority
		}
	}
	return rows.Err()
}

// issueKeys is every issue the board's answer names, which is what the live hub marks its view
// dirty by (delivery_live_hub_intake.go's dirtyIssueLocked).
func (p *legionBoardPopulation) issueKeys() map[string]struct{} {
	keys := make(map[string]struct{}, len(p.issues))
	for key := range p.issues {
		keys[key] = struct{}{}
	}
	return keys
}

// treeRoot is the issue's tree root, by the one rule the metrics place an issue in a tree with
// (legion.TreeRoot): the `tree` the daemon named where a read carried one (dispatch://INFRA-980),
// and the topmost Legion issue of its parent chain otherwise.
//
// The walk knows every issue of the project (parents) and every issue the daemon reports, and an
// issue is Legion's for it by the one rule both readers state (dispatch://INFRA-1077's spec, "The
// top issue of a tree", and legion/metrics/population.go's isLegionIssue): the daemon reports it,
// or it carries Legion phase work. So a chain climbs through an issue that carries a handoff or a
// role session's event and that the daemon never reported, and stops below an issue that is
// neither, which is what keeps a tree out of the tracking issues above it. An issue of another
// project, which only a daemon that reports one brings here, is a root of its own.
func (p *legionBoardPopulation) treeRoot(key string) string {
	return legion.TreeRoot(key, p.treeNode)
}

// treeNode is what that walk reads of one issue: the daemon's `tree`, Dispatch's parent, and
// whether the issue is Legion's.
func (p *legionBoardPopulation) treeNode(key string) (legion.TreeNode, bool) {
	issue, reported := p.issues[key]
	parent, held := p.parents[key]
	if !reported && !held {
		return legion.TreeNode{}, false
	}
	node := legion.TreeNode{Parent: parent, Legion: reported || p.phaseWork.has(key)}
	if reported {
		node.Tree = issue.tree
	}
	return node, true
}

// legionBoardCards answers which issues carry a card and, for an issue that carries none, the
// nearest issue above it in its tree that does. The two answers are worked out together, since
// the one card that depends on the chain is an `admitted` issue holding no slot: it is an
// unstarted child where a card stands above it, and a card of its own where none does.
type legionBoardCards struct {
	population *legionBoardPopulation
	// queued is the issues the daemon's own state put in the Queue column, which carry a card
	// whatever their phase; carded keeps each answer once it is known.
	queued map[string]bool
	carded map[string]bool
}

// has is whether the issue carries a card.
func (c *legionBoardCards) has(key string) bool {
	if held, known := c.carded[key]; known {
		return held
	}
	// A chain that loops reads false for the issue being worked out, so the walk ends.
	c.carded[key] = false
	issue := c.population.issues[key]
	if issue == nil {
		return false
	}
	_, named := legionPhaseColumns[issue.phase]
	held := false
	switch {
	case c.queued[key]:
		held = true
	case !named:
		held = false
	case issue.phase == legion.PhaseAdmitted && issue.slot == nil:
		held = c.nearestCard(key) == ""
	default:
		held = true
	}
	c.carded[key] = held
	return held
}

// nearestCard is the nearest issue above key in its tree that carries a card, and "" where none
// does. It climbs the one chain the tree rule names (legion.TreeChain), so a count never leaves
// the tree the walk puts the issue in.
func (c *legionBoardCards) nearestCard(key string) string {
	chain := legion.TreeChain(key, c.population.treeNode)
	for _, above := range chain[1:] {
		if c.has(above) {
			return above
		}
	}
	return ""
}

// legionBoardCompute is GET /api/v1/legion/board's body for a population read for it. It reads
// nothing: the live hub answers the same body from the population it already holds, and now is
// only computed_at.
//
// A card's column is the first of these that holds: the daemon's waiting line, in its own order,
// which the view does not re-sort (the spec's "Rule details": that order puts answered roots
// first, then follows Dispatch's rank); the issues a blocker link holds, after them in the Queue
// column; and otherwise the column its phase falls in. An issue the daemon reports in `admitted`
// that holds no slot and that neither list names is a child its tree has not started: it is on no
// card, and the nearest card above it in its tree counts it. Where no issue above it carries a
// card — its tree climbs through issues the daemon reports nothing of, or through issues whose
// phase is on no column — it is a card of its own, since a count on an issue the board shows
// nothing of is a count no reader can see. Where no read has recorded an admission block the
// waiting line is Dispatch's rank order over the issues in that position, which is the order the
// daemon's own line follows once a root's wait is answered.
func legionBoardCompute(p *legionBoardPopulation, now time.Time) legionBoardResponse {
	cards := map[string][]legionBoardCard{}
	placed := map[string]bool{}
	blockers := map[string][]string{}
	var waiting []string
	if p.admission != nil {
		waiting = p.admission.Waiting
		for _, blocked := range p.admission.Blocked {
			blockers[blocked.Issue] = blocked.BlockedBy
		}
	}

	// The queue: the daemon's waiting line in its own order, then the issues a blocker link holds
	// that the line does not already name.
	queued := make([]string, 0, len(waiting)+len(blockers))
	seen := map[string]bool{}
	for _, key := range waiting {
		if p.issues[key] == nil || seen[key] {
			continue
		}
		seen[key] = true
		queued = append(queued, key)
	}
	blocked := make([]string, 0, len(blockers))
	for key := range blockers {
		if p.issues[key] != nil && !seen[key] {
			blocked = append(blocked, key)
		}
	}
	p.sortByRank(blocked)
	queued = append(queued, blocked...)
	// Which issues carry a card: the queue the daemon's state named, and the rest by phase. The
	// fallback queue below and the unstarted-child count both read it, so no card is lost to the
	// tree walk and no count lands on an issue the board shows nothing of.
	board := &legionBoardCards{population: p, queued: seen, carded: map[string]bool{}}
	if p.admission == nil {
		// No read has recorded a waiting line: the issues waiting for a slot that no card above
		// them holds, in Dispatch's rank order.
		for _, key := range p.ranked {
			issue := p.issues[key]
			if issue != nil && issue.phase == legion.PhaseAdmitted && issue.slot == nil && board.has(key) {
				queued = append(queued, key)
			}
		}
	}
	for _, key := range queued {
		placed[key] = true
	}

	// Every other issue the daemon reports, by its phase. An `admitted` issue holding no slot that
	// the queue does not name has not been started: it is counted on the nearest card above it,
	// and carries one of its own where there is none.
	uncarded := map[string]int{}
	for _, key := range p.sortedKeys() {
		issue := p.issues[key]
		if placed[key] {
			continue
		}
		column, named := legionPhaseColumns[issue.phase]
		if !named {
			continue
		}
		if issue.phase == legion.PhaseAdmitted && issue.slot == nil {
			if above := board.nearestCard(key); above != "" {
				uncarded[above]++
				continue
			}
		}
		placed[key] = true
		cards[column] = append(cards[column], p.card(issue, blockers[key], 0))
	}
	for _, key := range queued {
		cards[legionColumnQueue] = append(cards[legionColumnQueue], p.card(p.issues[key], blockers[key], uncarded[key]))
	}
	// A root's count is known only once every issue has been placed, so the cards already made
	// take theirs now.
	for column, made := range cards {
		if column == legionColumnQueue {
			continue
		}
		for index := range made {
			made[index].Children = uncarded[made[index].Key]
		}
	}

	columns := make([]legionBoardColumn, 0, len(legionBoardColumnOrder))
	for _, column := range legionBoardColumnOrder {
		made := cards[column.id]
		if made == nil {
			made = []legionBoardCard{}
		}
		columns = append(columns, legionBoardColumn{ID: column.id, Label: column.label, Cards: made})
	}

	header := legionBoardHeader{QueueLength: len(cards[legionColumnQueue])}
	// Slots in use are the daemon's own count where it reports one. dispatch://INFRA-980 makes
	// admission.active list every issue that holds a slot, children included, so it is the
	// authority; before that release the daemon lists roots there and only a root carries a slot,
	// so the two agree. A read that recorded no admission block falls back to the issues whose own
	// slot the sampler saw.
	if p.admission != nil && len(p.admission.Active) > 0 {
		header.SlotsInUse = len(p.admission.Active)
	} else {
		for _, issue := range p.issues {
			if issue.slot != nil {
				header.SlotsInUse++
			}
		}
	}
	if p.admission != nil {
		header.SlotCap = p.admission.Cap
		// Trees is the live trees' roots, which only a daemon on dispatch://INFRA-980 reports; it
		// is 0 for an older one, where the view shows no tree count rather than a wrong one.
		header.Trees = len(p.admission.Trees)
	}
	if read := p.read; read != nil {
		at := read.at.UTC()
		header.ReadAt, header.ReadOK, header.ReadError = &at, read.ok, read.failure
	}
	// Both blocks are objects, empty rather than null, so a page reads one shape whether or not a
	// read has recorded them.
	settings, sources := p.settings, p.settingSources
	if settings == nil {
		settings = map[string]json.RawMessage{}
	}
	if sources == nil {
		sources = map[string]json.RawMessage{}
	}
	return legionBoardResponse{
		Project: p.project, Header: header, Columns: columns,
		Settings: settings, SettingSources: sources, ComputedAt: now,
	}
}

// card is one issue's card, with the issues holding it and the children of its tree that are on no
// card of their own.
func (p *legionBoardPopulation) card(issue *legionBoardIssue, blockedBy []string, children int) legionBoardCard {
	card := legionBoardCard{
		Key: issue.key, Title: issue.title, Priority: issue.priority, Phase: issue.phase,
		Slot: issue.slot, Roles: issue.roles, BlockedBy: blockedBy, WaitingOn: issue.waitingOn,
		Tree: p.treeRoot(issue.key), Children: children,
	}
	if card.Roles == nil {
		card.Roles = []string{}
	}
	if card.BlockedBy == nil {
		card.BlockedBy = []string{}
	}
	if !issue.phaseSince.IsZero() {
		since := issue.phaseSince.UTC()
		card.PhaseSince = &since
	}
	if issue.checksVerdict != "" {
		verdict := issue.checksVerdict
		card.ChecksVerdict = &verdict
	}
	return card
}

// sortedKeys is every issue the daemon reported, in Dispatch's rank order, with the issues
// Dispatch does not hold after them by key, so one state always makes one board.
func (p *legionBoardPopulation) sortedKeys() []string {
	keys := slices.Collect(maps.Keys(p.issues))
	p.sortByRank(keys)
	return keys
}

// sortByRank orders keys by Dispatch's rank, the order readIssues read them in, and puts an issue
// Dispatch does not hold after the rest, by key.
func (p *legionBoardPopulation) sortByRank(keys []string) {
	rank := make(map[string]int, len(p.ranked))
	for index, key := range p.ranked {
		rank[key] = index
	}
	slices.SortFunc(keys, func(a, b string) int {
		ra, hasA := rank[a]
		rb, hasB := rank[b]
		switch {
		case hasA && hasB:
			return ra - rb
		case hasA:
			return -1
		case hasB:
			return 1
		default:
			return strings.Compare(a, b)
		}
	})
}
