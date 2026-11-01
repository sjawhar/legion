package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// askBlockIDs collects the id of every ask block tree holds. It never returns nil: a document
// with none still returns an empty map, which askBlockSources.author reads as "no block is
// unindexed" rather than as an unknown baseline (askBlockSources.observed).
func askBlockIDs(tree *pmdoc.Node) map[string]struct{} {
	var ids map[string]struct{}
	walkAskBlocks(tree, func(id string) {
		if id == "" {
			return
		}
		if ids == nil {
			ids = make(map[string]struct{})
		}
		ids[id] = struct{}{}
	})
	if ids == nil {
		return map[string]struct{}{}
	}
	return ids
}

// askBlockOrder lists the id of every ask block tree holds in document order, "" for a block
// with none.
func askBlockOrder(tree *pmdoc.Node) []string {
	var ids []string
	walkAskBlocks(tree, func(id string) { ids = append(ids, id) })
	return ids
}

// walkAskBlocks calls visit with the id of each ask block tree holds, in document order, wherever
// it stands: in a blockquote, a list item or another typed block. It does not descend into text,
// which holds no block.
func walkAskBlocks(tree *pmdoc.Node, visit func(id string)) {
	if tree == nil {
		return
	}
	if tree.Type == "ask" {
		id, _ := tree.Attrs[pmdoc.BlockIDAttr].(string)
		visit(id)
	}
	for _, child := range tree.Children {
		if child.Type != "text" {
			walkAskBlocks(child, visit)
		}
	}
}

// stampedAsk is an ask block a block-id stamp gave a new id: previous, the id it had ("" for
// none), minted, the one it has, and copied, whether another block still holds previous - a repeat
// the stamp re-minted, a copy of the block that keeps the id.
type stampedAsk struct {
	previous, minted string
	copied           bool
}

// stampedAskBlocks pairs the ask block ids of one tree before and after a block-id stamp, which
// changes ids and nothing else, and returns each ask whose id the stamp changed.
func stampedAskBlocks(before, after []string) []stampedAsk {
	var held map[string]struct{}
	var stamped []stampedAsk
	for index, previous := range before {
		if after[index] == previous {
			continue
		}
		if held == nil {
			held = make(map[string]struct{}, len(after))
			for _, id := range after {
				held[id] = struct{}{}
			}
		}
		_, copied := held[previous]
		stamped = append(stamped, stampedAsk{previous: previous, minted: after[index], copied: copied})
	}
	return stamped
}

// recordStampedAskBlocks records ask blocks whose ids a settlement's own stamp minted. The stamp's
// own transaction is suppressed, so no update it fires itself goes through the room's regular
// observer - but the next observer to render does catch the room up past the stamp, and its own
// tree walk, run outside state.mu, can straddle it: read askBlockIDs before the stamp lands, then
// relock and write state.askBlocks back after, wiping the seen mark this records. Each renamed
// block is still marked seen directly, for the ordinary case where no straddle happens, unless it
// is a copy of the block that keeps its previous id - a live block still carries that id, so it
// must stay in state.askBlocks rather than be forgotten as retired. The author recorded for the id
// a non-copied rename replaces is carried to the minted one in both state.askAuthors, for the
// ordinary case, and state.pendingAskAuthors, which a straddling observer's own "newly seen"
// branch still finds and correctly restores even when its stale read wiped the seen mark
// (observeAskBlocks consumes a pending entry for any id it sees, seen or newly seen, so one this
// writes is never orphaned either). It returns the author it carried to each minted
// id, which the caller records durably in the transaction that stores the stamp
// (recordStampedAskAuthors). The caller holds state.mu.
func (state *roomState) recordStampedAskBlocks(stamped []stampedAsk) map[string]model.Actor {
	var carried map[string]model.Actor
	for _, ask := range stamped {
		if state.askBlocks != nil {
			state.askBlocks[ask.minted] = struct{}{}
			if !ask.copied {
				delete(state.askBlocks, ask.previous)
			}
		}
		author, recorded := state.askAuthors[ask.previous]
		if !recorded || ask.copied {
			delete(state.askAuthors, ask.minted)
			continue
		}
		state.askAuthors[ask.minted] = author
		if state.pendingAskAuthors == nil {
			state.pendingAskAuthors = make(map[string]model.Actor)
		}
		state.pendingAskAuthors[ask.minted] = author
		if carried == nil {
			carried = make(map[string]model.Actor, len(stamped))
		}
		carried[ask.minted] = author
	}
	return carried
}

// registerPendingAskAuthor credits actor as the author of id in state.pendingAskAuthors, unless
// an id already registered there has one: first-registration-wins, since a more specific
// registration (a stamp's own carry-forward, registerCarriedAskAuthors) must not be overwritten
// by a less specific one (registerAskAuthors) registered after it within the same write. The
// caller holds state.mu.
func (state *roomState) registerPendingAskAuthor(id string, actor model.Actor) {
	if _, registered := state.pendingAskAuthors[id]; registered {
		return
	}
	if state.pendingAskAuthors == nil {
		state.pendingAskAuthors = make(map[string]model.Actor)
	}
	state.pendingAskAuthors[id] = actor
}

// registerAskAuthors credits actor as the author of every id in ids a committed transaction's
// write or a service mutation introduces, computed from its own before/after trees at the write
// site rather than guessed from whichever update's observer later renders a catch-up that happens
// to include it: the room's update observer consumes each entry here the first time it sees the
// id (observeAskBlocks), and never prunes one it has not consumed, so attribution survives however
// many other updates land first (LEGION-503). It never overwrites an id a more specific
// registration already gave an author - a stamp's own carry-forward (registerCarriedAskAuthors),
// which the caller registers first within the same write. The caller holds state.mu.
func (state *roomState) registerAskAuthors(ids map[string]struct{}, actor model.Actor) {
	for id := range ids {
		state.registerPendingAskAuthor(id, actor)
	}
}

// carriedAskAuthors is the author recorded for each renamed ask block's previous id that has one,
// for a stamp's rename list: the same rule a settlement's or the backfill's own suppressed stamp
// applies, so an edit elsewhere in the document cannot take credit for a browser's re-minted ask
// id. A rename with no author to carry, or one that is a copy of the block that keeps its
// previous id, has none here, and falls to the observer's own fallback instead. This only reads
// state.askAuthors; it does not write state.pendingAskAuthors, since the write whose stamp
// produced stamped might still be refused after this call returns (growth, a later validation
// failure), and a refused write must not leave the room holding a trace of it (LEGION-503) - the
// caller stages the result on its own liveWrite and registers it only once the write is certain to
// land (registerCarriedAskAuthors, at commit). The caller holds state.mu.
func (state *roomState) carriedAskAuthors(stamped []stampedAsk) map[string]model.Actor {
	var carried map[string]model.Actor
	for _, ask := range stamped {
		author, recorded := state.askAuthors[ask.previous]
		if !recorded || ask.copied {
			continue
		}
		if carried == nil {
			carried = make(map[string]model.Actor, len(stamped))
		}
		carried[ask.minted] = author
	}
	return carried
}

// registerCarriedAskAuthors credits each id in authors (carriedAskAuthors's result, staged on a
// write's own liveWrite until the write is certain to land) with its carried-forward author, the
// same first-registration-wins rule registerAskAuthors applies for an id a write's own
// before/after diff introduces. The caller holds state.mu.
func (state *roomState) registerCarriedAskAuthors(authors map[string]model.Actor) {
	for id, author := range authors {
		state.registerPendingAskAuthor(id, author)
	}
}

// observeAskBlocks records ids, the ask blocks the room holds after an update its observer
// rendered, and attributes each one not already recorded: first to whichever write registered it
// (registerAskAuthors, registerCarriedAskAuthors, recordStampedAskBlocks - consumed here the
// first time its id is seen in a render, seen or newly seen, and never pruned otherwise, since no
// render order can tell which write introduced a block another update's catch-up happens to
// include first, or guarantee a straddling render has not wiped the room's own record that it was
// already seen), otherwise, for an id newly seen with no write's registration, to author, the room's
// one connected browser or SettlementActor when several are, nil for none - an id no write
// registered can only have come from a browser, whichever update's own observer ends up being the
// one to see it (LEGION-503). It forgets the author of a block the room no longer holds. A room
// whose earlier ask blocks are unknown - its load could not read the document - takes ids as its
// baseline and records no author at all, since it cannot tell which blocks are new; its caller
// passes author as nil for the same reason on a fresh load.
//
// It returns each id it named author for, which no write registered: the update's append records
// them durably in its own transaction (UpdateCredit.askAuthors, recordAskAuthors), so a settlement
// on another process, or after this one stops, still finds who wrote the block. A registered id is
// recorded by the transaction of the write that registered it. The caller holds state.mu.
func (state *roomState) observeAskBlocks(ids map[string]struct{}, author *model.Actor) map[string]model.Actor {
	for id := range state.askAuthors {
		if _, held := ids[id]; !held {
			delete(state.askAuthors, id)
		}
	}
	for id := range ids {
		registered, pending := state.pendingAskAuthors[id]
		if !pending {
			continue
		}
		delete(state.pendingAskAuthors, id)
		if state.askAuthors == nil {
			state.askAuthors = make(map[string]model.Actor)
		}
		state.askAuthors[id] = registered
	}
	var attributed map[string]model.Actor
	if state.askBlocks != nil && author != nil {
		for id := range ids {
			if _, seen := state.askBlocks[id]; seen {
				continue
			}
			if _, already := state.askAuthors[id]; already {
				continue
			}
			if state.askAuthors == nil {
				state.askAuthors = make(map[string]model.Actor)
			}
			state.askAuthors[id] = *author
			if attributed == nil {
				attributed = make(map[string]model.Actor)
			}
			attributed[id] = *author
		}
	}
	state.askBlocks = ids
	return attributed
}

// askBlockSources is where the ask blocks a settlement indexes came from, copied from the room's
// state once the settlement holds the tree it reconciles: observed, the ask blocks the room's
// update observer has seen (nil when it cannot tell), and authors, the author of the update that
// introduced each block not yet indexed. recorded is the durable record of the same authors
// (doc_ask_block_authors), which the settlement reads in its own transaction (reconcileAskBlocks):
// it holds the author of a block another process's write introduced, or one this room loaded with.
type askBlockSources struct {
	observed map[string]struct{}
	authors  map[string]model.Actor
	recorded map[string]model.Actor
}

// askSources copies the room's ask-block sources for a settlement. The caller holds state.mu.
func (state *roomState) askSources() askBlockSources {
	return askBlockSources{observed: maps.Clone(state.askBlocks), authors: maps.Clone(state.askAuthors)}
}

// author names who wrote the ask block carrying id, which no ask row indexes yet: the author of
// the update that introduced it, as the update observer recorded it, or, for a block a settlement's
// own id-repair renamed, the author carried forward from the id it replaced
// (recordStampedAskBlocks); else the author the durable record holds for it, which the write that
// introduced it recorded on whichever process made it. ok is false for a block neither names that
// the observer has not seen - its update is in the tree, but its observer has not run - which that
// observer records, arming the settlement that indexes it. A block the observer saw that neither
// names - one the room held when it loaded from a store no release recorded its author in, one an
// update with no source introduced, or one a stamp renamed while its own update's observer was
// still behind, so no author was there to carry forward - is fallback's, named after the
// settlement's own actor rather than its original editor.
func (sources askBlockSources) author(id string, fallback model.Actor) (model.Actor, bool) {
	if actor, recorded := sources.authors[id]; recorded {
		return actor, true
	}
	if actor, recorded := sources.recorded[id]; recorded {
		return actor, true
	}
	if sources.observed != nil {
		if _, seen := sources.observed[id]; !seen {
			return model.Actor{}, false
		}
	}
	return fallback, true
}

// The durable ask authors (doc_ask_block_authors, migration 0159) are the authors of the ask blocks
// no settlement has indexed yet, one row per block, outliving the room and the process that saw the
// write: a browser's block is recorded by the append of the update whose render first showed it
// (appendUpdateTxClass), a joined write's or a seed's by the transaction that commits it
// (Ledger.recordSettlementCredit), and a block a stamp renamed by the transaction that stores the
// stamp (recordStampedAskAuthors). The first registration wins. A settlement deletes the rows of the
// blocks it indexed or found indexed and of the blocks its tree no longer holds
// (deleteSettledAskAuthors). Every write holds the document's advisory lock. A block is keyed by
// its id's SHA-256 (block_id_key), so an id of any length has its row. An id PostgreSQL's text
// cannot hold (storableAskBlockID) has no row: every stamp mints it again, and the stamp records
// the author it carries to the minted id.

// storableAskBlockID reports whether id can name a durable row, as pmdoc's block-id repair keeps
// it: any text but the empty string and one holding U+0000.
func storableAskBlockID(id string) bool {
	return id != "" && strings.IndexByte(id, 0) < 0
}

// recordAskAuthors records authors, keyed by block id, in tx, keeping the author an earlier
// registration of a block already recorded.
func recordAskAuthors(ctx context.Context, tx pgx.Tx, room string, authors map[string]model.Actor) error {
	blocks := make([]string, 0, len(authors))
	encoded := make([]string, 0, len(authors))
	for block, actor := range authors {
		if !storableAskBlockID(block) {
			continue
		}
		raw, err := json.Marshal(actor)
		if err != nil {
			return fmt.Errorf("encode an ask block's author: %w", err)
		}
		blocks = append(blocks, block)
		encoded = append(encoded, string(raw))
	}
	if len(blocks) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		insert into doc_ask_block_authors (artifact_id, block_id, actor)
		select $1::uuid, block, actor from unnest($2::text[], $3::jsonb[]) as author(block, actor)
		on conflict (artifact_id, block_id_key(block_id)) do nothing
	`, room, blocks, encoded); err != nil {
		return fmt.Errorf("record the document's ask block authors: %w", err)
	}
	return nil
}

// recordStampedAskAuthors records, in tx, the transaction that stores a stamp, the author of each
// ask block the stamp renamed: carried, the author the room carried to the minted id
// (recordStampedAskBlocks), and otherwise the author recorded for the id it replaced, which another
// process's write can have recorded where this room never saw it. A copy of a block that keeps its
// previous id carries nothing, as in the room.
func recordStampedAskAuthors(ctx context.Context, tx pgx.Tx, room string, stamped []stampedAsk, carried map[string]model.Actor) error {
	if err := recordAskAuthors(ctx, tx, room, carried); err != nil {
		return err
	}
	previous := make([]string, 0, len(stamped))
	minted := make([]string, 0, len(stamped))
	for _, ask := range stamped {
		if ask.copied || !storableAskBlockID(ask.previous) || !storableAskBlockID(ask.minted) {
			continue
		}
		previous = append(previous, ask.previous)
		minted = append(minted, ask.minted)
	}
	if len(minted) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		insert into doc_ask_block_authors (artifact_id, block_id, actor)
		select recorded.artifact_id, renamed.minted, recorded.actor
		from unnest($2::text[], $3::text[]) as renamed(previous, minted)
		join doc_ask_block_authors recorded on recorded.artifact_id = $1 and recorded.block_id = renamed.previous
		on conflict (artifact_id, block_id_key(block_id)) do nothing
	`, room, previous, minted); err != nil {
		return fmt.Errorf("carry the authors of the ask blocks a stamp renamed: %w", err)
	}
	return nil
}

// readRecordedAskAuthors is the durable ask authors of room, keyed by block id. A settlement reads
// them in its own transaction, under the document's advisory lock.
func readRecordedAskAuthors(ctx context.Context, q Queryer, room string) (map[string]model.Actor, error) {
	rows, err := q.Query(ctx, `select block_id, actor from doc_ask_block_authors where artifact_id = $1`, room)
	if err != nil {
		return nil, fmt.Errorf("read the document's ask block authors: %w", err)
	}
	defer rows.Close()
	recorded := make(map[string]model.Actor)
	for rows.Next() {
		var block string
		var encoded []byte
		if err := rows.Scan(&block, &encoded); err != nil {
			return nil, fmt.Errorf("read the document's ask block authors: %w", err)
		}
		var actor model.Actor
		if err := json.Unmarshal(encoded, &actor); err != nil {
			return nil, fmt.Errorf("decode an ask block author of the document: %w", err)
		}
		recorded[block] = actor
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the document's ask block authors: %w", err)
	}
	return recorded, nil
}

// deleteSettledAskAuthors deletes, in a settlement's transaction, the durable author of each block
// in indexed, which an ask row names from now on, and of each block held does not hold, the ask
// blocks of the tree the settlement reconciled: a block deleted from the document, or an id a
// stamp replaced. A block still waiting for its ask - a copy, one whose observer has not run, one
// settlement cannot read - keeps its row.
func deleteSettledAskAuthors(ctx context.Context, tx pgx.Tx, room string, indexed []string, held map[string]struct{}) error {
	// Neither array may be NULL: `not block_id = any(NULL)` is NULL, which would keep every row of a
	// document that holds no ask block at all.
	if indexed == nil {
		indexed = []string{}
	}
	kept := make([]string, 0, len(held))
	for block := range held {
		if storableAskBlockID(block) {
			kept = append(kept, block)
		}
	}
	if _, err := tx.Exec(ctx, `
		delete from doc_ask_block_authors
		where artifact_id = $1 and (block_id = any($2::text[]) or not block_id = any($3::text[]))
	`, room, indexed, kept); err != nil {
		return fmt.Errorf("delete the authors of the document's settled ask blocks: %w", err)
	}
	return nil
}
