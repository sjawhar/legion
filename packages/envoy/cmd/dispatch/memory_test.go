package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const (
	// memoryTestServeEnv makes this test binary run main, so the memory tests drive a real Dispatch
	// process and read its own resident memory.
	memoryTestServeEnv = "DISPATCH_MEMORY_TEST_SERVE"
	memoryTestToken    = "memory-test-token"
	// requestMemoryBound is the most one request may raise a fresh server's resident memory above
	// what it held idle, and concurrentMemoryBound the most two at once may (LEGION-481). Production
	// runs one task of 1,024 MiB.
	requestMemoryBound    = 256 << 20
	concurrentMemoryBound = 512 << 20
	documentCap           = 1 << 20
)

// A markdown upload of any shape at the 1 MiB cap holds at most 256 MiB above the server's idle
// memory, through the real route on a real Dispatch process, multipart and JSON alike: stored, or
// refused with a 4xx that names why. Before LEGION-481 one 1 MiB `)_` upload took a gigabyte and
// the production task with it. Each shape runs on a fresh server, so what one leaves resident
// does not hide what the next costs, and the peak counts the settlement that follows a stored one.
func TestEveryUploadAtTheCapStaysWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	for _, shape := range capShapes() {
		modes := []string{"json"}
		if shape.name == ")_" {
			modes = append(modes, "multipart")
		}
		t.Run(shape.name, func(t *testing.T) {
			markdown := shape.markdown()
			if len(markdown) != documentCap {
				t.Fatalf("shape is %d bytes, want the %d-byte cap", len(markdown), documentCap)
			}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					server := memory.start(t)
					var upload uploadResult
					peak := server.peakAboveIdle(t, func() {
						upload = server.upload(t, mode, memory.issue, markdown)
						memory.waitForSettlement(t, upload)
					})
					requireAnswered(t, upload)
					t.Logf("%s %s: %d, %d MiB above idle in %s", shape.name, mode, upload.status, peak>>20, upload.elapsed)
					if peak > requestMemoryBound {
						t.Errorf("upload of %s (%s) held %d MiB above idle, want at most %d MiB", shape.name, mode, peak>>20, requestMemoryBound>>20)
					}
					if within, bounded := capShapeAnswersWithin[shape.name]; bounded && upload.elapsed > within {
						t.Errorf("upload of %s (%s) answered in %s, want at most %s", shape.name, mode, upload.elapsed, within)
					}
				})
			}
		})
	}
}

// The heaviest documents the element limit admits are stored, and storing one - upload and
// settlement - and then reading it - its text, its blocks, and a websocket load of its room - each
// hold at most the bound, a read on a server that has not loaded the document.
func TestTheHeaviestStoredDocumentsStayWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	for _, shape := range heaviestAdmittedShapes(t) {
		t.Run(shape.name, func(t *testing.T) {
			writer := memory.start(t)
			var upload uploadResult
			peak := writer.peakAboveIdle(t, func() {
				upload = writer.upload(t, "json", memory.issue, shape.markdown)
				memory.waitForSettlement(t, upload)
			})
			if upload.status != http.StatusCreated {
				t.Fatalf("upload of the heaviest %s the limit admits: status %d body %.300s, want 201", shape.name, upload.status, upload.body)
			}
			t.Logf("%s: stored, %d MiB above idle in %s", shape.name, peak>>20, upload.elapsed)
			if peak > requestMemoryBound {
				t.Errorf("storing %s held %d MiB above idle, want at most %d MiB", shape.name, peak>>20, requestMemoryBound>>20)
			}
			reader := memory.start(t)
			for _, read := range []struct {
				name string
				run  func(t *testing.T)
			}{
				{"text", func(t *testing.T) { reader.get(t, "/api/v1/artifacts/"+upload.artifactID+"/text") }},
				{"blocks", func(t *testing.T) { reader.get(t, "/api/v1/artifacts/"+upload.artifactID+"/blocks") }},
				{"websocket load", func(t *testing.T) { reader.loadOverWebsocket(t, upload.artifactID) }},
			} {
				peak := reader.peakAboveIdle(t, func() { read.run(t) })
				t.Logf("%s: %s read, %d MiB above idle", shape.name, read.name, peak>>20)
				if peak > requestMemoryBound {
					t.Errorf("%s read of %s held %d MiB above idle, want at most %d MiB", read.name, shape.name, peak>>20, requestMemoryBound>>20)
				}
			}
		})
	}
}

// Two uploads at once hold at most twice the bound together: two of a cap-sized `)_`, which the
// element limit refuses, and two of the heaviest `)_` document it admits, which are stored.
func TestTwoUploadsAtOnceStayWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	heaviest := heaviestAdmitted(t, func(units int) string { return strings.Repeat(")_", units) })
	for _, pair := range []struct{ name, markdown string }{
		{name: "cap-sized )_", markdown: fill(")_")},
		{name: "heaviest admitted )_", markdown: heaviest},
	} {
		t.Run(pair.name, func(t *testing.T) {
			server := memory.start(t)
			var uploads [2]uploadResult
			var failures [2]error
			peak := server.peakAboveIdle(t, func() {
				var group sync.WaitGroup
				for index := range uploads {
					group.Go(func() { uploads[index], failures[index] = server.tryUpload("json", memory.issue, pair.markdown) })
				}
				group.Wait()
				if err := errors.Join(failures[:]...); err != nil {
					t.Fatal(err)
				}
				for _, upload := range uploads {
					memory.waitForSettlement(t, upload)
				}
			})
			for _, upload := range uploads {
				requireAnswered(t, upload)
			}
			t.Logf("two uploads of %s: %d and %d, %d MiB above idle", pair.name, uploads[0].status, uploads[1].status, peak>>20)
			if peak > concurrentMemoryBound {
				t.Errorf("two uploads of %s at once held %d MiB above idle, want at most %d MiB", pair.name, peak>>20, concurrentMemoryBound>>20)
			}
		})
	}
}

// Two cold reads at once of the heaviest documents the limit admits hold at most twice the bound
// together, on a server that has not loaded the document: `)_`, and the hard breaks that, weighed as
// one inline node, held 565 MiB for two reads and 1,190 MiB for four. Four reads at once are
// logged beside them, the production task being 1,024 MiB.
func TestConcurrentColdReadsStayWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	for _, shape := range heaviestAdmittedShapes(t) {
		if shape.name != ")_" && !strings.HasPrefix(shape.name, "hard breaks") {
			continue
		}
		t.Run(shape.name, func(t *testing.T) {
			writer := memory.start(t)
			upload := writer.upload(t, "json", memory.issue, shape.markdown)
			if upload.status != http.StatusCreated {
				t.Fatalf("upload of the heaviest %s the limit admits: status %d body %.300s, want 201", shape.name, upload.status, upload.body)
			}
			memory.waitForSettlement(t, upload)
			writer.stop(t)
			for _, readers := range []int{2, 4} {
				server := memory.start(t)
				peak := server.peakAboveIdle(t, func() {
					failures := make([]error, readers)
					var group sync.WaitGroup
					for index := range readers {
						group.Go(func() {
							answer, err := server.trySend(http.MethodGet, "/api/v1/artifacts/"+upload.artifactID+"/text", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
							if err == nil && answer.status != http.StatusOK {
								err = fmt.Errorf("read %d: status %d body %.300s", index, answer.status, answer.body)
							}
							failures[index] = err
						})
					}
					group.Wait()
					if err := errors.Join(failures...); err != nil {
						t.Fatal(err)
					}
				})
				server.stop(t)
				t.Logf("%s: %d cold text reads at once, %d MiB above idle", shape.name, readers, peak>>20)
				if readers == 2 && peak > concurrentMemoryBound {
					t.Errorf("two cold reads of %s at once held %d MiB above idle, want at most %d MiB", shape.name, peak>>20, concurrentMemoryBound>>20)
				}
			}
		})
	}
}

// A caller's quote is read as markdown when its text alone matches nothing in the document: the
// `find` of an edit's replace, and the quote a comment or an ask anchors on. Each is caller
// markdown, as a write's is, so a quote filling the 1 MiB request cap holds at most the bound above
// a fresh server's idle memory, and is answered. Read with no element limit, a `find` of `- a`
// lines held over 500 MiB.
func TestEveryQuoteAtTheCapStaysWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	setup := memory.start(t)
	created := setup.send(t, http.MethodPost, "/api/v1/issues", "application/json",
		strings.NewReader(`{"project":"MEM","title":"Quotes","spec":"Before.\n"}`), http.Header{"X-Dispatch-User": {"alice"}})
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if created.status != http.StatusCreated || json.Unmarshal(created.body, &issue) != nil || issue.PrimaryArtifactID == "" {
		t.Fatalf("create the quoted issue: status %d body %.300s", created.status, created.body)
	}
	setup.stop(t)
	routes := []struct {
		name, path string
		body       func(quote string) map[string]any
	}{
		{"an edit's find", "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/edits", func(quote string) map[string]any {
			return map[string]any{"ops": []map[string]any{{"op": "replace", "find": quote, "with": "x"}}}
		}},
		{"a comment's quote", "/api/v1/issues/" + issue.Key + "/comments", func(quote string) map[string]any {
			return map[string]any{"body": "why", "anchor": map[string]any{"artifact": "spec", "quote": quote}}
		}},
		{"an ask's quote", "/api/v1/issues/" + issue.Key + "/asks", func(quote string) map[string]any {
			return map[string]any{"question": "why?", "anchor": map[string]any{"artifact": "spec", "quote": quote}}
		}},
	}
	for _, unit := range []string{")_", "- a\n"} {
		for _, route := range routes {
			t.Run(fmt.Sprintf("%s of %q", route.name, unit), func(t *testing.T) {
				body := bodyAtTheCap(t, unit, route.body)
				server := memory.start(t)
				var answer response
				peak := server.peakAboveIdle(t, func() {
					answer = server.send(t, http.MethodPost, route.path, "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
				})
				var refusal struct{ Code, Error string }
				if answer.status >= 500 || answer.status >= 400 && (json.Unmarshal(answer.body, &refusal) != nil || refusal.Code == "") {
					t.Fatalf("%s answered %d %.300s, want a success or a 4xx naming its code", route.name, answer.status, answer.body)
				}
				t.Logf("%s of %q: %d %s, %d MiB above idle", route.name, unit, answer.status, refusal.Code, peak>>20)
				if peak > requestMemoryBound {
					t.Errorf("%s of %q held %d MiB above idle, want at most %d MiB", route.name, unit, peak>>20, requestMemoryBound>>20)
				}
			})
		}
	}
}

// A run of writes, each within what one upload may hold, cannot grow a document past it on a real
// Dispatch process. For each way a document grows - prose and a code block's text by their bytes,
// `)_`, headings, hard breaks, list items and table rows by their elements - inserts are taken until
// one would leave the document's markdown past an upload's limits, which is refused with 413
// CAP_EXCEEDED, and so is every insert after it. The document still reads and its own text uploads
// back whole. At the bound - prose filling a document grown by bytes to the 1 MiB cap - a refused
// insert, its rendering, measure and trial load included, and a one-word edit each hold at most the
// bound above the idle memory of a server that has not loaded the document; the inserts before
// them, on one server, are logged with what earlier ones left resident. Prose and code make the
// thirty-two 900 KB inserts LEGION-481's review grew one document to 29.5 MB with, after which a
// one-word edit held about a gigabyte.
func TestAWriteCannotGrowADocumentPastWhatOneUploadMayHold(t *testing.T) {
	memory := newMemoryHarness(t)
	const spec = "Before sentinel.\n"
	prose := func(bytes int) string { return strings.Repeat("word ", bytes/5) }
	for _, shape := range []struct {
		name, spec, after, chunk string
		inserts                  int
		byBytes                  bool
	}{
		{"900 KB of prose", spec, "end", prose(900_000), 32, true},
		{"900 KB of a code block's text", spec, "end", "```\n" + strings.Repeat("a line of code, forty bytes long; more\n", 22_500) + "```\n", 32, true},
		{"42 KB of )_", spec, "end", strings.Repeat(")_", 21_000), 5, false},
		{"a thousand headings", spec, "end", strings.Repeat("# a\n", 1_000), 18, false},
		{"two thousand hard breaks", spec, "end", strings.Repeat("a  \n", 2_000) + "a\n", 11, false},
		{"three thousand list items", spec, "end", strings.Repeat("- a\n", 3_000), 6, false},
		{"a thousand table rows", spec + "\n| a | b |\n| - | - |\n| A10 | b |\n", "A10", strings.Repeat("| x | y |\n", 1_000), 8, false},
	} {
		t.Run(shape.name, func(t *testing.T) {
			insert := map[string]any{"op": "insert", "after": shape.after, "markdown": shape.chunk}
			server := memory.start(t)
			issue := server.createIssue(t, "Growth by "+shape.name, shape.spec)
			refused := false
			for index := range shape.inserts {
				var answer response
				peak := server.peakAboveIdle(t, func() { answer = server.edit(t, issue.PrimaryArtifactID, insert) })
				t.Logf("insert %d: %d, %d MiB above idle with what earlier inserts left: %.200s", index+1, answer.status, peak>>20, answer.body)
				switch {
				case answer.status == http.StatusOK && !refused:
				case refusedWithAdvice(answer):
					refused = true
				default:
					t.Errorf("insert %d answered %d %.300s, want 200 until one is refused with 413 CAP_EXCEEDED saying to shorten the change, and 413 after it", index+1, answer.status, answer.body)
				}
			}
			if !refused {
				t.Errorf("all %d inserts were taken, want the document's growth refused", shape.inserts)
			}
			server.get(t, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks")
			text := server.text(t, issue.PrimaryArtifactID)
			if shape.byBytes {
				if fill := documentCap - len(text) - 64; fill > 0 {
					if answer := server.edit(t, issue.PrimaryArtifactID, map[string]any{"op": "insert", "after": "end", "markdown": prose(fill)}); answer.status != http.StatusOK {
						t.Errorf("fill the document to the cap: %d %.300s, want 200", answer.status, answer.body)
					}
				}
				text = server.text(t, issue.PrimaryArtifactID)
			}
			server.stop(t)
			for _, write := range []struct {
				name string
				op   map[string]any
				want func(response) bool
			}{
				{"a refused insert", insert, refusedWithAdvice},
				{"a one-word edit", map[string]any{"op": "replace", "find": "sentinel", "with": "marker"}, func(answer response) bool { return answer.status == http.StatusOK }},
			} {
				cold := memory.start(t)
				var answer response
				peak := cold.peakAboveIdle(t, func() { answer = cold.edit(t, issue.PrimaryArtifactID, write.op) })
				cold.stop(t)
				t.Logf("%s: %s of the %d-byte document answered %d, %d MiB above idle: %.200s", shape.name, write.name, len(text), answer.status, peak>>20, answer.body)
				if !write.want(answer) {
					t.Errorf("%s answered %d %.300s", write.name, answer.status, answer.body)
				}
				if peak > requestMemoryBound {
					t.Errorf("%s of the %d-byte document held %d MiB above idle, want at most %d MiB", write.name, len(text), peak>>20, requestMemoryBound>>20)
				}
			}
			reader := memory.start(t)
			text = reader.text(t, issue.PrimaryArtifactID)
			upload, err := reader.tryUploadNamed("multipart", issue.Key, "spec.md", text)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: an upload of its own %d-byte text answered %d %.200s", shape.name, len(text), upload.status, upload.body)
			if upload.status != http.StatusCreated {
				t.Errorf("an upload of the document's own %d-byte text answered %d %.300s, want 201", len(text), upload.status, upload.body)
			}
		})
	}
}

// refusedWithAdvice reports whether answer is a write's growth refused: 413 CAP_EXCEEDED, saying
// what to do.
func refusedWithAdvice(answer response) bool {
	return answer.status == http.StatusRequestEntityTooLarge && strings.Contains(string(answer.body), `"code":"CAP_EXCEEDED"`) &&
		strings.Contains(string(answer.body), "shorten the change, or split the document")
}

// An ask's text and its answer are written into its block, so they grow a document as an edit
// does, and are held to what one upload may hold on a real Dispatch process as an edit is: edits of
// the asks of one document, each giving an option a 900 KB description, and answers of 900 KB to
// them, are taken until one would leave the document's markdown past 1 MiB, which is refused with
// 413 CAP_EXCEEDED, and so is every one after it. The document still reads, and a one-word edit on
// a server that has not loaded it holds at most the bound. Before the bound reached these routes,
// thirty-two such edits grew one document to 28.8 MB, on which a one-word edit held 1,204 MiB, and
// under the production task's 1,024 MiB the server was killed.
func TestAskEditsAndAnswersCannotGrowADocumentPastWhatOneUploadMayHold(t *testing.T) {
	memory := newMemoryHarness(t)
	const asks = 32
	var spec strings.Builder
	spec.WriteString("Before sentinel.\n")
	for index := range asks {
		fmt.Fprintf(&spec, "\n:::ask{#ask-%d urgency=\"med\" multiple=\"false\" state=\"open\"}\nQuestion %d?\n:::\n", index, index)
	}
	prose := strings.Repeat("word ", 180_000)
	for _, route := range []struct {
		name, method, path string
		body               map[string]any
	}{
		{"PATCH /api/v1/asks/{id}", http.MethodPatch, "", map[string]any{"options": []map[string]string{{"label": "A", "description": prose}}}},
		{"POST /api/v1/asks/{id}/answer", http.MethodPost, "/answer", map[string]any{"text": prose, "expected_edited_at": nil}},
	} {
		t.Run(route.name, func(t *testing.T) {
			body, err := json.Marshal(route.body)
			if err != nil {
				t.Fatalf("encode the write: %v", err)
			}
			server := memory.start(t)
			issue := server.createIssue(t, "Ask growth by "+route.name, spec.String())
			refused := false
			for index, askID := range server.blockAsks(t, issue.Key, asks) {
				var answer response
				peak := server.peakAboveIdle(t, func() {
					answer = server.send(t, route.method, "/api/v1/asks/"+askID+route.path, "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
				})
				t.Logf("write %d: %d, %d MiB above idle with what earlier writes left: %.200s", index+1, answer.status, peak>>20, answer.body)
				switch {
				case answer.status == http.StatusOK && !refused:
				case refusedWithAdvice(answer):
					refused = true
				default:
					t.Errorf("write %d answered %d %.300s, want 200 until one is refused with 413 CAP_EXCEEDED saying to shorten the change, and 413 after it", index+1, answer.status, answer.body)
				}
			}
			if !refused {
				t.Errorf("all %d writes were taken, want the document's growth refused", asks)
			}
			server.get(t, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks")
			text := server.text(t, issue.PrimaryArtifactID)
			server.stop(t)
			if len(text) > documentCap {
				t.Errorf("the document's markdown is %d bytes, past the %d-byte cap", len(text), documentCap)
			}
			cold := memory.start(t)
			var answer response
			peak := cold.peakAboveIdle(t, func() {
				answer = cold.edit(t, issue.PrimaryArtifactID, map[string]any{"op": "replace", "find": "sentinel", "with": "marker"})
			})
			cold.stop(t)
			t.Logf("%s: a one-word edit of the %d-byte document answered %d, %d MiB above idle: %.200s", route.name, len(text), answer.status, peak>>20, answer.body)
			if answer.status != http.StatusOK {
				t.Errorf("a one-word edit answered %d %.300s, want 200", answer.status, answer.body)
			}
			if peak > requestMemoryBound {
				t.Errorf("a one-word edit of the %d-byte document held %d MiB above idle, want at most %d MiB", len(text), peak>>20, requestMemoryBound>>20)
			}
		})
	}
}

// blockAsks are the ids of the asks settlement indexes from the blocks ask-0 to ask-<count-1> of
// the issue's spec, in that order, once it has indexed every one.
func (p *dispatchProcess) blockAsks(t *testing.T, issueKey string, count int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		answer := p.send(t, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks?state=all", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
		var asks []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
		}
		if answer.status != http.StatusOK || json.Unmarshal(answer.body, &asks) != nil {
			t.Fatalf("list the issue's asks: status %d body %.300s", answer.status, answer.body)
		}
		byBlock := map[string]string{}
		for _, ask := range asks {
			if ask.BlockID != nil {
				byBlock[*ask.BlockID] = ask.ID
			}
		}
		ids := make([]string, 0, count)
		for index := range count {
			if id, found := byBlock[fmt.Sprintf("ask-%d", index)]; found {
				ids = append(ids, id)
			}
		}
		if len(ids) == count {
			return ids
		}
		if time.Now().After(deadline) {
			t.Fatalf("settlement indexed %d of the spec's %d asks", len(ids), count)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// An upload's parse does not count front matter, so a new spec of front matter and the 16,384
// headings one document may hold is stored, as the headings alone are.
func TestASpecOfFrontMatterAndTheMostHeadingsIsStored(t *testing.T) {
	memory := newMemoryHarness(t)
	server := memory.start(t)
	issue := server.createIssue(t, "Front matter", "---\ntitle: a spec\n---\n\n"+strings.Repeat("# a\n", 16_384))
	if text := server.text(t, issue.PrimaryArtifactID); !strings.HasPrefix(text, "---\ntitle: a spec\n---\n") || strings.Count(text, "# a\n") != 16_384 {
		t.Fatalf("the spec reads back %d bytes opening %q, want its front matter and 16,384 headings", len(text), text[:min(len(text), 40)])
	}
}

// createdIssue is an issue a memory test created, and its primary document.
type createdIssue struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
}

// createIssue creates an issue of the memory harness's project whose spec is spec, past the
// near-duplicate check its title would meet beside the others a test creates.
func (p *dispatchProcess) createIssue(t *testing.T, title, spec string) createdIssue {
	t.Helper()
	body, err := json.Marshal(map[string]any{"project": "MEM", "title": title, "spec": spec, "force": true})
	if err != nil {
		t.Fatalf("encode the issue: %v", err)
	}
	created := p.send(t, http.MethodPost, "/api/v1/issues", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
	var issue createdIssue
	if created.status != http.StatusCreated || json.Unmarshal(created.body, &issue) != nil || issue.PrimaryArtifactID == "" {
		t.Fatalf("create the issue %q: status %d body %.300s", title, created.status, created.body)
	}
	return issue
}

// edit sends one edit operation to a document as a person does.
func (p *dispatchProcess) edit(t *testing.T, artifactID string, op map[string]any) response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"ops": []map[string]any{op}})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	return p.send(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
}

// text is a document's markdown, as GET .../text answers it.
func (p *dispatchProcess) text(t *testing.T, artifactID string) string {
	t.Helper()
	answer := p.send(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
	var text struct{ Markdown string }
	if answer.status != http.StatusOK || json.Unmarshal(answer.body, &text) != nil {
		t.Fatalf("read the document's text: status %d body %.300s", answer.status, answer.body)
	}
	return text.Markdown
}

// bodyAtTheCap is the JSON request body that build makes of the longest quote of unit repeated that
// fits the 1 MiB request cap.
func bodyAtTheCap(t *testing.T, unit string, build func(quote string) map[string]any) []byte {
	t.Helper()
	encode := func(units int) []byte {
		body, err := json.Marshal(build(strings.Repeat(unit, units)))
		if err != nil {
			t.Fatalf("encode the request: %v", err)
		}
		return body
	}
	unitBytes := len(encode(1)) - len(encode(0))
	units := (documentCap - len(encode(0))) / unitBytes
	body := encode(units)
	if len(body) > documentCap || documentCap-len(body) >= unitBytes {
		t.Fatalf("the body of %d units is %d bytes, want the most that fit %d", units, len(body), documentCap)
	}
	return body
}

// memoryShape is a markdown document a memory test uploads.
type memoryShape struct {
	name     string
	markdown func() string
}

// capShapeAnswersWithin is how long the upload of a cap shape that once took far longer may take
// to answer, by the shape's name.
var capShapeAnswersWithin = map[string]time.Duration{"link reference definitions": 10 * time.Second}

// capShapes are the shapes LEGION-481 names, each exactly at the 1 MiB cap: the ones LEGION-465's
// pull requests (#1670, #1669) found quadratic or deep, a table of a mebibyte of cells, and the
// node-heavy tables and links #1669's review measured. 1 MiB of bare `>` nests a quote per byte:
// main's parse ran 14 minutes into a stack overflow that took the server down, and #1669's depth
// bound refuses it as soon as it nests past 100. Flat list items, empty or not, are where the
// guard has to close a list to stop, and hard breaks weigh as blocks. A paragraph of link
// reference definitions cost goldmark time quadratic in its lines: a mebibyte of them took a
// minute to refuse, so it answers within ten seconds.
func capShapes() []memoryShape {
	repeated := func(unit string) func() string { return func() string { return fill(unit) } }
	return []memoryShape{
		{")_", repeated(")_")},
		{"a_", repeated("a_")},
		{"a_b*", repeated("a_b*")},
		{"a~b_", repeated("a~b_")},
		{"a_b&", repeated("a_b&")},
		{"<a", repeated("<a")},
		{"[a", repeated("[a")},
		{"[^a then ]", func() string { return fill("[^a")[:documentCap-1] + "]" }},
		{"a line feed per character", repeated("a\n")},
		{"one run of *", func() string { return "a" + fill("*")[1:] }},
		{"one run of _", func() string { return "a" + fill("_")[1:] }},
		{"one run of ~", func() string { return "a" + fill("~")[1:] }},
		{"262,140 nested marks", func() string {
			run := strings.Repeat("*", 2*262_140)
			return run + strings.Repeat("x", documentCap-2*len(run)) + run
		}},
		{"100-deep quotes repeated", func() string {
			line := strings.Repeat("> ", 100) + "a\n"
			text := strings.Repeat(line, documentCap/len(line))
			return text + strings.Repeat("a", documentCap-len(text))
		}},
		{"a table of 1 MiB of cells", func() string {
			head := "| a | b |\n| - | - |\n"
			text := head + strings.Repeat("| x | y |\n", (documentCap-len(head))/10)
			return text + strings.Repeat("z", documentCap-len(text))
		}},
		{"an escaped-pipe table", func() string {
			head := "| a |\n| --- |\n"
			text := head + strings.Repeat("| `x\\|y` |\n", (documentCap-len(head))/12)
			return text + strings.Repeat("z", documentCap-len(text))
		}},
		{"an image-in-link chain", repeated("[![a](b)](c)")},
		{"lists nested a level a line", func() string {
			var text strings.Builder
			for depth := 0; text.Len()+2*depth+4 <= documentCap; depth++ {
				text.WriteString(strings.Repeat("  ", depth) + "- a\n")
			}
			return text.String() + strings.Repeat("a", documentCap-text.Len())
		}},
		{"list items", repeated("- a\n")},
		{"empty list items", repeated("-\n")},
		{"hard breaks of two spaces", repeated("a  \n")},
		{"hard breaks of a backslash", repeated("a\\\n")},
		{"1 MiB of bare >", repeated(">")},
		{"link reference definitions", repeated("[a]: b\n")},
	}
}

// fill is unit repeated to exactly the document cap.
func fill(unit string) string {
	return strings.Repeat(unit, documentCap/len(unit)+1)[:documentCap]
}

// admittedShape is a document the element limit admits.
type admittedShape struct {
	name     string
	markdown string
}

// heaviestAdmittedShapes are the heaviest documents of the shapes that cost the most memory per
// element - italic spans, delimiter runs, table cells, headings, list items, hard breaks and inline
// HTML, the heaviest of all to store - that the element limit admits, each found by the parser the
// server writes with.
func heaviestAdmittedShapes(t *testing.T) []admittedShape {
	t.Helper()
	repeated := func(unit string) func(int) string {
		return func(units int) string { return strings.Repeat(unit, units) }
	}
	table := func(header, delimiter, row string) func(int) string {
		return func(rows int) string { return header + delimiter + strings.Repeat(row, rows) }
	}
	var shapes []admittedShape
	for _, shape := range []struct {
		name  string
		build func(int) string
	}{
		{")_", repeated(")_")},
		{"a_b*", repeated("a_b*")},
		{"a two-column table", table("| a | b |\n", "| - | - |\n", "| x | y |\n")},
		{"a four-column table", table("| a | b | c | d |\n", "| - | - | - | - |\n", "| w | x | y | z |\n")},
		{"headings", repeated("# a\n")},
		{"list items", repeated("- a\n")},
		{"hard breaks of two spaces", repeated("a  \n")},
		{"hard breaks of a backslash", repeated("a\\\n")},
		{"HTML spans", repeated("<b>a</b>")},
	} {
		shapes = append(shapes, admittedShape{name: shape.name, markdown: heaviestAdmitted(t, shape.build)})
	}
	return shapes
}

// heaviestAdmitted is the document of the most units build makes that the element limit admits,
// padded with a paragraph of plain words, which weighs next to nothing, to the cap: the cap of what
// the server stores, its rendering, which can run longer than the markdown written - a blank line
// between two headings, a line feed at the end - so the padding gives way to that.
func heaviestAdmitted(t *testing.T, build func(units int) string) string {
	t.Helper()
	document := func(units int) string {
		text := build(units) + "\n\n"
		return text + strings.Repeat("word ", documentCap/5+1)[:documentCap-len(text)]
	}
	admitted := func(units int) bool {
		_, err := pmdoc.Parse(document(units))
		if err != nil && !errors.Is(err, pmdoc.ErrTooManyElements) {
			t.Fatalf("parse %d units: %v", units, err)
		}
		return err == nil
	}
	low, high := 1, 1
	for admitted(high) {
		low, high = high, high*2
	}
	for high-low > 1 {
		if middle := (low + high) / 2; admitted(middle) {
			low = middle
		} else {
			high = middle
		}
	}
	text := document(low)
	for {
		tree, err := pmdoc.Parse(text)
		if err != nil {
			t.Fatalf("parse the heaviest document: %v", err)
		}
		rendered, err := pmdoc.Render(tree)
		if err != nil {
			t.Fatalf("render the heaviest document: %v", err)
		}
		over := len(rendered) - documentCap
		if over <= 0 {
			return text
		}
		text = text[:len(text)-over]
	}
}

// memoryHarness is the database and the issue the memory tests' servers share.
type memoryHarness struct {
	t        *testing.T
	database *store.Store
	issue    string
}

func newMemoryHarness(t *testing.T) *memoryHarness {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the memory tests read a server's resident memory from /proc")
	}
	harness := &memoryHarness{t: t, database: storetest.Open(t)}
	server := harness.start(t)
	for _, request := range []struct{ path, body string }{
		{"/api/v1/projects", `{"key":"MEM","name":"Memory"}`},
		{"/api/v1/issues", `{"project":"MEM","title":"Memory bound"}`},
	} {
		response := server.send(t, http.MethodPost, request.path, "application/json", strings.NewReader(request.body), http.Header{"X-Dispatch-User": {"alice"}})
		if response.status != http.StatusCreated {
			t.Fatalf("POST %s: status %d body %s", request.path, response.status, response.body)
		}
		var created struct{ Key string }
		if err := json.Unmarshal(response.body, &created); err != nil {
			t.Fatalf("decode %s: %v", request.path, err)
		}
		harness.issue = created.Key
	}
	server.stop(t)
	return harness
}

// waitForSettlement waits until the document a stored upload wrote has settled: the settlement
// that has read every update deletes the document's doc_settlements_pending row as it commits.
func (h *memoryHarness) waitForSettlement(t *testing.T, upload uploadResult) {
	t.Helper()
	if upload.status != http.StatusCreated {
		return
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var owed bool
		if err := h.database.Pool.QueryRow(context.Background(),
			`select exists(select 1 from doc_settlements_pending where artifact_id = $1)`, upload.artifactID,
		).Scan(&owed); err != nil {
			t.Fatalf("read the document's pending settlement: %v", err)
		}
		if !owed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("document %s never settled", upload.artifactID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dispatchProcess is a Dispatch server: this test binary running main.
type dispatchProcess struct {
	command *exec.Cmd
	base    string
	output  *lockedBuffer
	exited  chan struct{}
	idle    int64
}

func (h *memoryHarness) start(t *testing.T) *dispatchProcess {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatalf("free the port: %v", err)
	}
	command := exec.Command(os.Args[0])
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		memoryTestServeEnv + "=1",
		"DATABASE_URL=" + h.database.Pool.Config().ConnString(),
		"DISPATCH_AGENT_TOKEN=" + memoryTestToken,
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS=alice",
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + port,
		"DISPATCH_WEB_DIST=" + t.TempDir(),
		"DISPATCH_SERVER_URL=http://127.0.0.1:" + port,
	}
	server := &dispatchProcess{command: command, base: "http://127.0.0.1:" + port, output: &lockedBuffer{}, exited: make(chan struct{})}
	command.Stdout, command.Stderr = server.output, server.output
	if err := command.Start(); err != nil {
		t.Fatalf("start a Dispatch server: %v", err)
	}
	go func() {
		_ = command.Wait()
		close(server.exited)
	}()
	t.Cleanup(func() { server.stop(t) })
	deadline := time.Now().Add(time.Minute)
	for {
		if response, err := http.Get(server.base + "/healthz"); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-server.exited:
			t.Fatalf("the Dispatch server exited while starting:\n%s", server.output.tail())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Dispatch server did not answer /healthz:\n%s", server.output.tail())
		}
		time.Sleep(50 * time.Millisecond)
	}
	server.idle = server.resident(t, "VmRSS")
	return server
}

func (p *dispatchProcess) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
		_ = p.command.Process.Kill()
		<-p.exited
	}
}

// resident is the server's resident memory in bytes, VmRSS now or VmHWM its peak.
func (p *dispatchProcess) resident(t *testing.T, field string) int64 {
	t.Helper()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", p.command.Process.Pid))
	if err != nil {
		t.Fatalf("read the server's memory (has it died?): %v\n%s", err, p.output.tail())
	}
	for _, line := range strings.Split(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, field+":"); ok {
			kibibytes, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
			if err != nil {
				t.Fatalf("parse %s %q: %v", field, value, err)
			}
			return kibibytes << 10
		}
	}
	t.Fatalf("no %s in the server's status", field)
	return 0
}

// peakAboveIdle runs during and reports the most the server's resident memory rose above what it
// held idle, freshly started, while it ran.
func (p *dispatchProcess) peakAboveIdle(t *testing.T, during func()) int64 {
	t.Helper()
	// Writing 5 to clear_refs resets the peak (VmHWM) to the memory resident now.
	if err := os.WriteFile(fmt.Sprintf("/proc/%d/clear_refs", p.command.Process.Pid), []byte("5"), 0); err != nil {
		t.Fatalf("reset the server's peak memory: %v", err)
	}
	during()
	return p.resident(t, "VmHWM") - p.idle
}

type response struct {
	status int
	body   []byte
}

func (p *dispatchProcess) send(t *testing.T, method, path, contentType string, body io.Reader, header http.Header) response {
	t.Helper()
	answer, err := p.trySend(method, path, contentType, body, header)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// trySend is send for a goroutine other than the test's, which may not end the test.
func (p *dispatchProcess) trySend(method, path, contentType string, body io.Reader, header http.Header) (response, error) {
	request, err := http.NewRequest(method, p.base+path, body)
	if err != nil {
		return response{}, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	request.Header = header
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	answer, err := (&http.Client{Timeout: 20 * time.Minute}).Do(request)
	if err != nil {
		return response{}, fmt.Errorf("%s %s: %w\n%s", method, path, err, p.output.tail())
	}
	defer answer.Body.Close()
	read, err := io.ReadAll(answer.Body)
	if err != nil {
		return response{}, fmt.Errorf("read %s %s: %w", method, path, err)
	}
	return response{status: answer.StatusCode, body: read}, nil
}

func (p *dispatchProcess) get(t *testing.T, path string) {
	t.Helper()
	if answer := p.send(t, http.MethodGet, path, "", nil, http.Header{"X-Dispatch-User": {"alice"}}); answer.status != http.StatusOK {
		t.Fatalf("GET %s: status %d body %.300s", path, answer.status, answer.body)
	}
}

type uploadResult struct {
	status     int
	body       []byte
	artifactID string
	elapsed    time.Duration
}

// upload sends markdown as a new document of issue through POST /api/v1/issues/{key}/artifacts,
// as a JSON body or a multipart file, with an agent's bearer token as production's checks do.
func (p *dispatchProcess) upload(t *testing.T, mode, issue, markdown string) uploadResult {
	t.Helper()
	result, err := p.tryUpload(mode, issue, markdown)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// tryUpload is upload for a goroutine other than the test's.
func (p *dispatchProcess) tryUpload(mode, issue, markdown string) (uploadResult, error) {
	return p.tryUploadNamed(mode, issue, fmt.Sprintf("memory-%d.md", time.Now().UnixNano()), markdown)
}

// tryUploadNamed is tryUpload of a document named name: a new version of the issue's document of
// that name where it has one.
func (p *dispatchProcess) tryUploadNamed(mode, issue, name, markdown string) (uploadResult, error) {
	actor := map[string]string{"kind": "session", "id": "memory-test"}
	var body bytes.Buffer
	var contentType string
	switch mode {
	case "json":
		if err := json.NewEncoder(&body).Encode(map[string]any{"name": name, "content": markdown, "actor": actor}); err != nil {
			return uploadResult{}, fmt.Errorf("encode the upload: %w", err)
		}
		contentType = "application/json"
	case "multipart":
		writer := multipart.NewWriter(&body)
		encodedActor, err := json.Marshal(actor)
		if err != nil {
			return uploadResult{}, fmt.Errorf("encode the actor: %w", err)
		}
		for field, value := range map[string]string{"name": name, "actor": string(encodedActor)} {
			if err := writer.WriteField(field, value); err != nil {
				return uploadResult{}, fmt.Errorf("write %s: %w", field, err)
			}
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="file"; filename="`+name+`"`)
		header.Set("Content-Type", "text/markdown")
		part, err := writer.CreatePart(header)
		if err != nil {
			return uploadResult{}, fmt.Errorf("create the file part: %w", err)
		}
		if _, err := io.WriteString(part, markdown); err != nil {
			return uploadResult{}, fmt.Errorf("write the file part: %w", err)
		}
		if err := writer.Close(); err != nil {
			return uploadResult{}, fmt.Errorf("close the multipart body: %w", err)
		}
		contentType = writer.FormDataContentType()
	default:
		return uploadResult{}, fmt.Errorf("unknown upload mode %q", mode)
	}
	started := time.Now()
	answer, err := p.trySend(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", contentType, &body, http.Header{"Authorization": {"Bearer " + memoryTestToken}})
	if err != nil {
		return uploadResult{}, err
	}
	result := uploadResult{status: answer.status, body: answer.body, elapsed: time.Since(started).Round(time.Millisecond)}
	if answer.status == http.StatusCreated {
		var created struct {
			Artifact struct{ ID string } `json:"artifact"`
		}
		if err := json.Unmarshal(answer.body, &created); err != nil {
			return uploadResult{}, fmt.Errorf("decode the upload's answer: %w", err)
		}
		result.artifactID = created.Artifact.ID
	}
	return result, nil
}

// requireAnswered fails the test unless the upload was stored or refused with a 4xx naming its
// code and why.
func requireAnswered(t *testing.T, upload uploadResult) {
	t.Helper()
	if upload.status == http.StatusCreated {
		return
	}
	var refusal struct{ Code, Error string }
	if upload.status < 400 || upload.status >= 500 || json.Unmarshal(upload.body, &refusal) != nil || refusal.Code == "" || refusal.Error == "" {
		t.Fatalf("upload answered %d %.300s, want 201 or a 4xx naming its code and reason", upload.status, upload.body)
	}
}

// loadOverWebsocket opens the document's room as a browser does - it connects and asks for the
// whole document - and closes once the room has sent it.
func (p *dispatchProcess) loadOverWebsocket(t *testing.T, artifactID string) {
	t.Helper()
	address := "ws" + strings.TrimPrefix(p.base, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	connection, answer, err := gws.DefaultDialer.Dial(address, http.Header{"X-Dispatch-User": {"alice"}})
	if err != nil {
		t.Fatalf("connect to the document's room: %v (%#v)", err, answer)
	}
	defer connection.Close()
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(0)
	})
	if err := connection.WriteMessage(gws.BinaryMessage, append(frame, ygsync.EncodeSyncStep1(crdt.New())...)); err != nil {
		t.Fatalf("ask the room for the document: %v", err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read the room's answer: %v", err)
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			continue
		}
		if kind, err := decoder.ReadVarUint(); err != nil || kind != 0 {
			continue
		}
		if kind, _, err := ygsync.ReadSyncMessage(decoder.RemainingBytes()); err == nil && kind == ygsync.MsgSyncStep2 {
			break
		}
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close the connection: %v", err)
	}
	// The room's last peer leaving runs its settlement; give it the time a settlement of the
	// heaviest admitted document takes here, so the peak counts it.
	time.Sleep(3 * time.Second)
}

// lockedBuffer is the server's output, written by the process's copying goroutine and read by the
// test when it fails.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) tail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	output := b.buffer.String()
	if len(output) > 4000 {
		output = output[len(output)-4000:]
	}
	return output
}
