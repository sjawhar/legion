// Package asks holds the ask_followers relation shared by every path that writes an
// ask: the API handlers, document ask blocks, and the outbox that routes to followers.
// It sits below api and docs (docs cannot import api) so no insert site can miss it.
package asks

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Execer is the pgx surface shared by *pgxpool.Pool and pgx.Tx for writes.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Queryer is the pgx surface shared by *pgxpool.Pool and pgx.Tx for reads.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Follow records that sessionID follows askID and reports whether that is new;
// following twice changes nothing.
func Follow(ctx context.Context, db Execer, askID, sessionID string) (bool, error) {
	tag, err := db.Exec(ctx, `
		insert into ask_followers (ask_id, session_id)
		values ($1, $2)
		on conflict do nothing
	`, askID, sessionID)
	if err != nil {
		return false, fmt.Errorf("follow ask %q as %q: %w", askID, sessionID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// FollowAuthor follows askID for a session author and does nothing for a human, so
// every insert site can call it with the actor it already has.
func FollowAuthor(ctx context.Context, db Execer, askID string, author model.Actor) error {
	if author.Kind != "session" || author.ID == "" {
		return nil
	}
	_, err := Follow(ctx, db, askID, author.ID)
	return err
}

// Unfollow removes sessionID from askID's followers and reports whether a row went.
func Unfollow(ctx context.Context, db Execer, askID, sessionID string) (bool, error) {
	tag, err := db.Exec(ctx, `
		delete from ask_followers where ask_id = $1 and session_id = $2
	`, askID, sessionID)
	if err != nil {
		return false, fmt.Errorf("unfollow ask %q as %q: %w", askID, sessionID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Followers lists askID's followers in the order they joined.
func Followers(ctx context.Context, db Queryer, askID string) ([]model.AskFollower, error) {
	rows, err := db.Query(ctx, `
		select session_id, since from ask_followers
		where ask_id = $1
		order by since, session_id
	`, askID)
	if err != nil {
		return nil, fmt.Errorf("load followers of ask %q: %w", askID, err)
	}
	defer rows.Close()
	followers := []model.AskFollower{}
	for rows.Next() {
		var follower model.AskFollower
		if err := rows.Scan(&follower.SessionID, &follower.Since); err != nil {
			return nil, fmt.Errorf("scan follower of ask %q: %w", askID, err)
		}
		followers = append(followers, follower)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate followers of ask %q: %w", askID, err)
	}
	return followers, nil
}
