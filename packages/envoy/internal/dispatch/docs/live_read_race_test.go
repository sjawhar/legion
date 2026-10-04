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
	"sync/atomic"
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
// the document, until enough of its runs have overlapped one of the peer's updates (overlapping),
// and must take the document's lock for its read or read a copy taken under it. A walk of the tree
// meets a lock at each text it reads (YXmlText.ToDelta) and none in a run of rules, so the document
// holds one long enough that a keystroke lands inside the walk.
func TestReadsOfALiveDocumentRunBesideItsPeers(t *testing.T) {
	rules := func(count int) string { return strings.Repeat("***\n\n", count) }
	seeded := "# Heading\n\n" + rules(200) + "before\n\nafter\n"
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
		{"block id backfill", func(ctx context.Context, service *Service, artifactID, _ string) error {
			return service.backfillBlockIDs(ctx, artifactID).Err
		}},
	}
	for _, test := range reads {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID, serverURL := newPeeredService(t, seeded)
			service.markWait = 10 * time.Millisecond
			headingID := firstBlockID(t, service, artifactID)
			peer := typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond)
			ctx := context.Background()
			overlapping(t, func() bool {
				before := peerApplied(service, artifactID, peer)
				if err := test.read(ctx, service, artifactID, headingID); err != nil {
					t.Fatalf("read the live document: %v", err)
				}
				return peerApplied(service, artifactID, peer) > before
			})
		})
	}

	// A version's capture reads the room inside the Apply that holds it, between a transaction's
	// database work and the version's, so a run counts as overlapping only when the first peer's
	// update reached the room after the capture held the room (afterReadWarm), and its document
	// leads with a longer run of rules, as settlement's does. A capture that walks the room's
	// replica holds the replica's lock across the walk, which the update observer of a peer's
	// update takes next, so each peer lands at most one update in that walk: a second peer types
	// beside the first.
	t.Run("version capture", func(t *testing.T) {
		service, artifactID, serverURL := newPeeredService(t, rules(2_000)+seeded)
		// The hook is set before the peers type, as settlement's are.
		var peer atomic.Value
		var held atomic.Uint64
		applied := func() uint64 {
			client, _ := peer.Load().(crdt.ClientID)
			return peerApplied(service, artifactID, client)
		}
		service.afterReadWarm = func(string) { held.Store(applied() + 1) }
		peer.Store(typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond))
		typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond)
		ctx := context.Background()
		overlapping(t, func() bool {
			held.Store(0)
			tx, err := service.store.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			if _, err := service.SnapshotVersion(joined, artifactID, alice); err != nil {
				t.Fatalf("capture a version of the live document: %v", err)
			}
			at := held.Load()
			return at > 0 && applied()+1 > at
		})
	})

	// The unrecorded-mark sweep reads inside the transaction that unmarks, which holds the
	// document's lock, so none of the peer's updates lands while it reads: it runs for a fixed time
	// rather than until its runs have overlapped one.
	t.Run("unrecorded mark sweep", func(t *testing.T) {
		service, artifactID, serverURL := newPeeredService(t, seeded)
		typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond)
		for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); {
			if err := service.unmarkExpired(artifactID, []pmdoc.MarkRef{{Type: string(MarkComment), ID: "absent"}}); err != nil {
				t.Fatalf("sweep the live document: %v", err)
			}
		}
	})

	// A published write reads the room back for the text it wrote (recordPublishedLoss), once per
	// write. One write is committed and published, then its read-back runs over and over: a whole
	// write per read would leave too few reads beside the peer's updates to meet one.
	t.Run("published edit", func(t *testing.T) {
		service, artifactID, serverURL := newPeeredService(t, seeded)
		peer := typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond)
		ctx := context.Background()
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		joined, ledger := service.Join(ctx, tx)
		defer ledger.Discard()
		if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{
			{Op: "insert", After: "end", Markdown: "agent\n"},
		}, alice, nil); err != nil {
			t.Fatal(err)
		}
		if err := ledger.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		write := ledger.liveWriteFor(artifactID)
		if write == nil || !write.lostVerdict || len(write.lost) != 0 {
			t.Fatalf("the published write's read-back = %+v, want nothing lost", write)
		}
		overlapping(t, func() bool {
			before := peerApplied(service, artifactID, peer)
			write.lostVerdict = false
			service.recordPublishedLoss(write)
			if !write.lostVerdict || len(write.lost) != 0 {
				t.Fatalf("the published write's read-back = lost %v, verdict %t; want nothing lost", write.lost, write.lostVerdict)
			}
			return peerApplied(service, artifactID, peer) > before
		})
	})

	// Settlement reads the room only once every update it has seen is durable, so it runs beside a
	// peer that pauses between keystrokes long enough for that, and only a settlement that read the
	// room counts, overlapping when the peer's update reached the room between the settlement's
	// lock and the end of its read (afterSettleLock, afterSettleRead). Its document leads with a
	// longer run of rules, since the database work around its read leaves the walk a smaller share
	// of each run. The peer deletes each paragraph it types, so settlement also meets a block its
	// read counted to stamp that is gone when it stamps.
	t.Run("settlement", func(t *testing.T) {
		service, artifactID, serverURL := newPeeredService(t, rules(2_000)+seeded)
		// The hooks are set before the peer types, since a settlement can run on a timer from then.
		var peer atomic.Value
		var locked, read atomic.Uint64
		applied := func() uint64 {
			client, _ := peer.Load().(crdt.ClientID)
			return peerApplied(service, artifactID, client)
		}
		service.afterSettleLock = func(string) { locked.Store(applied()) }
		service.afterSettleRead = func(string) { read.Store(applied()) }
		peer.Store(typeIntoDocument(t, service, serverURL, artifactID, 20*time.Millisecond))
		overlapping(t, func() bool {
			state := service.room(artifactID)
			state.mu.Lock()
			generation := state.gen
			state.mu.Unlock()
			read.Store(0)
			service.settleRoom(artifactID, generation)
			return read.Load() > locked.Load()
		})
	})

	// The room's own update observer renders the room after each update, while another peer's
	// update can be applying.
	t.Run("update observer", func(t *testing.T) {
		service, artifactID, serverURL := newPeeredService(t, seeded)
		typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond)
		typeIntoDocument(t, service, serverURL, artifactID, 2*time.Millisecond)
		time.Sleep(500 * time.Millisecond)
	})
}

// overlapping runs read until overlapsWanted runs have met a peer's update. A peer that keeps
// typing brings that evidence at whatever speed the loaded machine allows, so no fixed time bounds
// it. The loop ends with the test's failure once the test has failed, which is how a typing peer
// whose send failed reports it (typeIntoDocument), and once the test binary's deadline is within
// overlapMargin, so a room that stopped taking the peer's updates fails the subtest by name rather
// than in the binary's timeout panic. A deadline that leaves no more than overlapMargin to begin
// with - a local `-timeout` shorter than the margin - fails once, by name, before the loop: that
// reads like a misconfigured run, not a peer that stopped overlapping.
func overlapping(t *testing.T, read func() bool) {
	t.Helper()
	rawDeadline, bounded := t.Deadline()
	if bounded && time.Until(rawDeadline) <= overlapMargin {
		t.Fatalf("the test binary's deadline leaves %s, not more than overlapMargin (%s): raise -timeout or lower overlapMargin for a local run", time.Until(rawDeadline).Round(time.Millisecond), overlapMargin)
	}
	deadline := rawDeadline.Add(-overlapMargin)
	runs := 0
	for overlapped := 0; overlapped < overlapsWanted; runs++ {
		if t.Failed() {
			t.Fatalf("%d of %d runs overlapped a peer's update before the test failed, want %d", overlapped, runs, overlapsWanted)
		}
		if bounded && time.Now().After(deadline) {
			t.Fatalf("%d of %d runs overlapped a peer's update with %s left before the test binary's deadline, want %d", overlapped, runs, overlapMargin, overlapsWanted)
		}
		if read() {
			overlapped++
		}
	}
	t.Logf("%d runs, %d of them beside a peer's update", runs, overlapsWanted)
}

const (
	overlapsWanted = 50
	// overlapMargin is what overlapping leaves of the test binary's deadline: time for the
	// subtest's typing peers to stop and their room to drain, which typeIntoDocument's cleanup
	// waits up to 30 s for at each step, so the failure is reported before the timeout panics.
	overlapMargin = 90 * time.Second
)

// peerApplied is how much of peer's typing the room has applied: its clock for the peer.
func peerApplied(service *Service, artifactID string, peer crdt.ClientID) uint64 {
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		return 0
	}
	return room.StateVector().Clock(peer)
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

// typists counts, per room, the peers typeIntoDocument has typing into it.
var typists sync.Map

// typeIntoDocument connects a peer that writes the document as a browser's keystrokes do - a
// paragraph inserted at the start, a word typed into it, the paragraph deleted, each its own
// update - round after round until the test ends, waiting a random time up to gap between
// updates so a read's start does not fall into step with them, and never running further ahead of
// the room than keepUp allows. It returns, once the peer has sent its first update and the room is
// resident, the peer's client id.
func typeIntoDocument(t *testing.T, service *Service, serverURL, artifactID string, gap time.Duration) crdt.ClientID {
	t.Helper()
	peer := newDeepPeer(t, serverURL, artifactID)
	fragment := peer.doc.GetXmlFragment(fragmentName)
	stopped := make(chan struct{})
	first := make(chan struct{})
	var wrote sync.Once
	var done sync.WaitGroup
	value, _ := typists.LoadOrStore(artifactID, new(atomic.Int64))
	typing := value.(*atomic.Int64)
	typing.Add(1)
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
				keepUp(service, artifactID, peer.doc, stopped)
				time.Sleep(rand.N(gap))
			}
		}
	})
	// Registered after the peer's connection, so it runs first: the writing ends, the room applies
	// the peer's last update, and, once no other peer is typing into the room, it makes every
	// update durable, before the connection closes. The service's shutdown then has none of the
	// peers' updates left to drain inside its budget, which a backlog of keystrokes would overrun.
	t.Cleanup(func() {
		close(stopped)
		done.Wait()
		client := peer.doc.ClientID()
		sent := peer.doc.StateVector().Clock(client)
		waitFor(t, 30*time.Second, "the room to apply the peer's last update", func() bool {
			room := service.srv.GetDoc(artifactID)
			return room == nil || room.StateVector().Clock(client) >= sent
		})
		// The room counts the updates it has not stored across all its peers (pendingUpdates), and
		// a peer still typing keeps that count above zero for as long as it types whenever the
		// room stores more slowly than it types, so only the last peer to stop waits for it.
		if typing.Add(-1) > 0 {
			return
		}
		typists.Delete(artifactID)
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
	return peer.doc.ClientID()
}

// A typing peer runs at most typingLead clocks of its own ahead of what the room has applied, and
// leaves the room at most typingBacklog updates it has not stored (keepUp).
const (
	typingLead    = 16
	typingBacklog = 16
)

// keepUp waits while the peer runs further ahead of the room than typingLead and typingBacklog
// allow, or until it stops typing. A peer that sent as fast as it could outran the room on a
// loaded machine, under -race or not, and the test's end then waited past its deadline for the
// backlog it had queued; one bounded this loosely still keeps the room busy with its updates.
func keepUp(service *Service, artifactID string, peer *crdt.Doc, stopped <-chan struct{}) {
	client := peer.ClientID()
	sent := peer.StateVector().Clock(client)
	state := service.room(artifactID)
	for {
		room := service.srv.GetDoc(artifactID)
		if room == nil {
			return
		}
		state.mu.Lock()
		unstored := int64(state.pendingUpdates)
		state.mu.Unlock()
		unstored += state.durableAppends.Load()
		if sent <= room.StateVector().Clock(client)+typingLead && unstored <= typingBacklog {
			return
		}
		select {
		case <-stopped:
			return
		case <-time.After(100 * time.Microsecond):
		}
	}
}

// allowing is err unless it is want, which the read is expected to meet.
func allowing(err, want error) error {
	if errors.Is(err, want) {
		return nil
	}
	return err
}
