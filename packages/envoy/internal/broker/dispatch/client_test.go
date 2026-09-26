package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCreateAndGetAsk(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/issues/AGENTC-1/asks":
			json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"ask-1","state":"open","edited_at":null,"answer":null}`))
		case "GET /api/v1/asks/ask-1":
			w.Write([]byte(`{"id":"ask-1","state":"answered","edited_at":null,"answer":{"user":"sjawhar","selected":["Approve"],"text":null,"at":"2026-09-26T05:00:00Z"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok", srv.Client())
	ask, err := c.CreateAsk(context.Background(), "AGENTC-1", "Release DEEL_API_KEY?", []Option{{Label: "Approve"}, {Label: "Deny"}}, "med")
	if err != nil || ask.ID != "ask-1" {
		t.Fatalf("create: %+v %v", ask, err)
	}
	if gotBody["question"] != "Release DEEL_API_KEY?" || gotBody["urgency"] != "med" || gotBody["multiple"] != false {
		t.Fatalf("body: %v", gotBody)
	}
	actor, _ := gotBody["actor"].(map[string]any)
	if actor["kind"] != "session" || actor["id"] != "agent-secrets-broker" {
		t.Fatalf("actor: %v", gotBody["actor"])
	}
	opts, _ := gotBody["options"].([]any)
	if len(opts) != 2 {
		t.Fatalf("options: %v", gotBody["options"])
	}
	first, _ := opts[0].(map[string]any)
	second, _ := opts[1].(map[string]any)
	if first["label"] != "Approve" || second["label"] != "Deny" {
		t.Fatalf("options: %v", gotBody["options"])
	}
	ask, err = c.GetAsk(context.Background(), "ask-1")
	if err != nil || ask.State != "answered" || ask.Answer == nil || ask.Answer.User != "sjawhar" || ask.Answer.Selected[0] != "Approve" {
		t.Fatalf("get: %+v %v", ask, err)
	}
	if !ask.Answer.At.Equal(time.Date(2026, 9, 26, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("answer at: %v", ask.Answer.At)
	}
}
