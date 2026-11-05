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
// A caller still holding a pooled connection stores nothing. Taking a second connection while one
// is held is the deadlock store.ErrNestedAcquire refuses, and work this optional asks rather than
// being refused, which would log the refusal's stack in a path that is working as intended.
func (s *server) storeDisplayTrees(ctx context.Context, subject string, texts ...string) {
	if len(texts) == 0 {
		return
	}
	if store.HoldsPooledConnection(ctx) {
		return
	}
	if err := texttrees.New(s.displayTreePool()).Store(ctx, pmdoc.DisplayLatest, texts...); err != nil {
		slog.Warn("dispatch: store display trees failed", "subject", subject, "error", err)
	}
}

// displayTreePool is the pool the trees are stored through, nil on a server with no database - one
// a test builds directly - where the store answers its own refusal and the write is unaffected.
func (s *server) displayTreePool() *store.Pool {
	if s.deps.Store == nil {
		return nil
	}
	return s.deps.Store.Pool
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
		eventPayloadTexts(event, func(_, text string) { texts = append(texts, text) })
		if subject == "" && len(texts) > 0 {
			subject = event.Type + " " + strconv.FormatInt(event.ID, 10)
		}
	}
	s.storeDisplayTrees(ctx, subject, texts...)
}

// askDisplayTexts appends every stored text of an ask to texts: its question, and the label and
// description of each of its answer options. The ask's answer and resolution are not among them -
// they are written by the transitions, which store their own (closeAsk).
func askDisplayTexts(texts []string, question string, options []model.AskOption) []string {
	texts = append(texts, question)
	for _, option := range options {
		texts = append(texts, option.Label, option.Description)
	}
	return texts
}
