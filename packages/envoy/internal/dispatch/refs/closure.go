package refs

import (
	"context"
	"sort"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Member is an artifact reached through an issue's reference closure. ViaBlocks names the
// blocks of Via that cite it, sorted, and is nil for a member no block cites: a structural
// edge, an attachment, or an item citing it from its own body rather than from one of its
// blocks.
type Member struct {
	RefKey    string
	Depth     int
	Via       model.ReferenceVia
	ViaBlocks []string
}

// documentTarget is true of a refs row r whose target is a document, or a block a current
// version of its document holds; documentOfTarget is that document's ref_key, which never holds
// a '#'. documentBlockHeld is the join that answers whether a block target is held, and
// documentTarget reads its answer rather than an `exists` probe of its own: inside the
// recursive walk below the probe sits under an OR, which no semijoin can carry, so the planner
// plans it as a hashed subplan and reads all of document_blocks to build the hash — 800,000
// rows and 45,915 buffers of a 122,128-buffer read, once for each place the predicate is
// spliced into. A lateral that stops at its first row is a nested loop keyed
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

// citesTheMember is true of a block link row r that cites the member of the lateral's outer row
// m as a document: a link to the document itself, and never one to a block of it, which cites
// that block and not the document it stands in. The walk reaches a member either way
// (documentOfTarget), and this is the narrower question ViaBlocks answers — which blocks a
// reader may draw the document under — so a document reached only through a link to one of its
// blocks is named by no block at all.
const citesTheMember = `r.to_kind = 'artifact' and r.to_id = m.ref_key`

// citingBlocks is the lateral that names the blocks of a member's via holding its citation, one
// arm per kind of via: a document's blocks are in block_refs, an item's in its own block
// mentions table, each keyed by the item column its primary key leads with, so an arm is one
// index lookup for the one via the member was reached through. Each item arm keeps the position
// filter every other read of an item's blocks keeps, so a block a later revision removed names
// nothing; a document's rows are reconciled per version and carry no such column.
//
// Each arm tests the kind of the member's via against the outer row, so only the arm of that
// kind can answer a row and the other three find none. The aggregate is null for a member no
// block of its via cites, which is the answer for a structural edge and for an item citing a
// document from its body rather than from one of its blocks.
var citingBlocks = buildCitingBlocks()

func buildCitingBlocks() string {
	arms := []string{`select r.block_id from block_refs r
					where m.via_kind = 'artifact' and r.artifact_id = m.via_uuid and ` + citesTheMember}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		arms = append(arms, `select r.block_id from `+tables.refs+` r
					join `+tables.blocks+` b on b.`+tables.item+` = r.`+tables.item+` and b.block_id = r.block_id
					where m.via_kind = '`+string(item)+`' and r.`+tables.item+` = m.via_uuid
					  and b.position is not null and `+citesTheMember)
	}
	return `left join lateral (
				select array_agg(distinct holder.block_id order by holder.block_id) as blocks
				from (
					` + strings.Join(arms, `
					union all
					`) + `
				) holder
			) citing on true`
}

// closureQuery is the statement Closure runs, held here so that its plan can be read by name.
// Its last select takes one member per document, as it always has, and the lateral then names
// the citing blocks of the one via that member was taken at: taking the blocks inside the walk
// instead would multiply its rows by them. The via's id is cast once, for the lateral's arms to
// key their tables by: every via kind — ask, comment, message, artifact — is identified by the
// uuid of its row, so the cast is of a uuid in every case.
//
// $2 says whether the issue's own documents are members too. One statement serves both answers,
// rather than two texts whose plans a reader would have to compare: the walk is the same, and
// only the last select's filter reads the flag.
var closureQuery = `
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
			select ` + documentOfTarget + `, 1, 'ask', r.ask_id::text` + askPartLinksFrom + `
			` + documentBlockHeld + askPartLinksCurrent + `
			  and r.ask_id in (select id from asks where issue_key = $1)
			  and ` + documentTarget + `
			union
			select ` + documentOfTarget + `, c.depth + 1, 'artifact', a.id::text
			from closure c
			join artifacts a on a.ref_key = c.ref_key
			join refs r on r.from_kind = 'artifact' and r.from_id = a.id::text
			` + documentBlockHeld + `
			where c.depth < 9 and ` + documentTarget + `
		)
		select m.ref_key, m.depth, m.via_kind, m.via_id, citing.blocks
		from (
			select distinct on (c.ref_key) c.ref_key, c.depth, c.via_kind, c.via_id, c.via_id::uuid as via_uuid
			from closure c
			join artifacts a on a.ref_key = c.ref_key
			where $2 or a.issue_key is distinct from $1
			order by c.ref_key, c.depth, c.via_kind, c.via_id
		) m
		` + citingBlocks + `
	`

// Closure follows artifact references from an issue's own content to eight hops. A reference to
// a block reaches the block's document, while a current version of the document holds that
// block: a deleted block's links drop out of the closure until a version holds its id again. A
// block of a comment or message reaches its item, as a document's block reaches its document: a
// link one holds is read from its item's own refs row, which every comment and message keeps
// beside its blocks' mentions, so the member's via is the item. An ask keeps no such row - its
// refs are its question's alone - so the links its parts hold are read from ask_block_refs, by
// the same rule: the member's via is the ask, at the depth the ask's own link has. A reference to
// an item block, as one to its item, reaches no document. Each member names the blocks of its
// via that cite it (ViaBlocks), so a reader can put the document under the block citing it.
//
// own says whether the issue's own documents are members as well. The walk always reads them -
// a document of the issue is how most of the closure is reached - and `own` decides only
// whether they are answered: false is the long-standing answer, the artifacts an issue reaches
// *outside* itself, and true adds its own, which is what a reader placing each document under
// the block citing it needs (`GET /issues/{key}/references?own=1`).
func Closure(ctx context.Context, q Queryer, issueKey string, own bool) ([]Member, bool, error) {
	rows, err := q.Query(ctx, closureQuery, issueKey, own)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	members := []Member{}
	truncated := false
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.RefKey, &member.Depth, &member.Via.Kind, &member.Via.ID, &member.ViaBlocks); err != nil {
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
