package itemblocks

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

// live is a stored paragraph at position, holding text.
func live(id, text string, position int) Stored {
	return Stored{ID: id, Type: "paragraph", Hash: Hash(text), Position: position}
}

// removed is a stored paragraph removed while it held text.
func removed(id, text string) Stored {
	return Stored{ID: id, Type: "paragraph", Hash: Hash(text), Position: -1}
}

func paragraphs(texts ...string) []Block {
	blocks := make([]Block, len(texts))
	for index, text := range texts {
		blocks[index] = Block{Type: "paragraph", Text: text, Hash: Hash(text)}
	}
	return blocks
}

// storedAfter is what an item holds once ids name next's blocks: each block of next live at its
// position, and every other block of stored removed with the hash it last held.
func storedAfter(stored []Stored, next []Block, ids []string) []Stored {
	var out []Stored
	for index, block := range next {
		out = append(out, Stored{ID: ids[index], Type: block.Type, Hash: Hash(block.Text), Position: index})
	}
	for _, block := range stored {
		if !slices.Contains(ids, block.ID) {
			block.Position = -1
			out = append(out, block)
		}
	}
	return out
}

func TestAlign(t *testing.T) {
	abc := []Stored{live("b1", "a", 0), live("b2", "b", 1), live("b3", "c", 2)}
	heading := paragraphs("a", "B", "c")
	heading[1].Type = "heading"
	cases := []struct {
		name        string
		stored      []Stored
		next        []Block
		counter     int
		want        []string
		wantCounter int
	}{
		{"unchanged", abc, paragraphs("a", "b", "c"), 4, []string{"b1", "b2", "b3"}, 4},
		{"moved", abc, paragraphs("c", "a", "b"), 4, []string{"b3", "b1", "b2"}, 4},
		{"reworded between kept blocks, same type", abc, paragraphs("a", "B", "c"), 4, []string{"b1", "b2", "b3"}, 4},
		{"retyped between kept blocks", abc, heading, 4, []string{"b1", "b4", "b3"}, 5},
		{"inserted", []Stored{live("b1", "a", 0), live("b3", "c", 1)}, paragraphs("a", "x", "c"), 4, []string{"b1", "b4", "b3"}, 5},
		{"restored", []Stored{live("b1", "a", 0), removed("b2", "b"), live("b3", "c", 1)}, paragraphs("a", "b", "c"), 4, []string{"b1", "b2", "b3"}, 4},
		{"two rewordings in order", []Stored{live("b1", "a", 0), live("b2", "b", 1), live("b3", "c", 2), live("b4", "d", 3)}, paragraphs("a", "B", "C", "d"), 5, []string{"b1", "b2", "b3", "b4"}, 5},
		{"a number is never reused", []Stored{live("b1", "a", 0), removed("b2", "b"), live("b3", "c", 1)}, paragraphs("a", "y", "c"), 4, []string{"b1", "b4", "b3"}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, counter := Align(tc.stored, tc.next, tc.counter)
			if !reflect.DeepEqual(got, tc.want) || counter != tc.wantCounter {
				t.Errorf("Align = %v, counter %d; want %v, counter %d", got, counter, tc.want, tc.wantCounter)
			}
		})
	}
}

// A body the parser refuses splits at blank lines, and the blocks whose text is unchanged keep
// their ids by text. A loose list splits into one piece per item in the fallback: the pieces take
// new ids and the list's is removed, and an edit back to the parsed body restores it by rule 2.
func TestAlignKeepsIdsAcrossASwitchToTheFallback(t *testing.T) {
	align := func(stored []Stored, body string, counter int) ([]Stored, []string, int) {
		t.Helper()
		next := Split(body)
		ids, counter := Align(stored, next, counter)
		return storedAfter(stored, next, ids), ids, counter
	}
	assertIDs := func(got []string, want ...string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
	}

	stored, ids, counter := align(nil, "First.\n\n- a\n- b\n\nLast.", 1)
	assertIDs(ids, "b1", "b2", "b3")
	next := Split("First.\n\n- a\n- b\n\n<div>x</div>\n\nLast.")
	if next[1].Type != "text" {
		t.Fatalf("Split read the body with block HTML as %+v, want the fallback", next)
	}
	ids, _ = Align(stored, next, counter)
	assertIDs(ids, "b1", "b2", "b4", "b3")

	stored, ids, counter = align(nil, "First.\n\n- a\n\n- b\n\nLast.", 1)
	assertIDs(ids, "b1", "b2", "b3")
	stored, ids, counter = align(stored, "First.\n\n- a\n\n- b\n\n<div>x</div>\n\nLast.", counter)
	assertIDs(ids, "b1", "b4", "b5", "b6", "b3")
	_, ids, counter = align(stored, "First.\n\n- a\n\n- b\n\nLast.", counter)
	assertIDs(ids, "b1", "b2", "b3")
	if counter != 7 {
		t.Errorf("counter = %d, want 7", counter)
	}
}

func TestCarry(t *testing.T) {
	// carry splits body, carries each id at the offset of its text in body, and aligns.
	carry := func(stored []Stored, body string, counter int, spans ...Carried) ([]string, int) {
		t.Helper()
		carried, err := ReadCarried(stored, body, spans)
		if err != nil {
			t.Fatal(err)
		}
		return Carry(stored, Split(body), carried, counter)
	}
	abc := []Stored{live("b1", "Alpha.", 0), live("b2", "Beta.", 1), live("b3", "Gamma.", 2)}
	cases := []struct {
		name        string
		stored      []Stored
		body        string
		spans       []Carried
		want        []string
		wantCounter int
	}{
		{
			"a reworded block keeps its id", abc, "Alpha.\n\nBeta, reworded.\n\nGamma.",
			[]Carried{{"b1", 0}, {"b2", 8}, {"b3", 25}}, []string{"b1", "b2", "b3"}, 4,
		},
		{
			"a split keeps the id on the first part and mints one for the second", abc, "Alpha.\n\nBe\n\nta.\n\nGamma.",
			[]Carried{{"b1", 0}, {"b2", 8}, {"b3", 17}}, []string{"b1", "b2", "b4", "b3"}, 5,
		},
		{
			"a join keeps the first", abc, "Alpha. Beta.\n\nGamma.",
			[]Carried{{"b1", 0}, {"b2", 7}, {"b3", 14}}, []string{"b1", "b3"}, 4,
		},
		{
			// Align would mint an id for the reworded block; the carried span keeps it.
			"a carried id whose block moved keeps it", abc, "Gamma moved.\n\nAlpha.\n\nBeta.",
			[]Carried{{"b3", 0}, {"b1", 14}, {"b2", 22}}, []string{"b3", "b1", "b2"}, 4,
		},
		{
			"an uncarried block equal to a removed block's last text takes that id back",
			[]Stored{live("b1", "Alpha.", 0), removed("b2", "Beta."), live("b3", "Gamma.", 1)}, "Alpha.\n\nBeta.\n\nGamma.",
			[]Carried{{"b1", 0}, {"b3", 15}}, []string{"b1", "b2", "b3"}, 4,
		},
		{
			"an uncarried new block takes the next id", abc, "Alpha.\n\nBeta.\n\nDelta.\n\nGamma.",
			[]Carried{{"b1", 0}, {"b2", 8}, {"b3", 23}}, []string{"b1", "b2", "b4", "b3"}, 5,
		},
		{
			"an uncarried block equal to a live block's text that nothing carried takes it, a move", abc, "Gamma.\n\nAlpha.",
			[]Carried{{"b1", 8}}, []string{"b3", "b1"}, 4,
		},
		{
			// A span starting in the blank lines after Beta. belongs to Beta.'s range, where b2
			// already stands: a join. Nothing takes b3 back by its text.
			"a span starting in the blank lines belongs to the block before them", abc, "Alpha.\n\nBeta.\n\nGamma.",
			[]Carried{{"b1", 0}, {"b2", 8}, {"b3", 13}}, []string{"b1", "b2", "b4"}, 5,
		},
		{
			"a span starting before the first block belongs to it", abc, "\n\nAlpha, reworded.",
			[]Carried{{"b1", 0}}, []string{"b1"}, 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, counter := carry(tc.stored, tc.body, 4, tc.spans...)
			if !reflect.DeepEqual(got, tc.want) || counter != tc.wantCounter {
				t.Errorf("Carry = %v, counter %d; want %v, counter %d", got, counter, tc.want, tc.wantCounter)
			}
		})
	}
}

func TestReadCarriedNamesTheBadField(t *testing.T) {
	stored := []Stored{live("b1", "Alpha.", 0), live("b2", "Beta.", 1), removed("b3", "Gamma.")}
	body := "😀 Alpha.\n\nBeta."
	got, err := ReadCarried(stored, body, []Carried{{"b1", 0}, {"b2", 11}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Carried{{"b1", 0}, {"b2", 13}}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReadCarried = %v, want %v (the emoji is two UTF-16 units and four bytes)", got, want)
	}
	for _, tc := range []struct {
		name    string
		carried []Carried
		field   string
	}{
		{"unknown id", []Carried{{"b9", 0}}, "blocks[0].id"},
		{"removed id", []Carried{{"b1", 0}, {"b3", 11}}, "blocks[1].id"},
		{"id twice", []Carried{{"b1", 0}, {"b1", 11}}, "blocks[1].id"},
		{"from outside the body", []Carried{{"b1", 99}}, "blocks[0].from"},
		{"from below zero", []Carried{{"b1", -1}}, "blocks[0].from"},
		{"from inside a character", []Carried{{"b1", 1}}, "blocks[0].from"},
		{"from out of order", []Carried{{"b1", 11}, {"b2", 0}}, "blocks[1].from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadCarried(stored, body, tc.carried)
			var carried *CarriedError
			if !errors.As(err, &carried) || carried.Field != tc.field {
				t.Errorf("ReadCarried error = %v, want one naming %s", err, tc.field)
			}
		})
	}
	if _, err := ReadCarried(stored, body, []Carried{{"b1", 11}, {"b2", 11}}); err != nil {
		t.Errorf("two spans at one offset, a join, refused: %v", err)
	}
}
