package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A crafted client can write an over-deep tree through the room's CRDT without passing through
// pmdoc.Update. Settlement skips that tree as it does every tree outside the schema: it writes no
// version and does not fail the live room.
func TestSettlementSkipsATreeOverTheDepthBound(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		transact(func(txn *crdt.Transaction) {
			docstest.WriteDeepChain(txn, fragment, pmdoc.MaxTreeDepth+1, "a")
		})
	}); err != nil {
		t.Fatalf("write crafted CRDT tree: %v", err)
	}

	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)

	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count document versions: %v", err)
	}
	if versions != 1 {
		t.Fatalf("settlement wrote %d versions for an over-deep document, want 1", versions)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocSchema) {
		t.Fatalf("read over-deep document: %v, want ErrDocSchema", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.failed != nil {
		t.Fatalf("settlement failed the live room: %v", state.failed)
	}
	if state.settleFailures != 0 {
		t.Fatalf("settlement recorded %d failures for an over-deep document", state.settleFailures)
	}
}

// The sweep of unrecorded marks reads the live document and then walks it again inside its
// transaction on the room, and a peer's update can land between the two. A walk that then meets a
// tree past the depth bound refuses it as the document outside the schema (ErrDocSchema), as every
// read of that tree does, rather than as an internal error. Every other write that marks or unmarks
// runs on its transaction's fork of the room (applyLive), which no peer's update reaches.
func TestMarkWalksTellATreeDeepenedSinceTheirReadAsOutsideTheSchema(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name  string
		setUp func(t *testing.T, service *Service, artifactID string)
		write func(service *Service, artifactID string) error
	}{
		{
			name: "sweeping an unrecorded mark",
			setUp: func(t *testing.T, service *Service, artifactID string) {
				browserMark(t, service, artifactID, "proofComment", "dangling", "brown")
			},
			write: func(service *Service, artifactID string) error {
				return service.unmarkExpired(artifactID, []pmdoc.MarkRef{{Type: "proofComment", ID: "dangling"}})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "The quick brown fox")
			test.setUp(t, service, artifactID)
			var fragment *crdt.YXmlFragment
			if err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
				fragment = doc.GetXmlFragment(fragmentName)
			}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
				t.Fatalf("warm the live document: %v", err)
			}
			deepened := false
			service.inServiceTransaction = func(txn *crdt.Transaction) {
				if !deepened {
					deepened = true
					docstest.WriteDeepChain(txn, fragment, pmdoc.MaxTreeDepth+1, "deep")
				}
			}
			if err := test.write(service, artifactID); !errors.Is(err, ErrDocSchema) {
				t.Fatalf("%s once a peer deepened the tree past the bound: %v, want ErrDocSchema", test.name, err)
			}
			if !deepened {
				t.Fatalf("%s made no service transaction to deepen the tree in", test.name)
			}
		})
	}
}
