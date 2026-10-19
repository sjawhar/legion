package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An edit rewrites a block's text as one span of the text the block held, counted in the UTF-16
// units Yjs counts in. Two characters outside the Basic Multilingual Plane that share a high
// surrogate (U+1F4CA and U+1F4CB, both D83D xxxx) or a low one (U+1F60A and U+1FA0A, both xxxx DE0A)
// agree on one unit of their pairs, so a span placed by comparing units would begin or end inside a
// pair, and half a pair stores as U+FFFD. These tests send the edit through the route and read the
// document back, so the stored text is what they hold, not a helper's return value.

func applyEdits(t *testing.T, handler http.Handler, artifactID string, ops []map[string]any) {
	t.Helper()
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{"ops": ops}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
}

// requireStored fails unless the stored markdown holds no U+FFFD and holds every one of parts.
func requireStored(t *testing.T, markdown string, parts ...string) {
	t.Helper()
	if strings.ContainsRune(markdown, '\uFFFD') {
		t.Fatalf("the stored document holds U+FFFD:\n%s", markdown)
	}
	for _, part := range parts {
		if !strings.Contains(markdown, part) {
			t.Fatalf("the stored document lacks %q:\n%s", part, markdown)
		}
	}
}

const emojiTable = "| session | id | title |\n| --- | --- | --- |\n" +
	"| s1 | a1 | 📊 GRADE: grading tool |\n" +
	"| s2 | a2 | 🚰 PIPE: task pipeline |\n" +
	"| s3 | a3 | 🔍 VERIF: oss verifier |\n" +
	"| s4 | a4 | 📋 TASK: task workflow |\n" +
	"| s5 | a5 | 🗂️ REPOS: repo layout |\n" +
	"| s6 | a6 | 📈 TREND: trends |\n"

// Rows deleted and rows inserted in one batch are written over each other in place, cell by cell,
// so a cell that held one title now holds another.
func TestDocumentEditsKeepEmojiWhenRowsAreReplacedInPlace(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Emoji table rows", emojiTable)

	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	table := tableBlockOf(t, blocks).ID
	applyEdits(t, handler, issue.PrimaryArtifactID, []map[string]any{
		{"op": "delete_row", "block": table, "index": 4},
		{"op": "delete_row", "block": table, "index": 3},
		{"op": "delete_row", "block": table, "index": 2},
		{"op": "delete_row", "block": table, "index": 1},
		{"op": "insert", "after": "title", "markdown": "| n1 | b1 | 🚰 PIPE: task pipeline |\n| n2 | b2 | 🔍 VERIF: oss verifier |\n| n3 | b3 | 📋 TASK: task workflow |\n| n4 | b4 | 📊 GRADE: grading tool |"},
	})

	requireStored(t, documentMarkdown(t, handler, issue.PrimaryArtifactID),
		"| n1 | b1 | 🚰 PIPE: task pipeline |",
		"| n2 | b2 | 🔍 VERIF: oss verifier |",
		"| n3 | b3 | 📋 TASK: task workflow |",
		"| n4 | b4 | 📊 GRADE: grading tool |",
		"| s5 | a5 | 🗂️ REPOS: repo layout |",
		"| s6 | a6 | 📈 TREND: trends |",
	)
}

// A row no operation names is written too when the rows around it change count: deleting the
// first body row and adding one after the last moves every other row up a place, so each of their
// titles is diffed against the title of the row above it (📊 against 🚰, 🚰 against 🔍, and on).
func TestDocumentEditsKeepEmojiInRowsNoOperationNames(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Emoji shifted rows", emojiTable)

	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	table := tableBlockOf(t, blocks).ID
	applyEdits(t, handler, issue.PrimaryArtifactID, []map[string]any{
		{"op": "delete_row", "block": table, "index": 1},
		{"op": "insert", "after": "📈 TREND: trends", "markdown": "| s7 | a7 | 🏉 SCRUM: scrum master |"},
	})

	requireStored(t, documentMarkdown(t, handler, issue.PrimaryArtifactID),
		"| s2 | a2 | 🚰 PIPE: task pipeline |",
		"| s3 | a3 | 🔍 VERIF: oss verifier |",
		"| s4 | a4 | 📋 TASK: task workflow |",
		"| s5 | a5 | 🗂️ REPOS: repo layout |",
		"| s6 | a6 | 📈 TREND: trends |",
		"| s7 | a7 | 🏉 SCRUM: scrum master |",
	)
}

// One emoji replaced by another that shares its high surrogate (the prefix of the two texts ends
// inside the pair) or its low surrogate (the suffix starts inside it), with text around both.
func TestDocumentEditsKeepEmojiWhenOneReplacesAnotherSharingASurrogate(t *testing.T) {
	for name, test := range map[string]struct{ was, now string }{
		"shared high surrogate": {was: "\U0001F4CA", now: "\U0001F4CB"},
		"shared low surrogate":  {was: "\U0001F60A", now: "\U0001FA0A"},
		"an emoji beside one it shares a high surrogate with": {
			was: "\U0001F4CA\U0001F4CA",
			now: "\U0001F4CA\U0001F4CB",
		},
	} {
		t.Run(name, func(t *testing.T) {
			handler := newTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Emoji replace", "before "+test.was+" after\n")
			applyEdits(t, handler, issue.PrimaryArtifactID, []map[string]any{
				{"op": "replace", "find": test.was, "with": test.now},
			})
			requireStored(t, documentMarkdown(t, handler, issue.PrimaryArtifactID), "before "+test.now+" after")
		})
	}
}
