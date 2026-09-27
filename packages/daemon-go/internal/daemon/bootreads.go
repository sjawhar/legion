package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/prompts"
)

// bootReads is what boot reads from the configuration, the daemon's environment and the files they
// name before it writes anything (readBoot).
type bootReads struct {
	project, operatorToken, dispatchToken, rolesDir string
	secrets                                         map[string]string
	natsUser                                        string // the NATS nkey seed's user's public key, "" with no seed
	instructions                                    []byte // nil when the configuration names none
	roleReferences                                  promptrefs.Names
	tmux                                            tmuxReads    // runtime: tmux
	sandbox                                         sandboxReads // runtime: kubernetes
}

// readBoot is every refusal boot makes from the configuration, the daemon's environment (lookup,
// and getenv for the OMP invocation) and the files they name, writing nothing: the project token,
// the operator bearer's file, operator configuration colliding with Legion's own
// (checkOperatorConfig), the launch secrets (readLaunchSecrets), the instructions file, the
// Dispatch bearer's file, the role prompts, and the runtime's own reads (readTmux, readSandbox).
// hostOMP is whether boot runs the host's Oh My Pi, false only for a test that replaced the
// runtime. prepare runs it first; CheckStart is it for `legion start --check-config`, so the check
// refuses whatever boot refuses before its first write. What boot does after it is not a check's to
// do: create the state directory, write the instructions copy and the Dispatch token file, read
// provider keys from secretsd, and run the plugin gate or the image probe.
func readBoot(cfg config.Config, lookup func(string) (string, bool), getenv func(string) string, hostOMP bool, log *slog.Logger) (bootReads, error) {
	var r bootReads
	var err error
	if r.project, err = claim.ProjectToken(cfg.Project); err != nil {
		return bootReads{}, err
	}
	if cfg.OperatorTokenFile == "" {
		return bootReads{}, errors.New("operator_token_file is required: the operator routes that spawn and drive claims authenticate against the bearer it names")
	}
	if r.operatorToken, err = config.ReadSecretPointer("operator_token_file", cfg.OperatorTokenFile); err != nil {
		return bootReads{}, err
	}
	if err := checkOperatorConfig(cfg, lookup); err != nil {
		return bootReads{}, err
	}
	if r.secrets, err = readLaunchSecrets(cfg, lookup); err != nil {
		return bootReads{}, err
	}
	if seed := r.secrets[natsauth.SeedVariable]; seed != "" {
		if r.natsUser, err = natsauth.PublicKey(seed); err != nil {
			return bootReads{}, err
		}
	}
	if cfg.InstructionsPath != "" {
		if r.instructions, err = config.ReadDeploymentInstructions(cfg.InstructionsPath); err != nil {
			return bootReads{}, err
		}
	}
	if cfg.DispatchURL != "" {
		if r.dispatchToken, err = config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile); err != nil {
			return bootReads{}, err
		}
	}
	if r.rolesDir, err = prompts.ResolveRolePromptsDir(os.LookupEnv); err != nil {
		return bootReads{}, fmt.Errorf("resolve role prompts: %w", err)
	}
	if r.roleReferences, err = promptrefs.Roles(r.rolesDir); err != nil {
		return bootReads{}, err
	}
	switch cfg.Runtime.Name {
	case "tmux":
		r.tmux, err = readTmux(cfg, lookup, getenv, hostOMP)
	case "kubernetes":
		r.sandbox, err = readSandbox(cfg, r.project, r.dispatchToken, r.natsUser, lookup, log)
	default:
		err = fmt.Errorf("runtime %q is neither tmux nor kubernetes", cfg.Runtime.Name)
	}
	if err != nil {
		return bootReads{}, err
	}
	return r, nil
}

// tmuxReads is what panes on this host need that readBoot reads: the OMP invocation, and the host's
// gh, git and jj.
type tmuxReads struct {
	invocation string            // "" when boot does not run the host's Oh My Pi
	tools      map[string]string // nil without a repository
}

// readTmux is tmux's share of readBoot: the OMP invocation when boot runs the host's Oh My Pi
// (hostOMP), whose one command is `mise where <tool>` when the invocation names a mise tool, and,
// for a configuration with a repository, the host's gh, git and jj Legion runs itself.
func readTmux(cfg config.Config, lookup func(string) (string, bool), getenv func(string) string, hostOMP bool) (tmuxReads, error) {
	var r tmuxReads
	var err error
	if hostOMP {
		if r.invocation, err = omplaunch.ResolveInvocation(cfg.OmpInvocation, getenv); err != nil {
			return tmuxReads{}, err
		}
	}
	if _, ok := cfg.Projects[cfg.Project]; ok {
		if r.tools, err = resolveTools(lookup); err != nil {
			return tmuxReads{}, err
		}
	}
	return r, nil
}

// CheckStart is `legion start --check-config`'s reading of what boot reads before it writes
// anything (readBoot), over the daemon's environment (lookup, and the process's for the OMP
// invocation): nil when boot would get past every such refusal, and the public key of the NATS
// nkey seed's user when the daemon has one, never the seed.
func CheckStart(cfg config.Config, lookup func(string) (string, bool)) (natsUser string, err error) {
	r, err := readBoot(cfg, lookup, os.Getenv, true, slog.New(slog.DiscardHandler))
	if err != nil {
		return "", err
	}
	return r.natsUser, nil
}
