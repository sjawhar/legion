package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const agentSession = "01a1058e-f14f-7684-87eb-3dc885955551"

// A picture pasted into a direct message is stored with the agent's conversation: it belongs to no
// issue or project, is addressed under the session, served by slug with the bytes' long-lived
// cache header, listed newest first, and cited from a message as any artifact is.
func TestAnAgentsConversationOwnsTheFilesUploadedToIt(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	image := bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 1024)
	base := "/api/v1/agents/" + agentSession + "/artifacts"

	response := multipartRequest(t, handler, base, map[string]string{"name": "shot.png"}, "shot.png", "image/png", image, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
		Version  model.Version  `json:"version"`
	}](t, response)
	artifact := created.Artifact
	if artifact.SessionID == nil || *artifact.SessionID != agentSession || artifact.IssueKey != nil ||
		artifact.Project != "" || artifact.RefKey != "agent/"+agentSession+"/shot-png" || artifact.Kind != "image" {
		t.Fatalf("artifact = %+v, want an image owned by session %s alone at agent/%s/shot-png", artifact, agentSession, agentSession)
	}
	if !strings.Contains(response.Body.String(), `"session_id":"`+agentSession+`"`) || !strings.Contains(response.Body.String(), `"issue_key":null`) {
		t.Fatalf("upload body %s, want session_id set and issue_key null", response.Body.String())
	}

	served := dispatchRequest(t, handler, http.MethodGet, base+"/shot-png/versions/1", nil, "alice")
	if served.Code != http.StatusOK || !bytes.Equal(served.Body.Bytes(), image) {
		t.Fatalf("serve by slug: status=%d, %d body bytes, want 200 with the image", served.Code, served.Body.Len())
	}
	if got := served.Header().Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
		t.Errorf("Cache-Control %q, want private, max-age=31536000, immutable", got)
	}
	if got := served.Header().Get("ETag"); got != files.SHA256(image) {
		t.Errorf("ETag %q, want the bytes' sha256", got)
	}
	byID := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/versions/1", nil, "alice")
	if byID.Code != http.StatusOK || !bytes.Equal(byID.Body.Bytes(), image) {
		t.Fatalf("serve by id: status=%d, %d body bytes", byID.Code, byID.Body.Len())
	}

	// The same name again is the next version, as on an issue; another name is another artifact,
	// listed first.
	if again := multipartRequest(t, handler, base, map[string]string{"name": "shot.png"}, "shot.png", "image/png", []byte("second"), "alice"); again.Code != http.StatusCreated {
		t.Fatalf("second version: status=%d body=%s", again.Code, again.Body.String())
	}
	if other := multipartRequest(t, handler, base, map[string]string{"name": "notes.txt"}, "notes.txt", "text/plain", []byte("plain"), "alice"); other.Code != http.StatusCreated {
		t.Fatalf("second artifact: status=%d body=%s", other.Code, other.Body.String())
	}
	read := decodeBody[model.Artifact](t, dispatchRequest(t, handler, http.MethodGet, base+"/shot-png", nil, "alice"))
	if read.ID != artifact.ID || len(read.Versions) != 2 {
		t.Fatalf("read by slug = %+v, want %s with two versions", read, artifact.ID)
	}
	listed := decodeBody[[]model.Artifact](t, dispatchRequest(t, handler, http.MethodGet, base, nil, "alice"))
	if len(listed) != 2 || listed[0].Slug != "notes-txt" || listed[1].Slug != "shot-png" || len(listed[1].Versions) != 2 {
		t.Fatalf("list = %+v, want notes-txt then shot-png with two versions", listed)
	}
	if missing := dispatchRequest(t, handler, http.MethodGet, base+"/absent-png", nil, "alice"); missing.Code != http.StatusNotFound {
		t.Fatalf("an absent slug: status=%d body=%s, want 404", missing.Code, missing.Body.String())
	}

	// A message that shows the picture cites it, and the backlink resolves to the session's file.
	issue := fileIssue(t, handler)
	if message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{
		"body": "Here:\n\n![shot.png](dispatch://agent/" + agentSession + "/artifact/shot-png@v1)",
	}, "alice"); message.Code != http.StatusCreated {
		t.Fatalf("post message: status=%d body=%s", message.Code, message.Body.String())
	}
	backlinks := graphEdges(t, handler, url.Values{"to": {"dispatch://agent/" + agentSession + "/artifact/shot-png"}})
	if backlinks.Node.ID != artifact.ID || backlinks.Node.Ref != "dispatch://agent/"+agentSession+"/artifact/shot-png" {
		t.Fatalf("node = %+v, want artifact %s addressed under the session", backlinks.Node, artifact.ID)
	}
	if len(backlinks.Edges) != 1 || backlinks.Edges[0].Kind != "mentions" || backlinks.Edges[0].Node.Kind != "message" {
		t.Fatalf("edges = %+v, want the message's one mention", backlinks.Edges)
	}
}

// An agent's conversation holds files and images: a document is refused with the reason, before
// anything is written, as is a session id no reference could carry. Its artifacts take no
// comment or ask, which only an issue's or a project's documents hold.
func TestAnAgentsConversationRefusesADocumentAndAnInvalidSessionID(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	base := "/api/v1/agents/" + agentSession + "/artifacts"
	before := databaseFingerprint(t, database)

	for name, response := range map[string]*httptest.ResponseRecorder{
		"inline JSON":   dispatchRequest(t, handler, http.MethodPost, base, map[string]string{"name": "notes.md", "content": "# Notes"}, "alice"),
		"markdown file": multipartRequest(t, handler, base, map[string]string{"name": "notes.md"}, "notes.md", "text/markdown", []byte("# Notes"), "alice"),
	} {
		refusal := decodeBody[struct{ Code, Error string }](t, response)
		if response.Code != http.StatusBadRequest || refusal.Code != "ARTIFACT_INPUT" || refusal.Error != agentOwnsFilesOnly {
			t.Errorf("%s: %d %s %q, want 400 ARTIFACT_INPUT %q", name, response.Code, refusal.Code, refusal.Error, agentOwnsFilesOnly)
		}
	}
	for _, session := range []string{"a%2Fb", "a%3Fb", "a%23b", "a%20b", "a%09b"} {
		upload := multipartRequest(t, handler, "/api/v1/agents/"+session+"/artifacts", map[string]string{"name": "shot.png"}, "shot.png", "image/png", []byte("png"), "alice")
		if upload.Code != http.StatusBadRequest || responseCode(t, upload) != "INVALID_SESSION_ID" {
			t.Errorf("upload to %s: status=%d body=%s, want 400 INVALID_SESSION_ID", session, upload.Code, upload.Body.String())
		}
		for _, path := range []string{"", "/shot-png", "/shot-png/versions/1"} {
			read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/"+session+"/artifacts"+path, nil, "alice")
			if read.Code != http.StatusBadRequest || responseCode(t, read) != "INVALID_SESSION_ID" {
				t.Errorf("GET %s%s: status=%d body=%s, want 400 INVALID_SESSION_ID", session, path, read.Code, read.Body.String())
			}
		}
	}
	assertUnchanged(t, "a refused agent upload", before, databaseFingerprint(t, database))

	uploaded := multipartRequest(t, handler, base, map[string]string{"name": "shot.png"}, "shot.png", "image/png", []byte("png"), "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	id := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	for _, path := range []string{"/comments", "/asks"} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+id+path, map[string]any{"body": "Nice", "question": "Nice?"}, "alice")
		if response.Code != http.StatusBadRequest || responseCode(t, response) != "ARTIFACT_AGENT_OWNED" {
			t.Errorf("POST %s on a session's file: status=%d body=%s, want 400 ARTIFACT_AGENT_OWNED", path, response.Code, response.Body.String())
		}
	}
}
