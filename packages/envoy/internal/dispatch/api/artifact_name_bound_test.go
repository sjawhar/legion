package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// nameBoundTarget is one route that creates an artifact under a name, the owner its reference key
// `<owner>/<slug>` starts with, and how an upload under name reaches it.
type nameBoundTarget struct {
	name, base, owner string
	upload            func(t *testing.T, handler http.Handler, base, name string) *httptest.ResponseRecorder
}

func nameBoundTargets(issueKey, session string) []nameBoundTarget {
	document := func(t *testing.T, handler http.Handler, base, name string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, base, map[string]any{"name": name, "content": "# Notes\n"}, "alice")
	}
	file := func(t *testing.T, handler http.Handler, base, name string) *httptest.ResponseRecorder {
		return multipartRequest(t, handler, base, map[string]string{"name": name}, "notes.bin", "application/octet-stream", []byte("bytes"), "alice")
	}
	picture := func(t *testing.T, handler http.Handler, base, name string) *httptest.ResponseRecorder {
		return multipartRequest(t, handler, base, map[string]string{"name": name}, "photo.png", "image/png", pngBytes("photo"), "alice")
	}
	return []nameBoundTarget{
		{"an issue's document, JSON", "/api/v1/issues/" + issueKey + "/artifacts", issueKey, document},
		{"an issue's file, multipart", "/api/v1/issues/" + issueKey + "/artifacts", issueKey, file},
		{"a project's document, JSON", "/api/v1/projects/CORE/artifacts", "CORE", document},
		{"a project's file, multipart", "/api/v1/projects/CORE/artifacts", "CORE", file},
		{"an agent's conversation's picture, multipart", "/api/v1/agents/" + session + "/artifacts", "agent/" + session, picture},
	}
}

// A new artifact whose name is past maxNewArtifactKeyBytes - 3000 incompressible characters, which
// main fails inside Postgres with a 500 because artifacts_ref_key_key cannot hold its key - or whose
// name fits but gives a reference key `<owner>/<slug>` past it, by its own length or by the suffix
// a taken slug adds, is 400 INVALID_ARTIFACT naming the bound in bytes, on every route that creates
// one, and writes nothing. A name whose key is exactly the bound is stored, read back by its slug,
// and takes a second version.
func TestANewArtifactNamePastTheBoundIsRefused(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "CORE", "Long artifact names", "Context paragraph.\n")
	const session = "session-0123456789abcdef"
	bound := fmt.Sprintf("past the %d bytes", maxNewArtifactKeyBytes)
	refused := func(t *testing.T, what string, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusBadRequest || responseCode(t, response) != "INVALID_ARTIFACT" || !strings.Contains(response.Body.String(), bound) {
			t.Fatalf("%s: status=%d body=%.400s, want 400 INVALID_ARTIFACT %q", what, response.Code, response.Body.String(), bound)
		}
	}
	// What an upload writes; a document the test stored before settles in the background, which
	// writes doc_settlements_pending whenever it likes.
	stored := func(t *testing.T) map[string]string {
		t.Helper()
		all := databaseFingerprint(t, database)
		return map[string]string{"artifacts": all["artifacts"], "artifact_versions": all["artifact_versions"], "events": all["events"], "refs": all["refs"]}
	}

	for _, target := range nameBoundTargets(issue.Key, session) {
		t.Run(target.name, func(t *testing.T) {
			// The hex names slug to themselves, so a name of the bound less `<owner>/` gives a key of
			// exactly the bound.
			atBound := randomBlockID(t, maxNewArtifactKeyBytes-len(target.owner+"/"))
			before := stored(t)
			refused(t, "a name of 3000 characters", target.upload(t, handler, target.base, randomBlockID(t, 3000)))
			refused(t, "a name one byte past the bound", target.upload(t, handler, target.base, randomBlockID(t, maxNewArtifactKeyBytes+1)))
			refused(t, "a name whose key is one byte past the bound", target.upload(t, handler, target.base, atBound+"a"))
			assertUnchanged(t, "a refused name", before, stored(t))

			created := target.upload(t, handler, target.base, atBound)
			if created.Code != http.StatusCreated {
				t.Fatalf("a name whose key is the bound: status=%d body=%.400s, want 201", created.Code, created.Body.String())
			}
			artifact := decodeBody[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, created).Artifact
			if len(artifact.RefKey) != maxNewArtifactKeyBytes {
				t.Fatalf("the reference key is %d bytes, want the bound %d", len(artifact.RefKey), maxNewArtifactKeyBytes)
			}
			if read := dispatchRequest(t, handler, http.MethodGet, target.base+"/"+artifact.Slug, nil, "alice"); read.Code != http.StatusOK || decodeBody[model.Artifact](t, read).ID != artifact.ID {
				t.Fatalf("read the artifact at the bound by its slug: status=%d body=%.400s", read.Code, read.Body.String())
			}
			again := target.upload(t, handler, target.base, atBound)
			if again.Code != http.StatusCreated {
				t.Fatalf("upload the name at the bound again: status=%d body=%.400s", again.Code, again.Body.String())
			}
			if second := decodeBody[struct {
				Artifact model.Artifact `json:"artifact"`
				Version  model.Version  `json:"version"`
			}](t, again); second.Artifact.ID != artifact.ID || second.Version.Number != 2 {
				t.Fatalf("the second upload stored artifact %s version %d, want %s version 2", second.Artifact.ID, second.Version.Number, artifact.ID)
			}

			// Another name slugging to the same slug takes it with a suffix, which would put the key
			// past the bound.
			before = stored(t)
			refused(t, "a name whose slug, suffixed, puts the key past the bound", target.upload(t, handler, target.base, atBound+"!"))
			assertUnchanged(t, "a refused suffixed slug", before, stored(t))
		})
	}
}

// An artifact stored before the bound under a name and a reference key past it - 5000 characters,
// which main stores, since Postgres compresses the repeated key into artifacts_ref_key_key - takes
// writes as before: an upload under its name stores a new version, an edit stores another, and the
// artifact reads.
func TestAnArtifactStoredPastTheNameBoundTakesVersionsAndEdits(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "CORE", "Stored long name", "Context paragraph.\n")
	ctx := context.Background()
	long := strings.Repeat("a", 5000)
	name := long + ".md"
	var id string
	var keyBytes int
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ($1, 'CORE', $2, $3, 'doc', '{"kind":"user","id":"alice"}')
		returning id::text, length(ref_key)
	`, issue.Key, long+"-md", name).Scan(&id, &keyBytes); err != nil {
		t.Fatalf("store an artifact past the bound: %v", err)
	}
	if keyBytes <= maxNewArtifactKeyBytes {
		t.Fatalf("the stored reference key is %d bytes, want past the bound %d", keyBytes, maxNewArtifactKeyBytes)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, 'Written before the bound.\n', '[{"kind":"user","id":"alice"}]')
	`, id); err != nil {
		t.Fatalf("store its first version: %v", err)
	}

	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": name, "content": "Uploaded after the bound.\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload under the stored long name: status=%d body=%.400s, want 201", uploaded.Code, uploaded.Body.String())
	}
	if version := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
		Version  model.Version  `json:"version"`
	}](t, uploaded); version.Artifact.ID != id || version.Version.Number != 2 {
		t.Fatalf("the upload stored artifact %s version %d, want %s version 2", version.Artifact.ID, version.Version.Number, id)
	}
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+id+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "An agent's line."}},
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("edit the long-named artifact: status=%d body=%.400s", edited.Code, edited.Body.String())
	}
	var versions int
	if err := database.Pool.QueryRow(ctx, `select count(*) from artifact_versions where artifact_id = $1`, id).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 3 {
		t.Fatalf("the long-named artifact has %d versions, want 3: the stored one, the upload's and the edit's", versions)
	}
	if read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+id, nil, "alice"); read.Code != http.StatusOK {
		t.Fatalf("read the long-named artifact: status=%d body=%.400s", read.Code, read.Body.String())
	}
}
