package store

import (
	"context"
	"testing"
	"time"
)

func TestPgPeopleStoreKeepsEachPersonsMembership(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool)
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
	people := NewPgPeopleStore(database.Pool)
	for _, email := range []string{"Sami@d.example", ""} {
		if err := people.Record(ctx, email); err == nil {
			t.Errorf("Record(%q) was accepted", email)
		}
	}
}
