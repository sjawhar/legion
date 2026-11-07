package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

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

// The two statements a test reads a row's drawn state out of, which is what the page would have
// shown in it and what a dismissal of that row carries (seen_replies): for a Reply row the
// caller's own user_threads.version, and for an Agent row the newest session reply in the root's
// thread with the ids drawn as unread there, read through the list route's own lateral
// (myAgentRootsWithFacts), so a test draws what the page draws.
const seenReplyVersions = `
	select t.thread_id::text, t.version from user_threads t
	where t.login = $1 and t.thread_id = any($2::uuid[])`

var seenAgentDrawn = myAgentMarks + `
	roots as (select $3::uuid as root_id)
	select agent.reply_id::text, agent.unread_reply_ids` + myAgentRootsWithFacts

// seenRepliesOf is the seen_replies a page would carry for the rows named: one entry per row,
// read out of the state the test seeded rather than written down. A Reply row the caller has no
// row for is carried at version 0, the version every row starts at.
func seenRepliesOf(t *testing.T, database *store.Store, login string, rows []string) map[string]any {
	t.Helper()
	ctx := context.Background()
	threads := []string{}
	for _, row := range rows {
		if thread, isReply := strings.CutPrefix(row, inboxDismissReplyPrefix); isReply {
			threads = append(threads, thread)
		}
	}
	versions := map[string]int64{}
	if len(threads) > 0 {
		read, err := database.Pool.Query(ctx, seenReplyVersions, login, threads)
		if err != nil {
			t.Fatalf("read the versions of %v: %v", threads, err)
		}
		defer read.Close()
		for read.Next() {
			var thread string
			var version int64
			if err := read.Scan(&thread, &version); err != nil {
				t.Fatalf("scan a thread's version: %v", err)
			}
			versions[inboxDismissReplyPrefix+thread] = version
		}
		if err := read.Err(); err != nil {
			t.Fatalf("read the versions of %v: %v", threads, err)
		}
	}
	seen := map[string]any{}
	for _, row := range rows {
		if root, isAgent := strings.CutPrefix(row, inboxDismissAgentPrefix); isAgent {
			var newest string
			unread := []string{}
			if err := database.Pool.QueryRow(ctx, seenAgentDrawn, login, login, root).
				Scan(&newest, &unread); err != nil {
				t.Fatalf("read what the page drew in %s: %v", root, err)
			}
			seen[row] = map[string]any{"newest_reply": newest, "unread_replies": unread}
			continue
		}
		seen[row] = map[string]any{"version": versions[row]}
	}
	return seen
}

// dismissBody is one named dismissal's body: the rows it names and what the page drew in each of
// them, which every named dismissal carries.
func dismissBody(t *testing.T, database *store.Store, login string, rows []string) map[string]any {
	t.Helper()
	return map[string]any{"threads": rows, "seen_replies": seenRepliesOf(t, database, login, rows)}
}

// dismissRows is one Dismiss of the rows named, and how many threads it answers it changed.
func dismissRows(t *testing.T, handler http.Handler, database *store.Store, login string, rows ...string) int {
	t.Helper()
	response := postInboxDismiss(t, handler, login, dismissBody(t, database, login, rows))
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

// threadRowState is what one Reply row says about the writes in its thread: the version a
// dismissal of it is held to (inboxDismissMarkThreads), and the two columns a write usually
// moves beside it. lastReply is empty and repliedAt is -infinity on a row nobody has written in.
type threadRowState struct {
	version   int64
	lastReply string
	repliedAt time.Time
}

// threadRowWrites reads that state off the caller's own row, which is where a test holding the
// version's rule - every write by somebody else moves it, whatever that write's time or id -
// reads the three of them from.
func threadRowWrites(t *testing.T, database *store.Store, login, threadID string) threadRowState {
	t.Helper()
	row := threadRowState{}
	if err := database.Pool.QueryRow(context.Background(), `
		select version, coalesce(last_reply_id::text, ''), coalesce(replied_at, '-infinity'::timestamptz)
		from user_threads where login = $1 and thread_id = $2
	`, login, threadID).Scan(&row.version, &row.lastReply, &row.repliedAt); err != nil {
		t.Fatalf("read %s's row for %s: %v", login, threadID, err)
	}
	return row
}

// One request dismisses exactly the threads it names, of both kinds, and takes them out of the
// Inbox list while leaving every other thread where it was. A dismissed thread is read as well, so
// it counts as unread nowhere, and the Dismissed view lists it; restore puts it back.
func TestDismissTakesTheNamedThreadsOutOfTheInbox(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database

	dismissed := dismissRows(t, handler, database, "alice",
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
	if again := dismissRows(t, handler, database, "alice",
		inboxDismissReplyPrefix+seed.first, inboxDismissAgentPrefix+seed.agent); again != 0 {
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

	if dismissed := dismissRows(t, handler, database, "alice",
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

// A changed ask answer is the write that moves no id at all: an answer's write is the ask itself
// (migration 0096 passes new.id as p_write), so user_threads_record assigns last_reply_id the id
// the row already held. The version is what says the write landed, and a press carrying the
// version the page drew before the change writes nothing - the answer that page showed is not
// the answer the thread holds now, so the thread stays in the Inbox, unread.
//
// A dismissal held to last_reply_id dismisses this thread over an answer nobody has read, which
// is why the row reads both columns off user_threads rather than taking the route's word for it.
func TestDismissLeavesAnAskThreadWhoseAnswerChanged(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A changed answer", "A spec")
	// Alice asks, which is what gives her a row in the ask's own thread (migration 0096's
	// insert branch), and Bob answers, the write that makes that row unread.
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Should we ship?",
		"options":  []map[string]string{{"label": "Ship"}, {"label": "Hold"}},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("alice asks: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)
	first := answerAskForChange(t, handler, ask.ID, "bob", "Ship", nil)
	if first.Answer == nil {
		t.Fatal("bob's answer is missing")
	}
	row := inboxDismissReplyPrefix + ask.ID
	body := dismissBody(t, database, "alice", []string{row})
	drawn := threadRowWrites(t, database, "alice", ask.ID)
	if drawn.lastReply != ask.ID {
		t.Fatalf("last_reply_id = %q, want the ask's own id %s, which 0096 passes as the answer's write",
			drawn.lastReply, ask.ID)
	}

	// Bob changes his answer, the write this row is about.
	answeredAt := timestampValue(first.Answer.At)
	answerAskForChange(t, handler, ask.ID, "bob", "Hold", &answeredAt)
	changed := threadRowWrites(t, database, "alice", ask.ID)
	if changed.lastReply != drawn.lastReply {
		t.Fatalf("last_reply_id %q after the change, want the %q it already held: this row is about the write that moves no id",
			changed.lastReply, drawn.lastReply)
	}
	if changed.version == drawn.version {
		t.Fatalf("version %d after a changed answer, want it moved off the %d the page drew",
			changed.version, drawn.version)
	}

	response := postInboxDismiss(t, handler, "alice", body)
	if response.Code != http.StatusOK {
		t.Fatalf("the dismiss answered %d %s", response.Code, response.Body.String())
	}
	if dismissed := dismissedBy(t, response); dismissed != 0 {
		t.Fatalf("dismissed = %d, want none: the answer the page drew is not the answer the thread holds", dismissed)
	}
	if at := threadDismissal(t, database, "alice", ask.ID); at != nil {
		t.Fatalf("the ask's thread was dismissed at %s, over an answer nobody has read", at)
	}
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); !slices.Contains(inbox, ask.ID) {
		t.Fatalf("state=inbox = %#v, want the ask's thread still in it", inbox)
	}
	if unread := unreadThreads(t, handler, "alice"); !slices.Contains(unread, ask.ID) {
		t.Fatalf("state=unread = %#v, want the ask's thread unread", unread)
	}
	// A press from a page that drew the changed answer dismisses it, so what turned the press
	// above away is the version it carried and not the row.
	if dismissed := dismissRows(t, handler, database, "alice", row); dismissed != 1 {
		t.Fatalf("a press at the version the row holds now changed %d, want the ask's thread", dismissed)
	}
}

// The stale page on the Agent half: a session reply lands after the page drew the row, and the
// press carries what that page drew. What refuses it is the newest-reply equality alone
// (inboxDismissCheckAgentRoots): by the time the press arrives nothing in the thread is unread,
// since the person read the conversation in the Agents page their Inbox tab never redrew, so
// every unread reply is one the page drew and the drawn-unread clause passes.
//
// This is the row the equality is kept for. Every other row of a reply the page never drew is
// refused by the drawn-unread clause, so without this one a later reader would take the
// equality for a second spelling of the same test and drop it - and dropping it buries a reply
// the page never showed. What it costs the Inbox is one more press: the row comes back drawn as
// read, and a press from that page dismisses it.
func TestDismissRefusesAnAgentRootWhoseNewestReplyThePageNeverDrew(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	row := inboxDismissAgentPrefix + seed.agent
	body := dismissBody(t, database, "alice", []string{row})

	replied := replyTo(t, handler, seed.agent, "And another thing.")
	if replied.Code != http.StatusCreated {
		t.Fatalf("reply to the agent thread: status=%d body=%s", replied.Code, replied.Body.String())
	}
	read := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"read_through": time.Now().UTC().Format(time.RFC3339Nano),
	}, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("alice reads the conversation: status=%d body=%s", read.Code, read.Body.String())
	}
	drawn, _ := body["seen_replies"].(map[string]any)[row].(map[string]any)
	landed, _ := seenRepliesOf(t, database, "alice", []string{row})[row].(map[string]any)
	if unread, _ := landed["unread_replies"].([]string); len(unread) != 0 {
		t.Fatalf("the thread holds %v unread, want none: this row is about what the drawn-unread clause lets through", unread)
	}
	if drawn["newest_reply"] == landed["newest_reply"] {
		t.Fatalf("the thread's newest reply is still the %v the page drew, so the press carries nothing stale", drawn["newest_reply"])
	}

	dismissChangedNothing(t, postInboxDismiss(t, handler, "alice", body),
		"the thread's newest reply is not the one the page drew")
	// The thread is left in the Inbox, read: the person read the conversation in another page,
	// which is why the press reaches the newest-reply equality at all, so this row ends where
	// api/inbox_dismiss_race_test.go's four end but for the unread count
	// (threadLeftUnreadInTheInbox).
	if at := agentThreadDismissal(t, database, "alice", seed.agent); at != nil {
		t.Fatalf("the agent thread was dismissed at %s, over a reply the page never drew", at)
	}
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); !slices.Contains(inbox, seed.agent) {
		t.Fatalf("state=inbox = %#v, want the agent thread still in it", inbox)
	}
	// The one more press, from a page that drew the reply that landed.
	if dismissed := dismissRows(t, handler, database, "alice", row); dismissed != 1 {
		t.Fatalf("a press from a page that drew the reply changed %d, want the agent thread", dismissed)
	}
}

// Every refusal, by status and code. The route's own, which say nothing of what the page drew:
// an Ask row's id, which the Inbox draws no Dismiss on, a row id of neither shape, an id that is
// not a uuid, the two forms together, a missing or unreadable dismiss_through where the filters
// are used, a narrowing the list itself refuses, and a session, for whom the Inbox is nobody's.
//
// Then the seven seen_replies answers, what a named dismissal carries for every row it names,
// with a row here for every shape each is refused in (readInboxDismissSeen and dismissInbox):
// a row threads names and seen_replies leaves out, which is also what a client written before
// the page's draw was asked for sends; an entry for a row threads does not name, which is a page
// asking about a row it did not ask to dismiss; a reply: entry that is not that row's one
// version; an agent: entry that is not the newest_reply and unread_replies the page drew - each
// of those two refused where the entry or one of its values is of the wrong type, where the
// row's own field is left out, and where the other kind's field is carried, since an entry is
// read in its own kind's shape and no other; an id that is not a uuid; and the map on a restore,
// which checks nothing, or beside the filters, where the server draws the rows itself.
func TestDismissRefusesWhatItCannotChange(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Refusals", "A spec")
	thread := "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	root := "0a4f4b0c-2c3b-4f6a-9b6a-7a1f0d2e5c48"
	other := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	drawnReply := "b1e9c0d2-5f84-4a3e-9c21-6d7f8e9a0b13"
	replyRow := inboxDismissReplyPrefix + thread
	agentRow := inboxDismissAgentPrefix + root
	// What a page that drew each of those two rows whole carries for it, which every entry below
	// is one field away from: a Reply row's own version, and an Agent row's newest session reply
	// with the ids it drew as unread.
	replyDrawn := map[string]any{"version": 3}
	agentDrawn := map[string]any{"newest_reply": drawnReply, "unread_replies": []string{drawnReply}}
	// named is one dismissal of that row alone, carrying entry as what the page drew in it: the
	// request every seen_replies row below is, with the entry its own.
	named := func(row string, entry any) map[string]any {
		return map[string]any{"threads": []string{row}, "seen_replies": map[string]any{row: entry}}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for name, refusal := range map[string]struct {
		body map[string]any
		code string
	}{
		"an ask row":            {map[string]any{"threads": []string{inboxDismissAskPrefix + thread}}, "INVALID_QUERY"},
		"no prefix":             {map[string]any{"threads": []string{thread}}, "INVALID_QUERY"},
		"another prefix":        {map[string]any{"threads": []string{"comment:" + thread}}, "INVALID_QUERY"},
		"not a uuid":            {map[string]any{"threads": []string{inboxDismissReplyPrefix + "not-a-uuid"}}, "INVALID_QUERY"},
		"threads with a mark":   {map[string]any{"threads": []string{replyRow}, "dismiss_through": now}, "INVALID_QUERY"},
		"threads with a filter": {map[string]any{"threads": []string{replyRow}, "issue": issue.Key}, "INVALID_QUERY"},
		"no mark":               {map[string]any{}, "INVALID_STATE"},
		"no mark with filters":  {map[string]any{"project": []string{"TEST"}}, "INVALID_STATE"},
		"unreadable mark":       {map[string]any{"dismiss_through": "yesterday"}, "INVALID_STATE"},
		"mark ahead":            {map[string]any{"dismiss_through": time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)}, "INVALID_STATE"},
		"both owners":           {map[string]any{"dismiss_through": now, "issue": issue.Key, "artifact": thread}, "INVALID_QUERY"},
		"unknown kind":          {map[string]any{"dismiss_through": now, "kind": []string{"ask"}}, "INVALID_QUERY"},
		"unknown priority":      {map[string]any{"dismiss_through": now, "priority": []string{"P9"}}, "INVALID_QUERY"},
		"unreadable owner":      {map[string]any{"dismiss_through": now, "artifact": "not-a-uuid"}, "INVALID_QUERY"},
		// A row threads names and seen_replies leaves out: the whole map missing, which is the
		// client that predates it, and a map that carries the other row the request named.
		"no seen_replies at all": {map[string]any{"threads": []string{replyRow}}, "INVALID_QUERY"},
		"a named row left out": {map[string]any{"threads": []string{replyRow, agentRow},
			"seen_replies": map[string]any{agentRow: agentDrawn}}, "INVALID_QUERY"},
		// A key naming no row threads names: a row id the request did not name, and a key that
		// is no row id at all, which names none of them either.
		"an entry for a row threads does not name": {map[string]any{"threads": []string{replyRow},
			"seen_replies": map[string]any{inboxDismissReplyPrefix + other: replyDrawn}}, "INVALID_QUERY"},
		"an entry keyed by no row id": {map[string]any{"threads": []string{replyRow},
			"seen_replies": map[string]any{"comment:" + thread: replyDrawn}}, "INVALID_QUERY"},
		// A reply: entry that is not that row's one version: the version sent bare rather than in
		// the object its kind takes, a value of the wrong type, no version at all, a version no
		// row ever carried, and the other kind's field, which is an unknown one here.
		"a reply entry that is not an object": {named(replyRow, 3), "INVALID_QUERY"},
		"a reply version of the wrong type":   {named(replyRow, map[string]any{"version": "3"}), "INVALID_QUERY"},
		"a reply entry with no version":       {named(replyRow, map[string]any{}), "INVALID_QUERY"},
		"a reply version below zero":          {named(replyRow, map[string]any{"version": -1}), "INVALID_QUERY"},
		"a reply entry carrying an agent field": {named(replyRow,
			map[string]any{"version": 3, "newest_reply": drawnReply}), "INVALID_QUERY"},
		// An agent: entry that is not the two things the page drew of its thread, refused in the
		// same shapes: not an object, either value of the wrong type, either field left out, and
		// version, which an Agent row has none of.
		"an agent entry that is not an object": {named(agentRow, []any{drawnReply}), "INVALID_QUERY"},
		"an agent newest_reply of the wrong type": {named(agentRow,
			map[string]any{"newest_reply": 3, "unread_replies": []string{}}), "INVALID_QUERY"},
		"an agent unread_replies of the wrong type": {named(agentRow,
			map[string]any{"newest_reply": drawnReply, "unread_replies": drawnReply}), "INVALID_QUERY"},
		"an agent entry with no newest_reply": {named(agentRow,
			map[string]any{"unread_replies": []string{drawnReply}}), "INVALID_QUERY"},
		"an agent entry with no unread_replies": {named(agentRow,
			map[string]any{"newest_reply": drawnReply}), "INVALID_QUERY"},
		"an agent entry carrying a version": {named(agentRow,
			map[string]any{"newest_reply": drawnReply, "unread_replies": []string{}, "version": 3}), "INVALID_QUERY"},
		// An id that is not a uuid, in either field a reply the page drew is named in.
		"a newest_reply that is not a reply id": {named(agentRow,
			map[string]any{"newest_reply": "not-a-uuid", "unread_replies": []string{}}), "INVALID_QUERY"},
		"an unread_replies holding what is not a reply id": {named(agentRow,
			map[string]any{"newest_reply": drawnReply, "unread_replies": []string{"not-a-uuid"}}), "INVALID_QUERY"},
		// The two the handler answers, each with an entry a dismissal of that row would be taken
		// by, so what is refused is the restore and the filters rather than the entry's shape.
		"seen_replies on a restore": {map[string]any{"threads": []string{replyRow}, "restore": true,
			"seen_replies": map[string]any{replyRow: replyDrawn}}, "INVALID_QUERY"},
		"seen_replies under the filters": {map[string]any{"dismiss_through": now, "project": []string{"TEST"},
			"seen_replies": map[string]any{replyRow: replyDrawn}}, "INVALID_QUERY"},
	} {
		t.Run(name, func(t *testing.T) {
			response := postInboxDismiss(t, handler, "alice", refusal.body)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+refusal.code+`"`) {
				t.Fatalf("status=%d body=%s, want 400 %s", response.Code, response.Body.String(), refusal.code)
			}
		})
	}
	bearer := sessionRequest(t, handler, http.MethodPost, "/api/v1/me/inbox/dismiss",
		map[string]any{"threads": []string{replyRow}})
	if bearer.Code != http.StatusForbidden {
		t.Fatalf("a session's dismiss: status=%d body=%s, want 403", bearer.Code, bearer.Body.String())
	}
}

// The Dismissed view reads newest dismissal first, a page at a time from the cursor the page before
// it answered, and its own paging alone: limit and offset are refused there, as a cursor is refused
// under every other state.
func TestDismissedPagesFromItsCursor(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Dismissed order", "A spec")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	threads := []string{}
	for index := range 3 {
		thread := seedUnreadThread(t, handler, comments, fmt.Sprintf("Dismissed %d?", index))
		// One dismissal each, in turn, so the marks are in a known order.
		if dismissed := dismissRows(t, handler, database, "alice", inboxDismissReplyPrefix+thread); dismissed != 1 {
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
		// One body for both presses: they name the same rows, so they carry the same versions,
		// which the round's own state answers (dismissBody). Nobody writes in these threads
		// while the round runs, so every version still stands when each press reaches its rows.
		dismissing := dismissBody(t, database, "alice", dismissRowIDs(marks))
		first, second := race(t, round,
			func() *httptest.ResponseRecorder {
				return postInboxDismiss(t, byThreadID, "alice", dismissing)
			},
			func() *httptest.ResponseRecorder {
				return postInboxDismiss(t, byHeap, "alice", dismissing)
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
		everything := dismissBody(t, database, "alice", dismissRowIDs(threads))
		dismiss, all := race(t, round,
			func() *httptest.ResponseRecorder {
				return postInboxDismiss(t, byThreadID, "alice", everything)
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

	if dismissed := dismissRows(t, handler, database, "alice", inboxDismissReplyPrefix+seed.second); dismissed != 1 {
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
	handler, database := seed.handler, seed.database
	// Bob signs in, so he is a person with an Inbox of his own.
	getMyReplies(t, handler, "bob", "")
	if dismissed := dismissRows(t, handler, database, "bob", inboxDismissReplyPrefix+seed.first); dismissed != 0 {
		t.Fatalf("bob dismissed %d of alice's threads, want 0", dismissed)
	}
	if at := threadDismissal(t, database, "alice", seed.first); at != nil {
		t.Fatalf("alice's thread was dismissed at %s by bob's request", at)
	}
	if dismissed := dismissRows(t, handler, database, "alice",
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

// INFRA-450's a13: a reply the person sends from the Dismissed view leaves the thread there, on
// both halves, which is what the two clears being unconditional must not have cost.
// user_threads_record clears dismissed_at for every login but the writer's, so their own row
// keeps its mark (migration 20261105093501), and the Agent trigger fires on a session's reply
// and on nothing else (20261105093507), so their own reply in an Agent thread clears nothing
// either. Both replies here are newer than the mark they are written against, which is the case
// a time comparison would have cleared.
func TestTheCallersOwnReplyLeavesBothHalvesDismissed(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	if dismissed := dismissRows(t, handler, database, "alice",
		inboxDismissReplyPrefix+seed.first, inboxDismissAgentPrefix+seed.agent); dismissed != 2 {
		t.Fatalf("dismissed = %d, want both threads", dismissed)
	}
	createThreadComment(t, handler, seed.firstIssue,
		map[string]any{"body": "Still mine.", "reply_to": seed.first}, "alice")
	writeAgentReply(t, database.Pool, seed.agentReply, "user", "alice", databaseNow(t, database.Pool))

	if at := threadDismissal(t, database, "alice", seed.first); at == nil {
		t.Fatalf("thread %s came back on the caller's own comment", seed.first)
	}
	if at := agentThreadDismissal(t, database, "alice", seed.agent); at == nil {
		t.Fatal("the agent thread came back on the caller's own reply")
	}
	away := threadsInState(t, handler, "alice", myRepliesStateDismissed)
	want := []string{seed.first, seed.agent}
	slices.Sort(want)
	if !slices.Equal(away, want) {
		t.Fatalf("state=dismissed = %#v, want both threads the caller replied in still there", away)
	}
	inbox := threadsInState(t, handler, "alice", myRepliesStateInbox)
	if slices.Contains(inbox, seed.first) || slices.Contains(inbox, seed.agent) {
		t.Fatalf("state=inbox = %#v, want neither thread the caller replied in", inbox)
	}
}
