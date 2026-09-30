package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// registerController records the project's controller as `legion controller start` leaves it: a
// session holding the current capability.
func registerController(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into controllers (project, capability_hash, generation, session, secret_hash, registered_at)
		values ($1, $2, 1, 'ses-controller', $3, now())`, testProject, []byte("capability"), []byte("secret")); err != nil {
		t.Fatalf("register the controller: %v", err)
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
// while a slot stands free, once while that wake is still unpublished however many such events
// arrive, and never while every slot is taken or held for a waiting root.
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

// The daemon's periodic tick wakes a registered controller while a slot stands free, so a walk that
// found nothing, or a day with no event, still ends in another walk and the day's report. It is one
// wake while that wake is still unpublished, named by the project, and none while the slots are full.
func TestTheControllerTickWakesTheControllerWhileASlotIsFree(t *testing.T) {
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
	t.Run("every slot taken", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		registerController(t, pool)
		seedSlotted(t, pool, "LEGION-RUNNING", "A")
		tick(t, pool, admission, "tick-1")
		assertControllerNotices(t, pool, nil)
	})
}
