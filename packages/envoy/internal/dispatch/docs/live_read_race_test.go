package docs

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// Every read of a resident room runs while the room's websocket peers write it. A walk of the live
// tree takes no lock (YXmlFragment.Children), so a read that walks it beside a peer's update reads
// that update halfway - a torn tree, or the process's death on a concurrent map read and write -
// and under -race it is a data race. Each read below runs over and over while a peer types into
// the document, and must take the document's lock for its read or read a copy taken under it.
func TestReadsOfALiveDocumentRunBesideItsPeers(t *testing.T) {
	const (
		readFor = 500 * time.Millisecond
		seeded  = "# Heading\n\nbefore\n\nafter\n"
	)
	alice := model.Actor{Kind: "user", ID: "alice"}
	reads := []struct {
		name string
		// read runs one read of the room and returns any error it should not have met.
		read func(ctx context.Context, service *Service, artifactID, headingID string) error
	}{
		{"GET text", func(ctx context.Context, service *Service, artifactID, _ string) error {
			_, _, err := service.TextWithToken(ctx, artifactID)
			return err
		}},
		{"GET blocks", func(ctx context.Context, service *Service, artifactID, _ string) error {
			_, err := service.Blocks(ctx, artifactID)
			return err
		}},
		{"GET block path", func(ctx context.Context, service *Service, artifactID, headingID string) error {
			_, err := service.BlockPath(ctx, artifactID, headingID)
			return err
		}},
		{"rendered text", func(ctx context.Context, service *Service, artifactID, _ string) error {
			_, err := service.Text(ctx, artifactID)
			return err
		}},
		{"quote's block", func(ctx context.Context, service *Service, artifactID, _ string) error {
			_, err := service.BlockForQuote(ctx, artifactID, "before")
			return err
		}},
		{"suggestion kind", func(ctx context.Context, service *Service, artifactID, _ string) error {
			_, err := service.SuggestionKind(ctx, artifactID, "absent")
			return allowing(err, ErrAnchorMissing)
		}},
		{"browser mark", func(ctx context.Context, service *Service, artifactID, _ string) error {
			service.markWait = 10 * time.Millisecond
			_, err := service.VerifyMark(ctx, artifactID, MarkComment, "absent")
			return allowing(err, ErrAnchorMissing)
		}},
		{"edit token", func(ctx context.Context, service *Service, artifactID, _ string) error {
			_, err := service.currentToken(ctx, artifactID)
			return err
		}},
		{"table edit prevalidation", func(ctx context.Context, service *Service, artifactID, headingID string) error {
			// The heading is no table, so the check refuses the operation after it has read the tree.
			_, err := service.prevalidateLiveOperations(ctx, artifactID, []model.EditOp{
				{Op: "delete_row", Block: headingID, Index: json.RawMessage("1")},
			})
			if err == nil {
				return errors.New("deleting a heading's row was not refused")
			}
			return nil
		}},
		{"version capture", func(ctx context.Context, service *Service, artifactID, _ string) error {
			tx, err := service.store.Pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			_, err = service.SnapshotVersion(joined, artifactID, alice)
			return err
		}},
		{"published edit", func(ctx context.Context, service *Service, artifactID, _ string) error {
			tx, err := service.store.Pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{
				{Op: "insert", After: "end", Markdown: "agent\n"},
			}, alice, nil); err != nil {
				return err
			}
			// Publishing the committed write reads the room back for the text it wrote.
			return ledger.Commit(ctx)
		}},
		{"unrecorded mark sweep", func(_ context.Context, service *Service, artifactID, _ string) error {
			return service.unmarkExpired(artifactID, []pmdoc.MarkRef{{Type: string(MarkComment), ID: "absent"}})
		}},
		{"block id backfill", func(ctx context.Context, service *Service, artifactID, _ string) error {
			return service.backfillBlockIDs(ctx, artifactID).Err
		}},
	}
	for _, test := range reads {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID, serverURL := newPeeredService(t, seeded)
			headingID := firstBlockID(t, service, artifactID)
			typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond)
			ctx := context.Background()
			for deadline := time.Now().Add(readFor); time.Now().Before(deadline); {
				if err := test.read(ctx, service, artifactID, headingID); err != nil {
					t.Fatalf("read the live document: %v", err)
				}
			}
		})
	}

	// Settlement reads the room only once every update it has seen is durable, so it runs beside a
	// peer that pauses between keystrokes long enough for that. A walk of the tree meets a lock at
	// each text it reads (YXmlText.ToDelta) and none in a run of rules, so the document leads with
	// one long enough that a keystroke lands inside the walk. The peer deletes each paragraph it
	// types, so settlement also meets a block its read counted to stamp that is gone when it stamps.
	t.Run("settlement", func(t *testing.T) {
		service, artifactID, serverURL := newPeeredService(t, strings.Repeat("***\n\n", 500)+seeded)
		typeIntoDocument(t, service, serverURL, artifactID, 20*time.Millisecond)
		for deadline := time.Now().Add(readFor); time.Now().Before(deadline); {
			state := service.room(artifactID)
			state.mu.Lock()
			generation := state.gen
			state.mu.Unlock()
			service.settleRoom(artifactID, generation)
		}
	})

	// The room's own update observer renders the room after each update, while another peer's
	// update can be applying.
	t.Run("update observer", func(t *testing.T) {
		service, artifactID, serverURL := newPeeredService(t, seeded)
		typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond)
		typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond)
		time.Sleep(readFor)
	})
}

// newPeeredService is a document service holding markdown, with settlement left to the test, and
// the URL its document websocket answers on.
func newPeeredService(t *testing.T, markdown string) (*Service, string, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, markdown)
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	return service, artifactID, server.URL
}

func firstBlockID(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	blocks, err := service.Blocks(context.Background(), artifactID)
	if err != nil || len(blocks) == 0 {
		t.Fatalf("blocks = %v, %v; want the seeded heading", blocks, err)
	}
	return blocks[0].ID
}

// typeIntoDocument connects a peer that writes the document as a browser's keystrokes do - a
// paragraph inserted at the start, a word typed into it, the paragraph deleted, each its own
// update - round after round until the test ends, waiting a random time up to gap between
// updates so a read's start does not fall into step with them. It returns once the peer has sent
// its first update and the room is resident.
func typeIntoDocument(t *testing.T, service *Service, serverURL, artifactID string, gap time.Duration) {
	t.Helper()
	peer := newDeepPeer(t, serverURL, artifactID)
	fragment := peer.doc.GetXmlFragment(fragmentName)
	stopped := make(chan struct{})
	first := make(chan struct{})
	var wrote sync.Once
	var done sync.WaitGroup
	done.Go(func() {
		for {
			var paragraph *crdt.YXmlElement
			var run *crdt.YXmlText
			for _, change := range []func(*crdt.Transaction){
				func(txn *crdt.Transaction) {
					paragraph = crdt.NewYXmlElement("paragraph")
					fragment.InsertElement(txn, 0, paragraph)
					run = crdt.NewYXmlText()
					paragraph.InsertText(txn, 0, run)
				},
				func(txn *crdt.Transaction) { run.Insert(txn, 0, "typed", nil) },
				// The room's own blocks reach the peer too, so it deletes its paragraph where it
				// stands rather than whatever block comes first.
				func(txn *crdt.Transaction) {
					for index, child := range fragment.Children() {
						if any(child) == any(paragraph) {
							fragment.Delete(txn, index, 1)
							return
						}
					}
				},
			} {
				select {
				case <-stopped:
					return
				default:
				}
				update := docstest.Transact(peer.doc, change)
				if update == nil {
					continue
				}
				if err := peer.sendFrame(ygsync.EncodeUpdate(update)); err != nil {
					t.Errorf("send peer update: %v", err)
					return
				}
				wrote.Do(func() { close(first) })
				time.Sleep(rand.N(gap))
			}
		}
	})
	// Registered after the peer's connection, so it runs first: the writing ends, and the room
	// applies the peer's last update and makes every update durable, before the connection
	// closes. The service's shutdown then has none of the peer's updates left to drain inside its
	// budget, which a backlog of keystrokes would overrun.
	t.Cleanup(func() {
		close(stopped)
		done.Wait()
		client := peer.doc.ClientID()
		sent := peer.doc.StateVector().Clock(client)
		waitFor(t, 30*time.Second, "the room to apply the peer's last update", func() bool {
			room := service.srv.GetDoc(artifactID)
			return room == nil || room.StateVector().Clock(client) >= sent
		})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
			t.Errorf("wait for the peer's updates to reach persistence: %v", err)
		}
		if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
			t.Errorf("wait for the peer's updates to become durable: %v", err)
		}
	})
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("the peer sent no update")
	}
	waitFor(t, 10*time.Second, "the room to load", func() bool {
		return service.srv.GetDoc(artifactID) != nil
	})
}

// allowing is err unless it is want, which the read is expected to meet.
func allowing(err, want error) error {
	if errors.Is(err, want) {
		return nil
	}
	return err
}
