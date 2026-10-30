package refs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The type of each part of an ask, its ask_blocks.type and the salt of its identity hash
// (AskParts).
const (
	AskQuestionPart = "question"
	AskOptionPart   = "option"
	AskAnswerPart   = "answer"
)

// askOptionJoin stands between an option's label and its description in the option's part text,
// and askAnswerJoin between the labels an answer selected and the text it was given.
const (
	askOptionJoin = " — "
	askAnswerJoin = " - "
)

// AskParts is the parts of the ask in order: its question, then each option in the ask's row
// order, then its current answer, which it has only while the ask holds one. A part's text is the
// ask row's own text: the question, an option as "<label> — <description>" or its label alone,
// and the answer as askAnswerText renders it.
//
// A part's identity hash, which the ids align on (itemblocks.Align), is salted with the part's
// type: itemblocks.matchKey matches on the hash alone, so an option whose text is the question's
// would otherwise take the question's id. An answer part's hash is salted with the answerer as
// well, since its maker is part of what it is: the same answer from another person is another
// answer, which takes a part of its own (askAnswerPart).
//
// A part is a text of its own, not a span of one body, since an ask has no body to take offsets
// into: every part's From and To are zero, a range no offset falls in (innermostBlocks), and
// nothing reads them. A part's mentions are read from the part's own text and paired to it
// (askPartPairs), where a comment's are placed by their offsets into its body.
func AskParts(ask model.Ask) []itemblocks.Block {
	parts := make([]itemblocks.Block, 0, len(ask.Options)+2)
	parts = append(parts, askPart(AskQuestionPart, ask.Question))
	for _, option := range ask.Options {
		parts = append(parts, askPart(AskOptionPart, askOptionText(option)))
	}
	if ask.Answer != nil {
		parts = append(parts, askAnswerPart(*ask.Answer))
	}
	return parts
}

// AskPartsAt is the time a write that did not change the ask's question or options records its
// parts at: the answer's own time when the ask holds one, since the answer is the part that
// write added, and now otherwise. It is what a transition and a restoration of a retracted ask
// index at, where a create and an edit index at the time their own write of the row carries.
func AskPartsAt(ask model.Ask) time.Time {
	if ask.Answer != nil {
		return ask.Answer.At
	}
	return time.Now().UTC()
}

// askPart is one part of an ask, its identity hash salted with its type.
func askPart(partType, partText string) itemblocks.Block {
	return itemblocks.Block{Type: partType, Text: partText, Hash: itemblocks.Hash(partType + "\n" + partText)}
}

// askAnswerPart is the ask's answer part: its text is the answer alone (askAnswerText), where its
// identity hash is salted with the answerer too, so an answer another person gives is a part of
// its own and no part records a maker who did not make it. The maker the part row keeps is
// askPartMakers'.
func askAnswerPart(answer model.AskAnswer) itemblocks.Block {
	part := askPart(AskAnswerPart, askAnswerText(answer))
	part.Hash = itemblocks.Hash(AskAnswerPart + "\n" + answer.User + "\n" + part.Text)
	return part
}

// askOptionText is an option's part text: its label and its description, or its label alone when
// it has no description.
func askOptionText(option model.AskOption) string {
	if option.Description == "" {
		return option.Label
	}
	return option.Label + askOptionJoin + option.Description
}

// askAnswerText is an answer's part text, the one line an ask.answered event's summary renders it
// as (outbox.askAnswerText): the labels it selected, ", " between them, then " - " and the text
// it was given, with either half alone when the other is empty.
func askAnswerText(answer model.AskAnswer) string {
	selected := strings.Join(answer.Selected, ", ")
	written := ""
	if answer.Text != nil {
		written = *answer.Text
	}
	switch {
	case written == "":
		return selected
	case selected == "":
		return written
	default:
		return selected + askAnswerJoin + written
	}
}

// askPartMakers is the maker each part records in ask_blocks.made_by, in AskParts' order: null
// for the question and every option, which the ask's author made, and the answerer for the answer
// part, who is not the ask's author. A part keeps the maker it was written with while its text
// stands, as it keeps its text.
func askPartMakers(ask model.Ask, parts []itemblocks.Block) ([]*string, error) {
	if ask.Answer == nil || len(parts) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(model.Actor{Kind: "user", ID: ask.Answer.User})
	if err != nil {
		return nil, fmt.Errorf("encode the answerer of ask %s: %w", ask.ID, err)
	}
	maker := string(encoded)
	makers := make([]*string, len(parts))
	makers[len(parts)-1] = &maker
	return makers, nil
}

// askPartsSum is the ask's ask_block_state.body_sha256: an ask has no body to hash, so the state
// records the hash of its parts' identity hashes, in order. Each is a fixed-length hex of the
// part's type and text, so no two readings of different parts give one sum.
func askPartsSum(parts []itemblocks.Block) string {
	hashes := make([]string, len(parts))
	for index, part := range parts {
		hashes[index] = part.Hash
	}
	return itemblocks.BodySHA256(strings.Join(hashes, "\n"))
}

// askPartPairs is each distinct link the parts hold, from the part that holds it. A part is its
// own text, so each part's mentions are read from that text alone and paired to it, with no
// offset arithmetic into one body (blockPairs over the one part).
func askPartPairs(parts []itemblocks.Block, ids []string, serverURL string) []blockPair {
	var pairs []blockPair
	for index, part := range parts {
		located := text.ExtractAt(part.Text, serverURL)
		if len(located) == 0 {
			continue
		}
		node := []model.ArtifactBlock{{ID: ids[index], From: 0, To: len(part.Text)}}
		pairs = append(pairs, blockPairs(node, located)...)
	}
	return pairs
}

// IndexAskParts records the ask's parts, written at `at` (the created_at of the insert, the
// edited_at of an edit, the time of the answer), as the current revision of its blocks, in the
// caller's transaction, through the core every item kind's blocks go through
// (indexItemBlocks): it takes the ask's state row after the ask row the caller holds, gives each
// part an id (itemblocks.Align, where an answer is replaced rather than reworded), writes
// the part rows and the text of each at the new revision, reconciles the links the parts hold,
// and stores the state. It bumps the revision only when the parts changed.
//
// It returns the targets of the part mentions it moved, which the caller's event counts.
func IndexAskParts(ctx context.Context, tx pgx.Tx, ask model.Ask, serverURL string, at time.Time) ([]Target, error) {
	parts := AskParts(ask)
	return indexAskParts(ctx, tx, ask, parts, askPartsSum(parts), serverURL, at)
}

// IndexAskPartsCounted is IndexAskParts for a writer that appends an event: it indexes the ask's
// parts, resolves the counted targets they moved (CountedChanges) and merges them into beside,
// what the same write moved elsewhere - the ask's own references, which every writer of an ask's
// question still reconciles (ReplaceCounted, ReplaceCountedEdit), or nothing
// (model.ReferenceChanges{}) for a write that reconciles none. Each target is named once
// (MergeChanges), since the ask's row and the part holding the same link resolve to it alike. It
// is ReplaceCountedItem's shape for an ask, whose question text is reconciled beside its parts
// rather than from one extraction.
func IndexAskPartsCounted(
	ctx context.Context,
	tx pgx.Tx,
	ask model.Ask,
	serverURL string,
	at time.Time,
	beside model.ReferenceChanges,
) (model.ReferenceChanges, error) {
	moved, err := IndexAskParts(ctx, tx, ask, serverURL, at)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	counted, err := CountedChanges(ctx, tx, moved)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return MergeChanges(beside, counted), nil
}

// indexAskParts is IndexAskParts over parts the caller has already read and summed.
func indexAskParts(ctx context.Context, tx pgx.Tx, ask model.Ask, parts []itemblocks.Block, sum, serverURL string, at time.Time) ([]Target, error) {
	makers, err := askPartMakers(ask, parts)
	if err != nil {
		return nil, err
	}
	return indexItemBlocks(ctx, tx, ItemAsk, ask.ID, at, itemBlockSource{
		blocks:  parts,
		bodySum: sum,
		madeBy:  makers,
		ids: func(stored []itemblocks.Stored, next []itemblocks.Block, _, counter int) ([]string, int, error) {
			ids, counter := itemblocks.Align(stored, next, counter, AskAnswerPart)
			return ids, counter, nil
		},
		pairs: func(next []itemblocks.Block, ids []string) []blockPair {
			return askPartPairs(next, ids, serverURL)
		},
	})
}

// askPartColumns selects the three columns an ask's parts are read from, aliased a, in the order
// scanAskForParts reads them, with the time the row was last written.
const askPartColumns = `a.question, coalesce(a.options, '[]'::jsonb), a.answer, coalesce(a.edited_at, a.created_at)`

// scanAskForParts decodes one askPartColumns row as the ask AskParts reads; extra receives, in
// order, any columns the query selects after them.
func scanAskForParts(row pgx.Row, askID string, extra ...any) (model.Ask, time.Time, error) {
	ask := model.Ask{ID: askID}
	var options, answer []byte
	var writtenAt time.Time
	targets := append([]any{&ask.Question, &options, &answer, &writtenAt}, extra...)
	if err := row.Scan(targets...); err != nil {
		return model.Ask{}, time.Time{}, err
	}
	if err := json.Unmarshal(options, &ask.Options); err != nil {
		return model.Ask{}, time.Time{}, fmt.Errorf("decode the options of ask %s: %w", askID, err)
	}
	if len(answer) > 0 {
		var value model.AskAnswer
		if err := json.Unmarshal(answer, &value); err != nil {
			return model.Ask{}, time.Time{}, fmt.Errorf("decode the answer of ask %s: %w", askID, err)
		}
		ask.Answer = &value
	}
	return ask, writtenAt, nil
}

// askForParts reads the ask as its parts are read from it — its question, its options and its
// answer — with the time its row was last written (coalesce(edited_at, created_at)); found is
// false when the ask is gone.
func askForParts(ctx context.Context, q Queryer, askID string) (model.Ask, time.Time, bool, error) {
	ask, writtenAt, err := scanAskForParts(q.QueryRow(ctx, `
		select `+askPartColumns+` from asks a where a.id = $1::uuid
	`, askID), askID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return model.Ask{}, time.Time{}, false, nil
	case err != nil:
		return model.Ask{}, time.Time{}, false, fmt.Errorf("read the parts of ask %s: %w", askID, err)
	}
	return ask, writtenAt, true, nil
}

// indexAskIfOwed is IndexItemIfOwed for an ask: it indexes the ask's parts when they are owed,
// which is when its state row is missing (a build older than the part index wrote the ask),
// owed (owed_at set by such a build's write, or by rebuild-refs), or indexed from other parts
// than the ask holds now. The caller holds the ask row.
func indexAskIfOwed(ctx context.Context, tx pgx.Tx, askID, serverURL string) (bool, error) {
	var stated, owed bool
	var sum *string
	ask, writtenAt, err := scanAskForParts(tx.QueryRow(ctx, `
		select `+askPartColumns+`, s.ask_id is not null, s.owed_at is not null, s.body_sha256
		from asks a left join ask_block_state s on s.ask_id = a.id
		where a.id = $1::uuid
	`, askID), askID, &stated, &owed, &sum)
	if err != nil {
		return false, fmt.Errorf("read the block state of ask %s: %w", askID, err)
	}
	parts := AskParts(ask)
	partsSum := askPartsSum(parts)
	if stated && !owed && sum != nil && *sum == partsSum {
		return false, nil
	}
	if _, err := indexAskParts(ctx, tx, ask, parts, partsSum, serverURL, writtenAt); err != nil {
		return false, fmt.Errorf("index the parts of ask %s: %w", askID, err)
	}
	return true, nil
}
