package docs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A write may not leave a document's markdown past either of an upload's limits and bigger than it
// was by that measure: longer than 1 MiB and longer than before, or making more than 65,536
// elements and more than before. Text that makes next to no elements - prose, or a code block's
// lines - is held by its bytes; text that makes many by its elements. A document already past a
// limit can still be trimmed or rewritten as long, and front matter, which an upload's parse does
// not count, is not counted here either.
func TestAWriteMayNotGrowADocumentPastWhatOneUploadMayHold(t *testing.T) {
	prose := func(bytes int) string { return strings.Repeat("word ", bytes/5) + "\n" }
	code := func(bytes int) string {
		return "```\n" + strings.Repeat("a line of code, forty bytes long; more\n", bytes/40) + "```\n"
	}
	headings := func(count int) string { return strings.Repeat("# a\n", count) }
	underscores := func(units int) string { return strings.Repeat(")_", units) }
	frontMatter := "---\ntitle: a spec\n---\n\n"
	tooLong := func(before, after string) string {
		return fmt.Sprintf("a markdown document is at most 1 MiB (1048576 bytes), and this change would make the document's markdown %d bytes (it was %d)", len(after), len(before))
	}
	for _, test := range []struct {
		name, before, after string
		refusal             string
	}{
		{"900 KB of prose onto 900 KB", prose(900_000), prose(900_000) + "\n" + prose(900_000), ""},
		{"900 KB of code onto 900 KB", code(900_000), code(1_800_000), ""},
		{"a heading onto 16,384, which weigh the limit", headings(16_384), headings(16_384) + "\nMore.\n",
			"make 65540 elements, past the 65536 one document may hold (it made 65536)"},
		{"front matter and 16,384 headings, new", "", frontMatter + headings(16_384), "-"},
		{"front matter and 16,385 headings, new", "", frontMatter + headings(16_385),
			"make 65540 elements, past the 65536 one document may hold (it made 0)"},
		{"more )_ than the guard reads, onto )_ it read whole", "Before.\n", underscores(150_000),
			"make more than 65536 elements, past the 65536 one document may hold (it made 4)"},
		{"more )_ onto more than the guard reads", underscores(150_000), underscores(160_000),
			"make more than 65536 elements, past the 65536 one document may hold (it made more than 65536)"},
		{"less )_ than a document past the guard held", underscores(160_000), underscores(150_000), "-"},
		{"trimming prose past 1 MiB", prose(1_800_000), prose(1_700_000), "-"},
		{"rewriting prose past 1 MiB as long", prose(1_800_000), "w" + prose(1_800_000)[1:], "-"},
		{"trimming headings past the limit", headings(17_001), headings(17_000), "-"},
		{"a heading's text rewritten as heavy past the limit", headings(17_001), headings(17_000) + "# b\n", "-"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := test.refusal
			if want == "" {
				want = tooLong(test.before, test.after)
			}
			err := refuseGrowth(growth{before: test.before, after: test.after})
			if want == "-" {
				if err != nil {
					t.Fatalf("refused: %v, want it taken", err)
				}
				return
			}
			if !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "shorten the change, or split the document") {
				t.Fatalf("got %v, want ErrDocumentTooLarge saying %q and what to do", err, want)
			}
		})
	}
}

// A document's margin - the record of every comment and suggestion it has had, which a browser
// shows beside the document and every load builds - is weighed with the document. One record may
// hold 256 KiB of text and the margin 1 MiB; a write that leaves either past its bound and bigger
// than it was is refused, naming the bound and both sizes. The state the server keeps beside a
// record's text (a suggestion's status) is not text a caller writes, so an accept rewrites a full
// margin's record; and a margin already past its bound, as a peer can leave one, can be trimmed but
// not grown. Without the bound, thirty-two suggestions of 900 KB took one cold websocket load of
// their document to 296 MiB.
func TestAMarginMayNotGrowPastWhatADocumentMayHold(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Text.\n")
	alice := model.Actor{Kind: "user", ID: "alice"}
	// A suggestion's record holds its author's reference, "user:alice", and its replacement.
	suggestion := func(replacement int, replies ...MarkReply) MarkRecord {
		return MarkRecord{Kind: "replace", By: "user:alice", CreatedAt: "2026-10-03T00:00:00Z", Content: strings.Repeat("a", replacement), Status: "pending", Replies: replies}
	}
	refused := func(err error, want string) {
		t.Helper()
		if !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want ErrDocumentTooLarge saying %q", err, want)
		}
	}
	refused(joinedProjectMark(service, artifactID, "too-big", suggestion(300_000), alice),
		"holds at most 256 KiB (262144 bytes) of text, and this change would make one hold 300010 bytes (it held 0)")
	for index := range 4 {
		if err := joinedProjectMark(service, artifactID, fmt.Sprintf("s%d", index), suggestion(250_000), alice); err != nil {
			t.Fatalf("suggestion %d of four that fit the margin: %v", index+1, err)
		}
	}
	refused(joinedProjectMark(service, artifactID, "s4", suggestion(250_000), alice),
		"a document's margin holds at most 1 MiB (1048576 bytes) of comment and suggestion text, and this change would make it hold 1250050 bytes (it held 1000040)")
	accepted := suggestion(250_000)
	accepted.Status = "accepted"
	if err := joinedProjectMark(service, artifactID, "s0", accepted, alice); err != nil {
		t.Fatalf("accepting a suggestion in a margin that holds 1,000,040 bytes: %v", err)
	}
	refused(joinedProjectMark(service, artifactID, "s1", suggestion(250_000, MarkReply{By: "user:alice", Text: strings.Repeat("r", 20_000)}), alice),
		"holds at most 256 KiB (262144 bytes) of text, and this change would make one hold 270020 bytes (it held 250010)")

	// A peer's record takes the margin past its bound outside any server write.
	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		marks := doc.GetMap(marksMapName)
		transact(func(txn *crdt.Transaction) {
			marks.Set(txn, "peer", map[string]any{"text": strings.Repeat("p", 300_000)})
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := joinedProjectMark(service, artifactID, "s1", suggestion(100), alice); err != nil {
		t.Fatalf("trimming a suggestion in a margin past its bound: %v", err)
	}
	refused(joinedProjectMark(service, artifactID, "s2", suggestion(250_001), alice),
		"this change would make it hold 1050141 bytes (it held 1050140)")
}

// The same write to the same document gets the same answer. ygo loads a document's state writer
// by writer in order of their client ids, and a writer read before one whose items it builds on
// has all of its items parked until that writer is read; a load that parks more than ygo's
// pending-item cap fails. A transaction's writes take the id after every writer the document holds
// (writerAfter), so a new version of 16,384 headings, what one upload may hold, is read after the
// first version it replaces whichever id that version's writer drew - the lowest or the highest a
// browser can - and parks nothing. Under a random id it was read first whenever it drew an id
// below that writer's, about half the time, parked every item it wrote, and the load margin
// refused it: the same upload taken on one try and refused on the next.
func TestANewVersionIsTakenWhicheverIDItsFirstVersionsWriterDrew(t *testing.T) {
	service, _ := newTestService(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	headings := strings.Repeat("# a\n", 16_384)
	for _, first := range []crdt.ClientID{1, math.MaxUint32, 1, math.MaxUint32} {
		artifactID := createDocument(t, service.store, "One line.\n")
		seedByWriter(t, service, artifactID, first, "One line.\n")
		if _, err := joinedReplaceText(service, artifactID, headings, alice); err != nil {
			t.Fatalf("a new version of 16,384 headings over a first version written by client %d: %v", first, err)
		}
		loaded, err := service.persistence.Load(context.Background(), artifactID)
		if err != nil {
			t.Fatal(err)
		}
		if err := crdt.ApplyUpdateV1(crdt.New(), loaded.Update, nil); err != nil {
			t.Fatalf("a cold load of the version over client %d's: %v", first, err)
		}
	}
}

// seedByWriter writes artifactID's first live state, markdown's tree, as the writer client.
func seedByWriter(t *testing.T, service *Service, artifactID string, client crdt.ClientID, markdown string) {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New(crdt.WithClientID(client))
	fragment := doc.GetXmlFragment(fragmentName)
	doc.GetMap(marksMapName)
	if err := doc.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		t.Fatal(err)
	}
	if err := joinedWrite(service, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		_, err := service.persistence.AppendUpdateTx(ctx, tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil), true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
