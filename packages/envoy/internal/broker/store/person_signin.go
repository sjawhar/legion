package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// A person's sign-ins are rows of 0025's person_signins, one per box key: the person, the RFC
// 7638 thumbprint of the key `agent-secrets login` made on that machine, the Google account of
// the machine, and the times the row was made, last used and revoked.
//
// The exchange writes through RecordPersonSignIn in its own transaction, so the row and the
// access token the trade mints land together or not at all; the routes Dispatch relays read and
// revoke through the two methods below, on the pool.
//
// A revoked row is never deleted and never re-used: it is what refuses that box key for good, so
// a person who revokes a sign-in is not undone by the box trading the very key again. A person
// signing in on a second box is a second thumbprint and a row of its own, which is why revoking
// one leaves the others working.
//
// The three vocabularies - a lowercase login, an RFC 7638 thumbprint and a lowercase Google
// account - are closed by the migration's own check constraints, which are the one place they
// are written down, so nothing a caller spells reaches a person's own list through this table.

// ErrSignInRevoked is a trade whose person has revoked the box key it proved. It is the one
// refusal RecordPersonSignIn has.
var ErrSignInRevoked = errors.New("store: the person has revoked this machine's sign-in")

// ErrNoSignIn is a sign-in id that is not that person's, or is no sign-in at all.
var ErrNoSignIn = errors.New("store: no sign-in of this person has that id")

// PersonSignIn is one live person_signins row as a person reads it on their own list.
type PersonSignIn struct {
	// ID is the row's id, which the revoke route takes.
	ID string
	// Person is the login the sign-in was made as.
	Person string
	// Thumbprint is the RFC 7638 thumbprint of the box key the sign-in is bound to.
	Thumbprint string
	// MachineAccount is the Google account of the machine the sign-in was made on, as
	// BROKER_VM_SERVICES lists it.
	MachineAccount string
	// CreatedAt is when the sign-in was first recorded, and LastUsedAt when its box key last
	// traded a token.
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// RecordPersonSignIn records, in tx, that person signed in on the machine machineAccount names
// with the box key thumbprint, and answers ErrSignInRevoked when that person has revoked that
// key.
//
// The first trade of a person and a box key inserts the row; every later trade of that pair is
// the same sign-in, so it moves last_used_at and the machine's account and nothing else. It
// takes tx rather than the pool because the exchange must write this and the access token it is
// minting together: a revoke that commits first refuses the trade, and one that commits after it
// ends the token the trade minted (RevokePersonSignIn).
//
// It is one statement, so a revoke cannot land between the read of the revoked rows and the
// write of the live one: Postgres runs the data-modifying branch to completion whether or not
// the query reads its output, and the insert's own conflict is what two trades racing for one
// pair resolve on.
func RecordPersonSignIn(ctx context.Context, tx pgx.Tx, person, thumbprint, machineAccount string) error {
	var revoked bool
	err := tx.QueryRow(ctx, `with ended as (
			select 1 from person_signins
			where person = lower($1) and thumbprint = $2 and revoked_at is not null
		), recorded as (
			insert into person_signins (person, thumbprint, machine_account)
			select lower($1), $2, lower($3) where not exists (select 1 from ended)
			on conflict (person, thumbprint) where revoked_at is null
			do update set last_used_at = now(), machine_account = excluded.machine_account
			returning 1
		)
		select exists (select 1 from ended)`, person, thumbprint, machineAccount).Scan(&revoked)
	if err != nil {
		return err
	}
	if revoked {
		return ErrSignInRevoked
	}
	return nil
}

// PersonSignIns lists the live sign-ins of person, newest first: one per machine the person has
// signed in on and not revoked. A person with none has an empty list and no error.
func (s *Store) PersonSignIns(ctx context.Context, person string) ([]PersonSignIn, error) {
	rows, err := s.Pool.Query(ctx, `select id::text, person, thumbprint, machine_account, created_at, last_used_at
		from person_signins where person = lower($1) and revoked_at is null order by created_at desc, id`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonSignIn
	for rows.Next() {
		var in PersonSignIn
		if err := rows.Scan(&in.ID, &in.Person, &in.Thumbprint, &in.MachineAccount, &in.CreatedAt, &in.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// RevokePersonSignIn ends sign-in id of person and answers how many of that person's live access
// tokens it ended with it. A row already revoked is revoked again by nobody: it answers no error
// and no tokens, as revoking a machine login twice answers the same. An id no sign-in of that
// person has is ErrNoSignIn, so one person cannot learn another's ids by trying them.
//
// It is one statement, and that is the point. Revoking the sign-in alone would leave every token
// already minted under it writing for up to BROKER_ACCESS_TOKEN_SECONDS, and a resource server's
// affirmative introspection answer for as long again, so the tokens are ended in the revoke's own
// transaction, exactly as revoking a machine login ends the tokens its credential holds
// (oauthstore.EndCredential). Which tokens those are is written in each row's stored session: the
// person it names and the key it is bound to, which together are this sign-in and no other, so
// the person's sign-ins on their other boxes keep every token they hold. Only access tokens are
// ended, because an exchanged trade is answered no refresh token at all.
func (s *Store) RevokePersonSignIn(ctx context.Context, id, person string) (int, error) {
	var found, ended int
	err := s.Pool.QueryRow(ctx, `with target as (
			select id from person_signins where id = $1 and person = lower($2) for no key update
		), revoked as (
			update person_signins set revoked_at = now()
			where id in (select id from target) and revoked_at is null
			returning person, thumbprint
		), tokens as (
			update oauth_access_tokens set active = false
			where active and (request #>> '{session,extra,person}', request #>> '{session,jwk_thumbprint}')
				in (select person, thumbprint from revoked)
			returning 1
		)
		select (select count(*) from target), (select count(*) from tokens)`, id, person).Scan(&found, &ended)
	if err != nil {
		return 0, err
	}
	if found == 0 {
		return 0, ErrNoSignIn
	}
	return ended, nil
}
