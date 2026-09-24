package sandbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// The provisioning token is the implement App's installation token, and every agent of a tree can
// write the tree volume. These tests run a pod's init containers exactly as the manifest states
// them, on this machine, and plant on the tree volume what a tree agent can plant there for the
// next pod of the tree: nothing planted may ever run where the provisioning Secret is readable.
//
// Provisioning also pins its git and jj — no hook, the git boot resolved, https alone, a credential
// scoped to github.com, no working-copy snapshot in a credentialed fetch; internal/workspace's
// tests prove each. Those pins are defence: the boundary must hold without them. So every git and
// jj a container starts here is one on which each pin is ineffective, and one whose credential
// answers any host with the token.

// canaryToken is the provisioning Secret's value in every pod the rig runs.
const canaryToken = "ghs_canary_the_tree_must_never_read"

// boundaryRepo is the repository every pod provisions (testSpec's).
const boundaryRepo = "sjawhar/legion-smoke"

// defeatedTool is the git or jj first on every container's PATH: the real one, with every pin
// provisioning's runner adds made ineffective. The core.hooksPath pin is renamed out of the
// GIT_CONFIG_* pairs, a credential scoped to github.com is widened to every host, the https-only
// transport list is dropped, and a process holding the one-shot credential asks the any-host
// askpass. jj also loses the git.executable-path it is handed and --ignore-working-copy.
const defeatedTool = `#!/bin/sh
unset GIT_ALLOW_PROTOCOL
i=0
while [ "$i" -lt "${GIT_CONFIG_COUNT:-0}" ]; do
	eval "key=\${GIT_CONFIG_KEY_$i}"
	case "$key" in
		core.hooksPath) export "GIT_CONFIG_KEY_$i=legion.defeated" ;;
		credential.https://github.com.helper) export "GIT_CONFIG_KEY_$i=credential.helper" ;;
	esac
	i=$((i + 1))
done
[ -z "$LEGION_PROVISIONING_TOKEN_FILE" ] || export GIT_ASKPASS=@ASKPASS@
if [ "@NAME@" = jj ]; then
	for arg; do
		shift
		case "$arg" in
			--config=git.executable-path=*|--ignore-working-copy) ;;
			*) set -- "$@" "$arg" ;;
		esac
	done
fi
exec @REAL@ "$@"
`

// anyHostAskpass answers every host's prompts with the one-shot credential's token.
const anyHostAskpass = `#!/bin/sh
case "$1" in
	Username*) printf '%s\n' x-access-token ;;
	*) cat "$LEGION_PROVISIONING_TOKEN_FILE" ;;
esac
`

// initRig runs pods' init containers against one tree volume, reaching "github.com" through the
// cluster's egress proxy: a stand-in that serves boundaryRepo over git's smart HTTP to a request
// that authenticates as x-access-token with canaryToken.
type initRig struct {
	t *testing.T
	r *Runtime
	// root holds everything the rig makes; pods holds each running pod's volumes and container
	// filesystems, sink what planted code recorded, and pattern the canary a probe looks for (off
	// every volume, so no planted file carries it).
	root, pods, tree, sink, pattern string
	// bin is ahead of the system's directories on every container's PATH: the defeated git and jj.
	bin    string
	legion string
	github *gitHubStandIn
	// network is what every process reaching a remote is told: the egress proxy, the trust store
	// holding the stand-in's CA, and the hosts reached directly.
	network []string
	pod     int
	// host is the server a plant named, when it named one.
	host *recordingHost
}

func newInitRig(t *testing.T, legion string) *initRig {
	t.Helper()
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	rig := &initRig{
		t: t, r: r, root: root, pods: filepath.Join(root, "pods"), tree: filepath.Join(root, "tree"),
		sink: filepath.Join(root, "sink"), pattern: filepath.Join(root, "pattern"), bin: filepath.Join(root, "bin"), legion: legion,
	}
	for _, dir := range []string{rig.pods, rig.tree, rig.bin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(rig.pattern, []byte(canaryToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.github = newGitHubStandIn(t, root)
	rig.network = []string{
		"HTTPS_PROXY=" + rig.github.proxy.URL, "https_proxy=" + rig.github.proxy.URL,
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost", "GIT_SSL_CAINFO=" + rig.github.caFile,
	}
	askpass := filepath.Join(root, "any-host-askpass")
	if err := os.WriteFile(askpass, []byte(anyHostAskpass), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "jj"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("provisioning's boundary tests drive a real %s: %v", name, err)
		}
		script := strings.NewReplacer("@NAME@", name, "@REAL@", shellprefix.Literal(real), "@ASKPASS@", shellprefix.Literal(askpass)).Replace(defeatedTool)
		if err := os.WriteFile(filepath.Join(rig.bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return rig
}

// clone and workspace are where provisioning puts the shared clone and an issue's workspace.
func (rig *initRig) clone() string {
	return filepath.Join(rig.tree, "repos", "github.com", "sjawhar", "legion-smoke")
}

func (rig *initRig) workspace(issue string) string {
	return filepath.Join(rig.tree, "workspaces", "sjawhar", "legion-smoke", strings.ToLower(issue))
}

// probe is a shell command a planted program runs: it appends what it is, and the first copy of
// the canary it can read on any volume of the running pod or on the tree volume, to the sink.
func (rig *initRig) probe(what string) string {
	return fmt.Sprintf(`printf '%%s read=[%%s]\n' %s "$(grep -rhoFf %s %s %s 2>/dev/null | head -n 1)" >> %s`,
		shellprefix.Literal(what), shellprefix.Literal(rig.pattern), shellprefix.Literal(rig.pods), shellprefix.Literal(rig.tree), shellprefix.Literal(rig.sink))
}

// script writes an executable that runs probe(what), then tail.
func (rig *initRig) script(what, tail string) string {
	rig.t.Helper()
	path := filepath.Join(rig.root, "planted", what)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		rig.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+rig.probe(what)+"\n"+tail+"\n"), 0o755); err != nil {
		rig.t.Fatal(err)
	}
	return path
}

// sunk is every line planted code recorded.
func (rig *initRig) sunk() []string {
	rig.t.Helper()
	body, err := os.ReadFile(rig.sink)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		rig.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(body)), "\n")
}

// launch is a pod of spec's claim, as the runtime would create it.
func (rig *initRig) launch(spec runtime.SpawnSpec) corev1.PodSpec {
	rig.t.Helper()
	l, err := rig.r.prepare(spec)
	if err != nil {
		rig.t.Fatal(err)
	}
	return rig.r.podTemplate(l, false).Spec
}

// run executes the pod's init containers in order, as the kubelet does: each volume a directory —
// the tree volume the rig's, every other one the pod's own, gone with the pod — and each container
// a process of the Go legion built from this commit, with the manifest's argv, environment, and
// working directory, every pod path in them moved to the directory of the volume mounted there.
// While a container runs, every volume it does not mount, and every other container's filesystem,
// is unreadable; a read-only mount is not writable. The first container that fails ends the pod.
func (rig *initRig) run(pod corev1.PodSpec) error {
	rig.t.Helper()
	rig.pod++
	podDir := filepath.Join(rig.pods, fmt.Sprintf("pod-%d", rig.pod))
	defer func() { _ = os.RemoveAll(podDir) }()
	volumes := map[string]string{}
	for _, volume := range pod.Volumes {
		dir := filepath.Join(podDir, "volumes", volume.Name)
		if volume.PersistentVolumeClaim != nil {
			dir = rig.tree
		} else if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if volume.Secret != nil {
			for _, item := range volume.Secret.Items {
				value := "not-the-provisioning-token"
				if item.Key == provisionTokenKey {
					value = canaryToken
				}
				if err := os.WriteFile(filepath.Join(dir, item.Path), []byte(value), 0o600); err != nil {
					return err
				}
			}
		}
		volumes[volume.Name] = dir
	}
	for _, c := range pod.InitContainers {
		if err := rig.runContainer(podDir, volumes, c); err != nil {
			return err
		}
	}
	return nil
}

func (rig *initRig) runContainer(podDir string, volumes map[string]string, c corev1.Container) error {
	local := filepath.Join(podDir, "containers", c.Name)
	home, tmp := filepath.Join(local, "home"), filepath.Join(local, "tmp")
	for _, dir := range []string{home, tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	type move struct{ pod, host string }
	moves := []move{{podHome, home}}
	mounted := map[string]bool{local: true}
	var readOnly []string
	for _, mount := range c.VolumeMounts {
		host := filepath.Join(volumes[mount.Name], mount.SubPath)
		moves = append(moves, move{mount.MountPath, host})
		mounted[volumes[mount.Name]] = true
		if mount.ReadOnly {
			readOnly = append(readOnly, host)
		}
	}
	if !slices.ContainsFunc(moves, func(m move) bool { return m.pod == "/tmp" }) {
		moves = append(moves, move{"/tmp", tmp})
	}
	slices.SortFunc(moves, func(a, b move) int { return len(b.pod) - len(a.pod) })
	remap := func(value string) string {
		for _, m := range moves {
			if value == m.pod || strings.HasPrefix(value, m.pod+"/") {
				return m.host + value[len(m.pod):]
			}
		}
		return value
	}

	declared := map[string]string{}
	for _, v := range c.Env {
		declared[v.Name] = v.Value
	}
	env := []string{}
	for _, v := range c.Env {
		env = append(env, v.Name+"="+remap(kubeExpand(v.Value, declared)))
	}
	if _, set := declared["TMPDIR"]; !set {
		env = append(env, "TMPDIR="+tmp)
	}
	env = append(env, "PATH="+rig.bin+":/usr/bin:/bin", "HOME="+home)
	env = append(env, rig.network...)
	argv := make([]string, len(c.Command))
	for i, arg := range c.Command {
		argv[i] = remap(kubeExpand(arg, declared))
	}
	if argv[0] != rig.r.tools.Legion {
		return fmt.Errorf("init container %s runs %s, not the Go legion", c.Name, argv[0])
	}
	argv[0] = rig.legion

	var hidden []string
	candidates := []string{rig.tree}
	for _, parent := range []string{filepath.Join(podDir, "volumes"), filepath.Join(podDir, "containers")} {
		entries, err := os.ReadDir(parent)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			candidates = append(candidates, filepath.Join(parent, entry.Name()))
		}
	}
	for _, dir := range candidates {
		if !mounted[dir] {
			if err := os.Chmod(dir, 0); err != nil {
				return err
			}
			hidden = append(hidden, dir)
		}
	}
	for _, dir := range readOnly {
		if err := chmodTree(dir, false); err != nil {
			return err
		}
	}
	defer func() {
		for _, dir := range hidden {
			_ = os.Chmod(dir, 0o700)
		}
		for _, dir := range readOnly {
			_ = chmodTree(dir, true)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = env
	command.Dir = remap(c.WorkingDir)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("init container %s: %w\n%s", c.Name, err, output)
	}
	return nil
}

// chmodTree adds or removes every owner write permission under dir.
func chmodTree(dir string, writable bool) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() &^ 0o222
		if writable {
			mode |= 0o200
		}
		return os.Chmod(path, mode)
	})
}

// git and jj run a defeated tool in the rig's network, as a tree agent's own command would; what
// it prints is returned, its failure ignored: a live check only needs the planted program to run.
func (rig *initRig) git(dir string, args ...string) string {
	rig.t.Helper()
	return rig.tool(dir, "git", args...)
}

func (rig *initRig) jj(dir string, args ...string) string {
	rig.t.Helper()
	return rig.tool(dir, "jj", args...)
}

func (rig *initRig) tool(dir, name string, args ...string) string {
	rig.t.Helper()
	command := exec.Command(filepath.Join(rig.bin, name), args...)
	command.Dir = dir
	command.Env = append([]string{
		"PATH=" + rig.bin + ":" + os.Getenv("PATH"), "HOME=" + filepath.Join(rig.root, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(rig.root, "config"), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
	}, rig.network...)
	output, _ := command.CombinedOutput()
	return string(output)
}

// holdsNoCanary fails when a file on the tree volume holds the provisioning token.
func (rig *initRig) holdsNoCanary() {
	rig.t.Helper()
	err := filepath.WalkDir(rig.tree, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), canaryToken) {
			rig.t.Errorf("the tree volume holds the provisioning token in %s", path)
		}
		return nil
	})
	if err != nil {
		rig.t.Fatalf("walk the tree volume: %v", err)
	}
}

// plantWorkspaceConfig writes a legacy .jj/workspace-config.toml into a working copy on the tree
// volume: jj migrates such a file into the configuration it reads.
func plantWorkspaceConfig(t *testing.T, dir, toml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".jj", "workspace-config.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// plantFilter makes every changed .txt file of the working copy at dir pass through a clean filter
// that runs probe(what), and changes one.
func (rig *initRig) plantFilter(dir, what string) {
	rig.t.Helper()
	filter := rig.script(what, "exec cat")
	plantWorkspaceConfig(rig.t, dir, "[git.filter]\nenabled = true\n[git.filter.drivers.planted]\nclean = ['"+filter+"']\nsmudge = ['cat']\nrequired = false\n")
	for name, body := range map[string]string{".gitattributes": "*.txt filter=planted\n", "planted.txt": "changed\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			rig.t.Fatal(err)
		}
	}
}

// gitHooks is every hook githooks(5) names.
var gitHooks = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge",
	"pre-push", "pre-receive", "update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "push-to-checkout", "pre-auto-gc", "post-rewrite", "sendemail-validate",
	"fsmonitor-watchman", "p4-changelist", "p4-prepare-changelist", "p4-post-changelist",
	"p4-pre-submit", "post-index-change",
}

// plantHooks writes each named hook into dir, each running probe("hook <name>").
func (rig *initRig) plantHooks(dir string, hooks ...string) {
	rig.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		rig.t.Fatal(err)
	}
	for _, hook := range hooks {
		body := "#!/bin/sh\n" + rig.probe("hook "+hook) + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, hook), []byte(body), 0o755); err != nil {
			rig.t.Fatal(err)
		}
	}
}

// A tree agent can plant each of these on the tree volume for the next pod of its tree (the Stage
// 4b plan's Task 4b.6b; #1286's reviews). The next pod provisions with them in place: whatever
// runs, nothing it runs reads the provisioning token, no host the tree named receives it, and no
// file on the tree volume holds it. Each plant is shown live afterwards, by a tree agent's own
// command triggering it, so its absence from provisioning is the boundary's doing.
func TestNothingTheTreePlantsReadsTheProvisioningToken(t *testing.T) {
	legion := filepath.Join(t.TempDir(), "legion")
	if output, err := exec.Command("go", "build", "-o", legion, "github.com/sjawhar/legion/daemon/cmd/legion").CombinedOutput(); err != nil {
		t.Fatalf("build the Go legion: %v\n%s", err, output)
	}
	probeFilters := exec.Command("jj", "config", "list", "--include-defaults", "git.filter.enabled")
	probeFilters.Env = append(os.Environ(), "JJ_CONFIG="+filepath.Join(t.TempDir(), "none.toml"), "XDG_CONFIG_HOME="+t.TempDir(), "HOME="+t.TempDir())
	if out, _ := probeFilters.Output(); len(strings.TrimSpace(string(out))) == 0 {
		t.Fatal("this jj has no working-copy filters: provisioning is proven against the jj the worker image ships (packages/daemon/docker/worker.Dockerfile, ARG JJ_TOOL), which CI installs")
	}

	type vector struct {
		name string
		// sameIssue is a second pod on the first pod's issue, whose workspace therefore exists
		// (update-stale); otherwise it is another issue's, whose workspace is added.
		sameIssue bool
		plant     func(rig *initRig)
		// live triggers the plant as a tree agent's command would.
		live func(rig *initRig)
	}
	vectors := []vector{
		{
			name:  "hooks in .git/hooks",
			plant: func(rig *initRig) { rig.plantHooks(filepath.Join(rig.clone(), ".git", "hooks"), gitHooks...) },
			live:  func(rig *initRig) { rig.git(rig.clone(), "update-ref", "refs/legion/live", "HEAD") },
		},
		{
			name: "hooks under core.hooksPath",
			plant: func(rig *initRig) {
				hooks := filepath.Join(rig.root, "planted-hooks")
				rig.plantHooks(hooks, gitHooks...)
				rig.git(rig.clone(), "config", "core.hooksPath", hooks)
			},
			live: func(rig *initRig) { rig.git(rig.clone(), "update-ref", "refs/legion/live", "HEAD") },
		},
		{
			name: "git.executable-path in the clone's legacy workspace config",
			plant: func(rig *initRig) {
				git, err := exec.LookPath("git")
				if err != nil {
					rig.t.Fatal(err)
				}
				planted := rig.script("git", "exec "+shellprefix.Literal(git)+` "$@"`)
				plantWorkspaceConfig(rig.t, rig.clone(), "[git]\nexecutable-path = '"+planted+"'\n")
			},
			live: func(rig *initRig) { rig.jj(rig.clone(), "git", "fetch", "-R", rig.clone()) },
		},
		{
			name:  "a working-copy filter in the clone",
			plant: func(rig *initRig) { rig.plantFilter(rig.clone(), "clone filter") },
			live:  func(rig *initRig) { rig.jj(rig.clone(), "status", "-R", rig.clone()) },
		},
		{
			name: "url.<ext::command>.insteadOf",
			plant: func(rig *initRig) {
				command := rig.script("ext", "exit 1")
				rig.git(rig.clone(), "config", "protocol.ext.allow", "always")
				rig.git(rig.clone(), "config", "url.ext::"+command+" %S.insteadOf", "https://github.com/"+boundaryRepo)
			},
			live: func(rig *initRig) { rig.git(rig.clone(), "ls-remote", "origin") },
		},
		{
			name: "a remote on another host, answered by any-host credentials",
			plant: func(rig *initRig) {
				rig.host = newRecordingHost(rig.t, false)
				rig.git(rig.clone(), "remote", "set-url", "origin", rig.host.server.URL+"/"+boundaryRepo)
				rig.git(rig.clone(), "config", "http.sslVerify", "false")
			},
			live: func(rig *initRig) { rig.git(rig.clone(), "ls-remote", "origin") },
		},
		{
			name: "an http.proxy that terminates TLS, with sslVerify off",
			plant: func(rig *initRig) {
				rig.host = newRecordingHost(rig.t, true)
				rig.git(rig.clone(), "config", "http.proxy", rig.host.server.URL)
				rig.git(rig.clone(), "config", "http.sslVerify", "false")
			},
			live: func(rig *initRig) { rig.git(rig.clone(), "ls-remote", "origin") },
		},
		{
			name:  "post-index-change, which jj workspace add runs",
			plant: func(rig *initRig) { rig.plantHooks(filepath.Join(rig.clone(), ".git", "hooks"), "post-index-change") },
			live:  func(rig *initRig) { rig.git(rig.clone(), "read-tree", "HEAD") },
		},
		{
			name:      "a working-copy filter in the issue workspace's legacy config, which update-stale runs",
			sameIssue: true,
			plant:     func(rig *initRig) { rig.plantFilter(rig.workspace(testTree), "workspace filter") },
			live:      func(rig *initRig) { rig.jj(rig.workspace(testTree), "status") },
		},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			rig := newInitRig(t, legion)
			if err := rig.run(rig.launch(rootSpec(t))); err != nil {
				t.Fatalf("the tree's first pod: %v", err)
			}
			if rig.github.authorized() == 0 {
				t.Fatal("the first pod provisioned without presenting the provisioning token to github.com")
			}
			v.plant(rig)
			second := workerSpec(t)
			if !v.sameIssue {
				token, err := claim.NewToken(testProject, "LEGION-209", claim.RoleImplementer)
				if err != nil {
					t.Fatal(err)
				}
				second = testSpec(t, token, claim.RoleImplementer, "LEGION-209")
			}
			if err := rig.run(rig.launch(second)); err != nil {
				t.Logf("the second pod, with the plant in place: %v", err)
			}

			for _, line := range rig.sunk() {
				if strings.Contains(line, canaryToken) {
					t.Errorf("planted code read the provisioning token: %s", line)
				}
			}
			// A server a plant named must never receive the token; its live check is that it received
			// anything at all.
			if rig.host != nil {
				for _, credential := range rig.host.credentials() {
					if strings.Contains(credential, canaryToken) {
						t.Errorf("the host the tree named received the provisioning token: %s", credential)
					}
				}
			}
			rig.holdsNoCanary()

			v.live(rig)
			if rig.host != nil {
				if rig.host.requests() == 0 {
					t.Fatal("the host the tree named received nothing, even from a tree agent's own command: the plant is not live")
				}
			} else if len(rig.sunk()) == 0 {
				t.Fatal("the plant never ran, even for a tree agent's own command: it is not live")
			}
		})
	}
}

// gitHubStandIn is github.com as a pod reaches it: through the cluster's egress proxy, which
// tunnels CONNECT github.com:443 to a TLS server whose certificate the stand-in's CA signed. It
// serves boundaryRepo over git's smart HTTP to a request authenticated as x-access-token with
// canaryToken, and challenges every other request.
type gitHubStandIn struct {
	proxy  *httptest.Server
	caFile string
	mu     sync.Mutex
	count  int
}

func newGitHubStandIn(t *testing.T, root string) *gitHubStandIn {
	t.Helper()
	projects := filepath.Join(root, "github")
	remote := filepath.Join(projects, boundaryRepo)
	seed := filepath.Join(root, "seed")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", remote},
		{"init", "--quiet", "--initial-branch=main", seed},
		{"-C", seed, "-c", "user.name=Legion test", "-c", "user.email=legion-test@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "seed"},
		{"-C", seed, "push", "--quiet", remote, "HEAD:main"},
	} {
		command := exec.Command(git, argv...)
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, output)
		}
	}
	ca, caKey, caPEM := newCertificateAuthority(t)
	caFile := filepath.Join(root, "github-ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	g := &gitHubStandIn{caFile: caFile}
	backend := &cgi.Handler{
		Path: git, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + projects, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"},
	}
	g.proxy = tlsTerminatingProxy(t, leafCertificate(t, ca, caKey), func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != "x-access-token" || password != canaryToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		g.mu.Lock()
		g.count++
		g.mu.Unlock()
		backend.ServeHTTP(w, r)
	}, nil)
	return g
}

// authorized is how many requests presented the provisioning token.
func (g *gitHubStandIn) authorized() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.count
}

// recordingHost is a server a tree agent points provisioning at: a TLS host of its own, or an
// http.proxy that terminates TLS for whatever host the client asked to CONNECT to, with a
// certificate no trust store holds. It records every request and the credentials each carried,
// and challenges for more.
type recordingHost struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   int
	creds  []string
}

func newRecordingHost(t *testing.T, proxy bool) *recordingHost {
	t.Helper()
	h := &recordingHost{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen++
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(authorization, "Basic "))
			h.creds = append(h.creds, string(decoded))
		}
		h.mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="planted"`)
		w.WriteHeader(http.StatusUnauthorized)
	}
	if proxy {
		ca, caKey, _ := newCertificateAuthority(t)
		h.server = tlsTerminatingProxy(t, leafCertificate(t, ca, caKey), handler, func() {
			h.mu.Lock()
			h.seen++
			h.mu.Unlock()
		})
	} else {
		h.server = httptest.NewTLSServer(http.HandlerFunc(handler))
		t.Cleanup(h.server.Close)
	}
	return h
}

func (h *recordingHost) requests() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen
}

func (h *recordingHost) credentials() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.creds)
}

// tlsTerminatingProxy is an HTTP proxy that answers every CONNECT itself: it terminates TLS with
// certificate and serves handler on the tunnel. onConnect, when set, runs for each CONNECT.
func tlsTerminatingProxy(t *testing.T, certificate tls.Certificate, handler http.HandlerFunc, onConnect func()) *httptest.Server {
	t.Helper()
	tunnels := &tunnelListener{conns: make(chan net.Conn), done: make(chan struct{})}
	inner := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = inner.Serve(tunnels) }()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		if onConnect != nil {
			onConnect()
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			_ = conn.Close()
			return
		}
		select {
		case tunnels.conns <- tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}}):
		case <-tunnels.done:
			_ = conn.Close()
		}
	}))
	t.Cleanup(func() {
		proxy.Close()
		_ = inner.Close()
	})
	return proxy
}

// tunnelListener hands the proxy's tunnels to an http.Server as accepted connections.
type tunnelListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *tunnelListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *tunnelListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *tunnelListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func newCertificateAuthority(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Legion test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return ca, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// leafCertificate is a server certificate for github.com that ca signed.
func leafCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "github.com"}, DNSNames: []string{"github.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
