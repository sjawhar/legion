package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
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
