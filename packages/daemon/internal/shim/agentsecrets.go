package shim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// AgentSecrets is a pod's enrollment with the secrets broker as the shim carries it:
// the key directory (a memory-backed volume the runtime mounts), the projected token file, and
// the `agent-secrets` binary. Nil on a tmux pane.
type AgentSecrets struct {
	KeyDir, TokenFile, Binary string
}

// enrollmentFile is what `agent-secrets` reads beside key.pem (Plan A Task 14: `<key dir>/enrollment`).
const enrollmentFile = "enrollment"

// keygenTimeout bounds `agent-secrets keygen`, a local key generation.
const keygenTimeout = 30 * time.Second

// renewRestartDelay is the wait before a renewer that exited is started again.
const renewRestartDelay = 5 * time.Second

// permanentRenewExitCode is agent-secrets' documented exitRenewRefused. It crosses the process
// boundary because a terminal broker refusal (a pod renewal without its token, or an enrollment
// that has ended) must stop the shim rather than crash-looping a renewer that cannot succeed.
const permanentRenewExitCode = 76

// lastLineWriter forwards a child stream while retaining its final nonblank line for the shim's
// terminal log. exec.Cmd waits for its copy goroutine before Wait returns, so Last is complete
// when startRenewer classifies the child's exit.
type lastLineWriter struct {
	partial string
	last    string
}

func (w *lastLineWriter) Write(p []byte) (int, error) {
	lines := strings.Split(w.partial+string(p), "\n")
	for _, line := range lines[:len(lines)-1] {
		if line = strings.TrimSpace(line); line != "" {
			w.last = line
		}
	}
	w.partial = lines[len(lines)-1]
	return len(p), nil
}

func (w *lastLineWriter) Last() string {
	if line := strings.TrimSpace(w.partial); line != "" {
		return line
	}
	return w.last
}

// identity runs `agent-secrets keygen --out <key dir>` once and reads the projected token: the
// hello2 payload. It runs before the first dial, so a pod that cannot generate its key never
// registers as one that has. The thumbprint is keygen's stdout, trimmed.
func (s *shim) identity() (*shimwire.AgentSecretsHello, error) {
	a := s.cfg.AgentSecrets
	if a == nil {
		return nil, nil
	}
	s.mu.Lock()
	thumbprint := s.thumbprint
	s.mu.Unlock()
	if thumbprint == "" {
		ctx, cancel := context.WithTimeout(s.loop, keygenTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, a.Binary, "keygen", "--out", a.KeyDir)
		cmd.Env = s.cfg.Env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("agent-secrets keygen --out %s: %w: %s", a.KeyDir, err, strings.TrimSpace(stderr.String()))
		}
		thumbprint = strings.TrimSpace(stdout.String())
		if thumbprint == "" {
			return nil, fmt.Errorf("agent-secrets keygen --out %s printed no thumbprint", a.KeyDir)
		}
		s.mu.Lock()
		s.thumbprint = thumbprint
		s.mu.Unlock()
		s.log.Printf("[worker-shim] agent-secrets: key %s generated in %s", thumbprint, a.KeyDir)
	}
	token, err := readPodToken(a)
	if err != nil {
		return nil, err
	}
	return &shimwire.AgentSecretsHello{Thumbprint: thumbprint, PodToken: token}, nil
}

// readPodToken reads the projected token from its mount. The kubelet rewrites the file before the
// token expires, so each read is the token as it is now.
func readPodToken(a *AgentSecrets) (string, error) {
	token, err := os.ReadFile(a.TokenFile)
	if err != nil {
		return "", fmt.Errorf("agent-secrets: read the projected token %s: %w", a.TokenFile, err)
	}
	return strings.TrimSpace(string(token)), nil
}

// podToken is the daemon's agent-secrets-token frame: the projected token is read again and
// answered, for an enrollment the daemon makes after the token the hello carried has expired. A
// pane with no key directory answers a refusal naming the flag, as enroll does.
func (s *shim) podToken(f shimwire.AgentSecretsToken) {
	answer := func(token string, err error) {
		result := shimwire.AgentSecretsTokenResult{ID: f.ID, OK: err == nil, PodToken: token}
		if err != nil {
			result.Error = err.Error()
			s.log.Printf("[worker-shim] agent-secrets: token request refused: %v", err)
		}
		s.toDaemon(result)
	}
	if err := f.Validate(); err != nil {
		answer("", err)
		return
	}
	if s.cfg.AgentSecrets == nil {
		answer("", errors.New("this shim was started without --agent-secrets-key-dir; it has no projected token"))
		return
	}
	answer(readPodToken(s.cfg.AgentSecrets))
}

// enroll is the daemon's agent-secrets-enrollment frame: the id is written to <key dir>/enrollment
// (0600, written whole and renamed into place), the renewer is started if it is not running, and
// the frame is answered. A pane with no key directory answers a refusal naming the flag: the
// daemon is the only sender, and the pane is not one it should have enrolled.
func (s *shim) enroll(f shimwire.AgentSecretsEnrollment) {
	answer := func(err error) {
		result := shimwire.AgentSecretsEnrollmentResult{ID: f.ID, OK: err == nil}
		if err != nil {
			result.Error = err.Error()
			s.log.Printf("[worker-shim] agent-secrets: enrollment %s refused: %v", f.EnrollmentID, err)
		}
		s.toDaemon(result)
	}
	if err := f.Validate(); err != nil {
		answer(err)
		return
	}
	a := s.cfg.AgentSecrets
	if a == nil {
		answer(errors.New("this shim was started without --agent-secrets-key-dir; it enrolls nothing"))
		return
	}
	if err := writeEnrollment(a.KeyDir, f.EnrollmentID); err != nil {
		answer(err)
		return
	}
	s.log.Printf("[worker-shim] agent-secrets: enrolled as %s", f.EnrollmentID)
	s.startRenewer(f.EnrollmentID)
	answer(nil)
}

func writeEnrollment(dir, id string) error {
	temporary, err := os.CreateTemp(dir, enrollmentFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", filepath.Join(dir, enrollmentFile), err)
	}
	name := temporary.Name()
	_, writeErr := temporary.WriteString(id + "\n")
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", filepath.Join(dir, enrollmentFile), err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, enrollmentFile)); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// startRenewer runs `agent-secrets renew --pod-token-file <projected token file>` — the lease
// heartbeat that proves the pod live to the broker — as a child of the shim, restarted after
// renewRestartDelay only when it exits transiently, until the shim ends. Each renewal carries the
// pod's current projected token, which the broker checks names the pod UID and service account the
// enrollment was made for; a terminal broker refusal exits 76 and stops the renewer. The shim and
// agent-secrets ship in one worker image, so the renewer always knows the flag. It reads
// AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR from the shim's own environment, where the runtime
// set them. One renewer per shim: a second enrollment frame starts no second one.
func (s *shim) startRenewer(enrollmentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renewing {
		return
	}
	s.renewing = true
	a := s.cfg.AgentSecrets
	s.renewers.Add(1)
	go func() {
		defer s.renewers.Done()
		for s.loop.Err() == nil {
			cmd := exec.CommandContext(s.loop, a.Binary, "renew", "--pod-token-file", a.TokenFile)
			tail := &lastLineWriter{}
			cmd.Env = s.cfg.Env
			cmd.Stdout = os.Stderr
			cmd.Stderr = io.MultiWriter(os.Stderr, tail)
			if err := cmd.Start(); err != nil {
				s.log.Printf("[worker-shim] agent-secrets: renewer could not start: %v; retrying in %s", err, renewRestartDelay)
			} else {
				s.log.Printf("[worker-shim] agent-secrets: renewer started (pid %d)", cmd.Process.Pid)
				err := cmd.Wait()
				if s.loop.Err() != nil {
					return
				}
				var exited *exec.ExitError
				if errors.As(err, &exited) && exited.ExitCode() == permanentRenewExitCode {
					s.log.Printf("[worker-shim] agent-secrets: renewer permanently refused for enrollment %s (exit %d): %q; not restarting", enrollmentID, permanentRenewExitCode, tail.Last())
					return
				}
				s.log.Printf("[worker-shim] agent-secrets: renewer exited (%v); restarting in %s", err, renewRestartDelay)
			}
			select {
			case <-s.clock.After(renewRestartDelay):
			case <-s.loop.Done():
				return
			}
		}
	}()
}
