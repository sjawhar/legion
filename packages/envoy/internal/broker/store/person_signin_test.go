package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// The people, box keys and machines these tests use. Every thumbprint is the 43 base64url
// characters an RFC 7638 SHA-256 thumbprint is, which the table's own check holds them to.
const (
	signInPerson   = "ada@example.com"
	signInOther    = "grace@example.com"
	signInBoxOne   = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	signInBoxTwo   = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	signInBoxThree = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	signInMachine  = "example-fleet@example-project.iam.gserviceaccount.com"
	signInOtherVM  = "example-pool@example-project.iam.gserviceaccount.com"
)

// record writes one sign-in through a transaction of its own, which is how the exchange writes
// it: the broker's own transaction carries the token the sign-in is recorded beside.
func record(t *testing.T, st *store.Store, person, thumbprint, machine string) error {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := store.RecordPersonSignIn(ctx, tx, person, thumbprint, machine); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return nil
}

// TestAPersonsFirstTradeRecordsTheSignInAndEachLaterOneUsesIt: the first trade of a person and a
// box key writes the row, and every later trade of that pair moves its last use and nothing
// else, so a person holds one sign-in per box however often that box trades.
func TestAPersonsFirstTradeRecordsTheSignInAndEachLaterOneUsesIt(t *testing.T) {
	st := storetest.Open(t)
	if err := record(t, st, signInPerson, signInBoxOne, signInMachine); err != nil {
		t.Fatalf("the first trade of a box key: %v", err)
	}
	first := onlySignIn(t, st, signInPerson)
	if first.Person != signInPerson || first.Thumbprint != signInBoxOne || first.MachineAccount != signInMachine {
		t.Fatalf("the recorded sign-in = %+v, want %s on %s with %s", first, signInPerson, signInMachine, signInBoxOne)
	}
	if !first.LastUsedAt.Equal(first.CreatedAt) {
		t.Errorf("a sign-in just made was last used at %v and made at %v, want the two alike", first.LastUsedAt, first.CreatedAt)
	}
	if err := record(t, st, signInPerson, signInBoxOne, signInMachine); err != nil {
		t.Fatalf("the second trade of the same box key: %v", err)
	}
	again := onlySignIn(t, st, signInPerson)
	if again.ID != first.ID || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("the second trade made the sign-in %+v, want the one %+v it already had", again, first)
	}
	if !again.LastUsedAt.After(first.LastUsedAt) {
		t.Errorf("the second trade left last_used_at at %v, want it moved past %v", again.LastUsedAt, first.LastUsedAt)
	}
}

// TestAPersonSigningInOnASecondBoxGetsASignInOfItsOwn: a thumbprint that is simply new is a
// second sign-in, not a refusal, so a person works on as many boxes as they sign in on; and one
// person's sign-ins are never another's, even on the same machine.
func TestAPersonSigningInOnASecondBoxGetsASignInOfItsOwn(t *testing.T) {
	st := storetest.Open(t)
	for _, c := range []struct{ person, thumbprint, machine string }{
		{signInPerson, signInBoxOne, signInMachine},
		{signInPerson, signInBoxTwo, signInOtherVM},
		{signInOther, signInBoxThree, signInMachine},
	} {
		if err := record(t, st, c.person, c.thumbprint, c.machine); err != nil {
			t.Fatalf("record(%s, %s): %v", c.person, c.machine, err)
		}
	}
	mine, err := st.PersonSignIns(context.Background(), signInPerson)
	if err != nil {
		t.Fatalf("PersonSignIns: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("PersonSignIns(%s) = %+v, want the two boxes they signed in on and no one else's", signInPerson, mine)
	}
	for _, in := range mine {
		if in.Person != signInPerson {
			t.Errorf("the list carries %s's sign-in %+v", in.Person, in)
		}
	}
	// Newest first, which is the order the person's own page reads.
	if !mine[0].CreatedAt.After(mine[1].CreatedAt) && mine[0].Thumbprint != signInBoxTwo {
		t.Errorf("PersonSignIns answered %+v, want the newest sign-in first", mine)
	}
}

// TestARevokedSignInIsRefusedForeverAndLeavesTheOthersWorking: the revoke refuses that one box
// key from then on, whoever presents it and however often, while the person's other boxes go on
// trading. A key revoked once is refused even when it is presented again, since the revoked row
// keeps that pair's slot and the trade's own conflict lands on it.
func TestARevokedSignInIsRefusedForeverAndLeavesTheOthersWorking(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	for _, thumbprint := range []string{signInBoxOne, signInBoxTwo} {
		if err := record(t, st, signInPerson, thumbprint, signInMachine); err != nil {
			t.Fatalf("record(%s): %v", thumbprint, err)
		}
	}
	revoked := signInFor(t, st, signInPerson, signInBoxOne)
	if _, _, err := revokeSignIn(t, st, revoked.ID, signInPerson); err != nil {
		t.Fatalf("RevokePersonSignInTx: %v", err)
	}
	for i := range 2 {
		if err := record(t, st, signInPerson, signInBoxOne, signInMachine); !errors.Is(err, store.ErrSignInRevoked) {
			t.Fatalf("trade %d of the revoked box key = %v, want %v", i, err, store.ErrSignInRevoked)
		}
	}
	if err := record(t, st, signInPerson, signInBoxTwo, signInMachine); err != nil {
		t.Fatalf("the person's other box = %v, want it still trading: revoking one sign-in ends one", err)
	}
	live, err := st.PersonSignIns(ctx, signInPerson)
	if err != nil {
		t.Fatalf("PersonSignIns: %v", err)
	}
	if len(live) != 1 || live[0].Thumbprint != signInBoxTwo {
		t.Fatalf("PersonSignIns after the revoke = %+v, want the one box key that is left", live)
	}
}

// TestASignInsBoxKeyPresentedFromAnotherMachineIsRefused: a sign-in is the person, the key and
// the machine together, and the launcher credential it holds is keyed by all three
// (exchange.PersonSubject). A row that followed its key to a new machine would ask for a
// subject no live credential has, and every trade of it from then on would collide on
// launcher_credentials_live_key and be refused invalid_grant with nothing naming the cure. So
// the machine is held, the row keeps the account it was made with, and the box signs in again.
func TestASignInsBoxKeyPresentedFromAnotherMachineIsRefused(t *testing.T) {
	st := storetest.Open(t)
	if err := record(t, st, signInPerson, signInBoxOne, signInMachine); err != nil {
		t.Fatalf("the first trade: %v", err)
	}
	made := onlySignIn(t, st, signInPerson)
	if err := record(t, st, signInPerson, signInBoxOne, signInOtherVM); !errors.Is(err, store.ErrSignInMachineChanged) {
		t.Fatalf("the same box key from %s = %v, want %v", signInOtherVM, err, store.ErrSignInMachineChanged)
	}
	kept := onlySignIn(t, st, signInPerson)
	if kept.ID != made.ID || kept.MachineAccount != signInMachine {
		t.Fatalf("the sign-in is now %+v, want the one row it was, still on %s", kept, signInMachine)
	}
	if err := record(t, st, signInPerson, signInBoxOne, signInMachine); err != nil {
		t.Fatalf("the machine the sign-in was made on trades again: %v, want it still working", err)
	}
	// A revoke outranks the machine in the wording, since it is the refusal that is final for
	// the person rather than for the box.
	revoked := signInFor(t, st, signInPerson, signInBoxOne)
	if _, _, err := revokeSignIn(t, st, revoked.ID, signInPerson); err != nil {
		t.Fatalf("RevokePersonSignInTx: %v", err)
	}
	if err := record(t, st, signInPerson, signInBoxOne, signInOtherVM); !errors.Is(err, store.ErrSignInRevoked) {
		t.Fatalf("a revoked sign-in's key from another machine = %v, want %v", err, store.ErrSignInRevoked)
	}
}

// TestRevokingASignInEndsTheTokensMintedUnderIt: the revoke ends, in its own transaction, every
// live access token of that person bound to that box key, and leaves every other token alone —
// the same person's token on another box, and another person's on this one. Without it a token
// already minted would go on writing for up to BROKER_ACCESS_TOKEN_SECONDS past the revoke.
func TestRevokingASignInEndsTheTokensMintedUnderIt(t *testing.T) {
	st := storetest.Open(t)
	for _, c := range []struct{ person, thumbprint string }{
		{signInPerson, signInBoxOne},
		{signInPerson, signInBoxTwo},
		{signInOther, signInBoxOne},
	} {
		if err := record(t, st, c.person, c.thumbprint, signInMachine); err != nil {
			t.Fatalf("record(%s, %s): %v", c.person, c.thumbprint, err)
		}
	}
	// Two tokens on the revoked box and one on each of the others, written as the exchange
	// writes them: the person and the bound key live in the stored session's JSON.
	for _, c := range []struct{ signature, person, thumbprint string }{
		{"token-revoked-1", signInPerson, signInBoxOne},
		{"token-revoked-2", signInPerson, signInBoxOne},
		{"token-other-box", signInPerson, signInBoxTwo},
		{"token-other-person", signInOther, signInBoxOne},
	} {
		storeAccessToken(t, st, c.signature, c.person, c.thumbprint)
	}
	target := signInFor(t, st, signInPerson, signInBoxOne)
	thumbprint, ended, err := revokeSignIn(t, st, target.ID, signInPerson)
	if err != nil {
		t.Fatalf("RevokePersonSignInTx: %v", err)
	}
	if thumbprint != signInBoxOne {
		t.Fatalf("the revoke answered the box key %q, want %q: it is what names the launcher credential the sign-in held", thumbprint, signInBoxOne)
	}
	if ended != 2 {
		t.Fatalf("the revoke ended %d tokens, want the 2 minted on that box under that person", ended)
	}
	for signature, want := range map[string]bool{
		"token-revoked-1":    false,
		"token-revoked-2":    false,
		"token-other-box":    true,
		"token-other-person": true,
	} {
		if got := tokenActive(t, st, signature); got != want {
			t.Errorf("%s is active=%t after the revoke, want %t", signature, got, want)
		}
	}
	// A second revoke of the same sign-in ends nothing and refuses nothing, as revoking a
	// machine login twice answers the same.
	if thumbprint, ended, err := revokeSignIn(t, st, target.ID, signInPerson); err != nil || ended != 0 || thumbprint != "" {
		t.Fatalf("revoking it again = %q, %d, %v; want no box key, no token ended and no error", thumbprint, ended, err)
	}
}

// TestRevokingASignInThatIsNotThePersonsIsAnUnknownId: a sign-in of anyone else, and an id
// nothing has, answer alike, so nobody learns another person's sign-ins by trying ids.
func TestRevokingASignInThatIsNotThePersonsIsAnUnknownId(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if err := record(t, st, signInOther, signInBoxOne, signInMachine); err != nil {
		t.Fatalf("record: %v", err)
	}
	theirs := signInFor(t, st, signInOther, signInBoxOne)
	for _, id := range []string{theirs.ID, "00000000-0000-4000-8000-000000000000"} {
		if _, _, err := revokeSignIn(t, st, id, signInPerson); !errors.Is(err, store.ErrNoSignIn) {
			t.Errorf("RevokePersonSignInTx(%s as %s) = %v, want %v", id, signInPerson, err, store.ErrNoSignIn)
		}
	}
	if live, err := st.PersonSignIns(ctx, signInOther); err != nil || len(live) != 1 {
		t.Fatalf("the other person's sign-in = %+v, %v; want it untouched", live, err)
	}
}

// TestPersonSignInsRefusesAValueOutsideItsVocabulary: the migration's own checks close every
// word this table holds, so a login in capitals, a thumbprint that is not an RFC 7638 one, and a
// blank machine account are refused by Postgres and never reach a person's own list. A login the
// caller spells in capitals is lowercased by the writer rather than refused, since that is the
// one canonical form every login the broker compares is in.
func TestPersonSignInsRefusesAValueOutsideItsVocabulary(t *testing.T) {
	st := storetest.Open(t)
	if err := record(t, st, "Ada@Example.com", signInBoxOne, "Example-Fleet@example-project.iam.gserviceaccount.com"); err != nil {
		t.Fatalf("a login and an account in capitals: %v, want them written in lowercase", err)
	}
	in := onlySignIn(t, st, signInPerson)
	if in.Person != signInPerson || in.MachineAccount != signInMachine {
		t.Fatalf("the recorded sign-in = %+v, want the lowercase login and account", in)
	}
	for _, c := range []struct{ name, person, thumbprint, machine string }{
		{name: "a blank person", person: "", thumbprint: signInBoxTwo, machine: signInMachine},
		{name: "a thumbprint that is too short", person: signInOther, thumbprint: "AAAA", machine: signInMachine},
		{name: "a thumbprint carrying a character base64url has not", person: signInOther, thumbprint: signInBoxTwo[:42] + "=", machine: signInMachine},
		{name: "a thumbprint that is a provider's own sentence", person: signInOther, thumbprint: "the key ada@example.com signed with", machine: signInMachine},
		{name: "a blank machine account", person: signInOther, thumbprint: signInBoxTwo, machine: ""},
	} {
		if err := record(t, st, c.person, c.thumbprint, c.machine); err == nil {
			t.Errorf("record(%s) was accepted, want Postgres to refuse it", c.name)
		}
	}
}

// revokeSignIn revokes one sign-in through a transaction of its own, which is how
// enroll.Service.RevokePersonSignIn calls it: the credential that sign-in held is ended in that
// same transaction, which is the enrollment service's half and not this one's.
func revokeSignIn(t *testing.T, st *store.Store, id, person string) (string, int, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	thumbprint, ended, err := store.RevokePersonSignInTx(ctx, tx, id, person)
	if err != nil {
		return "", 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return thumbprint, ended, nil
}

// onlySignIn is the one live sign-in person holds, failing the test when they hold any other
// number of them.
func onlySignIn(t *testing.T, st *store.Store, person string) store.PersonSignIn {
	t.Helper()
	live, err := st.PersonSignIns(context.Background(), person)
	if err != nil {
		t.Fatalf("PersonSignIns: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("PersonSignIns(%s) = %+v, want exactly one", person, live)
	}
	return live[0]
}

// signInFor is person's live sign-in on the box key thumbprint names.
func signInFor(t *testing.T, st *store.Store, person, thumbprint string) store.PersonSignIn {
	t.Helper()
	live, err := st.PersonSignIns(context.Background(), person)
	if err != nil {
		t.Fatalf("PersonSignIns: %v", err)
	}
	for _, in := range live {
		if in.Thumbprint == thumbprint {
			return in
		}
	}
	t.Fatalf("PersonSignIns(%s) = %+v, which holds no sign-in on %s", person, live, thumbprint)
	return store.PersonSignIn{}
}

// storeAccessToken writes one live oauth_access_tokens row shaped as the exchange's own: the
// stored request's session carries the person among its extra claims and the thumbprint of the
// key the token is DPoP-bound to, which together are what the revoke selects on.
func storeAccessToken(t *testing.T, st *store.Store, signature, person, thumbprint string) {
	t.Helper()
	request, err := json.Marshal(map[string]any{
		"client_id": "dispatch-token-exchange",
		"session": map[string]any{
			"extra":          map[string]any{"person": person, "session": "example-session"},
			"jwk_thumbprint": thumbprint,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Pool.Exec(context.Background(), `insert into oauth_access_tokens (signature, request_id, request, expires_at)
		values ($1, $2, $3, now() + interval '15 minutes')`, signature, signature, request)
	if err != nil {
		t.Fatalf("store the access token %s: %v", signature, err)
	}
}

// tokenActive reports whether the access token signature names is still live.
func tokenActive(t *testing.T, st *store.Store, signature string) bool {
	t.Helper()
	var active bool
	err := st.Pool.QueryRow(context.Background(), `select active from oauth_access_tokens where signature = $1`, signature).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the access token %s is gone, want it still stored and only deactivated", signature)
	}
	if err != nil {
		t.Fatalf("read the access token %s: %v", signature, err)
	}
	return active
}

// TestMigrationTwentyFiveHoldsOneSignInPerPersonAndBoxKey: the unique index is what makes a
// person's second trade of a box key move the row it already has rather than add another, and a
// revoked row is inside it, so the pair's slot is kept for good and a trade of a revoked key
// has the index's own conflict to land on rather than a read that a concurrent revoke can slip
// past.
func TestMigrationTwentyFiveHoldsOneSignInPerPersonAndBoxKey(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	insert := func() error {
		_, err := st.Pool.Exec(ctx, `insert into person_signins (person, thumbprint, machine_account)
			values ($1, $2, $3)`, signInPerson, signInBoxOne, signInMachine)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("the first sign-in: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("a second sign-in of one person and one box key was accepted, want the unique index to refuse it")
	}
	if _, err := st.Pool.Exec(ctx, `update person_signins set revoked_at = now() where person = $1`, signInPerson); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("a sign-in beside a revoked one of the same pair was accepted, want the revoked row to keep the pair's slot")
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, `select count(*) from person_signins`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("the table holds %d rows, want the one revoked row and no other", rows)
	}
	// A revoked time before the row was made is refused: a revoke that could predate its own
	// sign-in would read as a sign-in that never worked.
	if _, err := st.Pool.Exec(ctx, `update person_signins set revoked_at = created_at - interval '1 hour' where revoked_at is not null`); err == nil {
		t.Error("a revoke dated before its sign-in was accepted, want Postgres to refuse it")
	}
}

// TestATradeRacingTheRevokeThatCommitsFirstIsRefused: the revoke holds its transaction open
// while the trade's statement waits on the row it locked, so the trade's snapshot is older than
// the revoke. A statement that read the revoked rows first would find none, insert, and leave a
// second live row for one person and box key behind the revoke's back — the whole reason the
// index is not partial and the conflict decides.
func TestATradeRacingTheRevokeThatCommitsFirstIsRefused(t *testing.T) {
	st := storetest.Open(t, "pool_max_conns=6")
	ctx := context.Background()
	if err := record(t, st, signInPerson, signInBoxOne, signInMachine); err != nil {
		t.Fatalf("the first trade: %v", err)
	}
	signIn := signInFor(t, st, signInPerson, signInBoxOne)

	revoking, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the revoke: %v", err)
	}
	defer revoking.Rollback(ctx)
	if _, _, err := store.RevokePersonSignInTx(ctx, revoking, signIn.ID, signInPerson); err != nil {
		t.Fatalf("RevokePersonSignInTx: %v", err)
	}

	traded := make(chan error, 1)
	go func() {
		trading, err := st.Pool.Begin(ctx)
		if err != nil {
			traded <- err
			return
		}
		defer trading.Rollback(ctx)
		if err := store.RecordPersonSignIn(ctx, trading, signInPerson, signInBoxOne, signInMachine); err != nil {
			traded <- err
			return
		}
		traded <- trading.Commit(ctx)
	}()

	// The trade is waiting on the row the revoke locked, which is the race: its snapshot was
	// taken before the revoke committed.
	storetest.AwaitLockWaiters(t, st.Pool, revoking, 1)
	if err := revoking.Commit(ctx); err != nil {
		t.Fatalf("commit the revoke: %v", err)
	}
	if err := <-traded; !errors.Is(err, store.ErrSignInRevoked) {
		t.Fatalf("the trade racing the revoke = %v, want %v", err, store.ErrSignInRevoked)
	}

	var live, all int
	if err := st.Pool.QueryRow(ctx, `select count(*) filter (where revoked_at is null), count(*)
		from person_signins where person = $1 and thumbprint = $2`, signInPerson, signInBoxOne).Scan(&live, &all); err != nil {
		t.Fatal(err)
	}
	if live != 0 || all != 1 {
		t.Fatalf("the pair holds %d live of %d rows, want the one revoked row and nothing live", live, all)
	}
}
