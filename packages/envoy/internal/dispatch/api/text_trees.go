package api

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// storeDisplayTrees stores the display tree of every text a committed write stored, so a read of
// that text copies the stored bytes instead of reading the markdown again (texttrees). Every call
// stands right after the write's own commit, in the request that made it and before its response:
// never inside the transaction, because a tree that cannot be stored must never fail the write,
// and the read makes it then exactly as it does for a text whose row is not written yet.
//
// A failure is logged under subject - the issue, or the id of the row the texts were written for -
// and changes no answer. Nothing else is reported: the answer a reader is given is the same either
// way.
//
// The store itself is texttrees.StoreFor, the one policy every path that writes a stored text
// keeps, which is also why a caller still holding a pooled connection stores nothing.
func (s *server) storeDisplayTrees(ctx context.Context, subject string, texts ...string) {
	s.storeDisplayTreesIn(ctx, pmdoc.DisplayLatest, subject, texts...)
}

// storeDisplayTreesIn is storeDisplayTrees in one named mode. Every write stores the latest
// mode, which is what a reader of a comment, a message, an ask or a document's head is shown; a
// past version is read in the version mode, which no write produces, so its tree is stored by
// the first read of it (getArtifactVersion) and by nothing else.
func (s *server) storeDisplayTreesIn(ctx context.Context, mode pmdoc.DisplayMode, subject string, texts ...string) {
	if err := texttrees.StoreFor(ctx, s.deps.Store, mode, texts...); err != nil {
		slog.Warn("dispatch: store display trees failed", "subject", subject, "mode", int(mode), "error", err)
	}
}

// primeDisplayTrees puts the stored display tree of each of texts into this process's display
// cache (pmdoc.PutStoredDisplayTree), so the shaping that follows copies the stored bytes
// instead of reading the markdown again, and answers the texts the table holds no tree for.
//
// The whole read is one query, however many texts the answer carries: a comment list of two
// hundred bodies after a restart is one round trip, not one per body and not two hundred
// markdown parses. A text repeated among them - the same body quoted twice - is asked for once.
//
// A text whose tree this process already holds is not asked for at all: the cache is keyed by
// exactly what the table is keyed by (pmdoc.HoldsDisplayTree), and what the shaping is about to
// be answered from is the cache either way. One new comment moves both lists' validators, so the
// read after a write is this case and not the restart: 199 of the 200 bodies of a feed are where
// the comment list left them, and the query that is left asks for the one that is new.
//
// What a held text costs is that this read no longer learns whether the table holds its row. A
// text this process read for itself - one whose tree the shaping built because no row answered
// for it, and whose store then failed - is not asked for again until this process restarts; the
// fill pass (texttreefill) stores it meanwhile, and no answer is different either way, since a
// reader is served the tree this process holds.
//
// It stores nothing. A text answered as missing is read for display by the shaping exactly as it
// was before there was a table, and the request stores it afterwards (storeDisplayTrees), on a
// connection of its own and outside any transaction of its caller's.
//
// A caller still holding a pooled connection reads nothing and every text is missing. A document
// load and a compaction shape their answer inside a read-only transaction (docs/persistence.go,
// docs/compaction.go), and a second connection taken while one is held is the deadlock
// store.ErrNestedAcquire refuses; the reader then builds its trees as it did before, which is
// the one thing this is always allowed to fall back to.
//
// A failed read is logged at warning and answers every text as missing too: a table that cannot
// be queried costs a reader time and never an answer.
func (s *server) primeDisplayTrees(ctx context.Context, texts ...string) (missing []string) {
	return s.primeDisplayTreesIn(ctx, pmdoc.DisplayLatest, texts...)
}

// primeDisplayTreesIn is primeDisplayTrees in one named mode: the latest mode for everything a
// write stores, the version mode for a past version's own text.
func (s *server) primeDisplayTreesIn(ctx context.Context, mode pmdoc.DisplayMode, texts ...string) (missing []string) {
	if len(texts) == 0 {
		return nil
	}
	distinct := make([]string, 0, len(texts))
	sums := make([][32]byte, 0, len(texts))
	seen := make(map[[32]byte]struct{}, len(texts))
	for _, text := range texts {
		// An empty text has no tree and no row (pmdoc.DisplayJSON), so it is neither asked for
		// nor answered as missing: there would be nothing to read and nothing to store.
		if text == "" {
			continue
		}
		sum := texttrees.Sum(text)
		if _, repeated := seen[sum]; repeated {
			continue
		}
		seen[sum] = struct{}{}
		if pmdoc.HoldsDisplayTree(sum, mode) {
			continue
		}
		distinct = append(distinct, text)
		sums = append(sums, sum)
	}
	if len(sums) == 0 {
		return nil
	}
	if store.HoldsPooledConnection(ctx) {
		return distinct
	}
	stored, err := texttrees.For(s.deps.Store).Read(ctx, mode, sums)
	if err != nil {
		slog.Warn("dispatch: read display trees failed", "texts", len(sums), "error", err)
		return distinct
	}
	// The capacity is exactly what the table did not answer for, which on a list this process
	// has already served is none: that read allocates nothing here and its request writes
	// nothing afterwards.
	missing = make([]string, 0, len(sums)-len(stored))
	for index, sum := range sums {
		tree, held := stored[sum]
		if !held {
			missing = append(missing, distinct[index])
			continue
		}
		pmdoc.PutStoredDisplayTree(sum, mode, tree)
	}
	return missing
}

// readDisplayTrees primes the display trees of a read that carries them and answers the store of
// whatever the table did not hold, which a caller defers:
//
//	defer s.readDisplayTrees(r.Context(), trees, issueKey, texts)()
//
// The prime runs where the call stands, before the shaping it serves. The store runs when the
// function holding the defer returns - in a handler, after the answer is written - because it is
// the next reader's saving and not this one's, and a store that fails has changed no answer.
//
// A caller that asked for no tree neither reads the table nor writes to it: no field of the
// response would carry what either found.
func (s *server) readDisplayTrees(ctx context.Context, trees bool, subject string, texts []string) func() {
	return s.readDisplayTreesIn(ctx, pmdoc.DisplayLatest, trees, subject, texts)
}

// readVersionDisplayTrees is readDisplayTrees for a past version's text, which is read in the
// version mode because the attributes the server wrote into that text are part of what the
// version said. No write stores that mode, so the first read of a version stores its tree and
// every later read of it copies those bytes - the one place a read, rather than a write, is
// where a tree first reaches the table.
func (s *server) readVersionDisplayTrees(ctx context.Context, trees bool, subject string, texts []string) func() {
	return s.readDisplayTreesIn(ctx, pmdoc.DisplayVersion, trees, subject, texts)
}

func (s *server) readDisplayTreesIn(ctx context.Context, mode pmdoc.DisplayMode, trees bool, subject string, texts []string) func() {
	if !trees || len(texts) == 0 {
		return func() {}
	}
	missing := s.primeDisplayTreesIn(ctx, mode, texts...)
	return func() { s.storeDisplayTreesIn(ctx, mode, subject, missing...) }
}

// storeEventDisplayTrees stores the display trees of every stored text the published events carry
// in their payloads, keyed as every other tree is by the text alone. The event log and a live
// frame answer those trees by path (eventDisplayTrees), and an event carries texts no write of a
// stored text would ever store: an ask edit's payload holds the question and option labels it
// replaced, which no row holds any more, and those are read on every reading of the ask's history.
//
// The texts are found by the reader the event lists use (eventPayloadTexts), so what is stored is
// exactly what a read of those events answers, field for field.
//
// It runs after the broker has the events: a live socket's frame is a reader waiting, and it is
// not held for a write whose only effect is to save that reader's next read a markdown parse.
func (s *server) storeEventDisplayTrees(ctx context.Context, events []model.Event) {
	var texts []string
	var subject string
	for _, event := range events {
		texts = eventDisplayTexts(texts, event)
		if subject == "" && len(texts) > 0 {
			subject = event.Type + " " + strconv.FormatInt(event.ID, 10)
		}
	}
	s.storeDisplayTrees(ctx, subject, texts...)
}

// eventDisplayTexts appends every stored text the events carry a tree for, found by the reader
// the shaping uses (eventPayloadTexts), so what a read primes and stores is exactly what that
// read answers.
func eventDisplayTexts(texts []string, events ...model.Event) []string {
	for _, event := range events {
		eventPayloadTexts(event, func(_, text string) { texts = append(texts, text) })
	}
	return texts
}

// commentDisplayTexts appends every stored text a comment is shown with: its body, and the
// excerpt of the block it answers (commentRead.shape).
func commentDisplayTexts(texts []string, comments ...model.Comment) []string {
	for _, comment := range comments {
		texts = append(texts, comment.Body)
		if comment.ReplyBlock != nil {
			texts = append(texts, comment.ReplyBlock.Text)
		}
	}
	return texts
}

// messageDisplayTexts is commentDisplayTexts for messages, which are shown exactly as comments
// are (shapedMessage.shape).
func messageDisplayTexts(texts []string, messages ...model.Message) []string {
	for _, message := range messages {
		texts = append(texts, message.Body)
		if message.ReplyBlock != nil {
			texts = append(texts, message.ReplyBlock.Text)
		}
	}
	return texts
}

// askReadDisplayTexts appends every stored text each of asks is shown with (askRead.shape),
// which is texttrees.AskShownTexts, the list this read and the background fill of the trees
// share: a write stores the texts it wrote (texttrees.AskWrittenTexts), and a reader is shown
// those and what the transitions wrote.
func askReadDisplayTexts(texts []string, asks ...model.Ask) []string {
	for _, ask := range asks {
		texts = texttrees.AskShownTexts(texts, ask.Question, ask.Options, ask.Answer, ask.Resolution)
	}
	return texts
}

// askAnswerDisplayTexts appends the text of each answer a thread lists (askAnswerReads).
func askAnswerDisplayTexts(texts []string, answers []model.AskAnswer) []string {
	for _, answer := range answers {
		if answer.Text != nil {
			texts = append(texts, *answer.Text)
		}
	}
	return texts
}

// askEditDisplayTexts appends the texts each recorded rewording is shown with: the question and
// the option labels and descriptions it replaced (askEditReads). No row holds them any more, and
// every reading of the ask's history shows them, so the publish path stored them
// (storeEventDisplayTrees) and this is the read that copies them back.
func askEditDisplayTexts(texts []string, edits []model.AskEdit) []string {
	for _, edit := range edits {
		texts = texttrees.AskWrittenTexts(texts, edit.Previous.Question, edit.Previous.Options)
	}
	return texts
}
