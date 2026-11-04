// Package decisionmigration is the one-time reader of the retired decisions document format.
// Runtime decision reads never import it. Plans keep ambiguous prose, label it for review and
// only discard citation-only records whose ask remains the source of truth.
package decisionmigration

import (
 "crypto/sha256"
 "encoding/hex"
 "fmt"
 "slices"
 "strings"

 "github.com/sjawhar/envoy/internal/dispatch/pmdoc"
 "github.com/sjawhar/envoy/internal/dispatch/text"
)

type Entry struct {
 ID string
 Title string
 Body string
 Shaped []string
 Asks []string
 Replaced bool
 Unsure bool
 Drop bool
 Review bool
}

// Plan reads top-level heading sections with the Markdown parser, so fenced code, tables and
// nested headings cannot manufacture decisions. The source UUID and ordinal make retry IDs
// stable without coalescing two distinct choices with the same title.
func Plan(markdown, source string) ([]Entry, error) {
 tree, err := pmdoc.ParseRendering(markdown)
 if err != nil { return nil, err }
 var entries []Entry
 var children []*pmdoc.Node
 var title string
 var replaced, inReplaced bool
 closeEntry := func() error {
  if title == "" { return nil }
  body, err := pmdoc.Render(&pmdoc.Node{Type:"doc",Children:children})
  if err != nil { return err }
  digest := sha256.Sum256([]byte(fmt.Sprintf("%s/%d",source,len(entries))))
  entry := Entry{ID:"decision-"+hex.EncodeToString(digest[:12]),Title:title,Body:strings.TrimSpace(body),Replaced:replaced,Review:title=="Imported decision context"}
  for _, ref := range text.Extract(body, "") {
   switch ref.Kind {
   case "ask":
    if !slices.Contains(entry.Asks,ref.ID) { entry.Asks=append(entry.Asks,ref.ID) }
   case "block":
    if ref.Item == "" {
     canonical, _ := text.FormatBlockRef(ref)
     if !slices.Contains(entry.Shaped,canonical) { entry.Shaped=append(entry.Shaped,canonical) }
    }
   }
  }
  for _, line := range strings.Split(body,"\n") { if strings.HasPrefix(strings.TrimSpace(line),"Unsure:") { entry.Unsure=true } }
  // No semantic inference: citation boilerplate carries no independent choice. Narrative
  // around an ask may carry reasoning, so keep it and flag its attribution for review.
  stripped := body
  if len(entry.Asks)>0 {
   for _, token := range strings.Fields(body) {
    if strings.HasPrefix(token,"dispatch://") { stripped=strings.ReplaceAll(stripped,token,"") }
   }
   stripped=strings.Trim(stripped," \t\r\n.")
   entry.Drop = stripped == "Decided in  Shapes" || stripped == "Decided in" || stripped == "Decided in  Shapes  and" || stripped == ""
   entry.Review = !entry.Drop
  }
  entries=append(entries,entry)
  title=""
  children=nil
  return nil
 }
 for _, node := range tree.Children {
  level,_ := node.Attrs["level"].(int)
  if node.Type=="heading" && (level<=2 || inReplaced && level==3) {
   if err := closeEntry(); err != nil { return nil,err }
   var words strings.Builder
   pmdoc.Walk(node,func(n *pmdoc.Node) bool { if n.Type=="text" { words.WriteString(n.Text) }; return true })
   heading:=words.String()
   if level<=2 { inReplaced=level==2 && heading=="Replaced" }
   if level==2 && !inReplaced || level==3 && inReplaced { title=heading; replaced=inReplaced }
   if level==1 && heading!="Decisions" { title="Imported decision context";replaced=false;children=append(children,node) }
   continue
  }
  if title=="" {title="Imported decision context";replaced=false}
  children=append(children,node)
 }
 if err := closeEntry(); err != nil { return nil,err }
 return entries,nil
}

// Markdown is the preserved agent choice as one typed node. A from_ask relation supplies
// provenance only: it never changes this agent choice into a person's decision.
func (e Entry) Markdown(askReference func(string) string) (string, error) {
 attrs:=pmdoc.Attrs{pmdoc.BlockIDAttr:e.ID,"title":e.Title,"replaced":e.Replaced,"shaped":e.Shaped}
 if len(e.Asks)>0 { attrs["from_ask"]=askReference(e.Asks[0]) }
 body,err:=pmdoc.ParseRendering(e.Body)
 if err!=nil { return "", err }
 if len(body.Children)==0 { body.Children=[]*pmdoc.Node{{Type:"paragraph"}} }
 node:=&pmdoc.Node{Type:"decision",Attrs:attrs,Children:body.Children}
 return pmdoc.Render(&pmdoc.Node{Type:"doc",Children:[]*pmdoc.Node{node}})
}
