package config

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// repository_renames settles into the one equivalence every consumer takes, in file order: each
// pair is one repository in both directions and in any case, and a repository no pair names is
// itself alone.
func TestRepositoryRenamesSettleTheEquivalence(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile+"repository_renames:\n"+
		"  - from: sjawhar/legion\n    to: cybermonklabs/legion\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RepositoryRenames.Same("SJAWHAR/Legion", "cybermonklabs/legion") || !cfg.RepositoryRenames.Same("cybermonklabs/legion", "sjawhar/legion") {
		t.Errorf("RepositoryRenames = %v, want the pair one repository both ways", cfg.RepositoryRenames.Pairs())
	}
	if cfg.RepositoryRenames.Same("sjawhar/legion", "acme/widgets") {
		t.Error("an unconfigured repository is Same as a configured one")
	}

	cfg, err = Load(writeConfigFile(t, minimalFile+"repository_renames:\n"+
		"  - {from: sjawhar/legion, to: cybermonklabs/legion}\n"+
		"  - {from: acme/widgets, to: acme/gadgets}\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []ghrepo.Rename{
		{From: ghrepo.MustParse("sjawhar/legion"), To: ghrepo.MustParse("cybermonklabs/legion")},
		{From: ghrepo.MustParse("acme/widgets"), To: ghrepo.MustParse("acme/gadgets")},
	}
	if pairs := cfg.RepositoryRenames.Pairs(); !slices.Equal(pairs, want) {
		t.Errorf("RepositoryRenames.Pairs() = %v, want %v in file order", pairs, want)
	}
	if !cfg.RepositoryRenames.Same("acme/gadgets", "acme/widgets") {
		t.Error("the second pair is not one repository")
	}
}

// A file that names no rename, names the key with no value, or gives it an empty list settles the
// zero value, which answers exactly as a name comparison without case does.
func TestRepositoryRenamesAreTheZeroValueWithoutTheKey(t *testing.T) {
	for _, body := range []string{"", "repository_renames:\n", "repository_renames: []\n"} {
		cfg, err := Load(writeConfigFile(t, minimalFile+body), noEnv)
		if err != nil {
			t.Fatalf("Load(%q): %v", body, err)
		}
		if !cfg.RepositoryRenames.IsZero() {
			t.Errorf("Load(%q) RepositoryRenames = %v, want the zero value", body, cfg.RepositoryRenames.Pairs())
		}
		if !cfg.RepositoryRenames.Same("sjawhar/legion", "SJAWHAR/LEGION") || cfg.RepositoryRenames.Same("sjawhar/legion", "cybermonklabs/legion") {
			t.Errorf("Load(%q) RepositoryRenames answers something other than a comparison without case", body)
		}
	}
}

// Every refusal of repository_renames names the key, the entry and the value: a value that is no
// list, an entry that is no mapping, a field neither from nor to, a missing or empty side, a side
// Parse refuses, and each structural rule of the equivalence itself.
func TestRepositoryRenamesRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"not a list", "repository_renames: sjawhar/legion\n",
			"repository_renames must be an array"},
		{"not a list of mappings", "repository_renames: [sjawhar/legion]\n",
			"repository_renames[0] must be a mapping"},
		{"an unknown field", "repository_renames: [{from: a/b, to: c/d, when: yesterday}]\n",
			"unknown key repository_renames[0].when"},
		{"a field twice", "repository_renames: [{from: a/b, from: c/d, to: e/f}]\n",
			"repository_renames[0] names from twice"},
		{"no from", "repository_renames: [{to: c/d}]\n",
			"repository_renames[0].from is required: the repository as it was"},
		{"an empty from", "repository_renames: [{from: '', to: c/d}]\n",
			"repository_renames[0].from must not be empty"},
		{"no to", "repository_renames: [{from: a/b}]\n",
			"repository_renames[0].to is required: the repository it moved to"},
		{"an empty to", "repository_renames: [{from: a/b, to: ''}]\n",
			"repository_renames[0].to must not be empty"},
		{"a from Parse refuses", "repository_renames: [{from: legion, to: c/d}]\n",
			`repository_renames[0].from must be "owner/name" (got "legion")`},
		{"a to Parse refuses", "repository_renames: [{from: a/b, to: cybermonklabs/legion.git}]\n",
			`repository_renames[0].to "cybermonklabs/legion.git" ends in ".git", which GitHub strips from a repository's name: name it "cybermonklabs/legion"`},
		{"a repository renamed to itself", "repository_renames: [{from: a/b, to: A/B}]\n",
			"repository_renames: rename 0 (a/b -> A/B) renames a repository to itself: from and to name two repositories, compared without case"},
		{"one from twice", "repository_renames: [{from: a/b, to: c/d}, {from: A/B, to: e/f}]\n",
			"repository_renames: rename 1 (A/B -> e/f) and rename 0 (a/b -> c/d) both rename A/B: a repository moved to one name"},
		{"one to twice", "repository_renames: [{from: a/b, to: c/d}, {from: e/f, to: C/D}]\n",
			"repository_renames: rename 1 (e/f -> C/D) and rename 0 (a/b -> c/d) both name C/D as the repository moved to: a repository moved from one name"},
		{"a chain", "repository_renames: [{from: a/b, to: c/d}, {from: c/d, to: e/f}]\n",
			"repository_renames: rename 1 (c/d -> e/f) chains onto rename 0 (a/b -> c/d) through c/d: a rename pairs two names, never a chain of three"},
		{"a chain the other way round", "repository_renames: [{from: c/d, to: e/f}, {from: a/b, to: c/d}]\n",
			"repository_renames: rename 1 (a/b -> c/d) chains onto rename 0 (c/d -> e/f) through c/d: a rename pairs two names, never a chain of three"},
		{"one owner to two owners", "repository_renames: [{from: a/b, to: c/d}, {from: a/e, to: f/g}]\n",
			"repository_renames: rename 1 (a/e -> f/g) and rename 0 (a/b -> c/d) move repositories of a to two owners, f and c: an owner's repositories moved to one owner, since OwnerAliases keys an alias by a, the owner a pair moved a repository away from, and the second pair would overwrite the first silently"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+tc.body), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}
