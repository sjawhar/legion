package docs

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// runRoomSync runs rs until the test ends or stop is called, and returns once its first LISTEN
// stands: a notification sent before that reaches nobody. stop returns once rs has returned.
func runRoomSync(t *testing.T, rs *roomSync) (stop func()) {
	t.Helper()
	listening := make(chan struct{}, 1)
	rs.listening = func() {
		select {
		case listening <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rs.run(ctx)
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the room-sync listener did not return within 10s of its context ending")
		}
	}
	t.Cleanup(stop)
	select {
	case <-listening:
	case <-time.After(roomWait(t)):
		t.Fatal("the room-sync listener never listened")
	}
	return stop
}

// Membership is the set of processes holding a room-sync LISTEN connection, read from
// pg_stat_activity by the name that connection carries. Two processes on one database, each on a
// pool of its own, each see both ids, their own included; the NOTIFY sender, a connection of its
// own as well, is never counted; and a process whose listener stops leaves at once, with nothing
// to collect, because its connection is gone.
func TestEveryProcessesRoomSyncListenerIsAMemberUntilItStops(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, second)
	if taskA.processID == "" || taskA.processID == taskB.processID {
		t.Fatalf("the two services' process ids are %q and %q, want two distinct ids", taskA.processID, taskB.processID)
	}

	runRoomSync(t, newRoomSync(taskA))
	stopB := runRoomSync(t, newRoomSync(taskB))
	if err := taskA.roomSyncSender.Notify(ctx, documentAwarenessChannel, "{}"); err != nil {
		t.Fatalf("send through task A's sender: %v", err)
	}

	want := []string{taskA.processID, taskB.processID}
	slices.Sort(want)
	for _, process := range []struct {
		name     string
		database *store.Store
	}{{"task A", database}, {"task B", second}} {
		members, err := roomSyncMembers(ctx, process.database.Pool)
		if err != nil {
			t.Fatalf("%s reads the members: %v", process.name, err)
		}
		if !slices.Equal(members, want) {
			t.Fatalf("%s reads the members %v, want %v: both listeners, and not the sender", process.name, members, want)
		}
	}

	stopB()
	waitFor(t, roomWait(t), "task B to leave the members once its listener stopped", func() bool {
		members, err := roomSyncMembers(ctx, database.Pool)
		if err != nil {
			t.Fatalf("read the members: %v", err)
		}
		return slices.Equal(members, []string{taskA.processID})
	})
}

// The sender is what a request uses to reach the other processes while it holds a transaction of
// the shared pool, so it must take no connection of that pool (store.ErrNestedAcquire) and its
// notification must go out at once, not at the transaction's commit. Here the caller's context
// refuses a second shared connection, each send reaches the other process's handler before the
// transaction ends, and the transaction then rolls back, which would have dropped a notification
// sent inside it. The sender's connection is then killed under it, as one idle for long can die,
// and the next send redials and still arrives.
func TestTheRoomSyncSenderReachesTheOtherProcessFromInsideATransaction(t *testing.T) {
	database := storetest.Open(t)
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	heard := make(chan string, 4)
	listenerB := newRoomSync(taskB)
	listenerB.awareness = func(_ context.Context, payload string) { heard <- payload }
	runRoomSync(t, listenerB)

	ctx := store.WithTransactionTracking(context.Background())
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the request's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := database.Pool.Exec(ctx, `select 1`); !errors.Is(err, store.ErrNestedAcquire) {
		t.Fatalf("a second shared connection inside the transaction = %v, want ErrNestedAcquire: the guard this test leans on is not armed", err)
	}
	send := func(when, payload string) {
		t.Helper()
		if err := taskA.roomSyncSender.Notify(ctx, documentAwarenessChannel, payload); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		select {
		case got := <-heard:
			if got != payload {
				t.Fatalf("%s: task B's handler heard %q, want %q", when, got, payload)
			}
		case <-time.After(roomWait(t)):
			t.Fatalf("%s: task B's handler heard nothing while the sender's caller held its transaction", when)
		}
	}
	send("the first send", "first")

	senderName := roomSyncSenderApplicationPrefix + taskA.processID
	var terminated int
	if err := tx.QueryRow(ctx, `
		select count(*) from (
			select pg_terminate_backend(pid) from pg_stat_activity
			where datname = current_database() and application_name = $1
		) killed
	`, senderName).Scan(&terminated); err != nil {
		t.Fatalf("terminate the sender's connection: %v", err)
	}
	if terminated != 1 {
		t.Fatalf("terminated %d connections named %q, want the sender's one", terminated, senderName)
	}
	waitFor(t, roomWait(t), "the sender's terminated connection to leave pg_stat_activity", func() bool {
		var alive bool
		if err := database.Pool.QueryRow(context.Background(), `
			select exists (select 1 from pg_stat_activity where datname = current_database() and application_name = $1)
		`, senderName).Scan(&alive); err != nil {
			t.Fatalf("read the sender's connection: %v", err)
		}
		return !alive
	})
	send("the send after the sender's connection died", "second")

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back the request's transaction: %v", err)
	}
}
