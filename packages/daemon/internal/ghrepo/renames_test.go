package ghrepo

import (
	"errors"
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
// (as a from or as a to), a chain of two pairs either way round, a pair missing a side, and one
// owner's repositories moved to two owners, which would key one alias twice.
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
		{"one owner to two owners", []Rename{MustRename("a/x", "b/x"), MustRename("A/y", "c/y")},
			"rename 1 (A/y -> c/y) and rename 0 (a/x -> b/x) move repositories of A to two owners, c and b: an owner's repositories moved to one owner, since OwnerAliases keys an alias by A, the owner a pair moved a repository away from, and the second pair would overwrite the first silently"},
		{"one owner to two owners, one of them its own", []Rename{MustRename("a/x", "a/y"), MustRename("a/z", "c/z")},
			"rename 1 (a/z -> c/z) and rename 0 (a/x -> a/y) move repositories of a to two owners, c and a: an owner's repositories moved to one owner, since OwnerAliases keys an alias by a, the owner a pair moved a repository away from, and the second pair would overwrite the first silently"},
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

// Two pairs out of one owner to that one owner are the transfer of two repositories, and two
// owners moving to one owner key one alias each: neither is the shape the refusal above names, so
// both are held.
func TestNewRenamesHoldsSeveralPairsSharingAnOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pairs []Rename
	}{
		{"one owner's two repositories to one owner", []Rename{MustRename("sjawhar/legion", "cybermonklabs/legion"), MustRename("SJAWHAR/envoy", "cybermonklabs/envoy")}},
		{"two owners to one owner", []Rename{MustRename("sjawhar/legion", "cybermonklabs/legion"), MustRename("acme/widgets", "CyberMonkLabs/widgets")}},
		{"one owner's two repositories renamed inside it", []Rename{MustRename("acme/widgets", "acme/gadgets"), MustRename("acme/old", "ACME/new")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			renames, err := NewRenames(tc.pairs)
			if err != nil {
				t.Fatalf("NewRenames(%v) = %v, want both pairs held", tc.pairs, err)
			}
			if got := renames.Pairs(); !slices.Equal(got, tc.pairs) {
				t.Errorf("Pairs() = %v, want %v", got, tc.pairs)
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

// Under reads under the name the fact carries first and the pair's other name second: a
// deployment that configures no rename, and a name no pair holds, make the one read they always
// made; a configured name is read twice only when the first read found nothing; and a read that
// fails stops where it failed.
func TestUnderReadsTheNameThenItsPartner(t *testing.T) {
	var zero Renames
	renames := MustRenames(MustRename("sjawhar/legion", "cybermonklabs/legion"), MustRename("acme/widgets", "acme/gadgets"))
	// read answers the one name held, as a store holding one row does, and records every name it
	// was read under. Its *string is nil — the zero value Under reads on — for every other name.
	read := func(reads *[]string, held string) func(string) (*string, error) {
		return func(name string) (*string, error) {
			*reads = append(*reads, name)
			if held != "" && strings.EqualFold(name, held) {
				return &held, nil
			}
			return nil, nil
		}
	}
	for _, tc := range []struct {
		what       string
		renames    Renames
		name, held string
		want       []string
		found      bool
	}{
		{"a deployment that configures no rename reads the name alone", zero, "sjawhar/legion", "sjawhar/legion", []string{"sjawhar/legion"}, true},
		{"and reads it alone when it finds nothing", zero, "sjawhar/legion", "", []string{"sjawhar/legion"}, false},
		{"a name no pair holds is read alone", renames, "other/repo", "", []string{"other/repo"}, false},
		{"a configured name found at once is read once", renames, "cybermonklabs/legion", "cybermonklabs/legion", []string{"cybermonklabs/legion"}, true},
		{"one that finds nothing is read again under its partner", renames, "cybermonklabs/legion", "sjawhar/legion", []string{"cybermonklabs/legion", "sjawhar/legion"}, true},
		{"from either side, the name as the fact spells it", renames, "SJAWHAR/Legion", "cybermonklabs/legion", []string{"SJAWHAR/Legion", "cybermonklabs/legion"}, true},
		{"and the partner as the pair spells it", renames, "acme/gadgets", "", []string{"acme/gadgets", "acme/widgets"}, false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			var reads []string
			found, err := Under(tc.renames, tc.name, read(&reads, tc.held))
			if err != nil {
				t.Fatalf("Under: %v", err)
			}
			if !slices.Equal(reads, tc.want) {
				t.Errorf("Under read %q, want %q", reads, tc.want)
			}
			if (found != nil) != tc.found {
				t.Errorf("Under = %v, want found = %v", found, tc.found)
			}
		})
	}

	t.Run("a read that fails stops at the name it failed under", func(t *testing.T) {
		var reads []string
		refusal := errors.New("the database is down")
		found, err := Under(renames, "sjawhar/legion", func(name string) (*string, error) {
			reads = append(reads, name)
			return nil, refusal
		})
		if !errors.Is(err, refusal) || found != nil {
			t.Fatalf("Under = %v, %v; want the read's own error and nothing found", found, err)
		}
		if !slices.Equal(reads, []string{"sjawhar/legion"}) {
			t.Errorf("Under read %q after a failed read, want sjawhar/legion alone", reads)
		}
	})
}

// servedBy is the predicate OwnerAliases and Serving take, over the repositories a project
// serves, compared without case as GitHub compares names.
func servedBy(repositories ...string) func(Repository) bool {
	return func(repository Repository) bool {
		return slices.ContainsFunc(repositories, func(r string) bool { return strings.EqualFold(r, repository.String()) })
	}
}

// The zero value — a deployment that configures no rename — is no alias of any owner, and the
// repository GitHub serves a recorded one under is that recorded one. Neither answer asks the
// served-repository predicate anything, since there is no pair to ask about.
func TestZeroRenamesAliasNoOwnerAndServeTheRecordedRepository(t *testing.T) {
	var renames Renames
	asked := func(Repository) bool {
		t.Error("the zero Renames asked whether the project serves a repository")
		return true
	}
	if aliases := renames.OwnerAliases(asked); len(aliases) != 0 {
		t.Errorf("zero Renames.OwnerAliases = %v, want no alias", aliases)
	}
	for _, recorded := range []string{"sjawhar/legion", "cybermonklabs/legion", "acme/widgets"} {
		if got := renames.Serving(MustParse(recorded), asked); got.String() != recorded {
			t.Errorf("zero Renames.Serving(%s) = %s, want %s unchanged", recorded, got, recorded)
		}
	}
}

// Rule R over the table a configured rename answers: a recorded repository resolves to the other
// name of its pair exactly when the recorded name is the pair's from, the project serves the
// pair's to, and the project does not serve the pair's from; every other recorded name, the
// pair's to included, resolves to itself. OwnerAliases keys one alias for each pair that
// resolves, and none for a pair inside one owner, which moves nothing between installations.
//
// What the rule reads is the repositories the project serves, one at a time, so each case runs
// twice: as the project's mapping gives it, and with one more repository of the owner the pair
// moved the repository away from served beside it. Both must answer the same. The second is the
// state of the production failure this rule replaces: adding sjawhar/oh-my-pi to the project's
// mapping made sjawhar an owner of the project, an owner test read that as the pair's old owner
// being the project's own, and the pair stopped resolving while 47 trees could not push.
//
// The served set is all this rule reads, so the mapping states the issue names collapse here to
// four: M1, M2 and M4 all serve the pair's to alone, M3 serves its from, M5 serves both and M0
// serves neither. The daemon's own twelve cases drive those states through the mapping that
// builds those sets.
func TestOwnerAliasesAndServingOverAConfiguredRename(t *testing.T) {
	transfer := MustRename("sjawhar/legion", "CyberMonkLabs/legion")
	for _, tc := range []struct {
		name    string
		pairs   []Rename
		served  []string
		aliases map[string]string
		serving map[string]string
	}{
		{
			name:  "M1, M2 and M4: the project serves the repository the pair moved to and not the one it moved from",
			pairs: []Rename{transfer}, served: []string{"CyberMonkLabs/legion", "acme/widgets"},
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
			// The M3 row, which a symmetric rule gets wrong: the pair is configured before the
			// transfer, so the project still serves the repository under the name it will move
			// away from. A rule that answered from either side, as Partner does, would answer
			// the second line below with sjawhar/legion and address the repository the transfer
			// moves away from — this failure in reverse — while the daemon's own records of a
			// repository GitHub already holds under the pair's to need no resolution at all.
			name:  "M3: the project serves the repository the pair moved from, and not the one it moved to",
			pairs: []Rename{transfer}, served: []string{"sjawhar/legion", "acme/widgets"},
			serving: map[string]string{"sjawhar/legion": "sjawhar/legion", "CyberMonkLabs/legion": "CyberMonkLabs/legion"},
		},
		{
			// The M5 row, which "resolve whenever the to is served" gets wrong: somebody made a
			// repository at the old location, which deletes GitHub's redirect, so the two names
			// are two repositories and each is addressed under the name it was recorded with. A
			// rule that read the to alone would answer the first line below with
			// CyberMonkLabs/legion and address the wrong repository of the two.
			name:  "M5: the project serves both names, which GitHub then holds as two repositories",
			pairs: []Rename{transfer}, served: []string{"sjawhar/legion", "CyberMonkLabs/legion"},
			serving: map[string]string{"sjawhar/legion": "sjawhar/legion", "CyberMonkLabs/legion": "CyberMonkLabs/legion"},
		},
		{
			name:  "M0: the project serves neither name of the pair",
			pairs: []Rename{transfer}, served: []string{"CyberMonkLabs/other"},
			serving: map[string]string{"sjawhar/legion": "sjawhar/legion", "CyberMonkLabs/legion": "CyberMonkLabs/legion"},
		},
		{
			name:  "a rename inside one owner",
			pairs: []Rename{MustRename("acme/widgets", "ACME/gadgets")}, served: []string{"acme/widgets", "ACME/gadgets"},
			serving: map[string]string{"acme/widgets": "acme/widgets", "ACME/gadgets": "ACME/gadgets"},
		},
		{
			name:  "two owners moved a repository, one of them to an owner outside the project",
			pairs: []Rename{transfer, MustRename("beta/tools", "outside/tools")}, served: []string{"CyberMonkLabs/legion"},
			aliases: map[string]string{"sjawhar": "cybermonklabs"},
			serving: map[string]string{"sjawhar/legion": "CyberMonkLabs/legion", "beta/tools": "beta/tools"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			renames, err := NewRenames(tc.pairs)
			if err != nil {
				t.Fatalf("NewRenames: %v", err)
			}
			for _, state := range []struct {
				name   string
				served []string
			}{
				{name: "state A: the project serves no other repository of the owner the pair moved from", served: tc.served},
				{name: "state B: it serves one of that owner's repositories beside them", served: append(slices.Clone(tc.served), "sjawhar/oh-my-pi")},
			} {
				t.Run(state.name, func(t *testing.T) {
					served := servedBy(state.served...)
					if aliases := renames.OwnerAliases(served); !maps.Equal(aliases, tc.aliases) {
						t.Errorf("OwnerAliases over %q = %v, want %v", state.served, aliases, tc.aliases)
					}
					for recorded, want := range tc.serving {
						if got := renames.Serving(MustParse(recorded), served); got.String() != want {
							t.Errorf("Serving(%s) over %q = %s, want %s", recorded, state.served, got, want)
						}
					}
				})
			}
		})
	}
}

// MovedTo is the direction-aware half of the equivalence, the half Serving needs: a pair's from
// answers the repository it moved to, in whatever case either name is written, and every other
// name answers nothing — the pair's to among them, which is the name GitHub already holds and so
// the one no resolution is owed. Partner cannot stand in for it, and the two lines below that
// compare them say why: Partner answers from either side, which cannot tell a name recorded
// before a transfer from the one GitHub has now, so a Serving built on it would answer a record
// of the pair's to with the name the transfer moved the repository away from.
func TestMovedToAnswersTheToOfTheFromAlone(t *testing.T) {
	renames := MustRenames(MustRename("sjawhar/legion", "CyberMonkLabs/legion"))
	for _, recorded := range []string{"sjawhar/legion", "SJAWHAR/Legion"} {
		to, moved := renames.MovedTo(MustParse(recorded))
		if !moved || to.String() != "CyberMonkLabs/legion" {
			t.Errorf("MovedTo(%s) = %s, %v; want CyberMonkLabs/legion, true", recorded, to, moved)
		}
	}
	for _, recorded := range []string{"CyberMonkLabs/legion", "cybermonklabs/legion", "sjawhar/oh-my-pi", "acme/widgets"} {
		if to, moved := renames.MovedTo(MustParse(recorded)); moved || to != (Repository{}) {
			t.Errorf("MovedTo(%s) = %s, %v; want the zero repository and false: no pair moved a repository away from that name", recorded, to, moved)
		}
	}
	if partner, paired := renames.Partner("CyberMonkLabs/legion"); !paired || partner.String() != "sjawhar/legion" {
		t.Errorf("Partner(CyberMonkLabs/legion) = %s, %v; want sjawhar/legion, true: the equivalence answers from either side", partner, paired)
	}
	var zero Renames
	if to, moved := zero.MovedTo(MustParse("sjawhar/legion")); moved || to != (Repository{}) {
		t.Errorf("the zero Renames.MovedTo(sjawhar/legion) = %s, %v; want the zero repository and false", to, moved)
	}
}
