package launcher

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The directories an issue pod's container gives each role under the roles directory
// (--roles-dir): its launcher token projection, its gh bundle, and its own memory-backed
// private, state and agent-secrets directories. role-init binds all of them but the token back
// at the fixed paths every role is told, so neither the shim nor the agent learns a new path
// (internal/runtime/sandbox's names.go). roleHomesDir is the one disk directory of the roles
// directory: it holds each role's home overlay layers and its own temporary directories.
const (
	roleLauncherDir     = "launcher"
	roleGHDir           = "gh"
	rolePrivateDir      = "private"
	roleStateDir        = "state"
	roleAgentSecretsDir = "agent-secrets"
	roleHomesDir        = ".homes"
	launcherTokenFile   = "LAUNCHER_TOKEN"
)

// roleHome is the home directory a role's overlay covers, and roleFixedRoot the root its own
// directories are bound back under: the worker image's, which the image makes as empty
// root-owned mount points (packages/daemon/docker/worker.Dockerfile). A test points both at
// directories of its own, since a machine running the tests has neither.
var (
	roleHome      = "/home/legion"
	roleFixedRoot = "/var/run/legion"
)

// RoleHome and RoleFixedRoot are those defaults, which `legion launcher role-init`'s flags carry
// so the command line says what the launcher gave it.
func RoleHome() string { return roleHome }

func RoleFixedRoot() string { return roleFixedRoot }

// RoleInitConfig is `legion launcher role-init`'s resolved flags: the role whose namespaces this
// process is the init of and the generation it runs, where every role's directories are, the home
// its overlay covers, the root its own directories are bound back under, the pipe it writes the
// shim's own wait status to, and the shim argv it runs as its child.
type RoleInitConfig struct {
	RolesDir   string   `json:"rolesDir"`
	Role       string   `json:"role"`
	Generation uint64   `json:"generation"`
	Home       string   `json:"home"`
	FixedRoot  string   `json:"fixedRoot"`
	StatusFD   int      `json:"statusFd"`
	Argv       []string `json:"argv"`
	// Stdout and Stderr are the launcher's own, which the shim inherits, so one container's log
	// carries every role's output behind its own prefix.
	Stdout io.Writer `json:"-"`
	Stderr io.Writer `json:"-"`
}

// roleStatusFD is the descriptor role-init writes the shim's wait status to: the launcher's one
// extra file, so the first descriptor after the standard three.
const roleStatusFD = 3

func (c RoleInitConfig) validate() error {
	switch {
	case !filepath.IsAbs(c.RolesDir):
		return fmt.Errorf("role-init: roles directory %q is not absolute", c.RolesDir)
	case c.Role == "":
		return errors.New("role-init: no role")
	case c.Generation == 0:
		return errors.New("role-init: no generation")
	case !filepath.IsAbs(c.Home):
		return fmt.Errorf("role-init: home %q is not absolute", c.Home)
	case !filepath.IsAbs(c.FixedRoot):
		return fmt.Errorf("role-init: fixed root %q is not absolute", c.FixedRoot)
	case c.StatusFD < roleStatusFD:
		return fmt.Errorf("role-init: status descriptor %d is not one the launcher passes", c.StatusFD)
	case len(c.Argv) == 0:
		return errors.New("role-init: no shim argv")
	}
	return nil
}

// RoleInit is `legion launcher role-init`: the init of one role generation's user, mount, PID and
// IPC namespaces, which the launcher clones into them and nothing else ever runs. It hides every
// other role's credentials and its own launcher token, binds the role's own directories back at
// the fixed paths, gives the role a private home overlay and private temporary directories,
// starts the shim as its child in a nested user namespace as the image's unprivileged user,
// forwards stop signals to it, reaps the orphans the role leaves, and reports the shim's own
// wait status. Every failure it can hit is this role's exit and never the launcher's.
func RoleInit(cfg RoleInitConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	return roleInit(cfg)
}

// roleInitLaunch is the command line and the extra environment that run one generation's
// role-init: this binary's own hidden subcommand. A test replaces it with its re-entrant helper.
var roleInitLaunch = func(cfg RoleInitConfig) (argv, env []string, err error) {
	self, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("find this binary: %w", err)
	}
	argv = []string{
		self, "launcher", RoleInitSubcommand,
		"--roles-dir", cfg.RolesDir, "--role", cfg.Role, "--generation", strconv.FormatUint(cfg.Generation, 10),
		"--home", cfg.Home, "--fixed-root", cfg.FixedRoot, "--status-fd", strconv.Itoa(cfg.StatusFD), "--",
	}
	return append(argv, cfg.Argv...), nil, nil
}

// RoleInitSubcommand is `legion launcher`'s hidden subcommand, the one the launcher runs itself.
const RoleInitSubcommand = "role-init"

// roleProcess is one generation's role-init: the command that is the init of the role's new
// namespaces, and the pipe it writes the shim's own wait status to.
type roleProcess struct {
	cmd    *exec.Cmd
	status *os.File
	write  *os.File
}

// newRoleProcess builds a generation's role-init over cfg, with env the generation's environment
// and stdout and stderr the role's own prefixed writers.
func newRoleProcess(cfg RoleInitConfig, env []string, stdout, stderr io.Writer) (*roleProcess, error) {
	attr, err := roleNamespaceAttr()
	if err != nil {
		return nil, err
	}
	cfg.StatusFD = roleStatusFD
	argv, extra, err := roleInitLaunch(cfg)
	if err != nil {
		return nil, err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("role %s status pipe: %w", cfg.Role, err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = mergeEnv(env, extra)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = []*os.File{write}
	cmd.SysProcAttr = attr
	return &roleProcess{cmd: cmd, status: read, write: write}, nil
}

// start runs the generation and hands the status pipe's write end to it alone, so the read end
// ends as soon as role-init is gone.
func (p *roleProcess) start() error {
	err := p.cmd.Start()
	_ = p.write.Close()
	if err != nil {
		_ = p.status.Close()
	}
	return err
}

// parseStatus reads the wait status role-init wrote to its status pipe: the shim's own, so the
// daemon is told the role's exit and not its namespace init's. It is nil when role-init wrote
// none — it was killed before the shim ended, or never got that far.
func parseStatus(raw []byte) *syscall.WaitStatus {
	value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil
	}
	status := syscall.WaitStatus(value)
	return &status
}

// exitOfStatus is the exit a wait status reports.
func exitOfStatus(status syscall.WaitStatus, generation uint64) shimwire.LauncherExit {
	exit := shimwire.LauncherExit{Generation: generation, Code: status.ExitStatus()}
	if status.Signaled() {
		exit.Signal = status.Signal().String()
	}
	return exit
}
