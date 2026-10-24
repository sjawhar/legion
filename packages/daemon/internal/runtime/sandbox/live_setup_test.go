//go:build e2e

// The Stage 4a harness's runtime.kubernetes.setup checks: the shared runtime starts every
// role through a setup command, and a setup failure prevents the agent from starting. The rig is
// live_test.go.
package sandbox

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// setup-ran: the root's first setup record witnesses the same checks `awaitRunning` applies to
// every successful role launch in this run.
func (r *liveRig) checkSetupRan() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	return r.checkSetupRecord(root)
}

// checkSetupRecord proves c's current setup ran before its current stub agent. awaitRunning calls
// it after every successful launch or resume, so the proof covers every role the rig
// starts, including each separately provisioned issue pod, instead of only the root architect.
func (r *liveRig) checkSetupRecord(c *liveClaim) error {
	out, err := r.exec(c, "cat", "--", c.marker)
	if err != nil {
		return err
	}
	setupLine, setupAt, agentAt := "", -1, -1
	agentLine := string(c.role) + ":" + c.loc.Incarnation
	for at, line := range strings.Split(out, "\n") {
		if line == agentLine {
			agentAt = at
		}
		if strings.HasPrefix(line, "setup:"+string(c.role)+":"+c.loc.Incarnation+" ") {
			setupLine, setupAt = line, at
		}
	}
	if setupAt < 0 {
		return fmt.Errorf("%s has no setup record for %s: %q", c.marker, c.loc.Incarnation, out)
	}
	if agentAt < 0 {
		return fmt.Errorf("%s has no agent record for %s after setup: %q", c.marker, c.loc.Incarnation, out)
	}
	if setupAt >= agentAt {
		return fmt.Errorf("the setup record is line %d and the agent record is line %d in %s, want setup before agent: %q", setupAt, agentAt, c.marker, out)
	}
	setup, err := parseSetupMarker(setupLine)
	if err != nil {
		return err
	}
	if setup.role != string(c.role) || setup.incarnation != c.loc.Incarnation {
		return fmt.Errorf("setup record %#v, want role %s at %s", setup, c.role, c.loc.Incarnation)
	}
	if err := setupGitProblem(setup); err != nil {
		return err
	}
	// The directory the ls-remote calls ran in is the setup's own, made fresh and removed on exit.
	if left, err := r.exec(c, "sh", "-c", `if [ -e "$1" ]; then echo present; else echo absent; fi`, "gone", setup.gitFrom); err != nil {
		return err
	} else if left != "absent" {
		return fmt.Errorf("the setup's ls-remote directory %s is still %s after it exited, want removed", setup.gitFrom, left)
	}
	if err := setupHelperProblem(setup); err != nil {
		return err
	}

	pid, err := r.agentPid(c)
	if err != nil {
		return err
	}
	if setup.ppid != pid {
		return fmt.Errorf("setup record parent pid %s, want the agent pid %s: setup must finish before that wrapper execs into the agent", setup.ppid, pid)
	}
	cwd, err := r.exec(c, "readlink", "/proc/"+pid+"/cwd")
	if err != nil {
		return err
	}
	if setup.cwd != cwd {
		return fmt.Errorf("setup recorded cwd %q, want the agent's starting cwd %q", setup.cwd, cwd)
	}
	uid, err := r.exec(c, "sh", "-c", `sed -n 's/^Uid:[[:space:]]*\([0-9]*\).*/\1/p' "/proc/$1/status"`, "uid", pid)
	if err != nil {
		return err
	}
	pod, err := r.getPod(SandboxName(c.token))
	if err != nil {
		return err
	}
	wantUID, workingDir, err := rolesContainerSettings(pod)
	if err != nil {
		return err
	}
	if setup.uid != wantUID || uid != wantUID {
		return fmt.Errorf("setup uid %q and agent uid %q, want pod %s's RunAsUser %q", setup.uid, uid, pod.Name, wantUID)
	}
	if workingDir != cwd {
		return fmt.Errorf("the %s container's WorkingDir is %q, want the agent cwd %q", runtime.IssuePodContainer, workingDir, cwd)
	}
	note("operator", "%s setup marker before its agent marker: cwd %s; uid %s; git ls-remote from the fresh %s succeeded with the role's helper and was refused without it; setup parent pid %s became the agent", c.name, setup.cwd, setup.uid, setup.gitFrom, setup.ppid)
	return nil
}

// setup-negative: a second runtime from the run's Options replaces setup with an argv that exits
// 7. Its launcher resolves through the shared listener only for this throwaway Sandbox, so the
// primary runtime's live role connections stay with their own runtime. A shim hello precedes its
// child spawn by design; the absence of the stub marker and the launcher-reported exit prove that
// the agent itself never started.
func (r *liveRig) checkSetupNegative() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	c := r.claim("setup-negative")
	r.mapRepository(r.env.repo)
	name := SandboxName(c.token)
	if err := r.recordSandbox(name); err != nil {
		return err
	}
	cleaned := false
	defer func() {
		if !cleaned {
			_, _ = r.kubectl("delete", "sandbox", name, "--ignore-not-found", "--wait=false")
		}
	}()

	ctx, stop := context.WithCancel(r.ctx)
	defer stop()
	opts := r.runtimeOptions(r.streamAddress(), r.ln)
	opts.Setup = []string{"/bin/sh", "-c", "exit 7"}
	rt, err := New(ctx, r.rc, opts)
	if err != nil {
		return fmt.Errorf("a second runtime whose setup exits 7: %w", err)
	}
	shared := r.rt.LauncherResolver()
	second := rt.LauncherResolver()
	r.ln.SetLauncherResolver(func(hello shimwire.LauncherHello) (stream.LauncherHandler, string) {
		if hello.Sandbox == name {
			return second(hello)
		}
		return shared(hello)
	})
	defer r.ln.SetLauncherResolver(shared)

	c.bootToken, c.gen = r.reg.mint(c.token, true)
	since := time.Now()
	loc, err := rt.Spawn(ctx, r.spec(c, ""))
	if err != nil {
		return fmt.Errorf("Spawn(%s) on the failing-setup runtime: %w", c.token, err)
	}
	c.loc, c.last = &loc, &loc
	reg, ok := r.reg.await(c.token, c.gen, since, liveBootTimeout)
	if !ok {
		pod, podErr := r.getPod(name)
		if podErr != nil {
			return fmt.Errorf("the failing-setup shim did not register at generation %d within %s, and the pod could not be read: %w", c.gen, liveBootTimeout, podErr)
		}
		return r.networkPathFailure(pod)
	}
	if reg.hash != tokenHash(c.bootToken) {
		return fmt.Errorf("the failing-setup shim registered token hash %s, want %s", short(reg.hash), short(tokenHash(c.bootToken)))
	}

	var last runtime.Observation
	if err := r.poll(liveBootTimeout, "failing setup generation "+loc.Incarnation+" to end", func() (bool, error) {
		obs, err := rt.Probe(ctx, loc)
		if err != nil {
			return false, err
		}
		last = obs
		switch obs.Kind {
		case runtime.Alive, runtime.Uncertain:
			return false, nil
		case runtime.Gone:
			if strings.Contains(obs.Detail, "code 7") {
				return true, nil
			}
			return false, fmt.Errorf("the failing setup ended %s without code 7: %s", loc.Incarnation, obs.Detail)
		default:
			return false, fmt.Errorf("the failing setup observed %s for %s: %s", obs.Kind, loc.Incarnation, obs.Detail)
		}
	}); err != nil {
		return err
	}
	note("runtime", "failing setup's shim registered generation %d, then %s — %s", reg.gen, last.Kind, oneLine(last.Detail))

	// The roles container's log carries every role's lines, each prefixed with its role.
	line := string(c.role) + ": legion setup: runtime.kubernetes.setup exited 7; not starting Oh My Pi"
	log, err := r.kubectl("logs", name, "-c", runtime.IssuePodContainer)
	if err != nil {
		return err
	}
	if !strings.Contains(log, line) {
		return fmt.Errorf("the %s container's log lacks %q: %s", runtime.IssuePodContainer, line, strings.TrimSpace(log))
	}
	note("operator", "logs %s -c %s: %s", name, runtime.IssuePodContainer, line)

	lines, err := r.markerLines(c)
	if err != nil {
		return err
	}
	if slices.Contains(lines, string(c.role)+":"+loc.Incarnation) {
		return fmt.Errorf("the failing setup started the agent, whose marker %s:%s is in %v", c.role, loc.Incarnation, lines)
	}
	note("operator", "exec cat %s: no %s:%s marker; the stub agent never started", c.marker, c.role, loc.Incarnation)

	if _, err := r.kubectl("delete", "sandbox", name, "--wait=true", "--timeout=5m"); err != nil {
		return err
	}
	if _, err := r.kubectl("delete", "pvc", IssueClaimName(c.token), "--ignore-not-found", "--wait=true", "--timeout=5m"); err != nil {
		return err
	}
	if pvcs, err := r.issuePVCs(c.issue); err != nil || len(pvcs) != 0 {
		return fmt.Errorf("setup-negative PVCs after Sandbox deletion: %v (%v)", pvcs, err)
	}
	cleaned = true
	c.loc, c.last, c.state = nil, nil, stateReleased
	note("operator", "deleted throwaway Sandbox %s and its PVC", name)
	return nil
}

// rolesContainerSettings are the user and working directory of an issue pod's one container, which
// every role's agent runs as and starts in.
func rolesContainerSettings(pod *corev1.Pod) (uid, workingDir string, err error) {
	var container *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == runtime.IssuePodContainer {
			container = &pod.Spec.Containers[i]
		}
	}
	if container == nil {
		return "", "", fmt.Errorf("pod %s has no %s container", pod.Name, runtime.IssuePodContainer)
	}
	var user *int64
	if pod.Spec.SecurityContext != nil {
		user = pod.Spec.SecurityContext.RunAsUser
	}
	if container.SecurityContext != nil && container.SecurityContext.RunAsUser != nil {
		user = container.SecurityContext.RunAsUser
	}
	if user == nil {
		return "", "", fmt.Errorf("pod %s's %s container has no RunAsUser", pod.Name, runtime.IssuePodContainer)
	}
	return strconv.FormatInt(*user, 10), container.WorkingDir, nil
}
