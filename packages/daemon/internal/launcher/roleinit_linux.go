//go:build linux

package launcher

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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
func roleInit(stop context.Context, cfg RoleInitConfig) error {
	if os.Getpid() != 1 {
		return errors.New("role-init: not the init of a PID namespace of its own")
	}
	// The kernel delivers a signal to a PID namespace's init only while it handles it. `legion`'s
	// main registers for the launcher's stop (SIGTERM, and SIGINT) before it runs any subcommand,
	// and ends stop on it, so a stop that arrives while the namespace is still being set up
	// reaches the shim the moment it starts, rather than waiting out the launcher's grace for a
	// kill. A stop before main runs misses it, in one of two ways. Before the Go runtime installs
	// its own handlers, in its first instructions, the kernel drops it: role-init goes on to start
	// the shim as though no stop came, and the launcher's SIGKILL at the end of its grace ends the
	// role. From then until main registers, during package initialization, the runtime's handler
	// finds no one registered and ends role-init (runtime/signal_unix.go, dieFromSignal: its
	// raise is ignored by an init, so it exits 128+15), which the launcher reports as that exit
	// rather than as a setup failure. Child exits arrive on a channel of their own; a dropped
	// SIGCHLD costs nothing, since one reap pass takes every exited orphan.
	exits := make(chan os.Signal, 1)
	signal.Notify(exits, syscall.SIGCHLD)
	defer signal.Stop(exits)
	// The shim never inherits the launcher's status pipe: role-init alone reports the role's exit.
	syscall.CloseOnExec(cfg.StatusFD)
	if err := prepareRoleNamespace(cfg); err != nil {
		return fmt.Errorf("role-init: %w", err)
	}
	return runRoleShim(stop, cfg, exits)
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
		if err := carryOver(cfg, bind[1], stage, func() error { return bindMount(bind[0], bind[1]) }); err != nil {
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
		if err := carryOver(cfg, resolved, stage, func() error { return mountTmpfs(resolved, "size=64m") }); err != nil {
			return fmt.Errorf("give the role its own %s: %w", path, err)
		}
	}
	if err := privateDevices(); err != nil {
		return err
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

// privateDevices gives the role its own pseudo-terminals and its own view of POSIX message queues
// where the image's /dev has them. A new devpts instance at /dev/pts: the container's lists, and
// lets the opener of a sibling's terminal read and write it, every terminal of every role, all
// the same user outside their namespaces; the new one holds only those the role opens, with
// /dev/ptmx opening into it (bound over a /dev/ptmx that is a node; a link to pts/ptmx already
// resolves there). The mqueue filesystem mounted at /dev/mqueue from inside the role's own IPC
// namespace shows that namespace's queues alone, where the container's mount showed — and let a
// role create — queues of the container's IPC namespace, a channel every sibling could read.
// Linux lets the owner of a user namespace mount both, and gVisor registers both as mountable
// there (runsc/boot/vfs.go: devpts and mqfs AllowUserMount; devpts takes newinstance, and a new
// IPC namespace gets its own POSIX queue registry, task_clone.go). ptmxmode 0666 is what container
// runtimes give /dev/ptmx, so the role's unprivileged agent can open it. A path the image does not
// have is skipped; any other failure is a refusal, since the role would otherwise share the
// container's with its siblings.
func privateDevices() error {
	if _, err := os.Stat("/dev/pts"); err == nil {
		if err := unix.Mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666"); err != nil {
			return fmt.Errorf("give the role its own /dev/pts: %w", err)
		}
		info, err := os.Lstat("/dev/ptmx")
		switch {
		case errors.Is(err, fs.ErrNotExist), err == nil && info.Mode()&fs.ModeSymlink != 0:
		case err != nil:
			return fmt.Errorf("give the role its own /dev/ptmx: %w", err)
		default:
			if err := unix.Mount("/dev/pts/ptmx", "/dev/ptmx", "", unix.MS_BIND, ""); err != nil {
				return fmt.Errorf("give the role its own /dev/ptmx: %w", err)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("give the role its own /dev/pts: %w", err)
	}
	if _, err := os.Stat("/dev/mqueue"); err == nil {
		if err := unix.Mount("mqueue", "/dev/mqueue", "mqueue", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return fmt.Errorf("give the role its own /dev/mqueue: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("give the role its own /dev/mqueue: %w", err)
	}
	return nil
}

// privateHome gives the role a writable layer of its own over the image's home, then puts every
// volume the home carries — the shared config home, an operator's mount — back at its
// own path on top. The container mounts those volumes under the home mounts directory rather than
// under the home itself, so the overlay's lower layer, the home, has no mount under it: Linux
// refuses an overlay whose lower layer has mounts under it that a more privileged namespace locked
// (clone_private_mount), and every mount the container carries is locked in the role's namespaces.
// A home that carries a mount anyway is refused, naming it, rather than overlaid on one kernel and
// hidden on another. What the role writes anywhere else under its home is its own: no sibling role
// and nothing in the container sees it. Each volume's place is made in the role's own layer
// without following a link the role left there (makeLike). The pod's shared config home is the
// one volume not bound back: the role's own config home goes at its path instead (ownConfigHome),
// and a mount under the shared one is bound back on top of the role's.
func privateHome(cfg RoleInitConfig, homes, work string) error {
	if _, carried, err := mountsUnder(cfg.Home); err != nil {
		return err
	} else if len(carried) > 0 {
		return fmt.Errorf("the home %s carries mounts (%s): the container mounts what belongs under the home under %s instead",
			cfg.Home, strings.Join(carried, ", "), cfg.HomeMounts)
	}
	options := "lowerdir=" + cfg.Home + ",upperdir=" + filepath.Join(homes, "upper") + ",workdir=" + work
	if err := unix.Mount("overlay", cfg.Home, "overlay", 0, options); err != nil {
		return fmt.Errorf("give the role its own layer over %s: %w", cfg.Home, err)
	}
	// The image reaches the home mounts directory through a link (its /var/run is /run), and
	// mountinfo names each mount by the path the link resolves to, so each is placed relative to
	// that resolved directory.
	homeMounts, mounts, err := mountsUnder(cfg.HomeMounts)
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
		rel, err := filepath.Rel(homeMounts, mount)
		if err != nil {
			return err
		}
		target := filepath.Join(cfg.Home, rel)
		source := mount
		if rel == ConfigHomeDir {
			if source, err = ownConfigHome(mount, filepath.Join(homes, RoleConfigHomeDir), mounts); err != nil {
				return fmt.Errorf("give the role its own config home: %w", err)
			}
		}
		if err := makeLike(cfg, source, cfg.Home, target); err != nil {
			return fmt.Errorf("make %s's place in the role's home: %w", target, err)
		}
		if err := bindMount(source, target); err != nil {
			return fmt.Errorf("bind %s at %s: %w", source, target, err)
		}
		if source == mount {
			bound = append(bound, mount)
		}
	}
	return nil
}

// ownConfigHome is the role's own config home, own, made the first time a generation of the role
// starts in the pod as a copy of the pod's shared one, shared, and the role's from then on: what
// workspace-init wrote there (Legion's jj configuration of the issue's clone, which jj keeps in
// the config home) is what the role's jj reads, and what the role writes there stays its own
// across its generations. A git `core.fsmonitor` or a jj alias one role sets in its config home
// would otherwise run in every sibling's git or jj, with the sibling's credentials. The copy
// leaves out every mount under the shared config home (among mounts, mountinfo's list), which
// privateHome binds back over the role's copy, and is made under a name of its own, finished with
// the marker file configHomeMarker, and renamed into place. An own that is not a directory
// holding that marker is no finished copy of role-init's — a generation that stopped midway, or a
// directory something else made at that path — and is removed and made again, so a role never
// starts without workspace-init's jj configuration. The marker is in the role's own config home,
// so a role that removes it has its config home copied afresh at its next generation.
func ownConfigHome(shared, own string, mounts []string) (string, error) {
	switch complete, err := configHomeComplete(own); {
	case err != nil:
		return "", err
	case complete:
		return own, nil
	}
	if err := os.RemoveAll(own); err != nil {
		return "", err
	}
	partial := own + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return "", err
	}
	err := filepath.WalkDir(shared, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != shared && slices.Contains(mounts, path) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(shared, path)
		if err != nil {
			return err
		}
		target := filepath.Join(partial, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.Mkdir(target, info.Mode().Perm())
		case entry.Type().IsRegular():
			return copyRegular(path, target, info.Mode().Perm())
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("%s is neither a directory, a regular file nor a link", path)
		}
	})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(partial, configHomeMarker), nil, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(partial, own); err != nil {
		return "", err
	}
	return own, nil
}

// configHomeMarker is the file ownConfigHome writes into a role's own config home last, before it
// renames the copy into place.
const configHomeMarker = ".legion-config-home-complete"

// configHomeComplete reports whether own is a finished copy of ownConfigHome's: a directory, not a
// link, holding the regular file configHomeMarker.
func configHomeComplete(own string) (bool, error) {
	info, err := os.Lstat(own)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !info.IsDir():
		return false, nil
	}
	marker, err := os.Lstat(filepath.Join(own, configHomeMarker))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return marker.Mode().IsRegular(), nil
}

// copyRegular copies the regular file source to target, which it creates with perm.
func copyRegular(source, target string, perm fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// carryOver puts the role's own filesystem at dir (place) while keeping every mount strictly
// under dir — an operator's volume under /tmp, say — visible on top of it: each is held aside on
// the role's stage first and bound back at its own path after, since the new filesystem would
// otherwise hide it and what the role writes there would never reach the volume. What the role
// writes anywhere else under dir is its own: no sibling role and nothing in the container sees it.
// The home is not carried this way (privateHome): an overlay's lower layer may have no mount under
// it at all.
func carryOver(cfg RoleInitConfig, dir, stage string, place func() error) error {
	root, carried, err := mountsUnder(dir)
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
		if err := makeLike(cfg, mount, held, aside); err != nil {
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
		if err := makeLike(cfg, aside, root, mount); err != nil {
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
// it forwards the launcher's stop (stop's end, held since role-init began) to every process of
// the role as SIGTERM, once, reaps the orphans the role leaves behind, reports the shim's own wait
// status, and ends the namespace with its own exit. The launcher's only other signal is SIGKILL,
// which ends role-init, and the namespace with it, without asking.
func runRoleShim(stop context.Context, cfg RoleInitConfig, exits <-chan os.Signal) error {
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
	stopped := stop.Done()
	for running := true; running; {
		select {
		case <-ended:
			running = false
		case <-exits:
			if _, err := reapNamespace(cmd.Process.Pid); err != nil {
				roleLogf(cfg, "role-init: %v", err)
			}
		case <-stopped:
			// Every process of the role, a detached one included: this is its PID 1.
			stopped = nil
			if err := signalGeneration(cmd.Process.Pid, syscall.SIGTERM); err != nil {
				roleLogf(cfg, "role-init: %v", err)
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
// a mount above another is bound back first, and the path dir resolves to, by which mountinfo
// names them: a dir reached through a link (the image's /var/run is a link to /run) is listed
// by its target, so dir as given would match none of its mounts.
func mountsUnder(dir string) (string, []string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", nil, fmt.Errorf("read this namespace's mounts: %w", err)
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
		if strings.HasPrefix(point, resolved+string(filepath.Separator)) {
			under = append(under, point)
		}
	}
	if err := lines.Err(); err != nil {
		return "", nil, fmt.Errorf("read this namespace's mounts: %w", err)
	}
	return resolved, under, nil
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

// makeLike makes target, a path under root, a mount point of source's kind — a directory for a
// directory, an empty file for anything else — so a bind of source has somewhere to land, and
// follows no link below root on the way (mountPointBeneath). A file already there is the mount
// point itself, and is left as it is: it is never opened for writing.
func makeLike(cfg RoleInitConfig, source, root, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	return mountPointBeneath(root, target, info.IsDir(), func(link string) {
		roleLogf(cfg, "role-init: %s was a link, which role-init does not follow on the way to a mount point: replaced it with the mount point's place", link)
	})
}

// mountPointBeneath makes target, a path under root, a mount point — a directory when dir is
// set, an empty file otherwise — following no link at or below root's entries. root is a directory
// role-init named itself (the home, a temporary directory, the stage); what lies below it is one
// of the role's own layers, which an earlier generation of the role wrote, so a link there could
// name any path the container shows, another role's directory under the roles directory among
// them, before the covers go on, and a walk that followed it would make directories there: an
// empty config home a sibling would then keep as its own. A link in the way is the role's own
// entry, and is removed and replaced by the directory or file the mount needs, with replaced told
// its path; any other entry in the way that is not a directory is refused. Nothing of the role
// runs while role-init does, and no other role reaches the role's layers, so the path the walk
// made is the path the bind that follows lands on.
func mountPointBeneath(root, target string, dir bool, replaced func(link string)) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s is not under %s", target, root)
	}
	if rel == "." {
		if !dir {
			return fmt.Errorf("%s is a directory, where a file's mount point goes", target)
		}
		return nil
	}
	parent, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", root, err)
	}
	defer func() { _ = unix.Close(parent) }()
	names := strings.Split(rel, string(filepath.Separator))
	path := root
	for index, name := range names {
		path = filepath.Join(path, name)
		file := !dir && index == len(names)-1
		var stat unix.Stat_t
		err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil && stat.Mode&unix.S_IFMT == unix.S_IFLNK {
			if err := unix.Unlinkat(parent, name, 0); err != nil {
				return fmt.Errorf("remove the link %s: %w", path, err)
			}
			replaced(path)
			err = unix.ENOENT
		}
		switch {
		case errors.Is(err, unix.ENOENT) && file:
			fd, err := unix.Openat(parent, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
			if err != nil {
				return fmt.Errorf("make %s: %w", path, err)
			}
			return unix.Close(fd)
		case errors.Is(err, unix.ENOENT):
			if err := unix.Mkdirat(parent, name, 0o755); err != nil {
				return fmt.Errorf("make %s: %w", path, err)
			}
		case err != nil:
			return fmt.Errorf("read %s: %w", path, err)
		case file:
			return nil
		case stat.Mode&unix.S_IFMT != unix.S_IFDIR:
			return fmt.Errorf("%s is neither a directory nor a link, where a mount point's directory goes", path)
		}
		next, err := unix.Openat(parent, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		_ = unix.Close(parent)
		parent = next
	}
	return nil
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
