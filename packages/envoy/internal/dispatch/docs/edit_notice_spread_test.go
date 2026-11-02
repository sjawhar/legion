package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A document the writer stored before the reader read a blank line inside a typed block in a list
// item as that item's: a tight item holding an ask, whose question and options that writer put a
// blank line between. The reader reads that item spread now, and the version a person's answer
// writes carries the writer's own shape, which it reads tight, so the two stored versions differ
// in the item's spread as well as in the ask's server state. The answer is still no edit of
// theirs, and the sweep sends no notice (serverStateOnly, stripListSpread).
func TestEditNoticeAnAnswerInAnItemStoredSpacedSendsNone(t *testing.T) {
	const spaced = "- Listed.\n  :::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\n" +
		"  Which one?\n\n  - A\n  - B\n  :::\n- Second.\n"
	const answered = "- Listed.\n  :::ask{#decision urgency=\"high\" multiple=\"false\" state=\"answered\"" +
		" answered_by=\"alice@example.com\" answered_at=\"2026-11-02T05:00:00Z\" selected=\"[\\\"A\\\"]\"}\n" +
		"  Which one?\n  - A\n  - B\n  :::\n- Second.\n"
	service, _ := newTestService(t)
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	document := createIssueDocument(t, service.store, 2, spaced)
	addPersonsVersion(t, service.store, document, 2, answered, alice)
	if sent, err := service.sendDueEditNotices(context.Background()); err != nil || sent != 0 {
		t.Fatalf("sweep after an answer in an item stored spaced = %d sent, %v; want none sent", sent, err)
	}
	if edits := humanEdits(t, service.store, document); len(edits) != 0 {
		t.Fatalf("the answer sent %d edit notices, want none: %+v", len(edits), edits)
	}
}
