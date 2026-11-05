package refs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// IsIdentity reports whether a reference kind names a person, a session or a role - the three
// kinds whose mentions are identity mentions, stored in identity_mentions and in no other
// relation (store/migrations/20261104090139_mention_graph.up.sql).
//
// They are kept out of refs, block_refs and the item block tables deliberately. Every build
// reconciles a source it writes by deleting the rows its own parser did not find, and the
// document and item reconciliations are scoped by the owner alone (replaceBlockRefs,
// replaceItemBlockRefs), so a build that does not know identity mentions deletes them whatever
// storage kind they wear, and then marks the source indexed with nothing to revisit it. A build
// that predates identity mentions names identity_mentions nowhere, so during a rolling deploy's
// old/new overlap, and after a rollback, every row of it survives.
func IsIdentity(kind string) bool {
	return kind == "person" || kind == "session" || kind == "role"
}

// identityRow is one identity mention a reconciliation writes: the graph node it is from, and
// whom it names. For a block the node is its node id, so the row outlives the block's text.
type identityRow struct {
	fromID string
	target Target
}

// identityRows is the rows one source writes for every identity target in targets.
func identityRows(fromID string, targets []Target) []identityRow {
	rows := make([]identityRow, len(targets))
	for i, target := range targets {
		rows[i] = identityRow{fromID: fromID, target: target}
	}
	return rows
}

// replaceIdentityMentions reconciles the identity mentions one reconciliation owns - every row
// under ownerKind and ownerID - with rows: rows it no longer finds are deleted, new ones inserted
// with insertedAt as their created_at (nil: now()), and a row that persists keeps its created_at
// and its source_seq, so a mention is dated by the write that first made it and not by the latest
// edit around it.
//
// inheritFrom names the owner a first indexing takes a new row's created_at and source_seq from -
// the item's or the document's own body mentions, so a block's mention of a target its source
// already mentions carries that source's date, exactly as replaceBlockRefs and
// replaceItemBlockRefs inherit from refs. It is empty for every other write.
//
// It returns the targets whose rows moved, deleted first, which the counted-change readers
// resolve by name (CountedChanges).
func replaceIdentityMentions(
	ctx context.Context,
	tx pgx.Tx,
	ownerKind, ownerID, fromKind string,
	rows []identityRow,
	insertedAt *time.Time,
	inheritFrom string,
) ([]Target, error) {
	fromIDs := make([]string, len(rows))
	toKinds := make([]string, len(rows))
	toIDs := make([]string, len(rows))
	for i, row := range rows {
		fromIDs[i], toKinds[i], toIDs[i] = row.fromID, row.target.Kind, row.target.ID
	}
	changed, err := scanTargets(tx.Query(ctx, `
		delete from identity_mentions m
		where m.owner_kind = $1 and m.owner_id = $2
		  and not exists (
		    select 1 from unnest($3::text[], $4::text[], $5::text[]) as p (from_id, to_kind, to_id)
		    where p.from_id = m.from_id and p.to_kind = m.to_kind and p.to_id = m.to_id
		  )
		returning m.to_kind, m.to_id
	`, ownerKind, ownerID, fromIDs, toKinds, toIDs))
	if err != nil {
		return nil, fmt.Errorf("clear the identity mentions of %s %s: %w", ownerKind, ownerID, err)
	}
	if len(rows) == 0 {
		return changed, nil
	}
	added, err := scanTargets(tx.Query(ctx, `
		insert into identity_mentions (owner_kind, owner_id, from_kind, from_id, to_kind, to_id, created_at, source_seq)
		select $1, $2, $6, p.from_id, p.to_kind, p.to_id,
		       coalesce(prior.created_at, $7::timestamptz, now()), prior.source_seq
		from unnest($3::text[], $4::text[], $5::text[]) as p (from_id, to_kind, to_id)
		left join identity_mentions prior
		       on prior.owner_kind = nullif($8, '') and prior.owner_id = $2
		      and prior.to_kind = p.to_kind and prior.to_id = p.to_id
		on conflict (owner_kind, owner_id, from_kind, from_id, to_kind, to_id) do nothing
		returning to_kind, to_id
	`, ownerKind, ownerID, fromIDs, toKinds, toIDs, fromKind, insertedAt, inheritFrom))
	if err != nil {
		return nil, fmt.Errorf("record the identity mentions of %s %s: %w", ownerKind, ownerID, err)
	}
	return append(changed, added...), nil
}

// replaceDocumentIdentityMentions reconciles the identity mentions of one document version's
// blocks. A block's node id is its document's reference key and the block id, the id
// document_blocks stores and graph_mention_edges joins on, so the row outlives the block's text;
// the key is read only when the version holds an identity mention at all. At the document's first
// indexing a new row takes the date of the document's own body mention of that target, as
// replaceBlockRefs takes it from refs.
func replaceDocumentIdentityMentions(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	versionAt time.Time,
	firstIndexing bool,
	blocks []identityRow,
) ([]Target, error) {
	if len(blocks) > 0 {
		var refKey string
		if err := tx.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, artifactID).Scan(&refKey); err != nil {
			return nil, fmt.Errorf("read the reference key of %s: %w", artifactID, err)
		}
		for i := range blocks {
			blocks[i].fromID = refKey + "#" + blocks[i].fromID
		}
	}
	inherit := ""
	if firstIndexing {
		inherit = "artifact"
	}
	return replaceIdentityMentions(ctx, tx, blockOwnerKind("artifact"), artifactID, "block", blocks, &versionAt, inherit)
}

// blockOwnerKind is the owner a source's block reconciliation writes its identity mentions under:
// the source's own kind and its blocks are two reconciliations of one id, each replacing only its
// own rows.
func blockOwnerKind(sourceKind string) string { return sourceKind + "_blocks" }

// IdentityMentionsOf lists the identities one source's body names, as identity_mentions holds
// them, newest first. It is the read the mention census compares a re-parse of the source against.
func IdentityMentionsOf(ctx context.Context, q Queryer, fromKind, fromID string) ([]Target, error) {
	return scanTargets(q.Query(ctx, `
		select to_kind, to_id from identity_mentions
		where from_kind = $1 and from_id = $2
		order by to_kind, to_id
	`, fromKind, fromID))
}
