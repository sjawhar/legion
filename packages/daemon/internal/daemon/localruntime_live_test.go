//go:build e2e

// The runtime a live proof uses when it needs a real Oh My Pi pane and the host has no tmux. A
// Legion pod carries no tmux and may not have one added for a test (a sixth client in the worker
// image is an image change, skill://legion-worker/references/tools.md), so
// provider_error_retry_live_test.go — the one proof that must watch a real agent's turns fail —
// starts each worker as a child process group of the test process instead of as a pane on the
// daemon's private tmux server.
//
// Everything below the pane is the real thing: the command is the one a pane runs,
// `legion worker-shim --connect unix://<the daemon's stream socket> --boot-token-file <file> --
// <omp argv>`, built from this checkout's `legion` and the Oh My Pi LEGION_OMP_PATH names; the
// environment is `internal/runtime/tmux`'s own (tmux.PaneVariables over tmux.PaneInputs), which is
// the function that package exports for exactly this — a process it does not start that must be
// told what a pane is told. The agent therefore registers over the daemon's own HTTP routes,
// speaks the daemon's own worker stream through the real shim, and loads this branch's two
// plugins, as a pane does.
//
// What differs from `internal/runtime/tmux`, and why:
//
//   - There is no tmux, so there is no window, no pane id, no `sh -c` and no server-wide
//     environment table. The locator still says `tmux`, because `runtime.Locator.Validate` admits
//     two runtime words and a local process is neither a pod nor nothing; its window is the claim's
//     issue and its pane `%<pid>`, so `Locator.Label` still reads as a place. The incarnation is
//     the tmux runtime's own `<pid>:<start ticks>`, read from /proc, so a reissued pid is told from
//     the process the locator recorded exactly as it is there.
//   - The inner Oh My Pi command is an argv rather than a shell fragment. A pane's shell expands
//     `$(cat <role prompt files>)` inside one `--append-system-prompt` word (omplaunch's
//     SystemPromptArgument); with no shell, promptArgument reads the same files and composes the
//     same word.
//   - No GitHub credential is minted and no `gh` bundle is written: the pane of this proof pushes
//     nothing, so its spec carries no repository and PaneInputs names no owners directory, which
//     is the same pane a runtime with no GitHubCredential function opens.
//   - ProvisionsWorkspaces answers true, as the Sandbox runtime does, so the daemon clones no
//     repository on its host; this runtime makes the claim's working directory itself.
//   - PodDrift, ReplacePod, RescueUnpushed and CleanupTree hold nothing per place or per tree, so
//     each answers as a process-only runtime answers: no drift, nothing to replace, nothing to
//     rescue, nothing to delete.
//
// Every child is started in a process group of its own (Setpgid) and every stop signals that
// group, so Oh My Pi and the shim go together and a failed or finished run leaves no child
// behind: the test's cleanup kills every group the runtime ever started, whatever the proof did.
package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

// localStopGrace is how long a stop waits for a child to end after the shutdown frame and then
// after SIGTERM, before the group is SIGKILLed. Oh My Pi's own exit on a shutdown is under a
// second once it is idle; a turn in flight takes the longer path.
const localStopGrace = 15 * time.Second

// localObserveInterval is how often the observation sweep reports every tracked process. The
// supervisor's own probe interval is the configuration's (ProbeInterval); this is the stream of
// unsolicited observations beside it.
const localObserveInterval = 2 * time.Second

// localPane is one running worker: the command, the locator the runtime minted for it, and the
// channel its Wait goroutine closes when the process group's leader has gone.
type localPane struct {
	spec    runtime.SpawnSpec
	cmd     *exec.Cmd
	locator runtime.Locator
	pid     int
	log     string
	done    chan struct{}
}

// localRuntime is the runtime itself. Conns is the daemon's own connection directory, which the
// runtime factory hands every runtime; a stop sends the shutdown frame over the claim's connection
// before it signals anything, as the tmux runtime's stop does.
type localRuntime struct {
	t         *testing.T
	stateDir  string
	legion    string
	omp       string
	home      string
	profile   string
	daemonURL string
	envoyURL  string
	natsURLs  []string
	// overlays answers the Oh My Pi overlay files one claim's pane is told, in PI_CONFIG_FILES
	// order. A pane's overlays are the operator's (runtime.tmux.pi_config_files) and its role's
	// (role_config_files) in production; this proof routes each claim to a stand-in of its own, and
	// two claims of the proof share a role, so the choice is per claim here.
	overlays func(claim.Token) []string
	conns    runtime.Conns
	stream   string
	// paneLogs is the directory each worker's stdout and stderr is written under, one file per
	// launch, kept for the run's evidence.
	paneLogs string

	mu      sync.Mutex
	panes   map[claim.Token]*localPane
	started []*localPane
	seq     int
	obs     chan runtime.Observation
	stopped bool
}

// newLocalRuntime builds the runtime and registers the cleanup that kills every process group it
// ever started: a proof that fails part way leaves no Oh My Pi running. Each worker's own output
// is written beside the daemon's log, in the directory the run's script keeps as its evidence
// (liveDaemonLogEnv), since the state directory goes with the test.
func newLocalRuntime(t *testing.T, stateDir, legion, omp, home, profile string) *localRuntime {
	t.Helper()
	logs := filepath.Join(stateDir, "panes")
	if daemonLog := os.Getenv(liveDaemonLogEnv); daemonLog != "" {
		logs = filepath.Join(filepath.Dir(daemonLog), "panes")
	}
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatalf("make the pane log directory %s: %v", logs, err)
	}
	r := &localRuntime{
		t: t, stateDir: stateDir, legion: legion, omp: omp, home: home, profile: profile,
		overlays: func(claim.Token) []string { return nil },
		paneLogs: logs, panes: map[claim.Token]*localPane{}, obs: make(chan runtime.Observation, 256),
	}
	t.Cleanup(r.killEverything)
	return r
}

// killEverything SIGKILLs every process group this runtime started that is still alive. It runs
// from the test's cleanup, after the daemon has stopped, so nothing it kills is still being
// supervised.
func (r *localRuntime) killEverything() {
	r.mu.Lock()
	r.stopped = true
	started := slices.Clone(r.started)
	r.mu.Unlock()
	for _, pane := range started {
		_ = syscall.Kill(-pane.pid, syscall.SIGKILL)
	}
	for _, pane := range started {
		select {
		case <-pane.done:
		case <-time.After(10 * time.Second):
			r.t.Errorf("the worker of %s (pid %d) did not end after SIGKILL; its log is %s", pane.spec.Claim, pane.pid, pane.log)
		}
	}
}

// Launches is every spec this runtime was asked to start, in order, so a step can read which
// session a relaunch opened and what it was started from.
func (r *localRuntime) Launches() []runtime.SpawnSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	specs := make([]runtime.SpawnSpec, 0, len(r.started))
	for _, pane := range r.started {
		specs = append(specs, pane.spec)
	}
	return specs
}

// LaunchesOf is Launches narrowed to one claim.
func (r *localRuntime) LaunchesOf(token claim.Token) []runtime.SpawnSpec {
	var specs []runtime.SpawnSpec
	for _, spec := range r.Launches() {
		if spec.Claim == token {
			specs = append(specs, spec)
		}
	}
	return specs
}

func (r *localRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile != "" {
		return runtime.Locator{}, fmt.Errorf("spawn %s: a spec with a session file is Resume's", spec.Claim)
	}
	return r.launch(ctx, spec)
}

func (r *localRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if prev != nil {
		if err := r.awaitGone(ctx, *prev); err != nil {
			return runtime.Locator{}, err
		}
	}
	// The claim resumes the agent it recorded or none, as every runtime does: a session file that
	// is not there is a refusal, charged as any launch failure is.
	if spec.ResumeSessionFile != "" {
		if _, err := os.Stat(spec.ResumeSessionFile); err != nil {
			return runtime.Locator{}, fmt.Errorf("resume %s: the recorded session file is missing: %s", spec.Claim, spec.ResumeSessionFile)
		}
	}
	return r.launch(ctx, spec)
}

// launch starts one worker: its working directory and boot-token file written, then the shim
// around Oh My Pi in a process group of its own. A claim whose process still runs is refused —
// one claim, one process — as the tmux runtime refuses it.
func (r *localRuntime) launch(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if err := runtime.ValidateSpawnSpec(spec, nil); err != nil {
		return runtime.Locator{}, err
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return runtime.Locator{}, fmt.Errorf("spawn %s: the proof's runtime has stopped", spec.Claim)
	}
	if live, ok := r.panes[spec.Claim]; ok && alive(live.pid) {
		r.mu.Unlock()
		return runtime.Locator{}, fmt.Errorf("spawn %s: its process (pid %d) still runs", spec.Claim, live.pid)
	}
	r.seq++
	seq := r.seq
	r.mu.Unlock()

	workDir := filepath.Join(r.stateDir, "workspaces", spec.Issue)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: make its working directory: %w", spec.Claim, err)
	}
	bootToken := runtime.SecretFile{
		Variable: "LEGION_BOOT_TOKEN",
		Path:     runtime.SecretFilePath(r.stateDir, string(spec.Claim)),
		Value:    spec.BootToken,
	}
	if err := runtime.WriteSecretFiles(r.stateDir, []runtime.SecretFile{bootToken}); err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	prompt, err := promptArgument(spec.Prompt)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	variables, err := tmux.PaneVariables(spec, tmux.PaneInputs{
		StateDir: r.stateDir, Workspace: workDir, DaemonURL: r.daemonURL, EnvoyURL: r.envoyURL,
		NATSURLs: r.natsURLs, RoleConfigFiles: r.overlays(spec.Claim),
	}, []runtime.SecretFile{bootToken})
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}

	argv := []string{
		r.legion, "worker-shim",
		"--connect", r.stream,
		"--boot-token-file", bootToken.Path,
		"--", r.omp, "--mode", "rpc", "--append-system-prompt", prompt,
	}
	if spec.ResumeSessionFile != "" {
		argv = slices.Insert(argv, len(argv)-4, "--resume="+spec.ResumeSessionFile)
	}
	logPath := filepath.Join(r.paneLogs, fmt.Sprintf("%s.%d.log", spec.Claim, seq))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: open its log %s: %w", spec.Claim, logPath, err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = r.paneEnvironment(variables, workDir)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Its own process group: one signal ends the shim, Oh My Pi and whatever Oh My Pi started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	pid := cmd.Process.Pid
	ticks, _, err := startTicks(pid)
	if err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
		logFile.Close()
		return runtime.Locator{}, fmt.Errorf("spawn %s: read pid %d's start ticks: %w", spec.Claim, pid, err)
	}
	pane := &localPane{
		spec: spec, cmd: cmd, pid: pid, log: logPath, done: make(chan struct{}),
		locator: runtime.Locator{
			Runtime:     runtime.RuntimeTmux,
			Claim:       spec.Claim,
			Incarnation: strconv.Itoa(pid) + ":" + strconv.FormatUint(ticks, 10),
			Tmux:        &runtime.TmuxLocator{Window: strings.ToLower(spec.Issue), Pane: "%" + strconv.Itoa(pid)},
		},
	}
	r.mu.Lock()
	r.panes[spec.Claim] = pane
	r.started = append(r.started, pane)
	r.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(pane.done)
		r.report(runtime.Observation{Locator: pane.locator, Kind: runtime.Gone, At: time.Now(),
			Detail: "the worker's process group ended; its log is " + logPath})
	}()
	return pane.locator, nil
}

// paneEnvironment is the child's whole environment: the variables a pane is told, the PATH a pane
// is told (the daemon's launcher directory first, as workerbin.Path puts it), the HOME and
// OMP_PROFILE that select the run's own Oh My Pi profile — which under tmux reach a pane from the
// daemon's own environment through the tmux server — and TMPDIR. Nothing of the test process's own
// environment rides through: this test runs inside a Legion pod, whose environment names that
// pod's workspace, daemon and credentials, and a pane that inherited them would register with the
// wrong daemon.
func (r *localRuntime) paneEnvironment(variables []string, workDir string) []string {
	env := []string{
		"PATH=" + workerbin.Path(os.Getenv("PATH"), r.stateDir),
		"HOME=" + r.home,
		"OMP_PROFILE=" + r.profile,
		"TMPDIR=" + filepath.Join(workDir, "tmp"),
		"LANG=C.UTF-8",
	}
	_ = os.MkdirAll(filepath.Join(workDir, "tmp"), 0o700)
	return append(env, variables...)
}

// promptArgument is the value of the one `--append-system-prompt` word, as a pane's shell produces
// it from omplaunch.SystemPromptArgument: the role prompt files concatenated, the addressing, then
// the deployment instructions, separated by a blank line, with each command substitution's trailing
// newlines dropped as `$(…)` drops them.
func promptArgument(parts runtime.PromptParts) (string, error) {
	read := func(paths ...string) (string, error) {
		var body strings.Builder
		for _, path := range paths {
			file, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("read the prompt file: %w", err)
			}
			body.Write(file)
		}
		return strings.TrimRight(body.String(), "\n"), nil
	}
	roles, err := read(parts.RolePromptPaths...)
	if err != nil {
		return "", err
	}
	fragments := []string{roles}
	if parts.Addressing != "" {
		fragments = append(fragments, parts.Addressing)
	}
	if parts.DeploymentInstructionsPath != "" {
		instructions, err := read(parts.DeploymentInstructionsPath)
		if err != nil {
			return "", err
		}
		fragments = append(fragments, instructions)
	}
	return strings.Join(fragments, "\n\n"), nil
}

func (r *localRuntime) Suspend(ctx context.Context, loc runtime.Locator) error {
	return r.stop(ctx, loc)
}

func (r *localRuntime) Release(ctx context.Context, k runtime.Known) error {
	if err := k.Validate(); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	defer r.forget(k.Claim)
	if k.Locator == nil {
		return nil
	}
	return r.stop(ctx, *k.Locator)
}

// stop ends the locator's process: the shutdown frame over the claim's connection first, so the
// shim ends Oh My Pi itself, then SIGTERM and finally SIGKILL to the whole process group. A
// locator that no longer names the claim's current process is already stopped, and nothing is
// signalled for it.
func (r *localRuntime) stop(ctx context.Context, loc runtime.Locator) error {
	pane, ok := r.pane(loc)
	if !ok {
		return nil
	}
	if r.conns != nil {
		if conn, has := r.conns.Conn(loc.Claim); has {
			shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), localStopGrace)
			_ = conn.Shutdown(shutdown)
			cancel()
		}
	}
	if waitGone(pane, localStopGrace) {
		return nil
	}
	_ = syscall.Kill(-pane.pid, syscall.SIGTERM)
	if waitGone(pane, localStopGrace) {
		return nil
	}
	_ = syscall.Kill(-pane.pid, syscall.SIGKILL)
	if waitGone(pane, localStopGrace) {
		return nil
	}
	return fmt.Errorf("stop %s: pid %d is still running after SIGKILL", loc.Claim, pane.pid)
}

// awaitGone waits out the stop grace for prev's process, as a resume over a previous incarnation
// must before it starts a second one.
func (r *localRuntime) awaitGone(ctx context.Context, prev runtime.Locator) error {
	pane, ok := r.pane(prev)
	if !ok {
		return nil
	}
	if waitGone(pane, localStopGrace) {
		return nil
	}
	return fmt.Errorf("resume %s: its previous process (pid %d) is still running", prev.Claim, pane.pid)
}

func waitGone(pane *localPane, within time.Duration) bool {
	select {
	case <-pane.done:
		return true
	case <-time.After(within):
		return false
	}
}

// pane is the running pane loc names, and false when loc names no process this runtime still
// records as that claim's current one.
func (r *localRuntime) pane(loc runtime.Locator) (*localPane, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pane, ok := r.panes[loc.Claim]
	if !ok || pane.locator.Incarnation != loc.Incarnation {
		return nil, false
	}
	return pane, true
}

func (r *localRuntime) forget(token claim.Token) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.panes, token)
}

func (r *localRuntime) Probe(_ context.Context, loc runtime.Locator) (runtime.Observation, error) {
	return r.observation(loc), nil
}

// observation is what this runtime can say about loc now: the process it recorded running, gone,
// or some other process on that pid.
func (r *localRuntime) observation(loc runtime.Locator) runtime.Observation {
	now := time.Now()
	pane, ok := r.pane(loc)
	if !ok {
		return runtime.Observation{Locator: loc, Kind: runtime.Gone, At: now,
			Detail: "this runtime records no such process for the claim"}
	}
	pid, recordedTicks, err := parseLocalIncarnation(loc.Incarnation)
	if err != nil {
		return runtime.Observation{Locator: loc, Kind: runtime.Uncertain, At: now, Detail: err.Error()}
	}
	ticks, running, err := startTicks(pid)
	switch {
	case err != nil:
		return runtime.Observation{Locator: loc, Kind: runtime.Uncertain, At: now, Detail: err.Error()}
	case !running:
		return runtime.Observation{Locator: loc, Kind: runtime.Gone, At: now, Detail: fmt.Sprintf("pid %d is gone", pid)}
	case ticks != recordedTicks:
		return runtime.Observation{Locator: loc, Kind: runtime.NotRecordedProcess, At: now,
			Detail: fmt.Sprintf("pid %d started at %d ticks, and the locator records %d", pid, ticks, recordedTicks)}
	}
	// A process that has exited but whose status this runtime has not reaped is a zombie, which
	// /proc still shows at its recorded start ticks: the Wait goroutine's channel is the one
	// reading that tells it from a live worker.
	select {
	case <-pane.done:
		return runtime.Observation{Locator: loc, Kind: runtime.Gone, At: now, Detail: fmt.Sprintf("pid %d has exited", pid)}
	default:
		return runtime.Observation{Locator: loc, Kind: runtime.Alive, At: now}
	}
}

// Observe is the periodic sweep: every tracked process reported every interval, plus the Gone each
// Wait goroutine reports the moment its child ends.
func (r *localRuntime) Observe(ctx context.Context) (<-chan runtime.Observation, error) {
	go func() {
		ticker := time.NewTicker(localObserveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			r.mu.Lock()
			locators := make([]runtime.Locator, 0, len(r.panes))
			for _, pane := range r.panes {
				locators = append(locators, pane.locator)
			}
			r.mu.Unlock()
			for _, loc := range locators {
				r.report(r.observation(loc))
			}
		}
	}()
	return r.obs, nil
}

// report puts one observation on the channel, dropping it when nothing is reading: an observation
// the supervisor missed is re-reported by the next sweep, and a blocked send would stall a Wait
// goroutine for the whole run.
func (r *localRuntime) report(o runtime.Observation) {
	select {
	case r.obs <- o:
	default:
	}
}

// ReconcileOrphans ends every process this runtime started that no known claim names. Nothing it
// did not start is its own, so nothing else is ever killed here.
func (r *localRuntime) ReconcileOrphans(ctx context.Context, known []runtime.Known, _ time.Duration) error {
	for _, k := range known {
		if err := k.Validate(); err != nil {
			return fmt.Errorf("reconcile orphans: %w", err)
		}
	}
	r.mu.Lock()
	var orphans []runtime.Locator
	for token, pane := range r.panes {
		if !slices.ContainsFunc(known, func(k runtime.Known) bool { return k.Claim == token }) {
			orphans = append(orphans, pane.locator)
		}
	}
	r.mu.Unlock()
	for _, loc := range orphans {
		if err := r.stop(ctx, loc); err != nil {
			return err
		}
		r.forget(loc.Claim)
	}
	return nil
}

// AdoptWorkingCopy is nothing to do: this proof's workers commit nothing, and their working
// directory is no jj workspace.
func (r *localRuntime) AdoptWorkingCopy(context.Context, runtime.Locator, runtime.GitIdentity) error {
	return nil
}

// ProvisionsWorkspaces answers true, as the Sandbox runtime does, so the daemon clones no
// repository on its host; launch makes each claim's working directory.
func (r *localRuntime) ProvisionsWorkspaces() bool { return true }

// ProvisionBound is zero: the child starts the agent at once, as a tmux pane does.
func (r *localRuntime) ProvisionBound() time.Duration { return 0 }

// LaunchBound is zero: a launch is a local fork and exec that states no bound of its own, as a
// tmux pane start does.
func (r *localRuntime) LaunchBound() time.Duration { return 0 }

// PodDrift answers nothing: a child process group shares no place fixed at its creation.
func (r *localRuntime) PodDrift(context.Context, runtime.Known) ([]string, error) { return nil, nil }

// ReplacePod has no place to replace.
func (r *localRuntime) ReplacePod(context.Context, runtime.Known, func(context.Context) error) error {
	return nil
}

// RescueUnpushed has no disk that goes with a place, so it has nothing to check.
func (r *localRuntime) RescueUnpushed(context.Context, runtime.Known, runtime.RescueRequest) ([]runtime.WorkspaceRescue, error) {
	return nil, nil
}

// CleanupTree holds nothing per tree.
func (r *localRuntime) CleanupTree(context.Context, string) error { return nil }

// parseLocalIncarnation reads the `<pid>:<start ticks>` an incarnation is, as the tmux runtime
// spells it.
func parseLocalIncarnation(s string) (pid int, ticks uint64, err error) {
	pidText, ticksText, ok := strings.Cut(s, ":")
	pid, pidErr := strconv.Atoi(pidText)
	ticks, ticksErr := strconv.ParseUint(ticksText, 10, 64)
	if !ok || pidErr != nil || ticksErr != nil || pid <= 0 {
		return 0, 0, fmt.Errorf("incarnation %q is not <pid>:<start ticks>", s)
	}
	return pid, ticks, nil
}

// startTicks is pid's start ticks — field 22 of /proc/<pid>/stat, counted after the parenthesised
// command name, which may itself hold spaces and brackets — or running=false when the process is
// gone.
func startTicks(pid int) (ticks uint64, running bool, err error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	_, rest, ok := strings.Cut(string(stat), ") ")
	if !ok {
		return 0, false, fmt.Errorf("malformed /proc/%d/stat line: %s", pid, strings.TrimSpace(string(stat)))
	}
	// Field 3 (state) onwards, so starttime (field 22) is index 22-3.
	fields := strings.Fields(rest)
	const starttime = 22 - 3
	if len(fields) <= starttime {
		return 0, false, fmt.Errorf("malformed /proc/%d/stat line (no starttime field)", pid)
	}
	if ticks, err = strconv.ParseUint(fields[starttime], 10, 64); err != nil {
		return 0, false, fmt.Errorf("malformed /proc/%d/stat starttime: %w", pid, err)
	}
	return ticks, true, nil
}

// alive is whether pid names a process at all.
func alive(pid int) bool {
	_, running, err := startTicks(pid)
	return err == nil && running
}
