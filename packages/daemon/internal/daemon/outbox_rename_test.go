package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// transferredLegion is the pair a deployment configures over sjawhar/legion's transfer to
// cybermonklabs: the records of a tree opened before it keep sjawhar/legion, while GitHub holds
// the repository under cybermonklabs/legion, on cybermonklabs' installation.
var transferredLegion = ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))

// identityTokens is roleTokens with a bot identity of its own for each owner, so a lease shows
// the owner it was minted for.
type identityTokens struct{ roleTokens }

func (t identityTokens) Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	lease, err := t.roleTokens.Token(ctx, role, owner)
	lease.Identity = runtime.GitIdentity{Name: "legion-" + string(role) + "[bot] of " + owner, Email: owner + "@users.noreply.github.com"}
	return lease, err
}

// The git identity a tree recorded before its repository's transfer commits as is minted on the
// installation that holds the repository now: the record keeps sjawhar/legion, and under the
// configured pair with cybermonklabs served the lease is cybermonklabs', which is the half the
// old owner cannot supply — the installation covering a transferred repository is the new
// owner's, so without this a launch and a working-copy adoption of such a tree both fail. Both
// of identity's callers, a launch's spec (specs.SpawnSpec) and a working copy's adoption
// (adoptIdentity), hand it the recorded repository and leave the owner to it. With no pair
// configured the same record is minted for sjawhar, as it always was.
func TestTheGitIdentityOfATreeRecordedBeforeAConfiguredRenameIsMintedForTheOwnerGitHubServes(t *testing.T) {
	recorded := ghrepo.MustParse("sjawhar/legion")
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		served  string
		want    string
	}{
		{name: "the pair configured", renames: transferredLegion, served: "cybermonklabs/legion", want: "cybermonklabs"},
		{name: "no pair configured", served: "sjawhar/legion", want: "sjawhar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repos := newRepositories("LEGION", tc.renames)
			repos.set([]mappedRepository{{Repo: ghrepo.MustParse(tc.served), Default: true}}, nil)
			w := &workflowRuntime{tokens: identityTokens{}, repositories: repos}
			want := runtime.GitIdentity{Name: "legion-implement[bot] of " + tc.want, Email: tc.want + "@users.noreply.github.com"}

			launch, err := w.identity(context.Background(), claim.RoleImplementer, recorded)
			if err != nil || launch != want {
				t.Fatalf("the identity of a launch on %s = %+v, %v; want %+v", recorded, launch, err, want)
			}
			adopt := adoptIdentity(w.identity, func(context.Context, string) (ghrepo.Repository, string, error) {
				return recorded, "main", nil
			})
			adopted, err := adopt(context.Background(), claim.RoleImplementer, "LEGION-208")
			if err != nil || adopted != want {
				t.Fatalf("the identity of an adoption of %s = %+v, %v; want %+v", recorded, adopted, err, want)
			}
		})
	}
}
