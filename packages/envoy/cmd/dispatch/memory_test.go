//go:build memory

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

const (
	// requestMemoryBound is the most one request may raise a fresh server's resident memory above
	// what it held idle, and concurrentMemoryBound the most two at once may (LEGION-481). Production
	// runs one task of 1,024 MiB.
	requestMemoryBound    = 256 << 20
	concurrentMemoryBound = 512 << 20
	documentCap           = 1 << 20
	// marginEditAllowance is the most a margin filled to its bounds may add to what a cold edit of
	// its document holds: two cold edits of one heaviest document, with a full margin and without,
	// differed by -10 to +88 MiB over four runs (the four-column table the most), where a margin of
	// ninety-six 900 KB suggestions adds over 300 MiB.
	marginEditAllowance = 128 << 20
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
// hold at most the bound, a read on a server that has not loaded the document. Escaped syntax is
// among them (escapedAdmittedShapes): a stored document of `\)\_`, which weighed four elements
// before an escape weighed one, held 256 MiB to read cold.
func TestTheHeaviestStoredDocumentsStayWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	for _, shape := range append(heaviestAdmittedShapes(t), escapedAdmittedShapes(t)...) {
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
// together, on a server that has not loaded the document: `)_`, the hard breaks that, weighed as
// one inline node, held 565 MiB for two reads and 1,190 MiB for four, and escaped syntax, of which
// four cold reads of a stored `\)\_` held 973 MiB before an escape weighed an element. Four reads
// at once are logged beside them, the production task being 1,024 MiB.
func TestConcurrentColdReadsStayWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	var shapes []admittedShape
	for _, shape := range heaviestAdmittedShapes(t) {
		if shape.name == ")_" || strings.HasPrefix(shape.name, "hard breaks") {
			shapes = append(shapes, shape)
		}
	}
	for _, shape := range append(shapes, escapedAdmittedShapes(t)...) {
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
// the asks of one document, each giving an option a 900 KB description, are taken until one would
// leave the document's markdown past 1 MiB, which is refused with 413 CAP_EXCEEDED, and so is every
// one after it; answers of 900 KB to them are all taken, each the document has no room for kept on
// its ask and left out of its block. Either way the document stays within the cap, still reads, and
// a one-word edit on a server that has not loaded it holds at most the bound. Before the bound
// reached these routes, thirty-two such edits grew one document to 28.8 MB, on which a one-word
// edit held 1,204 MiB, and under the production task's 1,024 MiB the server was killed.
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
		// refuses says the route refuses the write the document has no room for, rather than
		// taking it without the text.
		refuses bool
	}{
		{"PATCH /api/v1/asks/{id}", http.MethodPatch, "", map[string]any{"options": []map[string]string{{"label": "A", "description": prose}}}, true},
		{"POST /api/v1/asks/{id}/answer", http.MethodPost, "/answer", map[string]any{"text": prose, "expected_edited_at": nil}, false},
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
				case route.refuses && refusedWithAdvice(answer):
					refused = true
				case route.refuses:
					t.Errorf("write %d answered %d %.300s, want 200 until one is refused with 413 CAP_EXCEEDED saying to shorten the change, and 413 after it", index+1, answer.status, answer.body)
				default:
					t.Errorf("answer %d answered %d %.300s, want 200", index+1, answer.status, answer.body)
				}
			}
			if route.refuses && !refused {
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

// An ask's question and options are plain text the server makes into markdown, so they are weighed
// before they are rendered, on a real Dispatch process as in the route's tests: an option
// description of 920 KB of `)_`, whose edit held 417 MiB before its text was weighed, and one of
// 330,000 lines, whose edit did not answer within twenty minutes, are each refused with 413
// CAP_EXCEEDED, naming the text's weight, within the bound and in seconds.
func TestAnAsksTextIsWeighedBeforeItIsRendered(t *testing.T) {
	memory := newMemoryHarness(t)
	server := memory.start(t)
	issue := server.createIssue(t, "Ask text", "Before.\n\n:::ask{#ask-0 urgency=\"med\" multiple=\"false\" state=\"open\"}\nQuestion 0?\n:::\n")
	askID := server.blockAsks(t, issue.Key, 1)[0]
	for _, description := range []struct{ name, text string }{
		{"920 KB of )_", strings.Repeat(")_", 460_000)},
		{"330,000 lines", strings.Repeat("a\n", 330_000)},
	} {
		body, err := json.Marshal(map[string]any{"options": []map[string]string{{"label": "A", "description": description.text}}})
		if err != nil {
			t.Fatalf("encode the edit: %v", err)
		}
		var answer response
		started := time.Now()
		peak := server.peakAboveIdle(t, func() {
			answer = server.send(t, http.MethodPatch, "/api/v1/asks/"+askID, "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
		})
		elapsed := time.Since(started)
		t.Logf("an option description of %s: %d in %s, %d MiB above idle: %.200s", description.name, answer.status, elapsed, peak>>20, answer.body)
		if answer.status != http.StatusRequestEntityTooLarge || !strings.Contains(string(answer.body), "this write's text weighs at least") {
			t.Errorf("an option description of %s answered %d %.300s, want 413 CAP_EXCEEDED naming the text's weight", description.name, answer.status, answer.body)
		}
		if peak > requestMemoryBound {
			t.Errorf("refusing an option description of %s held %d MiB above idle, want at most %d MiB", description.name, peak>>20, requestMemoryBound>>20)
		}
		if elapsed > 10*time.Second {
			t.Errorf("refusing an option description of %s took %s, want at most 10s", description.name, elapsed)
		}
	}
}

// An answer is stored on its ask as well as in its block, and settlement writes it back into a block
// that returns to the document, so returning blocks cannot grow a document past what one upload may
// hold on a real Dispatch process either: twenty-four answers of 900 KB, each taken against a
// document their deleted blocks had left small, then all twenty-four blocks returned in one edit,
// leave the document within 1 MiB, the edit and its settlement hold at most the bound, and so does
// each cold read of the document. Before settlement weighed what it writes back, they left a 21.6 MB
// document whose cold text read held 373 MiB, and four at once over 1,000 MiB.
func TestReturnedAnswersCannotGrowADocumentPastWhatOneUploadMayHold(t *testing.T) {
	memory := newMemoryHarness(t)
	const asks = 24
	var spec, blocks strings.Builder
	spec.WriteString("Before sentinel.\n")
	for index := range asks {
		block := fmt.Sprintf("\n:::ask{#ask-%d urgency=\"med\" multiple=\"false\"}\nQuestion %d?\n:::\n", index, index)
		spec.WriteString(block)
		blocks.WriteString(block)
	}
	body, err := json.Marshal(map[string]any{"text": strings.Repeat("word ", 180_000), "expected_edited_at": nil})
	if err != nil {
		t.Fatalf("encode the answer: %v", err)
	}
	server := memory.start(t)
	issue := server.createIssue(t, "Returned answers", spec.String())
	for index, askID := range server.blockAsks(t, issue.Key, asks) {
		if answer := server.send(t, http.MethodPost, "/api/v1/asks/"+askID+"/answer", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}}); answer.status != http.StatusOK {
			t.Fatalf("answer %d: %d %.300s, want 200", index+1, answer.status, answer.body)
		}
		if answer := server.edit(t, issue.PrimaryArtifactID, map[string]any{"op": "delete", "block": fmt.Sprintf("ask-%d", index)}); answer.status != http.StatusOK {
			t.Fatalf("delete block %d: %d %.300s, want 200", index+1, answer.status, answer.body)
		}
	}
	memory.waitForSettled(t, issue.PrimaryArtifactID)
	var answer response
	peak := server.peakAboveIdle(t, func() {
		answer = server.edit(t, issue.PrimaryArtifactID, map[string]any{"op": "insert", "after": "end", "markdown": blocks.String()})
		memory.waitForSettled(t, issue.PrimaryArtifactID)
	})
	text := server.text(t, issue.PrimaryArtifactID)
	server.stop(t)
	t.Logf("returning %d answered blocks in one %d-byte edit answered %d and left a %d-byte document, %d MiB above idle with its settlement", asks, blocks.Len(), answer.status, len(text), peak>>20)
	if answer.status != http.StatusOK {
		t.Fatalf("the edit returning the blocks answered %d %.300s, want 200", answer.status, answer.body)
	}
	if len(text) > documentCap {
		t.Errorf("returning the blocks left a %d-byte document, past the %d-byte cap", len(text), documentCap)
	}
	if peak > requestMemoryBound {
		t.Errorf("the edit returning the blocks and its settlement held %d MiB above idle, want at most %d MiB", peak>>20, requestMemoryBound>>20)
	}
	memory.coldPeaks(t, "returned answers", issue.PrimaryArtifactID, sentinelEdit)
}

// sentinelEdit is a one-word edit of a document that holds "sentinel".
var sentinelEdit = map[string]any{"op": "replace", "find": "sentinel", "with": "marker"}

// coldPeaks reads and edits a document on servers that have not loaded it (coldReads, coldEdit)
// and fails the test where a read or the edit holds more than the bound above idle.
func (h *memoryHarness) coldPeaks(t *testing.T, name, artifactID string, edit map[string]any) {
	t.Helper()
	h.coldReads(t, name, artifactID)
	if peak := h.coldEdit(t, name, artifactID, edit); peak > requestMemoryBound {
		t.Errorf("%s: a cold one-word edit held %d MiB above idle, want at most %d MiB", name, peak>>20, requestMemoryBound>>20)
	}
}

// coldReads reads a document on servers that have not loaded it - its text and a websocket load of
// its room, each on a fresh server, then four reads of its text at once - and fails the test where
// one of the first two holds more than the bound above idle. Every peak is logged under name; the
// four reads at once are logged beside the production task's 1,024 MiB.
func (h *memoryHarness) coldReads(t *testing.T, name, artifactID string) {
	t.Helper()
	for _, cold := range []struct {
		name string
		run  func(server *dispatchProcess)
	}{
		{"text read", func(server *dispatchProcess) { server.get(t, "/api/v1/artifacts/"+artifactID+"/text") }},
		{"websocket load", func(server *dispatchProcess) { server.loadOverWebsocket(t, artifactID) }},
	} {
		server := h.start(t)
		peak := server.peakAboveIdle(t, func() { cold.run(server) })
		server.stop(t)
		t.Logf("%s: a cold %s, %d MiB above idle", name, cold.name, peak>>20)
		if peak > requestMemoryBound {
			t.Errorf("%s: a cold %s held %d MiB above idle, want at most %d MiB", name, cold.name, peak>>20, requestMemoryBound>>20)
		}
	}
	server := h.start(t)
	peak := server.peakAboveIdle(t, func() {
		failures := make([]error, 4)
		var group sync.WaitGroup
		for index := range failures {
			group.Go(func() {
				answer, err := server.trySend(http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
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
	t.Logf("%s: four cold text reads at once, %d MiB above idle", name, peak>>20)
}

// coldEdit sends edit, a one-word edit, to a document on a server that has not loaded it, fails the
// test unless it is taken, and logs and returns its peak above the server's idle memory.
func (h *memoryHarness) coldEdit(t *testing.T, name, artifactID string, edit map[string]any) int64 {
	t.Helper()
	server := h.start(t)
	var answer response
	peak := server.peakAboveIdle(t, func() { answer = server.edit(t, artifactID, edit) })
	server.stop(t)
	t.Logf("%s: a cold one-word edit answered %d, %d MiB above idle", name, answer.status, peak>>20)
	if answer.status != http.StatusOK {
		t.Errorf("%s: a cold one-word edit answered %d %.300s, want 200", name, answer.status, answer.body)
	}
	return peak
}

// A document's margin - every comment and suggestion record, which every load of the document
// builds - holds at most 256 KiB of text a record and 1 MiB in all, so on a real Dispatch process
// no comment, and no cold read or edit of the document it leaves, holds more than the bound: the
// thirty-two and ninety-six suggestions of 900 KB that main takes, suggestions filling the margin
// to its bound, and an ordinary workload of comments, suggestions and replies, which is taken
// whole. On main thirty-two suggestions of 900 KB took a cold websocket load of their document to
// 296 MiB, and sixty-four took four cold reads at once to 1,223 MiB.
func TestTheMarginStaysWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	var spec strings.Builder
	spec.WriteString("Before sentinel.\n\n")
	for index := range 100 {
		fmt.Fprintf(&spec, "q%02dx ", index)
	}
	spec.WriteString("\n")
	suggestion := func(replacement string) func(index int, _ []string) map[string]any {
		return func(index int, _ []string) map[string]any {
			return map[string]any{
				"body":       "a suggestion",
				"anchor":     map[string]any{"artifact": "spec", "quote": fmt.Sprintf("q%02dx", index)},
				"suggestion": map[string]string{"replace_with": replacement},
			}
		}
	}
	sentence := strings.Repeat("A sentence of ordinary length. ", 16)
	ordinary := func(index int, roots []string) map[string]any {
		switch {
		case index%6 == 5:
			return map[string]any{"body": sentence, "reply_to": roots[len(roots)-1]}
		case index%3 == 0:
			return suggestion(sentence)(index, roots)
		default:
			return map[string]any{"body": sentence, "anchor": map[string]any{"artifact": "spec", "quote": fmt.Sprintf("q%02dx", index)}}
		}
	}
	for _, shape := range []struct {
		name     string
		comments int
		comment  func(index int, roots []string) map[string]any
		allTaken bool
	}{
		{"32 suggestions of 900 KB", 32, suggestion(strings.Repeat("a", 900_000)), false},
		{"96 suggestions of 900 KB", 96, suggestion(strings.Repeat("a", 900_000)), false},
		{"suggestions of 255,000 bytes to the margin's bound", 5, suggestion(strings.Repeat("a", 255_000)), false},
		{"an ordinary workload", 72, ordinary, true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			server := memory.start(t)
			issue := server.createIssue(t, "Margin of "+shape.name, spec.String())
			var roots []string
			taken, refused, heaviest := 0, 0, int64(0)
			for index := range shape.comments {
				request := shape.comment(index, roots)
				body, err := json.Marshal(request)
				if err != nil {
					t.Fatalf("encode comment %d: %v", index+1, err)
				}
				var answer response
				peak := server.peakAboveIdle(t, func() {
					answer = server.send(t, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
				})
				heaviest = max(heaviest, peak)
				switch {
				case answer.status == http.StatusCreated:
					taken++
					var created struct{ ID string }
					if err := json.Unmarshal(answer.body, &created); err != nil {
						t.Fatalf("decode comment %d: %v", index+1, err)
					}
					if _, reply := request["reply_to"]; !reply {
						roots = append(roots, created.ID)
					}
				case answer.status == http.StatusRequestEntityTooLarge && strings.Contains(string(answer.body), `"code":"CAP_EXCEEDED"`):
					refused++
				default:
					t.Fatalf("comment %d answered %d %.300s, want 201 or 413 CAP_EXCEEDED", index+1, answer.status, answer.body)
				}
			}
			server.stop(t)
			t.Logf("%s: %d taken, %d refused 413, the heaviest request %d MiB above idle", shape.name, taken, refused, heaviest>>20)
			if heaviest > requestMemoryBound {
				t.Errorf("%s: a comment held %d MiB above idle, want at most %d MiB", shape.name, heaviest>>20, requestMemoryBound>>20)
			}
			if shape.allTaken && refused > 0 {
				t.Errorf("%s: %d of %d comments refused, want every one taken", shape.name, refused, shape.comments)
			}
			memory.coldPeaks(t, shape.name, issue.PrimaryArtifactID, sentinelEdit)
		})
	}
}

// The heaviest documents the element limit admits, each with suggestions of 255,000 bytes until
// its margin's bound refuses one - the most a stored document's rendering and its margin can hold
// together - are each still read and loaded within the bound on a server that has not loaded them,
// and a full margin adds at most marginEditAllowance to what a cold one-word edit of the document
// held before it. The edit replaces a word with one as long, so it is weighed whole - its
// rendering's elements and its trial load - and taken. What the edit holds without a margin is the
// element limit's own (logged, and over the bound for the densest shapes: tables and inline HTML),
// which the margin's bounds do not move.
func TestTheHeaviestDocumentsWithAFullMarginStayWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	replacement := strings.Repeat("a", 255_000)
	edit := map[string]any{"op": "replace", "find": "word", "with": "wore", "occurrence": 0}
	for _, shape := range heaviestAdmittedShapes(t) {
		t.Run(shape.name, func(t *testing.T) {
			writer := memory.start(t)
			upload := writer.upload(t, "json", memory.issue, shape.markdown)
			if upload.status != http.StatusCreated {
				t.Fatalf("upload of the heaviest %s the limit admits: status %d body %.300s, want 201", shape.name, upload.status, upload.body)
			}
			memory.waitForSettlement(t, upload)
			writer.stop(t)
			alone := memory.coldEdit(t, shape.name+" with no margin", upload.artifactID, edit)
			writer = memory.start(t)
			taken := 0
			for {
				body, err := json.Marshal(map[string]any{
					"body":       "a suggestion",
					"anchor":     map[string]any{"artifact": upload.artifactID, "quote": "word", "occurrence": taken},
					"suggestion": map[string]string{"replace_with": replacement},
				})
				if err != nil {
					t.Fatalf("encode the suggestion: %v", err)
				}
				answer := writer.send(t, http.MethodPost, "/api/v1/issues/"+memory.issue+"/comments", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
				if answer.status == http.StatusCreated && taken < 8 {
					taken++
					continue
				}
				if answer.status != http.StatusRequestEntityTooLarge || !strings.Contains(string(answer.body), "a document's margin holds at most 1 MiB") {
					t.Fatalf("suggestion %d answered %d %.300s, want 201 until the margin's bound refuses one", taken+1, answer.status, answer.body)
				}
				break
			}
			writer.stop(t)
			t.Logf("%s: %d suggestions of %d bytes taken before the margin's bound", shape.name, taken, len(replacement))
			name := shape.name + " with a full margin"
			memory.coldReads(t, name, upload.artifactID)
			if full := memory.coldEdit(t, name, upload.artifactID, edit); full-alone > marginEditAllowance {
				t.Errorf("%s: a cold one-word edit held %d MiB above idle, %d MiB more than with no margin, want at most %d MiB more", name, full>>20, (full-alone)>>20, marginEditAllowance>>20)
			}
		})
	}
}

// A new version of 16,384 headings, what one upload may hold, over a first version of one line is
// taken on every try on a real Dispatch process, and its document then reads on a server that has
// not loaded it. A transaction's writes take the id after every writer the document holds; under a
// random id a new version that drew an id below its first version's writer parked every item it
// wrote, and the load margin refused it on 7 of 12 tries.
func TestANewVersionAtTheLimitIsTakenOnEveryTry(t *testing.T) {
	memory := newMemoryHarness(t)
	const tries = 12
	headings := strings.Repeat("# a\n", 16_384)
	server := memory.start(t)
	var taken []string
	for try := range tries {
		name := fmt.Sprintf("near-limit-%d.md", try)
		first, err := server.tryUploadNamed("multipart", memory.issue, name, "One line.\n")
		if err != nil {
			t.Fatal(err)
		}
		if first.status != http.StatusCreated {
			t.Fatalf("try %d: the first version answered %d %.300s, want 201", try+1, first.status, first.body)
		}
		memory.waitForSettlement(t, first)
		version, err := server.tryUploadNamed("multipart", memory.issue, name, headings)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("try %d: a new version of 16,384 headings answered %d %.200s", try+1, version.status, version.body)
		if version.status == http.StatusCreated {
			taken = append(taken, version.artifactID)
			memory.waitForSettlement(t, version)
		}
	}
	server.stop(t)
	t.Logf("%d of %d new versions taken", len(taken), tries)
	if len(taken) != tries {
		t.Errorf("%d of %d new versions of 16,384 headings were refused, want every one taken", tries-len(taken), tries)
	}
	reader := memory.start(t)
	for _, artifactID := range taken {
		if text := reader.text(t, artifactID); strings.Count(text, "# a\n") != 16_384 {
			t.Errorf("document %s reads back %d bytes, want its 16,384 headings", artifactID, len(text))
		}
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
// minute to refuse, so it answers within ten seconds. Escaped syntax weighed four elements a
// mebibyte, and its upload held 265 to 323 MiB before its rendering was refused by its bytes.
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
		{`\~a`, repeated(`\~a`)},
		{`)\_`, repeated(`)\_`)},
		{`\)\_`, repeated(`\)\_`)},
		{`\[a`, repeated(`\[a`)},
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

// escapedAdmittedShapes are the heaviest documents of escaped syntax the element limit admits: each
// escape spells a character - `~`, `_`, `[` - that the renderer reads back bare, as the delimiter or
// opener it makes, so a document of them costs what that syntax does. Weighed as the four elements
// of its one text, a stored `\)\_` held 275 MiB to store, 256 MiB to read cold and 973 MiB for four
// cold reads at once.
func escapedAdmittedShapes(t *testing.T) []admittedShape {
	t.Helper()
	var shapes []admittedShape
	for _, unit := range []string{`\~a`, `)\_`, `\)\_`, `\[a`, "&#95;a"} {
		shapes = append(shapes, admittedShape{name: unit, markdown: heaviestAdmitted(t, func(units int) string { return strings.Repeat(unit, units) })})
	}
	return shapes
}

// heaviestAdmitted is the document of the most units build makes that the element limit admits,
// padded with a paragraph of plain words, which weighs next to nothing, to the cap: the cap of what
// the server stores, its rendering, which can run longer than the markdown written - a blank line
// between two headings, a line feed at the end - so the padding gives way to that. The rendering is
// weighed as well as the markdown, as the server weighs a new document: an escape it writes bare
// (`\[a` is stored `[a`) can weigh more there than as written.
func heaviestAdmitted(t *testing.T, build func(units int) string) string {
	t.Helper()
	document := func(units int) string {
		text := build(units) + "\n\n"
		return text + strings.Repeat("word ", documentCap/5+1)[:documentCap-len(text)]
	}
	admitted := func(units int) bool {
		tree, err := pmdoc.Parse(document(units))
		if err != nil && !errors.Is(err, pmdoc.ErrTooManyElements) {
			t.Fatalf("parse %d units: %v", units, err)
		}
		if err != nil {
			return false
		}
		rendered, err := pmdoc.Render(tree)
		if err != nil {
			t.Fatalf("render %d units: %v", units, err)
		}
		return !pmdoc.MeasureDocument(rendered).TooHeavy()
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
