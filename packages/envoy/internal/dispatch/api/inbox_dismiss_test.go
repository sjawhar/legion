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

// postInboxDismiss is one Dismiss: the body as written, from login.
func postInboxDismiss(t *testing.T, handler http.Handler, login string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/inbox/dismiss", body, login)
}

// dismissedBy is the number of threads one Dismiss answers it changed.
func dismissedBy(t *testing.T, response *httptest.ResponseRecorder) int {
	t.Helper()
	return decodeBody[struct {
		Dismissed int `json:"dismissed"`
	}](t, response).Dismissed
}

// dismissRows is one Dismiss of the rows named, and how many threads it answers it changed.
func dismissRows(t *testing.T, handler http.Handler, login string, rows ...string) int {
	t.Helper()
	response := postInboxDismiss(t, handler, login, map[string]any{"threads": rows})
	if response.Code != http.StatusOK {
		t.Fatalf("dismiss %v: status=%d body=%s", rows, response.Code, response.Body.String())
	}
	return dismissedBy(t, response)
}

// restoreRows is the same request with restore, which puts the threads back in the Inbox.
func restoreRows(t *testing.T, handler http.Handler, login string, rows ...string) int {
	t.Helper()
	response := postInboxDismiss(t, handler, login, map[string]any{"threads": rows, "restore": true})
	if response.Code != http.StatusOK {
		t.Fatalf("restore %v: status=%d body=%s", rows, response.Code, response.Body.String())
	}
	return dismissedBy(t, response)
}

// dismissThrough is one Dismiss of the threads the narrowing matches at or before at, and how many
// threads it answers it changed.
func dismissThrough(t *testing.T, handler http.Handler, login string, at time.Time, narrowing map[string]any) int {
	t.Helper()
	body := map[string]any{"dismiss_through": at.UTC().Format(time.RFC3339Nano)}
	for name, value := range narrowing {
		body[name] = value
	}
	response := postInboxDismiss(t, handler, login, body)
	if response.Code != http.StatusOK {
		t.Fatalf("dismiss through %#v: status=%d body=%s", body, response.Code, response.Body.String())
	}
	return dismissedBy(t, response)
}

// threadsInState is the thread ids one state answers, sorted.
func threadsInState(t *testing.T, handler http.Handler, login, state string) []string {
	t.Helper()
	page := getMyReplies(t, handler, login, "?kind=reply,agent&state="+state)
	threads := []string{}
	for _, row := range page.Rows {
		threads = append(threads, row.ThreadID)
	}
	slices.Sort(threads)
	return threads
}

// threadDismissal is the caller's stored dismissal mark on one Reply thread, nil when the thread is
// in their Inbox.
func threadDismissal(t *testing.T, database *store.Store, login, threadID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select dismissed_at from user_threads where login = $1 and thread_id = $2
	`, login, threadID).Scan(&at); err != nil {
		t.Fatalf("read %s's dismissal of %s: %v", login, threadID, err)
	}
	return at
}

// One request dismisses exactly the threads it names, of both kinds, and takes them out of the
// Inbox list while leaving every other thread where it was. A dismissed thread is read as well, so
// it counts as unread nowhere, and the Dismissed view lists it; restore puts it back.
func TestDismissTakesTheNamedThreadsOutOfTheInbox(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database

	dismissed := dismissRows(t, handler, "alice",
		inboxDismissReplyPrefix+seed.first, inboxDismissAgentPrefix+seed.agent)
	if dismissed != 2 {
		t.Fatalf("dismissed = %d, want the Reply thread and the Agent thread", dismissed)
	}
	if at := threadDismissal(t, database, "alice", seed.first); at == nil {
		t.Fatalf("thread %s carries no dismissal mark", seed.first)
	}
	if _, unread := threadMark(t, database, "alice", seed.first); unread {
		t.Fatalf("thread %s is still unread, want a dismissal to mark it read too", seed.first)
	}
	rows := countRows(t, database, `
		select count(*) from user_agent_thread_dismissed where login = 'alice' and message_id = $1
	`, seed.agent)
	if rows != 1 {
		t.Fatalf("%d dismissal rows for the agent thread, want the one the request wrote", rows)
	}

	inbox := threadsInState(t, handler, "alice", myRepliesStateInbox)
	left := []string{seed.second, seed.other, seed.document}
	slices.Sort(left)
	if !slices.Equal(inbox, left) {
		t.Fatalf("state=inbox = %#v, want every thread the request left alone", inbox)
	}
	away := threadsInState(t, handler, "alice", myRepliesStateDismissed)
	want := []string{seed.first, seed.agent}
	slices.Sort(want)
	if !slices.Equal(away, want) {
		t.Fatalf("state=dismissed = %#v, want the two threads dismissed", away)
	}
	// Dismissing marks a thread read, so the Inbox's own count - state=unread - no longer holds
	// either of them.
	if unread := unreadThreads(t, handler, "alice"); slices.Contains(unread, seed.first) || slices.Contains(unread, seed.agent) {
		t.Fatalf("state=unread = %#v, want neither dismissed thread", unread)
	}
	// The same request again changes nothing: it answers 0 and announces nothing.
	events := countEvents(t, database, "user_thread_state.updated") + countEvents(t, database, "user_agent_state.updated")
	if again := dismissRows(t, handler, "alice", inboxDismissReplyPrefix+seed.first, inboxDismissAgentPrefix+seed.agent); again != 0 {
		t.Fatalf("the second dismiss changed %d, want 0", again)
	}
	after := countEvents(t, database, "user_thread_state.updated") + countEvents(t, database, "user_agent_state.updated")
	if after != events {
		t.Fatalf("a dismiss that changed nothing appended %d events, want none", after-events)
	}

	// restore puts both back in the Inbox, drawn as they were read.
	if restored := restoreRows(t, handler, "alice",
		inboxDismissReplyPrefix+seed.first, inboxDismissAgentPrefix+seed.agent); restored != 2 {
		t.Fatalf("restored = %d, want both threads", restored)
	}
	if at := threadDismissal(t, database, "alice", seed.first); at != nil {
		t.Fatalf("thread %s still carries the mark %s after a restore", seed.first, at)
	}
	back := threadsInState(t, handler, "alice", myRepliesStateInbox)
	slices.Sort(back)
	everything := []string{seed.first, seed.second, seed.other, seed.document, seed.agent}
	slices.Sort(everything)
	if !slices.Equal(back, everything) {
		t.Fatalf("state=inbox = %#v after the restore, want every thread", back)
	}
	if away := threadsInState(t, handler, "alice", myRepliesStateDismissed); len(away) != 0 {
		t.Fatalf("state=dismissed = %#v after the restore, want nothing", away)
	}
}

// A filtered request dismisses exactly the threads the list would answer under the same narrowing
// whose newest reply is at or before dismiss_through: a thread written in since stays in the Inbox.
// restore with the same filters puts back the ones dismissed at or before the mark.
func TestDismissTakesWhatTheFiltersMatchThroughTheMark(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	// One thread written in after the mark the request carries.
	later := seedUnreadThread(t, handler, "/api/v1/issues/"+seed.secondIssue+"/comments", "Written after?")

	if dismissed := dismissThrough(t, handler, "alice", seed.cutoff, map[string]any{"project": []string{"TEST"}}); dismissed != 3 {
		t.Fatalf("dismissed = %d, want TEST's two issue threads and its document thread", dismissed)
	}
	away := threadsInState(t, handler, "alice", myRepliesStateDismissed)
	want := []string{seed.first, seed.second, seed.document}
	slices.Sort(want)
	if !slices.Equal(away, want) {
		t.Fatalf("state=dismissed = %#v, want the project's three threads", away)
	}
	inbox := threadsInState(t, handler, "alice", myRepliesStateInbox)
	left := []string{seed.other, seed.agent, later}
	slices.Sort(left)
	if !slices.Equal(inbox, left) {
		t.Fatalf("state=inbox = %#v, want the other project's thread, the Agent thread and the one written in since", inbox)
	}
	if at := threadDismissal(t, database, "alice", later); at != nil {
		t.Fatalf("the thread written in after the mark was dismissed at %s", at)
	}

	// The whole Inbox, both kinds, through a later mark: the other project's thread, the Agent
	// thread and the thread written in since go too.
	if dismissed := dismissThrough(t, handler, "alice", time.Now(), nil); dismissed != 3 {
		t.Fatalf("dismissed = %d, want the three threads left in the Inbox", dismissed)
	}
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); len(inbox) != 0 {
		t.Fatalf("state=inbox = %#v, want nothing left in it", inbox)
	}

	// restore over the same filters brings back what was dismissed at or before its mark.
	response := postInboxDismiss(t, handler, "alice", map[string]any{
		"dismiss_through": time.Now().UTC().Format(time.RFC3339Nano),
		"project":         []string{"TEST"},
		"restore":         true,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("restore TEST: status=%d body=%s", response.Code, response.Body.String())
	}
	if restored := dismissedBy(t, response); restored != 4 {
		t.Fatalf("restored = %d, want TEST's four threads", restored)
	}
	back := threadsInState(t, handler, "alice", myRepliesStateInbox)
	slices.Sort(back)
	want = []string{seed.first, seed.second, seed.document, later}
	slices.Sort(want)
	if !slices.Equal(back, want) {
		t.Fatalf("state=inbox = %#v, want TEST's four threads back", back)
	}
	// The other project's thread and the Agent thread stay dismissed: a project list without none
	// names neither, exactly as it names neither in a mark-all.
	away = threadsInState(t, handler, "alice", myRepliesStateDismissed)
	want = []string{seed.other, seed.agent}
	slices.Sort(want)
	if !slices.Equal(away, want) {
		t.Fatalf("state=dismissed = %#v, want the threads TEST's filter never named", away)
	}

	// An unfiltered restore brings those two back as well, the Agent thread among them, which only
	// a request naming no owner, project or priority reaches.
	unfiltered := postInboxDismiss(t, handler, "alice", map[string]any{
		"dismiss_through": time.Now().UTC().Format(time.RFC3339Nano),
		"restore":         true,
	})
	if unfiltered.Code != http.StatusOK {
		t.Fatalf("restore everything: status=%d body=%s", unfiltered.Code, unfiltered.Body.String())
	}
	if restored := dismissedBy(t, unfiltered); restored != 2 {
		t.Fatalf("restored = %d, want the other project's thread and the Agent thread", restored)
	}
	if away := threadsInState(t, handler, "alice", myRepliesStateDismissed); len(away) != 0 {
		t.Fatalf("state=dismissed = %#v, want nothing dismissed", away)
	}
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); len(inbox) != 6 {
		t.Fatalf("state=inbox = %#v, want every thread of the seed back", inbox)
	}
}

// A later write by somebody else brings a dismissed thread back, unread, and the person's own reply
// does not: a reply sent from the Dismissed view leaves the thread there. That is
// user_threads_record's one assignment (migration 20261105093501) for a Reply thread, and the
// trigger of 20261105093507 for an Agent thread, whose own cases are the test below this one.
func TestDismissEndsWhenSomebodyElseWritesAgain(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	comments := "/api/v1/issues/" + seed.firstIssue + "/comments"

	if dismissed := dismissRows(t, handler, "alice",
		inboxDismissReplyPrefix+seed.first, inboxDismissAgentPrefix+seed.agent); dismissed != 2 {
		t.Fatalf("dismissed = %d, want both threads", dismissed)
	}

	// Alice's own reply in the dismissed thread leaves it dismissed.
	createThreadComment(t, handler, seed.firstIssue, map[string]any{"body": "Still mine.", "reply_to": seed.first}, "alice")
	if at := threadDismissal(t, database, "alice", seed.first); at == nil {
		t.Fatalf("thread %s came back on the caller's own reply", seed.first)
	}
	if away := threadsInState(t, handler, "alice", myRepliesStateDismissed); !slices.Contains(away, seed.first) {
		t.Fatalf("state=dismissed = %#v, want the thread the caller replied in still there", away)
	}

	// A session's reply brings it back, unread.
	sessionComment(t, handler, comments, map[string]any{"body": "One more thing.", "reply_to": seed.first})
	if at := threadDismissal(t, database, "alice", seed.first); at != nil {
		t.Fatalf("thread %s still carries the mark %s after somebody else wrote in it", seed.first, at)
	}
	if _, unread := threadMark(t, database, "alice", seed.first); !unread {
		t.Fatalf("thread %s came back read, want it unread", seed.first)
	}
	if unread := unreadThreads(t, handler, "alice"); !slices.Contains(unread, seed.first) {
		t.Fatalf("state=unread = %#v, want the thread that came back", unread)
	}

	// The Agent thread comes back on a new session reply too, whose mark the dismissal predates.
	replied := replyTo(t, handler, seed.agent, "And another thing.")
	if replied.Code != http.StatusCreated {
		t.Fatalf("reply to the agent thread: status=%d body=%s", replied.Code, replied.Body.String())
	}
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); !slices.Contains(inbox, seed.agent) {
		t.Fatalf("state=inbox = %#v, want the agent thread back", inbox)
	}
	if away := threadsInState(t, handler, "alice", myRepliesStateDismissed); slices.Contains(away, seed.agent) {
		t.Fatalf("state=dismissed = %#v, want the agent thread gone from it", away)
	}
	if unread := unreadThreads(t, handler, "alice"); !slices.Contains(unread, seed.agent) {
		t.Fatalf("state=unread = %#v, want the agent thread unread again", unread)
	}
}

// Every refusal, by status and code: an Ask row's id, which the Inbox draws no Dismiss on, a row id
// of neither shape, an id that is not a uuid, the two forms together, a missing or unparsable
// dismiss_through where the filters are used, and a session, for whom the Inbox is nobody's.
func TestDismissRefusesWhatItCannotChange(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Refusals", "A spec")
	thread := "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for name, refusal := range map[string]struct {
		body map[string]any
		code string
	}{
		"an ask row":            {map[string]any{"threads": []string{inboxDismissAskPrefix + thread}}, "INVALID_QUERY"},
		"no prefix":             {map[string]any{"threads": []string{thread}}, "INVALID_QUERY"},
		"another prefix":        {map[string]any{"threads": []string{"comment:" + thread}}, "INVALID_QUERY"},
		"not a uuid":            {map[string]any{"threads": []string{inboxDismissReplyPrefix + "not-a-uuid"}}, "INVALID_QUERY"},
		"threads with a mark":   {map[string]any{"threads": []string{inboxDismissReplyPrefix + thread}, "dismiss_through": now}, "INVALID_QUERY"},
		"threads with a filter": {map[string]any{"threads": []string{inboxDismissReplyPrefix + thread}, "issue": issue.Key}, "INVALID_QUERY"},
		"no mark":               {map[string]any{}, "INVALID_STATE"},
		"no mark with filters":  {map[string]any{"project": []string{"TEST"}}, "INVALID_STATE"},
		"unreadable mark":       {map[string]any{"dismiss_through": "yesterday"}, "INVALID_STATE"},
		"mark ahead":            {map[string]any{"dismiss_through": time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)}, "INVALID_STATE"},
		"both owners":           {map[string]any{"dismiss_through": now, "issue": issue.Key, "artifact": thread}, "INVALID_QUERY"},
		"unknown kind":          {map[string]any{"dismiss_through": now, "kind": []string{"ask"}}, "INVALID_QUERY"},
		"unknown priority":      {map[string]any{"dismiss_through": now, "priority": []string{"P9"}}, "INVALID_QUERY"},
		"unreadable owner":      {map[string]any{"dismiss_through": now, "artifact": "not-a-uuid"}, "INVALID_QUERY"},
	} {
		t.Run(name, func(t *testing.T) {
			response := postInboxDismiss(t, handler, "alice", refusal.body)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+refusal.code+`"`) {
				t.Fatalf("status=%d body=%s, want 400 %s", response.Code, response.Body.String(), refusal.code)
			}
		})
	}
	bearer := sessionRequest(t, handler, http.MethodPost, "/api/v1/me/inbox/dismiss",
		map[string]any{"threads": []string{inboxDismissReplyPrefix + thread}})
	if bearer.Code != http.StatusForbidden {
		t.Fatalf("a session's dismiss: status=%d body=%s, want 403", bearer.Code, bearer.Body.String())
	}
}

// The Dismissed view reads newest dismissal first, a page at a time from the cursor the page before
// it answered, and its own paging alone: limit and offset are refused there, as a cursor is refused
// under every other state.
func TestDismissedPagesFromItsCursor(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Dismissed order", "A spec")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	threads := []string{}
	for index := range 3 {
		thread := seedUnreadThread(t, handler, comments, fmt.Sprintf("Dismissed %d?", index))
		// One dismissal each, in turn, so the marks are in a known order.
		if dismissed := dismissRows(t, handler, "alice", inboxDismissReplyPrefix+thread); dismissed != 1 {
			t.Fatalf("dismiss %s: changed %d, want 1", thread, dismissed)
		}
		threads = append(threads, thread)
	}

	page := getMyReplies(t, handler, "alice", "?state=dismissed")
	if page.Total != 3 || len(page.Rows) != 3 || page.Limit != myRepliesDismissedPage {
		t.Fatalf("dismissed page = %#v, want the three threads at a page of %d", page, myRepliesDismissedPage)
	}
	for index, row := range page.Rows {
		if row.ThreadID != threads[len(threads)-1-index] {
			t.Fatalf("row %d = %s, want %s: newest dismissal first", index, row.ThreadID, threads[len(threads)-1-index])
		}
		if row.DismissedAt == nil {
			t.Fatalf("row %d carries no dismissal mark: %#v", index, row)
		}
	}
	// A page that did not fill names no cursor; following the cursor of the newest row reads the
	// rows under it.
	if page.NextCursor != "" {
		t.Fatalf("next_cursor = %q on a page that did not fill", page.NextCursor)
	}
	from := myRepliesCursorOf(page.Rows[0])
	next := getMyReplies(t, handler, "alice", "?state=dismissed&cursor="+from)
	if len(next.Rows) != 2 || next.Rows[0].ThreadID != threads[1] {
		t.Fatalf("page from the cursor = %#v, want the two older dismissals", next.Rows)
	}

	for query, code := range map[string]string{
		"?state=dismissed&limit=1":    "INVALID_QUERY",
		"?state=dismissed&offset=1":   "INVALID_QUERY",
		"?state=dismissed&cursor=x":   "INVALID_QUERY",
		"?state=inbox&cursor=" + from: "INVALID_QUERY",
		"?cursor=" + from:             "INVALID_QUERY",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/replies"+query, nil, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("%s: status=%d body=%s, want 400 %s", query, response.Code, response.Body.String(), code)
		}
	}
}

// Two dismisses by one person and a Mark all read beside them, over the same threads, at once: two
// tabs, two devices, or a bulk dismiss racing the button. All three move the same user_threads rows
// and all three append under the same project, so the rows and the project are all that can order
// them - which is the lock order markInboxReadThreads writes down, taken by this route too. The
// crossing is made rather than waited for, exactly as TestInboxReadRacingAnotherMarkAll makes it:
// the two dismisses run on servers whose plans are forced apart (markAllOnPlan), so they reach the
// caller's rows in two orders, and both queue behind a row the test holds.
func TestDismissRacingAnotherDismissAndAMarkAll(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Two dismisses", "A spec")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	threads := make([]string, 0, inboxReadRaceThreads)
	for index := range inboxReadRaceThreads {
		threads = append(threads, seedUnreadThread(t, handler, comments, fmt.Sprintf("Racing %d?", index)))
	}
	byThreadID := markAllOnPlan(t, deps, storetest.IndexScan)
	byHeap := markAllOnPlan(t, deps, storetest.SequentialScan)
	for round := range raceRounds {
		// Every round starts with the threads in the Inbox, unread, and laid out so the two
		// dismisses disagree about the order they reach them in.
		restoreRows(t, handler, "alice", dismissRowIDs(threads)...)
		unreadAgain(t, database, threads)
		gate, marks := raceGate(t, database, threads)
		held := holdGateThread(t, database, gate)
		queued := make(chan int, 1)
		go func() {
			queued <- storetest.WaitForLockWaiters(t, held.tx, 2, 10*time.Second)
			held.release()
		}()
		mark := time.Now()
		first, second := race(t, round,
			func() *httptest.ResponseRecorder {
				return postInboxDismiss(t, byThreadID, "alice", map[string]any{"threads": dismissRowIDs(marks)})
			},
			func() *httptest.ResponseRecorder {
				return postInboxDismiss(t, byHeap, "alice", map[string]any{"threads": dismissRowIDs(marks)})
			})
		if waiters := <-queued; waiters != 2 {
			t.Fatalf("round %d: %d dismisses queued on the gate row, want both of them", round, waiters)
		}
		if first.Code != http.StatusOK || second.Code != http.StatusOK {
			t.Fatalf("round %d: the dismiss in thread_id order %d %s, the dismiss in heap order %d %s",
				round, first.Code, first.Body.String(), second.Code, second.Body.String())
		}
		// Between them the two requests dismiss every thread once: the second finds the rows the
		// first changed already dismissed and leaves them alone, so only the sum is fixed.
		if changed := dismissedBy(t, first) + dismissedBy(t, second); changed != len(marks) {
			t.Fatalf("round %d: the two dismisses changed %d threads between them, want %d",
				round, changed, len(marks))
		}
		for _, thread := range marks {
			if at := threadDismissal(t, database, "alice", thread); at == nil {
				t.Fatalf("round %d: thread %s is in the Inbox, want it dismissed", round, thread)
			}
			if _, unread := threadMark(t, database, "alice", thread); unread {
				t.Fatalf("round %d: thread %s is unread, want a dismissal to mark it read", round, thread)
			}
		}
		// A dismiss beside the Mark all read button, which takes the same locks over the same
		// rows: both finish, and the mark never moves back.
		restoreRows(t, handler, "alice", dismissRowIDs(threads)...)
		unreadAgain(t, database, threads)
		dismiss, all := race(t, round,
			func() *httptest.ResponseRecorder {
				return postInboxDismiss(t, byThreadID, "alice", map[string]any{"threads": dismissRowIDs(threads)})
			},
			func() *httptest.ResponseRecorder {
				return postInboxRead(t, byHeap, "alice", map[string]any{
					"read_through": mark.UTC().Format(time.RFC3339Nano),
				})
			})
		if dismiss.Code != http.StatusOK || all.Code != http.StatusOK {
			t.Fatalf("round %d: dismiss %d %s, mark-all %d %s",
				round, dismiss.Code, dismiss.Body.String(), all.Code, all.Body.String())
		}
		for _, thread := range threads {
			if _, unread := threadMark(t, database, "alice", thread); unread {
				t.Fatalf("round %d: thread %s is unread after both wrote, want it read", round, thread)
			}
		}
	}
}

// dismissRowIDs is the Reply row ids of threads, as the dismiss route names them.
func dismissRowIDs(threads []string) []string {
	rows := make([]string, 0, len(threads))
	for _, thread := range threads {
		rows = append(rows, inboxDismissReplyPrefix+thread)
	}
	return rows
}

// A dismissal of a read thread leaves the Inbox's count where it was: state=unread answers the
// unread rows alone, dismissed or not, so the badge never moves on a dismiss of something read.
func TestDismissLeavesTheUnreadCountAsItWas(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	// Alice reads one thread, which leaves it in her Inbox and out of the count.
	if marked := markThreadRead(t, handler, "alice", seed.second, time.Now()); marked.Code != http.StatusNoContent {
		t.Fatalf("read the thread: status=%d body=%s", marked.Code, marked.Body.String())
	}
	before := unreadThreads(t, handler, "alice")
	if slices.Contains(before, seed.second) {
		t.Fatalf("state=unread = %#v, want the thread just read out of it", before)
	}

	if dismissed := dismissRows(t, handler, "alice", inboxDismissReplyPrefix+seed.second); dismissed != 1 {
		t.Fatalf("dismissed = %d, want the read thread", dismissed)
	}
	if after := unreadThreads(t, handler, "alice"); !slices.Equal(after, before) {
		t.Fatalf("state=unread = %#v after dismissing a read thread, want %#v", after, before)
	}
	if at := threadDismissal(t, database, "alice", seed.second); at == nil {
		t.Fatalf("thread %s carries no dismissal mark", seed.second)
	}
	// The dismissed thread is out of the Inbox list all the same.
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); slices.Contains(inbox, seed.second) {
		t.Fatalf("state=inbox = %#v, want the dismissed thread gone", inbox)
	}
}

// A row the caller has nothing of - another person's thread, or an id no thread carries - changes
// nothing and answers 0, as a read mark on a thread they never wrote in writes nothing.
func TestDismissLeavesAThreadThatIsNotTheCallersAlone(t *testing.T) {
	seed := seedInboxRead(t)
	handler := seed.handler
	// Bob signs in, so he is a person with an Inbox of his own.
	getMyReplies(t, handler, "bob", "")
	if dismissed := dismissRows(t, handler, "bob", inboxDismissReplyPrefix+seed.first); dismissed != 0 {
		t.Fatalf("bob dismissed %d of alice's threads, want 0", dismissed)
	}
	if at := threadDismissal(t, seed.database, "alice", seed.first); at != nil {
		t.Fatalf("alice's thread was dismissed at %s by bob's request", at)
	}
	if dismissed := dismissRows(t, handler, "alice",
		inboxDismissReplyPrefix+"7c9e6679-7425-40de-944b-e07fc1f90ae7"); dismissed != 0 {
		t.Fatalf("dismissing an id no thread carries changed %d, want 0", dismissed)
	}
}

// One event per project the dismissal moved rows in, naming no thread, and one per session whose
// agent thread it changed, exactly as the mark-all appends them.
func TestDismissAppendsOneEventPerProjectAndSession(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	agentStates := countEvents(t, database, "user_agent_state.updated")

	if dismissed := dismissThrough(t, handler, "alice", time.Now(), nil); dismissed != 5 {
		t.Fatalf("dismissed = %d, want the four Reply threads and the Agent thread", dismissed)
	}
	if projects := markAllProjects(t, database); !slices.Equal(projects, []string{"OTHER", "TEST"}) {
		t.Fatalf("dismiss events = %#v, want one for each project it changed", projects)
	}
	if got := countEvents(t, database, "user_agent_state.updated"); got != agentStates+1 {
		t.Fatalf("%d user_agent_state.updated events, want one for the session it touched", got-agentStates)
	}
	var payload model.UserThreadStateEventPayload
	if err := database.Pool.QueryRow(context.Background(), `
		select payload from events where type = 'user_thread_state.updated' order by id desc limit 1
	`).Scan(&payload); err != nil {
		t.Fatalf("read the newest dismiss event: %v", err)
	}
	if payload.Login != "alice" || payload.ThreadID != "" {
		t.Fatalf("event payload = %#v, want the login alone, as a mark-all's event carries", payload)
	}
}

// agentThreadDismissal is the caller's stored dismissal mark on one Agent thread, nil when the
// thread is in their Inbox.
func agentThreadDismissal(t *testing.T, database *store.Store, login, rootID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select max(dismissed_at) from user_agent_thread_dismissed where login = $1 and message_id = $2
	`, login, rootID).Scan(&at); err != nil {
		t.Fatalf("read %s's dismissal of the agent thread %s: %v", login, rootID, err)
	}
	return at
}

// writeAgentReply writes one reply into an Agent thread at a time of the test's choosing, which no
// route lets a caller name: the trigger that clears a dismissal compares the mark against the
// reply's own created_at, so a test of that comparison has to write both sides of it. parent is
// the message it replies to - the root, or a reply under it - and kind the author's kind, as the
// Inbox reads it. into is the pool, or a transaction the test holds open so the reply is written
// and not committed (holdSessionReply).
func writeAgentReply(t *testing.T, into queryer, parent, kind, id string, at time.Time) string {
	t.Helper()
	var written string
	if err := into.QueryRow(context.Background(), `
		insert into messages (issue_key, author, body, in_reply_to, target, created_at)
		select null, jsonb_build_object('kind', $2::text, 'id', $3::text), 'Written at ' || $4::text,
		       parent.id, parent.target, $4::text::timestamptz
		from messages parent where parent.id = $1
		returning id::text
	`, parent, kind, id, at.UTC().Format(time.RFC3339Nano)).Scan(&written); err != nil {
		t.Fatalf("write a %s reply under %s: %v", kind, parent, err)
	}
	return written
}

// The Agent half's dismissal is cleared by the trigger of migration 20261105093507, in the
// transaction the reply is written in, which is what makes "dismissed" on this half a mark existing
// and nothing else: no read compares the mark against the thread's newest reply, and none has to
// read a dismissed thread at all.
//
// What clears it is a session's reply newer than the mark, at any depth. What does not: a reply the
// person had already seen when they dismissed the thread - one older than the mark - and a reply of
// their own, however new, since a reply they send from the Dismissed view leaves the thread there,
// exactly as their own reply leaves a Reply thread's mark where it stands.
func TestADismissedAgentThreadComesBackOnlyOnASessionReplyPastItsMark(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	if dismissed := dismissRows(t, handler, "alice", inboxDismissAgentPrefix+seed.agent); dismissed != 1 {
		t.Fatalf("dismissed = %d, want the agent thread", dismissed)
	}
	mark := agentThreadDismissal(t, database, "alice", seed.agent)
	if mark == nil {
		t.Fatal("the dismissal wrote no mark on the agent thread")
	}

	// A session reply older than the mark, and a reply of the caller's own newer than it: the mark
	// stays, and the thread stays out of the Inbox.
	writeAgentReply(t, database.Pool, seed.agent, "session", "s1", mark.Add(-time.Minute))
	writeAgentReply(t, database.Pool, seed.agent, "user", "alice", mark.Add(time.Minute))
	if at := agentThreadDismissal(t, database, "alice", seed.agent); at == nil {
		t.Fatal("the agent thread's mark is gone after a reply older than it and a reply of the caller's own")
	}
	if away := threadsInState(t, handler, "alice", myRepliesStateDismissed); !slices.Contains(away, seed.agent) {
		t.Fatalf("state=dismissed = %#v, want the agent thread still there", away)
	}
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); slices.Contains(inbox, seed.agent) {
		t.Fatalf("state=inbox = %#v, want the agent thread still out of it", inbox)
	}

	// A session reply newer than the mark, written two deep - under the reply already in the
	// thread, which is where a follow-up lands - so the clear is the walk up in_reply_to and not a
	// match on the root alone: the mark is gone and the thread is back, unread.
	writeAgentReply(t, database.Pool, seed.agentReply, "session", "s1", mark.Add(2*time.Minute))
	if at := agentThreadDismissal(t, database, "alice", seed.agent); at != nil {
		t.Fatalf("the agent thread still carries the mark %s after a session replied past it", at)
	}
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); !slices.Contains(inbox, seed.agent) {
		t.Fatalf("state=inbox = %#v, want the agent thread back", inbox)
	}
	if away := threadsInState(t, handler, "alice", myRepliesStateDismissed); slices.Contains(away, seed.agent) {
		t.Fatalf("state=dismissed = %#v, want the agent thread gone from it", away)
	}
	if unread := unreadThreads(t, handler, "alice"); !slices.Contains(unread, seed.agent) {
		t.Fatalf("state=unread = %#v, want the agent thread unread again", unread)
	}
}

// threadArtifact is the document a Reply thread hangs under, as the caller's own row names it.
// The seed knows its threads by id and the Inbox never answers an owner, so the owner a request
// locks is read back from the row the trigger wrote.
func threadArtifact(t *testing.T, database *store.Store, login, thread string) string {
	t.Helper()
	var artifact string
	if err := database.Pool.QueryRow(context.Background(), `
		select artifact_id::text from user_threads where login = $1 and thread_id = $2
	`, login, thread).Scan(&artifact); err != nil {
		t.Fatalf("read the document %s's thread %s hangs under: %v", login, thread, err)
	}
	return artifact
}

// holdGateDocument holds one document's row the way a write to it takes it, on a connection of
// the test's own, so a dismiss that locks that document queues behind this transaction. It
// releases once however many callers ask, as holdGateThread does and for the same reason.
func holdGateDocument(t *testing.T, database *store.Store, document string) gateHold {
	t.Helper()
	held, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("hold the gate document's row: %v", err)
	}
	var once sync.Once
	hold := gateHold{
		tx:      held,
		release: func() { once.Do(func() { _ = held.Rollback(context.Background()) }) },
	}
	t.Cleanup(hold.release)
	if _, err := held.Exec(context.Background(), `
		select 1 from artifacts where id = $1 for no key update
	`, document); err != nil {
		t.Fatalf("hold the gate document's row: %v", err)
	}
	return hold
}

// A dismiss reads the rows it changes before it writes their marks, and between the two it waits
// for every lock the order takes. A reply by somebody else that commits inside that window leaves
// its thread in the Inbox, unread: the alternative is a thread dismissed under a mark older than
// a reply nobody has read, which nothing undoes - user_threads_record and the trigger of
// 20261105093507 each clear a mark that is already there, and a mark written after they have run
// is not.
//
// The window is made rather than waited for, as TestInboxReadRacingAnotherMarkAll makes its
// crossing, and it is the real one rather than a thread back-dated into it: every row holds the
// row of a document the request dismisses a thread of. That document is step 1 of the request's
// lock order (markInboxReadThreads, api/inbox_read.go), taken after the read and before the
// issues and every write, so a request queued on it has read its rows and written none, and
// WaitForStatementBehind watches for that one statement rather than counting whoever waits. The
// reply each row then writes lands somewhere the gate does not reach - another issue's thread, or
// an Agent thread, which has no owner row at all - so it commits while the dismiss waits.
//
// The Agent row dismisses a Reply thread beside its root for that gate alone: an agent-only
// request locks no owner, and the one row it would queue on, its own dismissal, is the row it is
// about to write.
//
// What no row here reaches is the reply whose own transaction commits after the dismiss's write
// statement has begun. On the Reply half that one is ordered by the rows step 3 of that order
// locks: the trigger's update waits on them and clears the mark behind it. On the Agent half it
// is ordered by the thread's root, which the dismiss takes at step 2 and the clearing trigger
// takes `for share` before its delete - the pair the two tests below this one drive, each from
// one side: the reply that holds the root while the dismiss waits for it, and the dismiss that
// holds it while the reply waits.
func TestDismissLeavesAThreadRepliedToWhileItRan(t *testing.T) {
	for name, row := range map[string]struct {
		// body is the dismiss under test, and write the reply by somebody else that commits
		// while it waits. thread is the row that must come out of it still in the Inbox, and
		// mark its stored dismissal, which must be none.
		body   func(seed inboxReadSeed) map[string]any
		write  func(t *testing.T, seed inboxReadSeed)
		thread func(seed inboxReadSeed) string
		mark   func(t *testing.T, seed inboxReadSeed) *time.Time
	}{
		"a named reply dismiss": {
			body: func(seed inboxReadSeed) map[string]any {
				return map[string]any{"threads": []string{
					inboxDismissReplyPrefix + seed.document, inboxDismissReplyPrefix + seed.first,
				}}
			},
			write: func(t *testing.T, seed inboxReadSeed) {
				sessionComment(t, seed.handler, "/api/v1/issues/"+seed.firstIssue+"/comments",
					map[string]any{"body": "One more thing.", "reply_to": seed.first})
			},
			thread: func(seed inboxReadSeed) string { return seed.first },
			mark: func(t *testing.T, seed inboxReadSeed) *time.Time {
				return threadDismissal(t, seed.database, "alice", seed.first)
			},
		},
		"a filtered reply dismiss": {
			body: func(seed inboxReadSeed) map[string]any {
				return map[string]any{
					"dismiss_through": time.Now().UTC().Format(time.RFC3339Nano),
					"kind":            []string{myReplyKindReply},
				}
			},
			write: func(t *testing.T, seed inboxReadSeed) {
				sessionComment(t, seed.handler, "/api/v1/issues/"+seed.firstIssue+"/comments",
					map[string]any{"body": "One more thing.", "reply_to": seed.first})
			},
			thread: func(seed inboxReadSeed) string { return seed.first },
			mark: func(t *testing.T, seed inboxReadSeed) *time.Time {
				return threadDismissal(t, seed.database, "alice", seed.first)
			},
		},
		"an agent dismiss": {
			body: func(seed inboxReadSeed) map[string]any {
				return map[string]any{"threads": []string{
					inboxDismissReplyPrefix + seed.document, inboxDismissAgentPrefix + seed.agent,
				}}
			},
			write: func(t *testing.T, seed inboxReadSeed) {
				replied := replyTo(t, seed.handler, seed.agent, "And another thing.")
				if replied.Code != http.StatusCreated {
					t.Fatalf("reply to the agent thread: status=%d body=%s", replied.Code, replied.Body.String())
				}
			},
			thread: func(seed inboxReadSeed) string { return seed.agent },
			mark: func(t *testing.T, seed inboxReadSeed) *time.Time {
				return agentThreadDismissal(t, seed.database, "alice", seed.agent)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			seed := seedInboxRead(t)
			handler, database := seed.handler, seed.database
			held := holdGateDocument(t, database, threadArtifact(t, database, "alice", seed.document))
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() { answered <- postInboxDismiss(t, handler, "alice", row.body(seed)) }()
			if storetest.WaitForStatementBehind(t, database, held.tx, inboxReadLockDocuments, 10*time.Second) == 0 {
				t.Fatal("the dismiss never queued on the document row it locks before it writes")
			}
			row.write(t, seed)
			held.release()
			response := <-answered
			if response.Code != http.StatusOK {
				t.Fatalf("the dismiss answered %d %s", response.Code, response.Body.String())
			}
			// The gate thread, which nobody wrote in, is dismissed: the request ran whole, so
			// what the thread under test shows is this one comparison and not a request lost.
			if at := threadDismissal(t, database, "alice", seed.document); at == nil {
				t.Fatal("the document thread carries no mark, so the dismiss wrote nothing at all")
			}
			thread := row.thread(seed)
			if at := row.mark(t, seed); at != nil {
				t.Fatalf("thread %s was dismissed at %s, past a reply nobody has read", thread, at)
			}
			if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); !slices.Contains(inbox, thread) {
				t.Fatalf("state=inbox = %#v, want the thread replied to while the dismiss ran", inbox)
			}
			if away := threadsInState(t, handler, "alice", myRepliesStateDismissed); slices.Contains(away, thread) {
				t.Fatalf("state=dismissed = %#v, want the thread replied to while the dismiss ran left out of it", away)
			}
			if unread := unreadThreads(t, handler, "alice"); !slices.Contains(unread, thread) {
				t.Fatalf("state=unread = %#v, want the thread replied to while the dismiss ran unread", unread)
			}
		})
	}
}

// holdSessionReply writes one session reply into an Agent thread, at the moment it is called, on a
// transaction of the test's own and leaves that transaction open: the row is written and the
// trigger of 20261105093507 has run over it, and neither is committed. That is a reply in flight,
// which no route can be asked for - every reply route commits before it answers - and it is what
// the window below is made of. commit lands it; the roll back the cleanup makes is the safety net
// for a round that fails before that, and never runs after a commit, since rolling one transaction
// back twice returns its connection to the pool twice (gateHold, api/inbox_read_test.go).
func holdSessionReply(t *testing.T, database *store.Store, parent string) (pgx.Tx, func()) {
	t.Helper()
	replying, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the reply this test holds: %v", err)
	}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { _ = replying.Rollback(context.Background()) }) })
	writeAgentReply(t, replying, parent, "session", "s1", time.Now())
	return replying, func() {
		once.Do(func() {
			if err := replying.Commit(context.Background()); err != nil {
				t.Errorf("commit the reply this test held: %v", err)
			}
		})
	}
}

// The reply first, which is the half of the window the test above cannot reach: a session reply
// written and not committed when the dismiss reaches the Agent thread's root. The reply holds that
// root - the clearing trigger of 20261105093507 takes it `for share` before its delete - so the
// dismiss waits there (inboxDismissLockAgentRoots, step 2 of the lock order), and the insert that
// follows, a statement of its own and so a snapshot of its own, holds the committed reply and
// writes no mark for its thread. The thread ends where a reply nobody has read leaves it: in the
// Inbox, unread.
//
// Without that lock the dismiss waits nowhere, its insert reads a snapshot the reply is not in and
// writes the mark, and the reply's trigger has already run and found nothing to delete - a thread
// dismissed under a reply nobody has read, which is what this row is red for.
//
// The interleave is made rather than waited for, as the test above makes its own. The gate is the
// document of the Reply thread the request names, step 1, taken after the read and before every
// lock and every write: a dismiss queued there has taken its transaction's own now(), the mark it
// will store, so the reply this test then writes is newer than that mark and the comparison in the
// insert is not what leaves the thread alone. The reply goes two deep, under the reply already in
// the thread, where a follow-up lands: its own foreign key then holds its parent and not the root,
// so the only lock on the root is the trigger's.
func TestDismissLeavesAnAgentThreadWhoseReplyCommitsAsItWrites(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	held := holdGateDocument(t, database, threadArtifact(t, database, "alice", seed.document))
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- postInboxDismiss(t, handler, "alice", map[string]any{"threads": []string{
			inboxDismissReplyPrefix + seed.document, inboxDismissAgentPrefix + seed.agent,
		}})
	}()
	dismissing := storetest.WaitForStatementBehind(t, database, held.tx, inboxReadLockDocuments, 10*time.Second)
	if dismissing == 0 {
		t.Fatal("the dismiss never queued on the document row it locks before it writes")
	}
	replying, commit := holdSessionReply(t, database, seed.agentReply)
	held.release()
	if answer, early := storetest.WaitForQueuedBehind(t, replying, dismissing, answered, 10*time.Second); early {
		t.Fatalf("the dismiss answered %d %s without ever waiting at the agent thread's root, so its insert read a snapshot the reply is not in",
			answer.Code, answer.Body.String())
	}
	commit()

	response := <-answered
	if response.Code != http.StatusOK {
		t.Fatalf("the dismiss answered %d %s", response.Code, response.Body.String())
	}
	// The gate thread, which nobody wrote in, is dismissed and counted, and the agent thread is
	// neither: the request ran whole, so what the agent thread shows is this one window.
	if dismissed := dismissedBy(t, response); dismissed != 1 {
		t.Fatalf("dismissed = %d, want the gate thread alone", dismissed)
	}
	if at := threadDismissal(t, database, "alice", seed.document); at == nil {
		t.Fatal("the document thread carries no mark, so the dismiss wrote nothing at all")
	}
	if at := agentThreadDismissal(t, database, "alice", seed.agent); at != nil {
		t.Fatalf("the agent thread was dismissed at %s, past a reply nobody has read", at)
	}
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); !slices.Contains(inbox, seed.agent) {
		t.Fatalf("state=inbox = %#v, want the agent thread replied to while the dismiss wrote", inbox)
	}
	if away := threadsInState(t, handler, "alice", myRepliesStateDismissed); slices.Contains(away, seed.agent) {
		t.Fatalf("state=dismissed = %#v, want the agent thread replied to while the dismiss wrote left out of it", away)
	}
	if unread := unreadThreads(t, handler, "alice"); !slices.Contains(unread, seed.agent) {
		t.Fatalf("state=unread = %#v, want the agent thread replied to while the dismiss wrote unread", unread)
	}
}

// The dismiss first, the same pair the other way round, which is what says the lock did not simply
// make the dismiss win. The gate here is the caller's own row for the Reply thread the request
// names, step 3 of the lock order, so the dismiss queued there is holding the Agent root it took
// at step 2; the reply then arrives, waits at that root, and the dismiss commits its mark first.
// The reply's trigger is given the root next, finds a mark committed and older than the reply it
// is writing, and deletes it in the reply's own transaction: the thread comes back, unread, and
// the count the dismiss answered says the mark it deleted was really written.
func TestADismissHoldingTheAgentRootGivesTheThreadBackOnTheReply(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	held := holdGateThread(t, database, seed.document)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- postInboxDismiss(t, handler, "alice", map[string]any{"threads": []string{
			inboxDismissReplyPrefix + seed.document, inboxDismissAgentPrefix + seed.agent,
		}})
	}()
	if storetest.WaitForStatementBehind(t, database, held.tx, inboxReadLockThreads, 10*time.Second) == 0 {
		t.Fatal("the dismiss never queued on the caller's rows, which it locks after the agent thread's root")
	}
	replied := make(chan *httptest.ResponseRecorder, 1)
	go func() { replied <- replyTo(t, handler, seed.agent, "And another thing.") }()
	// Five seconds, where the dismiss's statement_timeout allows it ten at the gate: a reply that
	// has not queued by then is this row's red, and waiting the full ten would spend the dismiss's
	// own allowance on it and answer 500 instead.
	if waiting := storetest.WaitForLockWaiters(t, held.tx, 2, 5*time.Second); waiting < 2 {
		t.Fatalf("%d transactions queued behind the gate row, want the dismiss and the reply waiting at the root it holds", waiting)
	}
	held.release()

	response := <-answered
	if response.Code != http.StatusOK {
		t.Fatalf("the dismiss answered %d %s", response.Code, response.Body.String())
	}
	if dismissed := dismissedBy(t, response); dismissed != 2 {
		t.Fatalf("dismissed = %d, want both threads: the dismiss had the root first and wrote the agent thread's mark", dismissed)
	}
	reply := <-replied
	if reply.Code != http.StatusCreated {
		t.Fatalf("the reply answered %d %s", reply.Code, reply.Body.String())
	}
	if at := threadDismissal(t, database, "alice", seed.document); at == nil {
		t.Fatal("the document thread carries no mark, so the dismiss wrote nothing at all")
	}
	if at := agentThreadDismissal(t, database, "alice", seed.agent); at != nil {
		t.Fatalf("the agent thread still carries the mark %s, so the reply's trigger found none to delete", at)
	}
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); !slices.Contains(inbox, seed.agent) {
		t.Fatalf("state=inbox = %#v, want the agent thread back on the reply", inbox)
	}
	if away := threadsInState(t, handler, "alice", myRepliesStateDismissed); slices.Contains(away, seed.agent) {
		t.Fatalf("state=dismissed = %#v, want the agent thread gone from it", away)
	}
	if unread := unreadThreads(t, handler, "alice"); !slices.Contains(unread, seed.agent) {
		t.Fatalf("state=unread = %#v, want the agent thread unread again", unread)
	}
}
