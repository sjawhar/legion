package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// itemBodyBytes is roughly how much text a comment, a message or an ask carries. It is the size
// of the items the production tables hold, not a figure the spec gives: what the measurements
// read of an item is its tsvector and its first block, both of which this size exercises.
const itemBodyBytes = 600

// writeAsks writes the asks with their followers and their mentions. An ask has no blocks, so its
// mentions are rows of `refs` alone: that is what makes the 100,000 rows of `refs` twice the
// 50,000 block mentions spread over the three block tables.
func (r *seedRun) writeAsks(ctx context.Context) error {
	return r.inBatches(ctx, "asks", len(r.plan.asks), batchRows, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		mentions := newMentionRows("ask")
		followers := make([][]any, 0, to-from)
		for index := from; index < to; index++ {
			ask := r.plan.asks[index]
			mention := ""
			if nth := share(index, len(r.plan.asks), r.sizes.AskMentions); nth > 0 {
				mention = r.plan.mentionText(index%len(mentionKinds), index)
			}
			question := r.plan.askQuestion(r.random, mention, itemBodyBytes)
			var issueKey, anchor, blockID *string
			var artifactID, blockArtifact *string
			if ask.Issue >= 0 {
				issueKey = &r.plan.issues[ask.Issue].Key
			}
			if ask.Document >= 0 {
				artifactID = &r.plan.documents[ask.Document].ID
			}
			if ask.Anchored {
				document := r.plan.documents[ask.Document]
				block := firstParagraphBlock
				if ask.TableRow {
					block = r.tableRow
				}
				value := anchorJSON(document.ID, block, document.Versions, r.plan.vocabulary.phrase(r.random, 4))
				anchor = &value
			}
			if ask.BlockDoc >= 0 {
				blockArtifact = &r.plan.documents[ask.BlockDoc].ID
				blockID = &r.plan.asks[index].BlockID
			}
			var answer, resolution any
			switch ask.State {
			case "answered":
				answer = fmt.Sprintf(`{"text":%q,"by":%s}`, r.plan.vocabulary.phrase(r.random, 6), seedActor)
			case "resolved":
				resolution = fmt.Sprintf(`{"reason":%q,"by":%s}`, r.plan.vocabulary.phrase(r.random, 4), seedActor)
			}
			if _, err := tx.Exec(ctx, `
				insert into asks (id, issue_key, artifact_id, author, question, options, multiple, urgency,
				                  anchor, state, answer, resolution, created_at, kind, block_id, block_artifact_id)
				values ($1, $2, $3, $4::jsonb, $5, '[]'::jsonb, false, 'med', $6::jsonb, $7, $8::jsonb, $9::jsonb, $10, 'question', $11, $12)
			`, ask.ID, issueKey, artifactID, seedActor, question, anchor, ask.State, answer, resolution,
				ask.CreatedAt, blockID, blockArtifact); err != nil {
				return fmt.Errorf("ask %s: %w", ask.ID, err)
			}
			mentions.add(ask.ID, question, ask.CreatedAt)
			if index < r.sizes.AskFollowers {
				followers = append(followers, []any{ask.ID, ask.Follower, ask.CreatedAt})
			}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"ask_followers"},
			[]string{"ask_id", "session_id", "since"}, pgx.CopyFromRows(followers)); err != nil {
			return fmt.Errorf("ask followers: %w", err)
		}
		return mentions.write(ctx, tx)
	})
}

// writeComments writes the comments, indexes each one's blocks with the server's own indexer
// (refs.IndexItem) and writes the comment's own mentions. An edited comment is indexed twice, as
// its write and its edit indexed it: the blocks the edit dropped keep their rows with a null
// position.
func (r *seedRun) writeComments(ctx context.Context) error {
	return r.inBatches(ctx, "comments", len(r.plan.comments), batchItems, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		mentions := newMentionRows("comment")
		for index := from; index < to; index++ {
			comment := r.plan.comments[index]
			mention := ""
			if comment.MentionTo >= 0 {
				mention = r.plan.mentionText(comment.MentionTo, comment.MentionNth)
			}
			body := r.plan.itemBody(r.random, comment.Blocks, mention, itemBodyBytes)
			var issueKey, artifactID, anchor, repliesTo, askID *string
			var editedAt any
			if comment.Issue >= 0 {
				issueKey = &r.plan.issues[comment.Issue].Key
			}
			if comment.Document >= 0 {
				artifactID = &r.plan.documents[comment.Document].ID
			}
			if comment.Anchored {
				document := r.plan.documents[comment.Document]
				block := firstParagraphBlock
				if comment.TableRow {
					block = r.tableRow
				}
				value := anchorJSON(document.ID, block, document.Versions, r.plan.vocabulary.phrase(r.random, 4))
				anchor = &value
			}
			if comment.RepliesTo >= 0 {
				node := "comment/" + r.plan.comments[comment.RepliesTo].ID + "#b1"
				repliesTo = &node
			}
			if comment.Ask >= 0 {
				askID = &r.plan.asks[comment.Ask].ID
			}
			var replyTo *string
			if comment.ReplyTo >= 0 {
				replyTo = &r.plan.comments[comment.ReplyTo].ID
			}
			if comment.Edited {
				editedAt = comment.CreatedAt.Add(time.Hour)
			}
			if _, err := tx.Exec(ctx, `
				insert into comments (id, issue_key, artifact_id, author, body, anchor, reply_to, resolved,
				                      created_at, ask_id, replies_to_block, edited_at)
				values ($1, $2, $3, $4::jsonb, $5, $6::jsonb, $7, $8, $9, $10, $11, $12)
			`, comment.ID, issueKey, artifactID, seedActor, body, anchor, replyTo, index%5 == 0,
				comment.CreatedAt, askID, repliesTo, editedAt); err != nil {
				return fmt.Errorf("comment %s: %w", comment.ID, err)
			}
			if comment.Edited {
				// The body before the edit held one block more. Indexing it first and the
				// stored body second is the order the write and the edit ran in, and it leaves
				// the dropped block with a null position.
				longer := body + "\n\n" + r.plan.vocabulary.phrase(r.random, 12) + "."
				if _, err := refs.IndexItem(ctx, tx, refs.ItemComment, comment.ID, longer,
					comment.CreatedAt, text.ExtractAt(longer, serverURL), nil); err != nil {
					return fmt.Errorf("index comment %s before its edit: %w", comment.ID, err)
				}
			}
			at := comment.CreatedAt
			if comment.Edited {
				at = comment.CreatedAt.Add(time.Hour)
			}
			if _, err := refs.IndexItem(ctx, tx, refs.ItemComment, comment.ID, body, at,
				text.ExtractAt(body, serverURL), nil); err != nil {
				return fmt.Errorf("index comment %s: %w", comment.ID, err)
			}
			mentions.add(comment.ID, body, comment.CreatedAt)
		}
		return mentions.write(ctx, tx)
	})
}

// writeMessages writes the issue threads and indexes each message's blocks. Every fiftieth
// message is indexed twice, the first time from a body one block longer: that is a reindex under
// a parser reading the same text as fewer blocks, which leaves a message block with a null
// position - the state the view's filter on message_blocks is written for.
func (r *seedRun) writeMessages(ctx context.Context) error {
	return r.inBatches(ctx, "messages", len(r.plan.messages), batchItems, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		mentions := newMentionRows("message")
		for index := from; index < to; index++ {
			message := r.plan.messages[index]
			mention := ""
			if message.MentionTo >= 0 {
				mention = r.plan.mentionText(message.MentionTo, message.MentionNth)
			}
			body := r.plan.itemBody(r.random, message.Blocks, mention, itemBodyBytes)
			var inReplyTo, repliesTo *string
			if message.InReplyTo >= 0 {
				inReplyTo = &r.plan.messages[message.InReplyTo].ID
			}
			if message.RepliesTo >= 0 {
				node := "message/" + r.plan.messages[message.RepliesTo].ID + "#b1"
				repliesTo = &node
			}
			if _, err := tx.Exec(ctx, `
				insert into messages (id, issue_key, author, body, created_at, in_reply_to, replies_to_block)
				values ($1, $2, $3::jsonb, $4, $5, $6, $7)
			`, message.ID, r.plan.issues[message.Issue].Key, seedActor, body, message.CreatedAt,
				inReplyTo, repliesTo); err != nil {
				return fmt.Errorf("message %s: %w", message.ID, err)
			}
			if message.Reindexed {
				longer := body + "\n\n" + r.plan.vocabulary.phrase(r.random, 12) + "."
				if _, err := refs.IndexItem(ctx, tx, refs.ItemMessage, message.ID, longer,
					message.CreatedAt, text.ExtractAt(longer, serverURL), nil); err != nil {
					return fmt.Errorf("index message %s under the older parser: %w", message.ID, err)
				}
			}
			if _, err := refs.IndexItem(ctx, tx, refs.ItemMessage, message.ID, body,
				message.CreatedAt, text.ExtractAt(body, serverURL), nil); err != nil {
				return fmt.Errorf("index message %s: %w", message.ID, err)
			}
			mentions.add(message.ID, body, message.CreatedAt)
		}
		if err := mentions.write(ctx, tx); err != nil {
			return err
		}
		return r.writeSessionMention(ctx, tx, from, to)
	})
}

// writeSessionMention writes the one mention whose target is a session. `session` is a node kind
// refs/graph.go's nodeLoaders resolves, but text/refs.go's grammar names no session, so no body
// can write this row: a build that addressed sessions would write it, and the backlink count
// reads `refs` for every kind the loaders name, so the data set holds one.
func (r *seedRun) writeSessionMention(ctx context.Context, tx pgx.Tx, from, to int) error {
	if from > 0 || to == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, created_at)
		values ('message', $1, 'session', $2, $3)
		on conflict do nothing
	`, r.plan.messages[0].ID, r.plan.sessions[0], r.plan.messages[0].CreatedAt)
	return err
}

// mentionRows collects the `refs` rows one batch of items writes. The targets are read out of
// each body by text.ExtractAt and refs.TargetOf, the same reading the server's own indexer makes,
// so the rows are the rows a write of that body would leave.
type mentionRows struct {
	kind                string
	ids, kinds, targets []string
	times               []time.Time
	seen                map[[3]string]struct{}
}

func newMentionRows(kind string) *mentionRows {
	return &mentionRows{kind: kind, seen: map[[3]string]struct{}{}}
}

func (m *mentionRows) add(id, body string, at time.Time) {
	for _, ref := range text.Extract(body, serverURL) {
		target, ok := refs.TargetOf(ref)
		if !ok {
			continue
		}
		key := [3]string{id, target.Kind, target.ID}
		if _, held := m.seen[key]; held {
			continue
		}
		m.seen[key] = struct{}{}
		m.ids = append(m.ids, id)
		m.kinds = append(m.kinds, target.Kind)
		m.targets = append(m.targets, target.ID)
		m.times = append(m.times, at)
	}
}

func (m *mentionRows) write(ctx context.Context, tx pgx.Tx) error {
	if len(m.ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, created_at)
		select $1, r.id, r.kind, r.target, r.at
		from unnest($2::text[], $3::text[], $4::text[], $5::timestamptz[]) as r (id, kind, target, at)
		on conflict (from_kind, from_id, to_kind, to_id) do nothing
	`, m.kind, m.ids, m.kinds, m.targets, m.times)
	if err != nil {
		return fmt.Errorf("write %s mentions: %w", m.kind, err)
	}
	return nil
}
