package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// blocked_by is an issue-to-issue dependency: create and PATCH replace its complete target
// set, every issue payload exposes it, and list rows include it with the primary artifact id.
func TestIssueBlockedByRoundTrip(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	type issueRead struct {
		Key               string   `json:"key"`
		BlockedBy         []string `json:"blocked_by"`
		PrimaryArtifactID string   `json:"primary_artifact_id"`
	}
	create := func(title string, fields map[string]any) issueRead {
		t.Helper()
		body := map[string]any{"project": "CORE", "title": title}
		for name, value := range fields {
			body[name] = value
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[issueRead](t, response)
	}
	first := create("First blocker", nil)
	second := create("Second blocker", nil)
	dependent := create("Dependent", map[string]any{"blocked_by": []string{second.Key, first.Key}})
	want := []string{first.Key, second.Key}
	if !slices.Equal(dependent.BlockedBy, want) {
		t.Fatalf("created blocked_by = %v, want %v", dependent.BlockedBy, want)
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+dependent.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	if got := decodeBody[issueRead](t, detail).BlockedBy; !slices.Equal(got, want) {
		t.Fatalf("read blocked_by = %v, want %v", got, want)
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
	}
	var listedDependent *issueRead
	for _, row := range decodeBody[[]issueRead](t, listed) {
		if row.Key == dependent.Key {
			listedDependent = &row
			break
		}
	}
	if listedDependent == nil || !slices.Equal(listedDependent.BlockedBy, want) || listedDependent.PrimaryArtifactID == "" {
		t.Fatalf("list row = %#v, want blocked_by=%v and a primary_artifact_id", listedDependent, want)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+dependent.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	var log []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(events.Body).Decode(&log); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	foundCreated := false
	for _, event := range log {
		if event.Type != "issue.created" {
			continue
		}
		foundCreated = true
		var payload issueRead
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode issue.created payload: %v", err)
		}
		if !slices.Equal(payload.BlockedBy, want) {
			t.Fatalf("issue.created blocked_by = %v, want %v", payload.BlockedBy, want)
		}
	}
	if !foundCreated {
		t.Fatalf("issue events = %s, want issue.created", events.Body.String())
	}

	cleared := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+dependent.Key, map[string]any{
		"blocked_by": []string{},
	}, "alice")
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear blocked_by: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	if got := decodeBody[issueRead](t, cleared).BlockedBy; len(got) != 0 {
		t.Fatalf("patched blocked_by = %v, want []", got)
	}
}

// A blocked_by dependency cannot make a child wait on its parent, a parent wait on its descendant,
// or any issue wait on itself. One sibling may wait on another, and blockers never cross a
// Dispatch project boundary.
func TestIssueBlockedByRefusesDependencyCycles(t *testing.T) {
	handler := newTestHandler(t)
	for _, project := range []string{"CORE", "SIDE"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": project,
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
	}
	create := func(project, title string, fields map[string]any) model.Issue {
		t.Helper()
		body := map[string]any{"project": project, "title": title, "force": true}
		for name, value := range fields {
			body[name] = value
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	expectRefusal := func(name string, response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if response.Code != status || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("%s: status=%d body=%s, want %d %s", name, response.Code, response.Body.String(), status, code)
		}
	}

	first := create("CORE", "First", nil)
	second := create("CORE", "Second", map[string]any{"blocked_by": []string{first.Key}})
	expectRefusal("dependency loop", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+first.Key, map[string]any{
		"blocked_by": []string{second.Key},
	}, "alice"), http.StatusConflict, "DEPENDENCY_CYCLE")

	parent := create("CORE", "Parent", nil)
	child := create("CORE", "Child", map[string]any{"parent": parent.Key})
	expectRefusal("child waits on its parent", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]any{
		"blocked_by": []string{parent.Key},
	}, "alice"), http.StatusConflict, "DEPENDENCY_CYCLE")
	create("CORE", "Sibling", map[string]any{"parent": parent.Key, "blocked_by": []string{child.Key}})

	childToReparent := create("CORE", "Child to reparent", nil)
	intermediate := create("CORE", "Intermediate", map[string]any{"blocked_by": []string{childToReparent.Key}})
	parentWithBlocker := create("CORE", "Parent with blocker", map[string]any{
		"blocked_by": []string{intermediate.Key},
	})
	expectRefusal("reparent closes dependency loop", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+childToReparent.Key, map[string]any{
		"parent": parentWithBlocker.Key,
	}, "alice"), http.StatusConflict, "DEPENDENCY_CYCLE")

	foreign := create("SIDE", "Foreign", nil)
	expectRefusal("foreign-project blocker", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Cross-project dependency", "blocked_by": []string{foreign.Key},
	}, "alice"), http.StatusBadRequest, "BLOCKED_BY_OUTSIDE_PROJECT")
}

// An issue naming itself in blocked_by is an input mistake, refused as one rather than as a
// dependency cycle.
func TestIssueBlockedBySelfIsAnInputError(t *testing.T) {
	handler := newTestHandler(t)
	key := createListedIssues(t, handler, "CORE", 1)[0]
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"blocked_by": []string{key}}, "alice")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("self blocker: %d %s", response.Code, response.Body.String())
	}
	if refusal := decodeBody[struct {
		Code string `json:"code"`
	}](t, response); refusal.Code != "BLOCKED_BY_INPUT" {
		t.Fatalf("refusal = %s, want BLOCKED_BY_INPUT", response.Body.String())
	}
}

func TestIssueBlockedByConcurrentWritesDoNotDeadlock(t *testing.T) {
	for _, scenario := range []string{"reciprocal blockers", "blocker and reparent", "old parent and reparent"} {
		t.Run(scenario, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
				"key": "CORE", "name": "Core",
			}, "alice"); response.Code != http.StatusCreated {
				t.Fatalf("create project: %d %s", response.Code, response.Body.String())
			}
			create := func(title string, parent *string) string {
				t.Helper()
				response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": "CORE", "title": title, "force": true, "parent": parent,
				}, "alice")
				if response.Code != http.StatusCreated {
					t.Fatalf("create issue: %d %s", response.Code, response.Body.String())
				}
				return decodeBody[model.Issue](t, response).Key
			}
			for round := range 8 {
				first := create(fmt.Sprintf("First %d", round), nil)
				second := create(fmt.Sprintf("Second %d", round), nil)
				gateKey := first
				firstKey, secondKey := first, second
				firstBody := map[string]any{"blocked_by": []string{second}}
				secondBody := map[string]any{"blocked_by": []string{first}}
				wantConflicts := 1
				switch scenario {
				case "blocker and reparent":
					firstBody = map[string]any{"parent": second}
				case "old parent and reparent":
					child := create(fmt.Sprintf("Child %d", round), &first)
					gateKey, firstKey, secondKey = child, child, first
					firstBody = map[string]any{"parent": second}
					secondBody = map[string]any{"blocked_by": []string{child}}
					wantConflicts = 0
				}
				gate, err := database.Pool.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
				if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, gateKey); err != nil {
					t.Fatal(err)
				}
				responses := make(chan *httptest.ResponseRecorder, 2)
				go func() {
					responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+firstKey, firstBody, "alice")
				}()
				waitForDatabaseLocks(t, gate, 1)
				go func() {
					responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+secondKey, secondBody, "alice")
				}()
				waitForDatabaseLocks(t, gate, 2)
				if err := gate.Commit(context.Background()); err != nil {
					t.Fatal(err)
				}
				successes, conflicts := 0, 0
				for range 2 {
					response := awaitResponse(t, responses)
					switch response.Code {
					case http.StatusOK:
						successes++
					case http.StatusConflict:
						refusal := decodeBody[struct {
							Code string `json:"code"`
						}](t, response)
						if refusal.Code != "DEPENDENCY_CYCLE" {
							t.Fatalf("round %d: refusal = %s, want DEPENDENCY_CYCLE", round, response.Body.String())
						}
						conflicts++
					default:
						t.Errorf("round %d: %d %s", round, response.Code, response.Body.String())
					}
				}
				if successes != 2-wantConflicts || conflicts != wantConflicts {
					t.Fatalf("round %d: successes=%d conflicts=%d, want %d and %d",
						round, successes, conflicts, 2-wantConflicts, wantConflicts)
				}
			}
		})
	}
}

func TestIssueBlockedByCountLimitOnCreateAndPatch(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: %d %s", response.Code, response.Body.String())
	}
	var blockers []string
	for index := range 21 {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": fmt.Sprintf("Blocker %d", index), "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create blocker: %d %s", response.Code, response.Body.String())
		}
		blockers = append(blockers, decodeBody[model.Issue](t, response).Key)
	}
	atCap := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Dependent at cap", "blocked_by": blockers[:20],
	}, "alice")
	if atCap.Code != http.StatusCreated {
		t.Fatalf("create at cap: %d %s", atCap.Code, atCap.Body.String())
	}
	dependent := decodeBody[model.Issue](t, atCap)
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			path := "/api/v1/issues"
			body := map[string]any{"project": "CORE", "title": "Too many blockers", "blocked_by": blockers, "force": true}
			if method == http.MethodPatch {
				path += "/" + dependent.Key
				body = map[string]any{"blocked_by": blockers}
			}
			response := dispatchRequest(t, handler, method, path, body, "alice")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("over cap: %d %s, want 400", response.Code, response.Body.String())
			}
			refusal := decodeBody[struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}](t, response)
			if refusal.Code != "BLOCKED_BY_INPUT" || !strings.Contains(refusal.Error, "20") || !strings.Contains(refusal.Error, "blocked_by") {
				t.Fatalf("over-cap refusal must name blocked_by and its limit: %+v", refusal)
			}
		})
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+dependent.Key, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read after refusal: %d %s", read.Code, read.Body.String())
	}
	if got := decodeBody[model.Issue](t, read).BlockedBy; !slices.Equal(got, dependent.BlockedBy) {
		t.Fatalf("refused replacement changed blockers: %v, want %v", got, dependent.BlockedBy)
	}
}

func TestIssueBlockedByIgnoresIndirectRowActivity(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: %d %s", response.Code, response.Body.String())
	}
	var keys []string
	for index := range 3 {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": fmt.Sprintf("Node %d", index), "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create node: %d %s", response.Code, response.Body.String())
		}
		keys = append(keys, decodeBody[model.Issue](t, response).Key)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[1], map[string]any{
		"blocked_by": []string{keys[2]},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("seed dependency: %d %s", response.Code, response.Body.String())
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, keys[2]); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	body := map[string]any{"blocked_by": []string{keys[1]}}
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[0], body, "alice")
	}()
	response := awaitResponse(t, responses)
	if response.Code != http.StatusOK || !slices.Equal(decodeBody[model.Issue](t, response).BlockedBy, []string{keys[1]}) {
		t.Fatalf("busy indirect target must not block the dependency write: %d %s", response.Code, response.Body.String())
	}
}

func TestStatusPatchRacingReparentUsesCommittedParent(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, keys[1], keys[2]); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[2], map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := awaitResponse(t, responses)
	if response.Code != http.StatusOK {
		t.Fatalf("status after reparent: %d %s", response.Code, response.Body.String())
	}
	issue := decodeBody[model.Issue](t, response)
	if issue.Status != "todo" || issue.Parent == nil || *issue.Parent != keys[1] {
		t.Fatalf("status write lost the committed parent: %+v", issue)
	}
}

func TestReparentIgnoresUnrelatedGrandchildRowActivity(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 4)
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, keys[3], keys[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, keys[0], keys[1]); err != nil {
		t.Fatal(err)
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, keys[1]); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[0], map[string]string{"parent": keys[2]}, "alice")
	}()
	response := awaitResponse(t, responses)
	if response.Code != http.StatusOK {
		t.Fatalf("reparent with a busy descendant: %d %s", response.Code, response.Body.String())
	}
	if parent := decodeBody[model.Issue](t, response).Parent; parent == nil || *parent != keys[2] {
		t.Fatalf("reparent did not persist: %v", parent)
	}
}

// A status write locks its issue and then its parent. Reparenting that issue's sibling under it
// must not hold the shared parent while it waits for the new parent's row: the status write would
// hold that row and wait for the shared parent, and Postgres would abort one of them.
func TestReparentUnderSiblingDoesNotDeadlockWithItsStatusWrite(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	parent, moving, sibling := keys[0], keys[1], keys[2]
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set parent_key = $1 where key = any($2::text[])
	`, parent, []string{moving, sibling}); err != nil {
		t.Fatal(err)
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, parent); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]string{"parent": sibling}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+sibling, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if response := awaitResponse(t, responses); response.Code != http.StatusOK {
			t.Errorf("reparent beside a status write on the new parent: %d %s", response.Code, response.Body.String())
		}
	}
	if issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+moving, nil, "alice")); issue.Parent == nil || *issue.Parent != sibling {
		t.Fatalf("reparent did not persist: %v", issue.Parent)
	}
	if issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+sibling, nil, "alice")); issue.Status != "todo" {
		t.Fatalf("status write did not persist: %s", issue.Status)
	}
}

// A board move locks its two rank neighbours, and a reparent locks the issue and its new parent,
// each pair in key order. Reparenting one neighbour under the other while an issue moves between
// them therefore queues one write behind the other, whichever of the two the board shows first,
// rather than each holding one neighbour while waiting on the other.
func TestReparentDoesNotDeadlockWithBoardMoveBetweenItsEnds(t *testing.T) {
	for _, scenario := range []struct {
		name string
		// moving, parent and child index the three issues in creation order, which is key order.
		moving, parent, child int
		// childFirst moves the child ahead of its parent on the board before the race.
		childFirst bool
	}{
		{name: "board order matches key order", moving: 0, parent: 1, child: 2},
		{name: "board order reverses key order", moving: 2, parent: 0, child: 1, childFirst: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
			keys := createListedIssues(t, handler, "CORE", 3)
			moving, parent, child := keys[scenario.moving], keys[scenario.parent], keys[scenario.child]
			after, before := parent, child
			if scenario.childFirst {
				if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{
					"rank": map[string]string{"before": parent},
				}, "alice"); response.Code != http.StatusOK {
					t.Fatalf("rank the child first: %d %s", response.Code, response.Body.String())
				}
				after, before = child, parent
			}
			// The gate holds the neighbour the move names second, so the reparent waits for it and
			// the move reaches the other neighbour before the gate opens.
			gate, err := database.Pool.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback(context.Background())
			if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, before); err != nil {
				t.Fatal(err)
			}
			responses := make(chan *httptest.ResponseRecorder, 2)
			go func() {
				responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]string{"parent": parent}, "alice")
			}()
			waitForDatabaseLocks(t, gate, 1)
			go func() {
				responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]any{
					"rank": map[string]string{"after": after, "before": before},
				}, "alice")
			}()
			waitForDatabaseLocks(t, gate, 2)
			if err := gate.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if response := awaitResponse(t, responses); response.Code != http.StatusOK {
					t.Errorf("reparent beside a board move between its ends: %d %s", response.Code, response.Body.String())
				}
			}
			issues := map[string]model.Issue{}
			for _, key := range keys {
				issues[key] = decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
			}
			if got := issues[child].Parent; got == nil || *got != parent {
				t.Fatalf("reparent did not persist: %v", got)
			}
			if !(issues[after].Rank < issues[moving].Rank && issues[moving].Rank < issues[before].Rank) {
				t.Fatalf("board move did not put the issue between its neighbours: %q < %q < %q",
					issues[after].Rank, issues[moving].Rank, issues[before].Rank)
			}
		})
	}
}

// A PATCH locks the issue, its new parent and its rank neighbours together, in one key order.
// Moving an issue on the board while it is reparented under one of its neighbours therefore never
// has one write holding the issue while the other holds the neighbour: a move waiting for its
// gated first neighbour holds nothing yet, so the reparent finishes before the gate opens.
func TestReparentDoesNotDeadlockWithABoardMoveOfTheSameIssue(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	after, before, moving := keys[0], keys[1], keys[2]
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, after); err != nil {
		t.Fatal(err)
	}
	moves := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		moves <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]any{
			"rank": map[string]string{"after": after, "before": before},
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	reparents := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		reparents <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]string{"parent": before}, "alice")
	}()
	select {
	case response := <-reparents:
		if response.Code != http.StatusOK {
			t.Fatalf("reparent beside a waiting board move of the same issue: %d %s", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		// The reparent is waiting for the move. Open the gate and report how the two writes end.
		if err := gate.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		reparent, move := awaitResponse(t, reparents), awaitResponse(t, moves)
		t.Fatalf("reparent waited for a board move of the same issue; once the gate opened the reparent answered %d %s and the move %d %s",
			reparent.Code, reparent.Body.String(), move.Code, move.Body.String())
	}
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if response := awaitResponse(t, moves); response.Code != http.StatusOK {
		t.Fatalf("board move after the gate opened: %d %s", response.Code, response.Body.String())
	}
	issues := map[string]model.Issue{}
	for _, key := range keys {
		issues[key] = decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
	}
	if got := issues[moving].Parent; got == nil || *got != before {
		t.Fatalf("reparent did not persist: %v", got)
	}
	if !(issues[after].Rank < issues[moving].Rank && issues[moving].Rank < issues[before].Rank) {
		t.Fatalf("board move did not put the issue between its neighbours: %q < %q < %q",
			issues[after].Rank, issues[moving].Rank, issues[before].Rank)
	}
}

// A status write appends to its issue's parent, so it locks the issue and that parent together, in
// key order, as a board move locks its neighbours. A child shown above its parent on the board,
// with an issue moved between the two while the child's status is written, therefore never has the
// move holding the parent while the status write holds the child.
func TestStatusWriteDoesNotDeadlockWithBoardMoveBetweenTheIssueAndItsParent(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	parent, child, moving := keys[0], keys[1], keys[2]
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, parent, child); err != nil {
		t.Fatal(err)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{
		"rank": map[string]string{"before": parent},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rank the child above its parent: %d %s", response.Code, response.Body.String())
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, child); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]any{
			"rank": map[string]string{"after": child, "before": parent},
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if response := awaitResponse(t, responses); response.Code != http.StatusOK {
			t.Errorf("status write beside a board move between the issue and its parent: %d %s", response.Code, response.Body.String())
		}
	}
	issues := map[string]model.Issue{}
	for _, key := range keys {
		issues[key] = decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
	}
	if issues[child].Status != "todo" {
		t.Fatalf("status write did not persist: %s", issues[child].Status)
	}
	if !(issues[child].Rank < issues[moving].Rank && issues[moving].Rank < issues[parent].Rank) {
		t.Fatalf("board move did not put the issue between its neighbours: %q < %q < %q",
			issues[child].Rank, issues[moving].Rank, issues[parent].Rank)
	}
}

// A reparent locks the issue's old parent together with the issue and its new parent, in key
// order. Moving an issue up to its grandparent while its old parent's status is written therefore
// never has the reparent holding the grandparent while the status write holds the old parent.
func TestReparentToGrandparentDoesNotDeadlockWithAStatusWriteOnTheOldParent(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	grandparent, parent, child := keys[0], keys[1], keys[2]
	for _, link := range [][2]string{{parent, grandparent}, {child, parent}} {
		if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, link[1], link[0]); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, grandparent); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]string{"parent": grandparent}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+parent, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if response := awaitResponse(t, responses); response.Code != http.StatusOK {
			t.Errorf("reparent to the grandparent beside a status write on the old parent: %d %s", response.Code, response.Body.String())
		}
	}
	if issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+child, nil, "alice")); issue.Parent == nil || *issue.Parent != grandparent {
		t.Fatalf("reparent did not persist: %v", issue.Parent)
	}
	if issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+parent, nil, "alice")); issue.Status != "todo" {
		t.Fatalf("status write did not persist: %s", issue.Status)
	}
}

// A status write reads its issue's parent before it locks the pair, and a reparent of the issue
// can commit in between. It then takes its locks again for the parent that stands; a parent that
// moves at every attempt is refused with 409 PARENT_CONTENDED and nothing applied, never locked
// out of key order.
func TestStatusWriteRefusesAParentThatMovesAtEveryAttempt(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 4)
	first, second, third, child := keys[0], keys[1], keys[2], keys[3]
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, first, child); err != nil {
		t.Fatal(err)
	}
	// The first attempt locks the first parent and waits for the child, which this gate holds.
	childGate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer childGate.Rollback(context.Background())
	if _, err := childGate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, child); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, childGate, 1)
	// The second attempt will lock the second parent first, which this gate holds.
	parentGate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer parentGate.Rollback(context.Background())
	if _, err := parentGate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, second); err != nil {
		t.Fatal(err)
	}
	if _, err := childGate.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, second, child); err != nil {
		t.Fatal(err)
	}
	if err := childGate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForDatabaseLocks(t, parentGate, 1)
	if _, err := parentGate.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, third, child); err != nil {
		t.Fatal(err)
	}
	if err := parentGate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := awaitResponse(t, responses)
	if response.Code != http.StatusConflict {
		t.Fatalf("status write whose parent moved at every attempt: %d %s", response.Code, response.Body.String())
	}
	if refusal := decodeBody[struct {
		Code string `json:"code"`
	}](t, response); refusal.Code != "PARENT_CONTENDED" {
		t.Fatalf("refusal = %s, want PARENT_CONTENDED", response.Body.String())
	}
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+child, nil, "alice"))
	if issue.Status == "todo" || issue.Parent == nil || *issue.Parent != third {
		t.Fatalf("the refused write applied something: status %s, parent %v", issue.Status, issue.Parent)
	}
}

func TestChildCreateWithoutBlockersDoesNotWaitOnParentRow(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	parent := createListedIssues(t, handler, "CORE", 1)[0]
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, parent); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": "Child while parent is busy", "parent": parent, "force": true,
		}, "alice")
	}()
	response := awaitResponse(t, responses)
	if response.Code != http.StatusCreated {
		t.Fatalf("child with a busy parent: %d %s", response.Code, response.Body.String())
	}
	if actual := decodeBody[model.Issue](t, response).Parent; actual == nil || *actual != parent {
		t.Fatalf("created parent = %v, want %s", actual, parent)
	}
}

func TestConcurrentThreeWayDependenciesDoNotCommitCycle(t *testing.T) {
	handler := newTestHandler(t)
	keys := createListedIssues(t, handler, "CORE", 3)
	for round := range 20 {
		for _, key := range keys {
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"blocked_by": []string{}}, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("clear round %d: %d %s", round, response.Code, response.Body.String())
			}
		}
		start := make(chan struct{})
		responses := make(chan *httptest.ResponseRecorder, 3)
		for index, key := range keys {
			go func() {
				<-start
				responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
					"blocked_by": []string{keys[(index+1)%3]},
				}, "alice")
			}()
		}
		close(start)
		successes, refused := 0, 0
		for range 3 {
			response := awaitResponse(t, responses)
			switch {
			case response.Code == http.StatusOK:
				successes++
			case response.Code == http.StatusConflict && strings.Contains(response.Body.String(), `"code":"DEPENDENCY_CYCLE"`):
				refused++
			default:
				t.Fatalf("ring round %d: %d %s", round, response.Code, response.Body.String())
			}
		}
		edges := 0
		for index, key := range keys {
			response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("read ring: %d %s", response.Code, response.Body.String())
			}
			targets := decodeBody[model.Issue](t, response).BlockedBy
			if slices.Equal(targets, []string{keys[(index+1)%3]}) {
				edges++
			} else if len(targets) != 0 {
				t.Fatalf("unexpected ring edge: %s -> %v", key, targets)
			}
		}
		if successes != 2 || refused != 1 || edges != 2 {
			t.Fatalf("ring round %d: successes=%d refusals=%d committed edges=%d", round, successes, refused, edges)
		}
	}
}

// A board move locks its rank neighbours with every other row it names, in one key order, so while
// it waits for its gated anchor it holds its other neighbour (CORE-12 sorts before CORE-2). A
// status write on that neighbour waits for the move and finishes once the gate opens; a dependency
// write naming the neighbour finishes while the move still waits.
func TestBoardMoveStatusAndDependencyWriteDoNotDeadlock(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 12)
	anchor, parent, moving, dependent, neighbor := keys[1], keys[2], keys[3], keys[4], keys[11]
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set parent_key = $1 where key = any($2::text[])
	`, parent, []string{moving, neighbor}); err != nil {
		t.Fatal(err)
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, anchor); err != nil {
		t.Fatal(err)
	}
	moves := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		moves <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]any{
			"rank": map[string]string{"after": anchor, "before": neighbor}, "status": "todo",
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	statusWrites := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		statusWrites <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+neighbor, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	dependencyWrites := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		dependencyWrites <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+dependent, map[string]any{"blocked_by": []string{neighbor}}, "alice")
	}()
	if response := awaitResponse(t, dependencyWrites); response.Code != http.StatusOK {
		t.Fatalf("dependency write beside a waiting board move: %d %s", response.Code, response.Body.String())
	}
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if response := awaitResponse(t, moves); response.Code != http.StatusOK || decodeBody[model.Issue](t, response).Status != "todo" {
		t.Fatalf("board move: %d %s", response.Code, response.Body.String())
	}
	if response := awaitResponse(t, statusWrites); response.Code != http.StatusOK || decodeBody[model.Issue](t, response).Status != "todo" {
		t.Fatalf("status write on the board move's neighbour: %d %s", response.Code, response.Body.String())
	}
}

func TestDependencyWaitDoesNotHoldIssueRankOrNumberLocks(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	other := createListedIssues(t, handler, "SIDE", 2)
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if err := lockProjectIssueDependencies(context.Background(), gate, "CORE"); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		waiting <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[0], map[string]any{
			"blocked_by": []string{keys[1]}, "rank": map[string]string{"after": keys[1]},
		}, "alice")
	}()
	go func() {
		waiting <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": "Waiting dependent", "blocked_by": []string{keys[1]}, "force": true,
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	for _, write := range []struct {
		method string
		path   string
		body   map[string]any
		status int
	}{
		{http.MethodPatch, "/api/v1/issues/" + keys[0], map[string]any{"status": "todo"}, http.StatusOK},
		{http.MethodPatch, "/api/v1/issues/" + keys[2], map[string]any{"rank": map[string]string{"after": keys[1]}}, http.StatusOK},
		{http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Independent root", "force": true}, http.StatusCreated},
		{http.MethodPatch, "/api/v1/issues/" + other[0], map[string]any{"blocked_by": []string{other[1]}}, http.StatusOK},
	} {
		responses := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			responses <- dispatchRequest(t, handler, write.method, write.path, write.body, "alice")
		}()
		response := awaitResponse(t, responses)
		if response.Code != write.status {
			t.Fatalf("unrelated write during dependency wait: %s %s: %d %s", write.method, write.path, response.Code, response.Body.String())
		}
	}
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response := awaitResponse(t, waiting)
		if response.Code != http.StatusOK && response.Code != http.StatusCreated {
			t.Fatalf("dependency write after release: %d %s", response.Code, response.Body.String())
		}
		if got := decodeBody[model.Issue](t, response).BlockedBy; !slices.Equal(got, []string{keys[1]}) {
			t.Fatalf("dependency write lost its target: %v", got)
		}
	}
}
