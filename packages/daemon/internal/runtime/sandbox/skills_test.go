package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// testSkillsRepository is the skills marketplace a test's deployment names
// (runtime.kubernetes.skills_repository).
func testSkillsRepository(t *testing.T) ghrepo.Repository {
	t.Helper()
	repository, err := ghrepo.Parse("skills_repository", "theorem-labs/verification-skills")
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

// skillsRuntime is a runtime of goldenOptions naming the skills marketplace.
func skillsRuntime(t *testing.T) *Runtime {
	t.Helper()
	opts := goldenOptions()
	opts.SkillsRepository = testSkillsRepository(t)
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A deployment that names a skills repository gives every issue pod a third init container,
// skills-fetch, which clones that repository into the pod's own skills volume: `legion
// workspace-init skills` of the image, the pod's one reservation, the restricted security context
// every Legion container carries, and exactly three mounts — the provisioning Secret read-only, its
// own in-memory TMPDIR, and the skills volume read-write. It runs between workspace-fetch and
// workspace-init, so a pod whose skills cannot be fetched fails before workspace-init spends
// minutes on the issue's own clone.
func TestASkillsRepositoryAddsTheSkillsInitContainer(t *testing.T) {
	r := skillsRuntime(t)
	l, err := r.prepare(workerSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	pod := r.podTemplate(l).Spec
	names := make([]string, 0, len(pod.InitContainers))
	for _, c := range pod.InitContainers {
		names = append(names, c.Name)
	}
	if want := []string{fetchContainer, skillsContainer, initContainer}; !slices.Equal(names, want) {
		t.Fatalf("the issue pod's init containers are %q, want %q", names, want)
	}
	skills := containerNamed(t, pod, skillsContainer)
	wantCommand := []string{r.tools.Legion, "workspace-init", "skills", "--repo", "theorem-labs/verification-skills", "--dir", SkillsDir}
	if !slices.Equal(skills.Command, wantCommand) {
		t.Errorf("%s runs %q, want %q", skillsContainer, skills.Command, wantCommand)
	}
	if skills.Image != r.image {
		t.Errorf("%s runs image %q, want the worker image %q", skillsContainer, skills.Image, r.image)
	}
	if got, want := skills.Resources, (issuePod{}).resources(r); !sameRequirements(got, want) {
		t.Errorf("%s carries %+v, want the issue pod's one reservation %+v", skillsContainer, got, want)
	}
	if got, want := skills.SecurityContext, restrictedContainer(); !reflect.DeepEqual(got, want) {
		t.Errorf("%s carries security context %+v, want %+v", skillsContainer, got, want)
	}
	env := envOf(skills)
	for name, want := range map[string]string{
		"PATH":                        imagePath,
		"TMPDIR":                      initTempDir,
		"LEGION_PROVISION_TOKEN_FILE": ProvisionDir + "/" + provisionTokenKey,
	} {
		if env[name] != want {
			t.Errorf("%s is told %s=%q, want %q", skillsContainer, name, env[name], want)
		}
	}
	// Its TMPDIR and its token are the paths it actually mounts, not two strings that happen to
	// agree with the constants: a pointer at an unmounted path is a container that cannot run.
	if dir := env["TMPDIR"]; !slices.ContainsFunc(skills.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == skillsTempVolume && m.MountPath == dir }) {
		t.Errorf("%s's TMPDIR %q is not where it mounts %s", skillsContainer, dir, skillsTempVolume)
	}
	if file := env["LEGION_PROVISION_TOKEN_FILE"]; !slices.ContainsFunc(skills.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == provisionVolume && strings.HasPrefix(file, m.MountPath+"/")
	}) {
		t.Errorf("%s's provisioning token %q is not under where it mounts %s", skillsContainer, file, provisionVolume)
	}
	// It is told nothing else: no repository cache, no config home, no XDG directory.
	if len(env) != 3 {
		t.Errorf("%s is told %v, want its PATH, its TMPDIR and the provisioning token alone", skillsContainer, env)
	}
	wantMounts := []corev1.VolumeMount{
		{Name: provisionVolume, MountPath: ProvisionDir, ReadOnly: true},
		{Name: skillsTempVolume, MountPath: initTempDir},
		{Name: skillsVolume, MountPath: SkillsDir},
	}
	if !reflect.DeepEqual(skills.VolumeMounts, wantMounts) {
		t.Errorf("%s mounts %+v, want %+v", skillsContainer, skills.VolumeMounts, wantMounts)
	}
	volumes := map[string]corev1.Volume{}
	for _, volume := range pod.Volumes {
		volumes[volume.Name] = volume
	}
	if dir := volumes[skillsVolume].EmptyDir; dir == nil || dir.Medium != corev1.StorageMediumDefault {
		t.Errorf("the skills volume is %+v, want an emptyDir on the node's disk", volumes[skillsVolume].VolumeSource)
	}
	if dir := volumes[skillsTempVolume].EmptyDir; dir == nil || dir.Medium != corev1.StorageMediumMemory {
		t.Errorf("%s's TMPDIR volume is %+v, want an in-memory emptyDir", skillsContainer, volumes[skillsTempVolume].VolumeSource)
	}
}

// A deployment that names no skills repository runs the two init containers it always ran, carries
// neither skills volume, and renders no skills step into a role's start wrapper: nothing of this
// reaches a deployment that did not ask for it.
func TestNoSkillsRepositoryLeavesThePodAsItWas(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	l, err := r.prepare(workerSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	pod := r.podTemplate(l).Spec
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		if c.Name == skillsContainer {
			t.Fatalf("a deployment with no skills repository runs %s", skillsContainer)
		}
		for _, mount := range c.VolumeMounts {
			if mount.Name == skillsVolume || mount.Name == skillsTempVolume {
				t.Errorf("%s mounts %s without a skills repository", c.Name, mount.Name)
			}
		}
	}
	for _, volume := range pod.Volumes {
		if volume.Name == skillsVolume || volume.Name == skillsTempVolume {
			t.Errorf("the pod carries volume %s without a skills repository", volume.Name)
		}
	}
	if install := skillsInstallation(r.skills, r.tools.Legion); install != nil {
		t.Errorf("skillsInstall() = %+v without a skills repository, want none", install)
	}
}

// A role of a pod with a skills volume installs from that volume and from nothing else: the start
// wrapper's skills step adds the clone as a *local* marketplace, by the path the init container
// cloned it to, and installs every plugin the clone's own record names. It names no `owner/repo`
// and runs no git, because a clone in a role's launch context ends the pod's gVisor sandbox
// (dispatch://INFRA-1262); a local marketplace has no remote, so neither this step nor Oh My Pi's
// own refresh of a stale marketplace fetches anything.
func TestARolesStartWrapperInstallsTheSkillsFromThePodsVolume(t *testing.T) {
	r := skillsRuntime(t)
	install := skillsInstallation(r.skills, r.tools.Legion)
	if install == nil {
		t.Fatal("skillsInstall() = nil with a skills repository")
	}
	wantDir := SkillsDir + "/repo"
	if install.Dir != wantDir {
		t.Errorf("the skills clone is %q, want %q", install.Dir, wantDir)
	}
	// The clone's directory does not carry the repository's name, so a repository called COMMIT,
	// MARKETPLACE or PLUGINS — each of which `ghrepo.Parse` admits — cannot land on a record's
	// path, and changing the repository changes no path a role's wrapper names.
	if strings.Contains(install.Dir, r.skills.Name()) {
		t.Errorf("the skills clone %q is named for the repository, so a repository named for a record would collide with it", install.Dir)
	}
	if install.MarketplaceFile != SkillsDir+"/MARKETPLACE" || install.PluginsFile != SkillsDir+"/PLUGINS" {
		t.Errorf("the records are %q and %q, want them beside the clone under %s", install.MarketplaceFile, install.PluginsFile, SkillsDir)
	}
	script := startScript("/home/legion/.local/state/tester", 3, nil, "", install, installLockWait(2*time.Second, 3))
	for _, want := range []string{
		"'" + r.tools.Legion + "' skills-check --dir '" + SkillsDir + "' --omp '" + defaultAgent + "'",
		// The apostrophe in the rendered line is shell-quoted, so match the part that has none.
		"skills clone in " + wantDir + " recorded; not starting Oh My Pi",
		"'" + defaultAgent + "' plugin marketplace add '" + wantDir + "'",
		"'" + defaultAgent + "' plugin marketplace remove \"$market\"",
		"'" + defaultAgent + "' plugin install \"$plugin@$market\" --force",
		"market=$(cat '" + install.MarketplaceFile + "')",
		"done < '" + install.PluginsFile + "'",
		"legion skills: the skills marketplace in " + wantDir + " is not installable",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the start wrapper does not hold %q:\n%s", want, script)
		}
	}
	// Nothing in it reaches GitHub: the whole point of the arrangement.
	for _, never := range []string{"github.com", "marketplace add theorem-labs/", " git ", "git clone"} {
		if strings.Contains(script, never) {
			t.Errorf("the start wrapper holds %q, which would fetch in the role's launch context:\n%s", never, script)
		}
	}
	// The check runs after the last install and before the setup command: a setup that greps the
	// plugin list must not run before the launch has proved what that list holds.
	lastInstall := strings.LastIndex(script, "plugin install")
	checkAt := strings.Index(script, "skills-check")
	setupAt := strings.Index(script, "runtime.kubernetes.setup")
	if lastInstall < 0 || checkAt < 0 || setupAt < 0 || lastInstall > checkAt || checkAt > setupAt {
		t.Errorf("the skills check is not between the install and the setup command (install %d, check %d, setup %d):\n%s", lastInstall, checkAt, setupAt, script)
	}
	// The install runs before the deployment's setup command, so a setup may check it.
	if add, setup := strings.Index(script, "plugin marketplace add"), strings.Index(script, "runtime.kubernetes.setup"); add < 0 || setup < 0 || add > setup {
		t.Errorf("the skills install is not before the setup command:\n%s", script)
	}
}

// The layout the init container writes and the layout a role's start wrapper reads are one
// definition: the runtime renders workspace's own paths, so a change to either side moves both.
func TestTheSkillsLayoutIsTheWorkspacePackages(t *testing.T) {
	r := skillsRuntime(t)
	install := skillsInstallation(r.skills, r.tools.Legion)
	if install == nil {
		t.Fatal("skillsInstall() = nil with a skills repository")
	}
	if want := workspace.SkillsClone(SkillsDir, r.skills); install.Dir != want {
		t.Errorf("the clone is %q, want workspace.SkillsClone's %q", install.Dir, want)
	}
	if want := workspace.SkillsMarketplaceFile(SkillsDir); install.MarketplaceFile != want {
		t.Errorf("the marketplace record is %q, want %q", install.MarketplaceFile, want)
	}
	if want := workspace.SkillsPluginsFile(SkillsDir); install.PluginsFile != want {
		t.Errorf("the plugins record is %q, want %q", install.PluginsFile, want)
	}
	// SkillsDir is outside the two directories a role's namespaces cover, so every role of the pod
	// reads the volume at this one path (`legion launcher role-init`).
	for _, covered := range []string{RolesDir, HomeMountsDir} {
		if strings.HasPrefix(SkillsDir, covered+"/") || SkillsDir == covered {
			t.Errorf("SkillsDir %s is under %s, which a role's namespaces cover", SkillsDir, covered)
		}
	}
}

// runSkillsStart executes the actual /bin/sh wrapper with only the image's omp replaced. The
// command double may read stdin just as an installer can, while setup and the harness still use
// the wrapper's real positional parameters and frame stream.
func runSkillsStart(t *testing.T, dir string, install *skillsInstall, ompBody string, setup, agent []string) startRun {
	t.Helper()
	return runSkillsStartChecking(t, dir, install, ompBody, 0, setup, agent)
}

// runSkillsStartChecking is runSkillsStart with the status the stand-in `legion skills-check`
// exits with: 0 for a check that found the installed files to be the pod's recorded commit, and
// anything else for one that did not. Its own argv goes to the command log beside omp's, so a test
// can see where in the order the check ran.
func runSkillsStartChecking(t *testing.T, dir string, install *skillsInstall, ompBody string, checkExit int, setup, agent []string) startRun {
	t.Helper()
	stub := filepath.Join(dir, "omp stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nset -eu\n"+ompBody), 0o700); err != nil {
		t.Fatal(err)
	}
	// The check reads no stdin of the wrapper's either, which this proves the same way the omp
	// stand-in does.
	legion := install.Legion
	check := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$SKILLS_LOG\"\ninput=$(cat)\n" +
		"[ -z \"$input\" ] || { echo \"check read: $input\" >&2; exit 9; }\necho check-output\nexit " + strconv.Itoa(checkExit) + "\n"
	if err := os.WriteFile(legion, []byte(check), 0o700); err != nil {
		t.Fatal(err)
	}
	argv := startArgv(filepath.Join(dir, "state"), 3, nil, "", install, setup, agent, 1)
	argv[2] = strings.ReplaceAll(argv[2], shellQuote(defaultAgent), shellQuote(stub))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "SKILLS_LOG="+filepath.Join(dir, "commands"))
	cmd.Stdin = strings.NewReader("frame\n")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return startRun{0, stdout.String(), stderr.String()}
	case errors.As(err, &exit):
		return startRun{exit.ExitCode(), stdout.String(), stderr.String()}
	default:
		t.Fatalf("run skills wrapper: %v", err)
		return startRun{}
	}
}

func skillsFixture(t *testing.T, dir, market, plugins string) *skillsInstall {
	t.Helper()
	install := &skillsInstall{
		Dir: filepath.Join(dir, "local clone"), MarketplaceFile: filepath.Join(dir, "MARKETPLACE"),
		PluginsFile: filepath.Join(dir, "PLUGINS"), Root: dir, Legion: filepath.Join(dir, "legion stub"),
	}
	if err := os.MkdirAll(install.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{install.MarketplaceFile: market, install.PluginsFile: plugins} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return install
}

// An omp command must consume neither an RPC frame nor the next plugin record. The second plugin
// has no final newline deliberately: a truncated record must still be installed, not skipped.
func TestSkillsCommandsKeepTheHarnessFramesAndEveryPlugin(t *testing.T) {
	dir := t.TempDir()
	install := skillsFixture(t, dir, "skills\n", "one\ntwo")
	body := `printf '%s\n' "$*" >>"$SKILLS_LOG"
input=$(cat)
[ -z "$input" ] || { echo "installer read: $input" >&2; exit 9; }
echo installer-output
`
	setupLog, agentLog := filepath.Join(dir, "setup"), filepath.Join(dir, "agent")
	got := runSkillsStart(t, dir, install, body, recorder(setupLog, "setup-output", "a b", "$HOME", "*"), recorder(agentLog, "agent-output", "--mode", "rpc", "it's"))
	if got.exit != 0 {
		t.Fatalf("skills wrapper exited %d: %s", got.exit, got.stderr)
	}
	wantCommands := []string{
		"plugin marketplace add " + install.Dir,
		"plugin install one@skills --force", "plugin enable one@skills",
		"plugin install two@skills --force", "plugin enable two@skills",
		"skills-check --dir " + install.Root + " --omp " + filepath.Join(dir, "omp stub"),
	}
	if commands := readLines(t, filepath.Join(dir, "commands")); !slices.Equal(commands, wantCommands) {
		t.Errorf("successful add or plugin order was wrong: %q, want %q", commands, wantCommands)
	}
	if got.stdout != "agent-output\n" || !strings.Contains(got.stderr, "installer-output") {
		t.Errorf("installer output crossed the RPC stream: stdout=%q stderr=%q", got.stdout, got.stderr)
	}
	if got, want := readLines(t, setupLog), []string{dir, "arg a b", "arg $HOME", "arg *", "stdin "}; !slices.Equal(got, want) {
		t.Errorf("setup received %q, want %q", got, want)
	}
	if got, want := readLines(t, agentLog), []string{dir, "arg --mode", "arg rpc", "arg it's", "stdin frame"}; !slices.Equal(got, want) {
		t.Errorf("harness received %q, want %q", got, want)
	}
}

// A kept role may already have registered this name from GitHub. Updating that record would
// clone in the role's launch context; replacing it with the local source never does.
func TestSkillsRelaunchRebindsAnExistingMarketplaceLocally(t *testing.T) {
	dir := t.TempDir()
	install := skillsFixture(t, dir, "skills\n", "one\n")
	body := `printf '%s\n' "$*" >>"$SKILLS_LOG"
case "$1 $2 $3" in
  'plugin marketplace add')
    [ -f "$SKILLS_LOG.registered" ] || { touch "$SKILLS_LOG.registered"; exit 1; } ;;
  'plugin marketplace update') echo 'would clone the old remote' >&2; exit 8 ;;
esac
`
	got := runSkillsStart(t, dir, install, body, nil, []string{"/bin/true"})
	if got.exit != 0 {
		t.Fatalf("relaunch could not register the local clone: exit %d: %s", got.exit, got.stderr)
	}
	want := []string{
		"plugin marketplace add " + install.Dir,
		"plugin marketplace remove skills",
		"plugin marketplace add " + install.Dir,
		"plugin install one@skills --force", "plugin enable one@skills",
		"skills-check --dir " + install.Root + " --omp " + filepath.Join(dir, "omp stub"),
	}
	if commands := readLines(t, filepath.Join(dir, "commands")); !slices.Equal(commands, want) {
		t.Errorf("relaunch did not rebind locally in order: %q, want %q", commands, want)
	}
}

// A check that finds the installed files are not the pod's recorded commit stops the wrapper
// before the setup command and before the harness, with its own line: a role whose loaded bytes
// nobody can account for must not start (dispatch://INFRA-1300, R4). Its passing arm is the
// wrapper's ordinary path, which every other executable test here already runs.
func TestSkillsCheckFailureNeverStartsSetupOrTheHarness(t *testing.T) {
	dir := t.TempDir()
	install := skillsFixture(t, dir, "skills\n", "one\n")
	setupLog, agentLog := filepath.Join(dir, "setup"), filepath.Join(dir, "agent")
	got := runSkillsStartChecking(t, dir, install, "printf '%s\\n' \"$*\" >>\"$SKILLS_LOG\"\n", 1,
		recorder(setupLog, "setup-output"), recorder(agentLog, "agent-output"))
	if got.exit == 0 {
		t.Fatalf("the wrapper exited 0 after a failed skills check: stdout %q, stderr %q", got.stdout, got.stderr)
	}
	if !strings.Contains(got.stderr, "is not the commit this pod") || !strings.Contains(got.stderr, "not starting Oh My Pi") {
		t.Errorf("stderr %q does not name the failed check", got.stderr)
	}
	for _, log := range []string{setupLog, agentLog} {
		if _, err := os.Stat(log); err == nil {
			t.Errorf("%s ran after a failed skills check", filepath.Base(log))
		}
	}
	if got.stdout != "" {
		t.Errorf("a failed check wrote %q to the RPC frame stream", got.stdout)
	}
	// The check ran last, after the install it is checking.
	commands := readLines(t, filepath.Join(dir, "commands"))
	if len(commands) == 0 || !strings.HasPrefix(commands[len(commands)-1], "skills-check ") {
		t.Errorf("the check was not the wrapper's last skills command: %q", commands)
	}
}

// Empty or unreadable records and any failed marketplace or install command must stop the
// wrapper before setup and before the harness; an empty while loop is not an installation.
func TestSkillsFailuresNeverStartSetupOrTheHarness(t *testing.T) {
	for name, tc := range map[string]struct{ market, plugins, body string }{
		"empty marketplace":         {"", "one\n", "exit 0\n"},
		"empty plugins":             {"skills\n", "", "exit 0\n"},
		"blank plugins":             {"skills\n", "\n\n", "exit 0\n"},
		"unreadable plugins":        {"skills\n", "one\n", "exit 0\n"},
		"marketplace commands fail": {"skills\n", "one\n", "exit 7\n"},
		"second add fails":          {"skills\n", "one\n", "[ \"$1 $2 $3\" != 'plugin marketplace add' ]\n"},
		"enable fails":              {"skills\n", "one\n", "[ \"$1 $2\" != 'plugin enable' ]\n"},
		"second install fails":      {"skills\n", "one\ntwo\n", "[ \"$1 $2 $3\" != 'plugin install two@skills' ]\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			install := skillsFixture(t, dir, tc.market, tc.plugins)
			if name == "unreadable plugins" {
				if err := os.Remove(install.PluginsFile); err != nil {
					t.Fatal(err)
				}
			}
			setupLog, agentLog := filepath.Join(dir, "setup"), filepath.Join(dir, "agent")
			body := "printf '%s\\n' \"$*\" >>\"$SKILLS_LOG\"\n" + tc.body
			got := runSkillsStart(t, dir, install, body, recorder(setupLog, "setup"), recorder(agentLog, "agent"))
			if got.exit == 0 || !strings.Contains(got.stderr, "legion skills:") {
				t.Errorf("skills failure was not named: exit %d, stderr %q", got.exit, got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("skills failure wrote into the RPC frame stream: %q", got.stdout)
			}
			if name == "marketplace commands fail" || name == "second add fails" {
				for _, command := range readLines(t, filepath.Join(dir, "commands")) {
					if strings.HasPrefix(command, "plugin install") {
						t.Errorf("failed registration reached an install: %s", command)
					}
				}
			}
			for _, path := range []string{setupLog, agentLog} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("skills failure started %s: %v", path, err)
				}
			}
		})
	}
}

// The controller has no skills volume, even when every issue role of its deployment does.
func TestOnlyIssueRolesGetASkillsStartStep(t *testing.T) {
	r := skillsRuntime(t)
	for _, role := range append(slices.Clone(claim.Roles), claim.RoleController) {
		l, err := r.prepare(roleSpec(t, role))
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(strings.Join(launcherCommand(l, r).Argv, "\n"), "legion skills:")
		if want := role != claim.RoleController; got != want {
			t.Errorf("%s skills step = %t, want %t", role, got, want)
		}
	}
}

// Skills configuration is a pod-start choice. A config-only change opens the ordinary update
// round, while a role relaunched before it ends installs the clone that its pod holds. The fixed
// clone path and the MARKETPLACE record are that pod's own, not the new configuration's.
func TestSkillsConfigurationDriftsAndKeptLaunchesUseTheHeldRepository(t *testing.T) {
	for name, held := range map[string]string{"no skills yet": "", "another repository": "acme/old-skills"} {
		t.Run(name, func(t *testing.T) {
			opts := goldenOptions()
			if held != "" {
				opts.SkillsRepository = ghrepo.MustParse(held)
			}
			old, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			l, err := old.prepare(workerSpec(t))
			if err != nil {
				t.Fatal(err)
			}
			s := &sandbox{}
			s.Spec.PodTemplate = old.podTemplate(l)
			now := skillsRuntime(t)
			if reasons := now.shapeDrift(s, l.kind); len(reasons) != 1 || !strings.Contains(reasons[0], "skills_repository") {
				t.Errorf("skills change produced no ordinary pod drift: %q", reasons)
			}
			kept, readable := now.keptBy(s, l)
			if !readable {
				t.Fatal("the stored pod shape could not be read")
			}
			if want := opts.SkillsRepository; kept.skills != want {
				t.Errorf("kept launch recorded skills %v, want the pod's %v", kept.skills, want)
			}
			l.kept = &kept
			command := strings.Join(launcherCommand(l, now).Argv, "\n")
			if held == "" {
				if strings.Contains(command, "legion skills:") {
					t.Error("a kept pod with no skills volume was told to install skills")
				}
			} else if !strings.Contains(command, SkillsDir+"/repo") {
				t.Errorf("kept launch did not name its pod's clone: %s", command)
			}
			if oldShape, nowShape := podShape(old, controllerPod{}), podShape(now, controllerPod{}); oldShape != nowShape {
				t.Error("issue skills configuration drifted the controller's pod")
			}
		})
	}
}

// The repository changed in the daemon, but the kept pod's read-only marketplace record still
// names the older clone's catalog. Installation must follow that record until pod replacement.
func TestKeptSkillsStartInstallsThePodsRecordedMarketplace(t *testing.T) {
	dir := t.TempDir()
	install := skillsFixture(t, dir, "old-market\n", "old-plugin\n")
	got := runSkillsStart(t, dir, install, `printf '%s\n' "$*" >>"$SKILLS_LOG"`, nil, []string{"/bin/true"})
	if got.exit != 0 || got.stdout != "" {
		t.Fatalf("kept skills wrapper exited %d, stdout %q: %s", got.exit, got.stdout, got.stderr)
	}
	if commands := readLines(t, filepath.Join(dir, "commands")); !slices.Contains(commands, "plugin install old-plugin@old-market --force") {
		t.Errorf("kept pod did not install its own recorded marketplace: %q", commands)
	}
}
