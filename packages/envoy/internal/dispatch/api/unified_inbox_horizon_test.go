package api

import (
 "context"
 "net/http"
 "testing"

 "github.com/sjawhar/envoy/internal/dispatch/events"
 "github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestUnifiedInboxSnapshotContainsEverySafeEventAndTheCallersWrite(t *testing.T) {
 handler,database,_:=newTestServer(t,testServerOptions{})
 issue:=createInteractionIssue(t,handler,"CORE","Horizon","A spec")
 response:=sessionRequest(t,handler,http.MethodPost,"/api/v1/issues/"+issue.Key+"/asks",map[string]any{"question":"Visible before notification","actor":sessionActor()})
 if response.Code!=http.StatusCreated{t.Fatalf("create ask: %d %s",response.Code,response.Body.String())}
 ask:=decodeBody[model.Ask](t,response)
 horizon:=events.NewHorizon(database.Pool)
 safe,err:=horizon.Await(context.Background());if err!=nil{t.Fatal(err)}
 var eventID int64
 if err:=database.Pool.QueryRow(context.Background(),`select id from events where type='ask.opened' and payload->>'id'=$1`,ask.ID).Scan(&eventID);err!=nil{t.Fatal(err)}
 if safe<eventID{t.Fatalf("safe=%d event=%d",safe,eventID)}
 response=dispatchRequest(t,handler,http.MethodGet,"/api/v1/me/inbox",nil,"alice")
 if response.Code!=200{t.Fatalf("read: %d %s",response.Code,response.Body.String())}
 page:=decodeBody[unifiedInboxPage](t,response)
 if page.Total!=1||page.Rows[0].ID!=ask.ID{t.Fatalf("safe node/read-your-write missing: %+v",page)}
 // The named read observes a shared horizon, not an asynchronous inbox index.
 // A fresh observer may return an earlier H; that must not hide committed nodes.
}
