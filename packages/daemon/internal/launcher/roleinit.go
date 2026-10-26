package launcher

import (
	"bytes"
	"context"
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

// roleHome is the home directory a role's overlay covers, roleHomeMounts where the container
// mounts what belongs under it, and roleFixedRoot the root a role's own directories are bound back
// under: the worker image's (Home, HomeMountsDir, FixedRoot). A test points them at directories of
// its own, since a machine running the tests has none of them.
var (
	roleHome       = Home
	roleHomeMounts = HomeMountsDir
	roleFixedRoot  = FixedRoot
)

// RoleHome, RoleHomeMounts and RoleFixedRoot are those defaults, which `legion launcher
// role-init`'s flags carry so the command line says what the launcher gave it.
func RoleHome() string { return roleHome }

func RoleHomeMounts() string { return roleHomeMounts }

func RoleFixedRoot() string { return roleFixedRoot }

// RoleInitConfig is `legion launcher role-init`'s resolved flags: the role whose namespaces this
// process is the init of and the generation it runs, where every role's directories are, the home
// its overlay covers and the directory the container mounts the home's volumes under, the root its
// own directories are bound back under, the pipe it writes the shim's own wait status to, and the
// shim argv it runs as its child.
type RoleInitConfig struct {
	RolesDir   string   `json:"rolesDir"`
	Role       string   `json:"role"`
	Generation uint64   `json:"generation"`
	Home       string   `json:"home"`
	HomeMounts string   `json:"homeMounts"`
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

// RoleInitSetupFailedExit is the exit status `legion launcher role-init` uses for one thing alone:
// it failed before it started the shim, setting up the role's namespaces or starting the shim
// itself. Every other end of role-init is told apart from it: once the shim started, role-init
// reports the shim's own wait status and exits 0, a role-init that panics exits 2, and one that
// Go's runtime ends for a signal no one registered for exits 128 plus the signal (roleInit).
// sysexits' EX_OSERR, an operating-system error, which is what such a failure is.
const RoleInitSetupFailedExit = 71

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
	case !filepath.IsAbs(c.HomeMounts) || overlaps(c.HomeMounts, c.Home):
		return fmt.Errorf("role-init: home mounts %q is not an absolute directory apart from the home %q", c.HomeMounts, c.Home)
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
// the fixed paths, gives the role a private home overlay, its own config home, private temporary
// directories and its own pseudo-terminals and message queues, starts the shim as its child in a
// nested user namespace as the image's unprivileged user, forwards the launcher's stop to it,
// reaps the orphans the role leaves, and reports the shim's own wait status. Every failure it can
// hit is this role's exit and never the launcher's. stop ends at the launcher's stop: the context
// `legion`'s main ends on SIGTERM or SIGINT, the signals it registers for in its first statement.
// Every error it returns is a failure before the shim started, which `legion launcher role-init`
// exits RoleInitSetupFailedExit for.
func RoleInit(stop context.Context, cfg RoleInitConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	return roleInit(stop, cfg)
}

// CheckRoleNamespaces runs one role generation as an issue pod's launcher runs every role — its
// role-init cloned into a user, mount, PID and IPC namespace of its own, which sets the namespaces
// up and starts its shim in a nested user namespace as the image's unprivileged user — over a
// layout of its own under dir, with shim as the shim's argv, and refuses unless the shim ran and
// exited 0. `legion probe-image --role-namespaces` runs it in the daemon's probe pod, placed and
// secured as every issue pod is, with dir on a disk volume as the role homes volume is and its
// own `legion version` as the shim, so a runtime or node that refuses a role's namespaces, a mount
// role-init makes, or the overlay over its home fails the daemon's boot rather than every role's
// start. The layout is the roles directory, fixed root, home and home mounts directory role-init's
// flags name, with the one role's gh, private and state directories and nothing else.
func CheckRoleNamespaces(dir string, shim ...string) error {
	if len(shim) == 0 {
		return errors.New("role namespaces: no shim argv")
	}
	const role = "probe"
	cfg := RoleInitConfig{
		RolesDir: filepath.Join(dir, "roles"), Role: role, Generation: 1, Home: filepath.Join(dir, "home"),
		HomeMounts: filepath.Join(dir, "home-mounts"), FixedRoot: filepath.Join(dir, "fixed"), Argv: shim,
	}
	dirs := []string{cfg.Home, cfg.HomeMounts}
	for _, name := range []string{RoleGHDir, RolePrivateDir, RoleStateDir} {
		dirs = append(dirs, filepath.Join(cfg.RolesDir, role, name), filepath.Join(cfg.FixedRoot, name))
	}
	for _, path := range dirs {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("role namespaces: make the probe's layout: %w", err)
		}
	}
	var output bytes.Buffer
	process, err := newRoleProcess(cfg, os.Environ(), &output, &output)
	if err != nil {
		return fmt.Errorf("role namespaces: %w", err)
	}
	if err := process.start(); err != nil {
		return fmt.Errorf("role namespaces: start role-init in namespaces of its own: %w", err)
	}
	raw, _ := io.ReadAll(process.status)
	_ = process.status.Close()
	waitErr := process.cmd.Wait()
	if status := parseStatus(raw); waitErr != nil || status == nil || status.ExitStatus() != 0 || status.Signaled() {
		shim := "none, it never ran or never ended"
		if status != nil {
			shim = fmt.Sprintf("%+v", exitOfStatus(*status, cfg.Generation))
		}
		return fmt.Errorf("role namespaces: a role generation over a probe layout under %s did not run its shim to a clean exit (role-init: %v; the shim's exit: %s): %s",
			dir, waitErr, shim, strings.TrimSpace(output.String()))
	}
	return nil
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
		"--home", cfg.Home, "--home-mounts", cfg.HomeMounts, "--fixed-root", cfg.FixedRoot,
		"--status-fd", strconv.Itoa(cfg.StatusFD), "--",
	}
	return append(argv, cfg.Argv...), nil, nil
}

// overlaps reports whether one of two clean absolute paths is the other or lies under it.
func overlaps(a, b string) bool {
	under := func(child, parent string) bool { return parent == "/" || strings.HasPrefix(child, parent+"/") }
	return a == b || under(a, b) || under(b, a)
}

// containerPath is where the launcher, outside every role's namespaces, finds a file a role names
// under its home: a path under the home is under the home mounts in the container, where the
// volume holding it is mounted, and any other path is the same in both views.
func containerPath(path string) string {
	rest, ok := strings.CutPrefix(path, roleHome+"/")
	if !ok {
		return path
	}
	return filepath.Join(roleHomeMounts, rest)
}

// RoleInitSubcommand is `legion launcher`'s hidden subcommand, the one the launcher runs itself.
const RoleInitSubcommand = "role-init"

// roleProcess is one generation's role-init: the command that is the init of the role's new
// namespaces, the pipe it writes the shim's own wait status to, and, in an issue pod's container,
// the prefixed writers of the generation's own output, which its wait flushes.
type roleProcess struct {
	cmd    *exec.Cmd
	status *os.File
	write  *os.File
	output []io.Writer
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
