//go:build linux

package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The three re-entrant helpers this test runs as processes of its own: the stage, which builds a
// fake container layout inside a mount namespace of its own and starts role-init in it; role-init
// itself, which the stage starts as the init of the role's new namespaces; and the agent, the
// stand-in worker shim role-init runs inside them, which reports what it can reach.
const (
	roleInitStageEnv = "LEGION_ROLE_INIT_TEST_STAGE"
	roleInitAgentEnv = "LEGION_ROLE_INIT_TEST_AGENT"
)

// stagePlan is what the stage builds and runs. Mount is the directory it covers with a memory
// filesystem of its own, so the layout it builds — and the role's own mounts over it — stay out
// of the machine's filesystem and out of the test process's view.
type stagePlan struct {
	Self     string
	Mount    string
	Role     string
	Sibling  string
	Owner    string
	Detached string
}

// agentPlan is what the agent reports on: the paths its role should and should not reach, the
// process the stage started outside its namespaces, and the detached, SIGTERM-ignoring process it
// leaves behind for the stage to look for once the namespaces are gone. Label is what its lines
// carry, so the stage's two generations read apart.
type agentPlan struct {
	Home       string
	RolesDir   string
	FixedRoot  string
	Role       string
	Sibling    string
	Owner      string
	Label      string
	Generation uint64
	OutsidePID int
	Detached   string
}

// TestRoleNamespaceProbeHelper is the probe itself: cloned into a role's namespaces, it does the
// first thing role-init does there. A machine may allow the namespaces and refuse the mounts.
func TestRoleNamespaceProbeHelper(t *testing.T) {
	dir := os.Getenv(roleProbeEnv)
	if dir == "" {
		return
	}
	if err := privateMounts(); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
	if err := mountTmpfs(dir, "size=1m"); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// Every start of a role runs in namespaces of its own. The role reads its own gh bundle and its
// generation's credentials at the fixed paths every role is told; a sibling role's directory and
// every launcher token, its own included, are gone; it sees and can signal only its own
// processes; what it writes under its home and in its temporary directories lands on its own
// layer, invisible to the container; every mount its home carried is still there; and when the
// role's init exits nothing of the role is left, not even a detached process that ignores
// SIGTERM.
func TestARoleRunsInNamespacesOfItsOwn(t *testing.T) {
	requireRoleNamespaces(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	plan := stagePlan{Self: self, Mount: stageMountPoint(t), Role: "a", Sibling: "b", Owner: "acme", Detached: "987654"}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	stage := exec.Command(self, "-test.run=^TestRoleInitStageHelper$")
	stage.Env = append(os.Environ(), roleInitStageEnv+"="+string(encoded))
	stage.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	output, err := stage.CombinedOutput()
	if err != nil {
		t.Fatalf("the stage: %v\n%s", err, output)
	}
	report := reported(string(output))
	for _, want := range []struct{ key, value, why string }{
		{"agent own-gh", "gh-a", "the role reads its own gh bundle at the fixed path every role is told"},
		{"agent own-boot-token", "boot-a", "the role reads its generation's credentials at the fixed path the daemon names"},
		{"agent own-state", "writable", "the role's own state directory is writable at the fixed path"},
		{"agent sibling", "absent", "no sibling role's directory is reachable"},
		{"agent own-launcher-token", "absent", "no launcher token is reachable, the role's own included"},
		{"agent roles-entries", "0", "the roles directory is covered by an empty filesystem"},
		{"agent uncover", "refused", "the role cannot uncover the roles directory from a namespace of its own"},
		{"agent uid", "1000", "the role's agent runs as the image's unprivileged user"},
		{"agent outside-process", "absent", "the role sees only its own processes"},
		{"agent signal-outside", "refused", "the role cannot signal a process outside it"},
		{"agent init-mem", "refused", "the role cannot read the memory of its own role-init, which holds the cover's namespace"},
		{"agent init-environ", "refused", "the role cannot read the environment its own role-init was started with"},
		{"agent init-status-fd", "refused", "the role cannot reach the launcher's status pipe through its own role-init"},
		{"agent own-agent-secrets", "present", "the role reads its own agent-secrets key at the fixed path"},
		{"agent own-agent-secrets-key", "key-a", "the key it reads there is its own"},
		{"agent home-mount", "mounted-a", "a mount the home carried is bound back over the role's overlay"},
		{"agent tmp-mount", "mounted-a", "a mount /tmp carried is bound back over the role's own /tmp"},
		{"agent home-carryover", "absent", "the role's first generation starts on a home layer with nothing of its own"},
		{"stage upper-home", "present", "what the role wrote under its home landed on its own layer"},
		{"stage upper-tmp", "present", "what the role wrote in /tmp landed in its own temporary directory"},
		{"stage upper-var-tmp", "present", "what the role wrote in /var/tmp landed in its own temporary directory"},
		{"stage container-home", "absent", "what the role wrote under its home is invisible to the container"},
		{"stage container-tmp", "absent", "what the role wrote in /tmp is invisible to the container"},
		{"stage container-shm", "absent", "what the role wrote in /dev/shm is invisible to the container"},
		{"stage detached", "gone", "a detached process of the role that ignores SIGTERM is gone once the role's init exits"},
		{"stage status", "0", "role-init reports the shim's own wait status"},
		// The role's relaunch: a second generation of the same role, over the same role-homes
		// volume, with no agent-secrets directory, as a pod not enrolled with the broker has it.
		{"agent2 own-gh", "gh-a", "the relaunched role reads its own gh bundle again"},
		{"agent2 own-agent-secrets", "absent", "a role whose pod is not enrolled starts with no agent-secrets directory"},
		{"agent2 sibling", "absent", "the relaunched role reaches no sibling"},
		{"agent2 init-mem", "refused", "the relaunched role cannot read its role-init either"},
		{"agent2 home-carryover", "present", "the relaunched role keeps its own home layer across generations"},
		{"agent2 tmp-mount", "mounted-a", "the relaunch keeps the mount /tmp carries"},
		{"stage2 status", "0", "the relaunch reports its own shim's wait status"},
		{"stage2 detached", "gone", "the relaunch leaves nothing behind either"},
	} {
		if got, ok := report[want.key]; !ok || got != want.value {
			t.Errorf("%s = %q, want %q: %s\n%s", want.key, got, want.value, want.why, output)
		}
	}
	// role-init and the agent, and nothing of the container's: the stage's own process, the
	// process it started outside, and the test binary that started the stage are all elsewhere.
	if pids, err := strconv.Atoi(report["agent pids"]); err != nil || pids < 2 || pids > 3 {
		t.Errorf("the role saw %q processes, want role-init and its own agent alone\n%s", report["agent pids"], output)
	}
}

// stageMountPoint is a directory the stage can cover with a memory filesystem of its own. It is
// the one piece of the machine the test needs: the layout must sit outside /tmp and outside the
// home a role's overlay covers, and must be a path the role's own mounts never hide.
func stageMountPoint(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{"/mnt", "/media", "/srv"} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	skipOutsideCI(t, "no directory to mount the role's layout on (/mnt, /media, /srv)")
	return ""
}

// reported is every `<who> <key>=<value>` line the stage and the agent printed.
func reported(output string) map[string]string {
	lines := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		who, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || (!strings.HasPrefix(who, "agent") && !strings.HasPrefix(who, "stage")) {
			continue
		}
		if key, value, ok := strings.Cut(rest, "="); ok {
			lines[who+" "+key] = value
		}
	}
	return lines
}

// TestRoleInitStageHelper is the container the role runs in: it builds a roles directory, a fixed
// root and a home inside a memory filesystem of its own, gives the home a mount of its own to
// carry, starts a process outside the role's namespaces, runs role-init over that layout, and
// reports what the container can see once the role is gone.
func TestRoleInitStageHelper(t *testing.T) {
	encoded := os.Getenv(roleInitStageEnv)
	if encoded == "" {
		return
	}
	var plan stagePlan
	if err := json.Unmarshal([]byte(encoded), &plan); err != nil {
		fmt.Println("stage error:", err)
		os.Exit(2)
	}
	if err := runStage(plan); err != nil {
		fmt.Println("stage error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runStage(plan stagePlan) error {
	if err := privateMounts(); err != nil {
		return err
	}
	if err := mountTmpfs(plan.Mount, "size=64m"); err != nil {
		return err
	}
	// The stage's own /tmp and /dev/shm, so what the role plants if its privacy broke lands here
	// and never on the machine, and the stage can see that it did not.
	for _, dir := range []string{"/tmp", "/dev/shm"} {
		if err := mountTmpfs(dir, "size=16m"); err != nil {
			return err
		}
	}
	rolesDir := filepath.Join(plan.Mount, "roles")
	fixedRoot := filepath.Join(plan.Mount, "fixed")
	home := filepath.Join(plan.Mount, "home")
	for _, role := range []string{plan.Role, plan.Sibling} {
		for _, name := range []string{roleLauncherDir, roleGHDir, rolePrivateDir, roleStateDir, roleAgentSecretsDir} {
			if err := os.MkdirAll(filepath.Join(rolesDir, role, name), 0o755); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Join(rolesDir, role, roleGHDir, plan.Owner), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(rolesDir, role, rolePrivateDir, "g1"), 0o700); err != nil {
			return err
		}
		for path, content := range map[string]string{
			filepath.Join(rolesDir, role, roleLauncherDir, launcherTokenFile):        "launcher-token-" + role,
			filepath.Join(rolesDir, role, roleGHDir, plan.Owner, "hosts.yml"):        "gh-" + role,
			filepath.Join(rolesDir, role, rolePrivateDir, "g1", "LEGION_BOOT_TOKEN"): "boot-" + role,
			filepath.Join(rolesDir, role, roleAgentSecretsDir, "key.pem"):            "key-" + role,
		} {
			if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{roleGHDir, rolePrivateDir, roleStateDir, roleAgentSecretsDir} {
		if err := os.MkdirAll(filepath.Join(fixedRoot, name), 0o755); err != nil {
			return err
		}
	}
	// A mount the home carries, as an issue pod's sessions and shared configuration do, and one
	// /tmp carries, as an operator's volume may: the role's own filesystems there must not hide
	// them.
	carried := filepath.Join(plan.Mount, "carried")
	if err := os.MkdirAll(carried, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(carried, "marker"), []byte("mounted-"+plan.Role), 0o644); err != nil {
		return err
	}
	for _, at := range []string{filepath.Join(home, "mounted"), "/tmp/mounted"} {
		if err := os.MkdirAll(at, 0o755); err != nil {
			return err
		}
		if err := bindMount(carried, at); err != nil {
			return err
		}
	}

	outside := exec.Command("/bin/sleep", "600")
	if err := outside.Start(); err != nil {
		return err
	}
	defer func() { _ = outside.Process.Kill(); _, _ = outside.Process.Wait() }()
	roleInitLaunch = roleInitHelperLaunch(plan.Self)
	layout := agentPlan{
		Home: home, RolesDir: rolesDir, FixedRoot: fixedRoot, Role: plan.Role, Sibling: plan.Sibling,
		Owner: plan.Owner, OutsidePID: outside.Process.Pid, Detached: plan.Detached,
	}

	if err := runGeneration(plan, layout, 1, "agent"); err != nil {
		return err
	}
	homes := filepath.Join(rolesDir, roleHomesDir, plan.Role)
	fmt.Printf("stage upper-home=%s\n", presence(filepath.Join(homes, "upper", "planted")))
	fmt.Printf("stage upper-tmp=%s\n", presence(filepath.Join(homes, "tmp", "planted")))
	fmt.Printf("stage upper-var-tmp=%s\n", presence(filepath.Join(homes, "var-tmp", "planted")))
	fmt.Printf("stage container-home=%s\n", presence(filepath.Join(home, "planted")))
	fmt.Printf("stage container-tmp=%s\n", presence(filepath.Join("/tmp", "planted")))
	fmt.Printf("stage container-shm=%s\n", presence(filepath.Join("/dev/shm", "planted")))
	fmt.Printf("stage detached=%s\n", detachedProcess(plan.Detached))

	// The role's relaunch: a second generation over the same role-homes volume, with the
	// agent-secrets directory gone, as a pod that is not enrolled with the secrets broker has it.
	if err := os.RemoveAll(filepath.Join(rolesDir, plan.Role, roleAgentSecretsDir)); err != nil {
		return err
	}
	layout.Detached = plan.Detached + "2"
	if err := runGeneration(plan, layout, 2, "agent2"); err != nil {
		return err
	}
	fmt.Printf("stage2 detached=%s\n", detachedProcess(layout.Detached))
	return nil
}

// runGeneration runs one generation of the role over the stage's layout and reports the wait
// status role-init gave back for it.
func runGeneration(plan stagePlan, layout agentPlan, generation uint64, label string) error {
	layout.Label = label
	layout.Generation = generation
	agent, err := json.Marshal(layout)
	if err != nil {
		return err
	}
	process, err := newRoleProcess(RoleInitConfig{
		RolesDir: layout.RolesDir, Role: plan.Role, Generation: generation, Home: layout.Home,
		FixedRoot: layout.FixedRoot, Argv: []string{plan.Self, "-test.run=^TestRoleInitAgentHelper$"},
	}, append(os.Environ(), roleInitAgentEnv+"="+string(agent)), os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if err := process.start(); err != nil {
		return err
	}
	status, _ := io.ReadAll(process.status)
	_ = process.status.Close()
	if err := process.cmd.Wait(); err != nil {
		return fmt.Errorf("role-init generation %d: %w", generation, err)
	}
	who := "stage"
	if generation > 1 {
		who = "stage2"
	}
	if reported := parseStatus(status); reported == nil {
		fmt.Printf("%s status=none\n", who)
	} else {
		fmt.Printf("%s status=%d\n", who, reported.ExitStatus())
	}
	return nil
}

// detachedProcess says whether any process of the machine still runs the detached command the
// role started, once the role's init has exited.
func detachedProcess(marker string) string {
	deadline := time.Now().Add(10 * time.Second)
	for {
		found := false
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return "unreadable: " + err.Error()
		}
		for _, entry := range entries {
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			body, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
			if err == nil && strings.Contains(string(body), marker) {
				found = true
				break
			}
		}
		if !found {
			return "gone"
		}
		if time.Now().After(deadline) {
			return "running"
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRoleInitAgentHelper is the worker shim's stand-in: the process role-init starts inside the
// role's namespaces, which reports what the role can reach and leaves a detached process behind.
// Every line it prints carries its generation's label, so the stage's two runs read apart.
func TestRoleInitAgentHelper(t *testing.T) {
	encoded := os.Getenv(roleInitAgentEnv)
	if encoded == "" {
		return
	}
	var plan agentPlan
	if err := json.Unmarshal([]byte(encoded), &plan); err != nil {
		fmt.Println("agent error:", err)
		os.Exit(2)
	}
	say := func(key, value string) { fmt.Printf("%s %s=%s\n", plan.Label, key, value) }
	say("uid", strconv.Itoa(os.Getuid()))
	say("own-gh", content(filepath.Join(plan.FixedRoot, roleGHDir, plan.Owner, "hosts.yml")))
	say("own-boot-token", content(filepath.Join(plan.FixedRoot, rolePrivateDir, "g1", "LEGION_BOOT_TOKEN")))
	say("own-state", writable(filepath.Join(plan.FixedRoot, roleStateDir)))
	// The agent-secrets directory is bound only when the pod is enrolled with the broker: its
	// key is the role's own in the first generation, and the second runs with none.
	say("own-agent-secrets", presence(filepath.Join(plan.FixedRoot, roleAgentSecretsDir, "key.pem")))
	say("own-agent-secrets-key", content(filepath.Join(plan.FixedRoot, roleAgentSecretsDir, "key.pem")))
	say("sibling", presence(filepath.Join(plan.RolesDir, plan.Sibling)))
	say("own-launcher-token", presence(filepath.Join(plan.RolesDir, plan.Role, roleLauncherDir, launcherTokenFile)))
	if entries, err := os.ReadDir(plan.RolesDir); err != nil {
		say("roles-entries", "unreadable: "+err.Error())
	} else {
		say("roles-entries", strconv.Itoa(len(entries)))
	}
	say("uncover", uncover(plan))
	say("pids", strconv.Itoa(processes()))
	say("outside-process", presence(filepath.Join("/proc", strconv.Itoa(plan.OutsidePID))))
	say("signal-outside", signalOutside(plan.OutsidePID))
	// Its own role-init, PID 1 of this namespace, holds CAP_SYS_ADMIN over the mount namespace
	// that carries the cover and the launcher's status pipe. The role runs in a user namespace
	// of its own, so the kernel refuses it every read of that process: its memory, the
	// environment its generation was started with, and the descriptors it holds.
	say("init-mem", readable("/proc/1/mem"))
	say("init-environ", readable("/proc/1/environ"))
	say("init-status-fd", readable("/proc/1/fd/"+strconv.Itoa(roleStatusFD)))
	say("home-mount", content(filepath.Join(plan.Home, "mounted", "marker")))
	say("tmp-mount", content(filepath.Join("/tmp", "mounted", "marker")))
	// What an earlier generation of this role wrote under its home is still the role's own: the
	// writable layer is the role's across its generations, as a role container's filesystem was.
	say("home-carryover", presence(filepath.Join(plan.Home, "planted")))
	for _, dir := range []string{plan.Home, "/tmp", "/var/tmp", "/dev/shm"} {
		if err := os.WriteFile(filepath.Join(dir, "planted"), []byte("planted"), 0o600); err != nil {
			say("write-"+dir, err.Error())
		}
	}
	detached := exec.Command("/bin/sh", "-c", "trap '' TERM; /bin/sleep "+plan.Detached)
	detached.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := detached.Start(); err != nil {
		say("detached-start", err.Error())
	}
	say("done", "yes")
	os.Exit(0)
}

// readable is what the role gets when it tries to read path: `refused` where the kernel denies
// it, which is what every probe of its own role-init must answer, and `readable` wherever it
// could open and reach the file at all. It never blocks: a descriptor of role-init's that is a
// pipe would otherwise hold the role here until its own init let go of the other end.
func readable(path string) string {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		var one [1]byte
		_, err = syscall.Read(fd, one[:])
		_ = syscall.Close(fd)
	}
	switch {
	case err == nil, errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EIO):
		return "readable"
	case errors.Is(err, fs.ErrPermission):
		return "refused"
	case errors.Is(err, fs.ErrNotExist):
		return "absent"
	default:
		return "unreadable: " + err.Error()
	}
}

// uncover is what a further namespace of the role's own can do to the cover over the roles
// directory: a nested user and mount namespace may not unmount it, so no sibling's credentials
// come back.
func uncover(plan agentPlan) string {
	script := "umount " + plan.RolesDir + " 2>/dev/null || umount -l " + plan.RolesDir + " 2>/dev/null"
	nested := exec.Command("/bin/sh", "-c", script)
	nested.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	if err := nested.Run(); err != nil {
		return "refused"
	}
	if _, err := os.Stat(filepath.Join(plan.RolesDir, plan.Sibling)); errors.Is(err, fs.ErrNotExist) {
		return "refused"
	}
	return "uncovered"
}

// processes is how many processes the role can see.
func processes() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	count := 0
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err == nil {
			count++
		}
	}
	return count
}

func signalOutside(pid int) string {
	if err := syscall.Kill(pid, 0); err != nil {
		return "refused"
	}
	return "delivered"
}

func presence(path string) string {
	switch _, err := os.Lstat(path); {
	case err == nil:
		return "present"
	case errors.Is(err, fs.ErrNotExist):
		return "absent"
	default:
		return "refused"
	}
}

func content(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return strings.TrimSpace(string(body))
}

func writable(dir string) string {
	path := filepath.Join(dir, "probe")
	if err := os.WriteFile(path, []byte("probe"), 0o600); err != nil {
		return "refused"
	}
	_ = os.Remove(path)
	return "writable"
}
