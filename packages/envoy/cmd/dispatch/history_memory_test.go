package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A document's stored history costs a cold read only the document it leaves, however much the
// writes before it deleted, and compaction stores only that document. Each history is one ordinary
// writes leave within every cap, as LEGION-496 measured them: five hundred replies of 2,000
// characters to one anchored comment, each of which projects the thread's whole margin record
// again; sixty-four 1 MB versions of one document; thirty-two suggestions carrying 900 KB of
// replace_with each. When a load merged every stored update, the replies left 257 MB stored and a
// cold one-word edit of their 3 KB document took 1,842 MiB above idle, past production's 1,024 MiB
// task. The suggestions are not history: an open suggestion keeps its replace_with in its margin
// record, so their 28.8 MB is the document itself, which compaction must keep whole, and what a
// cold read of it costs is the size of the live document, which no fold of the log bounds; their
// reads are logged rather than held to the bound. The writer is killed rather than stopped, so its
// shutdown compacts nothing and every read meets the log the writes left, as it meets one stored
// before LEGION-496; then a server that holds the room stops as a deploy stops it, which compacts
// it.
func TestAStoredHistoryCostsAColdReadOnlyTheDocumentItLeaves(t *testing.T) {
	memory := newMemoryHarness(t)
	words := make([]string, 0, 600)
	for index := range 600 {
		words = append(words, fmt.Sprintf("w%03d", index))
	}
	spec := "Before sentinel.\n\n" + strings.Join(words, " ") + "\n"
	for _, history := range []struct {
		name  string
		write func(t *testing.T, server *memoryServer) createdIssue
		// holds reports what the compacted document should still hold beyond its text.
		holds func(t *testing.T, marks map[string]any)
		// live is a history whose stored bytes are the live document's, so a cold read is held to
		// no bound here.
		live bool
	}{
		{
			name: "five hundred 2,000-character replies to one anchored comment",
			write: func(t *testing.T, server *memoryServer) createdIssue {
				issue := server.createIssue(t, "History of replies", spec)
				var root struct{ ID string }
				created := server.comment(t, issue.Key, map[string]any{"body": "root", "anchor": map[string]any{"artifact": "spec", "quote": words[0]}})
				if err := json.Unmarshal(created.body, &root); err != nil || root.ID == "" {
					t.Fatalf("decode the root comment: %v %.300s", err, created.body)
				}
				for index := range 500 {
					server.comment(t, issue.Key, map[string]any{"body": strings.Repeat("r", 1_996) + fmt.Sprintf("%04d", index), "reply_to": root.ID})
				}
				return issue
			},
			holds: func(t *testing.T, marks map[string]any) {
				for _, value := range marks {
					record, _ := value.(map[string]any)
					if replies, _ := record["replies"].([]any); len(replies) == 500 {
						return
					}
				}
				t.Fatalf("no margin record holds the thread's 500 replies: %d records", len(marks))
			},
		},
		{
			name: "sixty-four 1 MB versions of one document",
			write: func(t *testing.T, server *memoryServer) createdIssue {
				issue := server.createIssue(t, "History of versions", "Before sentinel.\n")
				for index := range 64 {
					markdown := "Before sentinel.\n\n" + strings.Repeat(fmt.Sprintf("v%03d ", index), 1_000_000/5)
					upload, err := server.tryUploadNamed("json", issue.Key, "spec.md", markdown[:1_000_000])
					if err != nil {
						t.Fatal(err)
					}
					if upload.status != http.StatusCreated {
						t.Fatalf("upload %d: status %d body %.300s", index, upload.status, upload.body)
					}
				}
				return issue
			},
			holds: func(*testing.T, map[string]any) {},
		},
		{
			name: "thirty-two suggestions of 900 KB",
			write: func(t *testing.T, server *memoryServer) createdIssue {
				issue := server.createIssue(t, "History of suggestions", spec)
				replacement := strings.Repeat("word ", 180_000)
				for index := range 32 {
					server.comment(t, issue.Key, map[string]any{
						"body": "s", "anchor": map[string]any{"artifact": "spec", "quote": words[index]},
						"suggestion": map[string]any{"replace_with": replacement},
					})
				}
				return issue
			},
			holds: func(t *testing.T, marks map[string]any) {
				suggestions := 0
				for _, value := range marks {
					record, _ := value.(map[string]any)
					if content, _ := record["content"].(string); len(content) == 900_000 {
						suggestions++
					}
				}
				if suggestions != 32 {
					t.Fatalf("%d margin records hold a suggestion's 900 KB replace_with, want 32", suggestions)
				}
			},
			live: true,
		},
	} {
		t.Run(history.name, func(t *testing.T) {
			writer := memory.start(t)
			issue := history.write(t, writer)
			artifactID := issue.PrimaryArtifactID
			memory.waitForSettlement(t, uploadResult{status: http.StatusCreated, artifactID: artifactID})
			writer.kill(t)
			t.Logf("stored before reading: %d bytes", memory.storedUpdateBytes(t, artifactID))

			for _, read := range []struct {
				name string
				run  func(t *testing.T, server *memoryServer)
			}{
				{"GET /text", func(t *testing.T, server *memoryServer) { server.text(t, artifactID) }},
				{"a websocket load", func(t *testing.T, server *memoryServer) { server.loadOverWebsocket(t, artifactID) }},
				{"a one-word edit", func(t *testing.T, server *memoryServer) {
					if answer := server.edit(t, artifactID, map[string]any{"op": "replace", "find": "sentinel", "with": "marker"}); answer.status != http.StatusOK {
						t.Fatalf("edit: status %d body %.300s", answer.status, answer.body)
					}
					memory.waitForSettlement(t, uploadResult{status: http.StatusCreated, artifactID: artifactID})
				}},
			} {
				cold := memory.start(t)
				peak := cold.peakAboveIdle(t, func() { read.run(t, cold) })
				cold.kill(t)
				t.Logf("a cold %s: %d MiB above idle", read.name, peak>>20)
				if !history.live && peak > requestMemoryBound {
					t.Errorf("a cold %s held %d MiB above idle, want at most %d MiB", read.name, peak>>20, requestMemoryBound>>20)
				}
			}

			holder := memory.start(t)
			holder.loadOverWebsocket(t, artifactID)
			text := holder.text(t, artifactID)
			if !strings.HasPrefix(text, "Before marker.") {
				t.Fatalf("the edited document reads %.80q, want it to open with the edit", text)
			}
			holder.stop(t)
			stored := memory.storedUpdateBytes(t, artifactID)
			state, marks := memory.foldedState(t, artifactID)
			t.Logf("stored after compaction: %d bytes; the document's state: %d bytes", stored, state)
			if bound := state + state/10 + 64<<10; stored > bound {
				t.Errorf("compaction left %d bytes stored, want about the document's %d-byte state, at most %d", stored, state, bound)
			}
			reader := memory.start(t)
			if got := reader.text(t, artifactID); got != text {
				t.Fatalf("the compacted document reads %.80q, want %.80q", got, text)
			}
			reader.kill(t)
			history.holds(t, marks)
		})
	}
}

// comment posts a comment on the issue as alice, and requires it stored.
func (p *memoryServer) comment(t *testing.T, issue string, body map[string]any) response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the comment: %v", err)
	}
	answer := p.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", "application/json", bytes.NewReader(encoded), http.Header{"X-Dispatch-User": {"alice"}})
	if answer.status != http.StatusCreated {
		t.Fatalf("comment: status %d body %.300s", answer.status, answer.body)
	}
	return answer
}

// kill ends the server at once, as a crash does: its shutdown runs nothing, so it compacts no room.
func (p *memoryServer) kill(t *testing.T) {
	t.Helper()
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.command.Process.Signal(syscall.SIGKILL)
	<-p.exited
}

// storedUpdateBytes is the size of the document's stored update log.
func (h *memoryHarness) storedUpdateBytes(t *testing.T, artifactID string) int {
	t.Helper()
	var stored int
	if err := h.database.Pool.QueryRow(context.Background(),
		`select coalesce(sum(octet_length(update)), 0) from doc_updates where artifact_id = $1`, artifactID,
	).Scan(&stored); err != nil {
		t.Fatalf("measure the stored update log: %v", err)
	}
	return stored
}

// foldedState applies the document's stored updates in order to one document and reports the size
// of that document's state and its margin records: what the stored log holds that a reader sees.
func (h *memoryHarness) foldedState(t *testing.T, artifactID string) (int, map[string]any) {
	t.Helper()
	rows, err := h.database.Pool.Query(context.Background(),
		`select update from doc_updates where artifact_id = $1 order by version`, artifactID)
	if err != nil {
		t.Fatalf("read the stored update log: %v", err)
	}
	defer rows.Close()
	doc := crdt.New()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			t.Fatalf("scan a stored update: %v", err)
		}
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatalf("apply a stored update: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored update log: %v", err)
	}
	return len(crdt.EncodeStateAsUpdateV1(doc, nil)), doc.GetMap("marks").Entries()
}
