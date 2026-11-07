package daemon

// The fourth place a lost session is reported is the session that replaces it
// (dispatch://INFRA-714 R8): nothing in a new session says an older one existed, so an agent
// started on a claim whose session is gone would read an empty transcript as a fresh start and
// redo work that is already pushed.

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func TestTheAddressingTellsAFreshAgentThatItsSessionWasLost(t *testing.T) {
	token, err := claim.NewToken("legion", "LEGION-209", claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	c := supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleTester}

	kept, err := AddressingFragment("legion", c, ghrepo.Repository{}, "", api.RepositoriesView{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(kept, "no session store") {
		t.Fatalf("a claim whose session is intact is told %q, want nothing of a lost session", kept)
	}

	c.SessionLost = true
	lost, err := AddressingFragment("legion", c, ghrepo.Repository{}, "", api.RepositoriesView{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"found in no session store", "only what you pushed and committed survives", "tell your architect"} {
		if !strings.Contains(lost, want) {
			t.Errorf("the fresh agent is told %q, want it to say %q", lost, want)
		}
	}
}
