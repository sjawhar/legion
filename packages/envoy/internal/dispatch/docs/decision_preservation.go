package docs

import (
 "context"
 "fmt"
 "sort"

 "github.com/sjawhar/envoy/internal/dispatch/model"
 "github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The ask row, not the editable copy of its state attributes, decides whether a person's
// answer is protected. Read before taking a Yjs transaction lock; no database call runs while
// the tree comparison holds it. A human can deliberately edit the spec; an agent cannot erase its answered asks.
func (s *Service) agentAnsweredDecisions(ctx context.Context,artifactID string,actor model.Actor)(map[string]uint8,error){
 if actor.Kind!="session"{return nil,nil}
 rows,err:=s.queryFrom(ctx).Query(ctx,`select a.block_id from asks a join artifacts d on d.id=a.block_artifact_id
 where d.id=$1::uuid and d.is_primary and a.state='answered' and a.block_id is not null`,artifactID)
 if err!=nil{return nil,err};defer rows.Close()
 var protected map[string]uint8
 for rows.Next(){var id string;if err:=rows.Scan(&id);err!=nil{return nil,err};if protected==nil{protected=map[string]uint8{}};protected[id]=0}
 return protected,rows.Err()
}

func refuseLostAnsweredDecisions(protected map[string]uint8,current,next *pmdoc.Node)error{
 if len(protected)==0{return nil}
 for id:=range protected{protected[id]=0}
 if current==nil{for id:=range protected{protected[id]=1}}
 for index,tree:=range []*pmdoc.Node{current,next}{
  if tree==nil{continue}
  flag:=uint8(1<<index)
  pmdoc.Walk(tree,func(node *pmdoc.Node)bool{
   if node.Type=="ask"{id,_:=node.Attrs[pmdoc.BlockIDAttr].(string);if prior,found:=protected[id];found{protected[id]=prior|flag}}
   return true
  })
 }
 var removed []string
 for id,state:=range protected{if state==1{removed=append(removed,id)}}
 if len(removed)==0{return nil}
 sort.Strings(removed)
 return &ErrInvalidAskBlock{Reason:fmt.Errorf("agent edit removes a person's answered decision %v from its spec; retain the ask node and its answer",removed)}
}
