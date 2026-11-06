//go:build memory

package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// A one-word edit of the heaviest documents the element limit admits, and the settlement after it,
// hold at most the bound above the idle memory of a server that has not loaded the document, on a
// real Dispatch process (LEGION-504). Each document is edited on peakRuns fresh servers and the
// least of their peaks is held to the bound, since garbage the collector has not reclaimed only
// adds to one run's peak (peaksAboveIdle has the numbers).
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
			// Each run swaps the sentinel word for its other spelling and back, so every run is a
			// one-word edit of the same document.
			words := [2]string{sentinelWord, "wird"}
			run := 0
			peaks := memory.peaksAboveIdle(t, peakRuns, func(server *memoryServer) {
				from, to := words[run%2], words[(run+1)%2]
				run++
				answer := server.edit(t, document.artifactID, map[string]any{"op": "replace", "find": from, "with": to})
				if answer.status != http.StatusOK {
					t.Fatalf("a one-word edit of %s answered %d %.300s, want 200", document.name, answer.status, answer.body)
				}
				memory.waitForSettled(t, document.artifactID)
			})
			least := slices.Min(peaks)
			t.Logf("%s: a cold one-word edit and its settlement, %d MiB above idle at least (%s)", document.name, least>>20, mebibytes(peaks))
			if least > requestMemoryBound {
				t.Errorf("a cold one-word edit of %s held %d MiB above idle in each of %d runs, want at most %d MiB", document.name, least>>20, peakRuns, requestMemoryBound>>20)
			}
		})
	}
}

// mebibytes lists peaks in MiB, as the test logs them.
func mebibytes(peaks []int64) string {
	shown := make([]string, len(peaks))
	for index, peak := range peaks {
		shown[index] = fmt.Sprintf("%d MiB", peak>>20)
	}
	return strings.Join(shown, ", ")
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
