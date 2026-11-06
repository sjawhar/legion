package api

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// encodedPNG, encodedJPEG and encodedGIF are real pictures of the given size: a header a decoder
// reads, unlike pngBytes and jpegBytes, which are a signature and a body and are what an upload
// whose header does not decode looks like.
func encodedPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatalf("encode a %dx%d PNG: %v", width, height, err)
	}
	return encoded.Bytes()
}

func encodedJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, width, height)), nil); err != nil {
		t.Fatalf("encode a %dx%d JPEG: %v", width, height, err)
	}
	return encoded.Bytes()
}

func encodedGIF(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	frame := image.NewPaletted(image.Rect(0, 0, width, height), color.Palette{color.Black, color.White})
	if err := gif.Encode(&encoded, frame, nil); err != nil {
		t.Fatalf("encode a %dx%d GIF: %v", width, height, err)
	}
	return encoded.Bytes()
}

// encodedWebP is a lossless WebP of the given size: a RIFF WEBP file holding one VP8L chunk whose
// five-byte header is the 0x2f signature, then 14 bits of width-1, 14 bits of height-1, one alpha
// bit and a three-bit version, least significant bit first. That header is all
// golang.org/x/image/webp's DecodeConfig reads, and the standard library decodes no WebP at all,
// which is why the upload path reads WebP through that package.
func encodedWebP(width, height int) []byte {
	header := []byte{0x2f, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(header[1:], uint32(width-1)|uint32(height-1)<<14)
	file := []byte("RIFF")
	// Everything after the size word: "WEBP", the chunk's eight-byte header, its body and the
	// pad byte RIFF gives a chunk of odd length.
	file = binary.LittleEndian.AppendUint32(file, uint32(4+8+len(header)+1))
	file = append(file, "WEBPVP8L"...)
	file = binary.LittleEndian.AppendUint32(file, uint32(len(header)))
	file = append(file, header...)
	return append(file, 0)
}

// shape is a version's recorded size as a test reads it, "none" for a version with neither
// number: the pair is recorded together or not at all, so a version holding one of the two is a
// failure this spelling shows.
func shape(version model.Version) string {
	switch {
	case version.Width == nil && version.Height == nil:
		return "none"
	case version.Width == nil || version.Height == nil:
		return "half recorded"
	}
	return strconv.Itoa(*version.Width) + "x" + strconv.Itoa(*version.Height)
}

// An upload of each of the four pictures a model is shown records that picture's own width and
// height, read from its header where the upload already holds the bytes, and stores it under the
// type its bytes are. The pair the upload answers is the pair the version read answers.
func TestUploadRecordsEachPicturesOwnSize(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Shaped", "A spec")
	owner := "/api/v1/issues/" + issue.Key
	for _, picture := range []struct {
		name    string
		mime    string
		content []byte
		shape   string
	}{
		{"shot.png", "image/png", encodedPNG(t, 120, 48), "120x48"},
		{"shot.jpg", "image/jpeg", encodedJPEG(t, 64, 96), "64x96"},
		{"shot.gif", "image/gif", encodedGIF(t, 32, 16), "32x16"},
		// Declared as something else on purpose: the stored type is read from the bytes, and so
		// is the size.
		{"shot.webp", "application/octet-stream", encodedWebP(200, 150), "200x150"},
	} {
		t.Run(picture.name, func(t *testing.T) {
			upload := uploadFile(t, handler, owner, picture.name, picture.mime, picture.content)
			if upload.Artifact.Kind != "image" {
				t.Fatalf("%s is stored as %q, want an image", picture.name, upload.Artifact.Kind)
			}
			if got := shape(upload.Version); got != picture.shape {
				t.Fatalf("the upload of %s answers %s, want %s", picture.name, got, picture.shape)
			}
			read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+upload.Artifact.ID, nil, "alice")
			if read.Code != http.StatusOK {
				t.Fatalf("read %s: status=%d body=%s", picture.name, read.Code, read.Body.String())
			}
			versions := decodeBody[model.Artifact](t, read).Versions
			if len(versions) != 1 {
				t.Fatalf("%s has %d versions, want one", picture.name, len(versions))
			}
			if got := shape(versions[0]); got != picture.shape {
				t.Fatalf("the version read of %s answers %s, want %s", picture.name, got, picture.shape)
			}
		})
	}
}

// Bytes that are not a readable picture, and a file that is not a picture at all, record neither
// number, and the upload is stored and served exactly as it was before Dispatch read a size: the
// same kind, the same stored type and the same bytes back.
func TestUploadOfUnreadableBytesRecordsNoSizeAndIsServedAsBefore(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Unreadable", "A spec")
	owner := "/api/v1/issues/" + issue.Key
	for _, upload := range []struct {
		name    string
		mime    string
		content []byte
		kind    string
		stored  string
	}{
		// A PNG signature with no header behind it: sniffed as a picture, stored as an image,
		// decoded by nothing.
		{"broken.png", "image/png", pngBytes("pixels"), "image", "image/png"},
		{"broken.jpg", "image/jpeg", jpegBytes("pixels"), "image", "image/jpeg"},
		// Not a picture at all: a video and a text file, each a file under its declared type.
		{"clip.mp4", "video/mp4", mp4Bytes("frames"), "file", "video/mp4"},
		{"rows.csv", "text/csv", []byte("one,two\n1,2\n"), "file", "text/csv"},
	} {
		t.Run(upload.name, func(t *testing.T) {
			stored := uploadFile(t, handler, owner, upload.name, upload.mime, upload.content)
			if stored.Artifact.Kind != upload.kind {
				t.Fatalf("%s is stored as %q, want %q", upload.name, stored.Artifact.Kind, upload.kind)
			}
			if stored.Version.MIME == nil || *stored.Version.MIME != upload.stored {
				t.Fatalf("%s is stored under %v, want %q", upload.name, stored.Version.MIME, upload.stored)
			}
			if got := shape(stored.Version); got != "none" {
				t.Fatalf("%s records %s, want no size at all", upload.name, got)
			}
			served := dispatchRequest(t, handler, http.MethodGet,
				"/api/v1/artifacts/"+stored.Artifact.ID+"/versions/1", nil, "alice")
			if served.Code != http.StatusOK || !bytes.Equal(served.Body.Bytes(), upload.content) ||
				served.Header().Get("Content-Type") != upload.stored {
				t.Fatalf("serve %s: status=%d type=%q bytes=%d, want 200 %q and the %d bytes uploaded",
					upload.name, served.Code, served.Header().Get("Content-Type"),
					served.Body.Len(), upload.stored, len(upload.content))
			}
		})
	}
}

// A version answer names the size of the version it answers for, which is the only size the
// artifact page and the Artifacts tab can draw from: they draw the version the reader chose. A
// second version of another shape leaves the first version's recorded pair alone.
func TestArtifactVersionAnswersCarryTheirOwnSize(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Reshaped", "A spec")
	owner := "/api/v1/issues/" + issue.Key
	first := uploadFile(t, handler, owner, "shot.png", "image/png", encodedPNG(t, 120, 48))
	second := uploadFile(t, handler, owner, "shot.png", "image/png", encodedPNG(t, 30, 200))
	if second.Version.Number != 2 {
		t.Fatalf("the second upload is version %d, want 2", second.Version.Number)
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+first.Artifact.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the artifact: status=%d body=%s", read.Code, read.Body.String())
	}
	versions := decodeBody[model.Artifact](t, read).Versions
	if len(versions) != 2 {
		t.Fatalf("the artifact has %d versions, want two", len(versions))
	}
	if got := shape(versions[0]) + " " + shape(versions[1]); got != "120x48 30x200" {
		t.Fatalf("the version answers are %s, want %s", got, "120x48 30x200")
	}
	// The list read of every artifact of the issue reads the same columns in the same order.
	listed := dispatchRequest(t, handler, http.MethodGet, owner+"/artifacts", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the issue's artifacts: status=%d body=%s", listed.Code, listed.Body.String())
	}
	for _, artifact := range decodeBody[[]model.Artifact](t, listed) {
		if artifact.ID != first.Artifact.ID {
			continue
		}
		if len(artifact.Versions) != 2 || shape(artifact.Versions[0]) != "120x48" || shape(artifact.Versions[1]) != "30x200" {
			t.Fatalf("the listed artifact's versions are %#v, want 120x48 then 30x200", artifact.Versions)
		}
	}
	// A document's version answers no size: there is no picture to measure.
	notes := uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nOne.\n")
	if got := shape(notes.Version); got != "none" {
		t.Fatalf("the document version records %s, want no size at all", got)
	}
}

// Both summaries carry the latest version's own size beside its content type, so one read tells a
// reference what to draw and what box to reserve for it. A second version of another shape moves
// the summaries to that version's size, since the summary answers for the latest alone; a video
// and a document carry none.
func TestEitherSummaryCarriesTheLatestVersionsSize(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Summarised", "A spec")
	createReferenceAPIProject(t, handler, "CORE")

	// The shape each owner's summaries must carry, by slug. The picture's second version is
	// another shape, so what the summary reports is the latest's and not the first's.
	uploads := func(owner string) map[string]string {
		clip := uploadFile(t, handler, owner, "clip.mp4", "video/mp4", mp4Bytes("frames"))
		uploadFile(t, handler, owner, "shot.png", "image/png", encodedPNG(t, 120, 48))
		shot := uploadFile(t, handler, owner, "shot.png", "image/png", encodedPNG(t, 30, 200))
		notes := uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nOne.\n")
		if shot.Version.Number != 2 {
			t.Fatalf("%s holds the picture at version %d, want 2", owner, shot.Version.Number)
		}
		return map[string]string{
			clip.Artifact.Slug:  "none",
			shot.Artifact.Slug:  "30x200",
			notes.Artifact.Slug: "none",
		}
	}
	summaryShape := func(width, height *int) string {
		return shape(model.Version{Height: height, Width: width})
	}

	issueOwner := "/api/v1/issues/" + issue.Key
	wanted := uploads(issueOwner)
	for caller, response := range summaryRead(t, handler, issueOwner+"/summary") {
		if response.Code != http.StatusOK {
			t.Fatalf("%s issue summary: status=%d body=%s", caller, response.Code, response.Body.String())
		}
		seen := map[string]bool{}
		for _, document := range decodeBody[issueSummary](t, response).Documents {
			// The issue's spec is a document of its own: it carries no size either.
			want, uploaded := wanted[document.Slug]
			if !uploaded {
				want = "none"
			}
			if got := summaryShape(document.Width, document.Height); got != want {
				t.Fatalf("%s issue summary of %s carries %s, want %s", caller, document.Slug, got, want)
			}
			// The type is still there beside it: one read answers both questions.
			if document.Slug == "shot-png" && (document.MIME == nil || *document.MIME != "image/png") {
				t.Fatalf("%s issue summary of %s carries type %v, want image/png", caller, document.Slug, document.MIME)
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
			if got := summaryShape(summary.Width, summary.Height); got != want {
				t.Fatalf("%s document summary of %s carries %s, want %s", caller, slug, got, want)
			}
		}
	}
}
