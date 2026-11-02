package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// launcherPodUIDAnnotation binds a role Secret's launcher token to the pod it was issued for; a
// launcher of any other pod is refused. annotationIssue carries the exact issue key (labels are
// lowercased), from which the resolver derives the claim token.
const (
	launcherPodUIDAnnotation = "legion.dev/launcher-pod-uid"
	annotationIssue          = "legion.dev/issue"
)

type launchers struct {
	mu       sync.Mutex
	sessions map[claim.Token]*launcherSession

	// saved is the bundle an issue's last release left and restored the confirmation that drops
	// it, the daemon's store behind both (Options.SavedWorkspaceBundle, WorkspaceBundleRestored).
	// A launchers built without the pair restores nothing.
	saved    runtime.SavedWorkspaceBundle
	restored runtime.WorkspaceBundleRestored
	log      *slog.Logger
}

type launcherSession struct {
	owner *launchers
	token claim.Token
	// issue is the exact issue key this launcher's pod is of, as the role Secret's annotation
	// carried it to the hello's authentication (launcherCredential.issue): the key the release
	// saved that workspace's bundle under. "" for a pod of no issue, the project controller's.
	issue  string
	id     string
	podUID string

	mu      sync.Mutex
	conn    net.Conn
	write   *shimwire.Writer
	state   shimwire.LauncherState
	ready   chan struct{}
	waiters map[string]chan shimwire.Frame
	closed  bool
}

func newLaunchers(saved runtime.SavedWorkspaceBundle, restored runtime.WorkspaceBundleRestored, log *slog.Logger) *launchers {
	return &launchers{sessions: map[claim.Token]*launcherSession{}, saved: saved, restored: restored, log: log}
}

// accept registers a launcher connection of the pod of issue. The newest authenticated connection
// for a role wins: a half-open predecessor is closed so its pending requests fail instead of
// waiting forever.
func (d *launchers) accept(token claim.Token, issue string, hello shimwire.LauncherHello) *launcherSession {
	s := &launcherSession{owner: d, token: token, issue: issue, id: hello.LauncherID, podUID: hello.PodUID,
		ready: make(chan struct{}), waiters: map[string]chan shimwire.Frame{}}
	d.mu.Lock()
	old := d.sessions[token]
	d.sessions[token] = s
	d.mu.Unlock()
	if old != nil {
		old.close()
	}
	return s
}

func (s *launcherSession) ServeLauncher(conn net.Conn, reader *bufio.Reader, writer *shimwire.Writer) {
	s.mu.Lock()
	s.conn, s.write = conn, writer
	closed := s.closed
	s.mu.Unlock()
	defer s.close()
	if closed {
		return
	}
	frames := shimwire.NewReader(reader)
	for {
		line, err := frames.ReadLine()
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			return
		}
		switch value := frame.(type) {
		case shimwire.LauncherState:
			s.mu.Lock()
			s.state = value
			select {
			case <-s.ready:
			default:
				close(s.ready)
			}
			s.mu.Unlock()
		case shimwire.LauncherStartResult:
			s.mu.Lock()
			if value.OK && (s.state.LastExit == nil || s.state.LastExit.Generation < value.RunningGeneration) &&
				(s.state.Child == nil || s.state.Child.Generation < value.RunningGeneration) {
				s.state.Child = &shimwire.LauncherChild{Generation: value.RunningGeneration}
			}
			s.mu.Unlock()
			s.deliver(value.ID, value)
		case shimwire.LauncherStopResult:
			s.deliver(value.ID, value)
		case shimwire.LauncherBundleResult:
			s.deliver(bundleWaiter(value.ID, value.Index), value)
		case shimwire.LauncherRestored:
			s.owner.restoreReported(s, value)
		default:
			return
		}
	}
}

func (s *launcherSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, waiter := range s.waiters {
		close(waiter)
	}
	s.waiters = nil
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	s.owner.remove(s.token, s)
}

func (s *launcherSession) deliver(id string, frame shimwire.Frame) {
	s.mu.Lock()
	waiter := s.waiters[id]
	delete(s.waiters, id)
	s.mu.Unlock()
	if waiter != nil {
		waiter <- frame
		close(waiter)
	}
}

func (d *launchers) remove(token claim.Token, session *launcherSession) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sessions[token] == session {
		delete(d.sessions, token)
	}
}

func (d *launchers) state(token claim.Token, podUID string) (shimwire.LauncherState, bool) {
	d.mu.Lock()
	session := d.sessions[token]
	d.mu.Unlock()
	if session == nil || session.podUID != podUID {
		return shimwire.LauncherState{}, false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return shimwire.LauncherState{}, false
	}
	select {
	case <-session.ready:
	default:
		return shimwire.LauncherState{}, false
	}
	return session.state, true
}

func (d *launchers) await(ctx context.Context, token claim.Token, podUID string) (*launcherSession, error) {
	for {
		d.mu.Lock()
		session := d.sessions[token]
		d.mu.Unlock()
		if session != nil && session.podUID == podUID {
			select {
			case <-session.ready:
				session.mu.Lock()
				closed := session.closed
				session.mu.Unlock()
				if !closed {
					return session, nil
				}
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		select {
		case <-time.After(25 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (d *launchers) request(ctx context.Context, token claim.Token, podUID string, frame shimwire.Frame, id string) (shimwire.Frame, *launcherSession, error) {
	session, err := d.await(ctx, token, podUID)
	if err != nil {
		return nil, nil, fmt.Errorf("launcher %s did not connect: %w", token, err)
	}
	waiter := make(chan shimwire.Frame, 1)
	session.mu.Lock()
	if session.closed || session.write == nil {
		session.mu.Unlock()
		return nil, nil, errors.New("launcher connection closed")
	}
	session.waiters[id] = waiter
	deadline := time.Now().Add(apiTimeout)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	err = session.conn.SetWriteDeadline(deadline)
	if err == nil {
		stopCancel := context.AfterFunc(ctx, func() { _ = session.conn.Close() })
		err = session.write.WriteFrame(frame)
		stopCancel()
		_ = session.conn.SetWriteDeadline(time.Time{})
	}
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		delete(session.waiters, id)
		session.mu.Unlock()
	}()
	if err != nil {
		session.close()
		return nil, nil, err
	}
	select {
	case answer, ok := <-waiter:
		if !ok {
			return nil, nil, errors.New("launcher connection closed before its answer")
		}
		d.mu.Lock()
		current := d.sessions[token]
		sameProcess := current != nil && current.podUID == podUID && current.id == session.id
		d.mu.Unlock()
		if !sameProcess {
			return nil, nil, errors.New("launcher process changed before its answer was confirmed")
		}
		return answer, session, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// startRefusal is a Start the launcher answered OK: false, without starting the generation: the one
// start error after which no child of that generation can be running (stopIssuedChild).
type startRefusal struct {
	token      claim.Token
	generation uint64
	reason     string
}

func (e startRefusal) Error() string {
	return fmt.Sprintf("launcher %s start generation %d: %s", e.token, e.generation, e.reason)
}

func (d *launchers) start(ctx context.Context, token claim.Token, podUID string, command shimwire.LauncherStart) error {
	// The work the release took off the pod this one replaces goes over first: the launcher holds
	// the bundle and restores it into the workspace before it runs the role's command, so the
	// restore has to be in its hands before any generation of the pod is started, this one
	// included (internal/launcher's bundleRestore). It never stops the start.
	d.restoreWorkspace(ctx, token, podUID)
	answer, _, err := d.request(ctx, token, podUID, command, command.ID)
	if err != nil {
		return err
	}
	result, ok := answer.(shimwire.LauncherStartResult)
	if !ok {
		return fmt.Errorf("launcher %s start: got %T", token, answer)
	}
	if !result.OK {
		return startRefusal{token: token, generation: command.Generation, reason: result.Error}
	}
	// An OK answer naming another generation says the launcher started something, and not what it
	// was asked: the Start's outcome is unknown, never a refusal, so the issued generation is stopped.
	if result.RunningGeneration != command.Generation {
		return fmt.Errorf("launcher %s start generation %d: answered OK naming generation %d", token, command.Generation, result.RunningGeneration)
	}
	return nil
}

// bundleRestoreBound is how long the hop of one pod's saved bundle may take. The 40 MiB a release
// carries at most is 160 slices of shimwire.LauncherBundlePayloadBytes, each one round trip the
// launcher answers out of memory, so this is room for a slow connection rather than for the
// common case; a hop that outruns it costs the restore and never the start.
const bundleRestoreBound = 2 * time.Minute

// restoreWorkspace carries the bundle the last release of this pod's issue saved to the pod's
// launcher. The launcher applies it to the workspace before the first role of the pod runs, so
// every call is from a start that has not been issued yet (start), and it reports what the
// restore did in a frame of its own, which is what the saved copy is dropped on
// (restoreReported).
//
// Nothing here stops a role. A slice the launcher refuses, a body that did not arrive as it was
// sent, a store that cannot be read and a connection that fails are each logged and left: a pod
// that never starts is worse than one whose workspace holds only what was pushed, and the
// launcher writes the agent its own notice of what was lost (internal/launcher's restoreWorkspace).
// The bundle stays in the database until a launcher has restored it, so the issue's next pod is
// sent it again.
func (d *launchers) restoreWorkspace(ctx context.Context, token claim.Token, podUID string) {
	if d.saved == nil || d.restored == nil {
		return
	}
	// The session names the issue its pod is of, as the Secret its launcher authenticated with
	// did. A launcher that never arrives is the start's own error to report, not this hop's.
	session, err := d.await(ctx, token, podUID)
	if err != nil || session.issue == "" {
		return
	}
	issue := session.issue
	restoring, cancel := context.WithTimeout(ctx, bundleRestoreBound)
	defer cancel()
	bundle, saved, err := d.saved(restoring, issue)
	if err != nil {
		d.log.Warn("launcher bundle: the saved workspace bundle could not be read, so the pod starts on what was pushed",
			"issue", issue, "launcher", token, "error", err)
		return
	}
	// Nothing was released, the restore is already confirmed, or the release saved a workspace
	// that had nothing unpushed: the pod starts on what its fetch provisioned, as every pod of an
	// issue that was never released does.
	if !saved || len(bundle.Bundle) == 0 {
		return
	}
	frames, err := bundleFrames(issue, bundle)
	if err != nil {
		d.log.Warn("launcher bundle: the saved workspace bundle could not be sliced, so the pod starts on what was pushed",
			"issue", issue, "launcher", token, "error", err)
		return
	}
	for _, frame := range frames {
		answer, _, err := d.request(restoring, token, podUID, frame, bundleWaiter(frame.ID, frame.Index))
		if err != nil {
			d.log.Warn("launcher bundle: a slice of the saved workspace bundle did not reach the launcher, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "slice", frame.Index, "slices", frame.Count, "error", err)
			return
		}
		result, ok := answer.(shimwire.LauncherBundleResult)
		if !ok {
			d.log.Warn("launcher bundle: the launcher answered a bundle slice with another frame, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "slice", frame.Index, "answer", answer.FrameType())
			return
		}
		if !result.OK {
			d.log.Warn("launcher bundle: the launcher refused a slice of the saved workspace bundle, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "slice", frame.Index, "slices", frame.Count, "error", result.Error)
			return
		}
		if !result.Held {
			continue
		}
		// Held ends the sequence: the launcher has the whole, digest-checked body and applies it
		// before its first role runs. The daemon's copy stays until the launcher reports what
		// the restore itself did (restoreReported) — a restore can still fail, and the work is
		// then in the daemon's database alone, which is where the launcher's notice says it is.
		d.log.Info("launcher bundle: the released workspace's saved work reached the pod that replaces it",
			"issue", issue, "launcher", token, "bytes", len(bundle.Bundle), "slices", len(frames))
		return
	}
	d.log.Warn("launcher bundle: the launcher took every slice of the saved workspace bundle without holding the whole",
		"issue", issue, "launcher", token, "slices", len(frames))
}

// restoreReported drops the saved bundle of the issue a launcher's pod is of, once that launcher
// reports that its restore put the work back. It is the one confirmation the bundle is deleted
// on: a restore that failed, and a pod no role ever started in, leave the work in the daemon's
// database for the issue's next pod, which is sent it again. Nothing else ever deletes it, so
// the issue's close and its tree's close are what end a bundle no pod restores (outbox's
// dropReleasedWork).
//
// It runs on the launcher's own frame loop rather than in a goroutine of its own, so a pod whose
// restore is confirmed has its bundle gone before any later frame of that launcher is read; the
// delete is one indexed row.
func (d *launchers) restoreReported(s *launcherSession, report shimwire.LauncherRestored) {
	if d.restored == nil || s.issue == "" {
		return
	}
	if !report.OK {
		d.log.Warn("launcher bundle: the pod's launcher could not restore the released workspace's saved work, which stays in the database for the issue's next pod",
			"issue", s.issue, "launcher", s.token, "sequence", report.ID, "error", report.Error)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	if err := d.restored(ctx, s.issue); err != nil {
		d.log.Warn("launcher bundle: the restored workspace bundle could not be dropped from the store",
			"issue", s.issue, "launcher", s.token, "error", err)
		return
	}
	d.log.Info("launcher bundle: the pod's launcher restored the released workspace's saved work, which is dropped from the database",
		"issue", s.issue, "launcher", s.token, "sequence", report.ID)
}

// bundleFrames are the slices that carry issue's saved bundle. The sequence's id is the issue and
// the digest of the body (shimwire.SplitWorkspaceBundle), so it is the same every time the same
// bundle is sent, which is what a pod whose launcher reconnects needs: the launcher answers a
// slice it has already taken under an id it knows with the answer it gave, Held included, and
// refuses only a payload that differs from the one that id carried — which another bundle of the
// same issue never is, since another body is another digest and so another sequence
// (internal/launcher's bundleRestore.receive).
func bundleFrames(issue string, b runtime.WorkspaceBundle) ([]shimwire.LauncherBundle, error) {
	return shimwire.SplitWorkspaceBundle("restore-"+issue, shimwire.WorkspaceBundle{
		Bundle: b.Bundle, Head: b.Head, Tips: b.Tips, Bookmarks: b.Bookmarks, At: b.At,
	})
}

// bundleWaiter names the request one slice's answer belongs to. A sequence is answered slice by
// slice, so its id alone would name as many answers as it has slices; the launcher keys its own
// results by the same pair.
func bundleWaiter(sequence string, index int) string { return sequence + "/" + strconv.Itoa(index) }

func (d *launchers) stop(ctx context.Context, token claim.Token, podUID string, command shimwire.LauncherStop) error {
	answer, session, err := d.request(ctx, token, podUID, command, command.ID)
	if err != nil {
		return err
	}
	result, ok := answer.(shimwire.LauncherStopResult)
	if !ok {
		return fmt.Errorf("launcher %s stop: got %T", token, answer)
	}
	if !result.OK {
		return fmt.Errorf("launcher %s stop generation %d: %s", token, command.Generation, result.Error)
	}
	// The launcher answers only after the child has exited; its own state frame may follow the
	// answer, so the confirmed stop is recorded now.
	if session != nil {
		session.mu.Lock()
		if child := session.state.Child; child != nil && child.Generation == command.Generation {
			session.state.Child = nil
			session.state.LastExit = &shimwire.LauncherExit{Generation: command.Generation}
		}
		session.mu.Unlock()
	}
	return nil
}

// bindLauncherSecrets binds each launcher role's Secret of the pod l launches to the new pod's uid,
// so only that pod's launchers authenticate with it. A write the API server refuses as a conflict
// — the gh-credential refresher rewrote the Secret between this read and this write
// (refreshGitHubCredential) — is retried once over a fresh read, so the refresher's gh files and the
// binding both land; a launch that failed here instead would leave the new pod running, unbound,
// until the supervisor's next attempt replaced it. Any other failure, or a second conflict, is
// returned.
func (r *Runtime) bindLauncherSecrets(ctx context.Context, s *sandbox, l launch, uid string) error {
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	for _, role := range l.roles {
		name := roleSecretName(s.Name, role)
		read := func() (*corev1.Secret, error) {
			secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("bind launcher secret %s: %w", name, err)
			}
			if !ownedBySandbox(secret.OwnerReferences, s.UID) {
				return nil, fmt.Errorf("bind launcher secret %s: it is not owned by Sandbox %s", name, s.Name)
			}
			return secret, nil
		}
		secret, err := read()
		if err != nil {
			return err
		}
		for retried := false; ; retried = true {
			if secret.Annotations == nil {
				secret.Annotations = map[string]string{}
			}
			secret.Annotations[launcherPodUIDAnnotation] = uid
			_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
			if err == nil {
				break
			}
			if !apierrors.IsConflict(err) || retried {
				return fmt.Errorf("bind launcher secret %s to pod UID: %w", name, err)
			}
			if secret, err = read(); err != nil {
				return err
			}
		}
		r.cacheLauncherCredential(s, secret)
	}
	return nil
}

// launcherBound reports whether pod is one whose launchers this runtime bound: the Secret of every
// role of l's pod, its launcher roles, exists, is the Sandbox's, binds pod by uid, and records its
// gh bundle's default owner (GitHubDefaultKey, empty for a controller whose project has no GitHub
// owner), which every role of one pod shares and l takes as its gitHubDefault, every agent's
// GH_CONFIG_DIR. A running pod an older runtime made (one worker container, no launchers, or one
// gh directory where a bundle now stands) or one whose binding never landed is replaced rather than
// trusted. Any other failure to read a Secret, such as the client's timeout, or a throttled request
// or a server error the launch's deadline for it outlasted (resend), proves nothing about the pod:
// it is returned, so only the launch that read it fails, and the caller stops that role's previous
// process (runtime.Runtime's Spawn and Resume); the pod and every other role running in it are left
// as they are. Each Secret it finds bound to pod, holding a launcher token, is held as its role's
// credential (cacheLauncherCredential), so the launcher a kept pod's launch waits for is not
// refused for a read of its own that failed earlier (credentialRetry).
func (r *Runtime) launcherBound(ctx context.Context, s *sandbox, pod *corev1.Pod, l *launch) (bool, error) {
	for _, role := range l.roles {
		name := roleSecretName(s.Name, role)
		secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("read launcher secret %s: %w", name, err)
		case !ownedBySandbox(secret.OwnerReferences, s.UID) || secret.Annotations[launcherPodUIDAnnotation] != string(pod.UID):
			return false, nil
		}
		recorded, ok := secret.Data[GitHubDefaultKey]
		if !ok {
			return false, nil
		}
		l.gitHubDefault = string(recorded)
		if len(secret.Data[LauncherTokenFile]) > 0 {
			r.cacheLauncherCredential(s, secret)
		}
	}
	return true, nil
}

// launcherCommand is the start command a role's launcher receives for one generation: the shim's
// argv (its flags, then `--`, then the agent's own argv), the generation's plain environment, the
// launch credentials it writes into its private directory, and the session a resume continues.
// The shim is told the pod's stop grace, the same TerminationGrace its launcher runs with, so the
// wait it makes for an in-flight CodeGraph warm-up on its way out stays inside the grace after
// which the launcher kills it. A tree role's shim is told --warm-codegraph: once its Oh My Pi has
// started it builds the CodeGraph index of the workspace LEGION_WORKSPACE names, which
// launchEnvironment carries for every role but the controller (issuePod.agentEnv, podkind.go),
// whose pod has no workspace. When the launch installs a stale-plugin relaunch's pair (l.spec.Plugins
// set, LEGION-164) or the deployment names a setup command (runtime.kubernetes.setup, r.setup), the
// agent's own argv is itself wrapped in the start script (startArgv): the shim's child is then
// `/bin/sh -c '<script>' legion-start <n> <setup…> <agent argv…>`, which installs the pair, runs
// the setup command, and execs the agent, so the shim's own preparation (--pod-safety) has
// already run and no new shim flag is needed; after the script's `exec` the child is the harness
// itself, so the shim's pid is unchanged. The setup command runs at every start of the role,
// a Spawn's, a Resume's and a replaced pod's alike. A Claude Code launch is wrapped whatever else
// it carries, for the `mkdir -p` of its configuration directory (claudeConfigDir) and the install
// of the pinned Claude Code its agent command names (claudeCodeBlock: the image carries the pin
// and never the binary), and adds one file to the generation's credentials: the MCP document its
// agent command reads
// (claudeMCPFile), which the launcher writes 0600 into the 0700 private directory it makes for
// the generation and removes with the rest when the generation ends (internal/launcher). It
// travels in Files and never in spec.Secrets, which would be two wrongs: every secret name also
// gets a `<NAME>_FILE` pointer in the worker's environment (mainEnvironment), and
// ValidateSpawnSpec refuses a credential-shaped name outright.
func launcherCommand(l launch, r *Runtime) shimwire.LauncherStart {
	dir := generationDir(l.spec.Generation)
	shim := []string{r.tools.Legion, "worker-shim", connectFlag, r.streamURL, "--boot-token-file", dir + "/" + bootTokenKey, "--pod-safety", "--stop-grace", r.terminationGrace.String()}
	if l.spec.Role != claim.RoleController {
		shim = append(shim, "--warm-codegraph")
	}
	// The shim is told the providers directory only where the pod mounts the providers Secret there,
	// which it does when something needs it (mountsProviders: a provider key, a providers secret, or
	// a session database URL key); a directory it is told and cannot read refuses its start
	// (shim.ReadProviderEnv). Under cloudsql-iam no URL key is held — each pod signs in to Cloud SQL
	// as itself — so a deployment with no provider key and no providers secret has no such directory.
	if r.mountsProviders() {
		shim = append(shim, "--provider-env-dir", ProvidersDir)
	}
	if r.agentSecrets != nil {
		shim = append(shim, "--agent-secrets-key-dir", AgentSecretsKeyDir, "--pod-token-file", AgentSecretsTokenDir+"/"+AgentSecretsTokenFile, "--agent-secrets-bin", r.tools.AgentSecrets)
	}
	_, values := r.launchEnvironment(l)
	values = append(values, bootTokenKey+"_FILE="+dir+"/"+bootTokenKey)
	files := maps.Clone(l.secrets)
	if files == nil {
		files = map[string]string{}
	}
	files[bootTokenKey] = l.spec.BootToken
	claudeDir := ""
	if l.runsClaude() {
		claudeDir = claudeConfigDir(l.spec.Role)
		files[claudeMCPFile] = l.claude.mcp
	}
	agentArgv := startArgv(roleStateHome(l.spec.Role), l.spec.Generation, l.spec.Plugins, claudeDir, r.setup, l.agentArgv(r.agent),
		installLockWait(r.bootTimeout, r.registrationIntervals))
	return shimwire.LauncherStart{
		ID: "start-" + fmt.Sprint(l.spec.Generation), Generation: l.spec.Generation,
		Argv: append(shim, append([]string{"--"}, agentArgv...)...), Env: values, Files: files,
		ResumeFile: l.spec.ResumeSessionFile,
	}
}
