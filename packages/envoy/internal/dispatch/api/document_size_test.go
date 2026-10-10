package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// renderWatch is a document service that records, while watching, every read that renders a
// document: its text, its blocks, or where one of its blocks stands.
type renderWatch struct {
	docs.API
	mu       sync.Mutex
	watching bool
	renders  []string
}

func (w *renderWatch) watch(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watching = on
}

func (w *renderWatch) record(read, artifactID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.watching {
		w.renders = append(w.renders, read+" "+artifactID)
	}
}

func (w *renderWatch) Text(ctx context.Context, artifactID string) (string, error) {
	w.record("Text", artifactID)
	return w.API.Text(ctx, artifactID)
}

func (w *renderWatch) TextWithToken(ctx context.Context, artifactID string) (string, string, error) {
	w.record("TextWithToken", artifactID)
	return w.API.TextWithToken(ctx, artifactID)
}

func (w *renderWatch) Blocks(ctx context.Context, artifactID string) ([]model.ArtifactBlock, error) {
	w.record("Blocks", artifactID)
	return w.API.Blocks(ctx, artifactID)
}

func (w *renderWatch) TextWithBlocks(ctx context.Context, artifactID string) (string, []model.ArtifactBlock, error) {
	w.record("TextWithBlocks", artifactID)
	return w.API.TextWithBlocks(ctx, artifactID)
}

func (w *renderWatch) BlockPath(ctx context.Context, artifactID, blockID string) (model.BlockPath, error) {
	w.record("BlockPath", artifactID)
	return w.API.BlockPath(ctx, artifactID, blockID)
}

// requireSize fails the test unless version carries want as its stored byte size.
func requireSize(t *testing.T, read string, version model.Version, want int) {
	t.Helper()
	if version.Size == nil {
		t.Fatalf("%s: version %d carries no size, want %d", read, version.Number, want)
	}
	if *version.Size != want {
		t.Fatalf("%s: version %d size = %d, want %d", read, version.Number, *version.Size, want)
	}
}

// The artifact list, a document's metadata and each version's metadata say how big the stored
// document is, so a person or a monitor can find one near the cap without the read that renders
// it. A document 101 bytes under the cap is listed with its markdown's byte size on every route
// that lists or reads its versions, and none of those reads renders it or loads its stored
// history: the document service saw no render, and a load failure armed before the reads is still
// waiting for the first read that loads the document.
func TestArtifactListsGiveANearCapDocumentsStoredSizeWithoutRenderingIt(t *testing.T) {
	var broken *invalidLoadOnceStore
	watch := &renderWatch{}
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		broken = &invalidLoadOnceStore{VersionedStore: docs.NewPgVersioned(database), invalid: make(map[string]bool)}
		service := docs.New(docs.Deps{Store: database, Persistence: broken, Settle: time.Hour})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		watch.API = service
		return watch
	})
	const spec = "Spec.\n"
	issue := createInteractionIssue(t, handler, "TEST", "Document sizes", spec)
	// One paragraph line of words is its own canonical rendering, so the stored markdown is
	// exactly what was sent.
	nearCap := strings.Repeat("word ", (pmdoc.MaxDocumentBytes-100)/5)
	nearCap = nearCap[:len(nearCap)-1] + "\n"
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "near-cap.md", "content": nearCap,
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the near-cap document: status=%d body=%.300s", uploaded.Code, uploaded.Body.String())
	}
	document := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	var stored bool
	if err := database.Pool.QueryRow(context.Background(), `
		select markdown = $2 from artifact_versions where artifact_id = $1 and number = 1
	`, document.ID, nearCap).Scan(&stored); err != nil || !stored {
		t.Fatalf("the stored version is not the uploaded markdown (equal=%v, %v)", stored, err)
	}

	// No room holds either document now, so a read that renders one loads its stored history.
	if err := watch.Quiesce(context.Background()); err != nil {
		t.Fatalf("close the live documents: %v", err)
	}
	broken.failNextLoad(document.ID)
	watch.watch(true)

	find := func(read string, artifacts []model.Artifact, id string) model.Artifact {
		t.Helper()
		for _, artifact := range artifacts {
			if artifact.ID == id {
				return artifact
			}
		}
		t.Fatalf("%s does not list artifact %s", read, id)
		return model.Artifact{}
	}
	read := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%.300s", path, response.Code, response.Body.String())
		}
		return response
	}
	only := func(read string, artifact model.Artifact) model.Version {
		t.Helper()
		if len(artifact.Versions) != 1 {
			t.Fatalf("%s: %s has %d versions, want 1", read, artifact.Name, len(artifact.Versions))
		}
		return artifact.Versions[0]
	}

	issueList := "/api/v1/issues/" + issue.Key + "/artifacts"
	listed := decodeBody[[]model.Artifact](t, read(issueList))
	requireSize(t, issueList, only(issueList, find(issueList, listed, document.ID)), len(nearCap))
	requireSize(t, issueList, only(issueList, find(issueList, listed, issue.PrimaryArtifactID)), len(spec))

	projectList := "/api/v1/projects/TEST/artifacts"
	requireSize(t, projectList, only(projectList, find(projectList, decodeBody[[]model.Artifact](t, read(projectList)), document.ID)), len(nearCap))

	issueRead := "/api/v1/issues/" + issue.Key
	detail := decodeBody[struct {
		Artifacts []model.Artifact `json:"artifacts"`
	}](t, read(issueRead))
	requireSize(t, issueRead, only(issueRead, find(issueRead, detail.Artifacts, document.ID)), len(nearCap))

	for _, path := range []string{
		"/api/v1/artifacts/" + document.ID,
		"/api/v1/issues/" + issue.Key + "/artifacts/" + document.Slug,
	} {
		requireSize(t, path, only(path, decodeBody[model.Artifact](t, read(path))), len(nearCap))
	}

	for _, path := range []string{
		"/api/v1/artifacts/" + document.ID + "/versions/1",
		"/api/v1/issues/" + issue.Key + "/artifacts/" + document.Slug + "/versions/1",
	} {
		requireSize(t, path, decodeBody[model.Version](t, read(path)), len(nearCap))
	}

	watch.watch(false)
	if len(watch.renders) != 0 {
		t.Fatalf("reading the documents' sizes rendered them: %v", watch.renders)
	}
	unloadable := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document.ID+"/text", nil, "alice")
	if unloadable.Code != http.StatusConflict || !strings.Contains(unloadable.Body.String(), `"code":"DOCUMENT_UNLOADABLE"`) {
		t.Fatalf("the first read that loads the document: status=%d body=%.300s, want the load failure armed before the size reads, 409 DOCUMENT_UNLOADABLE", unloadable.Code, unloadable.Body.String())
	}
}

// Every version a write answers, every version its event carries, and every version a reference
// read lists carry the version's stored byte size: an upload's, a named version's, an edit's named
// version and its artifact.version event, and the cited document's versions on an issue's
// reference closure and on a document's outgoing references.
func TestEveryVersionAWriteOrAReferenceReadAnswersCarriesItsStoredSize(t *testing.T) {
	handler := newTestHandler(t)
	const cited = "Spec.\n"
	target := createInteractionIssue(t, handler, "TEST", "Transport decision", cited)
	citing := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST", "title": "Rollout plan", "spec": "See dispatch://" + target.Key + "/artifact/spec.\n",
	}, "alice")
	if citing.Code != http.StatusCreated {
		t.Fatalf("create the citing issue: status=%d body=%s", citing.Code, citing.Body.String())
	}
	source := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, citing)

	const notes = "# Notes\n"
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+target.Key+"/artifacts", map[string]string{
		"name": "notes.md", "content": notes,
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	upload := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
		Version  model.Version  `json:"version"`
	}](t, uploaded)
	requireSize(t, "the upload's answer", upload.Version, len(notes))

	named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+upload.Artifact.ID+"/versions", map[string]string{
		"summary": "Named",
	}, "alice")
	if named.Code != http.StatusCreated {
		t.Fatalf("name a version: status=%d body=%s", named.Code, named.Body.String())
	}
	requireSize(t, "the named version's answer", decodeBody[model.Version](t, named), len(notes))

	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+upload.Artifact.ID+"/edits", map[string]any{
		"actor":   sessionActor(),
		"ops":     []map[string]string{{"op": "insert", "after": "end", "markdown": "More.\n"}},
		"summary": "Grew",
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
	editVersion := decodeBody[struct {
		Version *model.Version `json:"version"`
	}](t, edited).Version
	if editVersion == nil {
		t.Fatal("the edit with a summary answered no version")
	}
	grown := documentMarkdown(t, handler, upload.Artifact.ID)
	requireSize(t, "the edit's answer", *editVersion, len(grown))

	events := decodeBody[[]struct {
		Type    string `json:"type"`
		Payload struct {
			ArtifactID string        `json:"artifact_id"`
			Version    model.Version `json:"version"`
		} `json:"payload"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+target.Key+"/events", nil, "alice"))
	carried := false
	for _, event := range events {
		if event.Type == "artifact.version" && event.Payload.ArtifactID == upload.Artifact.ID && event.Payload.Version.Number == editVersion.Number {
			requireSize(t, "the edit's artifact.version event", event.Payload.Version, len(grown))
			carried = true
		}
	}
	if !carried {
		t.Fatalf("no artifact.version event for version %d of %s", editVersion.Number, upload.Artifact.ID)
	}

	closure := decodeBody[model.IssueReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+source.Key+"/references", nil, "alice"))
	if len(closure.Members) != 1 || len(closure.Members[0].Artifact.Versions) != 1 {
		t.Fatalf("the citing issue's reference closure = %+v, want the cited spec with its one version", closure)
	}
	requireSize(t, "the reference closure", closure.Members[0].Artifact.Versions[0], len(cited))

	outgoing := decodeBody[model.ArtifactReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+source.PrimaryArtifactID+"/references", nil, "alice"))
	if len(outgoing.Outgoing) != 1 || outgoing.Outgoing[0].Artifact == nil || len(outgoing.Outgoing[0].Artifact.Versions) != 1 {
		t.Fatalf("the citing spec's outgoing references = %+v, want the cited spec with its one version", outgoing)
	}
	requireSize(t, "the outgoing references", outgoing.Outgoing[0].Artifact.Versions[0], len(cited))
}
