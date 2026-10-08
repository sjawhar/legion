package daemon

import (
	"context"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// On the tmux runtime the daemon provisions each issue's workspace on its host, and the request
// carries the project's workspace_exclude, which a workspace it creates leaves out.
func TestTheHostProvisioningLeavesTheProjectsExclusionsOut(t *testing.T) {
	stateDir := t.TempDir()
	sup, _ := newOutboxSupervisor(t, "legion", stateDir)
	configured := config.Project{Repo: ghrepo.MustParse("acme/widgets"), WorkspaceExclude: []string{"tasks", "src/gen"}}
	runner := newOutbox(nil, nil, nil, nil, sup, nil, outboxTokens{}, nil, "legion", "LEGION", stateDir, configured, "", nil, quietLogger())
	var requests []workspace.Request
	runner.provision = func(_ context.Context, request workspace.Request) (workspace.Workspace, error) {
		requests = append(requests, request)
		return workspace.Workspace{}, nil
	}
	if err := runner.provisionWorkspace(context.Background(), record.Issue{Key: "LEGION-208"}); err != nil {
		t.Fatalf("provisionWorkspace: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("provisioned %d times, want once", len(requests))
	}
	if want := []string{"tasks", "src/gen"}; !reflect.DeepEqual(requests[0].Exclude, want) {
		t.Errorf("the provisioning request excludes %q, want %q", requests[0].Exclude, want)
	}
}
