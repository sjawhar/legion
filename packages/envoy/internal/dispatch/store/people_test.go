package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

func TestPgPeopleStoreKeepsEachPersonsMembership(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, "signing-key")
	email := "person-" + randomDatabaseSuffix(t) + "@d.example"

	if _, found, err := people.Membership(ctx, email); err != nil || found {
		t.Fatalf("membership before any sign-in: found=%t err=%v, want not found", found, err)
	}
	signedIn := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if err := people.SignIn(ctx, email, "refresh-1", signedIn); err != nil {
		t.Fatalf("sign in: %v", err)
	}
	// Recording a person who already signed in through the pool keeps their membership.
	if err := people.Record(ctx, email); err != nil {
		t.Fatalf("record: %v", err)
	}
	membership, found, err := people.Membership(ctx, email)
	if err != nil || !found || membership.RefreshToken != "refresh-1" || !membership.ConfirmedAt.Equal(signedIn) {
		t.Fatalf("membership after sign-in = %#v found=%t err=%v, want refresh-1 confirmed %s", membership, found, err, signedIn)
	}

	confirmed := signedIn.Add(90 * time.Minute)
	if err := people.Confirm(ctx, email, "refresh-2", confirmed); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	membership, _, err = people.Membership(ctx, email)
	if err != nil || membership.RefreshToken != "refresh-2" || !membership.ConfirmedAt.Equal(confirmed) {
		t.Fatalf("membership after confirm = %#v err=%v, want refresh-2 confirmed %s", membership, err, confirmed)
	}

	if err := people.End(ctx, email); err != nil {
		t.Fatalf("end: %v", err)
	}
	membership, found, err = people.Membership(ctx, email)
	if err != nil || !found || membership.RefreshToken != "" || !membership.ConfirmedAt.IsZero() {
		t.Fatalf("membership after end = %#v found=%t err=%v, want the person kept with neither", membership, found, err)
	}
}

// People are named by lowercase email; the table refuses any other spelling, so no writer can
// make one person two rows.
func TestPeopleTableRefusesAnEmailThatIsNotLowercase(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, "signing-key")
	for _, email := range []string{"Sami@d.example", ""} {
		if err := people.Record(ctx, email); err == nil {
			t.Errorf("Record(%q) was accepted", email)
		}
	}
}

// storedRefreshToken is people.refresh_token for email as the table holds it, as a backup copies
// it; nil when the row holds none.
func storedRefreshToken(t *testing.T, database *Store, email string) *string {
	t.Helper()
	var stored *string
	if err := database.Pool.QueryRow(context.Background(), `select refresh_token from people where email = $1`, email).Scan(&stored); err != nil {
		t.Fatalf("read the stored refresh token of %s: %v", email, err)
	}
	return stored
}

// captureLogs sends slog's default logger to a buffer until t ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// The row a backup copies never holds the refresh token: a sign-in and a confirmation each store it
// sealed under the signing key, in the versioned format, and only a store under that key opens it.
func TestPgPeopleStoreSealsTheRefreshTokenAtRest(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, "signing-key")
	email := "sealed-" + randomDatabaseSuffix(t) + "@d.example"
	confirmed := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, write := range []struct {
		name  string
		write func(token string) error
	}{
		{"sign-in", func(token string) error { return people.SignIn(ctx, email, token, confirmed) }},
		{"confirmation", func(token string) error { return people.Confirm(ctx, email, token, confirmed) }},
	} {
		token := "refresh-" + write.name + "-" + randomDatabaseSuffix(t)
		if err := write.write(token); err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		stored := storedRefreshToken(t, database, email)
		if stored == nil {
			t.Fatalf("after the %s the row holds no refresh token", write.name)
		}
		if strings.Contains(*stored, token) || !strings.HasPrefix(*stored, "v1:") {
			t.Fatalf("after the %s the row holds %q, want the token sealed in the v1: format", write.name, *stored)
		}
		membership, found, err := people.Membership(ctx, email)
		if err != nil || !found || membership.RefreshToken != token || !membership.ConfirmedAt.Equal(confirmed) {
			t.Fatalf("membership after the %s = %#v found=%t err=%v, want %s confirmed %s", write.name, membership, found, err, token, confirmed)
		}
	}
}

// A refresh token that does not open is no refresh token: one sealed under another signing key (the
// key rotated), one sealed for another person (a value moved between rows), and one stored in plain
// text before Dispatch sealed them. The read forgets it with its confirmation, so the person signs
// in again, and logs that once, naming the person and never the value; the person's next sign-in
// is kept.
func TestPgPeopleStoreReadsARefreshTokenThatDoesNotOpenAsNone(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, "signing-key")
	confirmed := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for name, store := range map[string]func(t *testing.T, email, token string){
		"sealed under another signing key": func(t *testing.T, email, token string) {
			if err := NewPgPeopleStore(database.Pool, "rotated-signing-key").SignIn(ctx, email, token, confirmed); err != nil {
				t.Fatal(err)
			}
		},
		"sealed for another person": func(t *testing.T, email, token string) {
			other := "other-" + email
			if err := people.SignIn(ctx, other, token, confirmed); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Pool.Exec(ctx, `insert into people (email, refresh_token, confirmed_at) select $1, refresh_token, confirmed_at from people where email = $2`, email, other); err != nil {
				t.Fatal(err)
			}
		},
		"stored in plain text": func(t *testing.T, email, token string) {
			if _, err := database.Pool.Exec(ctx, `insert into people (email, refresh_token, confirmed_at) values ($1, $2, $3)`, email, token, confirmed); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			email := "unopened-" + randomDatabaseSuffix(t) + "@d.example"
			token := "refresh-" + randomDatabaseSuffix(t)
			store(t, email, token)
			stored := storedRefreshToken(t, database, email)
			if stored == nil {
				t.Fatal("the row holds no refresh token to read")
			}
			logs := captureLogs(t)
			for read := range 2 {
				membership, found, err := people.Membership(ctx, email)
				if err != nil || !found || membership.RefreshToken != "" || !membership.ConfirmedAt.IsZero() {
					t.Fatalf("read %d: membership = %#v found=%t err=%v, want the person with no refresh token or confirmation", read, membership, found, err)
				}
			}
			if kept := storedRefreshToken(t, database, email); kept != nil {
				t.Errorf("the row still holds %q, want the value forgotten", *kept)
			}
			out := logs.String()
			if count := strings.Count(out, "a stored refresh token did not open"); count != 1 || !strings.Contains(out, email) {
				t.Errorf("logged %d lines naming the unopened token, want one naming %s:\n%s", count, email, out)
			}
			if strings.Contains(out, token) || strings.Contains(out, *stored) {
				t.Errorf("the log carries the stored value:\n%s", out)
			}
			if err := people.SignIn(ctx, email, "refresh-again", confirmed); err != nil {
				t.Fatal(err)
			}
			if membership, _, err := people.Membership(ctx, email); err != nil || membership.RefreshToken != "refresh-again" {
				t.Errorf("membership after signing in again = %#v err=%v, want refresh-again", membership, err)
			}
		})
	}
}

// A read that found a value that does not open forgets it only while the row still holds it: a
// sign-in recorded between the read and the forgetting keeps its own refresh token, and nothing is
// logged for it.
func TestPgPeopleStoreKeepsASignInRecordedAfterTheUnopenedRead(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, "signing-key")
	email := "raced-" + randomDatabaseSuffix(t) + "@d.example"
	if err := people.SignIn(ctx, email, "refresh-new", time.Now()); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	if err := people.forgetUnopened(ctx, email, "v1:the-value-the-read-found", errors.New("did not open")); err != nil {
		t.Fatal(err)
	}
	if membership, _, err := people.Membership(ctx, email); err != nil || membership.RefreshToken != "refresh-new" {
		t.Errorf("membership = %#v err=%v, want the later sign-in's refresh-new", membership, err)
	}
	if logs.Len() != 0 {
		t.Errorf("logged for a row that kept its sign-in:\n%s", logs.String())
	}
}

// Migration 0069 forgets every refresh token stored in plain text before Dispatch sealed them, with
// its confirmation, and keeps the person; a sealed token and a person holding none are left as they
// are, so applying it again changes nothing. Its census, taken before it applies, counts exactly the
// tokens it forgets, so the deploy that carries it is refused for that count alone.
func TestMigration0069ForgetsPlainTextRefreshTokens(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	migrateThrough(t, database, 68)
	people := NewPgPeopleStore(database.Pool, "signing-key")
	confirmed := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `insert into people (email, refresh_token, confirmed_at) values
		('plain-1@d.example', 'eyJjdHkiOiJKV1QiLCJlbmMiOiJBMjU2R0NNIn0.plain-1', $1),
		('plain-2@d.example', 'plain-2', $1)`, confirmed); err != nil {
		t.Fatal(err)
	}
	if err := people.SignIn(ctx, "sealed@d.example", "refresh-sealed", confirmed); err != nil {
		t.Fatal(err)
	}
	if err := people.Record(ctx, "recorded@d.example"); err != nil {
		t.Fatal(err)
	}
	sealed := *storedRefreshToken(t, database, "sealed@d.example")

	report, err := census(ctx, database.Pool.Config().ConnString(), migrationsThrough(t, 69), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := refusals(report); len(report.Pending) != 1 || report.Pending[0].Count == nil || *report.Pending[0].Count != 2 ||
		len(got) != 1 || !strings.Contains(got[0], "0069_people_clear_plain_refresh_tokens.up.sql: its census counts 2") {
		t.Fatalf("census before 0069, refusals %v:\n%s\nwant 0069's census alone, counting the 2 plain-text tokens", got, reportText(report))
	}

	type row struct {
		Email        string
		RefreshToken *string
		ConfirmedAt  *time.Time
	}
	rows := func() []row {
		t.Helper()
		result, err := database.Pool.Query(ctx, `select email, refresh_token, confirmed_at from people order by email`)
		if err != nil {
			t.Fatal(err)
		}
		defer result.Close()
		var out []row
		for result.Next() {
			var r row
			if err := result.Scan(&r.Email, &r.RefreshToken, &r.ConfirmedAt); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		if err := result.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	migrateThrough(t, database, 69)
	after := rows()
	sealedAt := confirmed
	want := []row{
		{Email: "plain-1@d.example"},
		{Email: "plain-2@d.example"},
		{Email: "recorded@d.example"},
		{Email: "sealed@d.example", RefreshToken: &sealed, ConfirmedAt: &sealedAt},
	}
	if len(after) != len(want) {
		t.Fatalf("people after 0069 = %+v, want %+v", after, want)
	}
	for i := range want {
		if after[i].Email != want[i].Email || !reflect.DeepEqual(after[i].RefreshToken, want[i].RefreshToken) ||
			(after[i].ConfirmedAt == nil) != (want[i].ConfirmedAt == nil) || (after[i].ConfirmedAt != nil && !after[i].ConfirmedAt.Equal(*want[i].ConfirmedAt)) {
			t.Errorf("person %d after 0069 = %+v, want %+v", i, after[i], want[i])
		}
	}
	if membership, _, err := people.Membership(ctx, "sealed@d.example"); err != nil || membership.RefreshToken != "refresh-sealed" {
		t.Errorf("the sealed sign-in after 0069 = %#v err=%v, want refresh-sealed", membership, err)
	}

	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Version == 69 {
			if _, err := database.Pool.Exec(ctx, migration.SQL); err != nil {
				t.Fatalf("apply 0069 again: %v", err)
			}
		}
	}
	if again := rows(); !reflect.DeepEqual(again, after) {
		t.Errorf("people after applying 0069 again = %+v, want unchanged %+v", again, after)
	}
}
