package text

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// mapResolver is a Resolver over fixed sets, as the shared text table's rows name them: the issue
// keys that exist, the question map of the text's document, and the whole id of every ask,
// comment, message and block a short id can name.
type mapResolver struct {
	issues    map[string]bool
	questions map[string]Ref
	items     []string
}

func (r mapResolver) Issues(keys []string) (map[string]bool, error) {
	found := map[string]bool{}
	for _, key := range keys {
		if r.issues[key] {
			found[key] = true
		}
	}
	return found, nil
}

func (r mapResolver) Questions() (map[string]Ref, error) {
	return r.questions, nil
}

// Items matches each short id against the whole ids, without case, as the database resolver
// matches it against the ids of the issue or document the reference names; a short id two ids or
// more start with is left out, as one no id starts with is.
func (r mapResolver) Items(prefixes []Prefix) (map[int]string, error) {
	found := map[int]string{}
	for index, prefix := range prefixes {
		short := prefix.Ref.ID
		if prefix.Block {
			short = prefix.Ref.Block
		}
		match, count := "", 0
		for _, id := range r.items {
			if len(id) >= len(short) && strings.EqualFold(id[:len(short)], short) {
				match, count = id, count+1
			}
		}
		if count == 1 {
			found[index] = match
		}
	}
	return found, nil
}

// countingResolver records each call, so a test can hold Mentions to one batch per kind.
type countingResolver struct {
	mapResolver
	issueCalls    [][]string
	questionCalls int
	itemCalls     [][]Prefix
	err           error
}

func (r *countingResolver) Issues(keys []string) (map[string]bool, error) {
	r.issueCalls = append(r.issueCalls, keys)
	if r.err != nil {
		return nil, r.err
	}
	return r.mapResolver.Issues(keys)
}

func (r *countingResolver) Questions() (map[string]Ref, error) {
	r.questionCalls++
	if r.err != nil {
		return nil, r.err
	}
	return r.mapResolver.Questions()
}

func (r *countingResolver) Items(prefixes []Prefix) (map[int]string, error) {
	r.itemCalls = append(r.itemCalls, prefixes)
	if r.err != nil {
		return nil, r.err
	}
	return r.mapResolver.Items(prefixes)
}

var (
	core1  = Ref{Kind: "issue", IssueKey: "CORE-1", ID: "CORE-1"}
	acme12 = Ref{Kind: "issue", IssueKey: "ACME-12", ID: "ACME-12"}
	xcore1 = Ref{Kind: "issue", IssueKey: "XCORE-1", ID: "XCORE-1"}

	q1  = Ref{Kind: "ask", IssueKey: "CORE-1", ID: "11111111-1111-4111-8111-111111111111"}
	q2  = Ref{Kind: "ask", IssueKey: "CORE-1", ID: "33333333-3333-4333-8333-333333333333"}
	q3  = Ref{Kind: "ask", IssueKey: "CORE-1", ID: "55555555-5555-4555-8555-555555555555"}
	q7b = Ref{Kind: "ask", IssueKey: "CORE-1", ID: "66666666-6666-4666-8666-666666666666"}

	// askID is the ask a short id names, and askTwin shares its first 8 characters with another
	// id, so no short id of it names one item.
	askID       = "2a7f1c90-1f4e-4a2b-9c3d-5e6f70819a2b"
	specBlockID = "5da04f23-4c71-4d5e-8f60-819a2b3c4d5e"
	askTwin     = "7e1a9b04-1111-4a11-8a11-111111111111"
	askOther    = "7e1a9b04-2222-4a22-8a22-222222222222"
)

// grammarResolver answers the table below: CORE-1, ACME-12 and XCORE-1 exist, the document
// declares q1, q2, q3 and `1`, one question block carries a uuid and two more share their first 8
// characters, and four items carry ids a reference can write short.
func grammarResolver() mapResolver {
	return mapResolver{
		issues: map[string]bool{"CORE-1": true, "ACME-12": true, "XCORE-1": true},
		questions: map[string]Ref{
			"q1":                                   q1,
			"q2":                                   q2,
			"q3":                                   q3,
			"1":                                    q1,
			"7b2c9d04-5e6f-4a71-8b92-c3d4e5f60718": q7b,
			"aa11bb22-1111-4a11-8a11-111111111111": q2,
			"aa11bb22-2222-4a22-8a22-222222222222": q3,
		},
		items: []string{askID, specBlockID, askTwin, askOther},
	}
}

func ask(id string) Ref {
	return Ref{Kind: "ask", IssueKey: "CORE-1", ID: id}
}

func specBlock(id string) Ref {
	return Ref{Kind: "block", IssueKey: "CORE-1", ID: "spec", Block: id}
}

// The grammar a text writes a reference bare or short by, row by row: what each body cites, and
// nothing else. The shared table holds the same rows for both readers
// (TestMentionsMatchesTheSharedTextTable); these name each rule on its own.
func TestMentionsReadsTheBareAndShortGrammar(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want []Ref
	}{
		{name: "key at the start", body: "CORE-1 is blocked.", want: []Ref{core1}},
		{name: "key after whitespace", body: "Blocked on CORE-1 today.", want: []Ref{core1}},
		{name: "key after a newline", body: "Blocked on\nCORE-1 today.", want: []Ref{core1}},
		{name: "key in parentheses", body: "Blocked (CORE-1) today.", want: []Ref{core1}},
		{name: "key in brackets", body: "Blocked [CORE-1] today.", want: []Ref{core1}},
		{name: "key in double quotes", body: `Blocked on "CORE-1" today.`, want: []Ref{core1}},
		{name: "key in single quotes", body: "Blocked on 'CORE-1' today.", want: []Ref{core1}},
		{name: "key in bold", body: "Blocked on **CORE-1** today.", want: []Ref{core1}},
		{name: "key in emphasis", body: "Blocked on _CORE-1_ today.", want: []Ref{core1}},
		{name: "key in a quote block", body: ">CORE-1 is blocked.", want: []Ref{core1}},
		{name: "key struck through", body: "Blocked on ~~CORE-1~~ today.", want: []Ref{core1}},
		{name: "key in a table cell", body: "|issue|state|\n|---|---|\n|CORE-1|open|", want: []Ref{core1}},
		{name: "key in a code span", body: "Blocked on `CORE-1` today.", want: []Ref{core1}},
		{name: "key in a code block", body: "```\nCORE-1 is blocked.\n```", want: []Ref{core1}},
		{name: "key after a space reference", body: "Blocked on&#32;CORE-1 today.", want: []Ref{core1}},
		{name: "key after a hex space reference", body: "Blocked on&#x20;CORE-1 today.", want: []Ref{core1}},
		{name: "key after a tab reference", body: "Blocked on&#9;CORE-1 today.", want: []Ref{core1}},
		{name: "key after a newline reference", body: "Blocked on&#10;CORE-1 today.", want: []Ref{core1}},
		{name: "a reference to another character starts nothing", body: "Blocked on&#33;CORE-1 today.", want: nil},
		{name: "a named entity starts nothing", body: "Blocked on&amp;CORE-1 today.", want: nil},
		{name: "a bare semicolon starts nothing", body: "Blocked on;CORE-1 today.", want: nil},
		{name: "a long key is a key", body: "See XCORE-1 here.", want: []Ref{xcore1}},
		{name: "two keys", body: "Both CORE-1 and ACME-12 are blocked.", want: []Ref{core1, acme12}},
		{name: "a repeated key is cited twice", body: "CORE-1 and CORE-1.", want: []Ref{core1, core1}},
		{name: "a key that names nothing", body: "Blocked on CORE-404.", want: nil},

		{name: "a path is no key", body: "See legion/CORE-1 here.", want: nil},
		{name: "a letter before the key", body: "See xCORE-1 here.", want: nil},
		{name: "a lowercase key is no key", body: "See core-1 here.", want: nil},
		{name: "letters after the number", body: "See CORE-1abc here.", want: nil},
		{name: "a hyphen after the number", body: "See CORE-1-2 here.", want: nil},
		{name: "a path after the key", body: "See CORE-1/spec here.", want: nil},
		{name: "a decimal after the key", body: "See CORE-1.5 here.", want: nil},
		{name: "an underscore after the key", body: "See CORE-1_x here.", want: nil},
		{name: "a fragment after the key", body: "See CORE-1#q1 here.", want: nil},
		{name: "an at sign after the key", body: "See CORE-1@v2 here.", want: nil},
		{name: "a key numbered zero", body: "See CORE-0 here.", want: nil},

		{name: "a period ends a key", body: "Done in CORE-1.", want: []Ref{core1}},
		{name: "a comma ends a key", body: "Blocked on CORE-1, then shipped.", want: []Ref{core1}},
		{name: "a colon ends a key", body: "Blocked on CORE-1: the rollout.", want: []Ref{core1}},
		{name: "a semicolon ends a key", body: "Blocked on CORE-1; shipped.", want: []Ref{core1}},
		{name: "an apostrophe ends a key", body: "CORE-1's spec is done.", want: []Ref{core1}},
		{name: "emphasis and a period end a key", body: "Blocked on _CORE-1_.", want: []Ref{core1}},
		{name: "a link's text ends a key", body: "See [the blocker](CORE-1).", want: []Ref{core1}},
		{name: "the end of the text ends a key", body: "Blocked on CORE-1", want: []Ref{core1}},

		{name: "a declared id", body: "See #q2 for the rollout.", want: []Ref{q2}},
		{name: "an id in emphasis", body: "As agreed, _see #q2_.", want: []Ref{q2}},
		{name: "an id before a period", body: "Answered in #q2.", want: []Ref{q2}},
		{name: "an id in a link", body: "See [the question](#q2).", want: []Ref{q2}},
		{name: "an id in a code span", body: "See `#q2` for the rollout.", want: []Ref{q2}},
		{name: "an escaped id", body: `See \#q2 and #q3 here.`, want: []Ref{q3}},
		{name: "a numeric reference to the hash", body: "See &#35;q2 here.", want: nil},
		{name: "a declaration is no mention", body: ":::ask{#q2}\nWhich region first?\n:::", want: nil},
		{name: "a repository issue is no id", body: "See owner/repo#1 here.", want: nil},
		{name: "a numbered id", body: "See #1 here.", want: []Ref{q1}},
		{name: "a short id of a question block", body: "See #7b2c9d04 for the rollout.", want: []Ref{q7b}},
		{name: "a short id two question blocks carry", body: "See #aa11bb22 here.", want: nil},
		{name: "a short id no question block carries", body: "See #deadbeef here.", want: nil},
		{name: "an id no question declares", body: "See #nope here.", want: nil},

		{
			name: "a short item id",
			body: "See dispatch://CORE-1/ask/2a7f1c90 here.",
			want: []Ref{ask(askID)},
		},
		{
			name: "a short item id in upper case",
			body: "See dispatch://CORE-1/ask/2A7F1C90 here.",
			want: []Ref{ask(askID)},
		},
		{
			name: "a longer short item id",
			body: "See dispatch://CORE-1/ask/2a7f1c90-1f4e here.",
			want: []Ref{ask(askID)},
		},
		{
			name: "a whole item id is never a short one",
			body: "See dispatch://CORE-1/ask/" + askID + " here.",
			want: []Ref{ask(askID)},
		},
		{
			name: "seven characters are too few",
			body: "See dispatch://CORE-1/ask/2a7f1c9 here.",
			want: []Ref{ask("2a7f1c9")},
		},
		{
			name: "a hyphen the id has none of",
			body: "See dispatch://CORE-1/ask/2a7f1c-90 here.",
			want: []Ref{ask("2a7f1c-90")},
		},
		{
			name: "a short item id two items carry",
			body: "See dispatch://CORE-1/ask/7e1a9b04 here.",
			want: []Ref{ask("7e1a9b04")},
		},
		{
			name: "a short item id no item carries",
			body: "See dispatch://CORE-1/ask/2a7f1c91 here.",
			want: []Ref{ask("2a7f1c91")},
		},
		{
			name: "a short block id",
			body: "See dispatch://CORE-1/spec#b-5da04f23 here.",
			want: []Ref{specBlock(specBlockID)},
		},
		{
			name: "a short block id no block carries",
			body: "See dispatch://CORE-1/spec#b-5da04f24 here.",
			want: []Ref{specBlock("5da04f24")},
		},
		{
			name: "a candidate inside a reference span",
			body: "See https://example.com/search?q=(CORE-1) and https://example.com/docs(#q2).",
			want: []Ref{
				{Kind: "url", ID: "https://example.com/search?q=(CORE-1)"},
				{Kind: "url", ID: "https://example.com/docs(#q2)"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := Mentions(test.body, "https://dispatch.test", grammarResolver())
			if err != nil {
				t.Fatalf("Mentions(%q) error: %v", test.body, err)
			}
			want := test.want
			if want == nil {
				want = []Ref{}
			}
			if refs := Refs(got); !reflect.DeepEqual(refs, want) {
				t.Errorf("Mentions(%q) cites %#v; want %#v", test.body, refs, want)
			}
		})
	}
}

// Every mention keeps the byte offset it is written at, a rewritten id included, and the
// references of one body come back in the order the body writes them.
func TestMentionsReportsEveryMentionAtItsOffsetInBodyOrder(t *testing.T) {
	body := "dispatch://CORE-1/ask/2a7f1c90 then CORE-1 and #q2, again CORE-1."
	got, err := Mentions(body, "https://dispatch.test", grammarResolver())
	if err != nil {
		t.Fatalf("Mentions() error: %v", err)
	}
	want := []Located{
		{Ref: ask(askID), Offset: 0},
		{Ref: core1, Offset: len("dispatch://CORE-1/ask/2a7f1c90 then ")},
		{Ref: q2, Offset: len("dispatch://CORE-1/ask/2a7f1c90 then CORE-1 and ")},
		{Ref: core1, Offset: len("dispatch://CORE-1/ask/2a7f1c90 then CORE-1 and #q2, again ")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Mentions() = %#v; want %#v", got, want)
	}
}

// A nil resolver reads nothing bare and resolves nothing short, so Mentions is ExtractAt.
func TestMentionsWithoutAResolverIsExtractAt(t *testing.T) {
	body := "CORE-1 and #q2 and dispatch://CORE-1/ask/2a7f1c90."
	got, err := Mentions(body, "https://dispatch.test", nil)
	if err != nil {
		t.Fatalf("Mentions() error: %v", err)
	}
	if want := ExtractAt(body, "https://dispatch.test"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Mentions() = %#v; want ExtractAt() = %#v", got, want)
	}
}

func TestMentionsAsksTheResolverOnceForEachKind(t *testing.T) {
	resolver := &countingResolver{mapResolver: grammarResolver()}
	body := "CORE-1, CORE-2 and CORE-1 again; #q1, #q2 and #q1_; " +
		"dispatch://CORE-1/ask/2a7f1c90 and dispatch://CORE-1/spec#b-5da04f23."
	got, err := Mentions(body, "https://dispatch.test", resolver)
	if err != nil {
		t.Fatalf("Mentions() error: %v", err)
	}
	if len(got) != 7 {
		t.Errorf("Mentions() = %#v; want CORE-1 twice, q1 twice, q2 and the two references", got)
	}
	if want := [][]string{{"CORE-1", "CORE-2"}}; !reflect.DeepEqual(resolver.issueCalls, want) {
		t.Errorf("Issues() calls = %v; want %v", resolver.issueCalls, want)
	}
	if resolver.questionCalls != 1 {
		t.Errorf("Questions() called %d times; want once", resolver.questionCalls)
	}
	want := [][]Prefix{{
		{Ref: ask("2a7f1c90")},
		{Ref: specBlock("5da04f23"), Block: true},
	}}
	if !reflect.DeepEqual(resolver.itemCalls, want) {
		t.Errorf("Items() calls = %#v; want %#v", resolver.itemCalls, want)
	}

	quiet := &countingResolver{mapResolver: grammarResolver()}
	for _, body := range []string{
		"Nothing bare: dispatch://CORE-1, # heading, owner/repo#1.",
		"A whole id: dispatch://CORE-1/ask/" + askID + ".",
		"",
	} {
		if _, err := Mentions(body, "https://dispatch.test", quiet); err != nil {
			t.Fatalf("Mentions(%q) error: %v", body, err)
		}
	}
	if len(quiet.issueCalls) != 0 || quiet.questionCalls != 0 || len(quiet.itemCalls) != 0 {
		t.Errorf("a body with no candidate asked the resolver: %v issue calls, %d question calls, %#v item calls",
			quiet.issueCalls, quiet.questionCalls, quiet.itemCalls)
	}
}

func TestMentionsReturnsTheResolversError(t *testing.T) {
	failure := errors.New("database gone")
	for _, body := range []string{
		"Blocked on CORE-1.",
		"See #q1.",
		"See dispatch://CORE-1/ask/2a7f1c90.",
	} {
		resolver := &countingResolver{mapResolver: grammarResolver(), err: failure}
		if _, err := Mentions(body, "https://dispatch.test", resolver); !errors.Is(err, failure) {
			t.Errorf("Mentions(%q) error = %v; want %v", body, err, failure)
		}
	}
}

// Stored markdown writes the whitespace at a mark's edge as a numeric character reference, so a
// reference to whitespace, decimal or hex, starts a key as the whitespace does; any other
// reference, or an entity that is not numeric, does not.
func TestMentionsReadsAWhitespaceReferenceAsWhitespace(t *testing.T) {
	resolver := mapResolver{issues: map[string]bool{"CORE-1": true}}
	for body, want := range map[string]int{
		"a&#32;CORE-1":       1,
		"a&#x20;CORE-1":      1,
		"a&#X20;CORE-1":      1,
		"a&#9;CORE-1":        1,
		"a&#10;CORE-1":       1,
		"a&#0000032;CORE-1":  1,
		"a&#x3000;CORE-1":    1,
		"a&#33;CORE-1":       0,
		"a&amp;CORE-1":       0,
		"a&#;CORE-1":         0,
		"a&#x;CORE-1":        0,
		"a&#00000032;CORE-1": 0,
		"a;CORE-1":           0,
		";CORE-1":            0,
	} {
		got, err := Mentions(body, "https://dispatch.test", resolver)
		if err != nil {
			t.Fatalf("Mentions(%q) error: %v", body, err)
		}
		if len(got) != want {
			t.Errorf("Mentions(%q) = %#v; want %d mentions", body, got, want)
		}
	}
}

// IsIDPrefix reads an id written short, and never a whole uuid: an exact id wins over a prefix, so
// a reference writing one asks a resolver nothing.
func TestIsIDPrefixReadsShortIDsAndNoWholeUUID(t *testing.T) {
	for value, want := range map[string]bool{
		"2a7f1c90":                             true,
		"2A7F1C90":                             true,
		"2a7f1c90-1f4e":                        true,
		"2a7f1c-90":                            true,
		"deadbeef":                             true,
		"2a7f1c9":                              false,
		"":                                     false,
		"q2":                                   false,
		"-2a7f1c90":                            false,
		"2a7f1c90_1f4e":                        false,
		"2a7f1g90":                             false,
		"2a7f1c90-1f4e-4a2b-9c3d-5e6f70819a2b": false,
		"2A7F1C90-1F4E-4A2B-9C3D-5E6F70819A2B": false,
		"2a7f1c901f4e4a2b9c3d5e6f70819a2b":     false,
		// A uuid one character short, and one whose hyphens sit elsewhere, are whole ids of
		// nothing, so both are read as a short id.
		"2a7f1c90-1f4e-4a2b-9c3d-5e6f70819a2":  true,
		"2a7f1c90-1f4e4-a2b-9c3d-5e6f70819a2b": true,
	} {
		if got := IsIDPrefix(value); got != want {
			t.Errorf("IsIDPrefix(%q) = %t; want %t", value, got, want)
		}
	}
}
