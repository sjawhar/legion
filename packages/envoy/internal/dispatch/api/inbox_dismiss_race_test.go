package api

// The windows POST /api/v1/me/inbox/dismiss can be caught in, on both halves, and the rows that
// make them. Each window is made rather than waited for: a transaction of the test's own holds a
// row the request's lock order takes (gateHold, api/inbox_read_test.go), or holds a write of its
// own written and uncommitted, and the test lets the two cross where it wants them to. What the
// rows hold is one thing - a write by somebody else that the page never drew is never buried,
// whatever order that write and the press commit in - and each of them says which clause of the
// route turns the press away.
//
// The writers are the test's own inserts rather than the routes', because no route lets a caller
// say when its write is stamped or leaves its transaction open, and a window is made of both.
// The lock order every gate here is a step of is markInboxReadThreads' (api/inbox_read.go), and
// the route's ordinary rows - what one press changes, what it refuses, what it announces - are
// api/inbox_dismiss_test.go's.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

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

// writeAgentReplyInsert writes one reply into an Agent thread at a time of the test's choosing,
// which no route lets a caller name: when a write is stamped, and what follows from it, is
// migration 20261105093501's header's own statement, and a test of what a dismiss does to a
// reply stamped before it began has to write both sides of that window itself. $1 is the message
// it replies to - the root, or a reply under it - $2 the author's kind, as the Inbox reads it,
// $3 the author and $4 the stamp.
const writeAgentReplyInsert = `
		insert into messages (issue_key, author, body, in_reply_to, target, created_at)
		select null, jsonb_build_object('kind', $2::text, 'id', $3::text), 'Written at ' || $4::text,
		       parent.id, parent.target, $4::text::timestamptz
		from messages parent where parent.id = $1
		returning id::text`

// writeAgentReply makes that write and answers the reply's id. into is the pool, or a
// transaction the test holds open so the reply is written and not committed (holdSessionReply).
func writeAgentReply(t *testing.T, into queryer, parent, kind, id string, at time.Time) string {
	t.Helper()
	var written string
	if err := into.QueryRow(context.Background(), writeAgentReplyInsert,
		parent, kind, id, at.UTC().Format(time.RFC3339Nano)).Scan(&written); err != nil {
		t.Fatalf("write a %s reply under %s: %v", kind, parent, err)
	}
	return written
}

// writeAgentReplyBehind makes it off the test's own goroutine, for the one write that waits: a
// reply whose clearing trigger takes a root a dismiss is holding (migration 20261105093507)
// blocks there until that dismiss commits, and the test has to go on meanwhile. It answers the
// channel the insert's own error lands on - nil once the row is written - rather than failing
// from a goroutine the test does not own.
func writeAgentReplyBehind(into queryer, parent, kind, id string, at time.Time) <-chan error {
	written := make(chan error, 1)
	go func() {
		var reply string
		written <- into.QueryRow(context.Background(), writeAgentReplyInsert,
			parent, kind, id, at.UTC().Format(time.RFC3339Nano)).Scan(&reply)
	}()
	return written
}

// writeThreadReply is the Reply half's own: one comment in a thread at a time of the test's
// choosing, which user_threads_record stamps the other participants' rows with (p_at, migration
// 20261105093501). into is the pool, or a transaction the test holds open, where the row and the
// trigger that ran over it are written and not committed.
func writeThreadReply(t *testing.T, into queryer, issue, thread, kind, id string, at time.Time) string {
	t.Helper()
	var written string
	if err := into.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, reply_to, created_at)
		values ($1, jsonb_build_object('kind', $3::text, 'id', $4::text), 'Written at ' || $5::text,
		        $2, $5::text::timestamptz)
		returning id::text
	`, issue, thread, kind, id, at.UTC().Format(time.RFC3339Nano)).Scan(&written); err != nil {
		t.Fatalf("write a %s comment in %s: %v", kind, thread, err)
	}
	return written
}

// databaseNow is one transaction's own now(), which is the stamp every write in it carries; when
// a write takes that stamp, and what the Inbox's dismissal builds on that, is migration
// 20261105093501's header's. A test that needs one write stamped before another transaction
// began reads the two off this clock, the one that stamps them, rather than off the test
// process's.
func databaseNow(t *testing.T, into queryer) time.Time {
	t.Helper()
	var at time.Time
	if err := into.QueryRow(context.Background(), `select now()`).Scan(&at); err != nil {
		t.Fatalf("read the transaction's own now(): %v", err)
	}
	return at
}

// The Agent half's dismissal is cleared by the trigger of migration 20261105093507, in the
// transaction the reply is written in, which is what makes "dismissed" on this half a mark
// existing and nothing else: no read compares the mark against the thread's newest reply, and
// none has to read a dismissed thread at all.
//
// What clears it is a session's reply, at any depth and whatever its time. The trigger compares
// the mark with nothing, for the reason migration 20261105093501's header gives for the clear
// beside it, and a dismiss that had drawn the reply is turned away before it writes a mark at
// all, by what the page drew (inboxDismissCheckAgentRoots).
//
// What does not clear it is a reply of the person's own, however new, since a reply they send
// from the Dismissed view leaves the thread there, exactly as their own reply leaves a Reply
// thread's mark where it stands.
func TestADismissedAgentThreadComesBackOnASessionReplyWhateverItsTime(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	if dismissed := dismissRows(t, handler, database, "alice", inboxDismissAgentPrefix+seed.agent); dismissed != 1 {
		t.Fatalf("dismissed = %d, want the agent thread", dismissed)
	}
	mark := agentThreadDismissal(t, database, "alice", seed.agent)
	if mark == nil {
		t.Fatal("the dismissal wrote no mark on the agent thread")
	}

	// A reply of the caller's own, newer than the mark: the mark stays and the thread stays out
	// of the Inbox.
	writeAgentReply(t, database.Pool, seed.agent, "user", "alice", mark.Add(time.Minute))
	if at := agentThreadDismissal(t, database, "alice", seed.agent); at == nil {
		t.Fatal("the agent thread's mark is gone after a reply of the caller's own")
	}
	if away := threadsInState(t, handler, "alice", myRepliesStateDismissed); !slices.Contains(away, seed.agent) {
		t.Fatalf("state=dismissed = %#v, want the agent thread still there", away)
	}
	if inbox := threadsInState(t, handler, "alice", myRepliesStateInbox); slices.Contains(inbox, seed.agent) {
		t.Fatalf("state=inbox = %#v, want the agent thread still out of it", inbox)
	}

	// A session reply stamped a minute before the mark, written two deep - under the reply
	// already in the thread, which is where a follow-up lands - so the clear is the walk up
	// in_reply_to and not a match on the root alone: the mark is gone and the thread is back,
	// unread. A stamp older than the mark is exactly what a reply that commits after a dismiss
	// began carries, and it comes back all the same.
	writeAgentReply(t, database.Pool, seed.agentReply, "session", "s1", mark.Add(-time.Minute))
	if at := agentThreadDismissal(t, database, "alice", seed.agent); at != nil {
		t.Fatalf("the agent thread still carries the mark %s after a session replied in it", at)
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

// holdGateDocument holds one document's row the way a write to it takes it, so a dismiss that
// locks that document queues behind this transaction: step 1 of the lock order, where
// holdGateThread's row is step 3 (holdGateRow, api/inbox_read_test.go).
func holdGateDocument(t *testing.T, database *store.Store, document string) gateHold {
	t.Helper()
	return holdGateRow(t, database, "the gate document's row", `
		select 1 from artifacts where id = $1 for no key update
	`, document)
}

// threadLeftUnreadInTheInbox is the four checks every window below closes with: the thread
// carries no dismissal mark, the Inbox lists it, the Dismissed view does not, and the unread
// count holds it. Together they are where a reply nobody has read leaves a thread, which is
// what each of those rows is about.
func threadLeftUnreadInTheInbox(t *testing.T, handler http.Handler, thread string, at *time.Time) {
	t.Helper()
	if at != nil {
		t.Fatalf("thread %s carries the dismissal %s, past a reply nobody has read", thread, at)
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
}

// dismissRanAndLeftTheThread is the ending each window below closes with, which is four checks:
// the dismiss answered 200, it changed the threads this window leaves it free to change - the
// gate thread, which nobody wrote in, among them, so the request ran whole and what the thread
// under test shows is this one window rather than a request lost - the gate thread carries its
// mark, and the thread under test came out of it where a write nobody drew leaves one
// (threadLeftUnreadInTheInbox). mark is that thread's own stored dismissal, read by the caller
// since the two halves keep theirs in different tables.
func dismissRanAndLeftTheThread(
	t *testing.T, seed inboxReadSeed, response *httptest.ResponseRecorder,
	dismissed int, thread string, mark *time.Time,
) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("the dismiss answered %d %s", response.Code, response.Body.String())
	}
	if changed := dismissedBy(t, response); changed != dismissed {
		t.Fatalf("dismissed = %d, want %d: the threads this window leaves the dismiss free to write",
			changed, dismissed)
	}
	if at := threadDismissal(t, seed.database, "alice", seed.document); at == nil {
		t.Fatal("the document thread carries no mark, so the dismiss wrote nothing at all")
	}
	threadLeftUnreadInTheInbox(t, seed.handler, thread, mark)
}

// dismissChangedNothing is the two checks every refused press closes with, here and in
// api/inbox_dismiss_test.go: the press answered 200, since what the page drew is stale rather
// than wrong, and it changed no thread at all. why is what that row holds the press away by,
// which is what a red one has to say, so each row keeps its own words.
func dismissChangedNothing(t *testing.T, response *httptest.ResponseRecorder, why string) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("the dismiss answered %d %s", response.Code, response.Body.String())
	}
	if dismissed := dismissedBy(t, response); dismissed != 0 {
		t.Fatalf("dismissed = %d, want none: %s", dismissed, why)
	}
}

// dismissChangedNothingAndLeftTheThread is the whole of that ending, which four of the windows
// below close with: the press changed nothing and the thread it named came out of it where a
// write nobody has read leaves one (threadLeftUnreadInTheInbox). mark is that thread's own
// stored dismissal, read by the caller since the two halves keep theirs in different tables.
//
// The fifth refused press, TestDismissRefusesAnAgentRootWhoseNewestReplyThePageNeverDrew in
// api/inbox_dismiss_test.go, takes dismissChangedNothing alone: the person there read the
// conversation in another page before pressing, so its thread is left in the Inbox read rather
// than unread, which is the whole of what that row is about.
func dismissChangedNothingAndLeftTheThread(
	t *testing.T, handler http.Handler, response *httptest.ResponseRecorder,
	thread string, mark *time.Time, why string,
) {
	t.Helper()
	dismissChangedNothing(t, response, why)
	threadLeftUnreadInTheInbox(t, handler, thread, mark)
}

// A dismiss reads the rows it changes before it writes their marks, and between the two it waits
// for every lock the order takes. A reply by somebody else that commits inside that window leaves
// its thread in the Inbox, unread: the alternative is a thread dismissed over a write nobody has
// drawn, which nothing undoes - user_threads_record and the trigger of 20261105093507 each clear
// a mark that is already there, and a mark written after they have run is not.
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
// What no row here reaches is the reply whose own transaction began before the dismiss did.
// Every writer here opens its transaction once the dismiss is already queued, so its reply is
// stamped after the dismiss began; the rows whose writer begins first, and whose write therefore
// carries the older stamp of the two, are the tests below this one. What orders those: on the
// Reply half the caller's own rows, which step 3 of that order locks and the trigger's update
// waits on; on the Agent half the thread's root, which the dismiss takes at step 2 and the
// clearing trigger takes `for key share` before its delete.
func TestDismissLeavesAThreadRepliedToWhileItRan(t *testing.T) {
	// The two Reply rows differ in the form the request takes and in nothing else: the same
	// session writes in the same thread, and the same row must come out of it unread. So body is
	// all either of them spells.
	type window struct {
		// body is the dismiss under test, and write the reply by somebody else that commits
		// while it waits. thread is the row that must come out of it still in the Inbox, and
		// mark its stored dismissal, which must be none. dismissed is how many threads the
		// request still changes, which is every thread it names or matches but that one.
		body      func(t *testing.T, seed inboxReadSeed) map[string]any
		write     func(t *testing.T, seed inboxReadSeed)
		thread    func(seed inboxReadSeed) string
		mark      func(t *testing.T, seed inboxReadSeed) *time.Time
		dismissed int
	}
	replyThread := window{
		write: func(t *testing.T, seed inboxReadSeed) {
			sessionComment(t, seed.handler, "/api/v1/issues/"+seed.firstIssue+"/comments",
				map[string]any{"body": "One more thing.", "reply_to": seed.first})
		},
		thread: func(seed inboxReadSeed) string { return seed.first },
		mark: func(t *testing.T, seed inboxReadSeed) *time.Time {
			return threadDismissal(t, seed.database, "alice", seed.first)
		},
	}
	named, filtered := replyThread, replyThread
	named.body = func(t *testing.T, seed inboxReadSeed) map[string]any {
		return dismissBody(t, seed.database, "alice", []string{
			inboxDismissReplyPrefix + seed.document, inboxDismissReplyPrefix + seed.first,
		})
	}
	named.dismissed = 1
	filtered.body = func(t *testing.T, seed inboxReadSeed) map[string]any {
		return map[string]any{
			"dismiss_through": time.Now().UTC().Format(time.RFC3339Nano),
			"kind":            []string{myReplyKindReply},
		}
	}
	// The filtered form matches every Reply thread of the seed - the two on TEST, the one on
	// OTHER and the document's - and writes all of them but the one written in since.
	filtered.dismissed = 3
	for name, row := range map[string]window{
		"a named reply dismiss":    named,
		"a filtered reply dismiss": filtered,
		"an agent dismiss": {
			body: func(t *testing.T, seed inboxReadSeed) map[string]any {
				return dismissBody(t, seed.database, "alice", []string{
					inboxDismissReplyPrefix + seed.document, inboxDismissAgentPrefix + seed.agent,
				})
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
			dismissed: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			seed := seedInboxRead(t)
			database := seed.database
			// The body is built here rather than in the goroutine below: it reads what the page
			// drew out of the database, and a helper that fails does it on the test's own
			// goroutine.
			body := row.body(t, seed)
			held := holdGateDocument(t, database, threadArtifact(t, database, "alice", seed.document))
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() { answered <- postInboxDismiss(t, seed.handler, "alice", body) }()
			if storetest.WaitForStatementBehind(t, database, held.tx, inboxReadLockDocuments, 10*time.Second) == 0 {
				t.Fatal("the dismiss never queued on the document row it locks before it writes")
			}
			row.write(t, seed)
			held.release()
			dismissRanAndLeftTheThread(t, seed, <-answered, row.dismissed, row.thread(seed), row.mark(t, seed))
		})
	}
}

// holdSessionReply writes one session reply into an Agent thread on a transaction of the test's
// own and leaves that transaction open: the row is written and the trigger of 20261105093507 has
// run over it, and neither is committed. That is a reply in flight, which no route can be asked
// for - every reply route commits before it answers - and it is what the windows below are made
// of. hold.commit lands it; the roll back beginGateHold registered is the safety net for a round
// that fails before that, and it never runs after a commit (gateHold, api/inbox_read_test.go).
//
// The reply carries the holding transaction's own now(), which is the stamp a write carries in
// production, so a hold opened before a dismiss is sent writes a reply stamped before that
// dismiss's mark and one opened while the dismiss waits writes a reply stamped after it - by the
// clock that stamps them both, not by the test process's.
func holdSessionReply(t *testing.T, database *store.Store, parent string) gateHold {
	t.Helper()
	hold := beginGateHold(t, database, "the reply this test holds")
	writeAgentReply(t, hold.tx, parent, "session", "s1", databaseNow(t, hold.tx))
	return hold
}

// The reply first, which is the half of the window the test above cannot reach: a session reply
// written and not committed when the dismiss reaches the Agent thread's root. The reply holds that
// root - its clearing trigger takes it before the delete (migration 20261105093507) - and the
// dismiss waits there, at the level that conflicts with the trigger's (inboxDismissLockAgentRoots,
// step 2 of the lock order). The check that follows, a statement of its own and so a snapshot of
// its own, holds the committed reply: it is unread and it is none of the ids the page drew, so
// the root is dropped and nothing writes a mark for it. The thread ends where a reply nobody has
// read leaves it: in the Inbox, unread.
//
// Without that lock the dismiss waits nowhere, its check reads a snapshot the reply is not in and
// the mark is written, and the reply's trigger has already run and found nothing to delete - a
// thread dismissed under a reply nobody has read, which is what this row is red for.
//
// The interleave is made rather than waited for, as the test above makes its own. The gate is the
// document of the Reply thread the request names, step 1, taken after the read and before every
// lock and every write: a dismiss queued there has already read every row it will write, so the
// reply this test then writes is one no read of that request saw, and what leaves the thread
// alone is the check at the root rather than anything the request read before it. The reply goes
// two deep, under the reply already in the thread, where a follow-up lands: its own foreign key
// then holds its parent and not the root, so the only lock on the root is the trigger's.
func TestDismissLeavesAnAgentThreadWhoseReplyCommitsAsItWrites(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	body := dismissBody(t, database, "alice", []string{
		inboxDismissReplyPrefix + seed.document, inboxDismissAgentPrefix + seed.agent,
	})
	held := holdGateDocument(t, database, threadArtifact(t, database, "alice", seed.document))
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- postInboxDismiss(t, handler, "alice", body) }()
	dismissing := storetest.WaitForStatementBehind(t, database, held.tx, inboxReadLockDocuments, 10*time.Second)
	if dismissing == 0 {
		t.Fatal("the dismiss never queued on the document row it locks before it writes")
	}
	replying := holdSessionReply(t, database, seed.agentReply)
	held.release()
	if answer, early := storetest.WaitForQueuedBehind(t, replying.tx, dismissing, answered, 10*time.Second); early {
		t.Fatalf("the dismiss answered %d %s without ever waiting at the agent thread's root, so its check read a snapshot the reply is not in",
			answer.Code, answer.Body.String())
	}
	replying.commit(t)

	// The gate thread, which nobody wrote in, is dismissed and counted, and the agent thread is
	// neither: the request ran whole, so what the agent thread shows is this one window.
	dismissRanAndLeftTheThread(t, seed, <-answered, 1, seed.agent,
		agentThreadDismissal(t, database, "alice", seed.agent))
}

// The dismiss first, the same pair the other way round, which is what says the lock did not simply
// make the dismiss win. The gate here is the caller's own row for the Reply thread the request
// names, step 3 of the lock order, so the dismiss queued there is holding the Agent root it took
// at step 2; the reply then arrives, waits at that root, and the dismiss commits its mark first.
// The reply's trigger is given the root next, finds the mark that dismiss committed and deletes
// it in the reply's own transaction, the two times deciding nothing (migration 20261105093507
// compares them with nothing): the thread comes back, unread, and the count the dismiss answered
// says the mark it deleted was really written.
func TestADismissHoldingTheAgentRootGivesTheThreadBackOnTheReply(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	body := dismissBody(t, database, "alice", []string{
		inboxDismissReplyPrefix + seed.document, inboxDismissAgentPrefix + seed.agent,
	})
	held := holdGateThread(t, database, seed.document)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- postInboxDismiss(t, handler, "alice", body) }()
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
	// Both threads: the dismiss had the root first and wrote the agent thread's mark, which the
	// reply's own trigger then deleted.
	if reply := <-replied; reply.Code != http.StatusCreated {
		t.Fatalf("the reply answered %d %s", reply.Code, reply.Body.String())
	}
	dismissRanAndLeftTheThread(t, seed, response, 2, seed.agent,
		agentThreadDismissal(t, database, "alice", seed.agent))
}

// The writer first, on the Agent half, with its transaction begun before the dismiss was sent:
// the window no row above reaches. The reply is stamped before the dismiss's mark, by migration
// 20261105093501's header's rule, and commits after the dismiss read its rows - a reply the
// person cannot have seen, carrying the older of the two stamps. What turns the dismiss away is
// what the page drew: the reply is unread and it is none of the ids the press carried
// (inboxDismissCheckAgentRoots, read under the root lock in a statement of its own). No mark is
// written, the thread is not counted and its replies are not marked read.
//
// The lock the dismiss waits at when the reply commits is the thread's root, taken `for update`
// at step 2 of the lock order: the reply's own clearing trigger holds it `for key share` until
// that transaction ends (migration 20261105093507). The reply goes two deep, under the reply
// already in the thread, where a follow-up lands, so its foreign key holds its parent and the
// only lock it has on the root is the trigger's.
//
// Which clause refuses it is the drawn-unread one: the reply's stamp leaves the thread's newest
// session reply where the page drew it, so the newest-reply equality passes and this row stays
// green with the equality alone removed. With the drawn-unread clause gone the mark is written,
// and the trigger that ran inside the reply's transaction found no mark to delete: a thread
// dismissed with a reply nobody has read.
func TestDismissLeavesAnAgentThreadWhoseOlderReplyCommitsAtItsRoot(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	body := dismissBody(t, database, "alice", []string{inboxDismissAgentPrefix + seed.agent})
	// The writer begins and writes before the dismiss is sent, which is what stamps its reply
	// before the mark that dismiss will store.
	replying := holdSessionReply(t, database, seed.agentReply)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- postInboxDismiss(t, handler, "alice", body) }()
	if storetest.WaitForStatementBehind(t, database, replying.tx, inboxDismissLockAgentRoots, 10*time.Second) == 0 {
		t.Fatal("the dismiss never queued at the agent thread's root, which the reply in flight holds")
	}
	replying.commit(t)

	dismissChangedNothingAndLeftTheThread(t, handler, <-answered, seed.agent,
		agentThreadDismissal(t, database, "alice", seed.agent),
		"the thread holds a reply the page never drew")
}

// The same pair the other way round, with the writer's transaction begun first all the same: the
// dismiss holds the Agent thread's root and commits its mark, and the reply - stamped before
// that mark, since its transaction began before the dismiss was sent - waits at the root and
// commits after. The trigger of 20261105093507 deletes the root's mark whatever its time, so the
// thread is back in the Inbox, unread, and the count the dismiss answered says the mark the
// trigger deleted was really written.
//
// The lock the dismiss waits at while the reply queues is the caller's own row for the Reply
// thread it names beside the root, step 3 of the lock order: a dismiss queued there is holding
// the root it took at step 2, which is what makes the reply wait.
//
// With the trigger's old comparison back - delete only a mark older than the reply - the mark
// outlives a reply nobody has read, and nothing afterwards would remove it.
func TestADismissHoldingTheAgentRootGivesTheThreadBackOnAnOlderReply(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	body := dismissBody(t, database, "alice", []string{
		inboxDismissReplyPrefix + seed.document, inboxDismissAgentPrefix + seed.agent,
	})
	// The writer's transaction begins before the dismiss is sent and takes its stamp there; it
	// writes once the dismiss holds the root, so its insert is what waits.
	replying := beginGateHold(t, database, "the reply this test holds")
	stamp := databaseNow(t, replying.tx)
	held := holdGateThread(t, database, seed.document)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- postInboxDismiss(t, handler, "alice", body) }()
	if storetest.WaitForStatementBehind(t, database, held.tx, inboxReadLockThreads, 10*time.Second) == 0 {
		t.Fatal("the dismiss never queued on the caller's rows, which it locks after the agent thread's root")
	}
	written := writeAgentReplyBehind(replying.tx, seed.agentReply, "session", "s1", stamp)
	// Five seconds, where the dismiss's statement_timeout allows it ten at the gate: a reply that
	// has not queued by then is this row's red, and waiting the full ten would spend the dismiss's
	// own allowance on it and answer 500 instead.
	if waiting := storetest.WaitForLockWaiters(t, held.tx, 2, 5*time.Second); waiting < 2 {
		t.Fatalf("%d transactions queued behind the gate row, want the dismiss and the reply waiting at the root it holds", waiting)
	}
	held.release()

	response := <-answered
	if err := <-written; err != nil {
		t.Fatalf("write the reply this test held behind the root: %v", err)
	}
	replying.commit(t)
	// Both threads: the dismiss had the root first and wrote the agent thread's mark, which the
	// reply's own trigger then deleted.
	dismissRanAndLeftTheThread(t, seed, response, 2, seed.agent,
		agentThreadDismissal(t, database, "alice", seed.agent))
}

// The Reply half's own window with the writer first: a comment whose transaction began before
// the dismiss was sent, so user_threads_record stamps replied_at before that dismiss's mark, and
// which commits while the dismiss waits - before its update statement runs, so that statement's
// own snapshot holds the comment. The update must leave the row alone: the row's version is the
// one that trigger's increment just moved and no longer the one the page drew.
//
// The lock the dismiss waits at when the comment commits is the document of the other Reply
// thread it names, step 1 of the lock order, taken after the read and before the caller's rows
// and every write. The comment lands in an issue's thread, which that gate does not reach, so
// nothing of it waits.
//
// With the version check gone nothing is left between the update and the row: it writes the mark
// over a comment nobody has read.
func TestDismissLeavesAReplyThreadWhoseOlderCommentCommitsBeforeItWrites(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	body := dismissBody(t, database, "alice", []string{
		inboxDismissReplyPrefix + seed.document, inboxDismissReplyPrefix + seed.first,
	})
	writing := beginGateHold(t, database, "the comment this test holds")
	stamp := databaseNow(t, writing.tx)
	held := holdGateDocument(t, database, threadArtifact(t, database, "alice", seed.document))
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- postInboxDismiss(t, handler, "alice", body) }()
	if storetest.WaitForStatementBehind(t, database, held.tx, inboxReadLockDocuments, 10*time.Second) == 0 {
		t.Fatal("the dismiss never queued on the document row it locks before it writes")
	}
	writeThreadReply(t, writing.tx, seed.firstIssue, seed.first, "session", "s1", stamp)
	writing.commit(t)
	held.release()

	dismissRanAndLeftTheThread(t, seed, <-answered, 1, seed.first,
		threadDismissal(t, database, "alice", seed.first))
}

// The same half with the trigger first, which is the one thing the row above does not reach: the
// comment's own transaction runs user_threads_record over the caller's row (migration
// 20261105093501) and holds that row, and the dismiss queues on it. That is the re-check. Read
// committed gives the dismiss the row version the trigger wrote once the comment commits, and
// the update's `where` is checked against that version rather than against the one this request
// read: the version differs, so the row is skipped, where the row above skipped it off a
// snapshot taken after the comment had already committed.
//
// The lock the dismiss waits at is that same user_threads row, step 3 of the lock order
// (inboxReadLockThreads, which the update behind it re-locks). The comment's transaction began
// before the dismiss was sent, so its write is one the page cannot have drawn however the two
// times fall, and the version the trigger moved under this row lock is what refuses the press.
func TestDismissSkipsAReplyThreadWhoseTriggerHoldsTheRowItWrites(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	body := dismissBody(t, database, "alice", []string{
		inboxDismissReplyPrefix + seed.document, inboxDismissReplyPrefix + seed.first,
	})
	writing := beginGateHold(t, database, "the comment this test holds")
	writeThreadReply(t, writing.tx, seed.firstIssue, seed.first, "session", "s1", databaseNow(t, writing.tx))
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- postInboxDismiss(t, handler, "alice", body) }()
	if storetest.WaitForStatementBehind(t, database, writing.tx, inboxReadLockThreads, 10*time.Second) == 0 {
		t.Fatal("the dismiss never queued on the caller's row, which the comment's own trigger holds")
	}
	writing.commit(t)

	dismissRanAndLeftTheThread(t, seed, <-answered, 1, seed.first,
		threadDismissal(t, database, "alice", seed.first))
}

// Two replies whose transactions interleave with inverted starts, which is what the newest reply
// alone cannot see. A begins first and commits last; B begins after A and commits before it, so
// A lands under the older stamp and the thread's newest session reply by stamp is B both before
// and after A commits. The dismissal names B, which is what the page drew, so the first test the
// check makes passes; A is unread and is none of the ids the page drew as unread, so the second
// fails and no mark is written. The thread stays in the Inbox with A unread.
//
// The lock the dismiss waits at when A commits is the thread's root, which A's clearing trigger
// holds `for key share`. B holds nothing by then - it committed before the dismiss was sent -
// and `for key share` is the level two replies in one thread share without queueing behind each
// other. A committing inside that window is what separates this row from the one below it,
// where the same pair of replies is settled before the press is sent at all.
//
// With the drawn-unread test gone the equality passes alone and the mark is written over A; with
// the check gone altogether nothing refuses the press at all.
func TestDismissLeavesAnAgentThreadWhoseInterleavedRepliesInvertTheirStamps(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	// A: begun first, written at its own transaction's stamp, left open.
	replying := beginGateHold(t, database, "the reply this test holds")
	writeAgentReply(t, replying.tx, seed.agentReply, "session", "s1", databaseNow(t, replying.tx))
	// B: begun after A and committed at once, under the newer stamp, which makes it the thread's
	// newest reply and so the reply the page draws.
	newest := writeAgentReply(t, database.Pool, seed.agentReply, "session", "s1", databaseNow(t, database.Pool))
	body := dismissBody(t, database, "alice", []string{inboxDismissAgentPrefix + seed.agent})
	drawn, _ := body["seen_replies"].(map[string]any)[inboxDismissAgentPrefix+seed.agent].(map[string]any)
	seen, _ := drawn["newest_reply"].(string)
	if seen != newest {
		t.Fatalf("the dismissal names %s as the row's newest reply, want %s, the one committed second", seen, newest)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- postInboxDismiss(t, handler, "alice", body) }()
	if storetest.WaitForStatementBehind(t, database, replying.tx, inboxDismissLockAgentRoots, 10*time.Second) == 0 {
		t.Fatal("the dismiss never queued at the agent thread's root, which the reply in flight holds")
	}
	replying.commit(t)

	dismissChangedNothingAndLeftTheThread(t, handler, <-answered, seed.agent,
		agentThreadDismissal(t, database, "alice", seed.agent),
		"the thread holds a reply nobody has read")
}

// The same inverted pair with both replies settled before the press is sent, which is the row
// the version is for: A begins first and commits last, B begins after A and commits before it,
// and A lands between the page's draw and the press. Nothing about the thread's newest reply
// moves - A is the older of the two stamps, so B stays the newest by `created_at desc, id desc`
// - and nothing about its times tells the press anything. What refuses it is the drawn-unread
// clause: A is unread for this caller and is none of the ids the page drew as unread.
//
// This is the row the Agent half's own check was built for. Held to the replies that were unread
// at its own read instead of to the ids the page drew, the check finds A among them and lets the
// press through, and the thread is dismissed with A unread: the window the test above cannot
// reach, since there the reply commits after that read has run.
func TestDismissLeavesAnAgentThreadWhoseOlderReplyLandsBeforeThePress(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	// A: begun first, written at its own transaction's stamp, left open.
	replying := beginGateHold(t, database, "the reply this test holds")
	older := writeAgentReply(t, replying.tx, seed.agentReply, "session", "s1", databaseNow(t, replying.tx))
	// B: begun after A and committed at once, under the newer stamp, which makes it the thread's
	// newest reply and so the reply the page draws.
	newest := writeAgentReply(t, database.Pool, seed.agentReply, "session", "s1", databaseNow(t, database.Pool))
	row := inboxDismissAgentPrefix + seed.agent
	body := dismissBody(t, database, "alice", []string{row})
	drawn, _ := body["seen_replies"].(map[string]any)[row].(map[string]any)
	if seen, _ := drawn["newest_reply"].(string); seen != newest {
		t.Fatalf("the page drew %s as the row's newest reply, want %s, the newer-stamped of the two", seen, newest)
	}
	if unread, _ := drawn["unread_replies"].([]string); slices.Contains(unread, older) {
		t.Fatalf("the page drew %v as unread, which holds the reply still in flight", unread)
	}

	// A lands, and the row the page drew is still the row the page drew in everything but what
	// is unread in it: that is what the press is held to and all that refuses it.
	replying.commit(t)
	landed, _ := seenRepliesOf(t, database, "alice", []string{row})[row].(map[string]any)
	if seen, _ := landed["newest_reply"].(string); seen != newest {
		t.Fatalf("the thread's newest reply is %s after the older one landed, want %s: this row is about a write that moves neither stamp", seen, newest)
	}
	if unread, _ := landed["unread_replies"].([]string); !slices.Contains(unread, older) {
		t.Fatalf("the reply that landed is not unread (%v), so the press would have nothing to be refused by", unread)
	}

	dismissChangedNothingAndLeftTheThread(t, handler, postInboxDismiss(t, handler, "alice", body), seed.agent,
		agentThreadDismissal(t, database, "alice", seed.agent),
		"the thread holds a reply the page never drew")
}

// The Reply half's own version of that row, and the one that says what the version counts. A
// comment whose transaction began before another comment's and committed after it moves neither
// of the two columns a write usually moves: replied_at takes the newer of the two stamps and
// last_reply_id moves only with it, so both stand exactly where the page drew them. The version
// moves all the same - user_threads_record increments it for every write by somebody else,
// whatever that write's time or id (migration 20261105093501) - and the press carrying the
// version the page drew writes nothing.
//
// Read off the row itself: the version moved, last_reply_id and replied_at did not. A dismissal
// held to either of those columns dismisses this thread over a comment nobody has read.
func TestDismissLeavesAReplyThreadWhoseOlderCommentLandsBeforeThePress(t *testing.T) {
	seed := seedInboxRead(t)
	handler, database := seed.handler, seed.database
	// A: begun first, written at its own transaction's stamp, left open.
	writing := beginGateHold(t, database, "the comment this test holds")
	stamp := databaseNow(t, writing.tx)
	// B: begun after A and committed at once, through the route a session writes by, under the
	// newer stamp - which is what the row's replied_at and last_reply_id come to hold.
	sessionComment(t, handler, "/api/v1/issues/"+seed.firstIssue+"/comments",
		map[string]any{"body": "One more thing.", "reply_to": seed.first})
	body := dismissBody(t, database, "alice", []string{inboxDismissReplyPrefix + seed.first})
	drawn := threadRowWrites(t, database, "alice", seed.first)

	writeThreadReply(t, writing.tx, seed.firstIssue, seed.first, "session", "s1", stamp)
	writing.commit(t)
	landed := threadRowWrites(t, database, "alice", seed.first)
	if landed.version == drawn.version {
		t.Fatalf("version %d after a comment by somebody else landed, want it moved off the %d the page drew",
			landed.version, drawn.version)
	}
	if landed.lastReply != drawn.lastReply || !landed.repliedAt.Equal(drawn.repliedAt) {
		t.Fatalf("last_reply_id %s and replied_at %s after the older comment landed, want the %s and %s the page drew: this row is about the write that moves neither",
			landed.lastReply, landed.repliedAt, drawn.lastReply, drawn.repliedAt)
	}

	dismissChangedNothingAndLeftTheThread(t, handler, postInboxDismiss(t, handler, "alice", body), seed.first,
		threadDismissal(t, database, "alice", seed.first),
		"the row holds a comment the page never drew")
}
