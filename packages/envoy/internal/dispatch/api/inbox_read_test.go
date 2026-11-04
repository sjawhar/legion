package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// postInboxRead is one Mark all read: the body as written, from login.
func postInboxRead(t *testing.T, handler http.Handler, login string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/inbox/read", body, login)
}

// markedBy is the number of threads one Mark all read answers it marked.
func markedBy(t *testing.T, response *httptest.ResponseRecorder) int {
	t.Helper()
	return decodeBody[struct {
		Marked int `json:"marked"`
	}](t, response).Marked
}

// markedThrough is one Mark all read through at, narrowed by narrowing, and how many threads it
// answers it marked.
func markedThrough(t *testing.T, handler http.Handler, login string, at time.Time, narrowing map[string]any) int {
	t.Helper()
	body := map[string]any{"read_through": at.UTC().Format(time.RFC3339Nano)}
	for name, value := range narrowing {
		body[name] = value
	}
	response := postInboxRead(t, handler, login, body)
	if response.Code != http.StatusOK {
		t.Fatalf("mark all read %#v: status=%d body=%s", body, response.Code, response.Body.String())
	}
	return markedBy(t, response)
}

// threadMark is the caller's stored read mark on one thread and whether it is still unread.
func threadMark(t *testing.T, database *store.Store, login, threadID string) (time.Time, bool) {
	t.Helper()
	var through time.Time
	var unread bool
	if err := database.Pool.QueryRow(context.Background(), `
		select read_through, unread from user_threads where login = $1 and thread_id = $2
	`, login, threadID).Scan(&through, &unread); err != nil {
		t.Fatalf("read %s's row on %s: %v", login, threadID, err)
	}
	return through, unread
}

// markAllProjects is the project of every user_thread_state.updated a mark-all appended: the
// events naming no thread, which is how one event stands for every thread marked in its project.
func markAllProjects(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select project_key from events
		where type = 'user_thread_state.updated' and payload->>'thread_id' is null
		order by project_key
	`)
	if err != nil {
		t.Fatalf("read the mark-all events: %v", err)
	}
	defer rows.Close()
	projects := []string{}
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			t.Fatalf("scan a mark-all event: %v", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the mark-all events: %v", err)
	}
	return projects
}

// inboxReadSeed is the Inbox a mark-all works over: four unread Reply threads - two on one
// project (the first at P0 on its own issue, the second at P1 on another), one on a second
// project, one on a project document, which has no issue and so no priority - and one Agent
// thread, every reply in before cutoff.
type inboxReadSeed struct {
	handler                        http.Handler
	database                       *store.Store
	first, second, other, document string
	firstIssue, secondIssue        string
	agent, agentReply              string
	cutoff                         time.Time
}

// seedInboxRead builds that Inbox on a server whose session s1 is live, so the Agent thread's
// message reaches it and its reply comes back.
func seedInboxRead(t *testing.T) inboxReadSeed {
	t.Helper()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	seed := inboxReadSeed{handler: handler, database: database}

	first := createInteractionIssue(t, handler, "TEST", "Mark all", "A spec")
	setIssuePriority(t, handler, first.Key, 0)
	seed.firstIssue = first.Key
	seed.first = seedUnreadThread(t, handler, "/api/v1/issues/"+first.Key+"/comments", "Ready to ship?")
	second := createAnswerListIssue(t, handler, "Second")
	setIssuePriority(t, handler, second.Key, 1)
	seed.secondIssue = second.Key
	seed.second = seedUnreadThread(t, handler, "/api/v1/issues/"+second.Key+"/comments", "Who reviews this?")
	other := createInteractionIssue(t, handler, "OTHER", "Elsewhere", "A spec")
	seed.other = seedUnreadThread(t, handler, "/api/v1/issues/"+other.Key+"/comments", "Is the runbook current?")
	document := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n\nStep one.\n")
	seed.document = seedUnreadThread(t, handler, "/api/v1/artifacts/"+document.ID+"/comments", "Does step one still hold?")

	agentRoot, agentReply := agentThread(t, handler, "btw", "Deploy is tonight.", "Noted.")
	seed.agent, seed.agentReply = agentRoot.ID, agentReply.ID

	seed.cutoff = time.Now()
	return seed
}

// seedUnreadThread is one comment thread Alice wrote in that a session has replied in since: one
// unread Reply row, whatever owner the comments path names.
func seedUnreadThread(t *testing.T, handler http.Handler, path, body string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{"body": body}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("seed %s on %s: status=%d body=%s", body, path, response.Code, response.Body.String())
	}
	root := decodeBody[model.Comment](t, response)
	sessionComment(t, handler, path, map[string]any{"body": "Yes.", "reply_to": root.ID})
	return root.ID
}

// unreadThreads is the thread ids the Inbox still shows the caller, of both kinds.
func unreadThreads(t *testing.T, handler http.Handler, login string) []string {
	t.Helper()
	page := getMyReplies(t, handler, login, "?kind=reply,agent")
	threads := []string{}
	for _, row := range page.Rows {
		threads = append(threads, row.ThreadID)
	}
	slices.Sort(threads)
	return threads
}

// One request marks every unread thread the list route would answer, loaded on a page or not,
// whose newest reply is at or before the mark: the Reply threads of every owner and the Agent
// thread, whose replies are marked by id. What it leaves alone: a thread written in since the
// mark, an open ask on an open issue assigned to the caller (no Reply row of theirs: the Inbox
// lists it in Waiting on you), a thread already read past the mark, and every session's own read
// mark. It answers the number of threads it marked and appends one event per project and one per
// session it touched; a second request, which moves nothing, answers 0 and appends none.
func TestInboxReadMarksEveryUnreadThreadThroughTheMark(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database

	// An open ask on Alice's own open issue, with a session's reply in its thread: the Inbox
	// shows it in Waiting on you, so it is no Reply row and no mark-all touches it.
	assignIssue(t, handler, seed.firstIssue, "alice")
	ask := createAnswerListAsk(t, handler, seed.firstIssue, "Which way on the deploy?")
	createThreadComment(t, handler, seed.firstIssue, map[string]any{"body": "Why ask?", "ask_id": ask.ID}, "alice")
	sessionComment(t, handler, "/api/v1/issues/"+seed.firstIssue+"/comments", map[string]any{"body": "Because.", "ask_id": ask.ID})

	// A thread Alice read further than the mark she is about to send, and one written in after
	// it: the first keeps its later mark, the second stays unread.
	alreadyRead := seedUnreadThread(t, handler, "/api/v1/issues/"+seed.secondIssue+"/comments", "Read already?")
	later := seedUnreadThread(t, handler, "/api/v1/issues/"+seed.secondIssue+"/comments", "Not yet read?")
	cutoff := time.Now()
	if response := markThreadRead(t, handler, "alice", alreadyRead, cutoff.Add(time.Second)); response.Code != http.StatusNoContent {
		t.Fatalf("read %s past the mark: status=%d body=%s", alreadyRead, response.Code, response.Body.String())
	}
	readFurther, _ := threadMark(t, database, "alice", alreadyRead)
	sessionComment(t, handler, "/api/v1/issues/"+seed.secondIssue+"/comments",
		map[string]any{"body": "One more thing.", "reply_to": later})

	agentStates := countEvents(t, database, "user_agent_state.updated")
	if marked := markedThrough(t, handler, "alice", cutoff, nil); marked != 5 {
		t.Fatalf("marked = %d, want the four Reply threads and the Agent thread", marked)
	}
	if left := unreadThreads(t, handler, "alice"); !slices.Equal(left, []string{later}) {
		t.Fatalf("still unread = %#v, want the thread written in after the mark alone", left)
	}
	for _, thread := range []string{seed.first, seed.second, seed.other, seed.document} {
		if through, unread := threadMark(t, database, "alice", thread); unread || through.Before(seed.cutoff) {
			t.Fatalf("thread %s = %s unread=%v, want it read through the mark", thread, through, unread)
		}
	}
	if through, _ := threadMark(t, database, "alice", alreadyRead); !through.Equal(readFurther) {
		t.Fatalf("the thread read past the mark moved back to %s, want %s", through, readFurther)
	}
	if _, unread := threadMark(t, database, "alice", ask.ID); !unread {
		t.Fatalf("the open ask's thread was marked read, want the Inbox's Waiting on you row left alone")
	}

	// The Agent thread's replies are read by id, and no session's own read mark moves, so a reply
	// the Inbox never listed stays unread in the live conversation.
	if read := countRows(t, database, `
		select count(*) from user_agent_reply_read where login = 'alice' and reply_id = $1
	`, seed.agentReply); read != 1 {
		t.Fatalf("%d rows for the agent reply, want the one the mark wrote", read)
	}
	if marks := countRows(t, database, `select count(*) from user_agent_read where login = 'alice'`); marks != 0 {
		t.Fatalf("%d user_agent_read rows, want none: a mark-all moves no session's read mark", marks)
	}

	// One event per project touched, naming no thread, and one per session.
	if projects := markAllProjects(t, database); !slices.Equal(projects, []string{"OTHER", "TEST"}) {
		t.Fatalf("mark-all events = %#v, want one for each project it touched", projects)
	}
	if got := countEvents(t, database, "user_agent_state.updated"); got != agentStates+1 {
		t.Fatalf("%d user_agent_state.updated events, want one for the session it touched", got-agentStates)
	}

	// The same request again moves nothing: it answers 0 and announces nothing.
	if marked := markedThrough(t, handler, "alice", cutoff, nil); marked != 0 {
		t.Fatalf("the second mark-all marked %d, want 0", marked)
	}
	if projects := markAllProjects(t, database); !slices.Equal(projects, []string{"OTHER", "TEST"}) {
		t.Fatalf("mark-all events = %#v after a request that moved nothing, want the first request's two", projects)
	}
	if got := countEvents(t, database, "user_agent_state.updated"); got != agentStates+1 {
		t.Fatalf("a mark-all that moved nothing appended %d agent events, want none", got-agentStates-1)
	}
}

// Each narrowing marks what the list route would answer under the same one, and nothing else. An
// Agent thread has no issue, no document, no project and no priority, so every narrowing but kind
// leaves it unread; a Reply thread has no urgency, so an urgency the Inbox can filter by marks
// nothing at all.
func TestInboxReadMarksOnlyWhatTheNarrowingNames(t *testing.T) {
	t.Run("kind", func(t *testing.T) {
		seed := seedInboxRead(t)
		if marked := markedThrough(t, seed.handler, "alice", seed.cutoff, map[string]any{"kind": []string{"reply"}}); marked != 4 {
			t.Fatalf("marked = %d, want the four Reply threads", marked)
		}
		if left := unreadThreads(t, seed.handler, "alice"); !slices.Equal(left, []string{seed.agent}) {
			t.Fatalf("still unread = %#v, want the Agent thread alone", left)
		}
	})
	t.Run("issue", func(t *testing.T) {
		seed := seedInboxRead(t)
		if marked := markedThrough(t, seed.handler, "alice", seed.cutoff, map[string]any{"issue": seed.firstIssue}); marked != 1 {
			t.Fatalf("marked = %d, want that issue's one thread", marked)
		}
		left := unreadThreads(t, seed.handler, "alice")
		want := []string{seed.second, seed.other, seed.document, seed.agent}
		slices.Sort(want)
		if !slices.Equal(left, want) {
			t.Fatalf("still unread = %#v, want every thread outside the issue", left)
		}
	})
	t.Run("priority", func(t *testing.T) {
		seed := seedInboxRead(t)
		if marked := markedThrough(t, seed.handler, "alice", seed.cutoff, map[string]any{"priority": []string{"P0"}}); marked != 1 {
			t.Fatalf("marked = %d, want the P0 issue's one thread", marked)
		}
		left := unreadThreads(t, seed.handler, "alice")
		want := []string{seed.second, seed.other, seed.document, seed.agent}
		slices.Sort(want)
		if !slices.Equal(left, want) {
			t.Fatalf("still unread = %#v, want the P1 thread, the ones with no priority and the Agent thread", left)
		}
	})
	t.Run("project", func(t *testing.T) {
		seed := seedInboxRead(t)
		if marked := markedThrough(t, seed.handler, "alice", seed.cutoff, map[string]any{"project": []string{"TEST"}}); marked != 3 {
			t.Fatalf("marked = %d, want TEST's two issue threads and its document thread", marked)
		}
		left := unreadThreads(t, seed.handler, "alice")
		want := []string{seed.other, seed.agent}
		slices.Sort(want)
		if !slices.Equal(left, want) {
			t.Fatalf("still unread = %#v, want the other project's thread and the Agent thread", left)
		}
	})
	t.Run("urgency", func(t *testing.T) {
		seed := seedInboxRead(t)
		if marked := markedThrough(t, seed.handler, "alice", seed.cutoff, map[string]any{"urgency": []string{"high"}}); marked != 0 {
			t.Fatalf("marked = %d, want none: no thread of either kind carries an urgency", marked)
		}
		if left := unreadThreads(t, seed.handler, "alice"); len(left) != 5 {
			t.Fatalf("still unread = %#v, want all five threads", left)
		}
		if projects := markAllProjects(t, seed.database); len(projects) != 0 {
			t.Fatalf("mark-all events = %#v, want none", projects)
		}
	})
}

// Every refusal, by status and code: the mark itself, the two owner narrowings together, an
// unknown value in any list - the empty kind among them, which only the list route's own split
// of an absent ?kind= stands for - a document id that is not one, and a session, for whom the
// Inbox is nobody's.
func TestInboxReadRefusesWhatItCannotMark(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Refusals", "A spec")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for name, refusal := range map[string]struct {
		body map[string]any
		code string
	}{
		"no mark":          {map[string]any{}, "INVALID_STATE"},
		"unreadable mark":  {map[string]any{"read_through": "yesterday"}, "INVALID_STATE"},
		"mark ahead":       {map[string]any{"read_through": time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)}, "INVALID_STATE"},
		"both owners":      {map[string]any{"read_through": now, "issue": issue.Key, "artifact": "7c9e6679-7425-40de-944b-e07fc1f90ae7"}, "INVALID_QUERY"},
		"unknown kind":     {map[string]any{"read_through": now, "kind": []string{"ask"}}, "INVALID_QUERY"},
		"empty kind":       {map[string]any{"read_through": now, "kind": []string{""}}, "INVALID_QUERY"},
		"unknown priority": {map[string]any{"read_through": now, "priority": []string{"P9"}}, "INVALID_QUERY"},
		"unknown urgency":  {map[string]any{"read_through": now, "urgency": []string{"urgent"}}, "INVALID_QUERY"},
		"unreadable owner": {map[string]any{"read_through": now, "artifact": "not-a-uuid"}, "INVALID_QUERY"},
	} {
		t.Run(name, func(t *testing.T) {
			response := postInboxRead(t, handler, "alice", refusal.body)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+refusal.code+`"`) {
				t.Fatalf("status=%d body=%s, want 400 %s", response.Code, response.Body.String(), refusal.code)
			}
		})
	}
	bearer := sessionRequest(t, handler, http.MethodPost, "/api/v1/me/inbox/read", map[string]any{"read_through": now})
	if bearer.Code != http.StatusForbidden {
		t.Fatalf("a session's mark-all: status=%d body=%s, want 403", bearer.Code, bearer.Body.String())
	}
}

// A mark-all and a session's reply in one of the threads it marks, at once: neither waits on the
// other forever, the mark is never lost and the reply's time is never lost either. The owner is
// what orders this pair (the lock order is markInboxReadThreads'): the mark-all takes the
// thread's issue `for share` before it touches any user_threads row, and the comment writer
// holds that issue `for no key update` while its trigger moves those rows, so one of the two
// waits at the issue holding nothing the other wants.
func TestInboxReadRacingASessionReply(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Race", "A spec")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Racing"}, "alice")
	sessionComment(t, handler, comments, map[string]any{"body": "Reply", "reply_to": root.ID})
	marks := 0
	for round := range raceRounds {
		before, _ := threadMark(t, database, "alice", root.ID)
		mark := time.Now()
		marked, replied := race(t, round,
			func() *httptest.ResponseRecorder {
				return postInboxRead(t, handler, "alice", map[string]any{
					"read_through": mark.UTC().Format(time.RFC3339Nano),
				})
			},
			func() *httptest.ResponseRecorder {
				return sessionRequest(t, handler, http.MethodPost, comments, map[string]any{
					"body": "Reply", "reply_to": root.ID, "actor": sessionActor(),
				})
			})
		if marked.Code != http.StatusOK || replied.Code != http.StatusCreated {
			t.Fatalf("round %d: mark-all %d %s, reply %d %s", round, marked.Code, marked.Body.String(), replied.Code, replied.Body.String())
		}
		marks += markedBy(t, marked)
		reply := decodeBody[model.Comment](t, replied)
		through, repliedAt := readThrough(t, database, root.ID)
		if through.Before(before) {
			t.Fatalf("round %d: read mark %s moved back from %s", round, through, before)
		}
		if repliedAt == nil || repliedAt.Before(reply.CreatedAt) {
			t.Fatalf("round %d: replied_at %v lost the reply at %s", round, repliedAt, reply.CreatedAt)
		}
	}
	if marks == 0 {
		t.Fatalf("over %d rounds the mark-all marked nothing: the race never took the write path", raceRounds)
	}
}

// A mark-all and one thread's own PUT /api/v1/me/threads/{id}/read, for the same person and a
// thread the mark-all marks, at once. Both move that user_threads row and both append under its
// project, and the thread's owner orders neither of them: each takes it `for share`, and two
// share locks do not conflict. Only taking the row and the project in one order does
// (markInboxReadThreads), so a round where one held the row while the other held the project
// would die of a deadlock (40P01) its caller answers 500.
//
// A second unread thread in the same project keeps the mark-all on its write path whichever of
// the two reaches the shared row first: it marks that thread every round, so it takes the
// project lock every round, which is the lock the pair can cross on.
func TestInboxReadRacingOneThreadsOwnMark(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Mark race", "A spec")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	shared := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Racing"}, "alice")
	other := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Also racing"}, "alice")
	for round := range raceRounds {
		// A session's reply in each thread leaves both of Alice's rows unread again.
		sessionComment(t, handler, comments, map[string]any{"body": "Reply", "reply_to": shared.ID})
		sessionComment(t, handler, comments, map[string]any{"body": "Reply", "reply_to": other.ID})
		mark := time.Now()
		all, one := race(t, round,
			func() *httptest.ResponseRecorder {
				return postInboxRead(t, handler, "alice", map[string]any{
					"read_through": mark.UTC().Format(time.RFC3339Nano),
				})
			},
			func() *httptest.ResponseRecorder { return markThreadRead(t, handler, "alice", shared.ID, mark) })
		if all.Code != http.StatusOK || one.Code != http.StatusNoContent {
			t.Fatalf("round %d: mark-all %d %s, thread mark %d %s",
				round, all.Code, all.Body.String(), one.Code, one.Body.String())
		}
		marked := markedBy(t, all)
		if marked == 0 {
			t.Fatalf("round %d: the mark-all marked nothing, so it never took the write path", round)
		}
		// Each thread is read through the mark both writers wrote, whichever of them moved the
		// shared row: a mark that landed is never lost, and a mark that moves forward every
		// round can never have moved back.
		for name, thread := range map[string]string{"the shared thread": shared.ID, "the other thread": other.ID} {
			through, unread := threadMark(t, database, "alice", thread)
			if through.Before(mark.Truncate(time.Microsecond)) || unread {
				t.Fatalf("round %d: %s's read mark %s lost the mark %s (unread %v)", round, name, through, mark, unread)
			}
		}
	}
}

// inboxReadRaceThreads is how many threads the two presses below share. A press that holds one
// row leaves the other nothing to wait for, so the set has to be big enough that the press
// reading in heap order is holding most of it by the time it reaches the row both wait on.
const inboxReadRaceThreads = 24

// markAllOnPlan mounts a second server on deps's database whose connections scan user_threads
// with plan alone (storetest.Planned), so two presses of Mark all read reach the caller's rows
// in two orders: a press on the index pool through the primary key, in thread_id order, and one
// on the sequential pool in heap order. That is the pair production takes - a large press's
// custom plan is a bitmap heap scan, a small one's and the generic plan an index scan on the
// primary key - and with random thread ids the two orders disagree.
func markAllOnPlan(t *testing.T, deps Deps, plan storetest.Plan) http.Handler {
	t.Helper()
	deps.Store = &store.Store{Pool: storetest.Planned(t, deps.Store.Pool, plan)}
	mux := http.NewServeMux()
	served := register(mux, t.Context(), deps)
	t.Cleanup(func() { waitBackgroundOf(t, served) })
	return mux
}

// unreadAgain puts Alice's rows back behind their newest reply, which is the state a press
// marks, and lays them out in the heap in descending thread_id order, which is what gives the
// two presses two scan orders to disagree about.
//
// It deletes the rows and inserts them again rather than updating them in place. An update
// writes each new row version where its own scan reaches it only while the page has room
// elsewhere; once these rows have been rewritten a few times each new version fits in the slot
// its old one left, so the heap settles into whatever order it is already in - ascending,
// after the first press reads them through the primary key - and no re-lay moves it again.
// An insert appends in the order it is given, so the layout is this statement's to decide.
// `unread` is generated, so it is not written.
func unreadAgain(t *testing.T, database *store.Store, threads []string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		with lifted as (
			delete from user_threads
			where login = 'alice' and thread_id = any($1::uuid[])
			returning login, thread_id, kind, project_key, issue_key, artifact_id, replied_at,
			          last_reply_id
		)
		insert into user_threads (
			login, thread_id, kind, project_key, issue_key, artifact_id, read_through, replied_at,
			last_reply_id
		)
		select login, thread_id, kind, project_key, issue_key, artifact_id,
		       replied_at - interval '1 second', replied_at, last_reply_id
		from lifted order by thread_id desc
	`, threads); err != nil {
		t.Fatalf("leave the threads unread again: %v", err)
	}
}

// heapOrder is the order a sequential scan of user_threads reaches threads in, which is the
// order the press on the sequential pool locks them in.
func heapOrder(t *testing.T, database *store.Store, threads []string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select thread_id::text from user_threads
		where login = 'alice' and thread_id = any($1::uuid[])
		order by ctid
	`, threads)
	if err != nil {
		t.Fatalf("read the caller's rows in heap order: %v", err)
	}
	heap, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the caller's rows in heap order: %v", err)
	}
	if len(heap) != len(threads) {
		t.Fatalf("the heap holds %d of the %d threads", len(heap), len(threads))
	}
	return heap
}

// raceGateAttempts is how many times raceGate re-lays the rows looking for a layout the two
// presses disagree about. One update usually leaves the heap in descending thread id order, but
// a row whose new version fits in its own page keeps its place, so a round can come back in
// ascending order, where no crossing set exists. Ten attempts: the layout that defeats the gate
// turned up about one round in seven, so ten independent re-lays leave a failure this test
// cannot cause in any run anyone will see, and a layout that never crosses in ten is a real
// answer rather than a flake.
const raceGateAttempts = 10

// raceGate picks the row both presses wait on and the threads this round leaves unread for
// them to mark. The gate has to be the lowest thread id of that set, so the press reading
// through the primary key reaches it holding nothing, and it has to sit behind rows of the set
// in the heap, so the press reading in heap order is holding some of them when it gets there.
// Every set that satisfies both is a cycle; the largest is taken, which is every thread
// whenever the lowest id is not the first row of the heap.
//
// The layout is read each round rather than arranged: an update writes a row's new version
// wherever its page has room, so a round that rewrote the rows can have reordered them - and a
// round that left them in ascending thread id order offers no cycle at all. That is not a
// failure of the change under test, so this re-lays the rows and reads the heap again rather
// than failing the round, and gives up only when no attempt crosses.
func raceGate(t *testing.T, database *store.Store, threads []string) (string, []string) {
	t.Helper()
	for attempt := range raceGateAttempts {
		if attempt > 0 {
			unreadAgain(t, database, threads)
		}
		heap := heapOrder(t, database, threads)
		gate, marked := "", []string(nil)
		for position, candidate := range heap {
			above, behind := []string{candidate}, 0
			for other, thread := range heap {
				if thread > candidate {
					above = append(above, thread)
					if other < position {
						behind++
					}
				}
			}
			if behind > 0 && len(above) > len(marked) {
				gate, marked = candidate, above
			}
		}
		if gate != "" {
			slices.Sort(marked)
			return gate, marked
		}
	}
	t.Fatalf(
		"after %d re-lays the heap still holds the threads in thread id order, so the two presses would agree",
		raceGateAttempts,
	)
	return "", nil
}

// markThreadsRead takes the threads this round leaves out past their newest reply, so neither
// press reads them and neither locks them.
func markThreadsRead(t *testing.T, database *store.Store, threads []string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update user_threads set read_through = now()
		where login = 'alice' and thread_id = any($1::uuid[])
	`, threads); err != nil {
		t.Fatalf("mark the threads this round leaves out read: %v", err)
	}
}

// gateHold is one thread's row held the way a press takes it, on a connection of the test's own.
// Both presses queue behind its transaction, and the roll back that follows starts one of them
// inside the other's scan.
type gateHold struct {
	tx pgx.Tx
	// release rolls the hold back, and does it once however many callers ask: the round's own
	// goroutine releases it as soon as both presses have queued, and the cleanup below is the
	// safety net for a round that fails before that. Rolling one transaction back twice returns
	// its connection to the pool twice, and the second roll back then reads a connection another
	// request has taken and written on - a data race `go test -race` reports and fails on.
	release func()
}

func holdGateThread(t *testing.T, database *store.Store, gate string) gateHold {
	t.Helper()
	held, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("hold the gate row: %v", err)
	}
	var once sync.Once
	hold := gateHold{
		tx:      held,
		release: func() { once.Do(func() { _ = held.Rollback(context.Background()) }) },
	}
	t.Cleanup(hold.release)
	if _, err := held.Exec(context.Background(), `
		select 1 from user_threads where login = 'alice' and thread_id = $1 for no key update
	`, gate); err != nil {
		t.Fatalf("hold the gate row: %v", err)
	}
	return hold
}

// Two Mark all read presses by one person over the same threads, at once, with their query
// plans forced apart (markAllOnPlan): two tabs, two devices, or a narrowed press racing an
// unfiltered one. Both take the same owner `for share`, which orders nothing, and both move the
// same user_threads rows, so those rows are all that can order the pair - which is what the
// lock pass in thread_id order does (markInboxReadThreads, inboxReadLockThreads).
//
// The crossing is made rather than waited for. Each round picks the row both presses wait on
// and the threads they mark (raceGate), holds that row on a connection of the test's own and
// lets both presses queue behind it: the press reading through the primary key reaches it
// first, holding nothing, and the press reading in heap order reaches it holding rows of the
// set. Rolling the hold back then starts the first press inside the second's scan. Without the
// lock pass the first walks up into rows the second holds while the second waits for the row
// the first was just given, and Postgres breaks the cycle with a 40P01 its caller answers 500.
// With it the two queue at the same row in the same order, and one simply follows the other.
//
// The two presses carry marks a millisecond apart, so one of them moves every row whichever
// order they commit in.
func TestInboxReadRacingAnotherMarkAll(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Two presses", "A spec")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	threads := make([]string, 0, inboxReadRaceThreads)
	for index := range inboxReadRaceThreads {
		threads = append(threads, seedUnreadThread(t, handler, comments, fmt.Sprintf("Racing %d?", index)))
	}
	byThreadID := markAllOnPlan(t, deps, storetest.IndexScan)
	byHeap := markAllOnPlan(t, deps, storetest.SequentialScan)
	for round := range raceRounds {
		unreadAgain(t, database, threads)
		gate, marks := raceGate(t, database, threads)
		markThreadsRead(t, database, slices.DeleteFunc(slices.Clone(threads), func(thread string) bool {
			return slices.Contains(marks, thread)
		}))
		held := holdGateThread(t, database, gate)
		queued := make(chan int, 1)
		go func() {
			queued <- storetest.WaitForLockWaiters(t, held.tx, 2, 10*time.Second)
			held.release()
		}()
		mark := time.Now()
		later := mark.Add(time.Millisecond)
		first, second := race(t, round,
			func() *httptest.ResponseRecorder {
				return postInboxRead(t, byThreadID, "alice", map[string]any{
					"read_through": mark.UTC().Format(time.RFC3339Nano),
				})
			},
			func() *httptest.ResponseRecorder {
				return postInboxRead(t, byHeap, "alice", map[string]any{
					"read_through": later.UTC().Format(time.RFC3339Nano),
				})
			})
		if waiters := <-queued; waiters != 2 {
			t.Fatalf("round %d: %d presses queued on the gate row, want both of them", round, waiters)
		}
		if first.Code != http.StatusOK || second.Code != http.StatusOK {
			t.Fatalf("round %d: the press in thread_id order %d %s, the press in heap order %d %s",
				round, first.Code, first.Body.String(), second.Code, second.Body.String())
		}
		// Between them the two presses mark every thread. Which of them marks it depends on
		// where the other's commit falls: a press that reads after the other committed finds
		// nothing unread and marks none, and one that reads before it marks what is left to
		// move, so only the sum is fixed.
		marked := markedBy(t, first) + markedBy(t, second)
		if marked < len(marks) {
			t.Fatalf("round %d: the two presses marked %d threads between them, want at least %d",
				round, marked, len(marks))
		}
		// Every thread is read through at least the first press's mark, whichever press moved
		// it: a mark is stored as no later than the server's own now(), so the second press's
		// millisecond of lead is spent as often as it is stored, and no mark is ever lost.
		for _, thread := range marks {
			through, unread := threadMark(t, database, "alice", thread)
			if through.Before(mark.Truncate(time.Microsecond)) || unread {
				t.Fatalf("round %d: thread %s's read mark %s lost the mark %s (unread %v)",
					round, thread, through, mark, unread)
			}
		}
	}
}
