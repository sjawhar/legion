package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The stored history of a room that formatted text, through its own commits and a browser's,
// replays to every version and every class. The room runs no format cleanup for its own commits,
// and a fold of its stored updates must not run one either (newDocumentCopy): a fold that did held
// formatting the room never held, and a version the room rendered as *x*y read back from the
// store as *xy*. Each version is named after a commit, so the check compares the fold with the
// room at every step of the edits, not only at their end.
func TestStoredOrderFormattedWritesReplayToEveryVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "seed")
	ctx := context.Background()
	agent := model.Actor{Kind: "session", ID: "agent-session"}

	versions := 0
	name := func() {
		t.Helper()
		waitForLandedAppends(t, service, artifactID)
		if _, err := namedVersion(t, service, artifactID, "step", agent); err != nil {
			t.Fatalf("name a version: %v", err)
		}
		versions++
	}

	var text *crdt.YXmlText
	roomCommit := func(change func(*crdt.Transaction)) {
		t.Helper()
		if err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			if text == nil {
				fragment := doc.GetXmlFragment(fragmentName)
				transact(func(txn *crdt.Transaction) {
					paragraph := crdt.NewYXmlElement("paragraph")
					fragment.InsertElement(txn, fragment.Len(), paragraph)
					text = crdt.NewYXmlText()
					paragraph.InsertText(txn, 0, text)
				})
			}
			transact(change)
		}); err != nil {
			t.Fatalf("commit in the room: %v", err)
		}
		name()
	}
	writeItalicLeavingMarkers(func(change func(*crdt.YXmlText, *crdt.Transaction)) {
		roomCommit(func(txn *crdt.Transaction) { change(text, txn) })
	})

	browser := crdt.New()
	if err := crdt.ApplyUpdateV1(browser, encodedRoom(t, service, artifactID), nil); err != nil {
		t.Fatalf("open the document in the browser: %v", err)
	}
	browserFragment := browser.GetXmlFragment(fragmentName)
	var browserText *crdt.YXmlText
	browserCommit := func(change func(*crdt.Transaction)) {
		t.Helper()
		before := browser.StateVector()
		browser.Transact(change)
		update := crdt.EncodeStateAsUpdateV1(browser, before)
		err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
			if err := crdt.ApplyUpdateV1(doc, update, &browserOrigin{}); err != nil {
				t.Errorf("apply the browser's update: %v", err)
			}
		})
		if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
			t.Fatalf("send the browser's update: %v", err)
		}
		name()
	}
	browserCommit(func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		browserFragment.InsertElement(txn, browserFragment.Len(), paragraph)
		browserText = crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, browserText)
	})
	writeItalicLeavingMarkers(func(change func(*crdt.YXmlText, *crdt.Transaction)) {
		browserCommit(func(txn *crdt.Transaction) { change(browserText, txn) })
	})

	if got, want := latestVersionMarkdown(t, service, artifactID), roomMarkdown(t, service, artifactID); got != want {
		t.Fatalf("the last version holds %q, want the room's %q", got, want)
	}
	report := assertStoredOrder(t, service, artifactID)
	if report.UpdatesChecked == 0 || report.VersionsChecked < versions {
		t.Fatalf("report = %+v, want updates checked and at least the %d named versions checked", report, versions)
	}
}
