package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// refusalCode is the code of a fact's refusal, or "" when it was not refused.
func refusalCode(result intake.Result) string {
	if result.Refusal == nil {
		return ""
	}
	return result.Refusal.Code
}

// mergingEngine is an engine of a gated project with a fixed clock, which the tests that drive an
// issue through its later phases share.
func mergingEngine() *Engine {
	return mergingEngineAt(time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))
}

// mergingEngineAt is mergingEngine with its clock at now.
func mergingEngineAt(now time.Time) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", DesignGate: config.DesignGateRootIssues, Linger: time.Hour, ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		Clock: func() time.Time { return now },
	}, nil)
}

// applyFacts applies each fact to pool through engine, failing the test on an error, and returns
// the fact's result.
func applyFacts(t *testing.T, pool *pgxpool.Pool, engine *Engine) func(id string, fact intake.Fact) intake.Result {
	return func(id string, fact intake.Fact) intake.Result {
		t.Helper()
		result, err := intake.ApplyFact(context.Background(), pool, "api", id, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", id, err)
		}
		return result
	}
}

// messageBodies is every Dispatch message the outbox holds, in order.
func messageBodies(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'body' from outbox where kind = 'dispatch_message' order by id")
	if err != nil {
		t.Fatalf("read Dispatch messages: %v", err)
	}
	defer rows.Close()
	bodies := []string{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatalf("scan Dispatch message: %v", err)
		}
		bodies = append(bodies, body)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate Dispatch messages: %v", err)
	}
	return bodies
}
