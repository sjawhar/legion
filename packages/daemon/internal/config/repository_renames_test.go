package config

import (
	"slices"
	"strings"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+tc.body), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A project may name one workflow of one repository once, the configured renames included: two
// deploys entries whose repositories a pair makes one repository and whose workflow is the same
// are both matched by one run (intake.DeployOf), which answers the first, so the second entry's
// step would never be read. readDeploys cannot see it — a project is read before the renames
// settle — so the settle refuses it, naming both entries, the workflow and the pair. The same two
// entries under no rename, the pair with another workflow each, and the pair across two projects
// are each configured as they are.
func TestDeploysOfOnePairsRepositoriesAreRefused(t *testing.T) {
	const pair = "repository_renames: [{from: sjawhar/legion, to: cybermonklabs/legion}]\n"
	project := func(deploys ...string) string {
		body := "projects:\n  DEMO:\n    deploys:\n"
		for _, deploy := range deploys {
			body += "      - " + deploy + "\n"
		}
		return strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", body, 1)
	}
	const (
		old     = "{repo: sjawhar/legion, workflow: .github/workflows/release.yml, step: Publish}"
		moved   = "{repo: CyberMonkLabs/legion, workflow: .github/workflows/release.yml, step: Deploy}"
		another = "{repo: CyberMonkLabs/legion, workflow: .github/workflows/envoy.yaml, step: Apply}"
	)

	_, err := Load(writeConfigFile(t, project(old, moved)+pair), noEnv)
	want := "projects.DEMO.deploys[0] (sjawhar/legion .github/workflows/release.yml) and projects.DEMO.deploys[1] (CyberMonkLabs/legion .github/workflows/release.yml) " +
		"name one workflow of one repository: repository_renames makes sjawhar/legion and CyberMonkLabs/legion one repository, " +
		"so a run of .github/workflows/release.yml matches both entries and only the first entry's step is ever read; name that workflow once"
	if err == nil || err.Error() != want {
		t.Fatalf("Load error = %v, want %q", err, want)
	}

	twoProjects := strings.Replace(minimalFile, "projects:\n  DEMO: {}\n",
		"projects:\n  DEMO:\n    deploys:\n      - "+old+"\n  OTHER:\n    deploys:\n      - "+moved+"\n", 1)
	for _, tc := range []struct{ name, body string }{
		{"the same two entries under no rename", project(old, moved)},
		{"the pair's two names with a workflow each", project(old, another) + pair},
		{"the pair's two names, one in each project", twoProjects + pair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeConfigFile(t, tc.body), noEnv); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}
