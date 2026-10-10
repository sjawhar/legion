// Daemon-pane is the skill-scenario rig's one reading of the Legion daemon: what the daemon gives a
// phase-worker pane, computed by the daemon's own functions, for worker-pane.ts and
// daemon-standin.ts to run Oh My Pi and the stand-in daemon under. It belongs to no module:
// daemon-pane.ts runs it with `go run -overlay` as a main inside a checkout's packages/daemon, the
// one place the daemon's internal packages can be imported from.
//
//	phase <role>             prints the phase the role's worker runs in: the first phase
//	                         workflow.RoleFor gives to the role
//	pane                     reads a paneRequest (JSON) on stdin, writes the claim's gh files
//	                         under its GH_CONFIG_DIR, and prints its paneResponse (JSON)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const usage = "usage: daemon-pane phase <role> | pane < request.json"

func main() {
	args := os.Args[1:]
	var err error
	switch {
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

func printPhase(role claim.Role) error {
	for _, p := range phase.All {
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
	// GHToken is the GitHub token the rig stands in for the claim's App token: it is rendered
	// into the claim's gh files (hosts.yml and config.yml under runtime.GHConfigDir), which the
	// pane's GH_CONFIG_DIR names, as the daemon's tmux runtime writes them at spawn. "" is a
	// daemon with no GitHub credential: no files, and no gh variable on the pane.
	GHToken string `json:"ghToken"`
	// Path is the PATH the daemon would start under; the pane's PATH is it behind the pane's
	// launcher directory (workerbin.Path).
	Path string `json:"path"`
	// SystemPrompt asks for the pane's system prompt, composed from the role prompts the daemon
	// module embeds.
	SystemPrompt bool `json:"systemPrompt"`
}

type paneResponse struct {
	// Env is every variable the daemon tells the pane, PATH included.
	Env map[string]string `json:"env"`
	// SystemPromptArgument is the pane's one --append-system-prompt argument, as shell text; ""
	// without SystemPrompt.
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
	ghConfigDir := ""
	if req.GHToken != "" {
		// The runtime's own writer (tmux.WriteGHConfig), so the rig's directory is the daemon's layout;
		// the App label and expiry only feed the runtime's log lines, which the rig does not write.
		ghConfigDir = runtime.GHConfigDir(req.StateDir, token)
		if _, err := tmux.WriteGHConfig(ghConfigDir, ghconfig.Render(req.GHToken, "", time.Time{})); err != nil {
			return fmt.Errorf("write the claim's gh files: %w", err)
		}
	}
	spec := runtime.SpawnSpec{Claim: token, Project: req.Project, Tree: req.Issue, Issue: req.Issue, Role: role, Generation: 1}
	variables, err := tmux.PaneVariables(spec, tmux.PaneInputs{
		StateDir: req.StateDir, Workspace: req.Workspace, DaemonURL: req.DaemonURL, EnvoyURL: req.EnvoyURL,
		NATSURLs: req.NATSURLs, GHConfigDir: ghConfigDir,
	}, []runtime.SecretFile{{Variable: "LEGION_BOOT_TOKEN", Path: req.BootTokenFile}})
	if err != nil {
		return err
	}
	res := paneResponse{Env: map[string]string{"PATH": workerbin.Path(req.Path, req.StateDir)}}
	for _, variable := range variables {
		name, value, _ := strings.Cut(variable, "=")
		res.Env[name] = value
	}
	if req.SystemPrompt {
		if res.SystemPromptArgument, err = systemPromptArgument(req, token); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

// systemPromptArgument composes the pane's role prompt as the daemon does (specs.SpawnSpec): the
// role prompts snapshotted under the state directory, the role's parts, then its addressing. A
// reviewer is told its project's review workflows after that, as a daemon whose project declares
// none tells it, since the rig has no project configuration.
func systemPromptArgument(req paneRequest, token claim.Token) (string, error) {
	role := claim.Role(req.Role)
	if claim.IsTreeArchitect(role, req.Issue, req.Issue) {
		return "", errors.New("a tree architect is also told its design gate policy, which the rig does not stand in for")
	}
	composer, err := prompts.New(req.StateDir)
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
	if role == claim.RoleReviewer {
		addressing += " " + daemon.ReviewWorkflowsFragment(nil)
	}
	return omplaunch.SystemPromptArgument(runtime.PromptParts{RolePromptPaths: parts.RolePromptPaths, Addressing: addressing}), nil
}
