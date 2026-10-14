package auth

import (
	"context"
	"time"
)

// PersonMembership is what Dispatch holds to confirm that a person signed in through the sign-in
// issuer may still use it: the issuer's refresh token for them and when their membership was last
// confirmed. A person recorded any other way (the local/test identity header, the local dev
// sign-in, the people migration), whose sign-in has ended, or whose stored refresh token does not
// open (store.PgPeopleStore keeps it sealed under the signing key), holds neither: an empty
// RefreshToken and a zero ConfirmedAt. Subject is the Google account (`sub`) that last signed in
// to the address with Google for a Workspace domain; empty for an address no such sign-in
// recorded. Ending a sign-in keeps it.
type PersonMembership struct {
	RefreshToken string
	ConfirmedAt  time.Time
	Subject      string
}

// PeopleStore persists the people who have signed in to Dispatch, keyed by lowercase email: the
// assignee picker's options, and each sign-in's membership.
type PeopleStore interface {
	// Record notes that email has signed in, keeping any membership it already holds.
	Record(ctx context.Context, email string) error
	// SignIn records a sign-in through the issuer: its refresh token, confirmed at confirmedAt.
	// subject is the Google account's `sub` of a sign-in for a Workspace domain, and empty for a
	// Cognito pool's, which neither reads nor writes the stored subject. When email already holds
	// another subject, the sign-in ends every session of email (SessionStore.RevokeSessions) before
	// it records subject, so a revoke that fails leaves the old subject for the next sign-in to
	// revoke again.
	SignIn(ctx context.Context, email, subject, refreshToken string, confirmedAt time.Time) error
	// Membership reads email's membership; found is false for a person who never signed in.
	Membership(ctx context.Context, email string) (membership PersonMembership, found bool, err error)
	// Confirm records a refresh that confirmed email's membership, with the refresh token to use next.
	Confirm(ctx context.Context, email, refreshToken string, confirmedAt time.Time) error
	// End forgets email's refresh token and confirmation, keeping the person and their subject.
	End(ctx context.Context, email string) error
}
