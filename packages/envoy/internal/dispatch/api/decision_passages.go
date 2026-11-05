package api

import (
	"context"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// pinDecisionPassages retains each shaped passage's original words in the existing GraphNode
// pin shape. The default compares the passage at the relation's creation with its text now;
// an explicitly requested document version supplies that version's words for historical matching.
func (s *server) pinDecisionPassages(ctx context.Context, decisions []model.Decision, artifactID string, version int) error {
	if version > 0 {
		var at time.Time
		if err := s.deps.Store.Pool.QueryRow(ctx, `select created_at from artifact_versions where artifact_id=$1::uuid and number=$2`, artifactID, version).Scan(&at); err != nil {
			return err
		}
		for i := range decisions {
			links := decisions[i].Links[:0]
			for _, link := range decisions[i].Links {
				if link.To.Parent != nil && link.To.Parent.ID == artifactID && link.CreatedAt.After(at) {
					continue
				}
				links = append(links, link)
			}
			decisions[i].Links = links
		}
	}
	type place struct{ decision, link int }
	var places []place
	var documents, blocks []string
	var times []time.Time
	var pins []int
	for i, decision := range decisions {
		for j, link := range decision.Links {
			if link.To.Parent == nil {
				continue
			}
			pin := 0
			if link.To.Pin != nil {
				pin = link.To.Pin.Version
			}
			pins = append(pins, pin)
			decisions[i].Links[j].To.Pin = nil
			documents = append(documents, link.To.Parent.ID)
			blocks = append(blocks, link.To.BlockID)
			times = append(times, link.CreatedAt)
			places = append(places, place{i, j})
		}
	}
	if len(places) == 0 {
		return nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
 select input.ord, chosen.number, at_version.text, current.text
 from unnest($1::uuid[],$2::text[],$3::timestamptz[],$4::int[]) with ordinality input(artifact,block,at,pin,ord)
 left join lateral (
   select case when input.artifact::text=$5 and $6::int>0 then $6::int
     when input.pin>0 then input.pin else
     (select max(v.number) from artifact_versions v where v.artifact_id=input.artifact and v.created_at<=input.at)
   end number
 ) chosen on true
 left join lateral (
   select t.text from document_block_texts t
   where t.artifact_id=input.artifact and t.block_id=input.block and t.version<=chosen.number
     and exists (select 1 from document_indexed_versions i where i.artifact_id=input.artifact and i.version=chosen.number)
   order by t.version desc limit 1
 ) at_version on true
 left join lateral (
   select t.text from document_block_texts t where t.artifact_id=input.artifact and t.block_id=input.block order by t.version desc limit 1
 ) current on true
 `, documents, blocks, times, pins, artifactID, version)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		var number *int
		var atVersion, current *string
		if err := rows.Scan(&ordinal, &number, &atVersion, &current); err != nil {
			return err
		}
		if number == nil || atVersion == nil {
			continue
		}
		at := places[ordinal-1]
		node := &decisions[at.decision].Links[at.link].To
		state := "unchanged"
		if current == nil {
			state = "removed"
		} else if *current != *atVersion {
			state = "changed"
		}
		node.Pin = &model.BlockPin{Version: *number, Ref: node.Ref + "@v" + strconv.Itoa(*number), Text: *atVersion, State: state}
	}
	return rows.Err()
}

// The ordinary block-links read deliberately omits removed targets. A decision also shows the
// passage it once shaped, so retain only its own shaped edges whose target has block history.
func (s *server) removedDecisionPassages(ctx context.Context, from, to string) ([]model.BlockLink, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
 select r.block_id,r.to_id,substr(r.to_id,length(a.ref_key)+2),r.created_at,r.created_version,
   a.issue_key,a.project_key,a.slug,a.is_primary
 from block_refs r
 join document_blocks f on f.artifact_id=r.artifact_id and f.block_id=r.block_id
 join artifacts a on a.id=$2::uuid and left(r.to_id,length(a.ref_key)+1)=a.ref_key||'#'
 where r.artifact_id=$1::uuid and r.kind='shaped' and r.to_kind='block'
   and not exists (select 1 from document_blocks t where t.artifact_id=a.id and t.node_id=r.to_id)
   and exists (select 1 from document_block_texts h where h.artifact_id=a.id and h.block_id=substr(r.to_id,length(a.ref_key)+2) and h.text is not null)
 order by r.created_at desc,r.from_node,r.to_id`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.BlockLink
	for rows.Next() {
		link := model.BlockLink{Kind: "shaped"}
		document := model.Artifact{ID: to, Kind: "doc"}
		if err := rows.Scan(&link.From.BlockID, &link.To.ID, &link.To.BlockID, &link.CreatedAt, &link.CreatedVersion, &document.IssueKey, &document.Project, &document.Slug, &document.Primary); err != nil {
			return nil, err
		}
		reference := artifactReference(document)
		link.To.Kind = "block"
		link.To.IssueKey, link.To.Project = document.IssueKey, document.Project
		link.To.Ref = refs.BlockRef(reference, link.To.BlockID)
		link.To.Document = reference
		link.To.Parent = &model.NodeParent{Kind: "artifact", ID: to, Ref: reference}
		result = append(result, link)
	}
	return result, rows.Err()
}
