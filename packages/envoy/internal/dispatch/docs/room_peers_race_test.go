package docs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Three browsers type into one document over its websocket while an agent edits it through the
// API, and nothing they do reads the room's live tree beside another's write. A browser's update
// reaches the room as a Yjs update ygo applies inside one transaction, under the document's lock
// (sync.ApplySyncMessage, crdt.ApplyUpdateV1); the API's edit reads and writes its transaction's
// own fork (applyLive), takes the room's state under that lock to build the fork (forkLive,
// crdt.EncodeStateAsUpdateV1), and, once it commits, applies its update to the room in a
// transaction and checks the published update for lost text through readLive. Run under -race,
// which reports a walk of the live tree beside a write, until enough of the API's edits have
// overlapped the browsers' typing; the API's last edit must then still be in the document. A walk
// meets the document's lock at each text it reads and none in a run of rules, and the browsers
// type at the document's start, so the document opens with a long run of rules: a walk that took
// no lock would read the room's first block, then walk the rules while a keystroke rewrites it.
func TestThreeBrowsersAndTheAPIEditOneRoomAtOnce(t *testing.T) {
	seeded := rules(2_000) + "# Heading\n\nbefore\n\nafter\n"
	service, artifactID, serverURL := newPeeredService(t, seeded)
	browsers := make([]crdt.ClientID, 3)
	for index := range browsers {
		browsers[index] = typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond)
	}
	typed := func() uint64 {
		var total uint64
		for _, browser := range browsers {
			total += peerApplied(service, artifactID, browser)
		}
		return total
	}
	agent := model.Actor{Kind: "session", ID: "agent-session"}
	words := [2]string{"before", "during"}
	edits := 0
	overlapping(t, func() bool {
		before := typed()
		from, to := words[edits%2], words[(edits+1)%2]
		edits++
		if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: from, With: to}}, agent, nil); err != nil {
			t.Fatalf("edit %d through the API: %v", edits, err)
		}
		return typed() > before
	})
	text, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	if want := words[edits%2]; !strings.Contains(text, want) {
		t.Fatalf("the document after %d API edits beside the browsers = %q, want the last edit's %q", edits, text, want)
	}
}
