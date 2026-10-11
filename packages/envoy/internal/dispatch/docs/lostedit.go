package docs

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A write to a live document is computed on its transaction's fork of the room, so a change a
// browser makes while the write is in flight merges with it rather than blocking it. The document
// outcome of that merge is correct CRDT behaviour: a human's deletion wins over an agent's edit
// inside the deleted text. What is not correct is telling the agent its edit applied. The merge
// annihilates the change - pmdoc.Update splices a paragraph's text in place, a browser's paragraph
// delete is an element delete, and ygo's delete cascades over the element's children, so whichever
// side merges first the inserted run ends up tombstoned or live under a tombstoned element - and
// the write still committed, versioned and returned 200 (LEGION-269).
//
// lossCheck is what a write carries so it can tell. It records the text the write's own Yjs
// update inserted, by the block each run landed in, and which operation wrote each of those
// blocks. Read again after a merge, it answers whether every inserted run is still live inside an
// element carrying the block id it was written into, and names the operations whose text is not.
//
// Two windows need the answer, both on the same write:
//
//   - Before the version is rendered, on the transaction's fork, once captureLiveTextAndAuthors
//     has brought it up to date with the room. A loss there refuses the whole batch
//     (ErrEditLost), and the transaction's rollback leaves no version, durable update, room
//     update or broadcast behind.
//   - After the committed write is published to the room, where a deletion that landed between
//     the version render and the publish shows up. Nothing can be undone by then, so the write
//     reports the loss on its 200 instead (Ledger.LostOps).
type lossCheck struct {
	// client authors every item the write's transaction inserts (liveWrite.clientID), and since
	// is the clock it had reached before this batch, so its own earlier writes are not read as
	// this batch's insertions.
	client crdt.ClientID
	since  uint64
	// blocks is, per operation index, the blocks whose own inline content that operation wrote.
	blocks []map[string]struct{}
	// inserted is the text this write's update inserted and left live, by the block holding it,
	// restricted to the blocks some operation claims.
	inserted pmdoc.AuthoredText
	// suggestion names the accepted comment when the write is a suggestion accept rather than a
	// batch of operations, which has one implicit operation and reports no index.
	suggestion string
}

// newLossCheck records what a write inserted. inserted is every run the write's update added, by
// the block holding it; blocks says which operation wrote each of those blocks, so every run left
// is one a lost verdict can name.
func newLossCheck(client crdt.ClientID, since uint64, blocks []map[string]struct{}, inserted pmdoc.AuthoredText) *lossCheck {
	claimed := pmdoc.AuthoredText{}
	for _, written := range blocks {
		for block := range written {
			if runs, wrote := inserted[block]; wrote {
				claimed[block] = runs
			}
		}
	}
	if len(claimed) == 0 {
		return nil
	}
	return &lossCheck{client: client, since: since, blocks: blocks, inserted: claimed}
}

// authoredClock is the clock the calling transaction's write to artifactID has reached on doc,
// which is the line between the text its next update inserts and everything it wrote before.
// It reads the document's state vector, which takes the document lock, so callers take it before
// they open their Yjs transaction.
func authoredClock(ctx context.Context, artifactID string, doc *crdt.Doc) uint64 {
	live := joinedLiveWrite(ctx, artifactID)
	if live == nil || doc == nil {
		return 0
	}
	return doc.StateVector()[live.clientID]
}

// recordInsertedText runs write, the Yjs update one batch of operations makes, and records on the
// calling transaction's live write what that update inserted and where. since is the clock that
// write had reached beforehand (authoredClock, taken outside the Yjs transaction), and suggestion
// names the accepted comment when the write is an accept rather than a batch, which has one
// implicit operation. Outside a transaction there is no fork and no window: the mutation reaches
// the room inside its own apply, with no later merge to survive, so there is nothing to record.
//
// The Yjs update decides which blocks are in play, and only then does blocks say which operation
// wrote each of them - a batch that inserted nothing asks nothing of the tree at all. The walk is
// lock-free, so the conditional edit path can call this from inside the Yjs transaction it holds
// for the whole batch.
func recordInsertedText(
	ctx context.Context,
	artifactID, suggestion string,
	fragment *crdt.YXmlFragment,
	since uint64,
	blocks func(candidates map[string]struct{}) []map[string]struct{},
	write func() error,
) error {
	live := joinedLiveWrite(ctx, artifactID)
	if live == nil {
		return write()
	}
	if err := write(); err != nil {
		return err
	}
	inserted, err := pmdoc.AuthoredTextRuns(fragment, live.clientID, since, nil)
	if err != nil {
		return documentSchemaError(err)
	}
	if len(inserted) == 0 {
		return nil
	}
	candidates := make(map[string]struct{}, len(inserted))
	for block := range inserted {
		candidates[block] = struct{}{}
	}
	if check := newLossCheck(live.clientID, since, blocks(candidates), inserted); check != nil {
		check.suggestion = suggestion
		live.loss = check
	}
	return nil
}

// refuseLostWrite refuses the calling transaction's write to room when the room's concurrent
// changes have already removed the text it inserted, before any version records it: LEGION-269's
// first window. fork is the transaction's document brought up to date with the room (joinRead);
// a caller with no write of its own, or whose write inserted nothing, has nothing to lose.
func (s *Service) refuseLostWrite(ctx context.Context, room string, fork *crdt.Doc, actor *model.Actor) error {
	write := joinedLiveWrite(ctx, room)
	if write == nil || write.loss == nil || fork == nil {
		return nil
	}
	lost, err := write.loss.lost(fork)
	if err != nil {
		return err
	}
	if len(lost) == 0 {
		return nil
	}
	credited := model.Actor{}
	if actor != nil {
		credited = *actor
	}
	state := s.lockState(room)
	participants := otherParticipants(state, credited)
	s.unlockState(room, state)
	refusal := &ErrEditLost{Suggestion: write.loss.suggestion, Participants: participants}
	if refusal.Suggestion == "" {
		refusal.Ops = lost
	}
	return refusal
}

// lost names the operations whose inserted text doc no longer holds in the block it was written
// into: deleted, left under a deleted ancestor, or moved into a different block by a concurrent
// browser move. It reads only the blocks the write inserted into, so an uncontended edit on a
// large document pays for its own paragraph rather than for every one. A doc holding a tree past
// the schema's depth bound is ErrDocSchema: what it holds cannot be read.
func (c *lossCheck) lost(doc *crdt.Doc) ([]int, error) {
	if c == nil || doc == nil {
		return nil, nil
	}
	written := make(map[string]struct{}, len(c.inserted))
	for block := range c.inserted {
		written[block] = struct{}{}
	}
	held, err := pmdoc.AuthoredTextRuns(doc.GetXmlFragment(fragmentName), c.client, c.since, written)
	if err != nil {
		return nil, documentSchemaError(err)
	}
	missing := c.inserted.Missing(held)
	if len(missing) == 0 {
		return nil, nil
	}
	var lost []int
	for index, blocks := range c.blocks {
		for block := range blocks {
			if _, gone := missing[block]; gone {
				lost = append(lost, index)
				break
			}
		}
	}
	sort.Ints(lost)
	return lost, nil
}

// ErrEditLost refuses a write whose text a concurrent change removed before the write's version
// was rendered. The document outcome stands - the other change wins - so the refusal is the whole
// batch: nothing of it is written, and the caller re-reads the document and decides again.
type ErrEditLost struct {
	// Ops are the operations whose text did not survive, by index in the request's batch.
	Ops []int
	// Suggestion names the accepted comment when the refused write is an accept, which has no
	// operation index.
	Suggestion string
	// Participants are the room's other connected browsers at the moment of the check. The room
	// tracks a set, so a loss names every peer that could have caused it rather than a culprit.
	Participants []model.Actor
}

func (e *ErrEditLost) Error() string {
	subject := "the accepted suggestion's text"
	if e.Suggestion == "" {
		subject = fmt.Sprintf("the text %s wrote", operationList(e.Ops))
	}
	return fmt.Sprintf(
		"%s was removed by a concurrent change to the document before this write could be versioned, so nothing was written%s; re-read the document and decide again",
		subject, participantList(e.Participants),
	)
}

func operationList(ops []int) string {
	if len(ops) == 1 {
		return fmt.Sprintf("operation %d", ops[0])
	}
	names := make([]string, 0, len(ops))
	for _, op := range ops {
		names = append(names, fmt.Sprint(op))
	}
	return "operations " + strings.Join(names, ", ")
}

func participantList(participants []model.Actor) string {
	if len(participants) == 0 {
		return ""
	}
	names := make([]string, 0, len(participants))
	for _, participant := range participants {
		names = append(names, participant.Kind+":"+participant.ID)
	}
	sort.Strings(names)
	return " (the document's other participants: " + strings.Join(names, ", ") + ")"
}

// otherParticipants are the room's connected peers other than actor, whom a lost write names.
// The caller holds state.mu.
func otherParticipants(state *roomState, actor model.Actor) []model.Actor {
	participants := make([]model.Actor, 0, len(state.connected))
	for _, connected := range state.connected {
		if connected == actor {
			continue
		}
		participants = append(participants, connected)
	}
	return participants
}
