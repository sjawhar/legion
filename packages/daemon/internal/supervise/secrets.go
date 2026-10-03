package supervise

import (
	"context"
	"errors"
	"time"
)

// Enrollment is one pod generation's enrollment with the secrets broker.
type Enrollment struct {
	ID          string
	Incarnation string
}

// PodEnrollment is what the machine hands the broker for one pod generation: the pod UID the
// runtime recorded as the locator's incarnation, the identity the shim's hello carried, and the
// agent's session id. It carries no issue: per the shared broker contract
// (dispatch://AGENTC-393/artifact/plan-overview-md), the broker's rules pick a request's approver
// at request time, never at enrollment.
type PodEnrollment struct {
	PodUID, Thumbprint, PodToken, Session string
}

// AgentSecretsIdentity is the pod identity a shim's hello2 carried (stream.AgentSecretsIdentity).
type AgentSecretsIdentity struct {
	Thumbprint, PodToken string
}

// Enroller is the secrets broker's enrollment routes as the machine uses them. Nil in Deps means
// this deployment enrolls no process: no identity is kept, nothing is enrolled or revoked.
type Enroller interface {
	Enroll(ctx context.Context, e PodEnrollment) (string, error)
	Revoke(ctx context.Context, id string) error
}

// PermanentError is an Enroll failure a retry with the same identity cannot change (the broker's
// 4xx); the machine drops the identity and waits for the shim's next hello.
type PermanentError interface{ Permanent() bool }

const (
	// revokeAttempts and revokeRetryDelay bound the revocation of a let-go process's enrollment;
	// past them the enrollment's lease, which nothing renews once the pod is gone, ends it.
	revokeAttempts   = 3
	revokeRetryDelay = 10 * time.Second
)

// heldIdentity is the latest hello2 identity for the claim's current incarnation, kept in memory
// until the enrollment lands: a resume enrolls on the hello itself, a fresh launch once the agent
// registers and the session id is known.
type heldIdentity struct {
	incarnation string
	AgentSecretsIdentity
}

// ensureEnrolled enrolls the claim's current process once the machine holds both halves of its
// identity: the hello's thumbprint and token for this incarnation, and the agent's session id,
// the broker's wake address. It never fails a transition: a refusal is logged with its code, a
// transient one retried on the next Alive observation, a permanent one on the shim's next hello.
// Once enrolled, the id is handed to the shim over the claim's connection, once per connection.
func (m *Machine) ensureEnrolled(ctx context.Context) error {
	if m.deps.Secrets == nil || m.claim.Locator == nil {
		return nil
	}
	incarnation := m.claim.Locator.Incarnation
	if m.claim.Enrollment == nil || m.claim.Enrollment.Incarnation != incarnation {
		if m.identity == nil || m.identity.incarnation != incarnation || m.claim.Session == "" {
			return nil
		}
		enrolling, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
		id, err := m.deps.Secrets.Enroll(enrolling, PodEnrollment{
			PodUID: incarnation, Thumbprint: m.identity.Thumbprint, PodToken: m.identity.PodToken, Session: m.claim.Session,
		})
		cancel()
		if err != nil {
			var permanent PermanentError
			if errors.As(err, &permanent) && permanent.Permanent() {
				m.log.Error("supervise: agent-secrets: enrollment refused; waiting for the shim's next hello", "incarnation", incarnation, "error", err)
				m.identity = nil
			} else {
				m.log.Warn("supervise: agent-secrets: enrollment failed; retrying on the next observation", "incarnation", incarnation, "error", err)
			}
			return nil
		}
		m.claim.Enrollment = &Enrollment{ID: id, Incarnation: incarnation}
		m.identity.PodToken = "" // held no longer than the enrollment needs it
		m.enrollmentSent = false
		m.log.Info("supervise: agent-secrets: enrolled", "incarnation", incarnation, "enrollment", id, "thumbprint", m.identity.Thumbprint)
		if err := m.persist(ctx); err != nil {
			return err
		}
	}
	if m.enrollmentSent {
		return nil
	}
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		return nil
	}
	sending, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	err := conn.AgentSecretsEnrollment(sending, m.claim.Enrollment.ID)
	cancel()
	if err != nil {
		m.log.Warn("supervise: agent-secrets: the shim did not take the enrollment; retrying on the next observation", "enrollment", m.claim.Enrollment.ID, "error", err)
		return nil
	}
	m.enrollmentSent = true
	return nil
}

// revoke ends the let-go process's enrollment at the broker: the first attempt on a goroutine of
// the machine's (counted like a send, so Wait covers it), each later one in a clock callback
// revokeRetryDelay after the last failure, revokeAttempts in all. No goroutine waits on a broker
// outage, and a fake clock drives the retries in tests. Past the last attempt the lease ends the
// enrollment; the give-up line says so.
func (m *Machine) revoke(e Enrollment) {
	if m.deps.Secrets == nil {
		m.log.Warn("supervise: agent-secrets: enrollment recorded with no broker configured; its lease ends it", "enrollment", e.ID)
		return
	}
	m.goroutines++
	go func() {
		defer func() {
			m.mu.Lock()
			m.goroutines--
			m.idle.Broadcast()
			m.mu.Unlock()
		}()
		m.revokeAttempt(e, 1)
	}()
}

// revokeAttempt is one DELETE of the enrollment. It reads nothing the lock guards — deps, the
// logger, and the machine's context are fixed at construction — so a clock callback may run it.
func (m *Machine) revokeAttempt(e Enrollment, attempt int) {
	revoking, cancel := context.WithTimeout(m.ctx, m.deps.Timeouts.RPC)
	err := m.deps.Secrets.Revoke(revoking, e.ID)
	cancel()
	if err == nil {
		m.log.Info("supervise: agent-secrets: enrollment revoked", "enrollment", e.ID, "incarnation", e.Incarnation, "attempt", attempt)
		return
	}
	m.log.Warn("supervise: agent-secrets: revocation failed", "enrollment", e.ID, "attempt", attempt, "error", err)
	if attempt >= revokeAttempts || m.ctx.Err() != nil {
		m.log.Error("supervise: agent-secrets: enrollment not revoked after every attempt; the lease ends it", "enrollment", e.ID, "attempts", attempt)
		return
	}
	m.deps.Clock.AfterFunc(revokeRetryDelay, func() { m.revokeAttempt(e, attempt+1) })
}
