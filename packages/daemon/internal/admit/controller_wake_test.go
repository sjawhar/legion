package admit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// registerController records the project's controller the way the daemon does when
// `legion controller start` mints a capability and its session registers with it: through the
// store's own MintController and RegisterController, under the project token, never the Dispatch
// key.
func registerController(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	token, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := st.MintController(ctx, token, []byte("capability"))
	if err != nil {
		t.Fatalf("mint the controller capability: %v", err)
	}
	if ok, err := st.RegisterController(ctx, token, generation, "ses-controller", []byte("secret"), time.Now()); err != nil || !ok {
		t.Fatalf("register the controller = %t, %v", ok, err)
	}
}

func tick(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "controller", eventID, intake.ControllerTick{}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
}

func unlabelledTodo(key string) intake.DispatchIssue {
	return intake.DispatchIssue{Key: key, Seq: 1, Type: "issue.updated", Status: "todo", Title: key, Rank: "M"}
}

// An issue nobody handed to Legion moving into todo is a new candidate for the controller's walk,
// which otherwise runs only at its start and when a slot frees. It wakes a registered controller
// while a slot stands free, and never while every slot is taken or held for a waiting root. The
// wake is due todoWakeDelay after the event, and until then every later event finds it pending,
// so an edit burst is one wake even though the outbox publishes whatever is due at once.
func TestATodoIssueNotHandedToLegionWakesTheControllerWhileASlotIsFree(t *testing.T) {
	todo := func(issue string) []effect {
		return []effect{{kind: record.OutboxKindControllerNotice, issue: issue, payload: record.ControllerNotice{Kind: record.TodoNotice}}}
	}
	t.Run("no controller registered", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		apply(t, pool, admission, "todo-1", unlabelledTodo("LEGION-1"), engineStub{})
		assertControllerNotices(t, pool, nil)
	})
	t.Run("a free slot, one wake for a burst", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
		registerController(t, pool)
		seedSlotted(t, pool, "LEGION-RUNNING", "A")
		apply(t, pool, admission, "todo-1", unlabelledTodo("LEGION-1"), engineStub{})
		apply(t, pool, admission, "todo-2", unlabelledTodo("LEGION-2"), engineStub{})
		assertControllerNotices(t, pool, todo("LEGION-1"))
		var due time.Time
		if err := pool.QueryRow(context.Background(), "select next_at from outbox where kind = $1", string(record.OutboxKindControllerNotice)).Scan(&due); err != nil {
			t.Fatalf("read the todo notice's due time: %v", err)
		}
		if !due.Equal(fixedNow.Add(todoWakeDelay)) {
			t.Fatalf("the todo notice is due %s, want %s: todoWakeDelay after the event", due, fixedNow.Add(todoWakeDelay))
		}
		if got := maybeIssue(t, pool, "LEGION-1"); got != nil {
			t.Fatalf("the unlabelled todo issue was recorded: %#v", got)
		}
	})
	t.Run("every slot taken or held for a waiting root", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
		registerController(t, pool)
		seedSlotted(t, pool, "LEGION-RUNNING", "A")
		seedWaiting(t, pool, "LEGION-WAITING", "B")
		apply(t, pool, admission, "todo-1", unlabelledTodo("LEGION-1"), engineStub{})
		assertControllerNotices(t, pool, nil)
	})
}

// The daemon's periodic tick wakes a registered controller whatever the slots, one wake while that
// wake is still unpublished, named by the project. With every slot taken, it is the only wake a
// tree waiting on a refused root claim gets: that tree holds its slot, its architect started
// nothing, and only the controller's walk rechecks the claim. It is also the turn that posts the
// day's report on a day nothing else wakes the controller.
func TestTheControllerTickWakesARegisteredControllerWhateverTheSlots(t *testing.T) {
	tickNotice := []effect{{kind: record.OutboxKindControllerNotice, issue: testProject, payload: record.ControllerNotice{Kind: record.TickNotice}}}
	t.Run("no controller registered", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		tick(t, pool, admission, "tick-1")
		assertControllerNotices(t, pool, nil)
	})
	t.Run("a free slot, one wake for repeated ticks", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		registerController(t, pool)
		tick(t, pool, admission, "tick-1")
		tick(t, pool, admission, "tick-2")
		assertControllerNotices(t, pool, tickNotice)
	})
	t.Run("the only slot held by a tree whose root has not started", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		registerController(t, pool)
		seedSlotted(t, pool, "LEGION-WAITING-ON-CLAIM", "A")
		for i := range 24 {
			tick(t, pool, admission, fmt.Sprintf("tick-%d", i))
		}
		assertControllerNotices(t, pool, tickNotice)
	})
	t.Run("the only slot held by a started tree", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		registerController(t, pool)
		seedSlotted(t, pool, "LEGION-RUNNING", "A")
		running := issue(t, pool, "LEGION-RUNNING")
		running.Phase = phase.Planning
		putIssue(t, pool, running)
		tick(t, pool, admission, "tick-1")
		assertControllerNotices(t, pool, tickNotice)
	})
}
