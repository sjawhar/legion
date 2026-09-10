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

// Closure follows artifact references from an issue's own content to eight hops.
func Closure(ctx context.Context, q Queryer, issueKey string) ([]Member, bool, error) {
	rows, err := q.Query(ctx, `
		with recursive closure as (
			select r.to_id as ref_key, 1 as depth, r.from_kind as via_kind, r.from_id as via_id
			from refs r
			where r.to_kind = 'artifact' and (
				  (r.from_kind = 'ask'      and r.from_id in (select id::text from asks      where issue_key = $1))
				or (r.from_kind = 'comment'  and r.from_id in (select id::text from comments  where issue_key = $1))
				or (r.from_kind = 'message'  and r.from_id in (select id::text from messages  where issue_key = $1))
				or (r.from_kind = 'artifact' and r.from_id in (select id::text from artifacts where issue_key = $1))
			)
			union
			select r.to_id, c.depth + 1, 'artifact', a.id::text
			from closure c
			join artifacts a on a.ref_key = c.ref_key
			join refs r on r.from_kind = 'artifact' and r.from_id = a.id::text and r.to_kind = 'artifact'
			where c.depth < 9
		)
		select distinct on (c.ref_key) c.ref_key, c.depth, c.via_kind, c.via_id
		from closure c
		join artifacts a on a.ref_key = c.ref_key
		where a.issue_key is distinct from $1
		order by c.ref_key, c.depth, c.via_kind, c.via_id
	`, issueKey)
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
