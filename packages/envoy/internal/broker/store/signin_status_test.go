package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// signInStatusReasons are the eleven coded reasons a failed sign-in may be written with, as
// 0022's check constraint lists them. The approval page's own constants are the other copy;
// what holds the two together is this test, which writes each one through the store.
var signInStatusReasons = []string{
	"invalid_client", "unauthorized_client", "redirect_uri_mismatch", "invalid_grant",
	"issuer_unreachable", "bad_issuer", "bad_audience", "bad_signature", "bad_nonce",
	"not_in_domain", "email_not_verified",
}

// A broker that has written none of these facts has no row at all, which reads as a status whose
// every fact is absent rather than as an error or a row of zero times.
func TestSignInStatusIsAbsentUntilSomethingHappens(t *testing.T) {
	st := storetest.Open(t)
	status, err := st.SignInStatus(context.Background())
	if err != nil {
		t.Fatalf("SignInStatus of a broker that has written nothing: %v", err)
	}
	if status != (store.SignInStatus{}) {
		t.Fatalf("SignInStatus = %+v, want every fact absent", status)
	}
}

// Each writer sets its own columns and leaves every other fact as it was, which is what lets two
// replicas write different facts without losing each other's; and each one's second write
// replaces its own fact alone.
func TestEachSignInStatusWriterTouchesOnlyItsOwnFact(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	checked := time.Now().Add(-4 * time.Hour).Truncate(time.Microsecond)
	signedIn := time.Now().Add(-3 * time.Hour).Truncate(time.Microsecond)
	failed := time.Now().Add(-2 * time.Hour).Truncate(time.Microsecond)
	decided := time.Now().Add(-time.Hour).Truncate(time.Microsecond)

	if err := st.RecordClientCheck(ctx, string(oidc.ClientCheckReady), checked); err != nil {
		t.Fatalf("RecordClientCheck: %v", err)
	}
	if err := st.RecordSignIn(ctx, signedIn); err != nil {
		t.Fatalf("RecordSignIn: %v", err)
	}
	if err := st.RecordFailedSignIn(ctx, "bad_nonce", failed); err != nil {
		t.Fatalf("RecordFailedSignIn: %v", err)
	}
	if err := st.RecordDecision(ctx, decided); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	requireStatus(t, st, store.SignInStatus{
		ClientCheckState: new(string(oidc.ClientCheckReady)), ClientCheckedAt: &checked,
		LastSignInAt: &signedIn, LastFailedSignInAt: &failed, LastFailedSignInReason: new("bad_nonce"),
		LastDecisionAt: &decided,
	})

	// One later check: its own two columns move and nothing else does.
	rechecked := time.Now().Truncate(time.Microsecond)
	if err := st.RecordClientCheck(ctx, string(oidc.ClientCheckIssuerUnreachable), rechecked); err != nil {
		t.Fatalf("RecordClientCheck again: %v", err)
	}
	requireStatus(t, st, store.SignInStatus{
		ClientCheckState: new(string(oidc.ClientCheckIssuerUnreachable)), ClientCheckedAt: &rechecked,
		LastSignInAt: &signedIn, LastFailedSignInAt: &failed, LastFailedSignInReason: new("bad_nonce"),
		LastDecisionAt: &decided,
	})
}

// The row cannot be duplicated, whoever writes it: every writer upserts the one row its primary
// key admits, so there is nothing to sweep and nothing to grow.
func TestSignInStatusHoldsOneRowAndOnlyOne(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	for i := range 3 {
		if err := st.RecordSignIn(ctx, time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("RecordSignIn %d: %v", i, err)
		}
		if err := st.RecordDecision(ctx, time.Now()); err != nil {
			t.Fatalf("RecordDecision %d: %v", i, err)
		}
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, `select count(*) from signin_status`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("signin_status holds %d rows after six writes, want 1", rows)
	}
	// A second row cannot be inserted at all: the key admits one value, and that value is taken.
	if _, err := st.Pool.Exec(ctx, `insert into signin_status (only_row) values (true)`); !store.IsUniqueViolation(err) {
		t.Fatalf("a second row with the same key = %v, want a unique violation", err)
	}
	if _, err := st.Pool.Exec(ctx, `insert into signin_status (only_row) values (false)`); err == nil {
		t.Fatal("a row keyed false was accepted, want the check to refuse it")
	}
}

// Both vocabularies are closed by the database itself, so no provider's own word can reach the
// public answer through this row: each listed word is written, and anything else is refused.
func TestSignInStatusRefusesAWordOutsideItsVocabulary(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	for _, state := range []oidc.ClientCheck{oidc.ClientCheckReady, oidc.ClientCheckRejected, oidc.ClientCheckIssuerUnreachable} {
		if err := st.RecordClientCheck(ctx, string(state), time.Now()); err != nil {
			t.Fatalf("RecordClientCheck(%q): %v", state, err)
		}
	}
	for _, reason := range signInStatusReasons {
		if err := st.RecordFailedSignIn(ctx, reason, time.Now()); err != nil {
			t.Fatalf("RecordFailedSignIn(%q): %v", reason, err)
		}
	}
	for _, state := range []string{"", "ok", "unknown", "Ready", "invalid_client"} {
		if err := st.RecordClientCheck(ctx, state, time.Now()); err == nil {
			t.Errorf("RecordClientCheck(%q) was accepted, want a refusal", state)
		}
	}
	for _, reason := range []string{"", "other", "expired", "the provider said ada@example.com may not sign in"} {
		if err := st.RecordFailedSignIn(ctx, reason, time.Now()); err == nil {
			t.Errorf("RecordFailedSignIn(%q) was accepted, want a refusal", reason)
		}
	}
}

// requireStatus fails t unless the whole row reads as want.
func requireStatus(t *testing.T, st *store.Store, want store.SignInStatus) {
	t.Helper()
	got, err := st.SignInStatus(context.Background())
	if err != nil {
		t.Fatalf("SignInStatus: %v", err)
	}
	for _, f := range []struct {
		name        string
		got, want   *time.Time
		gotS, wantS *string
	}{
		{name: "client_checked_at", got: got.ClientCheckedAt, want: want.ClientCheckedAt},
		{name: "last_sign_in_at", got: got.LastSignInAt, want: want.LastSignInAt},
		{name: "last_failed_sign_in_at", got: got.LastFailedSignInAt, want: want.LastFailedSignInAt},
		{name: "last_decision_at", got: got.LastDecisionAt, want: want.LastDecisionAt},
		{name: "client_check_state", gotS: got.ClientCheckState, wantS: want.ClientCheckState},
		{name: "last_failed_sign_in_reason", gotS: got.LastFailedSignInReason, wantS: want.LastFailedSignInReason},
	} {
		switch {
		case f.want != nil || f.got != nil:
			if f.got == nil || f.want == nil || !f.got.Equal(*f.want) {
				t.Errorf("%s = %v, want %v", f.name, f.got, f.want)
			}
		case f.wantS != nil || f.gotS != nil:
			if f.gotS == nil || f.wantS == nil || *f.gotS != *f.wantS {
				t.Errorf("%s = %v, want %v", f.name, f.gotS, f.wantS)
			}
		}
	}
}
