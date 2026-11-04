package api

import (
 "context"
 "strconv"
 "time"

 "github.com/sjawhar/envoy/internal/dispatch/model"
)

// pinDecisionPassages retains each shaped passage's original words in the existing GraphNode
// pin shape. The default compares the passage at the relation's creation with its text now;
// an explicitly requested document version supplies that version's words for historical matching.
func (s *server) pinDecisionPassages(ctx context.Context, decisions []model.Decision, artifactID string, version int) error {
 type place struct{ decision,link int }
 var places []place
 var documents,blocks []string
 var times []time.Time
 for i,decision:=range decisions {
  for j,link:=range decision.Links {
   if link.To.Parent==nil {continue}
   documents=append(documents,link.To.Parent.ID);blocks=append(blocks,link.To.BlockID);times=append(times,link.CreatedAt);places=append(places,place{i,j})
  }
 }
 if len(places)==0{return nil}
 rows,err:=s.deps.Store.Pool.Query(ctx,`
 select input.ord, chosen.number, at_version.text, current.text
 from unnest($1::uuid[],$2::text[],$3::timestamptz[]) with ordinality input(artifact,block,at,ord)
 left join lateral (
   select case when input.artifact::text=$4 and $5::int>0 then $5::int else
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
 `,documents,blocks,times,artifactID,version)
 if err!=nil{return err}
 defer rows.Close()
 for rows.Next(){
  var ordinal int;var number *int;var atVersion,current *string
  if err:=rows.Scan(&ordinal,&number,&atVersion,&current);err!=nil{return err}
  if number==nil || atVersion==nil {continue}
  at:=places[ordinal-1];node:=&decisions[at.decision].Links[at.link].To
  state:="unchanged"
  if current==nil {state="removed"} else if *current!=*atVersion {state="changed"}
  node.Pin=&model.BlockPin{Version:*number,Ref:node.Ref+"@v"+strconv.Itoa(*number),Text:*atVersion,State:state}
 }
 return rows.Err()
}
