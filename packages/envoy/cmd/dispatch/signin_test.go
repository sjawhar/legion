package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The boot's retirement of the refresh tokens stored in plain text is bounded: a sign-in pool that
// accepts the connection and never answers delays the start by the bound, not by each token's own
// timeout, and the start goes on, every token left in its row for the next boot and the counts
// logged without a token.
func TestRetirePlainRefreshTokensAtBootStopsAtItsBound(t *testing.T) {
	ctx := context.Background()
	pool := signInPool(t, "dispatch-client", "client-secret")
	pool.DelayRevocation(time.Minute)
	boot := resolveWith(t, map[string]string{"DISPATCH_SIGNIN_ISSUER": pool.URL()})
	signIn, err := discoverSignIn(ctx, boot)
	if err != nil {
		t.Fatalf("discover the sign-in pool: %v", err)
	}
	database := storetest.Open(t)
	tokens := []string{"plain-token-1", "plain-token-2", "plain-token-3"}
	for i, token := range tokens {
		if _, err := database.Pool.Exec(ctx, `insert into people (email, refresh_token, confirmed_at) values ($1, $2, now())`, []string{"a@d.example", "b@d.example", "c@d.example"}[i], token); err != nil {
			t.Fatal(err)
		}
	}
	people, _, err := openPeople(boot, t.TempDir(), database.Pool, signIn)
	if err != nil {
		t.Fatalf("openPeople: %v", err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	start := time.Now()
	retirePlainRefreshTokens(ctx, people, 500*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the boot's retirement took %s against a pool that never answers, want about its 500ms bound", elapsed)
	}
	// The store main builds revokes through the discovered pool: without it every token would fail
	// at once with no request made, and this bound would hold for the wrong reason.
	if got := pool.RevocationRequests(); len(got) == 0 {
		t.Error("the boot's retirement sent the sign-in pool nothing, want it to try to revoke the plain tokens")
	}
	out := logs.String()
	if !strings.Contains(out, "retired the refresh tokens stored in plain text") || !strings.Contains(out, "retired=0 failed=3") {
		t.Errorf("logged, want the counts retired=0 failed=3:\n%s", out)
	}
	for _, token := range tokens {
		if strings.Contains(out, token) {
			t.Errorf("the log carries %s:\n%s", token, out)
		}
	}
	var kept int
	if err := database.Pool.QueryRow(ctx, `select count(*) from people where refresh_token like 'plain-token-%'`).Scan(&kept); err != nil || kept != len(tokens) {
		t.Errorf("%d rows still hold their token (%v), want all %d left for the next boot", kept, err, len(tokens))
	}
}
