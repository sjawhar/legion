package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A database that already holds direct messages when 0051 lands has replies nobody ever marked
// read, because read marks did not exist. 0051 marks every such conversation read as of the
// migration, so the deploy does not turn every past reply into an unread one; replies stored
// afterwards still count. The mark is keyed on the canonical login, as the unread count reads it.
func TestUserAgentReadBackfillMarksEveryExistingDirectConversationRead(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 50)

	if _, err := store.Pool.Exec(ctx, `
		insert into messages (id, author, body, target, in_reply_to) values
			('00000000-0000-4000-8000-000000000001', '{"kind":"user","id":"alice"}', 'Where is it?', 'session:s1', null),
			('00000000-0000-4000-8000-000000000002', '{"kind":"session","id":"s1"}', 'At /dash.', 'session:s1', '00000000-0000-4000-8000-000000000001'),
			('00000000-0000-4000-8000-000000000003', '{"kind":"user","id":"bob"}', 'Status?', 'session:s2', null),
			('00000000-0000-4000-8000-000000000004', '{"kind":"session","id":"s3"}', 'Agent to agent.', 'session:s1', null),
			('00000000-0000-4000-8000-000000000005', '{"kind":"user","id":"alice"}', 'To a role.', 'role:planner', null),
			('00000000-0000-4000-8000-000000000006', '{"kind":"user","id":"Alice"}', 'As GitHub spells me.', 'session:s1', null),
			('00000000-0000-4000-8000-000000000007', '{"kind":"user","id":"Alice"}', 'Elsewhere.', 'session:s4', null)
	`); err != nil {
		t.Fatalf("seed messages: %v", err)
	}
	before := time.Now().Add(-time.Minute)
	migrateThrough(t, store, 51)

	rows, err := store.Pool.Query(ctx, `select login, session_id, read_through from user_agent_read order by 1, 2`)
	if err != nil {
		t.Fatalf("read marks: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var login, session string
		var readThrough time.Time
		if err := rows.Scan(&login, &session, &readThrough); err != nil {
			t.Fatal(err)
		}
		if readThrough.Before(before) {
			t.Errorf("%s/%s read through %s, want the migration's own time", login, session, readThrough)
		}
		got = append(got, login+"/"+session)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"alice/s1", "alice/s4", "bob/s2"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("read marks = %v, want %v: one per human, by canonical login, and session they sent a direct message to, and nothing for an agent's message or a role", got, want)
	}
}
