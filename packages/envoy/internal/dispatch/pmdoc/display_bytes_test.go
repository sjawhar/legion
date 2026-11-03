package pmdoc

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// displayTreeGrowthBound is how much larger an answer carrying the display trees may be than the
// same answer without them, on its wire bytes and on its gzip -9 bytes alike (INFRA-635's
// acceptance). The dashboard asks for the trees on every read, so this is what an issue page
// costs over what it cost before the trees existed.
const displayTreeGrowthBound = 3.0

// An answer of two hundred comments, and an activity feed of two hundred events, stay inside
// three times the bytes the same answer costs without the trees - on the wire and gzipped -9,
// which is the reading a browser gets once the responses are compressed.
//
// The corpus below is the shape the bound was measured on: an issue's thread of short replies,
// most of them carrying a short list. A list is where a tree's blocks come from, and so where
// its bytes come from: three list items are about 800 bytes of tree against 100 bytes of
// markdown. Run this with -v for the numbers.
func TestADisplayTreeAnswerStaysInsideThreeTimesItsBase(t *testing.T) {
	bodies := displayBoundBodies()
	for _, answer := range []struct {
		name       string
		build      func(bodies []string, trees bool) []byte
		base, head []byte
	}{
		{name: "an issue's comment list", build: commentListAnswer},
		{name: "an issue's activity feed", build: activityFeedAnswer},
	} {
		base, head := answer.build(bodies, false), answer.build(bodies, true)
		for _, reading := range []struct {
			name       string
			base, head int
		}{
			{"wire bytes", len(base), len(head)},
			{"gzip -9", gzipBytes(t, base), gzipBytes(t, head)},
		} {
			ratio := float64(reading.head) / float64(reading.base)
			t.Logf("%s, %s: base %d, head %d, ratio %.2f", answer.name, reading.name, reading.base, reading.head, ratio)
			if ratio > displayTreeGrowthBound {
				t.Errorf("%s, %s: head is %d bytes against the base's %d, %.2f times it, over the %.0f the acceptance allows",
					answer.name, reading.name, reading.head, reading.base, ratio, displayTreeGrowthBound)
			}
		}
	}
}

// gzipBytes is how many bytes answer compresses to at gzip -9, which is what a reader is sent.
func gzipBytes(t *testing.T, answer []byte) int {
	t.Helper()
	var out bytes.Buffer
	writer, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(answer); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Len()
}

// displayBoundBodies is two hundred comment bodies as an issue's thread holds them: short
// replies, each closing with a list, and each carrying the run detail that keeps a real thread
// from gzipping away to nothing.
func displayBoundBodies() []string {
	leads := []string{
		"Checked this against the staging stack and the numbers line up. I will run it once more after the next deploy.",
		"The failure is in the retry path, not the parser. Once the socket closes the loop keeps the old deadline.",
		"Agreed. Let's keep the bound as written and change the encoding instead; the reader does not need the ids.",
		"Rebased on main and the tests pass locally. Nothing else moved, and no other route changed with it.",
		"I read the spec again. The second round trip is the part I would avoid: the page draws every comment.",
		"Done. See internal/dispatch/pmdoc/display.go for where the mint used to happen, and the comment above it.",
		"This reproduces on a cold cache only. Warm the cache with one read and the second is served from it.",
		"The postgres plan changed after the index landed, and the p99 fell from 340ms to 21ms on the same rows.",
		"Not a regression. The old build never drew the quote at all, so there was nothing to compare against.",
		"Two things worry me about widening this. The browser then owns a decoder, and every client pays for it.",
		"Confirmed on the pod. I pulled the answer with curl, piped it through gzip -9 and counted the bytes.",
		"Let's split this. The anchor work is independent of the encoding work and the reviewers are different.",
		"The schema already supplies that attribute, so sending it is overhead the reader writes over again.",
		"I would rather fix the cause than widen the bound. A random id on every read defeats compression.",
		"Sorry for the noise - the earlier run used the wrong fork point, so the base already carried half of it.",
		"Reviewed. One note: the comment above displayChildren still says the drawing runs before the padding.",
		"Shipped behind the header, so nothing changes for the command-line tool or for an agent client today.",
		"We should write down the exact rule for a plain text. One holding a bare URL is not plain, for one.",
		"The feed is the harder surface: one event carries the body and the tree, so the words go out twice.",
		"Fine by me. Please leave the fixture regeneration to the test that documents it rather than by hand.",
		"Blocked on the database URL. Once it is exported in the pod the api suite runs green again.",
		"Closing this as a duplicate of the encoding issue; everything useful is written down there.",
		"One more reading, taken after the attribute cut. The list is now inside the bound on both.",
	}
	items := []string{
		"the parser suite is green",
		"the differential reports zero",
		"the api suite passes",
		"the fixture was regenerated",
		"the feed follows the list down",
		"gzip -9 was taken on the pod",
		"no other route changed",
		"the cache key is unchanged",
	}
	bodies := make([]string, 200)
	for index := range bodies {
		body := strings.Builder{}
		fmt.Fprintf(&body, "%s (run %d, digest %08x)\n", leads[index%len(leads)], index+1, index*2654435761)
		for item := range 3 {
			fmt.Fprintf(&body, "\n- %s (%d)", items[(index+item)%len(items)], 1700+index*7+item)
		}
		body.WriteString("\n")
		bodies[index] = body.String()
	}
	return bodies
}

// commentRow is the JSON one row of `GET /api/v1/issues/{key}/comments` carries: model.Comment
// with api's commentRead over it, filled as a thread of plain replies fills it.
type commentRow struct {
	ID         string          `json:"id"`
	IssueKey   string          `json:"issue_key"`
	ArtifactID *string         `json:"artifact_id"`
	Author     map[string]any  `json:"author"`
	Body       string          `json:"body"`
	BodyTree   json.RawMessage `json:"body_tree,omitempty"`
	Anchor     *string         `json:"anchor"`
	ReplyTo    *string         `json:"reply_to"`
	AskID      *string         `json:"ask_id"`
	Turn       *string         `json:"turn"`
	Resolved   bool            `json:"resolved"`
	ResolvedBy *string         `json:"resolved_by"`
	ResolvedAt *string         `json:"resolved_at"`
	EditedAt   *string         `json:"edited_at"`
	Suggestion *string         `json:"suggestion"`
	CreatedAt  string          `json:"created_at"`
	Mentions   []any           `json:"mentions"`
	Deliveries []any           `json:"deliveries"`
	ReplyBlock *string         `json:"reply_block"`
}

// commentListAnswer is the comment list as it is written, with the display trees for a caller
// that sent `Dispatch-Trees: 1` and without them for every other caller.
func commentListAnswer(bodies []string, trees bool) []byte {
	rows := make([]commentRow, len(bodies))
	for index, body := range bodies {
		rows[index] = commentRow{
			ID:         fmt.Sprintf("3f1c%04x-9b2a-4c7d-8e55-%012d", index, index),
			IssueKey:   "INFRA-635",
			Author:     map[string]any{"kind": "agent", "id": "legion-worker", "login": "legion"},
			Body:       body,
			CreatedAt:  "2026-11-04T00:02:15.513067Z",
			Mentions:   []any{},
			Deliveries: []any{},
		}
		if trees {
			rows[index].BodyTree = DisplayJSON(body, DisplayLatest)
		}
	}
	answer, err := json.Marshal(rows)
	if err != nil {
		panic(err)
	}
	return answer
}

// feedRow is the JSON one event of an issue's activity feed carries, whose trees ride in the
// event's own `trees` map keyed by the payload field's path (api's eventDisplayTrees).
type feedRow struct {
	ID      string                     `json:"id"`
	Type    string                     `json:"type"`
	Actor   map[string]any             `json:"actor"`
	Payload map[string]any             `json:"payload"`
	Trees   map[string]json.RawMessage `json:"trees,omitempty"`
	At      string                     `json:"at"`
}

// activityFeedAnswer is the event log of the same thread, one comment.created event per body.
func activityFeedAnswer(bodies []string, trees bool) []byte {
	rows := make([]feedRow, len(bodies))
	for index, body := range bodies {
		rows[index] = feedRow{
			ID:    fmt.Sprintf("%d", 918000+index),
			Type:  "comment.created",
			Actor: map[string]any{"kind": "agent", "id": "legion-worker"},
			Payload: map[string]any{
				"comment_id": fmt.Sprintf("3f1c%04x-9b2a-4c7d-8e55-%012d", index, index),
				"issue_key":  "INFRA-635",
				"body":       body,
			},
			At: "2026-11-04T00:02:15.513067Z",
		}
		if trees {
			rows[index].Trees = map[string]json.RawMessage{"payload.body": DisplayJSON(body, DisplayLatest)}
		}
	}
	answer, err := json.Marshal(rows)
	if err != nil {
		panic(err)
	}
	return answer
}
