package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// holdingListener is a fake Envoy listener that holds the first send of every idempotency key
// unanswered until the test releases it: the send a process made just before it died, whose frame
// may or may not have landed. Every later send under that key is answered as broadcastListener
// answers it, and recorded there.
type holdingListener struct {
	*broadcastListener
	url     string
	started chan string
	release func()
	mu      sync.Mutex
	held    map[string]int
}

func newHoldingListener(t *testing.T) *holdingListener {
	t.Helper()
	release, releaseOnce := heldUntilReleased(t)
	listener := &holdingListener{started: make(chan string, 16), release: releaseOnce, held: map[string]int{}}
	server, sends := newBroadcastListener(t, agentSessions, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/send" {
			return false
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read a send: %v", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var request struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.Unmarshal(data, &request); err != nil {
			t.Errorf("decode a send: %v", err)
		}
		listener.mu.Lock()
		first := listener.held[request.IdempotencyKey] == 0
		listener.held[request.IdempotencyKey]++
		listener.mu.Unlock()
		if !first {
			return false
		}
		listener.started <- request.IdempotencyKey
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	listener.broadcastListener, listener.url = sends, server.URL
	return listener
}

// answered is how many sends under key the listener answered: every one but the first, held.
func (l *holdingListener) answered(key string) int {
	count := 0
	for _, sent := range keysOf(l.broadcastListener) {
		if sent == key {
			count++
		}
	}
	return count
}

// sweepRoute is one write that delivers to a session, and where its attempt's row lives.
type sweepRoute struct {
	path func(issue, document string) string
	body map[string]any
	// atCommit is a write answered as soon as it commits, its delivery behind the answer: the
	// broadcast. Every other route answers once its own attempt has settled.
	atCommit bool
	// comment says the attempt is a comment mention's, keyed <comment id>:<target>:<mode>; any
	// other is a message's, keyed <message id>:<mode>.
	comment bool
}

// sweepRoutes is every route whose write delivers to a session.
var sweepRoutes = map[string]sweepRoute{
	"a send to one session": {
		path: func(string, string) string { return "/api/v1/agents/s1/messages" },
		body: map[string]any{"body": "Status?", "delivery": "steer"},
	},
	"the live view's model switch": {
		path: func(string, string) string { return "/api/v1/agents/s1/messages" },
		body: map[string]any{"body": "/model claude-opus", "delivery": "steer"},
	},
	"an issue message to a session": {
		path: func(issue, _ string) string { return "/api/v1/issues/" + issue + "/messages" },
		body: map[string]any{"body": "Status?", "target": "session:s1", "delivery": "steer"},
	},
	"a broadcast's recipient": {
		path:     func(string, string) string { return "/api/v1/broadcasts" },
		body:     map[string]any{"body": "Status?", "delivery": "steer", "session_ids": []string{"s1"}},
		atCommit: true,
	},
	"a comment's mention": {
		path: func(issue, _ string) string { return "/api/v1/issues/" + issue + "/comments" },
		body: map[string]any{
			"body": "@s1 look", "delivery": "steer", "mentions": []map[string]string{{"target": "session:s1"}},
		},
		comment: true,
	},
	"a document comment's mention": {
		path: func(_, document string) string { return "/api/v1/artifacts/" + document + "/comments" },
		body: map[string]any{
			"body": "@s1 look", "delivery": "steer", "mentions": []map[string]string{{"target": "session:s1"}},
		},
		comment: true,
	},
}

// INFRA-258. A delivery finishes on the server whether anyone asks again or not. For every route
// whose write delivers to a session: the write's attempt is sent, and its process dies inside the
// send - the client gave up meanwhile - leaving the attempt pending under its claim. A repeat under
// the write's key inside that claim answers the write the key made and sends nothing: the claim's
// holder may still be sending. Then, with no repeat at all, the delivery sweep sends the attempt
// once its claim lapses, under its own number and so its own idempotency key, which the listener
// drops if the first frame did land; a second pass sends nothing more, and the dead sender's late
// outcome changes nothing.
func TestTheDeliverySweepFinishesEveryDeliveringRouteOnce(t *testing.T) {
	for name, route := range sweepRoutes {
		t.Run(name, func(t *testing.T) {
			listener := newHoldingListener(t)
			handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
				envoyURL: listener.url, envoyTimeout: time.Minute,
			})
			// The lease of a run that died is not renewed again; this test ages it itself.
			served.requestKeyRenewal = time.Hour
			issue := createTestIssue(t, handler, "SWEEP", "Swept deliveries")
			document := createProjectDocument(t, handler, "SWEEP", "Runbook", "# Runbook\n\nStep one.\n").ID
			path := route.path(issue, document)
			server := httptest.NewServer(handler)
			// The hold first: closing the server waits for the held handler.
			t.Cleanup(func() {
				listener.release()
				server.Close()
			})

			ctx, abort := context.WithCancel(context.Background())
			defer abort()
			answered := make(chan int, 1)
			go func() {
				data, err := json.Marshal(route.body)
				if err != nil {
					t.Error(err)
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, bytes.NewReader(data))
				if err != nil {
					t.Error(err)
				}
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Dispatch-User", "alice")
				request.Header.Set(contracts.IdempotencyKeyHeader, "swept")
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					answered <- 0
					return
				}
				_ = response.Body.Close()
				answered <- response.StatusCode
			}()
			var key string
			select {
			case key = <-listener.started:
			case <-time.After(10 * time.Second):
				t.Fatal("the write never sent its attempt")
			}
			owner, _, _ := strings.Cut(key, ":")
			if route.atCommit {
				if status := <-answered; status != http.StatusCreated {
					t.Fatalf("the broadcast answered %d, want 201 at its commit", status)
				}
			} else {
				abort()
				if status := <-answered; status != 0 {
					t.Fatalf("the aborted write answered %d, want the client's own giving up", status)
				}
				// The dead process's key: no answer, its lease lapsed.
				if _, err := database.Pool.Exec(context.Background(),
					`update request_keys set leased_at = now() - interval '2 minutes' where idempotency_key = 'swept'`); err != nil {
					t.Fatal(err)
				}
			}

			// A repeat inside the attempt's claim: the write is answered as made, nothing is sent.
			repeat := keyedRequest(t, handler, http.MethodPost, path, route.body, "alice", "swept")
			if repeat.Code != http.StatusCreated || !replayed(repeat) {
				t.Fatalf("a repeat inside the claim: status=%d body=%s, want the made write replayed", repeat.Code, repeat.Body.String())
			}
			if sent := listener.answered(key); sent != 0 {
				t.Fatalf("a repeat inside the claim sent %d frames, want none", sent)
			}

			// The claim lapses with nobody asking again: the sweep sends the attempt, once.
			table, column := "message_deliveries", "message_id"
			if route.comment {
				table, column = "comment_deliveries", "comment_id"
			}
			if _, err := database.Pool.Exec(context.Background(), `update `+table+`
				set claimed_at = now() - interval '2 minutes', created_at = now() - interval '2 minutes'
				where `+column+` = $1`, owner); err != nil {
				t.Fatalf("lapse the attempt's claim: %v", err)
			}
			for pass := range 2 {
				if err := served.resumeOwedDeliveries(context.Background()); err != nil {
					t.Fatalf("sweep pass %d: %v", pass+1, err)
				}
				if sent := listener.answered(key); sent != 1 {
					t.Fatalf("after sweep pass %d the listener answered %d sends under %s, want the one resume", pass+1, sent, key)
				}
			}

			// The dead sender's own outcome, arriving late, is no longer its to record.
			listener.release()
			served.detached.wait()
			var attempts int
			var state string
			if err := database.Pool.QueryRow(context.Background(),
				`select count(*), min(state) from `+table+` where `+column+` = $1`, owner).Scan(&attempts, &state); err != nil {
				t.Fatal(err)
			}
			receiptType, receiptField := "message.delivery", "message_id"
			if route.comment {
				receiptType, receiptField = "comment.delivery", "comment_id"
			}
			var receipts int
			if err := database.Pool.QueryRow(context.Background(),
				`select count(*) from events where type = $1 and payload ->> $2 = $3`, receiptType, receiptField, owner,
			).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || state != "sent" || receipts != 1 {
				t.Fatalf("%s holds %d attempts, the first %q, with %d receipts; want one attempt, sent, with one receipt",
					owner, attempts, state, receipts)
			}
		})
	}
}

// A comment's mentions are delivered on a context its client cannot cancel, a write sent without a
// key included (a keyed one's whole run is detached already): a client that gives up while a
// mention is being sent leaves that send to finish, and its attempt settles as what the listener
// answered rather than as cut off or pending.
func TestACommentsMentionFinishesWhenItsClientGivesUp(t *testing.T) {
	for name, path := range map[string]func(issue, document string) string{
		"an issue comment":   sweepRoutes["a comment's mention"].path,
		"a document comment": sweepRoutes["a document comment's mention"].path,
	} {
		t.Run(name, func(t *testing.T) {
			sending, proceed := make(chan struct{}), make(chan struct{})
			letProceed := sync.OnceFunc(func() { close(proceed) })
			var once sync.Once
			listener, sends := newBroadcastListener(t, agentSessions, func(_ http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send" {
					once.Do(func() {
						close(sending)
						<-proceed
					})
				}
				return false
			})
			handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
			issue := createTestIssue(t, handler, "GONE", "Clients that give up")
			document := createProjectDocument(t, handler, "GONE", "Runbook", "# Runbook\n\nStep one.\n").ID
			server := httptest.NewServer(handler)
			t.Cleanup(func() {
				letProceed()
				server.Close()
			})
			data, err := json.Marshal(sweepRoutes["a comment's mention"].body)
			if err != nil {
				t.Fatal(err)
			}
			ctx, abort := context.WithCancel(context.Background())
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path(issue, document), bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Dispatch-User", "alice")
			gaveUp := make(chan error, 1)
			go func() {
				response, err := http.DefaultClient.Do(request)
				if err == nil {
					_ = response.Body.Close()
				}
				gaveUp <- err
			}()
			<-sending
			abort()
			if err := <-gaveUp; err == nil {
				t.Fatal("the client was answered before it gave up")
			}
			letProceed()
			deadline := time.Now().Add(10 * time.Second)
			var state string
			for time.Now().Before(deadline) {
				if err := database.Pool.QueryRow(context.Background(),
					`select coalesce(min(state), '') from comment_deliveries`).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state == "sent" {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			server.Close()
			served.detached.wait()
			if state != "sent" || len(keysOf(sends)) != 1 {
				t.Fatalf("the mention's attempt is %q after %d sends, want it sent once though the client gave up", state, len(keysOf(sends)))
			}
		})
	}
}

// The sweep resumes only what a dead process owed: an attempt its live sender still holds, one a
// later retry overtook, one opened inside the last minute that its writer has not claimed yet, and
// one past the window a repeat is recognised in are each left alone.
func TestTheDeliverySweepLeavesWhatIsNotOwed(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	author, err := json.Marshal(model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	message := func(attempts ...string) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(context.Background(), `
			insert into messages (author, body, target) values ($1, 'Status?', 'session:s1') returning id::text
		`, author).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for number, shape := range attempts {
			if _, err := database.Pool.Exec(context.Background(), `
				insert into message_deliveries (message_id, attempt, delivery, session_id, state, claimed_at, created_at, requested_by)
				values ($1, $2, 'steer', 's1', 'pending', now() - $3::interval, now() - $4::interval, $5)
			`, id, number+1, strings.Split(shape, "/")[0], strings.Split(shape, "/")[1], author); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	live := message("10 seconds/2 minutes")
	overtaken := message("5 minutes/6 minutes", "1 second/1 minute")
	unclaimed := message()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, requested_by)
		values ($1, 1, 'steer', 's1', 'pending', $2)
	`, unclaimed, author); err != nil {
		t.Fatal(err)
	}
	old := message("5 days/5 days")
	owed := message("5 minutes/5 minutes")
	if err := served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if keys := keysOf(sends); !equalStrings(keys, []string{owed + ":steer"}) {
		t.Fatalf("the sweep sent %v, want only the owed %s (live %s, overtaken %s, unclaimed %s, old %s)",
			keys, owed, live, overtaken, unclaimed, old)
	}
}

// The maintenance loop's pass is what runs the sweep: started as cmd/dispatch starts it, with the
// server's resume, it sends what a dead process owed without anybody asking.
func TestTheMaintenanceLoopResumesOwedDeliveries(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	author, err := json.Marshal(model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		with message as (
			insert into messages (author, body, target) values ($1, 'Status?', 'session:s1') returning id
		)
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, claimed_at, created_at, requested_by)
		select id, 1, 'steer', 's1', 'pending', now() - interval '5 minutes', now() - interval '5 minutes', $1 from message
		returning message_id::text
	`, author).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunSuggestionOutcomeSweep(ctx, database.Pool, 50*time.Millisecond, sweepLeases(t, database), served.resumeOwedDeliveries)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for len(keysOf(sends)) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	<-done
	served.detached.wait()
	if keys := keysOf(sends); !equalStrings(keys, []string{id + ":steer"}) {
		t.Fatalf("the loop sent %v, want the owed attempt of %s", keys, id)
	}
}

// sweepRig is a delivery sweep's database seeded straight from SQL, as a process that died
// mid-delivery leaves it.
type sweepRig struct {
	t        *testing.T
	database *store.Store
}

func (rig sweepRig) exec(sql string, args ...any) {
	rig.t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), sql, args...); err != nil {
		rig.t.Fatalf("seed the sweep: %v\n%s", err, sql)
	}
}

func (rig sweepRig) count(sql string, args ...any) int {
	rig.t.Helper()
	var count int
	if err := rig.database.Pool.QueryRow(context.Background(), sql, args...).Scan(&count); err != nil {
		rig.t.Fatalf("count: %v\n%s", err, sql)
	}
	return count
}

// strandMessages makes count messages to s1 on issue, each with its one attempt in delivery
// pending under a claim that lapsed ago, as old as that claim, and returns their ids.
func (rig sweepRig) strandMessages(issue string, count int, ago string) []string {
	rig.t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		with made as (
			insert into messages (issue_key, author, body, target)
			select $1, '{"kind":"user","id":"alice"}', 'Status ' || n, 'session:s1' from generate_series(1, $2) n
			returning id
		)
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, claimed_at, created_at, requested_by)
		select id, 1, 'steer', 's1', 'pending', now() - $3::interval, now() - $3::interval, '{"kind":"user","id":"alice"}'
		from made
		returning message_id::text
	`, issue, count, ago)
	if err != nil {
		rig.t.Fatalf("strand messages: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		rig.t.Fatalf("strand messages: %v", err)
	}
	return ids
}

// strandKeyedSends makes count keyed sends to s1 on issue whose process died between the
// message's commit and its first attempt, ago: the message, and its key in flight with the
// message as its recovery point and its lease lapsed. It returns the messages' ids.
func (rig sweepRig) strandKeyedSends(issue string, count int, ago string) []string {
	rig.t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		with made as (
			insert into messages (issue_key, author, body, target)
			select $1, '{"kind":"user","id":"alice"}', 'Keyed ' || n, 'session:s1' from generate_series(1, $2) n
			returning id
		)
		insert into request_keys (login, idempotency_key, request_digest, method, path, leased_at, recovery, created_at)
		select 'alice', 'stranded-' || id, 'digest', 'POST', '/api/v1/issues/' || $1 || '/messages',
		       now() - $3::interval, jsonb_build_object('message_id', id, 'delivery', 'steer'), now() - $3::interval
		from made
		returning recovery ->> 'message_id'
	`, issue, count, ago)
	if err != nil {
		rig.t.Fatalf("strand keyed sends: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		rig.t.Fatalf("strand keyed sends: %v", err)
	}
	return ids
}

// strandMentions makes count comments on issue mentioning s1, each with its one attempt pending
// under a claim that lapsed ago, as old as that claim; mentioned false leaves each comment
// without its mention row, as a comment that no longer holds it. It returns the comments' ids.
func (rig sweepRig) strandMentions(issue string, count int, ago string, mentioned bool) []string {
	rig.t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		insert into comments (issue_key, author, body)
		select $1, '{"kind":"user","id":"alice"}', '@s1 look ' || n from generate_series(1, $2) n
		returning id::text
	`, issue, count)
	if err != nil {
		rig.t.Fatalf("strand mentions: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		rig.t.Fatalf("strand mentions: %v", err)
	}
	if mentioned {
		rig.exec(`
			insert into comment_mentions (comment_id, target, delivery)
			select id::uuid, 'session:s1', 'steer' from unnest($1::text[]) id
		`, ids)
	}
	rig.exec(`
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, claimed_at, created_at)
		select id::uuid, 'session:s1', 1, 'steer', 's1', 'pending', now() - $2::interval, now() - $2::interval
		from unnest($1::text[]) id
	`, ids, ago)
	return ids
}

// INFRA-258. The sweep never sticks on an attempt nothing can send. An owed attempt whose issue
// closed - every sender refuses a delivery there - or whose comment no longer holds its mention
// is settled failed with that cause, and the receipt its row owes, instead of being picked again
// on every pass; a keyed send that closed with no attempt at all gets a failed first attempt the
// same way. However many there are, they never crowd what can be sent out of a pass's batch: a
// batch's worth and one more of each kind, all older than one sendable attempt of that kind, and
// one pass delivers the three sendable ones and settles every other. A second pass changes
// nothing.
func TestTheDeliverySweepSettlesWhatItCannotSend(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	rig := sweepRig{t: t, database: database}
	closed := createTestIssue(t, handler, "STUCK", "Closed under its deliveries")
	open := createTestIssue(t, handler, "OPEN", "Still open")
	unsendable := deliverySweepBatch + 1

	closedMessages := rig.strandMessages(closed, unsendable, "10 minutes")
	closedKeyed := rig.strandKeyedSends(closed, unsendable, "10 minutes")
	closedMentions := rig.strandMentions(closed, unsendable-1, "10 minutes", true)
	goneMention := rig.strandMentions(open, 1, "10 minutes", false)[0]
	sendableMessage := rig.strandMessages(open, 1, "5 minutes")[0]
	sendableKeyed := rig.strandKeyedSends(open, 1, "5 minutes")[0]
	posted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+open+"/comments",
		sweepRoutes["a comment's mention"].body, "alice")
	if posted.Code != http.StatusCreated {
		t.Fatalf("post the sendable comment: status=%d body=%s", posted.Code, posted.Body.String())
	}
	sendableMention := decodeBody[struct {
		ID string `json:"id"`
	}](t, posted).ID
	// Its attempt as a sender that died inside the send leaves it.
	rig.exec(`
		update comment_deliveries set state = 'pending', envelope_id = null,
		       claimed_at = now() - interval '5 minutes', created_at = now() - interval '5 minutes'
		where comment_id = $1
	`, sendableMention)
	if closing := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+closed,
		map[string]string{"status": "done"}, "alice"); closing.Code != http.StatusOK {
		t.Fatalf("close the issue: status=%d body=%s", closing.Code, closing.Body.String())
	}
	if len(closedMessages) != unsendable || len(closedKeyed) != unsendable || len(closedMentions) != unsendable-1 {
		t.Fatalf("seeded %d, %d and %d undeliverable attempts, want %d of each kind",
			len(closedMessages), len(closedKeyed), len(closedMentions)+1, unsendable)
	}

	mentionKey := sendableMention + ":session:s1:steer"
	want := []string{mentionKey, mentionKey, sendableKeyed + ":steer", sendableMessage + ":steer"}
	slices.Sort(want)
	settled := func() map[string]int {
		return map[string]int{
			"closed message attempts failed": rig.count(`
				select count(*) from message_deliveries d join messages m on m.id = d.message_id
				where m.issue_key = $1 and d.state = 'failed' and d.error = $2`, closed, undeliverableIssueClosed),
			"closed message attempts pending": rig.count(`
				select count(*) from message_deliveries d join messages m on m.id = d.message_id
				where m.issue_key = $1 and d.state = 'pending'`, closed),
			"closed message receipts": rig.count(`
				select count(*) from events where type = 'message.delivery' and issue_key = $1
				  and payload ->> 'state' = 'failed' and payload ->> 'error' = $2`, closed, undeliverableIssueClosed),
			"closed mention attempts failed": rig.count(`
				select count(*) from comment_deliveries d join comments c on c.id = d.comment_id
				where c.issue_key = $1 and d.state = 'failed' and d.error = $2`, closed, undeliverableIssueClosed),
			"closed mention receipts": rig.count(`
				select count(*) from events where type = 'comment.delivery' and issue_key = $1
				  and payload ->> 'state' = 'failed' and payload ->> 'error' = $2`, closed, undeliverableIssueClosed),
			"gone mention attempts failed": rig.count(`
				select count(*) from comment_deliveries where comment_id = $1 and state = 'failed' and error = $2`,
				goneMention, undeliverableMentionGone),
			"gone mention receipts": rig.count(`
				select count(*) from events where type = 'comment.delivery' and payload ->> 'comment_id' = $1
				  and payload ->> 'error' = $2`, goneMention, undeliverableMentionGone),
			"pending mention attempts": rig.count(`
				select count(*) from comment_deliveries where state = 'pending'`),
			"sendable attempts sent": rig.count(`
				select count(*) from message_deliveries where message_id = any($1::uuid[]) and state = 'sent'`,
				[]string{sendableMessage, sendableKeyed}) + rig.count(`
				select count(*) from comment_deliveries where comment_id = $1 and state = 'sent'`, sendableMention),
		}
	}
	wantSettled := map[string]int{
		// A keyed send with no attempt is settled as its first attempt, so the closed issue holds
		// one failed attempt per message of either kind.
		"closed message attempts failed":  2 * unsendable,
		"closed message attempts pending": 0,
		"closed message receipts":         2 * unsendable,
		"closed mention attempts failed":  unsendable - 1,
		"closed mention receipts":         unsendable - 1,
		"gone mention attempts failed":    1,
		"gone mention receipts":           1,
		"pending mention attempts":        0,
		"sendable attempts sent":          3,
	}
	for pass := range 2 {
		if err := served.resumeOwedDeliveries(context.Background()); err != nil {
			t.Fatalf("sweep pass %d: %v", pass+1, err)
		}
		if keys := keysOf(sends); !slices.Equal(keys, want) {
			t.Fatalf("after sweep pass %d the listener was sent %v, want the comment's first send and the three sendable attempts %v",
				pass+1, keys, want)
		}
		if got := settled(); !maps.Equal(got, wantSettled) {
			t.Fatalf("after sweep pass %d: %v, want %v", pass+1, got, wantSettled)
		}
	}
}

// INFRA-258. The sweep sends exactly the attempt it found owed. A delivery a card retry
// overtook while its first sender still held it has two pending attempts once both senders die,
// and the sweep owes only the retry's, the latest: it sends that one, in its own mode and to its
// own session, and never the older attempt in its place, which stays the retry's to answer.
func TestTheDeliverySweepSendsTheAttemptItFoundOwed(t *testing.T) {
	t.Run("a comment's mention", func(t *testing.T) {
		listener, sends := newBroadcastListener(t, agentSessions)
		handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
		rig := sweepRig{t: t, database: database}
		issue := createTestIssue(t, handler, "BOUND", "Overtaken mentions")
		posted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			sweepRoutes["a comment's mention"].body, "alice")
		if posted.Code != http.StatusCreated {
			t.Fatalf("post the comment: status=%d body=%s", posted.Code, posted.Body.String())
		}
		comment := decodeBody[struct {
			ID string `json:"id"`
		}](t, posted).ID
		// The first attempt in steer, and the card's retry in btw, both left by senders that died.
		rig.exec(`
			update comment_deliveries set state = 'pending', envelope_id = null,
			       claimed_at = now() - interval '10 minutes', created_at = now() - interval '10 minutes'
			where comment_id = $1 and attempt = 1
		`, comment)
		rig.exec(`
			insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, claimed_at, created_at)
			values ($1, 'session:s1', 2, 'btw', 's1', 'pending', now() - interval '5 minutes', now() - interval '5 minutes')
		`, comment)
		before := len(keysOf(sends))
		if err := served.resumeOwedDeliveries(context.Background()); err != nil {
			t.Fatal(err)
		}
		if keys := keysOf(sends); len(keys) != before+1 || !slices.Contains(keys, comment+":session:s1:btw") {
			t.Fatalf("the sweep sent %v after the comment's own send, want the retry's attempt alone, in btw", keys)
		}
		if states := rig.count(`
			select count(*) from comment_deliveries
			where comment_id = $1 and (attempt, state) in ((1, 'pending'), (2, 'sent'))
		`, comment); states != 2 {
			t.Fatalf("after the sweep the attempts are not the first pending and the retry's sent")
		}
	})
	t.Run("a message", func(t *testing.T) {
		listener, sends := newBroadcastListener(t, agentSessions)
		handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
		rig := sweepRig{t: t, database: database}
		issue := createTestIssue(t, handler, "BOUND", "An overtaken message")
		var message string
		if err := database.Pool.QueryRow(context.Background(), `
			insert into messages (issue_key, author, body, target)
			values ($1, '{"kind":"user","id":"alice"}', 'Status?', 'role:planner')
			returning id::text
		`, issue).Scan(&message); err != nil {
			t.Fatal(err)
		}
		// The first attempt went to the role's holder then, s1; the retry to its holder now, s2.
		rig.exec(`
			insert into message_deliveries (message_id, attempt, delivery, session_id, state, claimed_at, created_at, requested_by)
			values ($1, 1, 'steer', 's1', 'pending', now() - interval '10 minutes', now() - interval '10 minutes', '{"kind":"user","id":"alice"}'),
			       ($1, 2, 'steer', 's2', 'pending', now() - interval '5 minutes', now() - interval '5 minutes', '{"kind":"user","id":"alice"}')
		`, message)
		if err := served.resumeOwedDeliveries(context.Background()); err != nil {
			t.Fatal(err)
		}
		if targets := sends.targets(); !slices.Equal(targets, []string{"s2"}) {
			t.Fatalf("the sweep sent to %v, want the retry's attempt to s2 alone", targets)
		}
		if states := rig.count(`
			select count(*) from message_deliveries
			where message_id = $1 and (attempt, state) in ((1, 'pending'), (2, 'sent'))
		`, message); states != 2 {
			t.Fatalf("after the sweep the attempts are not the first pending and the retry's sent")
		}
	})
}

// The sweep's read of the keyed sends it owes a first attempt runs once a minute, over a table that
// holds every keyed write of the last day, so it reads the partial index request_keys_owed_sends
// (0120), which holds only the keyed sends still in flight, rather than every key. The sweep's own
// SQL is explained with sequential scans off, since a table this small is cheapest to scan
// whatever its indexes: the assertion is that the index serves the query, as the custom plan a
// statement's first runs get and as the generic plan Postgres may cache for it afterwards.
func TestTheSweepsKeyedSendReadUsesItsPartialIndex(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	explain := func(query string, args ...any) json.RawMessage {
		t.Helper()
		var planJSON []byte
		if err := tx.QueryRow(ctx, "explain (format json) "+query, args...).Scan(&planJSON); err != nil {
			t.Fatalf("explain %s: %v", query, err)
		}
		var plans []struct {
			Plan json.RawMessage `json:"Plan"`
		}
		if err := json.Unmarshal(planJSON, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("explain output %s: %v, want one plan", planJSON, err)
		}
		return plans[0].Plan
	}
	lease, ttl := served.keyLease().Seconds(), contracts.IdempotencyKeyTTL.Seconds()
	if custom := explain(owedKeyedSendsQuery, lease, ttl, deliverySweepBatch); !planReadsIndex(t, custom, "request_keys_owed_sends") {
		t.Errorf("the custom plan reads no request_keys_owed_sends:\n%s", custom)
	}
	if _, err := tx.Exec(ctx, "prepare owed_keyed_sends (float8, float8, int) as "+owedKeyedSendsQuery); err != nil {
		t.Fatalf("prepare the read: %v", err)
	}
	if _, err := tx.Exec(ctx, "set local plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatalf("force generic plans: %v", err)
	}
	if generic := explain("execute owed_keyed_sends(60, 86400, 100)"); !planReadsIndex(t, generic, "request_keys_owed_sends") {
		t.Errorf("the generic plan reads no request_keys_owed_sends:\n%s", generic)
	}
}
