package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// corpusRoom is one room at one commit position, as driveCorpusRooms hands it to a comparison:
// the service that holds it, the replica its update observer keeps, the corpus document's name,
// which is the room's, its live document, and stage, what the room and the position are, for a
// failure to name.
type corpusRoom struct {
	service *Service
	replica *renderedReplica
	room    string
	live    *crdt.Doc
	stage   string
}

// driveCorpusRooms drives one room per corpus document - every in-repo document, loaded as a room
// with the replica its update observer keeps (keepReplica) - and calls compare on each: before the
// room's first transaction, when the replica holds no state and the first read of the comparison
// takes its copy from the room itself and starts the replica from it (renderedReplica.atPoint),
// and after each of a series of browser edits, half of them classified by the update observer and
// half by the read that follows them, the two drainers of the room's log. It fails the test where
// the replica does not hold what the position says it holds, and returns how many comparisons it
// made and how many documents the corpus holds.
func driveCorpusRooms(t *testing.T, compare func(at corpusRoom)) (comparisons, documents int) {
	t.Helper()
	paths, err := filepath.Glob("../pmdoc/testdata/corpus/*.md")
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d documents)", err, len(paths))
	}
	service := &Service{}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		parsed, err := pmdoc.Parse(string(markdown))
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		update, err := encodeDocumentTree(parsed)
		if err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		live := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(live, update, nil); err != nil {
			t.Fatalf("load %s into a room: %v", name, err)
		}
		log := live.OpenUpdateLog()
		replica := service.keepReplica(live, log, nil)

		compare(corpusRoom{service, replica, name, live, name + ": a room with no transaction"})
		comparisons++
		if replica.doc == nil {
			t.Fatalf("%s: the replica of a room with no transaction holds no state after the comparison, so no read took its copy from the room and started it from that copy", name)
		}

		for index, edit := range []struct {
			what string
			edit func()
		}{
			{"a block appended", func() {
				editRoomDocument(t, live, appendBlocks(t, "A paragraph the read found.\n"))
			}},
			{"a table with marks appended", func() {
				editRoomDocument(t, live, appendBlocks(t, "| Column | Value |\n| --- | --- |\n| row | **cell** |\n"))
			}},
			{"a word typed", func() { typeIntoLastParagraph(t, live, "typed ") }},
			{"a character deleted", func() { deleteFromLastParagraph(t, live) }},
		} {
			edit.edit()
			if index%2 == 0 {
				replica.observe(name)
			}
			compare(corpusRoom{service, replica, name, live, name + ": " + edit.what})
			comparisons++
			if replica.doc == nil {
				t.Fatalf("%s: the replica holds no state after %s, so no read walked it", name, edit.what)
			}
		}
		log.Close()
	}
	return comparisons, len(paths)
}
