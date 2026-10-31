package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// xidTakingStatement is a statement that writes, or locks a row, and so may give its transaction a
// transaction id: the first one a transaction runs opens its write phase. It is wider than
// keyed_write_guard_test.go's writeStatement, which counts a row lock as no write.
var xidTakingStatement = regexp.MustCompile(`(?is)\b(insert\s+into|update\s+\S+\s+set|delete\s+from)\b|\bfor\s+(no\s+key\s+)?update\b`)

// requireOneWriteBatch finds, among the committed transactions report saw the request run, the one
// whose statements append an event, and requires that everything it ran from its first write on
// is one pipelined batch carrying every statement of writes, then COMMIT: the server ran the
// write phase back to back, and the client waited on it once. It reads the tracer's statements
// themselves, not the xid polls, so a write phase too quick for a poll to see is judged all the
// same.
func requireOneWriteBatch(t *testing.T, report *storetest.XidReport, writes ...string) {
	t.Helper()
	var transactions []storetest.XidTransaction
	for _, window := range report.Windows {
		transactions = append(transactions, window.Transaction)
	}
	transactions = append(transactions, report.Unsighted...)
	var appending []storetest.XidTransaction
	for _, transaction := range transactions {
		for _, statement := range transaction.Statements {
			if strings.Contains(statement.SQL, "insert into events") {
				appending = append(appending, transaction)
				break
			}
		}
	}
	if len(appending) != 1 {
		t.Fatalf("the request committed %d transactions appending an event, want 1", len(appending))
	}
	statements := appending[0].Statements
	first := -1
	for index, statement := range statements {
		if xidTakingStatement.MatchString(statement.SQL) {
			first = index
			break
		}
	}
	var listing strings.Builder
	for index, statement := range statements {
		listing.WriteString("\n  ")
		if index == first {
			listing.WriteString("-> ")
		}
		sql := strings.Join(strings.Fields(statement.SQL), " ")
		if len(sql) > 160 {
			sql = sql[:160] + "…"
		}
		listing.WriteString(sql)
	}
	if first < 0 {
		t.Fatalf("the event's transaction ran no write:%s", listing.String())
	}
	phase := statements[first:]
	if len(phase) != 2 || !strings.EqualFold(strings.TrimSpace(phase[1].SQL), "commit") {
		t.Fatalf("the write phase ran %d round trips before and with its COMMIT, want one batch and the COMMIT:%s", len(phase), listing.String())
	}
	batch := phase[0].SQL
	for _, write := range writes {
		if !strings.Contains(batch, write) {
			t.Errorf("the write phase's batch carries no %q:%s", write, listing.String())
		}
	}
}

// A write's write phase is one pipelined batch: from its first write to its COMMIT the transaction
// sends every statement at once and reads their results once, so Postgres runs them back to back
// and the transaction holds its id for the server's work, one or two client wakes and the commit,
// never for the time this process takes to be scheduled between two statements. A route joined to
// a document ledger (an anchored comment, an accepted suggestion, whose document operations queue
// their writes beside the route's) and a route that writes without one (a message, whose insert,
// references, event and stamp run as a batch of their own) are both held to it.
func TestWritePhaseSendsItsStatementsAsOneBatch(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	measure := func(t *testing.T, want int, request func(http.Handler) *httptest.ResponseRecorder) *storetest.XidReport {
		t.Helper()
		marked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.handler.ServeHTTP(w, r.WithContext(storetest.MarkXid(r.Context())))
		})
		var response *httptest.ResponseRecorder
		report := f.tracer.Measure(t, f.database.Pool, func() { response = request(marked) })
		if response.Code != want {
			t.Fatalf("the measured request answered %d, want %d: %s", response.Code, want, response.Body.String())
		}
		return report
	}

	t.Run("an anchored comment", func(t *testing.T) {
		report := measure(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/comments", map[string]any{
				"body": "Does dispatch://" + f.issueKey + " still hold once the outbox is sharded?", "anchor": map[string]any{"artifact": "spec", "quote": f.spare[1]},
				"actor": sessionActor(),
			})
		})
		requireOneWriteBatch(t, report, "insert into doc_updates", "insert into doc_settlements_pending", "insert into comments",
			"insert into refs", "insert into events", "update refs set source_seq")
	})

	t.Run("an accepted suggestion", func(t *testing.T) {
		suggestion := f.suggest(t)
		report := measure(t, http.StatusOK, func(h http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, h, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/accept", map[string]any{}, "alice")
		})
		requireOneWriteBatch(t, report, "insert into doc_updates", "update comments", "insert into artifact_versions",
			"insert into document_indexed_versions", "insert into events", "update block_refs set source_seq")
	})

	t.Run("a message", func(t *testing.T) {
		report := measure(t, http.StatusCreated, func(h http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+f.issueKey+"/messages", map[string]any{
				"body":  "Status: the spec at dispatch://" + f.issueKey + " has twenty open anchors; replies welcome.",
				"actor": sessionActor(),
			})
		})
		requireOneWriteBatch(t, report, "insert into messages", "insert into refs", "insert into events", "update refs set source_seq")
	})
}
