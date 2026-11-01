package store

import (
	"context"
	"testing"
	"time"
)

// beforeUserThreads is the last migration before user_threads: the five that make, fill and catch
// it up (0095_user_threads through 0099_user_threads_catch_up) apply after it.
const beforeUserThreads = 94

// userThread is one person's row for one thread, as the Replies page reads it.
type userThread struct {
	kind        string
	unread      bool
	lastReplyID *string
}

func readUserThread(t *testing.T, store *Store, login, threadID string) (userThread, bool) {
	t.Helper()
	var row userThread
	err := store.Pool.QueryRow(context.Background(), `
		select kind, unread, last_reply_id::text from user_threads where login = $1 and thread_id = $2
	`, login, threadID).Scan(&row.kind, &row.unread, &row.lastReplyID)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return userThread{}, false
		}
		t.Fatalf("read user_threads(%s, %s): %v", login, threadID, err)
	}
	return row, true
}

func insertReturningID(t *testing.T, store *Store, query string, args ...any) string {
	t.Helper()
	var id string
	if err := store.Pool.QueryRow(context.Background(), query, args...).Scan(&id); err != nil {
		t.Fatalf("insert: %v\n%s", err, query)
	}
	return id
}

// The seed starts every thread a person wrote in read at deploy, so history never floods the
// Replies page; from then on the triggers keep each row: a write by anyone else makes it unread,
// the person's own write reads it, an edit changes nothing, and a reply anywhere in a thread lands
// on its root. Issue-less direct messages are the Agents page's, and a comment whose owner has no
// project (an agent conversation's file) has no page to open, so neither makes a row.
func TestUserThreadsSeedStartsReadAndTriggersKeepEachRow(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeUserThreads)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	// A padded, mixed-case author id keys the same person as the read routes' canonicalLogin.
	comment := insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":" Alice "}', 'Ship it?')
		returning id::text`)
	insertReturningID(t, store, `
		insert into comments (issue_key, author, body, reply_to) values ('CORE-1', '{"kind":"session","id":"s1"}', 'On it.', $1)
		returning id::text`, comment)
	ask := insertReturningID(t, store, `
		insert into asks (issue_key, author, question, state, answer)
		values ('CORE-1', '{"kind":"session","id":"s1"}', 'Which?', 'answered',
			jsonb_build_object('user', 'alice', 'selected', '[]'::jsonb, 'at', now() - interval '1 hour'))
		returning id::text`)
	insertReturningID(t, store, `
		insert into comments (issue_key, author, body, ask_id) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Thanks.', $1)
		returning id::text`, ask)
	message := insertReturningID(t, store, `
		insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Status?')
		returning id::text`)
	insertReturningID(t, store, `
		insert into messages (issue_key, author, body, in_reply_to) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Green.', $1)
		returning id::text`, message)
	direct := insertReturningID(t, store, `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'Hi', 'session:s1')
		returning id::text`)
	insertReturningID(t, store, `
		insert into messages (author, body, in_reply_to, target) values ('{"kind":"session","id":"s1"}', 'Hello', $1, 'session:s1')
		returning id::text`, direct)
	file := insertReturningID(t, store, `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ('s1', 'photo-png', 'photo.png', 'image', '{"kind":"session","id":"s1"}')
		returning id::text`)
	fileComment := insertReturningID(t, store, `
		insert into comments (artifact_id, author, body) values ($1, '{"kind":"user","id":"alice"}', 'Nice.')
		returning id::text`, file)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, want := range []struct{ thread, kind string }{{comment, "comment"}, {ask, "ask"}, {message, "message"}} {
		row, ok := readUserThread(t, store, "alice", want.thread)
		if !ok || row.kind != want.kind || row.unread {
			t.Fatalf("seeded %s row = %+v (found %v), want a read %s row", want.kind, row, ok, want.kind)
		}
	}
	for _, thread := range []string{direct, fileComment} {
		if row, ok := readUserThread(t, store, "alice", thread); ok {
			t.Fatalf("thread %s has row %+v, want none", thread, row)
		}
	}

	// A session's reply after the migration makes the row unread and names itself.
	reply := insertReturningID(t, store, `
		insert into comments (issue_key, author, body, reply_to) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Done.', $1)
		returning id::text`, comment)
	if row, _ := readUserThread(t, store, "alice", comment); !row.unread || row.lastReplyID == nil || *row.lastReplyID != reply {
		t.Fatalf("after a session reply: %+v, want unread naming %s", row, reply)
	}
	// Editing it does not bring a row back once read; the person's own reply reads it.
	insertReturningID(t, store, `
		insert into comments (issue_key, author, body, reply_to) values ('CORE-1', '{"kind":"user","id":"ALICE\t"}', 'Thanks.', $1)
		returning id::text`, comment)
	if row, _ := readUserThread(t, store, "alice", comment); row.unread {
		t.Fatalf("after alice's own reply: %+v, want read", row)
	}
	if _, err := store.Pool.Exec(ctx, `update comments set body = 'Done, really.' where id = $1`, reply); err != nil {
		t.Fatalf("edit reply: %v", err)
	}
	if row, _ := readUserThread(t, store, "alice", comment); row.unread {
		t.Fatalf("after an edit: %+v, want still read", row)
	}
	// A second session reply brings it back.
	insertReturningID(t, store, `
		insert into comments (issue_key, author, body, reply_to) values ('CORE-1', '{"kind":"session","id":"s2"}', 'Also.', $1)
		returning id::text`, comment)
	if row, _ := readUserThread(t, store, "alice", comment); !row.unread {
		t.Fatalf("after a second session reply: %+v, want unread", row)
	}

	// An issue message reply under a reply lands on the thread's root.
	child := insertReturningID(t, store, `
		insert into messages (issue_key, author, body, in_reply_to) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Thanks', $1)
		returning id::text`, message)
	insertReturningID(t, store, `
		insert into messages (issue_key, author, body, in_reply_to) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Also', $1)
		returning id::text`, child)
	if row, ok := readUserThread(t, store, "alice", message); !ok || !row.unread {
		t.Fatalf("message thread after a nested session reply: %+v (found %v), want unread on the root", row, ok)
	}
	if _, ok := readUserThread(t, store, "alice", child); ok {
		t.Fatalf("a nested message has a row of its own, want only the root")
	}

	// A session's reply on the answered ask makes it unread; changing the answer reads it.
	insertReturningID(t, store, `
		insert into comments (issue_key, author, body, ask_id) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Follow-up.', $1)
		returning id::text`, ask)
	if row, _ := readUserThread(t, store, "alice", ask); !row.unread {
		t.Fatalf("answered ask after a session reply: %+v, want unread", row)
	}
	if _, err := store.Pool.Exec(ctx, `
		update asks set answer = jsonb_build_object('user', 'alice', 'selected', '[]'::jsonb, 'at', $2::timestamptz) where id = $1
	`, ask, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("change the answer: %v", err)
	}
	if row, _ := readUserThread(t, store, "alice", ask); row.unread {
		t.Fatalf("after alice changes her answer: %+v, want read", row)
	}
}

// A write that lands after user_threads and its comment trigger exist but before the seed runs is
// caught by the trigger, and the seed merges with it rather than undoing it: the row stays, naming
// the reply, and starts read like every thread the seed meets.
func TestUserThreadsSeedMergesWithRowsTheTriggerWrote(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeUserThreads+1)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	comment := insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Ship it?')
		returning id::text`)
	reply := insertReturningID(t, store, `
		insert into comments (issue_key, author, body, reply_to) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Yes.', $1)
		returning id::text`, comment)
	if row, ok := readUserThread(t, store, "alice", comment); !ok || !row.unread {
		t.Fatalf("before the seed: %+v (found %v), want the trigger's unread row", row, ok)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	row, ok := readUserThread(t, store, "alice", comment)
	if !ok || row.unread || row.lastReplyID == nil || *row.lastReplyID != reply {
		t.Fatalf("after the seed: %+v (found %v), want a read row naming %s", row, ok, reply)
	}
}

// A reply that commits while 0098's seed runs, on a thread whose participant the seed is still
// inserting, reaches no row: the trigger's update cannot see the seed's uncommitted row, and the
// seed's snapshot cannot see the reply. 0099 catches it up, so the participant's row is unread and
// names the reply.
func TestUserThreadsCatchUpCountsAReplyTheSeedRaced(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeUserThreads)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	// Alice wrote in the thread before the deploy, so only the seed gives her a row.
	root := insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Ship it?')
		returning id::text`)
	migrateThrough(t, store, beforeUserThreads+3)

	seed, err := migrationFiles.ReadFile("migrations/0098_user_threads_seed.up.sql")
	if err != nil {
		t.Fatalf("read the seed: %v", err)
	}
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the seed: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, string(seed)); err != nil {
		t.Fatalf("run the seed: %v", err)
	}
	// Another connection commits a session's reply while the seed's row is uncommitted.
	reply := insertReturningID(t, store, `
		insert into comments (issue_key, author, body, reply_to) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Yes.', $1)
		returning id::text`, root)
	if _, err := tx.Exec(ctx, `insert into schema_migrations (version) values ($1)`, beforeUserThreads+4); err != nil {
		t.Fatalf("record the seed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the seed: %v", err)
	}
	if row, ok := readUserThread(t, store, "alice", root); !ok || row.unread || row.lastReplyID != nil {
		t.Fatalf("after the raced seed: %+v (found %v), want the read row that missed the reply", row, ok)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	row, ok := readUserThread(t, store, "alice", root)
	if !ok || !row.unread || row.lastReplyID == nil || *row.lastReplyID != reply {
		t.Fatalf("after the catch-up: %+v (found %v), want an unread row naming %s", row, ok, reply)
	}
}

// An answer is a write like a reply. One given before the deploy is read, as every stored write
// is ("Everything read"), and the catch-up leaves it read; one given after makes every other
// participant's row unread, the asker's included, naming the ask, and leaves the answerer's read.
func TestUserThreadsCountAnAnswerAsAWrite(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeUserThreads)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	ask := insertReturningID(t, store, `
		insert into asks (issue_key, author, question, state, answer)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Which region?', 'answered',
			jsonb_build_object('user', 'bob', 'selected', '["eu-west-1"]'::jsonb, 'at', now() - interval '1 hour'))
		returning id::text`)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, login := range []string{"alice", "bob"} {
		if row, ok := readUserThread(t, store, login, ask); !ok || row.unread {
			t.Fatalf("%s's row after the deploy: %+v (found %v), want read", login, row, ok)
		}
	}
	if row, _ := readUserThread(t, store, "alice", ask); row.lastReplyID == nil || *row.lastReplyID != ask {
		t.Fatalf("alice's row after the deploy: %+v, want it to name the answer (the ask)", row)
	}

	if _, err := store.Pool.Exec(ctx, `
		update asks set answer = jsonb_build_object('user', 'bob', 'selected', '["us-east-1"]'::jsonb, 'at', clock_timestamp())
		where id = $1
	`, ask); err != nil {
		t.Fatalf("change the answer: %v", err)
	}
	if row, _ := readUserThread(t, store, "alice", ask); !row.unread || row.lastReplyID == nil || *row.lastReplyID != ask {
		t.Fatalf("alice's row after bob's new answer: %+v, want unread naming the ask", row)
	}
	if row, _ := readUserThread(t, store, "bob", ask); row.unread {
		t.Fatalf("bob's row after his own answer: %+v, want read", row)
	}
	insertReturningID(t, store, `
		insert into comments (issue_key, author, body, ask_id) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Thanks.', $1)
		returning id::text`, ask)
	if row, _ := readUserThread(t, store, "alice", ask); row.unread {
		t.Fatalf("alice's row after her own reply: %+v, want read", row)
	}
}

// The seed finds a message's thread where the trigger does: up in_reply_to, stopping after 64
// steps. A message 65 deep belongs to its first reply's thread, not the root's, in both.
func TestUserThreadsSeedStopsTheMessageWalkWhereTheTriggerDoes(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeUserThreads)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	chain := []string{insertReturningID(t, store, `
		insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Root')
		returning id::text`)}
	for depth := 1; depth <= 64; depth++ {
		chain = append(chain, insertReturningID(t, store, `
			insert into messages (issue_key, author, body, in_reply_to) values ('CORE-1', '{"kind":"session","id":"s1"}', 'Deeper', $1)
			returning id::text`, chain[depth-1]))
	}
	insertReturningID(t, store, `
		insert into messages (issue_key, author, body, in_reply_to) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Deepest', $1)
		returning id::text`, chain[64])
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, ok := readUserThread(t, store, "alice", chain[0]); ok {
		t.Fatalf("the seed walked past 64 steps to the root")
	}
	if _, ok := readUserThread(t, store, "alice", chain[1]); !ok {
		t.Fatalf("the seed did not stop at the root 64 steps up")
	}
	// A second message of alice's at the same depth, which the trigger walks, joins that thread.
	insertReturningID(t, store, `
		insert into messages (issue_key, author, body, in_reply_to) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Again', $1)
		returning id::text`, chain[64])
	var threads int
	if err := store.Pool.QueryRow(ctx, `select count(*) from user_threads where login = 'alice'`).Scan(&threads); err != nil {
		t.Fatalf("count alice's threads: %v", err)
	}
	if threads != 1 {
		t.Fatalf("alice has %d threads, want the seed's and the trigger's to be one", threads)
	}
}
