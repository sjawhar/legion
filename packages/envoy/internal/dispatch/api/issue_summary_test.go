package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// summaryRead is a summary route's answer as both callers read it: a person by cookie and an
// agent by bearer, the two kinds of caller the full reads admit.
func summaryRead(t *testing.T, handler http.Handler, path string) map[string]*httptest.ResponseRecorder {
	t.Helper()
	return map[string]*httptest.ResponseRecorder{
		"cookie": dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"),
		"bearer": agentRequest(t, handler, http.MethodGet, path, nil, "agent-token"),
	}
}

// uploadedArtifact is an upload route's answer.
type uploadedArtifact struct {
	Artifact model.Artifact `json:"artifact"`
	Version  model.Version  `json:"version"`
}

// uploadFile is a multipart upload of bytes to an issue or a project, under the content type its
// client declares; what the upload stores it under is read from the bytes (artifactKind).
func uploadFile(t *testing.T, handler http.Handler, owner, name, contentType string, content []byte) uploadedArtifact {
	t.Helper()
	response := multipartRequest(t, handler, owner+"/artifacts", map[string]string{"name": name}, name, contentType, content, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload %s to %s: status=%d body=%s", name, owner, response.Code, response.Body.String())
	}
	return decodeBody[uploadedArtifact](t, response)
}

func uploadImage(t *testing.T, handler http.Handler, issueKey, name, pixels string) uploadedArtifact {
	t.Helper()
	return uploadFile(t, handler, "/api/v1/issues/"+issueKey, name, "image/png", pngBytes(pixels))
}

func uploadDocument(t *testing.T, handler http.Handler, owner, name, content string) uploadedArtifact {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, owner+"/artifacts", map[string]string{"name": name, "content": content}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload %s to %s: status=%d body=%s", name, owner, response.Code, response.Body.String())
	}
	return decodeBody[uploadedArtifact](t, response)
}

// The issue summary is what a reference to an issue or to one of its documents shows: the issue's
// key, project, title and status, and every artifact it has, in the order the full read lists them,
// each with its greatest version - a picture's and a document's with several versions too - and
// whether it is the issue's spec, which a reference to a block of the spec names.
func TestIssueSummaryCarriesTheIssueAndEveryDocumentsLatestVersion(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Summarised", "A spec")
	uploadImage(t, handler, issue.Key, "diagram.png", "first pixels")
	image := uploadImage(t, handler, issue.Key, "diagram.png", "second pixels")
	if image.Version.Number != 2 || image.Artifact.Kind != "image" {
		t.Fatalf("second upload of the picture = %#v version %d, want its version 2", image.Artifact, image.Version.Number)
	}
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nOne.\n")
	uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nTwo.\n")
	notes := uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nThree.\n")
	if notes.Version.Number != 3 {
		t.Fatalf("third upload of the document = version %d, want 3", notes.Version.Number)
	}

	full := dispatchRequest(t, handler, http.MethodGet, owner, nil, "alice")
	if full.Code != http.StatusOK {
		t.Fatalf("full read: status=%d body=%s", full.Code, full.Body.String())
	}
	read := decodeBody[struct {
		Artifacts []model.Artifact `json:"artifacts"`
	}](t, full)
	if len(read.Artifacts) != 3 {
		t.Fatalf("full read lists %d artifacts, want the spec, the picture and the notes", len(read.Artifacts))
	}
	want := make([]issueSummaryDocument, len(read.Artifacts))
	for index, artifact := range read.Artifacts {
		latest := artifact.Versions[len(artifact.Versions)-1].Number
		want[index] = issueSummaryDocument{ID: artifact.ID, Slug: artifact.Slug, Name: artifact.Name, Kind: artifact.Kind, LatestVersion: &latest, Primary: artifact.Primary}
	}

	for caller, response := range summaryRead(t, handler, owner+"/summary") {
		if response.Code != http.StatusOK {
			t.Fatalf("%s summary: status=%d body=%s", caller, response.Code, response.Body.String())
		}
		summary := decodeBody[issueSummary](t, response)
		if summary.Key != issue.Key || summary.Project != "TEST" || summary.Title != "Summarised" || summary.Status != model.IssueStatusTriage {
			t.Fatalf("%s summary = %#v, want the issue's key, project, title and status", caller, summary)
		}
		if len(summary.Documents) != len(want) {
			t.Fatalf("%s summary documents = %#v, want %#v", caller, summary.Documents, want)
		}
		for index, document := range summary.Documents {
			expected := want[index]
			if document.ID != expected.ID || document.Slug != expected.Slug || document.Name != expected.Name || document.Kind != expected.Kind ||
				document.Primary != expected.Primary || document.LatestVersion == nil || *document.LatestVersion != *expected.LatestVersion {
				t.Fatalf("%s summary document %d = %#v (latest %v), want %#v (latest %d)", caller, index, document, document.LatestVersion, expected, *expected.LatestVersion)
			}
		}
		byID := map[string]int{}
		var primary []string
		for _, document := range summary.Documents {
			byID[document.ID] = *document.LatestVersion
			if document.Primary {
				primary = append(primary, document.Slug)
			}
		}
		if byID[image.Artifact.ID] != 2 || byID[notes.Artifact.ID] != 3 {
			t.Fatalf("%s summary latest versions = %#v, want the picture at 2 and the notes at 3", caller, byID)
		}
		if len(primary) != 1 || primary[0] == notes.Artifact.Slug || primary[0] == image.Artifact.Slug {
			t.Fatalf("%s summary marks %v primary, want the spec alone", caller, primary)
		}
		// The summary carries no versions, approvals, children or asks: only what a reference shows.
		for _, absent := range []string{`"versions"`, `"approval"`, `"children"`, `"open_asks"`, `"route_status"`} {
			if strings.Contains(response.Body.String(), absent) {
				t.Fatalf("%s summary carries %s: %s", caller, absent, response.Body.String())
			}
		}
	}

	for caller, response := range summaryRead(t, handler, "/api/v1/issues/TEST-999/summary") {
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"NOT_FOUND"`) {
			t.Fatalf("%s summary of a missing issue: status=%d body=%s, want 404 NOT_FOUND", caller, response.Code, response.Body.String())
		}
	}
}

// A project document's summary resolves its reference as the project document read does - by
// slug, else by unambiguous filename - and answers its project, name, kind and latest version.
func TestProjectDocumentSummaryResolvesAsTheDocumentReadDoes(t *testing.T) {
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	owner := "/api/v1/projects/CORE"
	uploadDocument(t, handler, owner, "plan.md", "# Plan\n\nFirst.\n")
	plan := uploadDocument(t, handler, owner, "plan.md", "# Plan\n\nSecond.\n")
	if plan.Version.Number != 2 {
		t.Fatalf("second upload of the plan = version %d, want 2", plan.Version.Number)
	}
	want := documentSummary{ID: plan.Artifact.ID, Project: "CORE", Slug: plan.Artifact.Slug, Name: "plan.md", Kind: "doc"}

	for _, reference := range []string{plan.Artifact.Slug, "plan.md"} {
		path := owner + "/artifacts/" + reference
		full := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		if full.Code != http.StatusOK || decodeBody[model.Artifact](t, full).ID != plan.Artifact.ID {
			t.Fatalf("full read of %q: status=%d body=%s, want the plan", reference, full.Code, full.Body.String())
		}
		for caller, response := range summaryRead(t, handler, path+"/summary") {
			if response.Code != http.StatusOK {
				t.Fatalf("%s summary of %q: status=%d body=%s", caller, reference, response.Code, response.Body.String())
			}
			summary := decodeBody[documentSummary](t, response)
			latest := summary.LatestVersion
			summary.LatestVersion = nil
			if summary != want || latest == nil || *latest != 2 {
				t.Fatalf("%s summary of %q = %#v (latest %v), want %#v at version 2", caller, reference, summary, latest, want)
			}
		}
	}

	for caller, response := range summaryRead(t, handler, owner+"/artifacts/missing/summary") {
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"ARTIFACT_NOT_FOUND"`) {
			t.Fatalf("%s summary of a missing document: status=%d body=%s, want the document read's 404", caller, response.Code, response.Body.String())
		}
		full := dispatchRequest(t, handler, http.MethodGet, owner+"/artifacts/missing", nil, "alice")
		if full.Code != response.Code || full.Body.String() != response.Body.String() {
			t.Fatalf("%s summary of a missing document answers %d %s, the full read %d %s; want the same refusal", caller, response.Code, response.Body.String(), full.Code, full.Body.String())
		}
	}
}

// mp4Bytes is an MP4's `ftyp` box and then body: bytes no picture sniffer reads as an image, so
// an upload of them is a file, stored under the video type its client declares.
func mp4Bytes(body string) []byte {
	return append([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00"), body...)
}

// jpegBytes is a JPEG's signature and then body, what http.DetectContentType reads as image/jpeg.
func jpegBytes(body string) []byte {
	return append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF"), body...)
}

// Both summaries carry the content type the artifact's latest version is stored under, which is
// how a reader tells a video from a picture before it draws either: a video upload is a file
// under the video type it declares, a picture is under the type its bytes are - the latest
// version's, which a second version of another type changes - and a document stores no type, so
// its summary's is null.
func TestEitherSummaryCarriesTheLatestVersionsContentType(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Filmed", "A spec")
	createReferenceAPIProject(t, handler, "CORE")
	video, jpeg := "video/mp4", "image/jpeg"
	type artifactType struct {
		kind string
		mime *string
	}
	// The three artifacts an owner holds, each with the kind and type its summary must carry.
	// The picture's second version is a JPEG, so what the summary reports is the latest
	// version's type and not the first's.
	uploads := func(owner string) map[string]artifactType {
		clip := uploadFile(t, handler, owner, "clip.mp4", "video/mp4", mp4Bytes("frames"))
		uploadFile(t, handler, owner, "shot.png", "image/png", pngBytes("pixels"))
		shot := uploadFile(t, handler, owner, "shot.png", "image/jpeg", jpegBytes("pixels"))
		notes := uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nOne.\n")
		if clip.Artifact.Kind != "file" || shot.Artifact.Kind != "image" || shot.Version.Number != 2 {
			t.Fatalf("%s holds the video as %#v and the picture as %#v version %d; want a file and an image at version 2",
				owner, clip.Artifact, shot.Artifact, shot.Version.Number)
		}
		return map[string]artifactType{
			clip.Artifact.Slug:  {kind: "file", mime: &video},
			shot.Artifact.Slug:  {kind: "image", mime: &jpeg},
			notes.Artifact.Slug: {kind: "doc", mime: nil},
		}
	}
	sameType := func(got *string, want artifactType) bool {
		if want.mime == nil {
			return got == nil
		}
		return got != nil && *got == *want.mime
	}

	issueOwner := "/api/v1/issues/" + issue.Key
	wanted := uploads(issueOwner)
	for caller, response := range summaryRead(t, handler, issueOwner+"/summary") {
		if response.Code != http.StatusOK {
			t.Fatalf("%s issue summary: status=%d body=%s", caller, response.Code, response.Body.String())
		}
		seen := map[string]bool{}
		for _, document := range decodeBody[issueSummary](t, response).Documents {
			// The issue's spec is a document of its own: it stores no type either.
			want, uploaded := wanted[document.Slug]
			if !uploaded {
				want = artifactType{kind: "doc"}
			}
			if document.Kind != want.kind || !sameType(document.MIME, want) {
				t.Fatalf("%s issue summary of %s = kind %q type %v, want kind %q type %v",
					caller, document.Slug, document.Kind, document.MIME, want.kind, want.mime)
			}
			seen[document.Slug] = true
		}
		for slug := range wanted {
			if !seen[slug] {
				t.Fatalf("%s issue summary names no %s", caller, slug)
			}
		}
	}

	projectOwner := "/api/v1/projects/CORE"
	for slug, want := range uploads(projectOwner) {
		for caller, response := range summaryRead(t, handler, projectOwner+"/artifacts/"+slug+"/summary") {
			if response.Code != http.StatusOK {
				t.Fatalf("%s summary of %s: status=%d body=%s", caller, slug, response.Code, response.Body.String())
			}
			summary := decodeBody[documentSummary](t, response)
			if summary.Kind != want.kind || !sameType(summary.MIME, want) {
				t.Fatalf("%s document summary of %s = kind %q type %v, want kind %q type %v",
					caller, slug, summary.Kind, summary.MIME, want.kind, want.mime)
			}
		}
	}
}
