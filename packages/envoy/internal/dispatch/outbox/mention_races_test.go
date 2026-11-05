package outbox

import (
 "context"
 "errors"
 "reflect"
 "testing"
 "time"

 "github.com/sjawhar/envoy/internal/contracts"
 "github.com/sjawhar/envoy/internal/dispatch/model"
 "github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMentionFallbackNeverRacesAPendingOrPossiblyDeliveredAttempt(t *testing.T) {
 for _, tc := range []struct { name, state, failure string; pending, fallback bool }{
  {"sweeper owns lapsed claim", "pending", "", true, false},
  {"lost receipt", "failed", contracts.ReceiptTimeoutCause, false, false},
  {"capability refusal", "failed", "session reader (Reader) does not advertise steer", false, false},
  {"policy refusal", "failed", "envoy listener answered: forbidden", false, false},
  {"transport failure", "failed", "envoy listener: connection refused", false, true},
 } {
  t.Run(tc.name, func(t *testing.T) {
   database := storetest.Open(t)
   seedIssue(t, database, "T-1", nil)
   const id = "00000000-0000-0000-0000-000000000051"
   seedMentionDelivery(t, database, id, "reader", tc.state)
   if _, err := database.Pool.Exec(context.Background(), `update comment_deliveries set error=nullif($2,''), resolve_error=nullif($2,''), claimed_at=now()-interval '2 minutes', created_at=now()-interval '2 minutes' where comment_id=$1`, id, tc.failure); err != nil { t.Fatal(err) }
   publisher := &recordingPublisher{}
   event := model.Event{ID: 100, IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind:"session",ID:"writer"}, Payload: map[string]any{"id":id,"mentions":mentionsOf("reader")}, CreatedAt: time.Now()}
   err := publish(context.Background(), Deps{Store:database, Publisher:publisher}, event, "", routing{}, map[string]struct{}{})
   if tc.pending && !errors.Is(err,errMentionPending) || !tc.pending && err != nil { t.Fatalf("publish=%v",err) }
   want := []string{"notifications.dispatch.issue.T-1.comment.created"}
   if tc.fallback { want=append(want,"notifications.agent.reader") }
   if got:=topicsOf(publisher.all()); !reflect.DeepEqual(got,want) { t.Fatalf("topics=%v want=%v",got,want) }
  })
 }
}

func TestQueuedCommentUsesCreationBodyRecipients(t *testing.T) {
 database:=storetest.Open(t)
 seedIssue(t,database,"T-1",nil)
 const id="00000000-0000-0000-0000-000000000052"
 if _,err:=database.Pool.Exec(context.Background(),`with written as (insert into comments(id,issue_key,author,body) values ($1,'T-1','{"kind":"session","id":"writer"}','Edited dispatch://sessions/later') returning id) insert into refs(from_kind,from_id,to_kind,to_id) select 'comment',id::text,'session_mention','later' from written`,id);err!=nil {t.Fatal(err)}
 publisher:=&recordingPublisher{}
 event:=model.Event{ID:101,IssueKey:new("T-1"),Type:"comment.created",Actor:model.Actor{Kind:"session",ID:"writer"},Payload:map[string]any{"id":id,"body":"Original dispatch://sessions/original"},CreatedAt:time.Now()}
 if err:=publish(context.Background(),Deps{Store:database,Publisher:publisher},event,"",routing{},map[string]struct{}{});err!=nil {t.Fatal(err)}
 want:=[]string{"notifications.dispatch.issue.T-1.comment.created","notifications.agent.original"}
 if got:=topicsOf(publisher.all());!reflect.DeepEqual(got,want) {t.Fatalf("topics=%v want=%v",got,want)}
}

func TestMentionFallbackDoesNotDuplicateTheSameEventsRoleForward(t *testing.T) {
 database:=storetest.Open(t)
 seedIssue(t,database,"T-1",nil)
 publisher:=&recordingPublisher{}
 event:=model.Event{ID:102,IssueKey:new("T-1"),Type:"comment.created",Actor:model.Actor{Kind:"user",ID:"alice"},Notify:true,Payload:map[string]any{"id":"00000000-0000-0000-0000-000000000053","mentions":mentionsOf(roleHolder)},CreatedAt:time.Now()}
 if err:=publish(context.Background(),withHeldRoles(Deps{Store:database,Publisher:publisher}),event,"",routing{route:new("role:reviewer")},map[string]struct{}{});err!=nil {t.Fatal(err)}
 want:=[]string{"notifications.dispatch.issue.T-1.comment.created","notifications.role.reviewer"}
 if got:=topicsOf(publisher.all());!reflect.DeepEqual(got,want) {t.Fatalf("topics=%v want=%v",got,want)}
}
