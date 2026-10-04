package shim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	token, err := os.ReadFile(a.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("agent-secrets: read the projected token %s: %w", a.TokenFile, err)
	}
	return &shimwire.AgentSecretsHello{Thumbprint: thumbprint, PodToken: strings.TrimSpace(string(token))}, nil
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
	s.startRenewer()
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

// startRenewer runs `agent-secrets renew` — the lease heartbeat that proves the pod live to the
// broker — as a child of the shim, restarted after renewRestartDelay whenever it exits, until the
// shim ends. It reads AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR from the shim's own environment,
// where the runtime set them. One renewer per shim: a second enrollment frame starts no second one.
func (s *shim) startRenewer() {
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
			cmd := exec.CommandContext(s.loop, a.Binary, "renew")
			cmd.Env = s.cfg.Env
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			if err := cmd.Start(); err != nil {
				s.log.Printf("[worker-shim] agent-secrets: renewer could not start: %v; retrying in %s", err, renewRestartDelay)
			} else {
				s.log.Printf("[worker-shim] agent-secrets: renewer started (pid %d)", cmd.Process.Pid)
				err := cmd.Wait()
				if s.loop.Err() != nil {
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
