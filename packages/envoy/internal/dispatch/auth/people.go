package auth

import (
	"context"
	"time"
)

// PersonMembership is what Dispatch holds to confirm that a person signed in through the sign-in
// pool may still use it: the pool's refresh token for them and when their membership was last
// confirmed. A person recorded any other way (the trusted identity header, the local dev sign-in,
// the people migration), or whose sign-in has ended, holds neither: an empty RefreshToken and a
// zero ConfirmedAt.
type PersonMembership struct {
	RefreshToken string
	ConfirmedAt  time.Time
}

// PeopleStore persists the people who have signed in to Dispatch, keyed by lowercase email: the
// assignee picker's options, and each pool sign-in's membership.
type PeopleStore interface {
	// Record notes that email has signed in, keeping any membership it already holds.
	Record(ctx context.Context, email string) error
	// SignIn records a sign-in through the pool: its refresh token, confirmed at confirmedAt.
	SignIn(ctx context.Context, email, refreshToken string, confirmedAt time.Time) error
	// Membership reads email's membership; found is false for a person who never signed in.
	Membership(ctx context.Context, email string) (membership PersonMembership, found bool, err error)
	// Confirm records a refresh that confirmed email's membership, with the refresh token to use next.
	Confirm(ctx context.Context, email, refreshToken string, confirmedAt time.Time) error
	// End forgets email's refresh token and confirmation, keeping the person.
	End(ctx context.Context, email string) error
}
