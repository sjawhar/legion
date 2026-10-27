package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// TestABrokerThatDiesBeforeServingRecordsNoServingBeat runs the REAL broker binary configured so
// that its boot fails after the bind but before srv.Serve: its GitHub exchange issuer answers its
// discovery read only after a delay of several sweeps, and then answers 404, so exchangeKeys fails
// and the broker exits non-zero having served nothing. A beat is a record that a broker is serving,
// so this broker must leave broker_serving as it found it: a fleet crash-looping at such a step
// must read as no broker serving, or the leases that run out meanwhile are swept. The delay is the
// point of the issuer: it gives a serving clock started before the failing step time to beat
// several times, so a clock started too early fails this test on every run rather than on a race
// between its first beat and the exit.
func TestABrokerThatDiesBeforeServingRecordsNoServingBeat(t *testing.T) {
	databaseURL := storetest.URL(t)
	const discoveryDelay = 3 * time.Second
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(discoveryDelay):
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(issuer.Close)
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	db, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the broker's database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(ctx) })
	serving := func() string {
		t.Helper()
		var rows string
		if err := db.QueryRow(ctx, `select coalesce(string_agg(process || ' ' || last_serving_at::text || ' ' || not_serving_after::text, ', ' order by process), '')
			from broker_serving`).Scan(&rows); err != nil {
			t.Fatalf("read broker_serving: %v", err)
		}
		return rows
	}
	before := serving()

	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_EXCHANGE_GITHUB_ISSUER=" + issuer.URL,
		"BROKER_EXCHANGE_GITHUB_AUDIENCE=example-broker-exchange",
		"BROKER_SWEEP_SECONDS=1",
	}})
	if err := broker.AwaitExit(t, 30*time.Second); err == nil {
		t.Fatalf("the broker exited 0, want a fatal exit at its exchange issuer's discovery:\n%s", broker.Log())
	}
	if log := broker.Log(); !strings.Contains(log, "broker: fatal") || !strings.Contains(log, "discovering issuer "+issuer.URL) {
		t.Fatalf("the broker exited, but not on its exchange issuer's discovery:\n%s", log)
	}
	if after := serving(); after != before {
		t.Fatalf("broker_serving after a broker that never served = %q, want it left at %q:\n%s", after, before, broker.Log())
	}
}
