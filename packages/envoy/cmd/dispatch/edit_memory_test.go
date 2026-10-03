package main

import (
	"net/http"
	"strings"
	"testing"
)

// A one-word edit of the heaviest documents the element limit admits, and the settlement after it,
// hold at most the bound above the idle memory of a server that has not loaded the document, on a
// real Dispatch process (LEGION-504). The edit held a room, a fork of it, three trees of the
// document and a load of the state it left, all at once: 379 MiB for 65,536 inline HTML spans,
// 255-275 MiB for a table.
func TestAColdEditOfTheHeaviestDocumentsStaysWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	writer := memory.start(t)
	type stored struct{ name, artifactID string }
	var documents []stored
	for _, shape := range heaviestAdmittedShapes(t) {
		upload := writer.upload(t, "json", memory.issue, withSentinelWord(t, shape.markdown))
		if upload.status != http.StatusCreated {
			t.Fatalf("upload of the heaviest %s the limit admits: status %d body %.300s, want 201", shape.name, upload.status, upload.body)
		}
		memory.waitForSettlement(t, upload)
		documents = append(documents, stored{name: shape.name, artifactID: upload.artifactID})
	}
	writer.stop(t)
	for _, document := range documents {
		t.Run(document.name, func(t *testing.T) {
			server := memory.start(t)
			var answer response
			peak := server.peakAboveIdle(t, func() {
				answer = server.edit(t, document.artifactID, map[string]any{"op": "replace", "find": sentinelWord, "with": "wird"})
				memory.waitForSettled(t, document.artifactID)
			})
			server.stop(t)
			if answer.status != http.StatusOK {
				t.Fatalf("a one-word edit of %s answered %d %.300s, want 200", document.name, answer.status, answer.body)
			}
			t.Logf("%s: a cold one-word edit and its settlement, %d MiB above idle", document.name, peak>>20)
			if peak > requestMemoryBound {
				t.Errorf("a cold one-word edit of %s held %d MiB above idle, want at most %d MiB", document.name, peak>>20, requestMemoryBound>>20)
			}
		})
	}
}

// sentinelWord is a word no heaviest admitted document holds but where withSentinelWord puts it.
const sentinelWord = "wurd"

// withSentinelWord is markdown with the first word of its padding (heaviestAdmitted) made
// sentinelWord, so a one-word edit finds it once and leaves the document's length as it was.
func withSentinelWord(t *testing.T, markdown string) string {
	t.Helper()
	at := strings.LastIndex(markdown, "\n\nword ")
	if at < 0 {
		t.Fatalf("no padding in the document")
	}
	return markdown[:at] + "\n\n" + sentinelWord + " " + markdown[at+len("\n\nword "):]
}
