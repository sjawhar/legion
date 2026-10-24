package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/launcher"
)

const launcherUsage = "usage: legion launcher --connect tcp://host:port --sandbox <name> [--pod-uid <uid>] [--stop-grace <duration>] (--roles-dir <dir> --role <role> [--role <role>…] | --token-file <path> --role <role> --private-dir <dir>)"

// roleList collects a repeated --role: an issue pod's container names every resident role, the
// controller's pod exactly one.
type roleList []string

func (l *roleList) String() string { return strings.Join(*l, ",") }

func (l *roleList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// runLauncher is PID 1 of an issue pod's one container, which serves every resident role, or of
// the controller pod's one container, which serves its one role. It has no workflow policy: it
// only authenticates to the daemon, once per role, and starts or stops that role's worker-shim
// on daemon commands. Its own hidden subcommand, `role-init`, is what it runs as the init of a
// role's namespaces; nothing else ever runs it.
func runLauncher(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == launcher.RoleInitSubcommand {
		return runRoleInit(args[1:], stdout, stderr)
	}
	flags := newFlags("launcher", launcherUsage, stderr)
	connect := flags.String("connect", "", "the daemon launcher stream: tcp://host:port")
	tokenFile := flags.String("token-file", "", "the role-private launcher token file (the controller pod's one role)")
	sandbox := flags.String("sandbox", "", "the issue Sandbox name")
	var roles roleList
	flags.Var(&roles, "role", "a role this launcher serves; repeated with --roles-dir for an issue pod's roles")
	rolesDir := flags.String("roles-dir", "", "an issue pod's container-level per-role root: each role's launcher token, gh bundle and private, state and agent-secrets directories under <dir>/<role>")
	podUID := flags.String("pod-uid", os.Getenv("POD_UID"), "the current Kubernetes pod UID (POD_UID by default)")
	privateDir := flags.String("private-dir", "", "the controller container's own memory-backed directory for each generation's credentials")
	stopGrace := flags.Duration("stop-grace", 0, "container shutdown grace (30s when unset)")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if len(flags.Args()) != 0 {
		fmt.Fprintln(stderr, launcherUsage)
		return 2
	}
	cfg := launcher.Config{
		Connect: *connect, Sandbox: *sandbox, PodUID: *podUID, StopGrace: *stopGrace,
		Stdout: stdout, Stderr: stderr,
	}
	if *rolesDir != "" {
		cfg.RolesDir, cfg.Roles = *rolesDir, roles
		if *tokenFile != "" {
			fmt.Fprintln(stderr, "launcher: --roles-dir holds every role's own launcher token: no --token-file")
			return 2
		}
	} else {
		if len(roles) > 1 {
			fmt.Fprintln(stderr, "launcher: several roles need --roles-dir, which holds each one's own token and private directory")
			return 2
		}
		if len(roles) == 1 {
			cfg.Role = roles[0]
		}
		token, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "launcher: token file %s: %v\n", *tokenFile, err)
			return 1
		}
		cfg.Token, cfg.PrivateDir = strings.TrimSpace(string(token)), *privateDir
	}
	if err := launcher.Run(ctx, cfg); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

const roleInitUsage = "usage: legion launcher role-init --roles-dir <dir> --role <role> [--home <dir>] [--fixed-root <dir>] --status-fd <n> -- <shim argv…>"

// runRoleInit is `legion launcher role-init`, the launcher's own hidden subcommand: the init of
// one role generation's user, mount, PID and IPC namespaces, which the launcher clones into them
// with the generation's environment. It is never run by hand, and `legion launcher --help` does
// not offer it: run outside those namespaces it refuses, since it would otherwise mount a role's
// private view over the container's own.
func runRoleInit(args []string, stdout, stderr io.Writer) int {
	flags := newFlags("launcher role-init", roleInitUsage, stderr)
	rolesDir := flags.String("roles-dir", "", "the container-level per-role root")
	role := flags.String("role", "", "the role whose namespaces this process is the init of")
	home := flags.String("home", launcher.RoleHome(), "the home directory the role's own layer covers")
	fixedRoot := flags.String("fixed-root", launcher.RoleFixedRoot(), "the root the role's own directories are bound back under")
	statusFD := flags.Int("status-fd", 0, "the descriptor to report the shim's own wait status on")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if len(flags.Args()) == 0 {
		fmt.Fprintln(stderr, roleInitUsage)
		return 2
	}
	if err := launcher.RoleInit(launcher.RoleInitConfig{
		RolesDir: *rolesDir, Role: *role, Home: *home, FixedRoot: *fixedRoot, StatusFD: *statusFD,
		Argv: flags.Args(), Stdout: stdout, Stderr: stderr,
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
