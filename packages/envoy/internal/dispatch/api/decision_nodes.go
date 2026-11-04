package api

import (
 "context"
 "sort"
 "strings"
 "time"

 "github.com/sjawhar/envoy/internal/dispatch/model"
 "github.com/sjawhar/envoy/internal/dispatch/pmdoc"
 "github.com/sjawhar/envoy/internal/dispatch/refs"
)

// decisionDocumentsQuery is a named, issue-scoped graph query. The block type selects the
// decision nodes; part_of selects their documents. It never follows a mention into a different
// issue, project document or direct message. The graph remains a view of the existing index.
const decisionDocumentsQuery = `
 select distinct a.id::text, a.is_primary, a.created_at
 from artifacts a
 join document_blocks b on b.artifact_id = a.id and b.type in ('decision','ask')
 join graph_edges g on g.from_kind = 'block' and g.from_id = b.node_id
   and g.kind = 'part_of' and g.to_kind = 'artifact' and g.to_id = a.ref_key
 where a.issue_key = $1 and a.kind = 'doc'
 order by a.is_primary desc, a.created_at, a.id::text`

func (s *server) issueDecisionNodes(ctx context.Context, key string) (issueDecisions, error) {
 answer := issueDecisions{OpenQuestions:[]issueAskRow{},Decisions:[]model.Decision{}}
 questions, err := s.issueDecisionQuestions(ctx,key)
 if err != nil { return answer,err }
 answer.OpenQuestions=questions
 rows,err:=s.deps.Store.Pool.Query(ctx,decisionDocumentsQuery,key)
 if err!=nil { return answer,err }
 var ids []string
 for rows.Next() {
  var id string; var primary bool; var at time.Time
  if err:=rows.Scan(&id,&primary,&at); err!=nil { rows.Close(); return answer,err }
  ids=append(ids,id)
 }
 rows.Close()
 if err:=rows.Err(); err!=nil { return answer,err }
 spec,found,err:=s.issueSpecDocument(ctx,key)
 if err!=nil { return answer,err }
 specID:=""
 if found { specID=spec.ID }
 for _,id:=range ids {
  document,err:=s.loadArtifact(ctx,s.deps.Store.Pool,id)
  if err!=nil { return answer,err }
  reference:=artifactReference(document)
  decisions,err:=s.readDecisions(ctx,document,reference,specID)
  if err!=nil { return answer,err }
  answer.Decisions=append(answer.Decisions,decisions...)
  if answer.Document==nil && len(decisions)>0 { answer.Document=&reference }
 }
 return answer,nil
}

// readDecisions reads each typed node's own text. Only the ask row can supply a person's
// decision: from_ask is provenance for an agent's choice, never permission to restate an answer.
func (s *server) readDecisions(ctx context.Context, document model.Artifact, reference, specID string) ([]model.Decision,error) {
 markdown,blocks,err:=s.deps.Docs.TextWithBlocks(ctx,document.ID)
 if err!=nil { return nil,err }
 changes,err:=refs.BlockChanges(ctx,s.deps.Store.Pool,document.ID)
 if err!=nil { return nil,err }
 keeps,err:=decisionKeeps(ctx,s.deps.Store.Pool,document.ID)
 if err!=nil { return nil,err }
 var links []model.BlockLink
 if specID!="" {
  links,err=refs.BlockLinks(ctx,s.deps.Store.Pool,document.ID,specID)
  if err!=nil { return nil,err }
 }
 rows,err:=s.deps.Store.Pool.Query(ctx,`select `+askReadColumns+` `+askReadFrom+` where a.block_artifact_id=$1::uuid and a.state='answered'`,document.ID)
 if err!=nil { return nil,err }
 asks:=map[string]model.Ask{}
 for rows.Next() {
  ask,_,err:=scanAskRead(rows)
  if err!=nil { rows.Close(); return nil,err }
  if ask.BlockID!=nil { asks[*ask.BlockID]=ask }
 }
 rows.Close()
 if err:=rows.Err(); err!=nil { return nil,err }
 sort.SliceStable(blocks,func(i,j int)bool{return blocks[i].From<blocks[j].From})
 decisions:=[]model.Decision{}
 for _,block:=range blocks {
  if block.Type!="decision" && block.Type!="ask" { continue }
  decision:=model.Decision{BlockID:block.ID,Ref:refs.BlockRef(reference,block.ID),Kind:"agent",Asks:[]string{},Links:[]model.DecisionLink{}}
  if change,ok:=changes[block.ID]; ok { at:=change.At; decision.ChangedVersion=change.Version;decision.ChangedAt=&at }
  if block.Type=="ask" {
   ask,found:=asks[block.ID]
   if !found || ask.Answer==nil { continue }
   decision.Kind="person"
   decision.Heading=ask.Question
   decision.Asks=[]string{ask.ID}
   decision.Text=askDecisionText(ask)
  } else {
   decision.Heading=blockAttribute(block,"title")
   decision.Replaced=blockAttribute(block,"replaced")=="true"
   decision.Text=decisionNodeText(sliceMarkdown(markdown,block.From,block.To))
   if decision.Heading=="" { decision.Heading=strings.SplitN(decision.Text,"\n",2)[0] }
   decision.Unsure=strings.HasPrefix(decision.Text,"Unsure:") || strings.Contains(decision.Text,"\n\nUnsure:")
   if keep,ok:=keeps[block.ID]; ok { decision.Kept=&keep }
  }
  for _,link:=range links {
   if link.Kind=="shaped" && link.From.BlockID==block.ID {
    decision.Links=append(decision.Links,model.DecisionLink{FromBlock:block.ID,To:link.To,CreatedVersion:link.CreatedVersion,CreatedAt:link.CreatedAt})
   }
  }
  decision.TextSHA256=decisionTextSum(decision.Text)
  decision.WaitingReview=decision.Kind=="agent" && !decision.Replaced && (decision.Kept==nil || decision.Kept.TextSHA256!=decision.TextSHA256)
  decisions=append(decisions,decision)
 }
 if err:=s.markNodeLinksStale(ctx,document.ID,decisions);err!=nil{return nil,err}
 return decisions,nil
}

func blockAttribute(block model.ArtifactBlock, name string) string {
 for _,attribute:=range block.Attributes { if attribute.Name==name{return attribute.Value} }
 return ""
}

func askDecisionText(ask model.Ask) string {
 var body strings.Builder
 body.WriteString(ask.Question)
 for _,option:=range ask.Options {
  body.WriteString("\n\n- "+option.Label)
  if option.Description!="" { body.WriteString(": "+option.Description) }
 }
 if ask.Answer!=nil {
  if len(ask.Answer.Selected)>0 { body.WriteString("\n\nSelected: "+strings.Join(ask.Answer.Selected,", ")) }
  if ask.Answer.Text!=nil { body.WriteString("\n\n"+*ask.Answer.Text) }
 }
 return body.String()
}

// decisionNodeText unwraps the typed block, not a heading section. The heading remains in the
// response text for compatibility, and every byte of the node's body is retained.
func decisionNodeText(markdown string) string {
 tree,err:=pmdoc.ParseRendering(markdown)
 if err!=nil || len(tree.Children)!=1 || tree.Children[0].Type!="decision" { return strings.TrimSpace(markdown) }
 node:=tree.Children[0]
 body,err:=pmdoc.Render(&pmdoc.Node{Type:"doc",Children:node.Children})
 if err!=nil { return strings.TrimSpace(markdown) }
 title,_:=node.Attrs["title"].(string)
 if title!="" { return "## "+title+"\n\n"+strings.TrimRight(body,"\n") }
 return strings.TrimRight(body,"\n")
}

// The same text-at-two-instants rule as Keep's legacy read, now keyed by stable block identity.
// One batched query reads all link histories of this document, independent of its version count.
func (s *server) markNodeLinksStale(ctx context.Context, artifactID string, decisions []model.Decision) error {
 var ids []string; var versions []int; var times []time.Time
 type at struct{ decision,link int }; var places []at
 for i,decision:=range decisions {
  if decision.Kind!="agent" {continue}
  for j,link:=range decision.Links {
   if link.To.ChangedAt==nil {continue}
   ids=append(ids,decision.BlockID);versions=append(versions,link.CreatedVersion);times=append(times,*link.To.ChangedAt);places=append(places,at{i,j})
  }
 }
 if len(ids)==0{return nil}
 rows,err:=s.deps.Store.Pool.Query(ctx,`
 select input.ord, first.text, latest.number, changed.text
 from unnest($2::text[],$3::int[],$4::timestamptz[]) with ordinality input(block,version,at,ord)
 left join lateral (select t.text from document_block_texts t where t.artifact_id=$1::uuid and t.block_id=input.block and t.version<=input.version order by t.version desc limit 1) first on true
 left join lateral (select v.number from artifact_versions v where v.artifact_id=$1::uuid and v.created_at<=input.at order by v.number desc limit 1) latest on true
 left join lateral (select t.text from document_block_texts t where t.artifact_id=$1::uuid and t.block_id=input.block and t.version<=latest.number order by t.version desc limit 1) changed on true
 `,artifactID,ids,versions,times)
 if err!=nil{return err}
 defer rows.Close()
 for rows.Next(){
  var ord int;var first,changed *string;var version *int
  if err:=rows.Scan(&ord,&first,&version,&changed);err!=nil{return err}
  place:=places[ord-1];decision:=&decisions[place.decision]
  if first==nil || decisionNodeText(*first)==decision.Text{continue}
  decision.Links[place.link].Stale=version==nil || changed!=nil && decisionNodeText(*changed)!=decision.Text
 }
 return rows.Err()
}
