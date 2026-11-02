package ghrepo

import (
	"slices"
	"strings"
	"testing"
)

// rename is a pair written as a test fixture, each side one Parse takes.
func rename(from, to string) Rename { return Rename{From: MustParse(from), To: MustParse(to)} }

// The zero value — a deployment that configures no rename — answers exactly as strings.EqualFold
// does, holds no pair and knows no partner.
func TestZeroRenamesAnswerAsEqualFold(t *testing.T) {
	var renames Renames
	if !renames.IsZero() {
		t.Error("the zero Renames is not IsZero")
	}
	if pairs := renames.Pairs(); len(pairs) != 0 {
		t.Errorf("Pairs() = %v, want none", pairs)
	}
	for _, tc := range []struct{ a, b string }{
		{"sjawhar/legion", "sjawhar/legion"},
		{"sjawhar/legion", "SJAWHAR/Legion"},
		{"sjawhar/legion", "cybermonklabs/legion"},
		{"sjawhar/legion", "sjawhar/legion-smoke"},
		{"", ""},
		{"", "sjawhar/legion"},
		{"not a repository", "not a repository"},
		{"not a repository", "sjawhar/legion"},
	} {
		if got, want := renames.Same(tc.a, tc.b), strings.EqualFold(tc.a, tc.b); got != want {
			t.Errorf("zero Renames.Same(%q, %q) = %v, want strings.EqualFold's %v", tc.a, tc.b, got, want)
		}
	}
	if partner, found := renames.Partner("sjawhar/legion"); found {
		t.Errorf("zero Renames.Partner(sjawhar/legion) = %s, %v, want none", partner, found)
	}
}

// A configured pair is one repository in both directions and in any case; an unrelated name is
// neither side's, and a string that is no `<owner>/<name>` is equal to nothing but itself.
func TestConfiguredRenamesAreOneRepositoryBothWays(t *testing.T) {
	renames, err := NewRenames([]Rename{rename("sjawhar/legion", "cybermonklabs/legion"), rename("acme/widgets", "acme/gadgets")})
	if err != nil {
		t.Fatalf("NewRenames: %v", err)
	}
	if renames.IsZero() {
		t.Error("a configured Renames is IsZero")
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"sjawhar/legion", "cybermonklabs/legion", true},
		{"cybermonklabs/legion", "sjawhar/legion", true},
		{"SJAWHAR/LEGION", "CyberMonkLabs/Legion", true},
		{"sjawhar/legion", "SJAWHAR/legion", true},
		{"acme/widgets", "acme/gadgets", true},
		{"acme/gadgets", "acme/widgets", true},
		{"sjawhar/legion", "acme/widgets", false},
		{"sjawhar/legion", "sjawhar/legion-smoke", false},
		{"cybermonklabs/legion", "acme/gadgets", false},
		{"other/repo", "sjawhar/legion", false},
		{"not a repository", "sjawhar/legion", false},
		{"not a repository", "not a repository", true},
	} {
		if got := renames.Same(tc.a, tc.b); got != tc.want {
			t.Errorf("Same(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	for _, tc := range []struct{ name, want string }{
		{"sjawhar/legion", "cybermonklabs/legion"},
		{"cybermonklabs/legion", "sjawhar/legion"},
		{"CyberMonkLabs/Legion", "sjawhar/legion"},
		{"acme/widgets", "acme/gadgets"},
	} {
		partner, found := renames.Partner(tc.name)
		if !found || partner.String() != tc.want {
			t.Errorf("Partner(%q) = %s, %v, want %s", tc.name, partner, found, tc.want)
		}
	}
	for _, unconfigured := range []string{"other/repo", "sjawhar/legion-smoke", "", "not a repository"} {
		if partner, found := renames.Partner(unconfigured); found {
			t.Errorf("Partner(%q) = %s, %v, want none", unconfigured, partner, found)
		}
	}
	want := []Rename{rename("sjawhar/legion", "cybermonklabs/legion"), rename("acme/widgets", "acme/gadgets")}
	if pairs := renames.Pairs(); !slices.Equal(pairs, want) {
		t.Errorf("Pairs() = %v, want the configured order %v", pairs, want)
	}
	// Pairs hands out a copy: changing it changes no answer.
	renames.Pairs()[0] = rename("other/one", "other/two")
	if !renames.Same("sjawhar/legion", "cybermonklabs/legion") {
		t.Error("changing the slice Pairs returned changed the equivalence")
	}
}

// Each refusal names the pair it cannot hold: a repository renamed to itself, one renamed twice
// (as a from or as a to), a chain of two pairs either way round, and a pair missing a side.
func TestNewRenamesRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pairs []Rename
		want  string
	}{
		{"to itself", []Rename{rename("sjawhar/legion", "sjawhar/legion")},
			"rename 0 (sjawhar/legion -> sjawhar/legion) renames a repository to itself: from and to name two repositories, compared without case"},
		{"to itself in another case", []Rename{rename("acme/widgets", "acme/Widgets")},
			"rename 0 (acme/widgets -> acme/Widgets) renames a repository to itself: from and to name two repositories, compared without case"},
		{"one from twice", []Rename{rename("acme/widgets", "acme/gadgets"), rename("ACME/Widgets", "other/one")},
			"rename 1 (ACME/Widgets -> other/one) and rename 0 (acme/widgets -> acme/gadgets) both rename ACME/Widgets: a repository moved to one name"},
		{"one to twice", []Rename{rename("acme/widgets", "acme/gadgets"), rename("other/one", "ACME/Gadgets")},
			"rename 1 (other/one -> ACME/Gadgets) and rename 0 (acme/widgets -> acme/gadgets) both name ACME/Gadgets as the repository moved to: a repository moved from one name"},
		{"a chain forwards", []Rename{rename("a/b", "c/d"), rename("C/D", "e/f")},
			"rename 1 (C/D -> e/f) chains onto rename 0 (a/b -> c/d) through C/D: a rename pairs two names, never a chain of three"},
		{"a chain backwards", []Rename{rename("c/d", "e/f"), rename("a/b", "C/D")},
			"rename 1 (a/b -> C/D) chains onto rename 0 (c/d -> e/f) through C/D: a rename pairs two names, never a chain of three"},
		{"no from", []Rename{{To: MustParse("acme/widgets")}},
			"rename 0 names no repository in from: a rename is the repository as it was and the one it moved to"},
		{"no to", []Rename{rename("a/b", "c/d"), {From: MustParse("acme/widgets")}},
			"rename 1 names no repository in to: a rename is the repository as it was and the one it moved to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			renames, err := NewRenames(tc.pairs)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("NewRenames error = %v, want %q", err, tc.want)
			}
			if !renames.IsZero() {
				t.Errorf("a refused NewRenames answered %v, want the zero value", renames.Pairs())
			}
		})
	}
}

// NewRenames of no pair is the zero value, which an absent key settles to.
func TestNewRenamesOfNoneIsTheZeroValue(t *testing.T) {
	for _, pairs := range [][]Rename{nil, {}} {
		renames, err := NewRenames(pairs)
		if err != nil {
			t.Fatalf("NewRenames(%v): %v", pairs, err)
		}
		partner, found := renames.Partner("acme/widgets")
		if !renames.IsZero() || found || !partner.IsZero() {
			t.Errorf("NewRenames(%v) = %+v, want the zero value with no partner", pairs, renames)
		}
	}
}

// Lookups is the order a read keyed by repository tries: the name the fact carries first, then
// the other name of the pair that names it. A deployment that configures no rename, and a name no
// pair holds, answer that one name alone, so such a read stays the one read it was.
func TestLookupsAreTheNameThenItsPartner(t *testing.T) {
	var zero Renames
	for _, name := range []string{"sjawhar/legion", "cybermonklabs/legion", "", "not a repository"} {
		if got := zero.Lookups(name); !slices.Equal(got, []string{name}) {
			t.Errorf("zero Renames.Lookups(%q) = %q, want %q alone", name, got, name)
		}
	}
	renames, err := NewRenames([]Rename{rename("sjawhar/legion", "cybermonklabs/legion"), rename("acme/widgets", "acme/gadgets")})
	if err != nil {
		t.Fatalf("NewRenames: %v", err)
	}
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"sjawhar/legion", []string{"sjawhar/legion", "cybermonklabs/legion"}},
		{"cybermonklabs/legion", []string{"cybermonklabs/legion", "sjawhar/legion"}},
		{"SJAWHAR/Legion", []string{"SJAWHAR/Legion", "cybermonklabs/legion"}},
		{"acme/gadgets", []string{"acme/gadgets", "acme/widgets"}},
		{"other/repo", []string{"other/repo"}},
		{"sjawhar/legion-smoke", []string{"sjawhar/legion-smoke"}},
		{"", []string{""}},
		{"not a repository", []string{"not a repository"}},
	} {
		if got := renames.Lookups(tc.name); !slices.Equal(got, tc.want) {
			t.Errorf("Lookups(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
