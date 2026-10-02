package sandbox

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"maps"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/stream"
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
}

type launcherSession struct {
	owner *launchers
	token claim.Token
	id    string

	mu      sync.Mutex
	conn    net.Conn
	write   *shimwire.Writer
	state   shimwire.LauncherState
	ready   chan struct{}
	waiters map[string]chan shimwire.Frame
	closed  bool
}

func newLaunchers() *launchers { return &launchers{sessions: map[claim.Token]*launcherSession{}} }

// LauncherResolver is registered with the worker stream after Runtime construction. It validates
// the role-private Secret token, its durable pod UID binding and the current controller-owned pod
// before a launcher can influence a role process.
func (r *Runtime) LauncherResolver() stream.LauncherResolver {
	return func(hello shimwire.LauncherHello) (stream.LauncherHandler, string) {
		role := claim.Role(hello.Role)
		if !claim.IsRole(role) {
			return nil, "launcher role is not a Legion role"
		}
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(ctx, roleSecretName(hello.Sandbox, role), metav1.GetOptions{})
		if err != nil {
			return nil, "launcher credential is unavailable"
		}
		if subtle.ConstantTimeCompare(secret.Data[LauncherTokenFile], []byte(hello.Token)) != 1 {
			return nil, "launcher token is not current"
		}
		if secret.Annotations[launcherPodUIDAnnotation] != hello.PodUID {
			return nil, "launcher pod UID is not bound to its Secret epoch"
		}
		token, err := claim.NewToken(r.project, secret.Annotations[annotationIssue], role)
		if err != nil || SandboxName(token) != hello.Sandbox {
			return nil, "launcher credential does not name this issue role"
		}
		view, err := r.view(hello.Sandbox)
		if err != nil || view.sandbox == nil || view.pod == nil || string(view.pod.UID) != hello.PodUID {
			return nil, "launcher pod is not the current controller-owned pod"
		}
		return r.launchers.accept(token, hello), ""
	}
}

// accept registers a launcher connection. The newest authenticated connection for a role wins:
// a half-open predecessor is closed so its pending requests fail instead of waiting forever.
func (d *launchers) accept(token claim.Token, hello shimwire.LauncherHello) *launcherSession {
	s := &launcherSession{owner: d, token: token, id: hello.LauncherID, ready: make(chan struct{}), waiters: map[string]chan shimwire.Frame{}}
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
	for {
		line, err := reader.ReadBytes('\n')
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
			s.deliver(value.ID, value)
		case shimwire.LauncherStopResult:
			s.deliver(value.ID, value)
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

func (d *launchers) state(token claim.Token) (shimwire.LauncherState, bool) {
	d.mu.Lock()
	session := d.sessions[token]
	d.mu.Unlock()
	if session == nil {
		return shimwire.LauncherState{}, false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return shimwire.LauncherState{}, false
	}
	return session.state, true
}

func (d *launchers) await(ctx context.Context, token claim.Token) (*launcherSession, error) {
	for {
		d.mu.Lock()
		session := d.sessions[token]
		d.mu.Unlock()
		if session != nil {
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

func (d *launchers) request(ctx context.Context, token claim.Token, frame shimwire.Frame, id string) (shimwire.Frame, error) {
	session, err := d.await(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("launcher %s did not connect: %w", token, err)
	}
	waiter := make(chan shimwire.Frame, 1)
	session.mu.Lock()
	if session.closed || session.write == nil {
		session.mu.Unlock()
		return nil, errors.New("launcher connection closed")
	}
	session.waiters[id] = waiter
	err = session.write.WriteFrame(frame)
	if err != nil {
		delete(session.waiters, id)
	}
	session.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case answer, ok := <-waiter:
		if !ok {
			return nil, errors.New("launcher connection closed before its answer")
		}
		return answer, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *launchers) start(ctx context.Context, token claim.Token, command shimwire.LauncherStart) error {
	answer, err := d.request(ctx, token, command, command.ID)
	if err != nil {
		return err
	}
	result, ok := answer.(shimwire.LauncherStartResult)
	if !ok {
		return fmt.Errorf("launcher %s start: got %T", token, answer)
	}
	if !result.OK || result.RunningGeneration != command.Generation {
		return fmt.Errorf("launcher %s start generation %d: %s", token, command.Generation, result.Error)
	}
	d.mu.Lock()
	session := d.sessions[token]
	d.mu.Unlock()
	if session != nil {
		session.mu.Lock()
		if child := session.state.Child; child == nil || child.Generation < command.Generation {
			session.state.Child = &shimwire.LauncherChild{Generation: command.Generation}
		}
		session.mu.Unlock()
	}
	return nil
}

func (d *launchers) stop(ctx context.Context, token claim.Token, command shimwire.LauncherStop) error {
	answer, err := d.request(ctx, token, command, command.ID)
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
	d.mu.Lock()
	session := d.sessions[token]
	d.mu.Unlock()
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

func (r *Runtime) bindLauncherSecrets(ctx context.Context, s *sandbox, l launch, uid string) error {
	for _, role := range claim.Roles {
		name := roleSecretName(s.Name, role)
		updating, cancel := call(ctx)
		secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(updating, name, metav1.GetOptions{})
		cancel()
		if err != nil {
			return fmt.Errorf("bind launcher secret %s: %w", name, err)
		}
		if !ownedBySandbox(secret.OwnerReferences, s.UID) {
			return fmt.Errorf("bind launcher secret %s: it is not owned by Sandbox %s", name, s.Name)
		}
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[launcherPodUIDAnnotation] = uid
		updating, cancel = call(ctx)
		_, err = r.kube.CoreV1().Secrets(r.namespace).Update(updating, secret, metav1.UpdateOptions{})
		cancel()
		if err != nil {
			return fmt.Errorf("bind launcher secret %s to pod UID: %w", name, err)
		}
	}
	return nil
}

// launcherBound reports whether pod is an issue pod whose role launchers this runtime bound: every
// role Secret's pod-UID binding names it. A running pod an older runtime made (one worker container,
// no launchers) or one whose binding never landed is replaced rather than trusted.
func (r *Runtime) launcherBound(ctx context.Context, s *sandbox, pod *corev1.Pod) bool {
	for _, role := range claim.Roles {
		reading, cancel := call(ctx)
		secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(reading, roleSecretName(s.Name, role), metav1.GetOptions{})
		cancel()
		if err != nil || !ownedBySandbox(secret.OwnerReferences, s.UID) || secret.Annotations[launcherPodUIDAnnotation] != string(pod.UID) {
			return false
		}
	}
	return true
}

func launcherCommand(l launch, r *Runtime) shimwire.LauncherStart {
	dir := generationDir(l.spec.Generation)
	shim := []string{r.tools.Legion, "worker-shim", "--connect", r.streamURL, "--boot-token-file", dir + "/" + bootTokenKey, "--pod-safety"}
	if r.mountsProviders() {
		shim = append(shim, "--provider-env-dir", ProvidersDir)
	}
	if r.agentSecrets != nil {
		shim = append(shim, "--agent-secrets-key-dir", AgentSecretsKeyDir, "--pod-token-file", AgentSecretsTokenDir+"/"+AgentSecretsTokenFile, "--agent-secrets-bin", r.tools.AgentSecrets)
	}
	env := r.mainEnvironment(l, "!"+r.tools.Legion+" credential")
	values := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if entry.ValueFrom == nil {
			values = append(values, entry.Name+"="+entry.Value)
		}
	}
	values = append(values, bootTokenKey+"_FILE="+dir+"/"+bootTokenKey)
	files := maps.Clone(l.secrets)
	if files == nil {
		files = map[string]string{}
	}
	files[bootTokenKey] = l.spec.BootToken
	return shimwire.LauncherStart{
		ID: "start-" + fmt.Sprint(l.spec.Generation), Generation: l.spec.Generation,
		Argv: append(shim, append([]string{"--"}, l.agentArgv(r.agent)...)...), Env: values, Files: files,
		ResumeFile: l.resumeFile,
	}
}
