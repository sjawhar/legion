package appauth

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// A bundle reaches every repository the project serves under the role's own owner, whatever a
// configured pair moved away from that owner. The pair moves one repository, not the owner, so
// the owner's other repositories are the project's own and a role must be able to read them.
//
// The case: the project serves cybermonklabs/legion, sjawhar/oh-my-pi and sjawhar/tools, the pair
// sjawhar/legion -> cybermonklabs/legion resolves, and the issue is recorded on sjawhar/oh-my-pi.
// The sjawhar directory then holds the write lease for sjawhar/oh-my-pi, which names that
// repository alone, so sjawhar/tools needs a repository directory of its own, as it has one in
// every state where no pair resolves. The bundle's repository directories are cut for the owner
// mintOwner answers for the owner serving the role's repository, and for an alias owner that is
// the owner the pair moved to, so they are cut over cybermonklabs' repositories and sjawhar/tools
// gets none: `legion gh` on sjawhar/tools picks the sjawhar directory, whose token names
// sjawhar/oh-my-pi alone, and GitHub refuses it.
func TestTheBundleReachesEveryRepositoryTheProjectServesUnderTheRecordedOwner(t *testing.T) {
	repos := projectRepos("cybermonklabs/legion", "sjawhar/oh-my-pi", "sjawhar/tools")
	credential := GitHubCredential(ownerTokens{}, repos, ghrepo.MustRenames(legionRename), nil, quietBundleLog())
	bundle, err := credential(context.Background(), claim.RoleImplementer, ghrepo.MustParse("sjawhar/oh-my-pi"))
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	tokens := bundleTokens(t, bundle)
	reached := map[string]bool{}
	for _, token := range tokens {
		owner, names, ok := leaseNames(token)
		if !ok {
			t.Fatalf("the lease %q names no repository: the fakes render every lease as <app>-token-for-<owner>/<name> or <app>-read-for-<owner>/<name>[+<name>]", token)
		}
		for _, name := range names {
			reached[owner+"/"+name] = true
		}
	}
	for _, want := range []string{"cybermonklabs/legion", "sjawhar/oh-my-pi", "sjawhar/tools"} {
		if !reached[want] {
			t.Errorf("no lease of the bundle names %s, which the project serves; the bundle holds %v reaching %v",
				want, slices.Sorted(slices.Values(dirNames(tokens))), slices.Sorted(slices.Values(dirNames(reached))))
		}
	}
}

// leaseNames is the owner and the repository names a fake lease token carries.
func leaseNames(token string) (string, []string, bool) {
	for _, mark := range []string{"-token-for-", "-read-for-"} {
		_, rest, found := strings.Cut(token, mark)
		if !found {
			continue
		}
		owner, names, ok := strings.Cut(rest, "/")
		if !ok {
			return "", nil, false
		}
		return owner, strings.Split(names, "+"), true
	}
	return "", nil, false
}

// dirNames is the keys of m, for a failure that says what the bundle held.
func dirNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
