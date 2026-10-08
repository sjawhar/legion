package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// pendingAuthorRows is the document's durable pending authors (doc_pending_authors): every author
// whose landed change no committed version lists yet.
func pendingAuthorRows(t *testing.T, database *store.Store, artifactID string) []model.Actor {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select actor from doc_pending_authors where artifact_id = $1 order by actor_kind, actor_id
	`, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending authors: %v", err)
	}
	defer rows.Close()
	var actors []model.Actor
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan a pending author: %v", err)
		}
		var actor model.Actor
		if err := json.Unmarshal(raw, &actor); err != nil {
			t.Fatalf("decode a pending author: %v", err)
		}
		actors = append(actors, actor)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the document's pending authors: %v", err)
	}
	return actors
}

// commitNamedVersion names a version of the live document as actor in a transaction of its own,
// as POST /artifacts/{id}/versions does, and returns it.
func commitNamedVersion(t *testing.T, service *Service, artifactID string, actor model.Actor) model.Version {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s's version: %v", actor.ID, err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	result, err := service.NamedVersion(joined, artifactID, actor.ID+"'s version", actor)
	if err != nil {
		t.Fatalf("name %s's version: %v", actor.ID, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit %s's version: %v", actor.ID, err)
	}
	return result.Version
}

// editAsConnectedPeer makes edit as actor's browser: actor is the room's one connected peer while
// the room applies it, so the room's update observer credits actor alone (creditContentChange).
func editAsConnectedPeer(t *testing.T, service *Service, artifactID string, connection uint64, actor model.Actor, edit func(*pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("warm the room for %s's edit: %v", actor.ID, err)
	}
	service.addConnection(artifactID, connection, actor)
	editAsPeer(t, service, artifactID, edit)
	service.removeConnection(artifactID, connection)
}

// waitForLandedAppends waits until every update the room has recorded is durable.
func waitForLandedAppends(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), untilTestDeadline(t))
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room's updates to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room's updates to become durable: %v", err)
	}
}

// awaitAppendLandedOrQueued returns once the room's latest update has either landed or is waiting
// for the document's advisory lock (withRoomLock's pg_advisory_lock), whichever the code under test
// makes it do.
func awaitAppendLandedOrQueued(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(untilTestDeadline(t))
	for time.Now().Before(deadline) {
		if !service.hasPendingUpdates(artifactID) &&
			(!service.hasDurableAppend(artifactID) || lockWaits(t, ctx, service.store, "%pg_advisory_lock(hashtext%") > 0) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the room's update neither landed nor waited for the document lock")
}

// countVersionAuthor is how many non-fixture versions list actor. The test artifact's initial
// version is a fixture baseline whose generic creator is `user:alice`; it predates every browser
// edit this helper measures and must not count as that peer's credit.
func countVersionAuthor(t *testing.T, database *store.Store, artifactID string, actor model.Actor) int {
	t.Helper()
	listed := 0
	for number := 2; number < nextVersionNumber(t, database, artifactID); number++ {
		if slices.Contains(versionAuthors(t, database, artifactID, number), actor) {
			listed++
		}
	}
	return listed
}

// A settlement lists an edit whose append is held until after it commits, and that append, once
// it lands, puts nothing back: once the room has gone and another process loads the document, the
// next version lists only its own author. The append is held by holding the edit's update observer
// once it has credited alice, before ygo hands the update to persistence. W2: the settlement
// deleted the pending row, and the append inserted a fresh one that the next load adopted.
func TestASettlementsListedEditIsNotOwedAgainWhenItsHeldAppendLands(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	agent := model.Actor{Kind: "session", ID: "agent-session"}
	bob := model.Actor{Kind: "session", ID: "bob-session"}
	// A landed change past the latest version, so the settlement has content to version.
	writeThroughLedger(t, service, artifactID, agent, "Agent's paragraph.\n", withoutVersion)
	waitForLandedAppends(t, service, artifactID)
	service.addConnection(artifactID, 1, alice)
	release := holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, appendBlocks(t, "Alice's paragraph.\n"))

	before := latestVersionNumber(t, service, artifactID)
	settleCurrentGeneration(t, service, artifactID)
	settled := latestVersionNumber(t, service, artifactID)
	if settled != before+1 {
		t.Fatalf("latest version = %d, want %d, the settlement's", settled, before+1)
	}
	if authors := versionAuthors(t, service.store, artifactID, settled); !slices.Equal(authors, []model.Actor{agent, alice}) {
		t.Fatalf("the settlement's version lists %+v, want %+v and %+v", authors, agent, alice)
	}
	release()
	waitForLandedAppends(t, service, artifactID)
	service.removeConnection(artifactID, 1)

	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the room: %v", err)
	}
	restarted, _ := newTestServiceInstance(t, service.store)
	writeThroughLedger(t, restarted, artifactID, bob, "Bob's paragraph.\n", withNamedVersion)
	if authors := latestVersionAuthors(t, restarted, artifactID); !slices.Equal(authors, []model.Actor{bob}) {
		t.Fatalf("the restarted process's version lists %+v, want %+v alone: version %d listed alice", authors, bob, settled)
	}
	if owed := pendingAuthorRows(t, service.store, artifactID); len(owed) != 0 {
		t.Fatalf("pending authors = %+v, want none", owed)
	}
}

// A browser edit made after a named version took its authors and before that version commits
// arms a settlement, and the version still owes nothing it listed: the settlement lists the later
// editor alone. W4: the version's commit skipped its in-memory release when the edit moved the
// room's generation, after its durable release had run.
func TestABrowserEditBeforeAVersionCommitsLeavesOnlyItsOwnAuthorOwed(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	versioner := model.Actor{Kind: "session", ID: "versioner-session"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, appendBlocks(t, "Alice's paragraph.\n"))
	waitForLandedAppends(t, service, artifactID)

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the named version: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	named, err := service.NamedVersion(joined, artifactID, "versioner's version", versioner)
	if err != nil {
		t.Fatalf("name the version: %v", err)
	}
	if !slices.Contains(named.Version.Authors, alice) {
		t.Fatalf("the named version lists %+v, want alice", named.Version.Authors)
	}
	editAsConnectedPeer(t, service, artifactID, 2, bob, appendBlocks(t, "Bob's paragraph.\n"))
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the named version: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)

	settleCurrentGeneration(t, service, artifactID)
	if latest := latestVersionNumber(t, service, artifactID); latest != named.Version.Number+1 {
		t.Fatalf("latest version = %d, want %d, the settlement's", latest, named.Version.Number+1)
	}
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Equal(authors, []model.Actor{bob}) {
		t.Fatalf("the settlement's version lists %+v, want %+v alone: alice was listed on version %d", authors, bob, named.Version.Number)
	}
}

// roomSeedEnv names a seed TestTwoProcessesListEveryEditorOnExactlyOneVersion replays; unset, the
// test picks one and logs it.
const roomSeedEnv = "DISPATCH_TEST_PENDING_AUTHORS_SEED"

// Two Dispatch processes serve one document's rooms at once, as a rolling deploy's overlap does.
// Browser edits from new peers, named versions, settlements and evictions interleave on both, in an
// order a logged seed draws, and a last version on each process lists whatever is still owed.
// Every editor is listed on exactly one version: W1 (memory kept credits after their append
// committed, and a lease delete let the other process list them again) and W3 (a load's
// generation bump discarded the first process's later credits) each break that.
func TestTwoProcessesListEveryEditorOnExactlyOneVersion(t *testing.T) {
	seed := time.Now().UnixNano()
	if fixed := os.Getenv(roomSeedEnv); fixed != "" {
		parsed, err := strconv.ParseInt(fixed, 10, 64)
		if err != nil {
			t.Fatalf("%s = %q: %v", roomSeedEnv, fixed, err)
		}
		seed = parsed
	}
	t.Logf("seed %d (replay with %s=%d)", seed, roomSeedEnv, seed)
	random := rand.New(rand.NewPCG(uint64(seed), 0))

	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, database)
	seedServiceText(t, taskA, artifactID, "First.\n\nSecond.\n")
	tasks := []*Service{taskA, taskB}
	names := []string{"A", "B"}

	var peers []model.Actor
	var connection atomic.Uint64
	const steps = 24
	for step := range steps {
		index := random.IntN(2)
		task, name := tasks[index], names[index]
		switch op := random.IntN(6); {
		case op < 3:
			peer := model.Actor{Kind: "user", ID: fmt.Sprintf("peer-%02d", len(peers))}
			peers = append(peers, peer)
			t.Logf("step %d: %s edits on task %s", step, peer.ID, name)
			editAsConnectedPeer(t, task, artifactID, connection.Add(1), peer, appendBlocks(t, peer.ID+"'s paragraph.\n"))
			waitForLandedAppends(t, task, artifactID)
		case op == 3:
			t.Logf("step %d: task %s names a version", step, name)
			commitNamedVersion(t, task, artifactID, model.Actor{Kind: "session", ID: "versioner-" + name})
		case op == 4:
			t.Logf("step %d: task %s settles", step, name)
			settleCurrentGeneration(t, task, artifactID)
		default:
			t.Logf("step %d: task %s evicts its room", step, name)
			if err := task.Evict(context.Background(), artifactID); err != nil {
				t.Fatalf("step %d: evict task %s's room: %v", step, name, err)
			}
		}
	}
	for index, task := range tasks {
		commitNamedVersion(t, task, artifactID, model.Actor{Kind: "session", ID: "sweeper-" + names[index]})
	}

	for _, peer := range peers {
		if listed := countVersionAuthor(t, database, artifactID, peer); listed != 1 {
			t.Errorf("%s is listed on %d versions, want exactly one", peer.ID, listed)
		}
	}
	if owed := pendingAuthorRows(t, database, artifactID); len(owed) != 0 {
		t.Errorf("pending authors after the last versions = %+v, want none", owed)
	}
}

// arrangeNoVersionSettlement has alice's browser type "x" and delete it, then runs the settlement
// her edits armed, which writes no version: the document reads as its latest version again.
func arrangeNoVersionSettlement(t *testing.T, service *Service, artifactID string, alice model.Actor) {
	t.Helper()
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	before := latestVersionNumber(t, service, artifactID)
	editAsConnectedPeer(t, service, artifactID, 1, alice, appendBlocks(t, "x\n"))
	editAsConnectedPeer(t, service, artifactID, 1, alice, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = tree.Children[:len(tree.Children)-1]
		return tree
	})
	waitForLandedAppends(t, service, artifactID)
	service.settle = 20 * time.Millisecond
	service.ScheduleSettlement(artifactID)
	waitFor(t, 30*time.Second, "the no-version settlement to commit", func() bool {
		owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
		return err == nil && !owed
	})
	if latest := latestVersionNumber(t, service, artifactID); latest != before {
		t.Fatalf("latest version = %d after alice typed and deleted, want %d: the settlement wrote one", latest, before)
	}
}

// An author a settlement that wrote no version did not list stays owed across a restart: the next
// version lists her beside its own author. W5: the settlement deleted the durable credit and kept
// it only in memory.
func TestAnAuthorANoVersionSettlementDidNotListSurvivesARestart(t *testing.T) {
	service, artifactID := newTestService(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "session", ID: "bob-session"}
	arrangeNoVersionSettlement(t, service, artifactID, alice)

	restarted, _ := newTestServiceInstance(t, service.store)
	writeThroughLedger(t, restarted, artifactID, bob, "Bob's paragraph.\n", withNamedVersion)
	if authors := latestVersionAuthors(t, restarted, artifactID); !slices.Equal(authors, []model.Actor{bob, alice}) {
		t.Fatalf("the restarted process's version lists %+v, want %+v and %+v", authors, bob, alice)
	}
}

// A room whose settlement wrote no version is released once it idles out, like any other: the
// state holds nothing the durable record does not (LEGION-513). W5: the settlement left the room's
// pending authors in memory and marked the state unsettled for good.
func TestARoomWhoseSettlementWroteNoVersionIsReleasedWhenIdle(t *testing.T) {
	service, database := newRoomReleaseService(t)
	artifactID := createDocument(t, database, "# First")
	arrangeNoVersionSettlement(t, service, artifactID, model.Actor{Kind: "user", ID: "alice"})

	waitFor(t, 30*time.Second, "the idle room to be evicted", func() bool {
		return service.srv.GetDoc(artifactID) == nil && !slices.Contains(service.srv.Rooms(), artifactID)
	})
	waitFor(t, 10*time.Second, "the evicted room's state to be released", func() bool {
		_, held := service.rooms.Load(artifactID)
		return !held
	})
}

// An author whose change landed through another process is listed once, on whichever process's
// version comes next: task A's version lists alice, whose edit task B's room took, and task B's
// next version does not list her again. The scoped cross-task rule (packages/envoy/AGENTS.md).
func TestAnAuthorAnotherProcessLandedIsListedOnce(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, database)
	seedServiceText(t, taskA, artifactID, "First.\n\nSecond.\n")
	alice := model.Actor{Kind: "user", ID: "alice"}
	editAsConnectedPeer(t, taskB, artifactID, 1, alice, appendBlocks(t, "Alice's paragraph.\n"))
	waitForLandedAppends(t, taskB, artifactID)

	onA := commitNamedVersion(t, taskA, artifactID, model.Actor{Kind: "session", ID: "versioner-A"})
	if !slices.Contains(onA.Authors, alice) {
		t.Fatalf("task A's version lists %+v, want alice, whose edit landed", onA.Authors)
	}
	onB := commitNamedVersion(t, taskB, artifactID, model.Actor{Kind: "session", ID: "versioner-B"})
	if slices.Contains(onB.Authors, alice) {
		t.Fatalf("task B's next version lists %+v, want no alice: task A's version %d listed her", onB.Authors, onA.Number)
	}
}

// A room's load reads what it needs without the document's advisory lock, which a durable writer
// can hold for as long as its transaction runs: a cold socket loads a document that owes a
// settlement while another transaction holds that lock.
func TestALoadNeverWaitsForTheDocumentLock(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	writeThroughLedger(t, service, artifactID, model.Actor{Kind: "session", ID: "writer-session"}, "Writer's paragraph.\n", withoutVersion)
	if owed, err := settlementPending(context.Background(), service.store.Pool, artifactID); err != nil || !owed {
		t.Fatalf("the document owes a settlement = %v (%v), want true", owed, err)
	}
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the room: %v", err)
	}

	ctx := context.Background()
	holder, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, artifactID); err != nil {
		t.Fatalf("hold the document lock: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	browser := connectPeer(t, server.URL, artifactID, "reader")
	if err := browser.AskForDocument(); err != nil {
		t.Fatalf("ask the room for its document: %v", err)
	}
	const limit = 10 * time.Second
	select {
	case <-browser.Answers:
	case <-browser.Ended:
		t.Fatal("the socket closed before the room sent its document")
	case <-time.After(limit):
		t.Fatalf("the cold room did not load within %s while another transaction held the document lock", limit)
	}
}

// An upload clears only the edits its fork held: bob's, which landed before the fork, and not
// alice's, which a browser made once the fork had read the room. The next version lists alice and
// not bob, and nothing owes bob.
func TestAnUploadClearsOnlyTheEditsItsForkHeld(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	uploader := model.Actor{Kind: "session", ID: "uploader-session"}
	editAsConnectedPeer(t, service, artifactID, 1, bob, appendBlocks(t, "Bob's paragraph.\n"))
	waitForLandedAppends(t, service, artifactID)

	edited := false
	service.afterForkRead = func(room string) {
		if room != artifactID || edited {
			return
		}
		edited = true
		editAsConnectedPeer(t, service, artifactID, 2, alice, appendBlocks(t, "Alice's paragraph.\n"))
		awaitAppendLandedOrQueued(t, service, artifactID)
	}
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the upload: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	const uploaded = "First.\n\nSecond, uploaded.\n"
	if _, err := service.ReplaceText(joined, artifactID, uploaded, uploader); err != nil {
		t.Fatalf("upload the replacement: %v", err)
	}
	service.afterForkRead = nil
	if !edited {
		t.Fatal("the upload's fork never read the room")
	}
	number := nextVersionNumber(t, service.store, artifactID)
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, $2, $3, $4)
	`, artifactID, number, uploaded, []model.Actor{uploader}); err != nil {
		t.Fatalf("insert the upload's version: %v", err)
	}
	ledger.WroteVersion(artifactID, model.Version{Number: number, Authors: []model.Actor{uploader}})
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the upload: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	if owed := pendingAuthorRows(t, service.store, artifactID); slices.Contains(owed, bob) {
		t.Fatalf("pending authors after the upload = %+v, want no bob: his edit was in its fork", owed)
	}

	next := commitNamedVersion(t, service, artifactID, model.Actor{Kind: "session", ID: "versioner-session"})
	if !slices.Contains(next.Authors, alice) || slices.Contains(next.Authors, bob) {
		t.Fatalf("the version after the upload lists %+v, want alice and not bob", next.Authors)
	}
}

// A named version whose transaction is discarded marks nothing it read: alice's edit, observed
// before the version read the room and appended after, lands owed and is listed once, on the next
// committed version.
func TestADiscardedNamedVersionMarksNothing(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}

	edited := false
	service.afterReadWarm = func(room string) {
		if room != artifactID || edited {
			return
		}
		edited = true
		editAsConnectedPeer(t, service, artifactID, 1, alice, appendBlocks(t, "Alice's paragraph.\n"))
		awaitAppendLandedOrQueued(t, service, artifactID)
	}
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the discarded version: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	discarded, err := service.NamedVersion(joined, artifactID, "discarded", model.Actor{Kind: "session", ID: "discarded-session"})
	service.afterReadWarm = nil
	if err != nil {
		t.Fatalf("name the discarded version: %v", err)
	}
	if !slices.Contains(discarded.Version.Authors, alice) {
		t.Fatalf("the discarded version lists %+v, want alice", discarded.Version.Authors)
	}
	ledger.Discard()
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the discarded version back: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	if owed := pendingAuthorRows(t, service.store, artifactID); !slices.Contains(owed, alice) {
		t.Fatalf("pending authors after the discard = %+v, want alice", owed)
	}

	commitNamedVersion(t, service, artifactID, model.Actor{Kind: "session", ID: "first-versioner"})
	commitNamedVersion(t, service, artifactID, model.Actor{Kind: "session", ID: "second-versioner"})
	if listed := countVersionAuthor(t, service.store, artifactID, alice); listed != 1 {
		t.Fatalf("alice is listed on %d versions, want exactly one", listed)
	}
	if owed := pendingAuthorRows(t, service.store, artifactID); len(owed) != 0 {
		t.Fatalf("pending authors = %+v, want none", owed)
	}
}

// An eviction that runs while a named version commits loses nothing the version read and owes
// nothing it listed: alice, whose edit was in flight when the version read the room, is listed on
// that version alone, and nothing owes her once her append lands.
func TestAnEvictionDuringAVersionsCommitListsItsAuthorOnce(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}

	edited := false
	service.afterReadWarm = func(room string) {
		if room != artifactID || edited {
			return
		}
		edited = true
		editAsConnectedPeer(t, service, artifactID, 1, alice, appendBlocks(t, "Alice's paragraph.\n"))
		awaitAppendLandedOrQueued(t, service, artifactID)
	}
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	named, err := service.NamedVersion(joined, artifactID, "evicted", model.Actor{Kind: "session", ID: "versioner-session"})
	service.afterReadWarm = nil
	if err != nil {
		t.Fatalf("name the version: %v", err)
	}
	if !slices.Contains(named.Version.Authors, alice) {
		t.Fatalf("the version lists %+v, want alice", named.Version.Authors)
	}
	evicted := make(chan error, 1)
	go func() { evicted <- service.Evict(context.Background(), artifactID) }()
	time.Sleep(50 * time.Millisecond) // let the eviction reach the room's close before the commit
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the version: %v", err)
	}
	select {
	case err := <-evicted:
		if err != nil {
			t.Fatalf("evict the room: %v", err)
		}
	case <-time.After(untilTestDeadline(t)):
		t.Fatal("the eviction did not finish")
	}
	waitFor(t, 30*time.Second, "alice's append to land", func() bool {
		return !service.hasDurableAppend(artifactID) && !service.hasPendingUpdates(artifactID)
	})
	if owed := pendingAuthorRows(t, service.store, artifactID); len(owed) != 0 {
		t.Fatalf("pending authors after the commit = %+v, want none: the version listed alice", owed)
	}
	commitNamedVersion(t, service, artifactID, model.Actor{Kind: "session", ID: "next-versioner"})
	if listed := countVersionAuthor(t, service.store, artifactID, alice); listed != 1 {
		t.Fatalf("alice is listed on %d versions, want exactly one", listed)
	}
}

// Two named versions commit while bob's append has not yet asked for the document lock: the first
// lists him, the second lists nobody new, and his append, landing after both, owes nothing. A
// second version that read the first's consumed in-flight record would list bob again.
func TestTwoVersionsBeforeAnAppendAsksForTheLockListItsAuthorOnce(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	entered := make(chan struct{})
	release := make(chan struct{})
	persistence := &blockingFirstAppendStore{VersionedStore: NewPgVersioned(database), entered: entered, release: release}
	service := New(Deps{
		Store:       database,
		Persistence: persistence,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      time.Hour,
	})
	t.Cleanup(func() {
		if persistence.released.CompareAndSwap(false, true) {
			close(release)
		}
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	bob := model.Actor{Kind: "user", ID: "bob"}
	first := model.Actor{Kind: "session", ID: "first-versioner"}
	second := model.Actor{Kind: "session", ID: "second-versioner"}

	editAsConnectedPeer(t, service, artifactID, 1, bob, appendBlocks(t, "Bob's paragraph.\n"))
	select {
	case <-entered:
	case <-time.After(untilTestDeadline(t)):
		t.Fatal("bob's append never reached the store")
	}
	v1 := commitNamedVersion(t, service, artifactID, first)
	v2 := commitNamedVersion(t, service, artifactID, second)
	if persistence.released.CompareAndSwap(false, true) {
		close(release)
	}
	waitForLandedAppends(t, service, artifactID)

	if !slices.Contains(v1.Authors, bob) {
		t.Fatalf("the first version lists %+v, want bob", v1.Authors)
	}
	if !slices.Equal(v2.Authors, []model.Actor{second}) {
		t.Fatalf("the second version lists %+v, want %+v alone", v2.Authors, second)
	}
	if owed := pendingAuthorRows(t, database, artifactID); slices.Contains(owed, bob) {
		t.Fatalf("pending authors after bob's append landed = %+v, want no bob", owed)
	}
}
