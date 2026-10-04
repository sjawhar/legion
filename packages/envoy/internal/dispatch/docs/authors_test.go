package docs

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// An edit a browser makes after a settlement has read the document it versions is not in that
// version, so the version does not credit its author, even when the edit's update observer
// credits them before the settlement would have taken its authors: the authors are taken with the
// read. The observer is held once it has credited the edit and before it arms the edit's own
// settlement, so the settlement goes on to version what it read. The version the edit's own
// settlement writes holds the edit and credits its author (LEGION-503).
func TestSettlementCreditsNoAuthorOfAnEditMadeAfterItsRead(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ReplaceText(context.Background(), artifactID, "First, alice.\n\nSecond.\n", alice); err != nil {
		t.Fatalf("alice's edit: %v", err)
	}
	bob := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, 1, bob)

	var release func()
	service.afterSettleRead = func(room string) {
		if room == artifactID && release == nil {
			release = holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, replaceRun("Second.", "Second, bob."))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if release == nil {
		t.Fatal("settlement never reached the window after its read")
	}
	requireLatestVersionMarkdown(t, service, artifactID, "First, alice.\n\nSecond.\n")
	versioned := latestVersionNumber(t, service, artifactID)
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Equal(authors, []model.Actor{alice}) {
		t.Fatalf("version %d authors = %#v, want alice alone: bob's edit is not in it", versioned, authors)
	}

	service.afterSettleRead = nil
	release()
	settleCurrentGeneration(t, service, artifactID)
	requireLatestVersionMarkdown(t, service, artifactID, "First, alice.\n\nSecond, bob.\n")
	if latest := latestVersionNumber(t, service, artifactID); latest != versioned+1 {
		t.Fatalf("latest version = %d, want %d, the edit's own", latest, versioned+1)
	}
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Equal(authors, []model.Actor{bob}) {
		t.Fatalf("version %d authors = %#v, want bob, whose edit it holds", versioned+1, authors)
	}
}

// An edit made after settlement takes its authors and before it copies the tree is in the version
// it copies but is credited after that take, so the version does not credit its author. The edit's
// own settlement sees that version and writes none, leaving the author pending for the next version
// that holds the edit (LEGION-503).
func TestSettlementCreditsAnEditMadeBetweenItsAuthorsAndCopyOnTheNextVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ReplaceText(context.Background(), artifactID, "First, alice.\n\nSecond.\n", alice); err != nil {
		t.Fatalf("alice's edit: %v", err)
	}
	bob := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, 1, bob)

	var release func()
	service.afterSettleAuthorsTake = func(room string) {
		if room == artifactID && release == nil {
			release = holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, replaceRun("Second.", "Second, bob."))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if release == nil {
		t.Fatal("settlement never reached the window between its authors and its copy")
	}
	const both = "First, alice.\n\nSecond, bob.\n"
	requireLatestVersionMarkdown(t, service, artifactID, both)
	versioned := latestVersionNumber(t, service, artifactID)
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Equal(authors, []model.Actor{alice}) {
		t.Fatalf("version %d authors = %#v, want alice alone: bob was credited after the take", versioned, authors)
	}

	service.afterSettleAuthorsTake = nil
	release()
	settleCurrentGeneration(t, service, artifactID)
	if latest := latestVersionNumber(t, service, artifactID); latest != versioned {
		t.Fatalf("latest version = %d, want %d, which already holds bob's edit", latest, versioned)
	}
	carol := model.Actor{Kind: "user", ID: "carol"}
	if _, err := service.ReplaceText(context.Background(), artifactID, both+"\nThird, carol.\n", carol); err != nil {
		t.Fatalf("carol's edit: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Contains(authors, bob) {
		t.Fatalf("version %d authors = %#v, want bob, whose edit it holds", latestVersionNumber(t, service, artifactID), authors)
	}
}

// An author already pending when a settlement takes its authors who edits again before it commits
// is credited on both versions: the settlement's, which holds the first edit, and the one the
// second edit's own settlement writes. The second edit's update observer credits the author under
// the same key before it arms that edit's settlement, and the settlement's commit releases only
// the entry it took, not that newer one (LEGION-503).
func TestSettlementKeepsTheCreditOfAnEditItsAuthorMakesBeforeItCommits(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	bob := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, 1, bob)
	editAsPeer(t, service, artifactID, replaceRun("First.", "First, edited."))

	var release func()
	service.afterSettleReconcile = func(room string) {
		if room == artifactID && release == nil {
			// bob edits again once the settlement has taken its authors. His edit's observer is
			// held once it has credited him.
			release = holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, replaceRun("Second.", "Second, edited."))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if release == nil {
		t.Fatal("settlement never reached the window between its authors and its commit")
	}
	requireLatestVersionMarkdown(t, service, artifactID, "First, edited.\n\nSecond.\n")
	first := latestVersionNumber(t, service, artifactID)
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Contains(authors, bob) {
		t.Fatalf("version %d authors = %#v, want bob, whose first edit it holds", first, authors)
	}

	service.afterSettleReconcile = nil
	release()
	settleCurrentGeneration(t, service, artifactID)
	requireLatestVersionMarkdown(t, service, artifactID, "First, edited.\n\nSecond, edited.\n")
	second := latestVersionNumber(t, service, artifactID)
	if second != first+1 {
		t.Fatalf("latest version = %d, want %d, the second edit's own", second, first+1)
	}
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Contains(authors, bob) {
		t.Fatalf("version %d authors = %#v, want bob, whose second edit it holds", second, authors)
	}
}

// ygo runs a room's update observer, which credits an edit to its authors, only once the edit's
// transaction has released the document, so a settlement can version an edit the room holds before
// its observer has credited it. That version credits the edit to no one, and the edit's own
// settlement finds the document already versioned and writes none. The author stays pending and is
// credited on the next version, which holds the edit too (LEGION-503).
func TestAnEditVersionedBeforeItsObserverCreditsItIsCreditedOnTheNextVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ReplaceText(ctx, artifactID, "First, alice.\n\nSecond.\n", alice); err != nil {
		t.Fatalf("alice's edit: %v", err)
	}
	bob := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, 1, bob)
	release := holdPeerEdit(t, service, artifactID, &service.beforeObserveUpdate, replaceRun("Second.", "Second, bob."))

	// The room holds bob's edit, and its observer has not credited him: the settlement versions
	// the edit and credits alice alone.
	settleCurrentGeneration(t, service, artifactID)
	const both = "First, alice.\n\nSecond, bob.\n"
	requireLatestVersionMarkdown(t, service, artifactID, both)
	versioned := latestVersionNumber(t, service, artifactID)
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Equal(authors, []model.Actor{alice}) {
		t.Fatalf("version %d authors = %#v, want alice alone: bob's observer had not credited him", versioned, authors)
	}

	release()
	// bob's edit's own settlement finds the document versioned and writes no version.
	settleCurrentGeneration(t, service, artifactID)
	if latest := latestVersionNumber(t, service, artifactID); latest != versioned {
		t.Fatalf("latest version = %d, want %d, which already holds bob's edit", latest, versioned)
	}

	carol := model.Actor{Kind: "user", ID: "carol"}
	if _, err := service.ReplaceText(ctx, artifactID, both+"\nThird, carol.\n", carol); err != nil {
		t.Fatalf("carol's edit: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	requireLatestVersionMarkdown(t, service, artifactID, both+"\nThird, carol.\n")
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Contains(authors, bob) {
		t.Fatalf("version %d authors = %#v, want bob, whose edit it holds and no version had credited",
			latestVersionNumber(t, service, artifactID), authors)
	}
}

// A new ask block is attributed to the update that introduced it, as the room's update observer
// records it, whenever in a settlement that update lands. Each case inserts the block between a
// settlement's author take and its copy, with the observer held at hook, then lets it go and
// settles again (LEGION-503).
func TestANewAskIsAttributedToTheUpdateThatIntroducedIt(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	session := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	for _, test := range []struct {
		name string
		// arrange leaves the room as the case needs it before the settlement: who edited last,
		// and who is connected when the ask block arrives.
		arrange func(t *testing.T, service *Service, artifactID string)
		// observer is the observer hook the browser's update is held at: before the observer
		// runs at all, or once it has recorded and credited the update.
		observer func(service *Service) *func(room string)
		want     model.Actor
	}{
		{
			// The only connected browser inserts the block, and its observer records and credits
			// it before the settlement copies the tree (Deep's round-3 finding 2).
			name: "a sole browser's block",
			arrange: func(t *testing.T, service *Service, artifactID string) {
				service.addConnection(artifactID, 1, bob)
			},
			observer: func(service *Service) *func(room string) { return &service.afterCreditUpdate },
			want:     bob,
		},
		{
			// The settlement copies the block before its update observer has run, so nothing has
			// recorded who wrote it. The settlement leaves it to the settlement that observer
			// arms, which knows (Deep's and Qual's probe A).
			name: "a block whose observer has not run",
			arrange: func(t *testing.T, service *Service, artifactID string) {
				service.addConnection(artifactID, 1, bob)
			},
			observer: func(service *Service) *func(room string) { return &service.beforeObserveUpdate },
			want:     bob,
		},
		{
			// Two people are connected, so the update cannot be pinned on either. Alice, who
			// edited before the take, is the settlement's own actor and must not stand in
			// (Deep's probe B, Qual's probe C, Acceptance's ambiguous-ask test).
			name: "an update two browsers could have sent",
			arrange: func(t *testing.T, service *Service, artifactID string) {
				service.addConnection(artifactID, 1, alice)
				editAsPeer(t, service, artifactID, replaceRun("First.", "First, alice."))
				service.addConnection(artifactID, 2, bob)
			},
			observer: func(service *Service) *func(room string) { return &service.afterCreditUpdate },
			want:     SettlementActor,
		},
		{
			// An agent's edit leaves its session the room's latest editor, and the settlement's
			// actor. A browser's block, sent while two people are connected, is not the session's,
			// which would follow a person's question (Acceptance's agent test).
			name: "an update two browsers could have sent after an agent's edit",
			arrange: func(t *testing.T, service *Service, artifactID string) {
				if _, err := service.ReplaceText(context.Background(), artifactID, "First, agent.\n\nSecond.\n", session); err != nil {
					t.Fatalf("agent edit: %v", err)
				}
				service.addConnection(artifactID, 1, alice)
				service.addConnection(artifactID, 2, bob)
			},
			observer: func(service *Service) *func(room string) { return &service.afterCreditUpdate },
			want:     SettlementActor,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
			settleCurrentGeneration(t, service, artifactID)
			test.arrange(t, service, artifactID)

			var release func()
			service.afterSettleAuthorsTake = func(room string) {
				if room == artifactID && release == nil {
					release = holdPeerEdit(t, service, artifactID, test.observer(service), appendBlocks(t, ":::ask{#window-ask urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n"))
				}
			}
			settleCurrentGeneration(t, service, artifactID)
			if release == nil {
				t.Fatal("settlement never reached the window between its authors and its copy")
			}
			service.afterSettleAuthorsTake = nil
			release()
			settleCurrentGeneration(t, service, artifactID)
			if author := blockAskAuthor(t, service, artifactID, "window-ask"); author != test.want {
				t.Fatalf("ask author = %#v, want %v", author, test.want)
			}
			if actor := blockAskOpenedActor(t, service, artifactID, "window-ask"); actor != test.want {
				t.Fatalf("ask.opened actor = %#v, want %v", actor, test.want)
			}
			if blockAskFollows(t, service, artifactID, "window-ask", session.ID) && test.want != session {
				t.Fatalf("session %q follows an ask it did not write", session.ID)
			}
		})
	}
}

// An ask block in the room before a settlement takes its authors keeps the author its update
// recorded when someone else edits in the take-to-copy window: Alice pasted it while she was the
// room's only editor and left, and Bob, alone after her, types in that window (Qual's probe B,
// Deep's probe E, Acceptance's present-before-the-take test).
func TestAnAskInTheRoomBeforeASettlementsTakeKeepsItsAuthor(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, 1, alice)
	editAsPeer(t, service, artifactID, appendBlocks(t, ":::ask{#alice-ask urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n"))
	service.removeConnection(artifactID, 1)
	service.addConnection(artifactID, 2, bob)

	var release func()
	service.afterSettleAuthorsTake = func(room string) {
		if room == artifactID && release == nil {
			release = holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, replaceRun("First.", "First, bob."))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if release == nil {
		t.Fatal("settlement never reached the window between its authors and its copy")
	}
	service.afterSettleAuthorsTake = nil
	release()
	settleCurrentGeneration(t, service, artifactID)
	if author := blockAskAuthor(t, service, artifactID, "alice-ask"); author != alice {
		t.Fatalf("ask author = %#v, want %v, whose browser pasted it before the settlement took its authors", author, alice)
	}
	if actor := blockAskOpenedActor(t, service, artifactID, "alice-ask"); actor != alice {
		t.Fatalf("ask.opened actor = %#v, want %v", actor, alice)
	}
}

// currentLiveMarkdown is the room's live document as rendered right now, read directly rather than
// through the room's own (possibly stale) contentMarkdown bookkeeping.
func currentLiveMarkdown(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	tree, err := lockedTreeOf(service.srv.GetDoc(artifactID))
	if err != nil {
		t.Fatalf("read live tree: %v", err)
	}
	markdown, err := documentMarkdown(tree)
	if err != nil {
		t.Fatalf("render live tree: %v", err)
	}
	return markdown
}

// holdNextObserver runs write on a goroutine of its own and returns once its mutation has reached
// the room - and its update observer has reached beforeObserveUpdate, held there - so write is in
// the live document but not yet reflected in the room's rendered replica or its ask-block
// bookkeeping. release lets the held observer go on and returns once write has finished.
func holdNextObserver(t *testing.T, service *Service, artifactID string, write func() error) (release func()) {
	t.Helper()
	held := make(chan struct{})
	proceed := make(chan struct{})
	let := sync.OnceFunc(func() { close(proceed) })
	t.Cleanup(let)
	var holding atomic.Bool
	holding.Store(true)
	service.beforeObserveUpdate = func(name string) {
		if name == artifactID && holding.CompareAndSwap(true, false) {
			close(held)
			<-proceed
		}
	}
	done := make(chan error, 1)
	go func() { done <- write() }()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the held write never reached its observer hook")
	}
	return func() {
		t.Helper()
		let()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("held write: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the held write never finished applying")
		}
	}
}

// A service write (ReplaceText, never inside a transaction) lands and is held at its own update
// observer - its mutation is in the room, but the room's rendered replica has not caught up to it
// yet. A browser's edit then lands and is observed immediately: that observer's catch-up absorbs
// the still-unobserved service write's change too, which a render keyed on whichever observer's
// own origin happens to run first would misattribute to the browser (round 5's regression). Each
// block still goes to its own author - the service write's actor from write-site registration
// (registerAskAuthors), the browser's from the room's one connected peer - because attribution no
// longer depends on which update's observer renders the merged catch-up (LEGION-503).
func TestAServiceWriteAndABrowserEditLandTogetherEachAskGoesToItsOwnAuthor(t *testing.T) {
	bob := model.Actor{Kind: "user", ID: "bob"}
	session := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	service.addConnection(artifactID, 1, bob)

	current := currentLiveMarkdown(t, service, artifactID)
	agentAsk := "\n:::ask{#agent-ask urgency=\"high\" multiple=\"false\"}\nWhich database?\n:::\n"
	release := holdNextObserver(t, service, artifactID, func() error {
		_, err := service.ReplaceText(context.Background(), artifactID, current+agentAsk, session)
		return err
	})
	room := service.srv.GetDoc(artifactID)
	_, update := peerEdit(t, room, appendBlocks(t, ":::ask{#bob-ask urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n"))
	if err := crdt.ApplyUpdateV1(room, update, "peer"); err != nil {
		t.Fatalf("apply bob's edit: %v", err)
	}
	release()
	settleCurrentGeneration(t, service, artifactID)
	settleCurrentGeneration(t, service, artifactID)
	if author := blockAskAuthor(t, service, artifactID, "bob-ask"); author != bob {
		t.Errorf("bob-ask author = %#v, want %v", author, bob)
	}
	if author := blockAskAuthor(t, service, artifactID, "agent-ask"); author != session {
		t.Errorf("agent-ask author = %#v, want %v", author, session)
	}
}

// As above, but the service write is a committed transaction's live write, published with
// liveWriteOrigin (the API-shaped case LEGION-503 reports): an agent's own ask was attributed to a
// concurrently editing browser user, and the agent never followed its own question.
func TestAPublishedAgentWriteAndABrowserEditLandTogetherEachAskGoesToItsOwnAuthor(t *testing.T) {
	bob := model.Actor{Kind: "user", ID: "bob"}
	session := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	service.addConnection(artifactID, 1, bob)

	current := currentLiveMarkdown(t, service, artifactID)
	agentAsk := "\n:::ask{#agent-ask urgency=\"high\" multiple=\"false\"}\nWhich database?\n:::\n"
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	joined, ledger := service.Join(context.Background(), tx)
	defer ledger.Discard()
	if _, err := service.ReplaceText(joined, artifactID, current+agentAsk, session); err != nil {
		t.Fatalf("agent joined edit: %v", err)
	}
	release := holdNextObserver(t, service, artifactID, func() error {
		return ledger.Commit(context.Background())
	})
	room := service.srv.GetDoc(artifactID)
	_, update := peerEdit(t, room, appendBlocks(t, ":::ask{#bob-ask urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n"))
	if err := crdt.ApplyUpdateV1(room, update, "peer"); err != nil {
		t.Fatalf("apply bob's edit: %v", err)
	}
	release()
	settleCurrentGeneration(t, service, artifactID)
	settleCurrentGeneration(t, service, artifactID)
	if author := blockAskAuthor(t, service, artifactID, "bob-ask"); author != bob {
		t.Errorf("bob-ask author = %#v, want %v", author, bob)
	}
	if author := blockAskAuthor(t, service, artifactID, "agent-ask"); author != session {
		t.Errorf("agent-ask author = %#v, want %v", author, session)
	}
	if !blockAskFollows(t, service, artifactID, "agent-ask", session.ID) {
		t.Errorf("session does not follow its own ask")
	}
}

// appendBlockWithCraftedID returns a live edit that appends the block markdown parses, like
// appendBlocks, but with its block id overwritten to id - a value the browser editor itself would
// never produce (one a hand-built client sent, or one Postgres cannot store), for a test of the
// id-repair path rather than the browser editor's own stamping.
func appendBlockWithCraftedID(t *testing.T, markdown, id string) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	parsed, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse appended block: %v", err)
	}
	pmdoc.EnsureBlockIDs(parsed)
	parsed.Children[len(parsed.Children)-1].Attrs[pmdoc.BlockIDAttr] = id
	return func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = append(tree.Children, parsed.Children...)
		return tree
	}
}

// askAuthorByQuestion is the author asks records for the one open ask whose question is question,
// for a test that cannot know a repaired block's minted id ahead of time.
func askAuthorByQuestion(t *testing.T, service *Service, artifactID, question string) model.Actor {
	t.Helper()
	var raw []byte
	if err := service.store.Pool.QueryRow(context.Background(), `
		select author from asks where block_artifact_id = $1 and question = $2
	`, artifactID, question).Scan(&raw); err != nil {
		t.Fatalf("read ask %q author: %v", question, err)
	}
	var author model.Actor
	if err := json.Unmarshal(raw, &author); err != nil {
		t.Fatalf("decode ask author: %v", err)
	}
	return author
}

// ApplyOps's own block-id repair (EnsureBlockIDs, stamping the tree before it applies the edit's
// own operations) can rename an existing, unrelated browser block whose id Postgres cannot store -
// one an agent's edit elsewhere in the document never touches - and carries forward the author
// recorded for the id it replaces (carryForwardStampedAskAuthors), the same rule a settlement's or
// the backfill's own stamp applies, instead of crediting the renamed block to the edit's own actor
// (LEGION-503).
func TestApplyOpsOwnBlockIDRepairCarriesForwardTheRenamedBlocksAuthor(t *testing.T) {
	bob := model.Actor{Kind: "user", ID: "bob"}
	session := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	service.addConnection(artifactID, 1, bob)
	editAsPeer(t, service, artifactID, appendBlockWithCraftedID(t,
		":::ask{urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n", "x\x00"))
	service.removeConnection(artifactID, 1)

	if _, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{
		{Op: "insert", After: "end", Markdown: "An agent's line."},
	}, session, nil); err != nil {
		t.Fatalf("agent edit: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	if author := askAuthorByQuestion(t, service, artifactID, "Which transport?"); author != bob {
		t.Errorf("renamed ask author = %#v, want %v, the browser who wrote it before the agent's unrelated edit repaired its id", author, bob)
	}
}

// holdPeerEdit applies a browser's edit to the room on a goroutine of its own and returns once
// the edit's update observer reaches hook, one of the observer's test hooks, which holds it there.
// release lets the observer go on and returns once the edit has finished applying.
func holdPeerEdit(t *testing.T, service *Service, artifactID string, hook *func(room string), edit func(*pmdoc.Node) *pmdoc.Node) (release func()) {
	t.Helper()
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("document room is not resident")
	}
	_, update := peerEdit(t, room, edit)
	held := make(chan struct{})
	proceed := make(chan struct{})
	let := sync.OnceFunc(func() { close(proceed) })
	t.Cleanup(let)
	var holding atomic.Bool
	holding.Store(true)
	*hook = func(name string) {
		if name == artifactID && holding.CompareAndSwap(true, false) {
			close(held)
			<-proceed
		}
	}
	applied := make(chan error, 1)
	go func() { applied <- crdt.ApplyUpdateV1(room, update, "peer") }()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Error("the peer's edit never reached its update observer's hook")
	}
	return func() {
		t.Helper()
		let()
		select {
		case err := <-applied:
			if err != nil {
				t.Fatalf("apply the peer's edit: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the peer's edit never finished applying")
		}
	}
}
