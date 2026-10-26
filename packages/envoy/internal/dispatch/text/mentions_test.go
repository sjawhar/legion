package text

import (
	"errors"
	"reflect"
	"testing"
)

// mapResolver is a Resolver over fixed sets, as the shared text table's rows name them.
type mapResolver struct {
	issues    map[string]bool
	questions map[string]Ref
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

// countingResolver records each call, so a test can hold Mentions to one batch per kind.
type countingResolver struct {
	mapResolver
	issueCalls    [][]string
	questionCalls int
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

var q1 = Ref{Kind: "ask", IssueKey: "CORE-1", ID: "33333333-3333-4333-8333-333333333333"}

func TestMentionsReportsEveryMentionAtItsOffsetInBodyOrder(t *testing.T) {
	body := "dispatch://CORE-2 then CORE-1 and #q1, again CORE-1."
	resolver := mapResolver{
		issues:    map[string]bool{"CORE-1": true, "CORE-2": true},
		questions: map[string]Ref{"q1": q1},
	}
	got, err := Mentions(body, "https://dispatch.test", resolver)
	if err != nil {
		t.Fatalf("Mentions() error: %v", err)
	}
	core1 := Ref{Kind: "issue", IssueKey: "CORE-1", ID: "CORE-1"}
	want := []Located{
		{Ref: Ref{Kind: "issue", IssueKey: "CORE-2", ID: "CORE-2"}, Offset: 0},
		{Ref: core1, Offset: 23},
		{Ref: q1, Offset: 34},
		{Ref: core1, Offset: 45},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Mentions() = %#v; want %#v", got, want)
	}
}

func TestMentionsAsksTheResolverOnceForEachKind(t *testing.T) {
	resolver := &countingResolver{mapResolver: mapResolver{
		issues:    map[string]bool{"CORE-1": true},
		questions: map[string]Ref{"q1": q1},
	}}
	body := "CORE-1, CORE-2 and CORE-1 again; #q1, #q2 and #q1_."
	got, err := Mentions(body, "https://dispatch.test", resolver)
	if err != nil {
		t.Fatalf("Mentions() error: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("Mentions() = %#v; want CORE-1 twice and q1 twice", got)
	}
	if want := [][]string{{"CORE-1", "CORE-2"}}; !reflect.DeepEqual(resolver.issueCalls, want) {
		t.Errorf("Issues() calls = %v; want %v", resolver.issueCalls, want)
	}
	if resolver.questionCalls != 1 {
		t.Errorf("Questions() called %d times; want once", resolver.questionCalls)
	}

	quiet := &countingResolver{}
	for _, body := range []string{"Nothing bare: dispatch://CORE-1, # heading, owner/repo#1.", ""} {
		if _, err := Mentions(body, "https://dispatch.test", quiet); err != nil {
			t.Fatalf("Mentions() error: %v", err)
		}
	}
	if len(quiet.issueCalls) != 0 || quiet.questionCalls != 0 {
		t.Errorf("a body with no candidate asked the resolver: %v issue calls, %d question calls",
			quiet.issueCalls, quiet.questionCalls)
	}
}

func TestMentionsReturnsTheResolversError(t *testing.T) {
	failure := errors.New("database gone")
	for _, body := range []string{"Blocked on CORE-1.", "See #q1."} {
		if _, err := Mentions(body, "https://dispatch.test", &countingResolver{err: failure}); !errors.Is(err, failure) {
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
