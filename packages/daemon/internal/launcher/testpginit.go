package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// TestPGInitSubcommand is `legion launcher`'s second hidden subcommand, the one the launcher runs
// itself: the init of the test server's own user, mount, PID and IPC namespaces.
const TestPGInitSubcommand = "testpg-init"

// TestPGInitSetupFailedExit is the exit status `legion launcher testpg-init` uses for one thing
// alone: it failed before it first ran the script, setting its namespaces up. It is
// RoleInitSetupFailedExit's sibling and the same value, sysexits' EX_OSERR.
const TestPGInitSetupFailedExit = RoleInitSetupFailedExit

// TestPGInitConfig is `legion launcher testpg-init`'s resolved flags: where every role's
// directories are, where the container mounts the home's volumes, the DSN the script must serve,
// and the descriptor it reports its first `ensure` on. The data directory is testPGDir(RolesDir),
// the one directory of the roles directory this process keeps.
type TestPGInitConfig struct {
	RolesDir   string `json:"rolesDir"`
	HomeMounts string `json:"homeMounts"`
	DSN        string `json:"dsn"`
	// ReadyFD is the write end of the launcher's pipe. This process closes it when its first
	// `ensure` has returned, so the launcher can hold a role start until the server the role
	// is told about is there. It is the launcher's one extra file, so the first descriptor
	// after the standard three.
	ReadyFD int `json:"readyFd"`
	// Stdout and Stderr are the launcher's own, which the script inherits, so the container's
	// log carries the server's output behind the script's prefix.
	Stdout io.Writer `json:"-"`
	Stderr io.Writer `json:"-"`
}

// testPGReadyFD is the descriptor the launcher passes as TestPGInitConfig.ReadyFD.
const testPGReadyFD = 3

func (c TestPGInitConfig) validate() error {
	switch {
	case !filepath.IsAbs(c.RolesDir):
		return fmt.Errorf("testpg-init: roles directory %q is not absolute", c.RolesDir)
	case !filepath.IsAbs(c.HomeMounts):
		return fmt.Errorf("testpg-init: home mounts %q is not absolute", c.HomeMounts)
	case c.DSN == "":
		return errors.New("testpg-init: no DSN")
	case c.ReadyFD < testPGReadyFD:
		return fmt.Errorf("testpg-init: ready descriptor %d is not one the launcher passes", c.ReadyFD)
	}
	return nil
}

// TestPGInit is `legion launcher testpg-init`: the init of the pod's test PostgreSQL's own user,
// mount, PID and IPC namespaces, which the launcher clones into them and nothing else ever runs.
// It covers every role's directories, binds the server's own back, mounts a /proc of its own,
// runs the image's script — `ensure` at once, reported on ReadyFD, and again on every tick — until
// stop, and reaps whatever the server leaves behind. Its exit ends the namespaces, and with them
// the postmaster, which is what makes it the server's PID 1 rather than the launcher.
//
// The namespaces are what bounds the DSN's role, which is this cluster's superuser: `copy … to
// program`, `copy … from '<path>'` and pg_read_file run as the postmaster's process, and the
// postmaster's process used to be the launcher's child in the roles container's own namespace,
// where every role's gh, private, state and agent-secrets directories are readable by the uid it
// runs as. In these namespaces they are behind an empty filesystem. The PID namespace is what
// keeps the processes apart: no role's and no launcher's pid exists in it at all, so there is no
// /proc/<pid> of one to open a root or an environment through. The one /proc/1 there is this
// process, whose root is the same covered mount namespace and whose environment is the
// launcher's, which carries <NAME>_FILE pointers and no secret value.
func TestPGInit(stop context.Context, cfg TestPGInitConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	return testPGInit(stop, cfg)
}

// testPGInitLaunch is the command line that runs the test server's init: this binary's own hidden
// subcommand. A test replaces it with its re-entrant helper.
var testPGInitLaunch = func(cfg TestPGInitConfig) (argv, env []string, err error) {
	self, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("find this binary: %w", err)
	}
	return []string{
		self, "launcher", TestPGInitSubcommand,
		"--roles-dir", cfg.RolesDir, "--home-mounts", cfg.HomeMounts, "--dsn", cfg.DSN,
		"--ready-fd", strconv.Itoa(cfg.ReadyFD),
	}, nil, nil
}
