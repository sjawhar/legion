package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestDecisionBrowserVersionRefusesForgedAttributionAndPointers(t *testing.T) {
	for _, test := range []struct {
		name, attributes string
	}{
		{"another person's attribution", `decided_by="alice"`},
		{"from a requirement", `from="dispatch://CORE-1/artifact/target#b-r1"`},
		{"from an unrelated ask", `from="dispatch://CORE-2/artifact/foreign#b-q1"`},
		{"shaped in an unrelated issue", `shaped="dispatch://CORE-2/artifact/foreign#b-r1"`},
		{"from_ask in an unrelated issue", `from_ask="dispatch://CORE-2/ask/ASK_ID"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, service := blockIndexService(t)
			writeIndexedDocument(t, database, service, "target", ":::requirement{#r1}\nA local requirement.\n:::\n")
			foreign := writeIndexedDocument(t, database, service, "foreign", ":::requirement{#r1}\nA foreign requirement.\n:::\n\n:::ask{#q1}\nWhich name?\n:::\n")
			if _, err := database.Pool.Exec(t.Context(), `update artifacts set issue_key='CORE-2' where id=$1`, foreign); err != nil {
				t.Fatal(err)
			}
			settleCurrentGeneration(t, service, foreign)
			var askID string
			if err := database.Pool.QueryRow(t.Context(), `select id::text from asks where block_artifact_id=$1 and block_id='q1'`, foreign).Scan(&askID); err != nil {
				t.Fatal(err)
			}
			source := writeIndexedDocument(t, database, service, "source", "Context.\n")
			settleCurrentGeneration(t, service, source)
			attributes := strings.ReplaceAll(test.attributes, "ASK_ID", askID)
			browserReplaceText(t, service, source, fmt.Sprintf(":::decision{#d1 %s}\nA browser choice.\n:::\n", attributes), model.Actor{Kind: "user", ID: "bob"})
			settleCurrentGeneration(t, service, source)
			for _, row := range pointerRowsOf(t, database, source) {
				if row.Kind == "decided_by" || row.Kind == "from" || row.Kind == "shaped" || row.Kind == "from_ask" {
					t.Fatalf("browser forgery wrote a trusted edge: %#v", row)
				}
			}
			var versions int
			if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id=$1`, source).Scan(&versions); err != nil || versions != 1 {
				t.Fatalf("browser forgery wrote a version: %d (%v)", versions, err)
			}
			browserReplaceText(t, service, source, "A valid later edit.\n", model.Actor{Kind: "user", ID: "alice"})
			settleCurrentGeneration(t, service, source)
			if got := latestVersionNumber(t, service, source); got != 2 {
				t.Fatalf("valid edit after a refused browser pointer reached version %d, want 2", got)
			}
		})
	}
}

func TestDecisionBrowserVersionAcceptsOwnAttributionAndScopedPointers(t *testing.T) {
	database, service := blockIndexService(t)
	spec := writeIndexedDocument(t, database, service, "spec", ":::requirement{#r1}\nA local requirement.\n:::\n\n:::ask{#q1}\nWhich name?\n:::\n")
	if _, err := database.Pool.Exec(t.Context(), `update artifacts set is_primary=true where id=$1`, spec); err != nil {
		t.Fatal(err)
	}
	source := writeIndexedDocument(t, database, service, "source", "Context.\n")
	settleCurrentGeneration(t, service, source)
	browserReplaceText(t, service, source, ":::decision{#d1 decided_by=\"bob\" from=\"dispatch://CORE-1/spec#b-q1\" shaped=\"dispatch://CORE-1/spec#b-r1\"}\nA browser choice.\n:::\n", model.Actor{Kind: "user", ID: "bob"})
	settleCurrentGeneration(t, service, source)
	rows := pointerRowsOf(t, database, source)
	if len(rows) != 3 {
		t.Fatalf("valid browser choice has pointers %#v, want its three relations", rows)
	}
	if got := latestVersionNumber(t, service, source); got != 2 {
		t.Fatalf("valid browser choice left version %d, want 2", got)
	}
}

func TestDecisionVersionCannotIndexUnreconciledAnswer(t *testing.T) {
	database, service := blockIndexService(t)
	source := writeIndexedDocument(t, database, service, "source", "Context.\n")
	editLiveTree(t, service, source, forgedAnsweredAsk("forged", "alice", &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}}))
	err := joinedWrite(service, func(ctx context.Context) error {
		_, err := service.SnapshotVersion(ctx, source, model.Actor{Kind: "user", ID: "bob"})
		return err
	})
	if err == nil {
		t.Fatal("a version accepted browser attribution before ask reconciliation")
	}
	for _, row := range pointerRowsOf(t, database, source) {
		if row.Kind == "decided_by" {
			t.Fatalf("a version trusted unreconciled browser attribution: %#v", row)
		}
	}
}

func TestDecisionBrowserMixedAuthorsAuthorizeOnlyTheIntroducer(t *testing.T) {
	for _, test := range []struct {
		name, first, last, decider string
		wantVersion                int
	}{
		{"forgery then claimed person's unrelated edit", "bob", "alice", "alice", 1},
		{"forgery after claimed person's unrelated edit", "alice", "bob", "alice", 1},
		{"own attribution then another person's unrelated edit", "alice", "bob", "alice", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, service := blockIndexService(t)
			source := writeIndexedDocument(t, database, service, "source", "Context.\n")
			settleCurrentGeneration(t, service, source)
			choice := fmt.Sprintf(":::decision{#d1 decided_by=\"%s\"}\nA choice.\n:::\n", test.decider)
			if test.name == "forgery after claimed person's unrelated edit" {
				browserReplaceText(t, service, source, "An unrelated edit.\n", model.Actor{Kind: "user", ID: test.first})
				browserReplaceText(t, service, source, choice, model.Actor{Kind: "user", ID: test.last})
			} else {
				browserReplaceText(t, service, source, choice, model.Actor{Kind: "user", ID: test.first})
				browserReplaceText(t, service, source, choice+"\nAn unrelated edit.\n", model.Actor{Kind: "user", ID: test.last})
			}
			settleCurrentGeneration(t, service, source)
			if got := latestVersionNumber(t, service, source); got != test.wantVersion {
				t.Fatalf("version = %d, want %d", got, test.wantVersion)
			}
			rows := pointerRowsOf(t, database, source)
			if test.wantVersion == 1 && len(rows) != 0 || test.wantVersion == 2 && (len(rows) != 1 || rows[0].ToID != test.decider) {
				t.Fatalf("introduced attribution rows = %#v", rows)
			}
			state := service.room(source)
			state.mu.Lock()
			failed, failures := state.failed, state.settleFailures
			state.mu.Unlock()
			if failed != nil || failures != 0 {
				t.Fatalf("policy check poisoned the room: failed=%v settleFailures=%d", failed, failures)
			}
		})
	}
}

func TestDecisionBrowserOriginKeepsItsWriterThroughDelayedObservation(t *testing.T) {
	database, service := blockIndexService(t)
	source := writeIndexedDocument(t, database, service, "source", "Context.\n")
	settleCurrentGeneration(t, service, source)
	state := service.room(source)
	state.mu.Lock()
	state.connected[1] = model.Actor{Kind: "user", ID: "bob"}
	state.mu.Unlock()
	tree, err := pmdoc.Parse(":::decision{#d1 decided_by=\"alice\"}\nA forged choice.\n:::\n")
	if err != nil {
		t.Fatal(err)
	}
	err = service.onPeerUpdate(websocket.PeerUpdateInfo{Room: source, Origin: "delayed-bob"}, func() error {
		// The introducing peer leaves and the claimed person joins before the log is drained.
		state.mu.Lock()
		delete(state.connected, 1)
		state.connected[2] = model.Actor{Kind: "user", ID: "alice"}
		state.mu.Unlock()
		var writeErr error
		if err := service.srv.Apply(context.Background(), source, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
			fragment := doc.GetXmlFragment(fragmentName)
			doc.Transact(func(txn *crdt.Transaction) {
				writeErr = pmdoc.Update(txn, fragment, tree)
			}, "delayed-bob")
		}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return err
		}
		return writeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	delete(state.connected, 2)
	state.mu.Unlock()
	settleCurrentGeneration(t, service, source)
	if got := latestVersionNumber(t, service, source); got != 1 {
		t.Fatalf("delayed Bob update authorized Alice and wrote version %d", got)
	}
	if rows := pointerRowsOf(t, database, source); len(rows) != 0 {
		t.Fatalf("delayed Bob update wrote person edges: %#v", rows)
	}
}
