package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Text a caller writes reaches the document with line feeds alone: a CR LF and a lone carriage
// return are each written as a line feed wherever text enters a document - a spec seeding it, an
// upload making a new document or a new version, an edit's insert (a block, or rows into a table),
// replace (in text and in code) and retype (its attributes), an accepted suggestion, a block ask's
// edited text, and an answer written into its ask block. Each stores the document the same text
// written with line feeds stores, so the browser editor, which reads both line endings as a line
// feed, reads it the same way.
func TestWrittenTextReachesTheDocumentWithLineFeeds(t *testing.T) {
	handler, _ := blockAskHandler(t)
	projects := 0
	issue := func(t *testing.T, spec string) (key, artifactID string) {
		t.Helper()
		projects++
		created := createInteractionIssue(t, handler, fmt.Sprintf("LE%c%c", 'A'+projects/26, 'A'+projects%26), "line endings", spec)
		return created.Key, created.PrimaryArtifactID
	}
	ok := func(t *testing.T, response *httptest.ResponseRecorder, what string) {
		t.Helper()
		if response.Code != http.StatusOK && response.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		}
	}
	const document = "# Title\n\nIntro.\n\nOne\ntwo.\n\n```\na\nb\n```\n"
	entries := []struct {
		name  string
		write func(t *testing.T, eol func(string) string) string
	}{
		{"a spec seeding the document", func(t *testing.T, eol func(string) string) string {
			_, artifactID := issue(t, eol(document))
			return documentMarkdown(t, handler, artifactID)
		}},
		{"an upload making a new document, then a new version", func(t *testing.T, eol func(string) string) string {
			key, _ := issue(t, "Intro.\n")
			var uploaded struct {
				Artifact model.Artifact `json:"artifact"`
			}
			for _, content := range []string{"First.\n", document} {
				response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts", map[string]string{
					"name": "notes.md", "content": eol(content),
				}, "alice")
				ok(t, response, "upload")
				uploaded = decodeBody[struct {
					Artifact model.Artifact `json:"artifact"`
				}](t, response)
			}
			return documentMarkdown(t, handler, uploaded.Artifact.ID)
		}},
		{"an insert", func(t *testing.T, eol func(string) string) string {
			_, artifactID := issue(t, "Intro.\n")
			ok(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": eol(document)}},
			}, "alice"), "insert")
			return documentMarkdown(t, handler, artifactID)
		}},
		{"a replace in text", func(t *testing.T, eol func(string) string) string {
			_, artifactID := issue(t, "Intro.\n\nBody.\n")
			ok(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": "Body.", "with": eol("One\ntwo.")}},
			}, "alice"), "replace")
			return documentMarkdown(t, handler, artifactID)
		}},
		{"a replace in code", func(t *testing.T, eol func(string) string) string {
			_, artifactID := issue(t, "Intro.\n\n```\nfoo\n```\n")
			ok(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": "foo", "with": eol("a\nb")}},
			}, "alice"), "replace in code")
			return documentMarkdown(t, handler, artifactID)
		}},
		{"an accepted suggestion", func(t *testing.T, eol func(string) string) string {
			key, artifactID := issue(t, "Intro.\n\nBody.\n")
			created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
				"body": "suggest", "anchor": map[string]any{"artifact": "spec", "quote": "Body."},
				"suggestion": map[string]string{"replace_with": eol("One\ntwo.")}, "actor": sessionActor(),
			})
			ok(t, created, "create suggestion")
			comment := decodeBody[model.Comment](t, created)
			ok(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice"), "accept")
			return documentMarkdown(t, handler, artifactID)
		}},
		{"an insert of rows into a table", func(t *testing.T, eol func(string) string) string {
			_, artifactID := issue(t, "| h |\n| :- |\n| old |\n")
			ok(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "insert", "after": "old", "markdown": eol("| a |\n| b |\n")}},
			}, "alice"), "insert rows")
			return documentMarkdown(t, handler, artifactID)
		}},
		{"a retype's attributes", func(t *testing.T, eol func(string) string) string {
			_, artifactID := issue(t, "Question here\n")
			blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice"))
			ok(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "retype", "block": blocks[0].ID, "type": "callout", "attributes": map[string]any{"kind": "note", "title": eol("One\ntwo")}}},
			}, "alice"), "retype")
			return strings.ReplaceAll(documentMarkdown(t, handler, artifactID), blocks[0].ID, "block")
		}},
		{"an answer written into its ask block", func(t *testing.T, eol func(string) string) string {
			key, artifactID := issue(t, "Context\n")
			blockID := "decision-" + strings.ToLower(key)
			ok(t, sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
				"actor": sessionActor(),
				"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": strings.Replace(transportAsk, "#decision", "#"+blockID, 1)}},
			}), "write ask block")
			awaitIndexedAskBlock(t, handler, artifactID, blockID, "Which transport?")
			ok(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+blockAskID(t, handler, key, blockID)+"/answer", map[string]any{
				"selected": []string{"REST"}, "text": eol("REST\nfirst."), "expected_edited_at": nil,
			}, "alice"), "answer")
			markdown := strings.ReplaceAll(documentMarkdown(t, handler, artifactID), blockID, "decision")
			return answeredAt.ReplaceAllString(markdown, `answered_at="…"`)
		}},
		{"a block ask's edited text", func(t *testing.T, eol func(string) string) string {
			key, artifactID := issue(t, "Context\n")
			// Each issue's ask block has its own id, since the inbox lists them all.
			blockID := "decision-" + strings.ToLower(key)
			ok(t, sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
				"actor": sessionActor(),
				"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": strings.Replace(transportAsk, "#decision", "#"+blockID, 1)}},
			}), "write ask block")
			awaitIndexedAskBlock(t, handler, artifactID, blockID, "Which transport?")
			ok(t, sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+blockAskID(t, handler, key, blockID), map[string]any{
				"question": eol("Which transport\nships first?\n\nPick one."),
				"options":  []map[string]string{{"label": "REST", "description": eol("Ship\nnow")}, {"label": "gRPC"}},
				"actor":    sessionActor(),
			}), "edit ask")
			return strings.ReplaceAll(documentMarkdown(t, handler, artifactID), blockID, "decision")
		}},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			lineFeeds := entry.write(t, func(text string) string { return text })
			for _, ending := range []struct{ name, eol string }{{"CR LF", "\r\n"}, {"a lone carriage return", "\r"}} {
				stored := entry.write(t, func(text string) string { return strings.ReplaceAll(text, "\n", ending.eol) })
				if stored != lineFeeds {
					t.Fatalf("with %s the document stores %q, want %q, what the same text with line feeds stores", ending.name, stored, lineFeeds)
				}
			}
			if strings.Contains(lineFeeds, "\r") {
				t.Fatalf("the document stores a carriage return: %q", lineFeeds)
			}
		})
	}
}

// answeredAt is an answered ask block's timestamp attribute, which differs between writes.
var answeredAt = regexp.MustCompile(`answered_at="[^"]*"`)
