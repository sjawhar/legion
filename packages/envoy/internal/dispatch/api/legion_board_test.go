package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// nameLegionDaemon names project's daemon, which is what brings the board.
func nameLegionDaemon(t *testing.T, handler http.Handler, project, stateURL string) {
	t.Helper()
	written := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/legion/"+project,
		map[string]any{"state_url": stateURL}, "alice")
	if written.Code != http.StatusOK {
		t.Fatalf("name %s's daemon: status=%d body=%s", project, written.Code, written.Body.String())
	}
}

// legionExec runs one seeding statement.
func legionExec(t *testing.T, database *store.Store, sql string, args ...any) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("seed (%s): %v", sql, err)
	}
}

// seedLegionIssue writes one Dispatch issue of the project, which is where a card's title,
// priority and parent come from (the plan's D13).
func seedLegionIssue(t *testing.T, database *store.Store, project string, number int, title, rank string, priority *int, parent string) string {
	t.Helper()
	key := fmt.Sprintf("%s-%d", project, number)
	var parentKey *string
	if parent != "" {
		parentKey = &parent
	}
	legionExec(t, database, `insert into issues (key, project_key, number, title, status, rank, priority, parent_key, created_by)
		values ($1, $2, $3, $4, 'in_progress', $5, $6, $7, '{"kind":"user","id":"alice"}'::jsonb)`,
		key, project, number, title, rank, priority, parentKey)
	return key
}

// seedLegionObservation writes one observation as the sampler would have recorded it.
func seedLegionObservation(t *testing.T, database *store.Store, project, issue, field, value string, at time.Time) {
	t.Helper()
	legionExec(t, database, `insert into legion_observations (project, issue_key, field, value, observed_at)
		values ($1, $2, $3, $4::jsonb, $5)`, project, issue, field, value, at)
}

// seedLegionRead writes one read of the project's daemon, the freshness the header shows.
func seedLegionRead(t *testing.T, database *store.Store, project string, at time.Time, failure string) {
	t.Helper()
	var message *string
	if failure != "" {
		message = &failure
	}
	legionExec(t, database, `insert into legion_reads (project, read_at, ok, error, bytes)
		values ($1, $2, $3, $4, 1024)`, project, at, failure == "", message)
}

// seedLegionBoard writes one project's board: a waiting line whose first root was answered, an
// issue a blocker link holds, a root in design with an unstarted child, a child in a turn, a root
// waiting on a person, a held issue, and an issue that finished. It is the state one read of a
// daemon would have recorded, written as the sampler records it.
func seedLegionBoard(t *testing.T, database *store.Store, project string, now time.Time) {
	t.Helper()
	ago := func(hours float64) time.Time { return now.Add(-time.Duration(hours * float64(time.Hour))) }
	p1 := 1
	p0 := 0
	keys := []struct {
		number        int
		title, rank   string
		priority      *int
		parent, phase string
	}{
		{10, "The answered root", "a", &p0, "", "awaiting_person"},
		{1, "The queued root", "b", &p1, "", "admitted"},
		{2, "The blocked root", "c", nil, "", "admitted"},
		{3, "The design root", "d", &p1, "", "admitted"},
		{4, "The implementer's child", "e", nil, project + "-3", "implementing"},
		{5, "The person's root", "f", nil, "", "awaiting_person"},
		{6, "The held issue", "g", nil, "", "held"},
		{7, "The finished issue", "h", nil, "", "done"},
		{8, "The unstarted child", "i", nil, project + "-3", "admitted"},
	}
	for _, issue := range keys {
		key := seedLegionIssue(t, database, project, issue.number, issue.title, issue.rank, issue.priority, issue.parent)
		seedLegionObservation(t, database, project, key, legion.FieldPhase, `"`+issue.phase+`"`, ago(3))
	}
	// The two issues holding a slot, one of them a child: nothing takes a slot holder for a root.
	seedLegionObservation(t, database, project, project+"-3", legion.FieldSlot, `0`, ago(5))
	seedLegionObservation(t, database, project, project+"-4", legion.FieldSlot, `1`, ago(4))
	// The implementer is in a turn; its reviewer's claim ended.
	seedLegionObservation(t, database, project, project+"-4", legion.ClaimFieldPrefix+"implementer", `"working"`, ago(2))
	seedLegionObservation(t, database, project, project+"-4", legion.ClaimFieldPrefix+"reviewer", `"idle"`, ago(2))
	seedLegionObservation(t, database, project, project+"-4", legion.FieldPullRequestChecksVerdict, `"failure"`, ago(1))
	// What each root in awaiting_person waits on: one answered, which is why the daemon's line
	// holds it first, and one still with the person.
	seedLegionObservation(t, database, project, project+"-10", legion.FieldWaitingOn,
		`{"kind":"ask","since":"2026-11-03T09:00:00Z","answeredAt":"2026-11-03T10:00:00Z"}`, ago(6))
	seedLegionObservation(t, database, project, project+"-5", legion.FieldWaitingOn,
		`{"kind":"gate","since":"2026-11-03T08:00:00Z"}`, ago(6))
	seedLegionObservation(t, database, project, legion.AdmissionKey, legion.FieldAdmission, fmt.Sprintf(`{
		"cap": 4,
		"active": [%q, %q],
		"waiting": [%q, %q],
		"blocked": [{"issue": %q, "blockedBy": [%q]}]
	}`, project+"-3", project+"-4", project+"-10", project+"-1", project+"-2", project+"-99"), ago(3))
	seedLegionRead(t, database, project, ago(0.01), "")
}

// legionBoardOf is the board the route answers for project.
func legionBoardOf(t *testing.T, handler http.Handler, project string) legionBoardResponse {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/legion/board?project="+project, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET the board of %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
	return decodeBody[legionBoardResponse](t, response)
}

// legionColumnKeys is the keys of one column's cards, in the board's own order.
func legionColumnKeys(t *testing.T, board legionBoardResponse, id string) []string {
	t.Helper()
	for _, column := range board.Columns {
		if column.ID != id {
			continue
		}
		keys := make([]string, len(column.Cards))
		for index, card := range column.Cards {
			keys[index] = card.Key
		}
		return keys
	}
	t.Fatalf("the board has no column %q", id)
	return nil
}

// legionCard is the card for key, wherever it is.
func legionCard(t *testing.T, board legionBoardResponse, key string) legionBoardCard {
	t.Helper()
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			if card.Key == key {
				return card
			}
		}
	}
	t.Fatalf("the board holds no card for %s", key)
	return legionBoardCard{}
}

// TestTheLegionBoardPutsEachIssueInOneColumn: the Queue column is the daemon's own waiting line,
// in its order, then the issues a blocker link holds; every other issue the daemon reports is in
// the column its phase names; an issue the tree has not started is on no card and is counted on
// its root's; and the header carries the slots in use, the cap, the queue's length and the last
// read.
func TestTheLegionBoardPutsEachIssueInOneColumn(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	seedLegionBoard(t, database, "INFRA", now)

	board := legionBoardOf(t, handler, "INFRA")
	if board.Project != "INFRA" {
		t.Fatalf("the board names project %q, want INFRA", board.Project)
	}
	if len(board.Columns) != len(legionBoardColumnOrder) {
		t.Fatalf("the board has %d columns, want the %d the view draws", len(board.Columns), len(legionBoardColumnOrder))
	}
	for index, column := range board.Columns {
		want := legionBoardColumnOrder[index]
		if column.ID != want.id || column.Label != want.label {
			t.Fatalf("column %d is %q/%q, want %q/%q", index, column.ID, column.Label, want.id, want.label)
		}
	}
	// The waiting line's own order, which the view does not re-sort: the answered root first,
	// though Dispatch's rank would put the queued root there, and the blocked one after both.
	if got := legionColumnKeys(t, board, legionColumnQueue); !slices.Equal(got, []string{"INFRA-10", "INFRA-1", "INFRA-2"}) {
		t.Errorf("the queue holds %v, want the waiting line then the blocked issue", got)
	}
	for id, want := range map[string][]string{
		legionColumnDesign:       {"INFRA-3"},
		legionColumnImplementing: {"INFRA-4"},
		legionColumnAwaiting:     {"INFRA-5"},
		legionColumnHeld:         {"INFRA-6"},
		legionColumnPlanning:     {},
		legionColumnTesting:      {},
		legionColumnReviewing:    {},
		legionColumnAccepting:    {},
		legionColumnMerging:      {},
	} {
		if got := legionColumnKeys(t, board, id); !slices.Equal(got, want) {
			t.Errorf("the %s column holds %v, want %v", id, got, want)
		}
	}
	// The finished issue and the child its tree has not started are on no card; the root counts
	// the child.
	for _, gone := range []string{"INFRA-7", "INFRA-8"} {
		for _, column := range board.Columns {
			if slices.ContainsFunc(column.Cards, func(card legionBoardCard) bool { return card.Key == gone }) {
				t.Errorf("%s has a card of its own, in %s", gone, column.ID)
			}
		}
	}
	design := legionCard(t, board, "INFRA-3")
	if design.Children != 1 {
		t.Errorf("the design root counts %d children its tree has not started, want the one", design.Children)
	}
	if design.Slot == nil || *design.Slot != 0 || design.Title != "The design root" || design.Priority == nil || *design.Priority != 1 {
		t.Errorf("the design root's card = %+v, want slot 0 with Dispatch's title and priority", design)
	}
	child := legionCard(t, board, "INFRA-4")
	if !slices.Equal(child.Roles, []string{"implementer"}) {
		t.Errorf("the child's roles in a turn = %v, want the implementer alone", child.Roles)
	}
	if child.ChecksVerdict == nil || *child.ChecksVerdict != "failure" {
		t.Errorf("the child's CI verdict = %v, want failure", child.ChecksVerdict)
	}
	if child.Tree != "INFRA-3" || child.Slot == nil || *child.Slot != 1 {
		t.Errorf("the child's card = %+v, want slot 1 on its root's tree", child)
	}
	if child.PhaseSince == nil || child.PhaseSince.After(now) {
		t.Errorf("the child's phase began %v, want the moment its phase was recorded", child.PhaseSince)
	}
	blocked := legionCard(t, board, "INFRA-2")
	if !slices.Equal(blocked.BlockedBy, []string{"INFRA-99"}) {
		t.Errorf("the blocked card names %v, want the issue holding it", blocked.BlockedBy)
	}
	person := legionCard(t, board, "INFRA-5")
	if person.WaitingOn == nil || person.WaitingOn.Kind != "gate" || person.WaitingOn.Since == nil || person.WaitingOn.AnsweredAt != nil {
		t.Errorf("the waiting root waits on %+v, want an unanswered gate with its since", person.WaitingOn)
	}
	if answered := legionCard(t, board, "INFRA-10"); answered.WaitingOn == nil || answered.WaitingOn.AnsweredAt == nil {
		t.Errorf("the answered root's wait = %+v, want the moment a person answered it", answered.WaitingOn)
	}
	header := board.Header
	if header.SlotsInUse != 2 || header.SlotCap == nil || *header.SlotCap != 4 || header.QueueLength != 3 {
		t.Errorf("the header = %+v, want 2 slots of 4 in use and a queue of 3", header)
	}
	if !header.ReadOK || header.ReadAt == nil || header.ReadError != nil {
		t.Errorf("the header's read = %+v, want the successful read the sampler recorded", header)
	}
}

// TestTheMergingColumnHoldsEveryPhaseTheMergerWorks: the merger's phases are one declaration
// (legion.MergerPhases), which the board's column map reads rather than spelling them again, so
// an issue in any of the three is on the Merging column's cards and on no other.
func TestTheMergingColumnHoldsEveryPhaseTheMergerWorks(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	var want []string
	for index, phase := range legion.MergerPhases {
		key := seedLegionIssue(t, database, "INFRA", index+1, "An issue the merger works", string(rune('a'+index)), nil, "")
		seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"`+phase+`"`, now.Add(-time.Hour))
		want = append(want, key)
	}
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	if got := legionColumnKeys(t, board, legionColumnMerging); !slices.Equal(got, want) {
		t.Errorf("the Merging column holds %v, want the issue of each of %v", got, legion.MergerPhases)
	}
}

// TestTheLegionBoardQueuesByRankUntilADaemonRecordsItsWaitingLine: before any read recorded an
// admission block — every read a daemon before this release answered — the Queue column is the
// roots waiting for a slot in Dispatch's rank order, which is the order the daemon's own line
// follows once no root's wait has been answered.
func TestTheLegionBoardQueuesByRankUntilADaemonRecordsItsWaitingLine(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	seedLegionIssue(t, database, "INFRA", 1, "Second by rank", "b", nil, "")
	seedLegionIssue(t, database, "INFRA", 2, "First by rank", "a", nil, "")
	seedLegionIssue(t, database, "INFRA", 3, "A child of the first", "c", nil, "INFRA-2")
	for _, key := range []string{"INFRA-1", "INFRA-2", "INFRA-3"} {
		seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"admitted"`, now.Add(-time.Hour))
	}
	seedLegionRead(t, database, "INFRA", now, "")

	board := legionBoardOf(t, handler, "INFRA")
	if got := legionColumnKeys(t, board, legionColumnQueue); !slices.Equal(got, []string{"INFRA-2", "INFRA-1"}) {
		t.Fatalf("the queue holds %v, want the two roots in Dispatch's rank order", got)
	}
	if board.Header.SlotCap != nil {
		t.Errorf("the header names a cap of %d, want none until a read records the admission block", *board.Header.SlotCap)
	}
	if root := legionCard(t, board, "INFRA-2"); root.Children != 1 {
		t.Errorf("the first root counts %d unstarted children, want the one", root.Children)
	}
}

// newAdmissionShapeState is a state document as a daemon on dispatch://INFRA-980 serves it: every
// issue holding a slot in admission.active, children included; waiting ordered by tree root; a
// blocked parent waiting for its children; a new admission.trees; a new per-issue tree; and a
// higher contract version.
const newAdmissionShapeState = `{
  "daemon": {"project": "INFRA", "contract": 47, "schema": 212},
  "admission": {
    "cap": 6,
    "active": ["INFRA-1", "INFRA-2"],
    "waiting": ["INFRA-3"],
    "trees": ["INFRA-1"],
    "blocked": [{"issue": "INFRA-4", "blockedBy": ["INFRA-2"], "waitingForChildren": true}]
  },
  "issues": {
    "INFRA-1": {"key": "INFRA-1", "phase": "admitted", "status": "in_progress", "tree": "INFRA-1",
      "slot": {"index": 0, "admittedAt": "2026-11-03T10:00:00Z"}},
    "INFRA-2": {"key": "INFRA-2", "phase": "implementing", "status": "in_progress", "tree": "INFRA-1",
      "slot": {"index": 1, "admittedAt": "2026-11-03T10:05:00Z"},
      "workers": {"implementer": {"claim": {"session": "s-1", "state": "working"}, "rounds": 1}},
      "pullRequest": {"state": "open", "checksVerdict": "pending", "requiresChecks": true}},
    "INFRA-3": {"key": "INFRA-3", "phase": "admitted", "status": "todo", "tree": "INFRA-3"},
    "INFRA-4": {"key": "INFRA-4", "phase": "admitted", "status": "todo", "tree": "INFRA-1"}
  },
  "settings": {"admission_cap": 6, "review_round_cap": 3, "gates": {"design": true}},
  "settingSources": {
    "admission_cap": {"source": "route", "by": "alice", "at": "2026-11-03T12:00:00Z"},
    "review_round_cap": {"source": "file", "at": "2026-11-03T06:00:00Z"}
  }
}`

// TestTheLegionBoardReadsADaemonOnTheNewAdmissionShape: the board is built from what the loose
// decode records of a daemon that has rolled onto dispatch://INFRA-980, so a child holding a slot
// is a card of its own on its root's tree and no slot holder is taken for a root.
func TestTheLegionBoardReadsADaemonOnTheNewAdmissionShape(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	// The root first: its children name it as their parent, which the issues table holds them to.
	for _, issue := range []struct {
		number      int
		title, rank string
		parent      string
	}{
		{1, "The root", "a", ""},
		{2, "The child in a turn", "b", "INFRA-1"},
		{3, "The waiting root", "c", ""},
		{4, "The blocked child", "d", "INFRA-1"},
	} {
		seedLegionIssue(t, database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
	}
	var state legion.State
	if err := json.Unmarshal([]byte(newAdmissionShapeState), &state); err != nil {
		t.Fatalf("decode the new state shape: %v", err)
	}
	if _, err := legion.RecordObservations(t.Context(), database.Pool, "INFRA", now.Add(-time.Hour), state.Observations()); err != nil {
		t.Fatalf("record the new state shape: %v", err)
	}
	seedLegionRead(t, database, "INFRA", now, "")

	board := legionBoardOf(t, handler, "INFRA")
	if got := legionColumnKeys(t, board, legionColumnQueue); !slices.Equal(got, []string{"INFRA-3", "INFRA-4"}) {
		t.Errorf("the queue holds %v, want the waiting root then the blocked child", got)
	}
	if got := legionColumnKeys(t, board, legionColumnDesign); !slices.Equal(got, []string{"INFRA-1"}) {
		t.Errorf("the design column holds %v, want the root holding a slot", got)
	}
	child := legionCard(t, board, "INFRA-2")
	if child.Tree != "INFRA-1" || child.Slot == nil || *child.Slot != 1 {
		t.Errorf("the child's card = %+v, want the slot it holds on the tree the daemon named", child)
	}
	if !slices.Equal(child.Roles, []string{"implementer"}) || child.ChecksVerdict == nil || *child.ChecksVerdict != "pending" {
		t.Errorf("the child's card = %+v, want the implementer in a turn and the pending verdict", child)
	}
	if header := board.Header; header.SlotsInUse != 2 || header.SlotCap == nil || *header.SlotCap != 6 {
		t.Errorf("the header = %+v, want the two slots the issues hold of the cap the daemon named", header)
	}
	if blocked := legionCard(t, board, "INFRA-4"); !slices.Equal(blocked.BlockedBy, []string{"INFRA-2"}) {
		t.Errorf("the blocked child names %v, want the issue the daemon says holds it", blocked.BlockedBy)
	}
	// The settings the daemon reports, with where each came from: Dispatch models none of their
	// values, so each is read here as a reader of one expects it.
	var admissionCap int
	if err := json.Unmarshal(board.Settings["admission_cap"], &admissionCap); err != nil || admissionCap != 6 {
		t.Errorf("the board's admission_cap setting = %s (%v), want the 6 the daemon reports", board.Settings["admission_cap"], err)
	}
	var gates map[string]bool
	if err := json.Unmarshal(board.Settings["gates"], &gates); err != nil || !gates["design"] {
		t.Errorf("the board's gates setting = %s (%v), want the object the daemon sent", board.Settings["gates"], err)
	}
	var source struct {
		Source string `json:"source"`
		By     string `json:"by"`
		At     string `json:"at"`
	}
	if err := json.Unmarshal(board.SettingSources["admission_cap"], &source); err != nil {
		t.Fatalf("decode admission_cap's source: %v", err)
	}
	if source.Source != "route" || source.By != "alice" || source.At != "2026-11-03T12:00:00Z" {
		t.Errorf("admission_cap came from %+v, want the route, alice and the moment the daemon named", source)
	}
}

// TestTheLegionBoardOfADaemonThatNeverAnsweredIsEmptyWithItsFailure: a project whose daemon has a
// setting but has never answered shows the board with its columns empty and the read's failure in
// the header (the plan's A4 case).
func TestTheLegionBoardOfADaemonThatNeverAnsweredIsEmptyWithItsFailure(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	seedLegionIssue(t, database, "INFRA", 1, "Nothing has been read of it", "a", nil, "")
	seedLegionRead(t, database, "INFRA", now, "the Legion daemon could not be read: dial tcp: connection refused")

	board := legionBoardOf(t, handler, "INFRA")
	for _, column := range board.Columns {
		if len(column.Cards) != 0 {
			t.Errorf("the %s column holds %d cards, want none: nothing has been read", column.ID, len(column.Cards))
		}
	}
	header := board.Header
	if header.ReadOK || header.ReadError == nil || header.ReadAt == nil {
		t.Fatalf("the header = %+v, want the failed read it names", header)
	}
	if header.SlotsInUse != 0 || header.SlotCap != nil || header.QueueLength != 0 {
		t.Errorf("the header = %+v, want no slot, no cap and an empty queue", header)
	}
	if board.Settings == nil || len(board.Settings) != 0 || board.SettingSources == nil || len(board.SettingSources) != 0 {
		t.Errorf("the board's settings = %v and sources = %v, want two empty objects rather than null",
			board.Settings, board.SettingSources)
	}
}

// TestTheLegionBoardOfAProjectWithNoDaemonIsRefused: a project that names no daemon has no board,
// which is what shows no view.
func TestTheLegionBoardOfAProjectWithNoDaemonIsRefused(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA", "LINUX")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/legion/board?project=LINUX", nil, "alice")
	if response.Code != http.StatusNotFound {
		t.Fatalf("the board of a project with no daemon: status=%d body=%s", response.Code, response.Body.String())
	}
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	if refusal.Code != "LEGION_NOT_CONFIGURED" {
		t.Fatalf("the refusal's code = %q, want LEGION_NOT_CONFIGURED", refusal.Code)
	}
}

// TestEachProjectsLegionBoardIsItsOwn: two projects, each with a daemon of its own, are answered
// their own boards (the spec's A4).
func TestEachProjectsLegionBoardIsItsOwn(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA", "LINUX")
	now := time.Now().UTC()
	for _, project := range []string{"INFRA", "LINUX"} {
		nameLegionDaemon(t, handler, project, "http://legion-"+project+".internal:13370")
		seedLegionBoard(t, database, project, now)
	}

	for _, project := range []string{"INFRA", "LINUX"} {
		board := legionBoardOf(t, handler, project)
		for _, column := range board.Columns {
			for _, card := range column.Cards {
				if key := card.Key; len(key) < len(project) || key[:len(project)] != project {
					t.Fatalf("%s's board holds %s", project, key)
				}
			}
		}
		if board.Header.QueueLength != 3 {
			t.Errorf("%s's queue is %d long, want its own three", project, board.Header.QueueLength)
		}
	}
}

// TestTheBoardAndTheMetricsPutAnIssueInTheSameTree: the view and the four metrics read one
// tree-root rule (legion.TreeRoot), so no issue is a child on the board and a root on a card.
// The chain here is 40 deep, longer than the depth cap the board's own walk once stopped at, and
// it hangs under a tracking issue that carries no Legion work, which the metrics' population
// holds and the daemon never reported.
func TestTheBoardAndTheMetricsPutAnIssueInTheSameTree(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	// The tracking issue above the tree: Dispatch holds it, the daemon reports nothing of it.
	tracking := seedLegionIssue(t, database, "INFRA", 900, "The tracking issue", "a", nil, "")
	parent := tracking
	for number := 901; number <= 940; number++ {
		key := seedLegionIssue(t, database, "INFRA", number, "A deep issue", fmt.Sprintf("b%02d", number-900), nil, parent)
		seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"implementing"`, now.Add(-time.Hour))
		seedLegionObservation(t, database, "INFRA", key, legion.ClaimFieldPrefix+"implementer", `"working"`, now.Add(-time.Hour))
		parent = key
	}
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	cards := 0
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			cards++
			if root := population.TreeRoot(card.Key); root != card.Tree {
				t.Errorf("%s is in tree %q on the board and %q in the metrics", card.Key, card.Tree, root)
			}
		}
	}
	if cards != 40 {
		t.Fatalf("the board shows %d cards, want the 40 issues the daemon reported", cards)
	}
	if root := population.TreeRoot("INFRA-940"); root != "INFRA-901" {
		t.Errorf("the deepest issue's tree root is %s, want INFRA-901 under the tracking issue", root)
	}
}

// closeLegionIssue closes one seeded issue, which is what brings its whole parent chain into the
// metrics' read scope (legion/metrics/population.go's scopeKeys: the issues that could have
// finished in the window and their ancestors).
func closeLegionIssue(t *testing.T, database *store.Store, key string, at time.Time) {
	t.Helper()
	legionExec(t, database, `update issues set status = 'done', closed_at = $2 where key = $1`, key, at)
}

// seedLegionRoleEvent writes one event of a session titled `Legion <role> · <titled>` on the
// issue. It is Legion phase work with no observation behind it where titled is the issue's own
// key; where it names another issue the event is a Legion role writing on an issue it does not
// work — a tracking issue it reports under — which is no phase work of this issue at all.
func seedLegionRoleEvent(t *testing.T, database *store.Store, issue, session, role, titled string, at time.Time) {
	t.Helper()
	seedSessionEvent(t, database, issue, session, "Legion "+role+" · "+titled, at)
}

// seedSessionEvent writes one event of the session on the issue under title, the way Dispatch
// learns a session's title: the actor's origin carries it, and the newest of them is the
// session's title (delivery/agents.go's ResolveSessionTitles). A title of no Legion shape is an
// ordinary session's, which the phase-work rule reads and finds no role in.
func seedSessionEvent(t *testing.T, database *store.Store, issue, session, title string, at time.Time) {
	t.Helper()
	legionExec(t, database, `insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		select $1, coalesce(max(seq), 0) + 1, 'message.created',
			jsonb_build_object('kind', 'session', 'id', $2::text,
				'origin', jsonb_build_object('session_title', $3::text)),
			'{}', false, $4
		from events where issue_key = $1`, issue, session, title, at)
}

// seedLegionHandoff records one handoff of the issue, the other kind of Legion phase work
// (legion/metrics/population.go's readHandoffs).
func seedLegionHandoff(t *testing.T, database *store.Store, issue, phase string, at time.Time) {
	t.Helper()
	legionExec(t, database, `insert into handoffs (issue_key, phase, version, body, author, created_at)
		values ($1, $2, 1, '{}', '{"kind":"session","id":"s-handoff"}'::jsonb, $3)`, issue, phase, at)
}

// TestTheBoardClimbsThroughAnIssueWithOnlyPhaseWork: the board and the metrics read one rule for
// what a Legion issue is (dispatch://INFRA-1077's spec, "The top issue of a tree") — the daemon
// reports it, or it carries Legion phase work — so a chain climbs through an issue the daemon
// never reported that carries a handoff, and through one that carries an event of a session
// titled `Legion <role> · <its key>`. Such an issue is an input to the walk and nothing else: it
// gets no card, and the board names no issue it did not name before.
func TestTheBoardClimbsThroughAnIssueWithOnlyPhaseWork(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	// Two chains, each a reported ancestor over a middle issue the daemon reports nothing of
	// over a reported child; under each child a closed issue, so the metrics' window reaches the
	// whole chain. The first middle carries a handoff, the second a Legion role session's event.
	for _, chain := range []struct{ ancestor, middle, card, closed int }{{800, 801, 802, 803}, {810, 811, 812, 813}} {
		for _, issue := range []struct {
			number int
			parent string
		}{
			{chain.ancestor, ""},
			{chain.middle, fmt.Sprintf("INFRA-%d", chain.ancestor)},
			{chain.card, fmt.Sprintf("INFRA-%d", chain.middle)},
			{chain.closed, fmt.Sprintf("INFRA-%d", chain.card)},
		} {
			key := seedLegionIssue(t, database, "INFRA", issue.number, fmt.Sprintf("Issue %d", issue.number),
				fmt.Sprintf("b%03d", issue.number), nil, issue.parent)
			if issue.number == chain.ancestor || issue.number == chain.card {
				seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"implementing"`, hourAgo)
			}
		}
		closeLegionIssue(t, database, fmt.Sprintf("INFRA-%d", chain.closed), hourAgo)
	}
	seedLegionHandoff(t, database, "INFRA-801", "implement", hourAgo)
	seedLegionRoleEvent(t, database, "INFRA-811", "s-811", "tester", "INFRA-811", hourAgo)
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	cards := map[string]legionBoardCard{}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			cards[card.Key] = card
		}
	}
	named := slices.Sorted(maps.Keys(cards))
	want := []string{"INFRA-800", "INFRA-802", "INFRA-810", "INFRA-812"}
	if !slices.Equal(named, want) {
		t.Fatalf("the board's cards are %v, want the four issues the daemon reports, %v", named, want)
	}
	for _, chain := range [][2]string{{"INFRA-802", "INFRA-800"}, {"INFRA-812", "INFRA-810"}} {
		card, root := chain[0], chain[1]
		if tree := cards[card].Tree; tree != root {
			t.Errorf("the board puts %s in tree %q, want %q, its ancestor above an issue with only phase work", card, tree, root)
		}
		if got := population.TreeRoot(card); got != root {
			t.Errorf("the metrics put %s in tree %q, want %q", card, got, root)
		}
	}
	for _, middle := range [][2]string{{"INFRA-801", "INFRA-800"}, {"INFRA-811", "INFRA-810"}} {
		if got := population.TreeRoot(middle[0]); got != middle[1] {
			t.Errorf("the metrics put the middle issue %s in tree %q, want %q", middle[0], got, middle[1])
		}
	}
	for key, card := range cards {
		if root := population.TreeRoot(key); root != card.Tree {
			t.Errorf("%s is in tree %q on the board and %q in the metrics", key, card.Tree, root)
		}
	}
}

// TestTheBoardAndTheMetricsStopBelowAnIssueThatIsNoLegionIssue: the walk stops below the first
// issue above it that is neither reported by the daemon nor carrying Legion phase work
// (dispatch://INFRA-1077's spec, "The top issue of a tree"), rather than passing over it to the
// Legion issue above. One chain, bottom to top: a closed issue, so the metrics' window reaches
// the chain; a bottom the daemon reports; a middle in neither half of the rule, which an event
// of a session titled for another issue does not put in either; and a top the daemon reports.
// Both readers answer the bottom its own root, and the top roots nothing below the gap.
func TestTheBoardAndTheMetricsStopBelowAnIssueThatIsNoLegionIssue(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	for _, issue := range []struct {
		number   int
		title    string
		rank     string
		parent   string
		reported bool
	}{
		{840, "The top, which the daemon reports", "a", "", true},
		{841, "The tracking issue in neither half of the rule", "b", "INFRA-840", false},
		{842, "The bottom, which the daemon reports", "c", "INFRA-841", true},
		{843, "Its closed child, which brings the chain into the window", "d", "INFRA-842", false},
	} {
		key := seedLegionIssue(t, database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
		if issue.reported {
			seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"implementing"`, hourAgo)
		}
	}
	// The middle carries an event of a Legion role, titled for the issue that role works and
	// not for this one: a tracking issue a worker reports under is written on by Legion
	// sessions and carries no phase work of its own.
	seedLegionRoleEvent(t, database, "INFRA-841", "s-842", "implementer", "INFRA-842", hourAgo)
	closeLegionIssue(t, database, "INFRA-843", hourAgo)
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	if root := population.TreeRoot("INFRA-842"); root != "INFRA-842" {
		t.Errorf("the metrics put INFRA-842 in tree %q, want INFRA-842: the issue above it is no Legion issue", root)
	}
	if root := population.TreeRoot("INFRA-840"); root != "INFRA-840" {
		t.Errorf("the metrics put INFRA-840 in tree %q, want itself", root)
	}
	cards := map[string]legionBoardCard{}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			cards[card.Key] = card
		}
	}
	if tree := cards["INFRA-842"].Tree; tree != "INFRA-842" {
		t.Errorf("the board puts INFRA-842 in tree %q, want INFRA-842, as the metrics do", tree)
	}
	for key, card := range cards {
		if key != "INFRA-840" && card.Tree == "INFRA-840" {
			t.Errorf("the board roots %s at INFRA-840, which is above an issue that is no Legion issue", key)
		}
		if root := population.TreeRoot(key); root != card.Tree {
			t.Errorf("%s is in tree %q on the board and %q in the metrics", key, card.Tree, root)
		}
	}
}

// TestTheLiveBoardReadsPhaseWorkAsTheOneReadDoesAfterARename: the issues that carry Legion phase
// work are one read (legion.PhaseWorkKeys), which takes each session's newest title the one way
// Dispatch resolves a title (delivery.ResolveSessionTitles). A session renamed after it wrote is
// where a second rule built from each event's own title answers differently: it kept the issue
// the session wrote on under the title it wrote under, while the one read no longer names it at
// all. The live board, whose set the event feed keeps, and the route's own read must still place
// the issue below it in the same tree as the metrics.
func TestTheLiveBoardReadsPhaseWorkAsTheOneReadDoesAfterARename(t *testing.T) {
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	for _, issue := range []struct {
		number   int
		title    string
		rank     string
		parent   string
		reported bool
	}{
		{860, "The top, which the daemon reports", "a", "", true},
		{861, "The tracking issue the renamed session wrote on", "b", "INFRA-860", false},
		{862, "The bottom, which the daemon reports", "c", "INFRA-861", true},
		{863, "Its closed child, which brings the chain into the window", "d", "INFRA-862", false},
	} {
		key := seedLegionIssue(t, rig.database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
		if issue.reported {
			seedLegionObservation(t, rig.database, "INFRA", key, legion.FieldPhase, `"implementing"`, hourAgo)
		}
	}
	closeLegionIssue(t, rig.database, "INFRA-863", hourAgo)
	seedLegionRead(t, rig.database, "INFRA", now.Add(-time.Minute), "")

	// One session, two events: it wrote on the tracking issue under that issue's own title, and
	// was renamed to the issue it went on to work. Its newest title is the only title Dispatch
	// resolves, so no issue carries phase work by the first event any more.
	seedLegionRoleEvent(t, rig.database, "INFRA-861", "s-860", "implementer", "INFRA-861", hourAgo)
	seedLegionRoleEvent(t, rig.database, "INFRA-862", "s-860", "implementer", "INFRA-862", now.Add(-30*time.Minute))
	var newest int64
	if err := rig.database.Pool.QueryRow(t.Context(), `select max(id) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the newest event: %v", err)
	}
	start := rig.mark()
	notifyChannel(t, rig.database, EventsChannel, "")
	rig.waitFor(t, start, eventFeedWait, "the catch-up over the two events", func(note deliveryHubNote) bool {
		return note.what == "events caught up" && note.through >= newest
	})

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("INFRA")))
	snapshot := conn.snapshot(t, 5*time.Second, "b")
	var live legionBoardResponse
	if err := json.Unmarshal(snapshot.Body, &live); err != nil {
		t.Fatalf("read the live board: %v", err)
	}
	population, err := metrics.Read(t.Context(), rig.database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	if root := population.TreeRoot("INFRA-862"); root != "INFRA-862" {
		t.Errorf("the metrics put INFRA-862 in tree %q, want INFRA-862: no session titled for INFRA-861 wrote on it", root)
	}
	if tree := legionCard(t, live, "INFRA-862").Tree; tree != "INFRA-862" {
		t.Errorf("the live board puts INFRA-862 in tree %q, want INFRA-862, as the metrics do", tree)
	}
	requireSameBody(t, "the board the event feed kept", snapshot.Body,
		getBody(t, rig.handler, windowTarget(t, legionBoardView, snapshot.Body, "")))
}

// seedPhaseWorkChain seeds three issues of INFRA, top to bottom: a top the daemon reports, a
// middle it reports nothing of, and a bottom it reports. The walk stops below the middle while
// the middle is no Legion issue, and climbs to the top the moment it carries phase work, so the
// bottom's tree is what a board read says of the phase-work set.
func seedPhaseWorkChain(t *testing.T, rig *hubRig, first int, at time.Time) (top, middle, bottom string) {
	t.Helper()
	top = seedLegionIssue(t, rig.database, "INFRA", first, "The top, which the daemon reports", "a", nil, "")
	middle = seedLegionIssue(t, rig.database, "INFRA", first+1, "The tracking issue in neither half of the rule", "b", nil, top)
	bottom = seedLegionIssue(t, rig.database, "INFRA", first+2, "The bottom, which the daemon reports", "c", nil, middle)
	for _, key := range []string{top, bottom} {
		seedLegionObservation(t, rig.database, "INFRA", key, legion.FieldPhase, `"implementing"`, at)
	}
	seedLegionRead(t, rig.database, "INFRA", at, "")
	return top, middle, bottom
}

// legionBoardOnTheHub is the board the hub's own server reads, the one whose phase-work set the
// event feed keeps: the rig builds the hub on a server of its own, so the handler's set is
// another one, filled by its own reads and marked stale by nothing.
func legionBoardOnTheHub(t *testing.T, rig *hubRig, project string) legionBoardResponse {
	t.Helper()
	population, err := rig.hub.server.readLegionBoard(t.Context(), project)
	if err != nil {
		t.Fatalf("read %s's board on the hub's server: %v", project, err)
	}
	return legionBoardCompute(population, time.Now().UTC())
}

// applyEvents wakes the hub and waits until its event cursor has reached the log's head, so
// every event the test wrote has been applied.
func (r *hubRig) applyEvents(t *testing.T, what string) {
	t.Helper()
	head := eventHead(t, r.database)
	notifyChannel(t, r.database, EventsChannel, "")
	r.waitForEventCursor(t, head, what)
}

// TestAnOrdinarySessionsEventLeavesThePhaseWorkSetCurrent: the issues that carry Legion phase
// work are one read of every project's whole event log and of every handoff
// (legion.PhaseWorkKeys), so the live hub marks the set stale only for an event that can move
// what that read answers (legionPhaseWork.observe). A session that is no Legion role writes
// most of Dispatch's feed and can neither give an issue phase work nor take it away: the rule
// reads its newest title and finds no role in it, now and at the read before.
//
// The handoff this records behind the event's back is how the test sees that no read was made:
// it is written into the store with no event of its own, so a board read that made the read
// again would climb through the issue it names, and one that answered from the set it held
// would not.
func TestAnOrdinarySessionsEventLeavesThePhaseWorkSetCurrent(t *testing.T) {
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	_, middle, bottom := seedPhaseWorkChain(t, rig, 880, now.Add(-time.Hour))
	rig.applyEvents(t, "the issues the test seeded")
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != bottom {
		t.Fatalf("the board puts %s in tree %q before the event, want itself: the issue above it carries no phase work", bottom, tree)
	}

	seedLegionHandoff(t, rig.database, middle, "plan", now.Add(-30*time.Minute))
	seedSessionEvent(t, rig.database, bottom, "s-880", "A person's own session", now.Add(-time.Minute))
	rig.applyEvents(t, "the ordinary session's event")

	if !rig.hub.server.legionPhaseWork.isCurrent() {
		t.Error("an ordinary session's event marked the phase-work set stale, so the next board read scans every project's whole event log again")
	}
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != bottom {
		t.Errorf("the board puts %s in tree %q after an ordinary session's event, want itself: the handoff recorded behind the event says the set was read again", bottom, tree)
	}
}

// TestALegionTitleAndARenameMarkThePhaseWorkSetStale: the other half of the narrow rule. An
// event whose title makes its session a Legion role working the issue it is written on gives
// that issue phase work, and an event that renames such a session takes it away again
// (legion.PhaseWorkKeys reads each session's newest title), so both mark the set stale and the
// board read after each answers the set as it stands.
func TestALegionTitleAndARenameMarkThePhaseWorkSetStale(t *testing.T) {
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	top, middle, bottom := seedPhaseWorkChain(t, rig, 890, now.Add(-time.Hour))
	rig.applyEvents(t, "the issues the test seeded")
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != bottom {
		t.Fatalf("the board puts %s in tree %q before the event, want itself: the issue above it carries no phase work", bottom, tree)
	}

	// The session's first event under a Legion role's title, on the very issue that title
	// names: the middle issue carries phase work from here on, and the walk climbs through it.
	seedLegionRoleEvent(t, rig.database, middle, "s-890", "implementer", middle, now.Add(-30*time.Minute))
	rig.applyEvents(t, "the Legion role's event")
	if rig.hub.server.legionPhaseWork.isCurrent() {
		t.Error("an event that makes its session a Legion role of the issue it wrote on left the phase-work set current, so the next board read answers without it")
	}
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != top {
		t.Errorf("the board puts %s in tree %q after the Legion role's event, want %s: the issue above it carries phase work now", bottom, tree, top)
	}

	// The same session renamed to the issue it went on to work: its newest title is the only
	// title Dispatch resolves, so the middle issue carries no phase work by it any more.
	seedLegionRoleEvent(t, rig.database, bottom, "s-890", "implementer", bottom, now.Add(-15*time.Minute))
	rig.applyEvents(t, "the renamed session's event")
	if rig.hub.server.legionPhaseWork.isCurrent() {
		t.Error("an event that renamed a Legion role's session left the phase-work set current, so the next board read still holds the issue it wrote on before")
	}
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != bottom {
		t.Errorf("the board puts %s in tree %q after the rename, want itself: no session titled for %s wrote on it", bottom, tree, middle)
	}
}

// TestAnUnstartedChildIsCountedOnTheNearestCardAndNeverLosesItsOwn: a card an issue would carry
// is never lost to the widened walk, and the count of an unstarted child lands on a card someone
// can read. The chain here hangs under an issue that carries only Legion phase work, so the walk
// climbs through it and no card stands at the tree's root: the admitted issue below it keeps its
// own card, and the unstarted child below that is counted on it rather than on the root the
// board shows nothing of.
func TestAnUnstartedChildIsCountedOnTheNearestCardAndNeverLosesItsOwn(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	// No admission block is recorded, so the Queue is Dispatch's rank order over the issues
	// waiting for a slot (the board's own fallback).
	seedLegionIssue(t, database, "INFRA", 820, "The issue with only phase work", "a", nil, "")
	seedLegionHandoff(t, database, "INFRA-820", "plan", hourAgo)
	for _, issue := range []struct {
		number int
		title  string
		rank   string
		parent string
	}{
		{821, "The admitted issue", "b", "INFRA-820"},
		{822, "Its unstarted child", "c", "INFRA-821"},
	} {
		key := seedLegionIssue(t, database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
		seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"admitted"`, hourAgo)
	}
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	cards := map[string]legionBoardCard{}
	columns := map[string]string{}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			cards[card.Key] = card
			columns[card.Key] = column.ID
		}
	}
	named := slices.Sorted(maps.Keys(cards))
	if want := []string{"INFRA-821"}; !slices.Equal(named, want) {
		t.Fatalf("the board's cards are %v, want %v: the admitted issue keeps the card it carried", named, want)
	}
	if column := columns["INFRA-821"]; column != legionColumnQueue {
		t.Errorf("INFRA-821 is in the %q column, want %q: it is waiting for a slot", column, legionColumnQueue)
	}
	if children := cards["INFRA-821"].Children; children != 1 {
		t.Errorf("INFRA-821 counts %d unstarted children, want the one below it: a count on a root the board shows no card of is a count nobody reads", children)
	}
	if tree := cards["INFRA-821"].Tree; tree != "INFRA-820" {
		t.Errorf("INFRA-821 is in tree %q, want INFRA-820, the issue with phase work above it", tree)
	}
}

// TestTheMetricsClimbThroughPhaseWorkOutsideTheirWindow: the phase-work half of what makes an
// issue Legion's is a fact of the project and not of a window, so the metrics read it for every
// issue of the project and not only for the issues their window's scope covers
// (legion/metrics/population.go's scopeKeys). Nothing here closed in the window, so nothing but
// that project-wide read puts the card issue in its ancestor's tree.
func TestTheMetricsClimbThroughPhaseWorkOutsideTheirWindow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	for _, issue := range []struct {
		number   int
		title    string
		rank     string
		parent   string
		reported bool
	}{
		{830, "The reported ancestor", "a", "", true},
		{831, "The issue with only phase work", "b", "INFRA-830", false},
		{832, "The card issue", "c", "INFRA-831", true},
	} {
		key := seedLegionIssue(t, database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
		if issue.reported {
			seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"implementing"`, hourAgo)
		}
	}
	seedLegionHandoff(t, database, "INFRA-831", "implement", hourAgo)
	seedLegionRoleEvent(t, database, "INFRA-831", "s-831", "reviewer", "INFRA-831", hourAgo)
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	if root := population.TreeRoot("INFRA-832"); root != "INFRA-830" {
		t.Errorf("the metrics put INFRA-832 in tree %q, want INFRA-830: no issue of this chain finished in the window", root)
	}
	if root := population.TreeRoot("INFRA-831"); root != "INFRA-830" {
		t.Errorf("the metrics put INFRA-831 in tree %q, want INFRA-830", root)
	}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			if root := population.TreeRoot(card.Key); root != card.Tree {
				t.Errorf("%s is in tree %q on the board and %q in the metrics", card.Key, card.Tree, root)
			}
		}
	}
}
