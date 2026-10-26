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
	HomeMounts string
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
// layer, invisible to the container; every mount its home carried is still there; its config home
// is its own, a copy of the pod's shared one that no sibling reads; and when the role's init exits
// nothing of the role is left, not even a detached process that ignores SIGTERM.
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
	// The stage's own IPC namespace, as a container's, so the message queue it makes to stand for
	// the container's lands in no namespace of the machine's.
	stage.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC,
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
		{"agent home-mount", "mounted-a", "a volume the home carries is bound back at its own path over the role's overlay"},
		{"agent home-mount-nested", "nested-a", "a mount inside a volume the home carries comes back with it"},
		{"agent home-mount-deep", "mounted-a", "a volume deep under the home lands at its path, made in the role's layer"},
		{"agent home-mount-writes", "volume", "what the role writes into a volume under its home reaches the volume"},
		{"agent home-mounts-view", "0", "the container's view of the home's volumes is covered: the role sees each at its own path alone"},
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
		{"agent2 home-mount", "mounted-a", "the relaunch binds the home's volumes back again"},
		{"agent2 home-mount-deep", "mounted-a", "the relaunch finds the deep volume's place already made in its layer"},
		{"agent2 home-mounts-view", "0", "the relaunch covers the container's view of the home's volumes too"},
		{"stage2 status", "0", "the relaunch reports its own shim's wait status"},
		{"stage2 detached", "gone", "the relaunch leaves nothing behind either"},
		// The config home: each role's own, seeded from the pod's shared one.
		{"agent config-legion", "legion-jj", "the role's config home holds the jj configuration workspace-init wrote in the shared one"},
		{"agent config-nested", "nested-a", "a mount under the shared config home comes back over the role's own"},
		{"agent config-planted", "absent", "the role's first generation starts on a config home with nothing of its own"},
		{"agent2 config-planted", "present", "the relaunched role keeps what it wrote in its own config home"},
		{"agent2 config-legion", "legion-jj", "the relaunched role still reads workspace-init's jj configuration"},
		{"agent-sibling own-gh", "gh-b", "the sibling role reads its own gh bundle"},
		{"agent-sibling config-legion", "legion-jj", "the sibling's config home holds workspace-init's jj configuration too"},
		{"agent-sibling config-planted", "absent", "what one role wrote in its config home never reaches a sibling's"},
		{"agent-sibling config-nested", "nested-a", "the sibling sees the mount under the shared config home too"},
		{"stage-sibling status", "0", "the sibling's role-init reports its shim's wait status"},
		{"stage-sibling detached", "gone", "the sibling leaves nothing behind"},
		{"stage shared-config-planted", "absent", "no role writes the pod's shared config home"},
		{"stage own-config-planted", "present", "what the role wrote in its config home landed in its own directory"},
		// Pseudo-terminals: the role's /dev/pts is a new instance holding none of the container's.
		{"agent pts-entries", "0", "the role's /dev/pts holds none of the container's terminals"},
		{"agent pty", "opened", "the role's agent can open a terminal of its own"},
		{"agent pts-own", "1", "the terminal the role opened is in its own /dev/pts"},
		{"agent2 pts-entries", "0", "the relaunch's /dev/pts holds none of the container's terminals either"},
	} {
		if got, ok := report[want.key]; !ok || got != want.value {
			t.Errorf("%s = %q, want %q: %s\n%s", want.key, got, want.value, want.why, output)
		}
	}
	// POSIX message queues, where the machine has a /dev/mqueue: the role's shows its own IPC
	// namespace's queues, none of the container's, and the container never sees one it makes.
	if report["stage mqueue"] == "present" {
		for _, want := range []struct{ key, value, why string }{
			{"agent container-queue", "absent", "the role's /dev/mqueue shows none of the container's queues"},
			{"agent own-queue", "made", "the role can make a queue in its own /dev/mqueue"},
			{"stage role-queue", "absent", "the container's /dev/mqueue shows none of the role's queues"},
		} {
			if got := report[want.key]; got != want.value {
				t.Errorf("%s = %q, want %q: %s\n%s", want.key, got, want.value, want.why, output)
			}
		}
	} else if report["stage mqueue"] != "absent" {
		t.Errorf("stage mqueue = %q, want present or absent\n%s", report["stage mqueue"], output)
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
	if err := mountTmpfs(plan.Mount, "size=96m"); err != nil {
		return err
	}
	// The test binary runs from wherever `go test` built it, under /tmp when TMPDIR is unset, as
	// on GitHub's runners; the stage covers /tmp next, so every process it starts runs a copy of
	// it kept on the stage's own filesystem.
	self := filepath.Join(plan.Mount, "launcher.test")
	if err := copyExecutable(plan.Self, self); err != nil {
		return err
	}
	plan.Self = self
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
	// The home mounts directory is reached through a link, as the worker image's is (its /var/run
	// is a link to /run): mountinfo names every mount under it by the link's target, so role-init
	// must find them there.
	homeMounts := filepath.Join(plan.Mount, "home-mounts")
	if err := os.MkdirAll(filepath.Join(plan.Mount, "run", "home-mounts"), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(filepath.Join(plan.Mount, "run", "home-mounts"), homeMounts); err != nil {
		return err
	}
	for _, role := range []string{plan.Role, plan.Sibling} {
		for _, name := range []string{RoleLauncherDir, RoleGHDir, RolePrivateDir, RoleStateDir, RoleAgentSecretsDir} {
			if err := os.MkdirAll(filepath.Join(rolesDir, role, name), 0o755); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Join(rolesDir, role, RoleGHDir, plan.Owner), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(rolesDir, role, RolePrivateDir, "g1"), 0o700); err != nil {
			return err
		}
		for path, content := range map[string]string{
			filepath.Join(rolesDir, role, RoleLauncherDir, LauncherTokenFile):        "launcher-token-" + role,
			filepath.Join(rolesDir, role, RoleGHDir, plan.Owner, "hosts.yml"):        "gh-" + role,
			filepath.Join(rolesDir, role, RolePrivateDir, "g1", "LEGION_BOOT_TOKEN"): "boot-" + role,
			filepath.Join(rolesDir, role, RoleAgentSecretsDir, "key.pem"):            "key-" + role,
		} {
			if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{RoleGHDir, RolePrivateDir, RoleStateDir, RoleAgentSecretsDir} {
		if err := os.MkdirAll(filepath.Join(fixedRoot, name), 0o755); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	// The volumes the home carries, as an issue pod's sessions and operator mounts do: mounted
	// under the home mounts at their path relative to the home, as the pod builder renders them,
	// so the home itself carries none. One sits shallow and holds a mount of its own; one sits
	// deep, under directories the home does not have. A mount /tmp carries, as an operator's
	// volume may, is carried over the role's own /tmp. The pod's shared config home sits at its
	// own place, holding the jj configuration workspace-init writes there and a mount of its own.
	carried := filepath.Join(plan.Mount, "carried")
	nested := filepath.Join(plan.Mount, "nested")
	shared := filepath.Join(plan.Mount, "shared-config")
	for dir, marker := range map[string]string{carried: "mounted-" + plan.Role, nested: "nested-" + plan.Role} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(marker), 0o644); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(carried, "nested"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(shared, "jj", "repos", "id"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(shared, "jj", "repos", "id", "config.toml"), []byte("legion-jj"), 0o644); err != nil {
		return err
	}
	for _, bind := range [][2]string{
		{carried, filepath.Join(homeMounts, "mounted")},
		{nested, filepath.Join(homeMounts, "mounted", "nested")},
		{carried, filepath.Join(homeMounts, "deep", "sessions")},
		{carried, "/tmp/mounted"},
		{shared, filepath.Join(homeMounts, ConfigHomeDir)},
		{nested, filepath.Join(homeMounts, ConfigHomeDir, "nested")},
	} {
		if err := os.MkdirAll(bind[1], 0o755); err != nil {
			return err
		}
		if err := bindMount(bind[0], bind[1]); err != nil {
			return err
		}
	}

	outside := exec.Command("/bin/sleep", "600")
	if err := outside.Start(); err != nil {
		return err
	}
	defer func() { _ = outside.Process.Kill(); _, _ = outside.Process.Wait() }()
	// A terminal of the container's own, held open while the role runs, and, where the machine
	// has a /dev/mqueue, a message queue of the container's IPC namespace: neither may reach the
	// role, and nothing the role opens may reach the container.
	pty, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return err
	}
	defer pty.Close()
	mqueue := "absent"
	if _, err := os.Stat("/dev/mqueue"); err == nil {
		if err := syscall.Mount("mqueue", "/dev/mqueue", "mqueue", 0, ""); err != nil {
			return err
		}
		queue, err := os.OpenFile("/dev/mqueue/container-queue", os.O_RDONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_ = queue.Close()
		mqueue = "present"
	}
	fmt.Printf("stage mqueue=%s\n", mqueue)
	roleInitLaunch = roleInitHelperLaunch(plan.Self)
	layout := agentPlan{
		Home: home, HomeMounts: homeMounts, RolesDir: rolesDir, FixedRoot: fixedRoot, Role: plan.Role,
		Sibling: plan.Sibling, Owner: plan.Owner, OutsidePID: outside.Process.Pid, Detached: plan.Detached,
	}

	if err := runGeneration(plan, layout, 1, "agent"); err != nil {
		return err
	}
	homes := filepath.Join(rolesDir, RoleHomesDir, plan.Role)
	fmt.Printf("stage upper-home=%s\n", presence(filepath.Join(homes, "upper", "planted")))
	fmt.Printf("stage upper-tmp=%s\n", presence(filepath.Join(homes, "tmp", "planted")))
	fmt.Printf("stage upper-var-tmp=%s\n", presence(filepath.Join(homes, "var-tmp", "planted")))
	fmt.Printf("stage container-home=%s\n", presence(filepath.Join(home, "planted")))
	fmt.Printf("stage container-tmp=%s\n", presence(filepath.Join("/tmp", "planted")))
	fmt.Printf("stage container-shm=%s\n", presence(filepath.Join("/dev/shm", "planted")))
	fmt.Printf("stage detached=%s\n", detachedProcess(plan.Detached))

	// The role's relaunch: a second generation over the same role-homes volume, with the
	// agent-secrets directory gone, as a pod that is not enrolled with the secrets broker has it.
	if err := os.RemoveAll(filepath.Join(rolesDir, plan.Role, RoleAgentSecretsDir)); err != nil {
		return err
	}
	layout.Detached = plan.Detached + "2"
	if err := runGeneration(plan, layout, 2, "agent2"); err != nil {
		return err
	}
	fmt.Printf("stage2 detached=%s\n", detachedProcess(layout.Detached))

	// The sibling role's first generation, after the role's two have written their own config
	// home: what the role wrote there never reaches the sibling, nor the pod's shared one.
	siblingPlan := plan
	siblingPlan.Role, siblingPlan.Sibling = plan.Sibling, plan.Role
	siblingLayout := layout
	siblingLayout.Role, siblingLayout.Sibling, siblingLayout.Detached = plan.Sibling, plan.Role, plan.Detached+"3"
	if err := runGeneration(siblingPlan, siblingLayout, 1, "agent-sibling"); err != nil {
		return err
	}
	fmt.Printf("stage-sibling detached=%s\n", detachedProcess(siblingLayout.Detached))
	fmt.Printf("stage shared-config-planted=%s\n", presence(filepath.Join(shared, "git", "config")))
	fmt.Printf("stage own-config-planted=%s\n", presence(filepath.Join(homes, RoleConfigHomeDir, "git", "config")))
	if mqueue == "present" {
		fmt.Printf("stage role-queue=%s\n", presence("/dev/mqueue/role-queue"))
	}
	return nil
}

// copyExecutable copies the executable at source to target, which the stage then runs in its
// place.
func copyExecutable(source, target string) error {
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, body, 0o755)
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
		HomeMounts: layout.HomeMounts, FixedRoot: layout.FixedRoot,
		Argv: []string{plan.Self, "-test.run=^TestRoleInitAgentHelper$"},
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
	// The stage's line for a generation carries the agent's label with `stage` in place of
	// `agent`: stage, stage2, stage-sibling.
	who := "stage" + strings.TrimPrefix(label, "agent")
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
	say("own-gh", content(filepath.Join(plan.FixedRoot, RoleGHDir, plan.Owner, "hosts.yml")))
	say("own-boot-token", content(filepath.Join(plan.FixedRoot, RolePrivateDir, "g1", "LEGION_BOOT_TOKEN")))
	say("own-state", writable(filepath.Join(plan.FixedRoot, RoleStateDir)))
	// The agent-secrets directory is bound only when the pod is enrolled with the broker: its
	// key is the role's own in the first generation, and the second runs with none.
	say("own-agent-secrets", presence(filepath.Join(plan.FixedRoot, RoleAgentSecretsDir, "key.pem")))
	say("own-agent-secrets-key", content(filepath.Join(plan.FixedRoot, RoleAgentSecretsDir, "key.pem")))
	say("sibling", presence(filepath.Join(plan.RolesDir, plan.Sibling)))
	say("own-launcher-token", presence(filepath.Join(plan.RolesDir, plan.Role, RoleLauncherDir, LauncherTokenFile)))
	if entries, err := os.ReadDir(plan.RolesDir); err != nil {
		say("roles-entries", "unreadable: "+err.Error())
	} else {
		say("roles-entries", strconv.Itoa(len(entries)))
	}
	say("uncover", uncover(plan.RolesDir, plan.Sibling, syscall.CLONE_NEWUSER|syscall.CLONE_NEWNS))
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
	say("home-mount-nested", content(filepath.Join(plan.Home, "mounted", "nested", "marker")))
	say("home-mount-deep", content(filepath.Join(plan.Home, "deep", "sessions", "marker")))
	if err := os.WriteFile(filepath.Join(plan.Home, "mounted", "written-"+plan.Label), []byte("volume"), 0o600); err != nil {
		say("home-mount-writes", err.Error())
	} else {
		say("home-mount-writes", content(filepath.Join(plan.Home, "deep", "sessions", "written-"+plan.Label)))
	}
	if entries, err := os.ReadDir(plan.HomeMounts); err != nil {
		say("home-mounts-view", "unreadable: "+err.Error())
	} else {
		say("home-mounts-view", strconv.Itoa(len(entries)))
	}
	say("tmp-mount", content(filepath.Join("/tmp", "mounted", "marker")))
	// What an earlier generation of this role wrote under its home is still the role's own: the
	// writable layer is the role's across its generations, as a role container's filesystem was.
	say("home-carryover", presence(filepath.Join(plan.Home, "planted")))
	// The role's config home is its own: what workspace-init wrote in the pod's shared one is
	// there, a mount under the shared one comes back over it, and what this role's earlier
	// generations wrote there is still the role's, while a sibling's writes never are. A
	// planted git configuration stands for a `core.fsmonitor` that would run in the sibling's git.
	configHome := filepath.Join(plan.Home, ConfigHomeDir)
	say("config-legion", content(filepath.Join(configHome, "jj", "repos", "id", "config.toml")))
	say("config-nested", content(filepath.Join(configHome, "nested", "marker")))
	say("config-planted", presence(filepath.Join(configHome, "git", "config")))
	if err := os.MkdirAll(filepath.Join(configHome, "git"), 0o755); err != nil {
		say("config-write", err.Error())
	} else if err := os.WriteFile(filepath.Join(configHome, "git", "config"), []byte("[core]\n\tfsmonitor = /planted-by-"+plan.Label+"\n"), 0o600); err != nil {
		say("config-write", err.Error())
	}
	// The role's own terminals and message queues: none of the container's is there, and what
	// the role opens lands in its own.
	say("pts-entries", ptsEntries())
	if pty, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0); err != nil {
		say("pty", err.Error())
	} else {
		say("pty", "opened")
		say("pts-own", ptsEntries())
		_ = pty.Close()
	}
	say("container-queue", presence("/dev/mqueue/container-queue"))
	if queue, err := os.OpenFile("/dev/mqueue/role-queue", os.O_RDONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
		say("own-queue", err.Error())
	} else {
		say("own-queue", "made")
		_ = queue.Close()
	}
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

// uncover is what a further namespace of the role's own (cloneflags: a user and mount namespace,
// as an agent's `unshare -Urm` makes) can do to the cover over the roles directory. The nested
// shell tries to unmount it and then looks for the sibling's directory itself, in its own mount
// namespace, where an unmount would have taken effect: `refused` when the cover stayed and the
// sibling is out of reach, `uncovered` when the sibling came back, `unmounted` when the unmount
// succeeded though no sibling showed, and `no-namespace: …` when the nested namespace could not be
// made at all, which proves nothing about the cover.
func uncover(rolesDir, sibling string, cloneflags uintptr) string {
	script := `if umount "$1" 2>/dev/null || umount -l "$1" 2>/dev/null; then unmounted=yes; else unmounted=no; fi
if [ -e "$1/$2" ]; then echo uncovered; elif [ "$unmounted" = yes ]; then echo unmounted; else echo refused; fi`
	nested := exec.Command("/bin/sh", "-c", script, "sh", rolesDir, sibling)
	nested.SysProcAttr = &syscall.SysProcAttr{Cloneflags: cloneflags}
	if cloneflags&syscall.CLONE_NEWUSER != 0 {
		nested.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		nested.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	}
	output, err := nested.Output()
	if err != nil {
		return "no-namespace: " + err.Error()
	}
	return strings.TrimSpace(string(output))
}

// The uncover probe can fail: over a cover a further mount namespace may remove — one made in the
// same user namespace, so the kernel does not lock it, as the reviewer's reproduction had it — it
// answers `uncovered`, read in the namespace where the unmount happened. A probe that read the
// sibling from its caller's namespace, where the cover never left, answered `refused` here.
func TestTheUncoverProbeReportsAnUnlockedCoverRemoved(t *testing.T) {
	if dir := os.Getenv(uncoverControlEnv); dir != "" {
		if err := privateMounts(); err != nil {
			fmt.Println("control error:", err)
			os.Exit(1)
		}
		roles := filepath.Join(dir, "roles")
		if err := os.MkdirAll(filepath.Join(roles, "b"), 0o755); err != nil {
			fmt.Println("control error:", err)
			os.Exit(1)
		}
		if err := mountTmpfs(roles, "size=1m"); err != nil {
			fmt.Println("control error:", err)
			os.Exit(1)
		}
		fmt.Printf("control locked=%s\n", uncover(roles, "b", syscall.CLONE_NEWUSER|syscall.CLONE_NEWNS))
		fmt.Printf("control unlocked=%s\n", uncover(roles, "b", syscall.CLONE_NEWNS))
		os.Exit(0)
	}
	requireRoleNamespaces(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	control := exec.Command(self, "-test.run=^TestTheUncoverProbeReportsAnUnlockedCoverRemoved$")
	control.Env = append(os.Environ(), uncoverControlEnv+"="+t.TempDir())
	control.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	output, err := control.CombinedOutput()
	if err != nil {
		t.Fatalf("the control: %v\n%s", err, output)
	}
	report := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "control "), "="); ok {
			report[key] = value
		}
	}
	if report["unlocked"] != "uncovered" {
		t.Errorf("the probe over a cover the nested namespace may remove = %q, want uncovered\n%s", report["unlocked"], output)
	}
	if report["locked"] != "refused" {
		t.Errorf("the probe over a cover a nested user namespace holds locked = %q, want refused\n%s", report["locked"], output)
	}
}

// uncoverControlEnv names the directory TestTheUncoverProbeReportsAnUnlockedCoverRemoved's
// re-entered control builds its cover in.
const uncoverControlEnv = "LEGION_ROLE_INIT_TEST_UNCOVER"

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

// ptsEntries is how many terminals /dev/pts lists, its ptmx aside.
func ptsEntries() string {
	entries, err := os.ReadDir("/dev/pts")
	if err != nil {
		return "unreadable: " + err.Error()
	}
	count := 0
	for _, entry := range entries {
		if entry.Name() != "ptmx" {
			count++
		}
	}
	return strconv.Itoa(count)
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

// roleCheckEnv names the directory TestTheRoleNamespaceCheckRunsARoleGeneration's re-entered
// helper lays the check's layouts out under.
const roleCheckEnv = "LEGION_ROLE_INIT_TEST_CHECK"

// The image probe's role-namespace check runs a whole role generation: it passes where role-init
// sets the namespaces up and its shim exits 0, and refuses, naming the shim's exit, where the shim
// fails. Its layout sits on a memory filesystem of the helper's own outside /tmp, as the probe
// pod's sits on a volume of its own: role-init covers the roles directory after it has given the
// role its own /tmp.
func TestTheRoleNamespaceCheckRunsARoleGeneration(t *testing.T) {
	if dir := os.Getenv(roleCheckEnv); dir != "" {
		self, err := os.Executable()
		if err == nil {
			err = privateMounts()
		}
		if err == nil {
			err = mountTmpfs(dir, "size=16m")
		}
		if err != nil {
			fmt.Println("check error:", err)
			os.Exit(1)
		}
		roleInitLaunch = roleInitHelperLaunch(self)
		answer := func(err error) string {
			if err == nil {
				return "passed"
			}
			return strings.ReplaceAll(err.Error(), "\n", " ")
		}
		fmt.Printf("check clean=%s\n", answer(CheckRoleNamespaces(filepath.Join(dir, "clean"), "/bin/true")))
		fmt.Printf("check failing=%s\n", answer(CheckRoleNamespaces(filepath.Join(dir, "failing"), "/bin/false")))
		os.Exit(0)
	}
	requireRoleNamespaces(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	check := exec.Command(self, "-test.run=^TestTheRoleNamespaceCheckRunsARoleGeneration$")
	check.Env = append(os.Environ(), roleCheckEnv+"="+stageMountPoint(t))
	check.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	output, err := check.CombinedOutput()
	if err != nil {
		t.Fatalf("the check's helper: %v\n%s", err, output)
	}
	report := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "check "), "="); ok {
			report[key] = value
		}
	}
	if report["clean"] != "passed" {
		t.Errorf("the check over a shim that exits 0 = %q, want passed\n%s", report["clean"], output)
	}
	if got := report["failing"]; !strings.Contains(got, "did not run its shim to a clean exit") || !strings.Contains(got, "Code:1") {
		t.Errorf("the check over a shim that exits 1 = %q, want a refusal naming exit 1\n%s", got, output)
	}
}
