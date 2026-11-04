package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// itemBodyBytes is roughly how much text a comment, a message or an ask carries. It is the size
// of the items the production tables hold, not a figure the spec gives: what the measurements
// read of an item is its tsvector and its first block, both of which this size exercises.
const itemBodyBytes = 600

// askQuestionBlock is the part id of an ask's question, the part a reply under an ask names. The
// question is the first part of every ask (refs.AskParts), and a part the index has not seen
// before takes the next id of the ask's state row, counted from one (itemblocks.Align), so b1 is
// the question of every ask the seed writes, as it is the first block of every comment and
// message it writes.
const askQuestionBlock = "b1"

// writeAsks writes the asks with their options, their followers, their mentions, the parts the
// ask is indexed into and the `ask.opened` event each one is read through.
//
// The parts - the question, each option and the answer the ask holds - are recorded with the
// server's own indexer (refs.PlanAskParts), in the ask's own transaction, as every route that
// writes an ask records them (api/references.go's queueAskParts). The insert owes that index
// (migration 20261101143212's asks_item_blocks_owed trigger), so an ask the seed left unindexed
// is work a started server would do for itself, rewriting rows a measurement is being read from.
// A mention the question carries is then a row of `refs`, the ask's own, and a row of
// `ask_block_refs`, held by the part the text is in.
//
// The opening event is written in the ask's own transaction, as the create route writes it
// (api/asks.go), because its payload carries the question the row was written with. Every read
// of an ask resolves that event (events.OpenedEventIDs, through api.attachOpenedEventIDs) and
// fails the whole request when the ask has none, so an ask written without one answers 500 on
// every route that lists or reads it.
func (r *seedRun) writeAsks(ctx context.Context) error {
	seqs := r.askEventSeqs()
	// An ask costs a part index as a comment costs a block index, so the asks go in the smaller
	// batches the indexed items are written in.
	return r.inBatches(ctx, "asks", len(r.plan.asks), batchItems, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		mentions := newMentionRows("ask")
		followers := make([][]any, 0, to-from)
		opened := make([][]any, 0, to-from)
		for index := from; index < to; index++ {
			ask := r.plan.asks[index]
			mention := ""
			if nth := share(index, len(r.plan.asks), r.sizes.AskMentions); nth > 0 {
				mention = r.plan.mentionText(index%len(mentionKinds), index)
			}
			question := r.plan.askQuestion(r.random, mention, itemBodyBytes)
			options := r.plan.askOptions(r.random, ask.Options)
			var issueKey, anchor, blockID *string
			var artifactID, blockArtifact *string
			if ask.Issue >= 0 {
				issueKey = &r.plan.issues[ask.Issue].Key
			}
			if ask.Document >= 0 {
				artifactID = &r.plan.documents[ask.Document].ID
			}
			var anchorValue *model.Anchor
			if ask.Anchored {
				document := r.plan.documents[ask.Document]
				block := firstParagraphBlock
				if ask.TableRow {
					block = r.tableRow
				}
				value := anchorJSON(document.ID, block, document.Versions, r.plan.vocabulary.phrase(r.random, 4))
				anchor = &value
				// The payload carries the anchor the row holds, read back with the server's own
				// reader of a stored anchor, so the two cannot describe different marks.
				anchorValue = &model.Anchor{}
				if err := model.DecodeAnchor([]byte(value), anchorValue); err != nil {
					return fmt.Errorf("ask %s: decode the anchor it is written with: %w", ask.ID, err)
				}
			}
			if ask.BlockDoc >= 0 {
				blockArtifact = &r.plan.documents[ask.BlockDoc].ID
				blockID = &r.plan.asks[index].BlockID
			}
			var answer *model.AskAnswer
			var resolution any
			switch ask.State {
			case "answered":
				given := r.askAnswer(ask, options)
				answer = &given
			case "resolved":
				resolution = fmt.Sprintf(`{"reason":%q,"by":%s}`, r.plan.vocabulary.phrase(r.random, 4), seedActor)
			}
			optionsJSON, err := json.Marshal(options)
			if err != nil {
				return fmt.Errorf("ask %s: encode its options: %w", ask.ID, err)
			}
			var answerJSON any
			if answer != nil {
				encoded, err := json.Marshal(answer)
				if err != nil {
					return fmt.Errorf("ask %s: encode its answer: %w", ask.ID, err)
				}
				answerJSON = string(encoded)
			}
			if _, err := tx.Exec(ctx, `
				insert into asks (id, issue_key, artifact_id, author, question, options, multiple, urgency,
				                  anchor, state, answer, resolution, created_at, kind, block_id, block_artifact_id)
				values ($1, $2, $3, $4::jsonb, $5, $6::jsonb, false, 'med', $7::jsonb, $8, $9::jsonb, $10::jsonb, $11, 'question', $12, $13)
			`, ask.ID, issueKey, artifactID, seedActor, question, string(optionsJSON), anchor, ask.State,
				answerJSON, resolution, ask.CreatedAt, blockID, blockArtifact); err != nil {
				return fmt.Errorf("ask %s: %w", ask.ID, err)
			}
			mentions.add(ask.ID, text.Refs(text.ExtractAt(question, serverURL)), ask.CreatedAt)
			if index < r.sizes.AskFollowers {
				followers = append(followers, []any{ask.ID, ask.Follower, ask.CreatedAt})
			}
			opening := model.Ask{
				ID:              ask.ID,
				IssueKey:        issueKey,
				ArtifactID:      artifactID,
				BlockID:         blockID,
				BlockArtifactID: blockArtifact,
				Author:          seedAuthor,
				Kind:            "question",
				Question:        question,
				Options:         options,
				Urgency:         "med",
				Anchor:          anchorValue,
				State:           "open",
				CreatedAt:       ask.CreatedAt,
			}
			payload, err := askOpenedPayload(opening)
			if err != nil {
				return err
			}
			// The parts are read from the ask as its row stands, answer and all, not from the ask
			// as it opened: the index records what a reader of the row finds now. The time is the
			// ask's own, which is the time its row was last written (coalesce(edited_at,
			// created_at)), so a re-index of it would date its parts as this one did.
			stored := opening
			stored.State, stored.Answer = ask.State, answer
			// The seed's transaction joins no write phase of anyone's, so it reads the index the
			// ask holds - none, its row having just been inserted - plans the parts against it
			// and runs the plan's statements at once, as a batch of their own, the way a caller
			// outside the document service indexes an item it wrote (refs.ReindexOwedItem).
			partIndex, err := refs.ReadItemIndex(ctx, tx, refs.ItemAsk, ask.ID)
			if err != nil {
				return fmt.Errorf("index the parts of ask %s: %w", ask.ID, err)
			}
			parts, err := refs.PlanAskParts(partIndex, stored, serverURL, ask.CreatedAt)
			if err != nil {
				return err
			}
			if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				parts.Queue(b)
				return nil
			}); err != nil {
				return fmt.Errorf("index the parts of ask %s: %w", ask.ID, err)
			}
			// The event is owned by the ask's own owner and dated at the ask, as one appended
			// with the ask is, and it is published (`notify` false, `published_at` set) like
			// every other event the seed writes.
			opened = append(opened, []any{
				r.askEventIDBase() + int64(index) + 1, issueKey, artifactID, nil, seqs[index],
				"ask.opened", seedActor, payload, false, ask.CreatedAt, ask.CreatedAt,
			})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"ask_followers"},
			[]string{"ask_id", "session_id", "since"}, pgx.CopyFromRows(followers)); err != nil {
			return fmt.Errorf("ask followers: %w", err)
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"events"}, eventColumns, pgx.CopyFromRows(opened)); err != nil {
			return fmt.Errorf("ask opening events: %w", err)
		}
		return mentions.write(ctx, tx)
	})
}

// askAnswer is the answer an answered ask holds, in the shape the answer route writes
// (api/ask_transitions.go): the answerer, the option it chose when the ask offers any, the text
// it was given and its time. The part index reads the stored answer as a model.AskAnswer, takes
// it as a part of its own and records its answerer as that part's maker (refs.askPartMakers), so
// an answer of another shape would index as a part no answer of the server's ever is. Its time is
// the ask's own, the one time every row and event of a seeded ask carries.
func (r *seedRun) askAnswer(ask askPlan, options []model.AskOption) model.AskAnswer {
	selected := []string{}
	if len(options) > 0 {
		selected = []string{options[0].Label}
	}
	given := r.plan.vocabulary.phrase(r.random, 6)
	return model.AskAnswer{User: seedAuthor.ID, Selected: selected, Text: &given, At: ask.CreatedAt}
}

// writeComments writes the comments, indexes each one's blocks with the server's own indexer
// (refs.IndexItem) and writes the comment's own mentions. An edited comment is indexed twice, as
// its write and its edit indexed it: the blocks the edit dropped keep their rows with a null
// position.
//
// A reply names the block it answers in replies_to_block, as the create route writes it
// (api/reply_blocks.go): a block of the comment it replies to, or the part of the ask it answers,
// whose node id starts `ask/`. The view reads the two as edges to a comment_block and to an
// ask_block, telling them apart by that prefix.
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
			if comment.AskPart {
				node := "ask/" + r.plan.asks[comment.Ask].ID + "#" + askQuestionBlock
				repliesTo = &node
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
			located := text.ExtractAt(body, serverURL)
			if _, err := refs.IndexItem(ctx, tx, refs.ItemComment, comment.ID, body, at,
				located, nil); err != nil {
				return fmt.Errorf("index comment %s: %w", comment.ID, err)
			}
			mentions.add(comment.ID, text.Refs(located), comment.CreatedAt)
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
			located := text.ExtractAt(body, serverURL)
			if _, err := refs.IndexItem(ctx, tx, refs.ItemMessage, message.ID, body,
				message.CreatedAt, located, nil); err != nil {
				return fmt.Errorf("index message %s: %w", message.ID, err)
			}
			mentions.add(message.ID, text.Refs(located), message.CreatedAt)
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

// add collects the rows mentioned leaves for the item id, which is text.ExtractAt's reading of
// the item's body read as references: the caller already made that reading for the indexer, so
// the body is scanned once.
func (m *mentionRows) add(id string, mentioned []text.Ref, at time.Time) {
	for _, ref := range mentioned {
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
