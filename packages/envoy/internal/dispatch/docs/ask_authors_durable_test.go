package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The author of an ask block no settlement has indexed yet is recorded in doc_ask_block_authors
// where it is known, in the transaction that makes the block durable, so a settlement on another
// process - one that never saw the write, or the one that resumed the document after the writer's
// stopped - creates the ask with the right author (plan D6). Each test settles on task B, a fresh
// process whose room loads the document with the block already in it, after alice's later browser
// edit on task A made her the document's latest edit source: without the durable record, task B's
// settlement would name her, the fallback for a block its room did not see introduced
// (askBlockSources.author).

// recordedAskAuthors is artifactID's durable ask block authors, keyed by block id.
func recordedAskAuthors(t *testing.T, database *store.Store, artifactID string) map[string]model.Actor {
	t.Helper()
	recorded, err := readRecordedAskAuthors(context.Background(), database.Pool, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	return recorded
}

// makeAliceTheLatestEditSource has alice's browser on service append a line, so the document's
// pending-settlement row names her as its latest edit source.
func makeAliceTheLatestEditSource(t *testing.T, service *Service, artifactID string) model.Actor {
	t.Helper()
	alice := model.Actor{Kind: "user", ID: "alice-on-task-a"}
	editAsBrowserPeer(t, service, artifactID, 2, alice, appendBlocks(t, "Alice's later line.\n"))
	waitForLandedAppends(t, service, artifactID)
	_, lastActor, err := readOwedSettlement(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if lastActor == nil || *lastActor != alice {
		t.Fatalf("the document's latest edit source is %+v, want alice, so the fallback author would not differ from the block's", lastActor)
	}
	return alice
}

// requireNoRecordedAskAuthors fails unless artifactID's durable ask authors are gone: deleted by
// the settlement that created the ask rows, or for blocks its tree no longer held.
func requireNoRecordedAskAuthors(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if recorded := recordedAskAuthors(t, database, artifactID); len(recorded) != 0 {
		t.Fatalf("doc_ask_block_authors still holds %+v after the settlement, want no row", recorded)
	}
}

// Bob types an ask block in his browser on task A; its append records him in the update's own
// transaction. Task B, which never saw the update, settles the document and the ask is his, and
// the row goes with the transaction that created the ask.
func TestAnAskBlockTypedOnOneProcessIsItsTypistsWhenAnotherProcessSettlesIt(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-a"}
	editAsBrowserPeer(t, pair.taskA, pair.artifactID, 1, bob,
		appendBlocks(t, ":::ask{#bob-ask urgency=\"med\" multiple=\"false\"}\nWhich region goes first?\n:::\n"))
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	if recorded := recordedAskAuthors(t, pair.database, pair.artifactID); recorded["bob-ask"] != bob {
		t.Fatalf("doc_ask_block_authors after bob's append = %+v, want bob-ask recorded as bob", recorded)
	}
	makeAliceTheLatestEditSource(t, pair.taskA, pair.artifactID)

	settleCurrentGeneration(t, pair.taskB, pair.artifactID)
	if author := blockAskAuthor(t, pair.taskB, pair.artifactID, "bob-ask"); author != bob {
		t.Fatalf("task B made bob's ask with author %+v, want bob, who typed it on task A", author)
	}
	if actor := blockAskOpenedActor(t, pair.taskB, pair.artifactID, "bob-ask"); actor != bob {
		t.Fatalf("task B's ask.opened names %+v, want bob", actor)
	}
	requireNoRecordedAskAuthors(t, pair.database, pair.artifactID)
}

// An agent's live write introduces an ask block on task A; its commit records the agent in the
// same transaction (Ledger.recordSettlementCredit), and task B's settlement keeps the writer.
func TestALiveWritesAskBlockKeepsItsWriterWhenAnotherProcessSettlesIt(t *testing.T) {
	pair := newProcessPair(t)
	agent := model.Actor{Kind: "session", ID: "session-agent-writer"}
	writeThroughLedger(t, pair.taskA, pair.artifactID, agent,
		":::ask{#agent-ask urgency=\"high\" multiple=\"false\"}\nShip it today?\n:::\n", withoutVersion)
	if recorded := recordedAskAuthors(t, pair.database, pair.artifactID); recorded["agent-ask"] != agent {
		t.Fatalf("doc_ask_block_authors after the agent's commit = %+v, want agent-ask recorded as the agent", recorded)
	}
	makeAliceTheLatestEditSource(t, pair.taskA, pair.artifactID)

	settleCurrentGeneration(t, pair.taskB, pair.artifactID)
	if author := blockAskAuthor(t, pair.taskB, pair.artifactID, "agent-ask"); author != agent {
		t.Fatalf("task B made the agent's ask with author %+v, want the agent, who wrote it on task A", author)
	}
	requireNoRecordedAskAuthors(t, pair.database, pair.artifactID)
}

// A seed records its ask blocks' author in its own transaction. Its pending-settlement row can
// name no latest edit source - a seed whose own version lists the seeder writes none
// (Ledger.recordSettlementCredit, releasedAuthors) - and a settlement then names SettlementActor on
// its events: before the durable record, a process other than the seeder's, which never
// registered the seed's blocks, made the seeded ask SettlementActor's rather than the seeder's.
func TestASeededAskBlockIsTheSeedersWhenAnotherProcessSettlesIt(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	seeder := model.Actor{Kind: "user", ID: "seed"}
	seedServiceText(t, taskA, artifactID, ":::ask{#seeded-ask urgency=\"med\" multiple=\"false\"}\nWho owns the rollout?\n:::\n")
	if recorded := recordedAskAuthors(t, database, artifactID); recorded["seeded-ask"] != seeder {
		t.Fatalf("doc_ask_block_authors after the seed = %+v, want seeded-ask recorded as the seeder", recorded)
	}
	// The row a seed whose own version listed the seeder leaves: owed, naming no edit source.
	if _, err := database.Pool.Exec(context.Background(), `
		update doc_settlements_pending set last_actor = null where artifact_id = $1
	`, artifactID); err != nil {
		t.Fatalf("clear the pending row's latest edit source: %v", err)
	}

	settleCurrentGeneration(t, taskB, artifactID)
	if author := blockAskAuthor(t, taskB, artifactID, "seeded-ask"); author != seeder {
		t.Fatalf("task B made the seeded ask with author %+v, want the seeder", author)
	}
	if actor := blockAskOpenedActor(t, taskB, artifactID, "seeded-ask"); actor != seeder {
		t.Fatalf("task B's ask.opened for the seeded ask names %+v, want the seeder", actor)
	}
	requireNoRecordedAskAuthors(t, database, artifactID)
}

// Bob's browser on task A leaves an ask block whose id Postgres cannot store, so it has no row of
// its own; the backfill's stamp mints it a new id and records the author it carries to it in the
// transaction that stores the stamp, and task B's settlement keeps bob.
func TestAStampedAskBlockKeepsItsAuthorWhenAnotherProcessSettlesIt(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-a"}
	const question = "Which transport do we pick?"
	editAsBrowserPeer(t, pair.taskA, pair.artifactID, 1, bob,
		appendBlockWithCraftedID(t, ":::ask{urgency=\"high\" multiple=\"false\"}\n"+question+"\n:::\n", "bad\x00id"))
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	if recorded := recordedAskAuthors(t, pair.database, pair.artifactID); len(recorded) != 0 {
		t.Fatalf("doc_ask_block_authors holds %+v for a block id Postgres cannot store, want no row", recorded)
	}
	if report := pair.taskA.backfillBlockIDs(context.Background(), pair.artifactID); report.Err != nil || report.Stamped == 0 {
		t.Fatalf("backfill the document's block ids = %+v, want a stamp", report)
	}
	recorded := recordedAskAuthors(t, pair.database, pair.artifactID)
	if len(recorded) != 1 {
		t.Fatalf("doc_ask_block_authors after the stamp = %+v, want the minted id recorded as bob", recorded)
	}
	for minted, author := range recorded {
		if author != bob || !storableAskBlockID(minted) {
			t.Fatalf("doc_ask_block_authors after the stamp = %+v, want the minted id recorded as bob", recorded)
		}
	}
	makeAliceTheLatestEditSource(t, pair.taskA, pair.artifactID)

	settleCurrentGeneration(t, pair.taskB, pair.artifactID)
	if author := askAuthorByQuestion(t, pair.taskB, pair.artifactID, question); author != bob {
		t.Fatalf("task B made the stamped ask with author %+v, want bob", author)
	}
	requireNoRecordedAskAuthors(t, pair.database, pair.artifactID)
}

// Bob types an ask block on task B; then alice, on task A, whose room took the block in through a
// catch-up and so knows no author for it, puts a paragraph carrying the same id ahead of it. Task
// A's settlement stamps the ask a new id, carries bob's recorded author to it from the id it
// replaced (recordStampedAskAuthors), and makes the ask his; the replaced id's row goes with it,
// since no ask block of the settled tree carries it.
func TestASettlementsStampCarriesTheAuthorAnotherProcessRecorded(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	alice := model.Actor{Kind: "user", ID: "alice-on-task-a"}
	const question = "Who signs off on the launch?"
	pair.leaveTaskABehind(t, 1, bob, ":::ask{#shared urgency=\"med\" multiple=\"false\"}\n"+question+"\n:::\n")
	if recorded := recordedAskAuthors(t, pair.database, pair.artifactID); recorded["shared"] != bob {
		t.Fatalf("doc_ask_block_authors after bob's append = %+v, want shared recorded as bob", recorded)
	}
	if outcome, err := pair.taskA.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, pair.taskA.srv.GetDoc(pair.artifactID)); err != nil || outcome != catchUpApplied {
		t.Fatalf("catch task A's room up = %v, %v, want catchUpApplied", outcome, err)
	}
	editAsBrowserPeer(t, pair.taskA, pair.artifactID, 2, alice, prependParagraphWithID(t, "Alice's preface.", "shared"))
	waitForLandedAppends(t, pair.taskA, pair.artifactID)

	settleCurrentGeneration(t, pair.taskA, pair.artifactID)
	if author := askAuthorByQuestion(t, pair.taskA, pair.artifactID, question); author != bob {
		t.Fatalf("task A made the restamped ask with author %+v, want bob, whose author it carried from the replaced id", author)
	}
	requireNoRecordedAskAuthors(t, pair.database, pair.artifactID)
}

// A block deleted before any settlement indexed it leaves its durable author behind until the next
// settlement, which deletes it with the block gone from its tree.
func TestASettlementDeletesTheRecordedAuthorOfABlockGoneFromTheDocument(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-a"}
	editAsBrowserPeer(t, pair.taskA, pair.artifactID, 1, bob,
		appendBlocks(t, ":::ask{#gone-ask urgency=\"low\" multiple=\"false\"}\nStill needed?\n:::\n"))
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	editAsBrowserPeer(t, pair.taskA, pair.artifactID, 1, bob, removeBlockWithID("gone-ask"))
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	if recorded := recordedAskAuthors(t, pair.database, pair.artifactID); recorded["gone-ask"] != bob {
		t.Fatalf("doc_ask_block_authors before the settlement = %+v, want gone-ask still recorded as bob", recorded)
	}

	settleCurrentGeneration(t, pair.taskB, pair.artifactID)
	var asks int
	if err := pair.database.Pool.QueryRow(context.Background(), `
		select count(*) from asks where block_artifact_id = $1
	`, pair.artifactID).Scan(&asks); err != nil {
		t.Fatalf("count the document's asks: %v", err)
	}
	if asks != 0 {
		t.Fatalf("the settlement made %d ask(s) of a deleted block, want none", asks)
	}
	requireNoRecordedAskAuthors(t, pair.database, pair.artifactID)
}

// prependParagraphWithID returns a live edit that puts a paragraph of text carrying block id id
// ahead of every block.
func prependParagraphWithID(t *testing.T, text, id string) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	parsed, err := pmdoc.Parse(text + "\n")
	if err != nil {
		t.Fatalf("parse the prepended paragraph: %v", err)
	}
	paragraph := parsed.Children[0]
	if paragraph.Attrs == nil {
		paragraph.Attrs = pmdoc.Attrs{}
	}
	paragraph.Attrs[pmdoc.BlockIDAttr] = id
	return func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = append([]*pmdoc.Node{paragraph}, tree.Children...)
		return tree
	}
}

// removeBlockWithID returns a live edit that deletes the top-level block carrying block id id.
func removeBlockWithID(id string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		kept := tree.Children[:0]
		for _, child := range tree.Children {
			if held, _ := child.Attrs[pmdoc.BlockIDAttr].(string); held != id {
				kept = append(kept, child)
			}
		}
		tree.Children = kept
		return tree
	}
}
