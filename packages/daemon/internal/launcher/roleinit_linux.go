//go:build linux

package launcher

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"golang.org/x/sys/unix"
)

// roleUID is the uid a role's agent runs as inside its own namespaces: the worker image's
// unprivileged user, so the files it writes are that user's outside too, and every path the
// image lays out for it reads as it does in a container of its own. It holds no capability
// outside that nested namespace.
const roleUID = 1000

// roleNamespaceAttr clones a generation into the role's own user, mount, PID and IPC namespaces,
// with the launcher's own user mapped to root inside them, so role-init can mount there while
// nothing it does reaches the container or another role. gVisor supports exactly these flags
// (pkg/sentry/kernel/task_clone.go, SupportedCloneFlags).
func roleNamespaceAttr() (*syscall.SysProcAttr, error) {
	return &syscall.SysProcAttr{
		Setpgid:                    true,
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}, nil
}

// roleInit is `legion launcher role-init`'s body: the namespaces' setup, then the shim as this
// process's child until it ends.
func roleInit(cfg RoleInitConfig) error {
	if os.Getpid() != 1 {
		return errors.New("role-init: not the init of a PID namespace of its own")
	}
	// The kernel delivers a signal from outside to a PID namespace's init only once it handles
	// it, so role-init takes the launcher's stop from its first instruction: a stop that arrives
	// while the namespace is still being set up reaches the shim the moment it starts, rather
	// than waiting out the launcher's grace for a kill.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	// The shim never inherits the launcher's status pipe: role-init alone reports the role's exit.
	syscall.CloseOnExec(cfg.StatusFD)
	if err := prepareRoleNamespace(cfg); err != nil {
		return fmt.Errorf("role-init: %w", err)
	}
	return runRoleShim(cfg, signals)
}

// prepareRoleNamespace is everything the role's mount namespace holds that the container's does
// not. Its order matters: every mount whose source is under the roles directory is made before
// the cover goes over it, and the role's own /proc last, so the role sees its own processes
// alone from the moment it starts.
func prepareRoleNamespace(cfg RoleInitConfig) error {
	role := filepath.Join(cfg.RolesDir, cfg.Role)
	homes := filepath.Join(cfg.RolesDir, RoleHomesDir, cfg.Role)
	stage := filepath.Join(homes, "stage")
	works := filepath.Join(homes, "work")
	work := filepath.Join(works, shimwire.GenerationDir(cfg.Generation))
	if err := privateMounts(); err != nil {
		return fmt.Errorf("hold this role's mounts to itself: %w", err)
	}
	// A generation that failed midway left its stage's empty stand-ins behind; their mounts went
	// with its namespace, so nothing under the stage is a mount here.
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("clear the role's stage: %w", err)
	}
	for _, dir := range []string{filepath.Join(homes, "upper"), work, filepath.Join(homes, "tmp"), filepath.Join(homes, "var-tmp"), stage} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("make the role's own %s directory: %w", filepath.Base(dir), err)
		}
	}
	// Each generation's overlay works in a directory of its own, so its mount never waits on the
	// previous generation's namespace letting go of one, which the kernel may finish after that
	// generation's init is reaped. The upper layer is the role's across generations, as a role
	// container's own filesystem was. What earlier generations' overlays left is cleared; a
	// leftover only costs disk, so one that cannot be cleared is logged and the role starts.
	entries, err := os.ReadDir(works)
	if err != nil {
		return fmt.Errorf("read the role's overlay work directories: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == filepath.Base(work) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(works, entry.Name())); err != nil {
			roleLogf(cfg, "role-init: clear an earlier generation's overlay work directory: %v", err)
		}
	}
	// The role's own directories at the fixed paths every role is told, so neither the shim nor
	// the agent learns a path of its own. Its launcher token is bound nowhere: no role can read
	// any launcher token, its own included. The agent-secrets directory is there only when the
	// pod is enrolled with the secrets broker.
	for _, bind := range []struct {
		source, target string
		optional       bool
	}{
		{filepath.Join(role, RoleGHDir), filepath.Join(cfg.FixedRoot, RoleGHDir), false},
		{filepath.Join(role, RolePrivateDir), filepath.Join(cfg.FixedRoot, RolePrivateDir), false},
		{filepath.Join(role, RoleStateDir), filepath.Join(cfg.FixedRoot, RoleStateDir), false},
		{filepath.Join(role, RoleAgentSecretsDir), filepath.Join(cfg.FixedRoot, RoleAgentSecretsDir), true},
	} {
		if _, err := os.Stat(bind.source); err != nil {
			if bind.optional && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("the role's %s directory: %w", filepath.Base(bind.source), err)
		}
		if err := bindMount(bind.source, bind.target); err != nil {
			return fmt.Errorf("bind %s at %s: %w", bind.source, bind.target, err)
		}
	}
	if err := privateHome(cfg, homes, work); err != nil {
		return err
	}
	// The role's own temporary directories, on the pod's disk as a container filesystem is.
	for _, bind := range [][2]string{
		{filepath.Join(homes, "tmp"), "/tmp"},
		{filepath.Join(homes, "var-tmp"), "/var/tmp"},
	} {
		if err := carryOver(bind[1], stage, func() error { return bindMount(bind[0], bind[1]) }); err != nil {
			return fmt.Errorf("give the role its own %s: %w", bind[1], err)
		}
	}
	// The role's own small memory filesystems where the image has one for the whole container:
	// the lock directories and POSIX shared memory. A path the image does not have is skipped,
	// and a path that is another's link (the image's /var/lock) is covered once; any other
	// failure to read the path is a refusal, since the role would otherwise share the
	// container's with its siblings.
	covered := map[string]bool{}
	for _, path := range []string{"/run/lock", "/var/lock", "/dev/shm"} {
		resolved, err := filepath.EvalSymlinks(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("give the role its own %s: %w", path, err)
		case covered[resolved]:
			continue
		}
		covered[resolved] = true
		if err := carryOver(resolved, stage, func() error { return mountTmpfs(resolved, "size=64m") }); err != nil {
			return fmt.Errorf("give the role its own %s: %w", path, err)
		}
	}
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("clear the role's stage: %w", err)
	}
	// The covers, last of the mounts made from under them: every role's directories go behind an
	// empty filesystem, this role's launcher token with them, and so does the container's view of
	// the home's volumes, which the role now sees at their own paths alone.
	for _, dir := range []string{cfg.RolesDir, cfg.HomeMounts} {
		if err := mountTmpfs(dir, "size=1m,mode=0755"); err != nil {
			return fmt.Errorf("cover %s: %w", dir, err)
		}
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount the role's own /proc: %w", err)
	}
	return nil
}

// privateHome gives the role a writable layer of its own over the image's home, then puts every
// volume the home carries — an issue pod's sessions and shared config home, an operator's mount —
// back at its own path on top. The container mounts those volumes under the home mounts
// directory rather than under the home itself, so the overlay's lower layer, the home, has no
// mount under it: Linux refuses an overlay whose lower layer has mounts under it that a more
// privileged namespace locked (clone_private_mount), and every mount the container carries is
// locked in the role's namespaces. A home that carries a mount anyway is refused, naming it, rather
// than overlaid on one kernel and hidden on another. What the role writes anywhere else under its
// home is its own: no sibling role and nothing in the container sees it.
func privateHome(cfg RoleInitConfig, homes, work string) error {
	if carried, err := mountsUnder(cfg.Home); err != nil {
		return err
	} else if len(carried) > 0 {
		return fmt.Errorf("the home %s carries mounts (%s): the container mounts what belongs under the home under %s instead",
			cfg.Home, strings.Join(carried, ", "), cfg.HomeMounts)
	}
	options := "lowerdir=" + cfg.Home + ",upperdir=" + filepath.Join(homes, "upper") + ",workdir=" + work
	if err := unix.Mount("overlay", cfg.Home, "overlay", 0, options); err != nil {
		return fmt.Errorf("give the role its own layer over %s: %w", cfg.Home, err)
	}
	mounts, err := mountsUnder(cfg.HomeMounts)
	if err != nil {
		return err
	}
	// A mount under another is carried by that one's recursive bind, so only the outermost of
	// each nest is bound; mountinfo lists a mount after the one it sits on.
	var bound []string
	for _, mount := range mounts {
		if slices.ContainsFunc(bound, func(outer string) bool { return strings.HasPrefix(mount, outer+"/") }) {
			continue
		}
		rel, err := filepath.Rel(cfg.HomeMounts, mount)
		if err != nil {
			return err
		}
		target := filepath.Join(cfg.Home, rel)
		if err := makeLike(mount, target); err != nil {
			return fmt.Errorf("make %s's place in the role's home: %w", target, err)
		}
		if err := bindMount(mount, target); err != nil {
			return fmt.Errorf("bind %s at %s: %w", mount, target, err)
		}
		bound = append(bound, mount)
	}
	return nil
}

// carryOver puts the role's own filesystem at dir (place) while keeping every mount strictly
// under dir — an operator's volume under /tmp, say — visible on top of it: each is held aside on
// the role's stage first and bound back at its own path after, since the new filesystem would
// otherwise hide it and what the role writes there would never reach the volume. What the role
// writes anywhere else under dir is its own: no sibling role and nothing in the container sees it.
// The home is not carried this way (privateHome): an overlay's lower layer may have no mount under
// it at all.
func carryOver(dir, stage string, place func() error) error {
	carried, err := mountsUnder(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	held, err := os.MkdirTemp(stage, "held-")
	if err != nil {
		return err
	}
	for index, mount := range carried {
		aside := filepath.Join(held, strconv.Itoa(index))
		if err := makeLike(mount, aside); err != nil {
			return fmt.Errorf("hold %s aside: %w", mount, err)
		}
		if err := bindMount(mount, aside); err != nil {
			return fmt.Errorf("hold %s aside: %w", mount, err)
		}
	}
	if err := place(); err != nil {
		return err
	}
	for index, mount := range carried {
		aside := filepath.Join(held, strconv.Itoa(index))
		if err := makeLike(aside, mount); err != nil {
			return fmt.Errorf("put %s back: %w", mount, err)
		}
		if err := bindMount(aside, mount); err != nil {
			return fmt.Errorf("put %s back: %w", mount, err)
		}
		if err := unix.Unmount(aside, unix.MNT_DETACH); err != nil {
			return fmt.Errorf("let go of %s's stand-in: %w", mount, err)
		}
	}
	return nil
}

// runRoleShim runs the generation's shim as this init's own child, in a nested user namespace as
// the image's unprivileged user with no capability outside it. role-init stays the role's PID 1:
// it forwards the launcher's stop (signals, held since role-init began) to every process of the
// role, reaps the orphans the role leaves behind, reports the shim's own wait status, and ends
// the namespace with its own exit.
func runRoleShim(cfg RoleInitConfig, signals <-chan os.Signal) error {
	cmd := exec.Command(cfg.Argv[0], cfg.Argv[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = cfg.Stdout, cfg.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:                    true,
		Cloneflags:                 syscall.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: roleUID, HostID: 0, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: roleUID, HostID: 0, Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("role-init: start the role's shim: %w", err)
	}
	ended := make(chan struct{})
	go func() { _ = cmd.Wait(); close(ended) }()
	for running := true; running; {
		select {
		case <-ended:
			running = false
		case received := <-signals:
			sig, ok := received.(syscall.Signal)
			switch {
			case !ok:
			case sig == syscall.SIGCHLD:
				if _, err := reapNamespace(cmd.Process.Pid); err != nil {
					roleLogf(cfg, "role-init: %v", err)
				}
			default:
				// Every process of the role, a detached one included: this is its PID 1.
				if err := signalGeneration(cmd.Process.Pid, sig); err != nil {
					roleLogf(cfg, "role-init: %v", err)
				}
			}
		}
	}
	reportStatus(cfg, cmd.ProcessState)
	// Nothing of the role outlives its report. The kernel ends a PID namespace with its init, and
	// this kill makes that certain before the launcher admits the role's next generation.
	_ = syscall.Kill(-1, syscall.SIGKILL)
	return nil
}

// reportStatus writes the shim's own wait status to the launcher's pipe, so the daemon is told
// the role's exit code and signal rather than its namespace init's.
func reportStatus(cfg RoleInitConfig, state *os.ProcessState) {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return
	}
	file := os.NewFile(uintptr(cfg.StatusFD), "role status")
	if file == nil {
		roleLogf(cfg, "role-init: no status pipe at descriptor %d", cfg.StatusFD)
		return
	}
	if _, err := fmt.Fprintf(file, "%d", int(status)); err != nil {
		roleLogf(cfg, "role-init: report the shim's status: %v", err)
	}
	_ = file.Close()
}

func roleLogf(cfg RoleInitConfig, format string, args ...any) {
	if cfg.Stderr == nil {
		return
	}
	fmt.Fprintf(cfg.Stderr, format+"\n", args...)
}

// mountsUnder are the mount points strictly under dir, in the order the namespace holds them, so
// a mount above another is bound back first.
func mountsUnder(dir string) ([]string, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read this namespace's mounts: %w", err)
	}
	defer file.Close()
	var under []string
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) < 5 {
			continue
		}
		point := unescapeMount(fields[4])
		if strings.HasPrefix(point, dir+string(filepath.Separator)) {
			under = append(under, point)
		}
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("read this namespace's mounts: %w", err)
	}
	return under, nil
}

// unescapeMount reads mountinfo's octal escapes for the characters it cannot write plainly.
func unescapeMount(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for index := 0; index < len(field); index++ {
		if field[index] == '\\' && index+3 < len(field) {
			if value, err := strconv.ParseUint(field[index+1:index+4], 8, 8); err == nil {
				out.WriteByte(byte(value))
				index += 3
				continue
			}
		}
		out.WriteByte(field[index])
	}
	return out.String()
}

// makeLike makes target a mount point of source's kind — a directory for a directory, an empty
// file for anything else — so a bind of source has somewhere to land. A file already there is
// the mount point itself, and is left as it is: it is never opened for writing.
func makeLike(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return file.Close()
}

// privateMounts holds every mount this namespace makes or drops to itself, so nothing it does
// propagates back to the container or to another role.
func privateMounts() error {
	return unix.Mount("none", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
}

func mountTmpfs(path, options string) error {
	return unix.Mount("tmpfs", path, "tmpfs", 0, options)
}

func bindMount(source, target string) error {
	return unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, "")
}
