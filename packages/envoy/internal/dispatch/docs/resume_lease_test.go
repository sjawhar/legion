package docs

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Two envoy-dispatch processes run the settlement resumption, each through
// RunSettlementResumption itself and each on a pool of its own (storetest.SecondProcess), and
// both tick at once with a settlement owed past settlementResumeAge: one runs the pass and arms
// it, the other skips, so the owed settlement is resumed once in the period rather than once per
// process (plan D9, audit finding 9).
func TestTwoResumptionRunnersResumeAnOwedSettlementOncePerPeriod(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	firstLeases, secondLeases := excerptLeases(t, database), excerptLeases(t, second)
	artifactID := createDocument(t, database, "# Owed")
	if _, err := database.Pool.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id, last_actor, marked_at) values ($1, null, now() - interval '2 minutes')
	`, artifactID); err != nil {
		t.Fatalf("owe the document's settlement: %v", err)
	}
	first, _ := newTestServiceInstance(t, database)
	other, _ := newTestServiceInstance(t, second)
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// Both first ticks land in one period of the database's clock, not on either side of a
	// boundary, where each would serve a period of its own.
	if untilBoundary := time.Until(time.Now().Truncate(SettlementResumeInterval).Add(SettlementResumeInterval)); untilBoundary < 10*time.Second {
		time.Sleep(untilBoundary + time.Second)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{}, 2)
	for _, runner := range []struct {
		service *Service
		leases  *pgxpool.Pool
	}{{first, firstLeases}, {other, secondLeases}} {
		go func() {
			defer func() { done <- struct{}{} }()
			runner.service.RunSettlementResumption(runCtx, runner.leases)
		}()
	}
	ran := func() int {
		return countExcerptLeaseLines(logs, "dispatch duty: leased pass ran", settlementResumptionLease)
	}
	skipped := func() int {
		return countExcerptLeaseLines(logs, "dispatch duty: leased pass skipped", settlementResumptionLease)
	}
	waitFor(t, roomWait(t), "both runners' first tick to run or skip", func() bool { return ran()+skipped() >= 2 })
	stop()
	<-done
	<-done

	resumed := strings.Count(logs.String(), `msg="dispatch: resumed owed document settlements"`)
	if ran() != 1 || skipped() != 1 || resumed != 1 {
		t.Fatalf("two resumption runners in one period: %d passes ran, %d skipped, the owed settlement resumed %d times; want one, one and once:\n%s",
			ran(), skipped(), resumed, logs.Lines("lease="))
	}
}
