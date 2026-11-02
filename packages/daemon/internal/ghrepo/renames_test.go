package ghrepo

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

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
	renames, err := NewRenames([]Rename{MustRename("sjawhar/legion", "cybermonklabs/legion"), MustRename("acme/widgets", "acme/gadgets")})
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
	want := []Rename{MustRename("sjawhar/legion", "cybermonklabs/legion"), MustRename("acme/widgets", "acme/gadgets")}
	if pairs := renames.Pairs(); !slices.Equal(pairs, want) {
		t.Errorf("Pairs() = %v, want the configured order %v", pairs, want)
	}
	// Pairs hands out a copy: changing it changes no answer.
	renames.Pairs()[0] = MustRename("other/one", "other/two")
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
		{"to itself", []Rename{MustRename("sjawhar/legion", "sjawhar/legion")},
			"rename 0 (sjawhar/legion -> sjawhar/legion) renames a repository to itself: from and to name two repositories, compared without case"},
		{"to itself in another case", []Rename{MustRename("acme/widgets", "acme/Widgets")},
			"rename 0 (acme/widgets -> acme/Widgets) renames a repository to itself: from and to name two repositories, compared without case"},
		{"one from twice", []Rename{MustRename("acme/widgets", "acme/gadgets"), MustRename("ACME/Widgets", "other/one")},
			"rename 1 (ACME/Widgets -> other/one) and rename 0 (acme/widgets -> acme/gadgets) both rename ACME/Widgets: a repository moved to one name"},
		{"one to twice", []Rename{MustRename("acme/widgets", "acme/gadgets"), MustRename("other/one", "ACME/Gadgets")},
			"rename 1 (other/one -> ACME/Gadgets) and rename 0 (acme/widgets -> acme/gadgets) both name ACME/Gadgets as the repository moved to: a repository moved from one name"},
		{"a chain forwards", []Rename{MustRename("a/b", "c/d"), MustRename("C/D", "e/f")},
			"rename 1 (C/D -> e/f) chains onto rename 0 (a/b -> c/d) through C/D: a rename pairs two names, never a chain of three"},
		{"a chain backwards", []Rename{MustRename("c/d", "e/f"), MustRename("a/b", "C/D")},
			"rename 1 (a/b -> C/D) chains onto rename 0 (c/d -> e/f) through C/D: a rename pairs two names, never a chain of three"},
		{"no from", []Rename{{To: MustParse("acme/widgets")}},
			"rename 0 names no repository in from: a rename is the repository as it was and the one it moved to"},
		{"no to", []Rename{MustRename("a/b", "c/d"), {From: MustParse("acme/widgets")}},
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
	renames, err := NewRenames([]Rename{MustRename("sjawhar/legion", "cybermonklabs/legion"), MustRename("acme/widgets", "acme/gadgets")})
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

// ownedBy is the owner predicate OwnerAliases and Serving take, over the owners a project serves
// repositories of, compared without case as GitHub compares owners.
func ownedBy(owners ...string) func(string) bool {
	return func(owner string) bool {
		return slices.ContainsFunc(owners, func(o string) bool { return strings.EqualFold(o, owner) })
	}
}

// The zero value — a deployment that configures no rename — is no alias of any owner, and the
// repository GitHub serves a recorded one under is that recorded one. Neither answer asks the
// owner predicate anything, since there is no pair to ask about.
func TestZeroRenamesAliasNoOwnerAndServeTheRecordedRepository(t *testing.T) {
	var renames Renames
	asked := func(string) bool {
		t.Error("the zero Renames asked whether an owner is the project's")
		return true
	}
	aliases, conflicts := renames.OwnerAliases(asked)
	if len(aliases) != 0 || len(conflicts) != 0 {
		t.Errorf("zero Renames.OwnerAliases = %v, %v, want neither an alias nor a conflict", aliases, conflicts)
	}
	for _, recorded := range []string{"sjawhar/legion", "cybermonklabs/legion", "acme/widgets"} {
		if got := renames.Serving(MustParse(recorded), asked); got.String() != recorded {
			t.Errorf("zero Renames.Serving(%s) = %s, want %s unchanged", recorded, got, recorded)
		}
	}
}

// A configured rename whose `to` owner the project owns and whose `from` owner it does not is one
// alias: the owner the repository moved from is served by the owner it moved to, and a repository
// recorded under the name it moved from is held by GitHub under the one it moved to. Every other
// shape is no alias — a rename inside one owner, one whose `to` owner the project does not own,
// and one between two owners of the project, which is a conflict and leaves each owner its own —
// and in each of those the recorded repository is the one GitHub serves. The repository a pair
// moved to is served under its own name, whichever side a name is asked from, and a repository of
// an alias owner that no pair names is its own too.
func TestOwnerAliasesAndServingOverAConfiguredRename(t *testing.T) {
	transfer := MustRename("sjawhar/legion", "CyberMonkLabs/legion")
	for _, tc := range []struct {
		name      string
		pairs     []Rename
		owners    []string
		aliases   map[string]string
		conflicts []Rename
		serving   map[string]string
	}{
		{
			name:  "the owner it moved to is the project's and the one it moved from is not",
			pairs: []Rename{transfer}, owners: []string{"CyberMonkLabs", "acme"},
			aliases: map[string]string{"sjawhar": "cybermonklabs"},
			serving: map[string]string{
				"sjawhar/legion":       "CyberMonkLabs/legion",
				"SJAWHAR/Legion":       "CyberMonkLabs/legion",
				"CyberMonkLabs/legion": "CyberMonkLabs/legion",
				"cybermonklabs/legion": "cybermonklabs/legion",
				"sjawhar/legion-smoke": "sjawhar/legion-smoke",
				"acme/widgets":         "acme/widgets",
			},
		},
		{
			name:  "the owner it moved to is not the project's",
			pairs: []Rename{transfer}, owners: []string{"sjawhar", "acme"},
			serving: map[string]string{"sjawhar/legion": "sjawhar/legion", "CyberMonkLabs/legion": "CyberMonkLabs/legion"},
		},
		{
			name:  "both owners are the project's",
			pairs: []Rename{transfer}, owners: []string{"sjawhar", "cybermonklabs"},
			conflicts: []Rename{transfer},
			serving:   map[string]string{"sjawhar/legion": "sjawhar/legion", "CyberMonkLabs/legion": "CyberMonkLabs/legion"},
		},
		{
			name:  "a rename inside one owner",
			pairs: []Rename{MustRename("acme/widgets", "ACME/gadgets")}, owners: []string{"acme"},
			serving: map[string]string{"acme/widgets": "acme/widgets", "ACME/gadgets": "ACME/gadgets"},
		},
		{
			name:  "one owner moved two repositories, one of them to an owner outside the project",
			pairs: []Rename{transfer, MustRename("sjawhar/tools", "outside/tools")}, owners: []string{"cybermonklabs"},
			aliases: map[string]string{"sjawhar": "cybermonklabs"},
			serving: map[string]string{"sjawhar/legion": "CyberMonkLabs/legion", "sjawhar/tools": "sjawhar/tools"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			renames, err := NewRenames(tc.pairs)
			if err != nil {
				t.Fatalf("NewRenames: %v", err)
			}
			owned := ownedBy(tc.owners...)
			aliases, conflicts := renames.OwnerAliases(owned)
			if !maps.Equal(aliases, tc.aliases) {
				t.Errorf("OwnerAliases over %q = %v, want %v", tc.owners, aliases, tc.aliases)
			}
			if !slices.Equal(conflicts, tc.conflicts) {
				t.Errorf("OwnerAliases' conflicts over %q = %v, want %v", tc.owners, conflicts, tc.conflicts)
			}
			for recorded, want := range tc.serving {
				if got := renames.Serving(MustParse(recorded), owned); got.String() != want {
					t.Errorf("Serving(%s) over %q = %s, want %s", recorded, tc.owners, got, want)
				}
			}
		})
	}
}
