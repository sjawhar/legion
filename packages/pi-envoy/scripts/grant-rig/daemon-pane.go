// Daemon-pane is the rigs' one reading of the Legion daemon: what the daemon gives a phase-worker
// pane, computed by the daemon's own functions, for the grant rig (run.ts, setup.sh,
// daemon-standin.ts) and the skill-scenario rig (../skill-scenarios/worker-pane.ts) to run Oh My Pi
// and the stand-in daemon under. It belongs to no module: daemon-pane.ts runs it with `go run
// -overlay` as a main inside a checkout's packages/daemon, the one place the daemon's internal
// packages can be imported from.
//
//	install <root> <legion>  installs root's worker-bin/gh and bin/legion, which execs legion, as
//	                         the daemon does at boot (workerbin.Install), and prints root's
//	                         worker-bin
//	phase <role>             prints the phase the role's worker runs in: the first phase
//	                         workflow.RoleFor gives to the role
//	pane                     reads a paneRequest (JSON) on stdin and prints its paneResponse (JSON)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const usage = "usage: daemon-pane install <root> <legion> | phase <role> | pane < request.json"

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 3 && args[0] == "install":
		err = install(args[1], args[2])
	case len(args) == 2 && args[0] == "phase":
		err = printPhase(claim.Role(args[1]))
	case len(args) == 1 && args[0] == "pane":
		err = pane()
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-pane %s: %v\n", args[0], err)
		os.Exit(1)
	}
}

func install(root, legion string) error {
	if err := workerbin.Install(root, legion); err != nil {
		return err
	}
	fmt.Println(workerbin.Dir(root))
	return nil
}

// phases is the phase vocabulary in the transition table's order (internal/phase).
var phases = []phase.Phase{
	phase.Admitted, phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Retro,
	phase.Merging, phase.AwaitingMerge, phase.ProductionCheck, phase.Done, phase.Held,
}

func printPhase(role claim.Role) error {
	for _, p := range phases {
		if workflow.RoleFor(p) == role {
			fmt.Println(p)
			return nil
		}
	}
	return fmt.Errorf("%q works no phase", role)
}

// paneRequest is the claim a pane is launched for, and the rig's stand-ins for what the daemon
// knows at boot. The pane's tree is its issue.
type paneRequest struct {
	Project string `json:"project"`
	Issue   string `json:"issue"`
	Role    string `json:"role"`
	// StateDir stands for the daemon's state_dir, Workspace for the issue's workspace.
	StateDir  string   `json:"stateDir"`
	Workspace string   `json:"workspace"`
	DaemonURL string   `json:"daemonUrl"`
	EnvoyURL  string   `json:"envoyUrl"`
	NATSURLs  []string `json:"natsUrls"`
	// BootTokenFile is the rig's boot token file, the one its stand-in daemon checks.
	BootTokenFile string `json:"bootTokenFile"`
	// Path is the PATH the daemon would start under: its gh, git and jj resolve on it as at boot,
	// and the pane's PATH is it behind the pane's worker-bin and bin.
	Path string `json:"path"`
	// RolesDir, when set, is the role-prompt bundle the pane's system prompt is composed from.
	RolesDir string `json:"rolesDir"`
}

type paneResponse struct {
	// Env is every variable the daemon tells the pane, PATH included.
	Env map[string]string `json:"env"`
	// SystemPromptArgument is the pane's one --append-system-prompt argument, as shell text; ""
	// without a RolesDir.
	SystemPromptArgument string `json:"systemPromptArgument,omitempty"`
}

func pane() error {
	var req paneRequest
	decoder := json.NewDecoder(os.Stdin)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	role := claim.Role(req.Role)
	token, err := claim.NewToken(req.Project, req.Issue, role)
	if err != nil {
		return err
	}
	tools, err := daemon.PaneTools(func(name string) (string, bool) {
		if name == "PATH" {
			return req.Path, true
		}
		return "", false
	})
	if err != nil {
		return err
	}
	spec := runtime.SpawnSpec{Claim: token, Project: req.Project, Tree: req.Issue, Issue: req.Issue, Role: role, Generation: 1}
	variables, err := tmux.PaneVariables(spec, tmux.PaneInputs{
		StateDir: req.StateDir, Workspace: req.Workspace, DaemonURL: req.DaemonURL, EnvoyURL: req.EnvoyURL,
		NATSURLs: req.NATSURLs, Tools: tools,
	}, []runtime.SecretFile{{Variable: "LEGION_BOOT_TOKEN", Path: req.BootTokenFile}})
	if err != nil {
		return err
	}
	res := paneResponse{Env: map[string]string{"PATH": workerbin.Path(req.Path, req.StateDir)}}
	for _, variable := range variables {
		name, value, _ := strings.Cut(variable, "=")
		res.Env[name] = value
	}
	if req.RolesDir != "" {
		if res.SystemPromptArgument, err = systemPromptArgument(req, token); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

// systemPromptArgument composes the pane's role prompt as the daemon does (specs.SpawnSpec): the
// bundle snapshotted under the state directory, the role's parts, then its addressing.
func systemPromptArgument(req paneRequest, token claim.Token) (string, error) {
	role := claim.Role(req.Role)
	if claim.IsTreeArchitect(role, req.Issue, req.Issue) {
		return "", errors.New("a tree architect is also told its design gate policy, which the rig does not stand in for")
	}
	composer, err := prompts.New(req.RolesDir, req.StateDir)
	if err != nil {
		return "", err
	}
	parts, err := composer.Compose(role, claim.IsTreeRoot(req.Issue, req.Issue))
	if err != nil {
		return "", err
	}
	addressing, err := daemon.AddressingFragment(req.Project, supervise.Claim{
		Token: token, Project: req.Project, Tree: req.Issue, Issue: req.Issue, Role: role, Generation: 1,
	})
	if err != nil {
		return "", err
	}
	return omplaunch.SystemPromptArgument(runtime.PromptParts{RolePromptPaths: parts.RolePromptPaths, Addressing: addressing}), nil
}
