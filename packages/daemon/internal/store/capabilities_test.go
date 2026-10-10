package store

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// liveReport is a session's report of every live row, from tmuxClaim(token)'s process, with one
// row failing: what a ready normalised and the daemon took at minute 2 of a measurement at minute 1.
func liveReport(token claim.Token) capabilities.Report {
	rows := make([]capabilities.Row, 0, len(capabilities.Live()))
	for _, name := range capabilities.Live() {
		row := capabilities.Row{Name: name, OK: true, Detail: string(name) + " checked"}
		if name == capabilities.GitHub {
			row.OK, row.Detail = false, "gh api user: HTTP 401"
		}
		rows = append(rows, row)
	}
	return capabilities.Report{
		Claim:      token,
		Generation: 4,
		Locator:    *tmuxClaim(token).Locator,
		MeasuredAt: at(1),
		ReportedAt: at(2),
		ElapsedMs:  1200,
		Rows:       rows,
	}
}

// sameReport compares two reports field by field, with the times compared as instants: Postgres
// hands a timestamptz back in the session's zone, not the one it was written in.
func sameReport(t *testing.T, got, want capabilities.Report) {
	t.Helper()
	untimed := func(r capabilities.Report) capabilities.Report {
		r.MeasuredAt, r.ReportedAt = time.Time{}, time.Time{}
		return r
	}
	if !reflect.DeepEqual(untimed(got), untimed(want)) {
		t.Errorf("report read back differs:\n got %+v\nwant %+v", got, want)
	}
	for _, field := range []struct {
		name      string
		got, want time.Time
	}{
		{"MeasuredAt", got.MeasuredAt, want.MeasuredAt},
		{"ReportedAt", got.ReportedAt, want.ReportedAt},
	} {
		if !field.got.Equal(field.want) {
			t.Errorf("report %s = %v, want %v", field.name, field.got, field.want)
		}
	}
}

func onlyReport(t *testing.T, store *Store) capabilities.Report {
	t.Helper()
	reports, err := store.CapabilityReports(context.Background())
	if err != nil {
		t.Fatalf("capability reports: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("capability reports = %d rows, want 1: %+v", len(reports), reports)
	}
	return reports[0]
}

func TestACapabilityReportRoundTrips(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	want := liveReport(c.Token)

	if err := store.PutCapabilityReport(ctx, want); err != nil {
		t.Fatalf("put capability report: %v", err)
	}

	sameReport(t, onlyReport(t, store), want)
}

// A claim has one report, its latest: the relaunched process's ready replaces what the earlier one
// measured, so the store never renders a session from a process that no longer runs.
func TestASecondCapabilityReportReplacesTheClaimsFirst(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	first := liveReport(c.Token)
	if err := store.PutCapabilityReport(ctx, first); err != nil {
		t.Fatalf("put the first report: %v", err)
	}
	second := liveReport(c.Token)
	second.Generation, second.MeasuredAt, second.ReportedAt, second.ElapsedMs = 5, at(3), at(4), 800
	second.Locator.Incarnation = "31002:900002"
	second.Locator.Tmux = &runtime.TmuxLocator{Window: "@4", Pane: "%52"}
	for i := range second.Rows {
		second.Rows[i].OK, second.Rows[i].Detail = true, string(second.Rows[i].Name)+" checked again"
	}

	if err := store.PutCapabilityReport(ctx, second); err != nil {
		t.Fatalf("put the second report: %v", err)
	}

	sameReport(t, onlyReport(t, store), second)
}

// Reports read back in claim token order, one per claim, so a daemon loading them at boot reads a
// stable list whatever order the readies arrived in.
func TestCapabilityReportsReadInClaimTokenOrder(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	for _, token := range []claim.Token{"legion-LEGION-209-tester", "legion-LEGION-209-implementer"} {
		if err := store.PutClaim(ctx, tmuxClaim(token)); err != nil {
			t.Fatalf("put claim %s: %v", token, err)
		}
		if err := store.PutCapabilityReport(ctx, liveReport(token)); err != nil {
			t.Fatalf("put the report of %s: %v", token, err)
		}
	}

	reports, err := store.CapabilityReports(ctx)
	if err != nil {
		t.Fatalf("capability reports: %v", err)
	}
	var tokens []claim.Token
	for _, report := range reports {
		tokens = append(tokens, report.Claim)
	}
	want := []claim.Token{"legion-LEGION-209-implementer", "legion-LEGION-209-tester"}
	if !reflect.DeepEqual(tokens, want) {
		t.Fatalf("capability reports read %v, want %v", tokens, want)
	}
}

// A claim's report goes with the claim. The store has no claim delete of its own, so the cascade
// is driven with the SQL a claim's removal would run.
func TestADeletedClaimTakesItsCapabilityReportWithIt(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if err := store.PutCapabilityReport(ctx, liveReport(c.Token)); err != nil {
		t.Fatalf("put capability report: %v", err)
	}

	if _, err := store.pool.Exec(ctx, `delete from claims where token = $1`, string(c.Token)); err != nil {
		t.Fatalf("delete the claim: %v", err)
	}

	reports, err := store.CapabilityReports(ctx)
	if err != nil {
		t.Fatalf("capability reports: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("the deleted claim's report remains: %+v", reports)
	}
}

// A report belongs to a claim the store holds: one of a claim it does not is refused by the foreign
// key, naming the claim, rather than kept as a row nothing renders.
func TestPutCapabilityReportRefusesAClaimTheStoreDoesNotHold(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)

	err := store.PutCapabilityReport(ctx, liveReport("legion-LEGION-209-implementer"))
	if err == nil {
		t.Fatal("a report of a claim the store does not hold was kept")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("refusal is not the foreign key's (23503): %v", err)
	}
	if !strings.Contains(err.Error(), "legion-LEGION-209-implementer") {
		t.Errorf("refusal does not name the claim: %v", err)
	}
}

// generation is a bigint, as claims.generation is: a value past it is refused before Postgres
// would, naming the claim.
func TestPutCapabilityReportRefusesAGenerationPastBigint(t *testing.T) {
	store := migratedStore(t)
	report := liveReport("legion-LEGION-209-implementer")
	report.Generation = math.MaxInt64 + 1

	err := store.PutCapabilityReport(context.Background(), report)
	if err == nil {
		t.Fatal("a generation past bigint was kept")
	}
	for _, want := range []string{"legion-LEGION-209-implementer", "does not fit a bigint"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

// A stored locator is checked where it is read back, as a claim's is: one naming a process the
// daemon could not recognise refuses the read by claim. One naming the claim alone — a ready from a
// claim whose machine held no process — names no process, and reads back as it was kept.
func TestCapabilityReportsValidatesAStoredLocatorThatNamesAProcess(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-reviewer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	bare := liveReport(c.Token)
	bare.Locator = runtime.Locator{Claim: c.Token}
	if err := store.PutCapabilityReport(ctx, bare); err != nil {
		t.Fatalf("put a report whose locator names the claim alone: %v", err)
	}
	sameReport(t, onlyReport(t, store), bare)

	if _, err := store.pool.Exec(ctx,
		`update claim_capabilities set locator = '{"runtime":"tmux","claim":"legion-LEGION-209-reviewer","incarnation":"1:2"}' where claim_token = $1`,
		string(c.Token),
	); err != nil {
		t.Fatalf("write a locator with no tmux member: %v", err)
	}
	_, err := store.CapabilityReports(ctx)
	if err == nil {
		t.Fatal("capability reports loaded a locator with no tmux member")
	}
	for _, want := range []string{"legion-LEGION-209-reviewer", "no tmux member"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}
