package admit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	legionstore "github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

type engineStub struct {
	store       record.Store
	recordChild *record.Issue
	done        string
}

func (e engineStub) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if e.recordChild != nil {
		if dispatch, ok := fact.(intake.DispatchIssue); ok && dispatch.Key == e.recordChild.Key {
			if err := e.store.PutIssue(ctx, tx, *e.recordChild); err != nil {
				return intake.Result{}, err
			}
		}
	}
	if e.done != "" {
		stored, err := e.store.Issue(ctx, tx, e.done)
		if err != nil {
			return intake.Result{}, err
		}
		if stored == nil {
			return intake.Result{}, nil
		}
		stored.Phase = phase.Done
		if err := e.store.PutIssue(ctx, tx, *stored); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, nil
}

const testProject = "LEGION"

var fixedNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func newAdmission(t *testing.T, cap int, log *slog.Logger) *Admission {
	t.Helper()
	admission := New(record.NewStore(), cap, testProject, log)
	admission.now = func() time.Time { return fixedNow }
	return admission
}

func apply(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string, fact intake.DispatchIssue, engine intake.Handler) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", eventID, fact, engine, admission); err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
}

type effect struct {
	kind    record.OutboxKind
	issue   string
	payload record.OutboxPayload
}

func assertEffects(t *testing.T, pool *pgxpool.Pool, want []effect) {
	t.Helper()
	if got := effects(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("outbox effects = %#v, want %#v", got, want)
	}
}

func effects(t *testing.T, pool *pgxpool.Pool) []effect {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select kind, issue, payload from outbox order by id`)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	defer rows.Close()
	var got []effect
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode outbox: %v", err)
		}
		got = append(got, effect{kind: row.Kind, issue: row.Issue, payload: payload})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	return got
}

func assertSlots(t *testing.T, pool *pgxpool.Pool, want []record.Slot) {
	t.Helper()
	got := slots(t, pool)
	if len(got) != len(want) {
		t.Fatalf("slots = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i].Issue != want[i].Issue || got[i].Index != want[i].Index || !got[i].AdmittedAt.Equal(want[i].AdmittedAt) {
			t.Fatalf("slots = %#v, want %#v", got, want)
		}
	}
}

func assertWaiting(t *testing.T, pool *pgxpool.Pool, want []string) {
	t.Helper()
	var got []string
	inTx(t, pool, func(tx pgx.Tx) {
		issues, err := record.NewStore().Issues(context.Background(), tx)
		if err != nil {
			t.Fatalf("list issues: %v", err)
		}
		slots, err := record.NewStore().Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		for _, waiting := range record.Waiting(issues, slots) {
			got = append(got, waiting.Key)
		}
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("waiting = %#v, want %#v", got, want)
	}
}

func slots(t *testing.T, pool *pgxpool.Pool) []record.Slot {
	t.Helper()
	var got []record.Slot
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		got, err = record.NewStore().Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
	})
	return got
}

func issue(t *testing.T, pool *pgxpool.Pool, key string) record.Issue {
	t.Helper()
	var got *record.Issue
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		got, err = record.NewStore().Issue(context.Background(), tx, key)
		if err != nil {
			t.Fatalf("read issue: %v", err)
		}
	})
	if got == nil {
		t.Fatalf("issue %s is missing", key)
	}
	return *got
}

func putIssue(t *testing.T, pool *pgxpool.Pool, issue record.Issue) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutIssue(context.Background(), tx, issue); err != nil {
			t.Fatalf("put issue: %v", err)
		}
	})
}

func seedSlotted(t *testing.T, pool *pgxpool.Pool, key, rank string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: key, Project: testProject, Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: rank}); err != nil {
			t.Fatalf("put active issue: %v", err)
		}
		slots, err := records.Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: key, Index: len(slots), AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("put slot: %v", err)
		}
	})
}

func seedWaiting(t *testing.T, pool *pgxpool.Pool, key, rank string) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: key, Project: testProject, Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: rank})
}

func inTx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Errorf("rollback transaction: %v", err)
		}
	}()
	fn(tx)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
}

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Fatal("LEGION_TEST_PG_DSN is required for real Postgres admission tests")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("connect admin database: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "legion_admit_test_" + randomSuffix(t)
	if _, err := admin.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	url := *base
	url.Path = "/" + name
	st, err := legionstore.Open(context.Background(), url.String())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatalf("migrate store: %v", err)
	}
	st.Close()
	pool, err := pgxpool.New(context.Background(), url.String())
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(bytes[:])
}

func testJetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	js := testnats.JetStream(t)
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create notification stream: %v", err)
	}
	return js
}

func reconcile(t *testing.T, pool *pgxpool.Pool, admission *Admission, summaries []dispatch.IssueSummary) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := admission.Reconcile(context.Background(), tx, summaries); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	})
}
