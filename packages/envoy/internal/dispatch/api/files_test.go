package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fileIssue creates the project and issue a file upload lands on and returns the issue key.
func fileIssue(t *testing.T, handler http.Handler) string {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "FILES", "name": "Files",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "FILES", "title": "Issue", "spec": "# Files",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, response).Key
}

type uploaded struct {
	Artifact struct {
		ID string `json:"id"`
	} `json:"artifact"`
	Version struct {
		Number int    `json:"number"`
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	} `json:"version"`
}

// versionRow is what a version's row holds of its bytes: nil when they live in the store.
func versionRow(t *testing.T, database *store.Store, artifactID string, number int) []byte {
	t.Helper()
	var content []byte
	if err := database.Pool.QueryRow(context.Background(), `select content from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&content); err != nil {
		t.Fatalf("read version row: %v", err)
	}
	return content
}

func TestUploadsGoToTheFileStoreAndAreServedFromIt(t *testing.T) {
	memory := files.NewMemory()
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)
	image := bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 4096)
	sha := files.SHA256(image)

	first := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.png"}, "shot.png", "image/png", image, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", first.Code, first.Body.String())
	}
	version := decodeBody[uploaded](t, first)
	if version.Version.SHA256 != sha || version.Version.Size != len(image) {
		t.Fatalf("version = %+v, want sha %s and size %d", version.Version, sha, len(image))
	}
	if row := versionRow(t, database, version.Artifact.ID, version.Version.Number); row != nil {
		t.Fatalf("the row holds %d bytes; with a store it should hold none", len(row))
	}
	if memory.Len() != 1 || memory.MIME(sha) != "image/png" {
		t.Fatalf("store holds %d objects (mime %q), want the one image as image/png", memory.Len(), memory.MIME(sha))
	}

	// The same bytes again, as a second version of the same name: one object, two rows.
	second := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.png"}, "shot.png", "image/png", image, "alice")
	if second.Code != http.StatusCreated {
		t.Fatalf("second upload: status=%d body=%s", second.Code, second.Body.String())
	}
	if memory.Len() != 1 || memory.Puts() != 1 {
		t.Fatalf("after a duplicate upload the store holds %d objects from %d writes, want 1 and 1", memory.Len(), memory.Puts())
	}

	// The version route answers from the store with the headers it always sent.
	served := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil, "alice")
	if served.Code != http.StatusOK || !bytes.Equal(served.Body.Bytes(), image) {
		t.Fatalf("serve version: status=%d, %d body bytes, want 200 with the image", served.Code, served.Body.Len())
	}
	headers := served.Header()
	if headers.Get("Content-Type") != "image/png" || headers.Get("ETag") != sha || headers.Get("Content-Disposition") != "attachment" || headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("serve version headers = %v", headers)
	}
	bySlug := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/artifacts/shot-png/versions/2", nil, "alice")
	if bySlug.Code != http.StatusOK || !bytes.Equal(bySlug.Body.Bytes(), image) {
		t.Fatalf("serve version by slug: status=%d, %d body bytes", bySlug.Code, bySlug.Body.Len())
	}
}

func TestAnUploadTheStoreRefusesLeavesNoVersionBehind(t *testing.T) {
	memory := files.NewMemory()
	memory.Fail = errors.New("bucket unreachable")
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)

	response := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "notes.bin"}, "notes.bin", "application/octet-stream", []byte("payload"), "alice")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("upload against a failing store: status=%d body=%s", response.Code, response.Body.String())
	}
	if code := decodeBody[struct {
		Code string `json:"code"`
	}](t, response).Code; code != "FILE_STORE_UNAVAILABLE" {
		t.Fatalf("error code = %q, want FILE_STORE_UNAVAILABLE", code)
	}
	var artifacts int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifacts where issue_key = $1 and kind <> 'doc'`, issue).Scan(&artifacts); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if artifacts != 0 {
		t.Fatalf("%d file artifacts exist after a refused upload, want none", artifacts)
	}
}

func TestARowStillHoldingItsBytesIsServedFromTheRow(t *testing.T) {
	// An upload made before the store existed keeps its bytes in the row; the store configured
	// later holds nothing for it, and the route serves the row.
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := fileIssue(t, handler)
	legacy := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "old.bin"}, "old.bin", "application/octet-stream", []byte("legacy bytes"), "alice")
	if legacy.Code != http.StatusCreated {
		t.Fatalf("legacy upload: status=%d body=%s", legacy.Code, legacy.Body.String())
	}
	version := decodeBody[uploaded](t, legacy)
	if versionRow(t, database, version.Artifact.ID, 1) == nil {
		t.Fatal("an upload with no store left no bytes in the row")
	}

	memory := files.NewMemory()
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: headerIdentity(database), AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: "https://dispatch.example", Files: memory,
	})
	if err != nil {
		t.Fatalf("new deps with a store: %v", err)
	}
	withStore := http.NewServeMux()
	Register(withStore, deps)
	served := dispatchRequest(t, withStore, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil, "alice")
	if served.Code != http.StatusOK || served.Body.String() != "legacy bytes" {
		t.Fatalf("serve a row-held version with a store configured: status=%d body=%q", served.Code, served.Body.String())
	}
	if memory.Len() != 0 {
		t.Fatalf("serving a row-held version wrote %d objects to the store", memory.Len())
	}
}

func TestAClearedRowWhoseObjectIsMissingAnswers502(t *testing.T) {
	memory := files.NewMemory()
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "gone.bin"}, "gone.bin", "application/octet-stream", []byte("gone bytes"), "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
	version := decodeBody[uploaded](t, response)
	memory.Delete(version.Version.SHA256)

	served := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil, "alice")
	if served.Code != http.StatusBadGateway {
		t.Fatalf("serve a version whose object is gone: status=%d body=%s", served.Code, served.Body.String())
	}
	if served.Header().Get("Content-Disposition") != "" || served.Header().Get("ETag") != "" {
		t.Fatalf("a failed read sent the success headers: %v", served.Header())
	}

	// Without a store at all, a cleared row is a configuration the server names: 503.
	noStore, err := NewDeps(DepsInput{
		Store: database, Identity: headerIdentity(database), AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: "https://dispatch.example",
	})
	if err != nil {
		t.Fatalf("new deps without a store: %v", err)
	}
	withoutStore := http.NewServeMux()
	Register(withoutStore, noStore)
	unconfigured := dispatchRequest(t, withoutStore, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil, "alice")
	if unconfigured.Code != http.StatusServiceUnavailable {
		t.Fatalf("serve a cleared row with no store: status=%d body=%s", unconfigured.Code, unconfigured.Body.String())
	}
}
