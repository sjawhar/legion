package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type rankedIssue struct {
	Key  string `json:"key"`
	Rank string `json:"rank"`
}

func TestPatchIssueRankPlacesCardBetweenProjectNeighbors(t *testing.T) {
	handler := newTestHandler(t)
	for _, project := range []string{"TEST", "OTHER"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": project + " project",
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
	}
	create := func(project, title string) rankedIssue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": title, "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[rankedIssue](t, response)
	}
	first := create("TEST", "First")
	second := create("TEST", "Second")
	third := create("TEST", "Third")
	if !(first.Rank < second.Rank && second.Rank < third.Rank) {
		t.Fatalf("new issue ranks = first=%q second=%q third=%q, want creation order", first.Rank, second.Rank, third.Rank)
	}
	other := create("OTHER", "Other")

	patchRank := func(key string, input map[string]any) rankedIssue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, input, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("patch %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
		return decodeBody[rankedIssue](t, response)
	}

	third = patchRank(third.Key, map[string]any{"rank": map[string]string{"before": first.Key}})
	if !(third.Rank < first.Rank) {
		t.Fatalf("before rank %q must precede first %q", third.Rank, first.Rank)
	}
	first = patchRank(first.Key, map[string]any{"rank": map[string]string{"after": second.Key}})
	if !(second.Rank < first.Rank) {
		t.Fatalf("after rank %q must follow second %q", first.Rank, second.Rank)
	}
	second = patchRank(second.Key, map[string]any{"rank": map[string]string{
		"after": third.Key, "before": first.Key,
	}})
	if !(third.Rank < second.Rank && second.Rank < first.Rank) {
		t.Fatalf("both-neighbor rank order = third=%q second=%q first=%q", third.Rank, second.Rank, first.Rank)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+second.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list issue events: status=%d body=%s", events.Code, events.Body.String())
	}
	storedEvents := decodeBody[[]model.Event](t, events)
	if len(storedEvents) == 0 {
		t.Fatal("rank update produced no event")
	}
	last := storedEvents[len(storedEvents)-1]
	payload, ok := last.Payload.(map[string]any)
	if !ok || last.Type != "issue.updated" || payload["rank"] != second.Rank {
		t.Fatalf("rank event = %#v, want issue.updated with rank %q", last, second.Rank)
	}

	for _, input := range []map[string]any{
		{"rank": map[string]string{"before": "MISSING-1"}},
		{"rank": map[string]string{"after": other.Key}},
	} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+first.Key, input, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"RANK_INPUT"`) {
			t.Fatalf("invalid rank input: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestPatchIssueRankAllocatesDistinctRanksForSequentialMovesIntoSameGap(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string) rankedIssue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": title, "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[rankedIssue](t, response)
	}
	first := create("First")
	second := create("Second")
	third := create("Third")
	fourth := create("Fourth")
	moveIntoGap := func(issue rankedIssue) rankedIssue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
			"rank": map[string]string{"after": first.Key, "before": second.Key},
		}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("move %s: status=%d body=%s", issue.Key, response.Code, response.Body.String())
		}
		return decodeBody[rankedIssue](t, response)
	}
	third = moveIntoGap(third)
	fourth = moveIntoGap(fourth)

	ranks := []string{first.Rank, second.Rank, third.Rank, fourth.Rank}
	sort.Strings(ranks)
	for index := 1; index < len(ranks); index++ {
		if ranks[index-1] >= ranks[index] {
			t.Fatalf("ranks must be distinct and strictly ordered: %#v", ranks)
		}
	}
	if !(first.Rank < third.Rank && third.Rank < second.Rank) {
		t.Fatalf("third rank %q must remain between %q and %q", third.Rank, first.Rank, second.Rank)
	}
	if !(first.Rank < fourth.Rank && fourth.Rank < second.Rank) {
		t.Fatalf("fourth rank %q must remain between %q and %q", fourth.Rank, first.Rank, second.Rank)
	}
}

func TestPatchIssueRankSerializesConcurrentMovesIntoSameGap(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string) rankedIssue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": title, "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[rankedIssue](t, response)
	}
	first := create("First")
	second := create("Second")
	third := create("Third")
	fourth := create("Fourth")

	ctx := context.Background()
	lock, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rank allocation lock: %v", err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if _, err := lock.Exec(ctx, `select pg_advisory_xact_lock(hashtext('issue-rank:' || $1))`, "TEST"); err != nil {
		t.Fatalf("lock project rank allocation: %v", err)
	}

	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, issue := range []rankedIssue{third, fourth} {
		go func(issue rankedIssue) {
			<-start
			responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
				"rank": map[string]string{"after": first.Key, "before": second.Key},
			}, "alice")
		}(issue)
	}
	close(start)
	waitForDatabaseLocks(t, lock, 2)
	if err := lock.Commit(ctx); err != nil {
		t.Fatalf("release rank allocation lock: %v", err)
	}

	moved := make([]rankedIssue, 0, 2)
	for range 2 {
		response := awaitResponse(t, responses)
		if response.Code != http.StatusOK {
			t.Fatalf("move into shared gap: status=%d body=%s", response.Code, response.Body.String())
		}
		moved = append(moved, decodeBody[rankedIssue](t, response))
	}
	ranks := []string{first.Rank, second.Rank, moved[0].Rank, moved[1].Rank}
	sort.Strings(ranks)
	for index := 1; index < len(ranks); index++ {
		if ranks[index-1] >= ranks[index] {
			t.Fatalf("concurrent ranks must be distinct and strictly ordered: %#v", ranks)
		}
	}
}

func TestCreateIssueWaitsForProjectRankAllocationLock(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	ctx := context.Background()
	lock, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rank allocation lock: %v", err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if _, err := lock.Exec(ctx, `select pg_advisory_xact_lock(hashtext('issue-rank:' || $1))`, "TEST"); err != nil {
		t.Fatalf("lock project rank allocation: %v", err)
	}

	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": "Blocked issue", "force": true,
		}, "alice")
	}()
	waitForDatabaseLocks(t, lock, 1)
	if err := lock.Commit(ctx); err != nil {
		t.Fatalf("release rank allocation lock: %v", err)
	}
	response := awaitResponse(t, responses)
	if response.Code != http.StatusCreated {
		t.Fatalf("create after rank allocation lock: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestListIssuesOrdersStatusColumnsThenRank(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string) rankedIssue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": title, "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[rankedIssue](t, response)
	}
	triage := create("Triage")
	todoFirst := create("Todo first")
	todoSecond := create("Todo second")
	done := create("Done")
	for key, status := range map[string]string{
		todoFirst.Key:  "todo",
		todoSecond.Key: "todo",
		done.Key:       "done",
	} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]string{"status": status}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("set %s status: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+todoSecond.Key, map[string]any{
		"rank": map[string]string{"before": todoFirst.Key},
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("reorder todo: status=%d body=%s", response.Code, response.Body.String())
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
	}
	issues := decodeBody[[]rankedIssue](t, listed)
	want := []string{triage.Key, todoSecond.Key, todoFirst.Key, done.Key}
	if len(issues) != len(want) {
		t.Fatalf("listed issues = %#v, want %d rows", issues, len(want))
	}
	for index, key := range want {
		if issues[index].Key != key {
			t.Fatalf("listed issue %d = %s, want %s (%#v)", index, issues[index].Key, key, issues)
		}
	}
}

// TestIssueRanksStayOrderedPastLowercaseDigitsAndProductionShapedMoves creates enough issues by
// plain append (no rank input) that a rank must carry a lowercase digit, and checks that
// Postgres's own "order by rank" agrees with the byte-order comparison the rank field assumes
// everywhere it sorts issues. It then exercises every shape a PATCH .../issues/{key}
// {"rank": ...} body can take -- {} (move to the project's end), {after} alone, {before}
// alone, both neighbors, and neighbors that are themselves byte-wise prefixes of each other
// (the shape the pre-existing append rule always produced) -- against ranks that already carry
// a lowercase digit, asserting the project's list order after each move.
func TestIssueRanksStayOrderedPastLowercaseDigitsAndProductionShapedMoves(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)

	// This test's whole point is a database whose default collation disagrees with byte order
	// on a lowercase digit. On a database whose default already is byte order, it would pass
	// with or without migration 0049, proving nothing.
	var collationIsNotByteOrder bool
	if err := database.Pool.QueryRow(context.Background(), `select 'a' < 'B'`).Scan(&collationIsNotByteOrder); err != nil {
		t.Fatalf("check the test database's default collation: %v", err)
	}
	if !collationIsNotByteOrder {
		t.Fatal(`this test's database already orders "a" after "B" (byte order: select 'a' < 'B' is false), so it cannot tell migration 0049's collate "C" apart from no fix at all; run it against a database whose default collation is not already byte order (e.g. en_US.utf8, Postgres's own default)`)
	}

	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "RANKORD", "name": "Rank Ordering",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string) rankedIssue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "RANKORD", "title": title, "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[rankedIssue](t, response)
	}

	const issueCount = 40
	issues := make([]rankedIssue, issueCount)
	for i := range issues {
		issues[i] = create(fmt.Sprintf("Issue %d", i))
	}

	rows, err := database.Pool.Query(context.Background(), `select rank from issues where project_key = $1 order by rank`, "RANKORD")
	if err != nil {
		t.Fatalf("read ranked issues: %v", err)
	}
	defer rows.Close()
	var ranks []string
	for rows.Next() {
		var rank string
		if err := rows.Scan(&rank); err != nil {
			t.Fatalf("scan rank: %v", err)
		}
		ranks = append(ranks, rank)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ranked issues: %v", err)
	}
	if len(ranks) != issueCount {
		t.Fatalf("got %d ranks, want %d", len(ranks), issueCount)
	}
	for i := 1; i < len(ranks); i++ {
		if !(ranks[i-1] < ranks[i]) {
			t.Fatalf("Postgres's rank order disagrees with Go byte order at index %d: %q then %q (full order: %v)", i, ranks[i-1], ranks[i], ranks)
		}
	}

	patchRank := func(key string, rank map[string]string) rankedIssue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"rank": rank}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("move %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
		return decodeBody[rankedIssue](t, response)
	}
	assertRelativeOrder := func(want ...string) {
		t.Helper()
		listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=RANKORD", nil, "alice")
		if listed.Code != http.StatusOK {
			t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
		}
		positions := map[string]int{}
		for index, issue := range decodeBody[[]rankedIssue](t, listed) {
			positions[issue.Key] = index
		}
		for i := 1; i < len(want); i++ {
			if positions[want[i-1]] >= positions[want[i]] {
				t.Fatalf("want %s before %s in project order, got positions %#v", want[i-1], want[i], positions)
			}
		}
	}

	// issues[6]'s rank ("a") is the first of the 40 to carry a lowercase digit.
	if !strings.ContainsAny(issues[6].Rank, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("issue 6's rank %q should already carry a lowercase digit", issues[6].Rank)
	}

	// rank:{} moves an issue to the project's end (lastProjectRank, no before/after).
	issues[0] = patchRank(issues[0].Key, map[string]string{})
	assertRelativeOrder(issues[39].Key, issues[0].Key)

	// rank:{after} alone, anchored on a lowercase-ranked neighbor: open-ended toward the end.
	issues[1] = patchRank(issues[1].Key, map[string]string{"after": issues[6].Key})
	assertRelativeOrder(issues[6].Key, issues[1].Key, issues[7].Key)

	// rank:{before} alone, anchored on the same lowercase-ranked neighbor: open-ended toward
	// the start.
	issues[2] = patchRank(issues[2].Key, map[string]string{"before": issues[6].Key})
	assertRelativeOrder(issues[5].Key, issues[2].Key, issues[6].Key)

	// Both neighbors lowercase-ranked: the two moves above left issues[1] and issues[0] each
	// carrying a lowercase digit.
	if !strings.ContainsAny(issues[1].Rank, "abcdefghijklmnopqrstuvwxyz") || !strings.ContainsAny(issues[0].Rank, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("both anchors should carry a lowercase digit: %q, %q", issues[1].Rank, issues[0].Rank)
	}
	issues[3] = patchRank(issues[3].Key, map[string]string{"after": issues[1].Key, "before": issues[0].Key})
	assertRelativeOrder(issues[1].Key, issues[3].Key, issues[7].Key)

	// Neighbors that are byte-wise prefixes of each other -- the shape the pre-existing append
	// rule always produced (prev + "U") -- exercising Between's common == len(prev) branch.
	prefixParent, prefixChild := create("Prefix parent"), create("Prefix child")
	const prefixParentRank = "00000000000000000100"
	for key, rank := range map[string]string{prefixParent.Key: prefixParentRank, prefixChild.Key: prefixParentRank + "U"} {
		if _, err := database.Pool.Exec(context.Background(), `update issues set rank = $1 where key = $2`, rank, key); err != nil {
			t.Fatalf("seed production-shaped rank for %s: %v", key, err)
		}
	}
	moved := create("Moved between prefixed neighbors")
	moved = patchRank(moved.Key, map[string]string{"after": prefixParent.Key, "before": prefixChild.Key})
	if !(prefixParentRank < moved.Rank && moved.Rank < prefixParentRank+"U") {
		t.Fatalf("moved rank %q must sit between %q and %q", moved.Rank, prefixParentRank, prefixParentRank+"U")
	}
	assertRelativeOrder(prefixParent.Key, moved.Key, prefixChild.Key)
}
