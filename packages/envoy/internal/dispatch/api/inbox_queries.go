package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every read behind an ask card's initial data: the orchestrator, its reply chain, and the
// per-ask edits, followers and reach (claimant and route). An Inbox card and an issue's ask list
// render from the same reads, each batched over every ask on the page.
// `inboxAskReplyChainsQuery` stays in comment_queries.go with `replyChainQuery`, the other
// recursion over `comments`, so the two can be read against each other; query_plan_test.go pins
// both from there.

func (s *server) loadInboxAskEdits(ctx context.Context, q queryer, askIDs []string) (map[string][]model.AskEdit, error) {
	edits := make(map[string][]model.AskEdit, len(askIDs))
	for _, askID := range askIDs {
		edits[askID] = []model.AskEdit{}
	}
	rows, err := q.Query(ctx, `
		select payload->>'id', payload->'previous', payload->'edited_by', created_at
		from events
		where type = 'ask.edited' and payload->>'id' = any($1::text[])
		order by payload->>'id', id
	`, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load inbox ask edits: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var askID string
		var previous, editedBy []byte
		var at time.Time
		if err := rows.Scan(&askID, &previous, &editedBy, &at); err != nil {
			return nil, fmt.Errorf("scan inbox ask edit: %w", err)
		}
		var edit model.AskEdit
		if err := json.Unmarshal(previous, &edit.Previous); err != nil {
			return nil, fmt.Errorf("decode inbox ask edit previous: %w", err)
		}
		if err := json.Unmarshal(editedBy, &edit.EditedBy); err != nil {
			return nil, fmt.Errorf("decode inbox ask edit editor: %w", err)
		}
		edit.At = timestampValue(at)
		edits[askID] = append(edits[askID], edit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load inbox ask edits: %w", err)
	}
	return edits, nil
}

func (s *server) loadInboxAskFollowers(ctx context.Context, q queryer, askIDs []string) (map[string][]model.AskFollower, error) {
	followers := make(map[string][]model.AskFollower, len(askIDs))
	for _, askID := range askIDs {
		followers[askID] = []model.AskFollower{}
	}
	rows, err := q.Query(ctx, `
		select ask_id::text, session_id, since
		from ask_followers
		where ask_id = any($1::uuid[])
		order by ask_id, since, session_id
	`, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load inbox ask followers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var askID string
		var follower model.AskFollower
		if err := rows.Scan(&askID, &follower.SessionID, &follower.Since); err != nil {
			return nil, fmt.Errorf("scan inbox ask follower: %w", err)
		}
		followers[askID] = append(followers[askID], follower)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load inbox ask followers: %w", err)
	}
	return followers, nil
}

// loadAskThreads batches the initial data each ask card renders, for the Inbox and for an
// issue's ask list. Both write paths normalise a reply under an ask to that ask's ask_id, so the
// recursive reply_to walk only picks up rows a pre-0043 server wrote or is still draining; it
// matches loadReplyChain exactly either way. Answers are not among the reads: an Inbox row is an
// open ask and can have none, so a caller that does want them (an issue's ask list asked for a
// thread) merges loadAskAnswersByAsk over the same ask ids itself rather than making every
// Inbox load scan `events` for answers that cannot exist.
func (s *server) loadAskThreads(ctx context.Context, q queryer, askIDs []string) (map[string]askThread, error) {
	threads := make(map[string]askThread, len(askIDs))
	for _, askID := range askIDs {
		threads[askID] = askThread{
			Replies:   []model.Comment{},
			Edits:     []model.AskEdit{},
			Answers:   []model.AskAnswer{},
			Followers: []model.AskFollower{},
		}
	}
	if len(askIDs) == 0 {
		return threads, nil
	}

	replies, err := s.loadInboxAskReplyChains(ctx, q, askIDs)
	if err != nil {
		return nil, err
	}
	edits, err := s.loadInboxAskEdits(ctx, q, askIDs)
	if err != nil {
		return nil, err
	}
	followers, err := s.loadInboxAskFollowers(ctx, q, askIDs)
	if err != nil {
		return nil, err
	}
	reaches, err := loadAskReaches(ctx, q, askIDs)
	if err != nil {
		return nil, err
	}
	for askID, thread := range threads {
		thread.Replies = replies[askID]
		thread.Edits = edits[askID]
		thread.Followers = followers[askID]
		thread.askReach = reaches[askID]
		threads[askID] = thread
	}
	return threads, nil
}

// askReachColumns select an askReach for the ask aliased a from the issue askReachJoin attaches
// as ri: the claimant only while a session holds the claim (the same rule the outbox's
// loadSessionClaimant reads), and the route; an empty id or route is none. A project document's
// ask joins no issue, so both are null.
const askReachColumns = `case when ri.claimed_by->>'kind' = 'session'
		then nullif(ri.claimed_by->>'id', '') end,
	nullif(ri.route, '')`

const askReachJoin = `
		left join issues ri on ri.key = a.issue_key`

// loadAskReaches reads every listed ask's askReach in one query, keyed by ask id; an ask the
// query does not find keeps the zero askReach, both null.
func loadAskReaches(ctx context.Context, q queryer, askIDs []string) (map[string]askReach, error) {
	reaches := make(map[string]askReach, len(askIDs))
	rows, err := q.Query(ctx, `
		select a.id::text, `+askReachColumns+`
		from asks a`+askReachJoin+`
		where a.id = any($1::uuid[])
	`, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load ask reach: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var askID string
		var reach askReach
		if err := rows.Scan(&askID, &reach.Claimant, &reach.Route); err != nil {
			return nil, fmt.Errorf("scan ask reach: %w", err)
		}
		reaches[askID] = reach
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load ask reach: %w", err)
	}
	return reaches, nil
}

// loadInboxAskReplyChains groups inboxAskReplyChainsQuery's rows by the ask each belongs to,
// preserving the query's order within a thread.
func (s *server) loadInboxAskReplyChains(ctx context.Context, q queryer, askIDs []string) (map[string][]model.Comment, error) {
	replies := make(map[string][]model.Comment, len(askIDs))
	for _, askID := range askIDs {
		replies[askID] = []model.Comment{}
	}
	rows, err := q.Query(ctx, inboxAskReplyChainsQuery, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load inbox ask replies: %w", err)
	}
	defer rows.Close()
	type threadedComment struct {
		askID   string
		comment model.Comment
	}
	comments := []threadedComment{}
	for rows.Next() {
		var askID string
		comment, err := scanComment(rows, &askID)
		if err != nil {
			return nil, fmt.Errorf("scan inbox ask reply: %w", err)
		}
		comments = append(comments, threadedComment{askID: askID, comment: comment})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load inbox ask replies: %w", err)
	}
	unhydrated := make([]model.Comment, len(comments))
	for index := range comments {
		unhydrated[index] = comments[index].comment
	}
	hydrated, err := s.hydrateCommentSideTables(ctx, q, unhydrated)
	if err != nil {
		return nil, fmt.Errorf("hydrate inbox ask replies: %w", err)
	}
	for index := range hydrated {
		replies[comments[index].askID] = append(replies[comments[index].askID], hydrated[index])
	}
	return replies, nil
}
