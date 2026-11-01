package refs

import (
	"context"
	"sort"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Member is an artifact reached through an issue's reference closure.
type Member struct {
	RefKey string
	Depth  int
	Via    model.ReferenceVia
}

// documentTarget is true of a refs row r whose target is a document, or a block a current
// version of its document holds; documentOfTarget is that document's ref_key, which never holds
// a '#'. documentBlockHeld is the join that answers whether a block target is held, and
// documentTarget reads its answer rather than an `exists` probe of its own: inside the
// recursive walk below the probe sits under an OR, which no semijoin can carry, so the planner
// plans it as a hashed subplan and reads all of document_blocks to build the hash — 800,000
// rows and 45,915 buffers of a 122,128-buffer read, once for each of the two places the
// predicate is spliced into. A lateral that stops at its first row is a nested loop keyed
// through document_blocks_node_id, the hash index on exactly node_id, and it runs at all only
// for a block target, because its own to_kind test is a one-time filter the planner re-reads on
// each loop.
//
// documentTarget states the two target kinds the scan must read in a clause of their own as
// well. The kinds are implied by its disjunction, but that disjunction now reads a column of a
// join, so the planner can no longer derive the restriction from it and would read refs whole.
const (
	documentBlockHeld = `left join lateral (
				select 1 as held from document_blocks d
				where r.to_kind = 'block' and d.node_id = r.to_id
				limit 1
			) block_target on true`
	documentTarget   = `(r.to_kind in ('artifact', 'block') and (r.to_kind = 'artifact' or block_target.held is not null))`
	documentOfTarget = `case when r.to_kind = 'block' then split_part(r.to_id, '#', 1) else r.to_id end`
)

// closureQuery is the statement Closure runs, held here so that its plan can be read by name.
const closureQuery = `
		with recursive closure as (
			select ` + documentOfTarget + ` as ref_key, 1 as depth, r.from_kind as via_kind, r.from_id as via_id
			from refs r
			` + documentBlockHeld + `
			where ` + documentTarget + ` and (
				  (r.from_kind = 'ask'      and r.from_id in (select id::text from asks      where issue_key = $1))
				or (r.from_kind = 'comment'  and r.from_id in (select id::text from comments  where issue_key = $1))
				or (r.from_kind = 'message'  and r.from_id in (select id::text from messages  where issue_key = $1))
				or (r.from_kind = 'artifact' and r.from_id in (select id::text from artifacts where issue_key = $1))
			)
			union
			select ` + documentOfTarget + `, c.depth + 1, 'artifact', a.id::text
			from closure c
			join artifacts a on a.ref_key = c.ref_key
			join refs r on r.from_kind = 'artifact' and r.from_id = a.id::text
			` + documentBlockHeld + `
			where c.depth < 9 and ` + documentTarget + `
		)
		select distinct on (c.ref_key) c.ref_key, c.depth, c.via_kind, c.via_id
		from closure c
		join artifacts a on a.ref_key = c.ref_key
		where a.issue_key is distinct from $1
		order by c.ref_key, c.depth, c.via_kind, c.via_id
	`

// Closure follows artifact references from an issue's own content to eight hops. A reference to
// a block reaches the block's document, while a current version of the document holds that
// block: a deleted block's links drop out of the closure until a version holds its id again. A
// block of a comment or message reaches its item, as a document's block reaches its document: a
// link one holds is read from its item's own refs row, which every comment and message keeps
// beside its blocks' mentions, so the member's via is the item. A reference to an item block, as
// one to its item, reaches no document.
func Closure(ctx context.Context, q Queryer, issueKey string) ([]Member, bool, error) {
	rows, err := q.Query(ctx, closureQuery, issueKey)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	members := []Member{}
	truncated := false
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.RefKey, &member.Depth, &member.Via.Kind, &member.Via.ID); err != nil {
			return nil, false, err
		}
		if member.Depth == 9 {
			truncated = true
			continue
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	sort.Slice(members, func(left, right int) bool {
		if members[left].Depth != members[right].Depth {
			return members[left].Depth < members[right].Depth
		}
		return members[left].RefKey < members[right].RefKey
	})
	return members, truncated, nil
}
