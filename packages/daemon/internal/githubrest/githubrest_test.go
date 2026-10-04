package githubrest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Post sends its body as JSON with the installation token, reads a 2xx answer such as GitHub's
// 201 Created, and gives any other answer back as an *Answer naming the method, the path and the
// status, so a caller can tell GitHub's refusal from a failed request.
func TestPostSendsJSONAndAnswersWithTheStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sent struct {
			Ref string `json:"ref"`
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Content-Type") != "application/json" ||
			json.NewDecoder(r.Body).Decode(&sent) != nil {
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
			return
		}
		if sent.Ref == "refs/heads/taken" {
			http.Error(w, `{"message":"Reference already exists"}`, http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ref":"` + sent.Ref + `"}`))
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	var created struct {
		Ref string `json:"ref"`
	}
	if err := client.Post(context.Background(), "/git/refs", map[string]string{"ref": "refs/heads/new"}, &created); err != nil || created.Ref != "refs/heads/new" {
		t.Fatalf("Post = %+v, %v; want the 201 answer read", created, err)
	}
	err := client.Post(context.Background(), "/git/refs?x=1", map[string]string{"ref": "refs/heads/taken"}, nil)
	var answer *Answer
	if !errors.As(err, &answer) || answer.Method != http.MethodPost || answer.Path != "/git/refs" || answer.Status != http.StatusUnprocessableEntity {
		t.Fatalf("Post of a taken ref = %v, want an *Answer for POST /git/refs with 422", err)
	}
}

// A list GitHub answers as an object's field is read from that field, and an answer without it is
// an error naming the field, never an empty list a caller would read as nothing required or run.
func TestGetListPagesRefusesAnAnswerWithoutTheField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/actions/runs" {
			w.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":37}]}`))
			return
		}
		w.Write([]byte(`{"total_count":0,"message":"no list here"}`))
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	type run struct {
		ID int `json:"id"`
	}
	if runs, err := GetListPages[run](context.Background(), client, "/actions/runs", "workflow_runs"); err != nil || len(runs) != 1 || runs[0].ID != 37 {
		t.Fatalf("GetListPages = %+v, %v; want the one run read from workflow_runs", runs, err)
	}
	runs, err := GetListPages[run](context.Background(), client, "/commits/head/check-runs", "check_runs")
	if err == nil || runs != nil || err.Error() != `GitHub answered GET /commits/head/check-runs with no "check_runs" list` {
		t.Fatalf("GetListPages of an answer without the field = %+v, %v; want an error naming check_runs", runs, err)
	}
}
