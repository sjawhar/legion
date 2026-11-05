package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// productionCheckRoleAndWakeVersion is the version of the migration this file tests, so the test
// can migrate through everything before it whatever lands beside it.
const productionCheckRoleAndWakeVersion = 20261105231916

// 20261105231916 gives the production check the role that works it and the time it is next
// looked at, drops the pending cause the daemon's own guess at production used to write, and
// swaps the outbox's check_compare kind for check_wake.
//
// The checks open at the upgrade are the reason the role column has a default: they are already
// being worked by their implementer, and nothing in the migration knows a project's setting, so
// each keeps implementer and everything else of the check — its bound, its look count, its wake
// record — exactly as the previous release left it. wake_at is null for both: until now nothing
// could name a wake.
//
// The check_compare row is the reason the delete stands before the constraint. Postgres validates
// a new CHECK against the rows already in the table, so a store holding one queued check_compare
// row would refuse this release's boot; the check's other rows, check_bound and check_look, are
// left where they are, since their executors are still there.
func TestTheProductionCheckTakesItsRoleAndWakeWith20261105231916(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, productionCheckRoleAndWakeVersion-1)

	opened := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for _, check := range []struct {
		issue string
		looks int
	}{
		{issue: "LEGION-208", looks: 2},
		{issue: "LEGION-209", looks: 1},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into issues
			(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ($1, $1, 'LEGION', 'merged root', 'done', 1, 'done', 'A', 1)`, check.issue); err != nil {
			t.Fatalf("record the closed issue %s: %v", check.issue, err)
		}
		if _, err := store.Pool().Exec(ctx, `insert into production_checks
			(issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at, looks, look_open,
			 look_wake_at, last_wake_kind, last_wake_detail, last_wake_at, pending_cause_kind, pending_cause_detail)
			values ($1, 'LEGION', $1, 'sjawhar/legion', 1300, 'd4e5f6', $2, $3, $4, false,
			 $2, 'merge', 'd4e5f6', $2, 'daemon boot', 'build b00f')`,
			check.issue, opened, opened.Add(336*time.Hour), check.looks); err != nil {
			t.Fatalf("open the production check of %s as the previous release leaves it: %v", check.issue, err)
		}
	}
	queue := func(kind, issue string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, '{}', 0, now(), '')`, kind, issue)
		return err
	}
	for _, row := range []struct{ kind, issue string }{
		{kind: "check_compare", issue: "LEGION-208"},
		{kind: "check_bound", issue: "LEGION-208"},
		{kind: "check_look", issue: "LEGION-209"},
	} {
		if err := queue(row.kind, row.issue); err != nil {
			t.Fatalf("queue a %s row of %s before the migration: %v", row.kind, row.issue, err)
		}
	}
	if err := queue("check_wake", "LEGION-208"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a check_wake row before the migration = %v, want the kind constraint to refuse it: it is the kind the migration exists for", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the check's role and wake: %v", err)
	}

	for _, want := range []struct {
		issue string
		looks int
	}{
		{issue: "LEGION-208", looks: 2},
		{issue: "LEGION-209", looks: 1},
	} {
		var role, lastWakeKind, lastWakeDetail string
		var looks int
		var lookOpen bool
		var boundAt time.Time
		var wakeAt *time.Time
		if err := store.Pool().QueryRow(ctx, `select role, looks, look_open, bound_at, last_wake_kind, last_wake_detail, wake_at
			from production_checks where issue = $1`, want.issue).
			Scan(&role, &looks, &lookOpen, &boundAt, &lastWakeKind, &lastWakeDetail, &wakeAt); err != nil {
			t.Fatalf("read the production check of %s after the migration: %v", want.issue, err)
		}
		if role != "implementer" {
			t.Errorf("%s's check is worked by %q, want implementer: that is the role already working it", want.issue, role)
		}
		if looks != want.looks || lookOpen {
			t.Errorf("%s's check = %d looks (open %v), want %d and none open: the migration counts no look", want.issue, looks, lookOpen, want.looks)
		}
		if !boundAt.Equal(opened.Add(336 * time.Hour)) {
			t.Errorf("%s's check is bound at %s, want %s: the migration moves no bound", want.issue, boundAt, opened.Add(336*time.Hour))
		}
		if lastWakeKind != "merge" || lastWakeDetail != "d4e5f6" {
			t.Errorf("%s's check was last woken by %q %q, want the merge's own wake kept", want.issue, lastWakeKind, lastWakeDetail)
		}
		if wakeAt != nil {
			t.Errorf("%s's check names its next look at %s, want none: until now nothing could name one", want.issue, wakeAt)
		}
	}

	var compares int
	if err := store.Pool().QueryRow(ctx, `select count(*) from outbox where kind = 'check_compare'`).Scan(&compares); err != nil {
		t.Fatalf("count the check_compare rows after the migration: %v", err)
	}
	if compares != 0 {
		t.Errorf("%d check_compare rows survive the migration, want none: no daemon can run one", compares)
	}
	for _, kept := range []struct{ kind, issue string }{
		{kind: "check_bound", issue: "LEGION-208"},
		{kind: "check_look", issue: "LEGION-209"},
	} {
		var rows int
		if err := store.Pool().QueryRow(ctx, `select count(*) from outbox where kind = $1 and issue = $2`,
			kept.kind, kept.issue).Scan(&rows); err != nil {
			t.Fatalf("count the %s rows of %s after the migration: %v", kept.kind, kept.issue, err)
		}
		if rows != 1 {
			t.Errorf("%s has %d %s rows after the migration, want the one it was queued with", kept.issue, rows, kept.kind)
		}
	}

	if err := queue("check_wake", "LEGION-208"); err != nil {
		t.Fatalf("queue a check_wake row after the migration: %v", err)
	}
	if err := queue("check_compare", "LEGION-208"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a check_compare row after the migration = %v, want the kind constraint to refuse it", err)
	}
}
