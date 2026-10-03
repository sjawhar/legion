//go:build e2e

// The Stage 4a harness's agent-secrets checks (AGENTC-393, spec Testing 10(b); red-team condition
// 2): two role processes of one verified pod each enroll at a distinct role-generation slot and
// cannot cross-use grants; a copied projected token for another pod, an old pod UID, and a
// self-enrollment from inside a pod all fail; the daemon's revocation ends an old pod's access when
// the pod is gone. The harness plays the daemon's part exactly as supervise.Machine does: enroll
// on the hello's identity with its recorded PodUID and role-generation slot, hand the id to the shim
// over the stream, revoke on Gone. The rig is live_test.go.
//
// secretsBlocked and shellJoin's real logic live in live_secrets_pure_test.go, which carries no
// e2e build tag so ordinary `go test ./...` exercises it directly (live_secrets_unit_test.go);
// secretsBlocked here is the thin e2e-tagged wrapper liveChecks' `blocked` field needs.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// secretsBlocked reports r.secretsBlockReason first — the attended machine login newLiveRig ran
// timed out, was denied, or expired — and falls back to agentSecretsBlockReason for a run whose
// inputs never attempted one at all.
func secretsBlocked(r *liveRig) string {
	if r.secretsBlockReason != "" {
		return r.secretsBlockReason
	}
	return agentSecretsBlockReason(r.env.agentSecretsURL, r.env.agentSecretsOperator, r.env.agentSecretsAutoSHA, r.env.agentSecretsBin)
}

// ensureEnrolled enrolls c's running role process unless the harness already did for this
// incarnation — what the daemon's machine does on the role's hello. Every check that needs an
// enrollment starts here, so the suspend, resume and kill checks between them, which relaunch
// processes the harness does not enroll, leave nothing stale behind.
func (r *liveRig) ensureEnrolled(c *liveClaim) (string, error) {
	if e, ok := r.enrollments[c.token]; ok && e.incarnation == c.loc.Incarnation {
		return e.id, nil
	}
	regs := r.reg.registrations(c.token)
	if len(regs) == 0 {
		return "", fmt.Errorf("%s registered no hello", c.token)
	}
	return r.enroll(c, regs[len(regs)-1])
}

func agentSecretsSlot(c *liveClaim) string {
	return fmt.Sprintf("%s-g%d", c.role, c.loc.Sandbox.Generation)
}

// enroll is the daemon's enrollment of c's role process: the identity its hello carried, the
// actual pod UID separate from its composed process incarnation, the role-generation slot and a
// session id the harness mints (the stub agent registers none). It carries no issue: the broker's
// rules pick a request's approver at request time, never at enrollment. The id goes to the shim
// over the claim's connection. An enrollment the harness holds for an earlier incarnation of the
// claim is revoked first, as letGo would have when that process went.
func (r *liveRig) enroll(c *liveClaim, reg registration) (string, error) {
	if reg.identity == nil {
		return "", fmt.Errorf("the hello of %s at generation %d carried no agent-secrets identity", c.token, reg.gen)
	}
	if err := r.revoke(c); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	enrolled, err := r.secrets.Enroll(ctx, agentsecrets.PodEnrollment{
		PodUID: c.loc.Sandbox.PodUID, Slot: agentSecretsSlot(c), Thumbprint: reg.identity.Thumbprint, PodToken: reg.identity.PodToken,
		Session: "s4a-" + string(c.token),
	})
	if err != nil {
		return "", fmt.Errorf("enroll %s (pod %s, slot %s): %w", c.token, short(c.loc.Sandbox.PodUID), agentSecretsSlot(c), err)
	}
	note("harness", "enrolled %s pod %s slot %s as %s (thumbprint %s)", c.token, short(c.loc.Sandbox.PodUID), agentSecretsSlot(c), enrolled.ID, reg.identity.Thumbprint)
	conn, ok := r.ln.Conn(c.token)
	if !ok {
		return "", fmt.Errorf("%s has no connection to hand the enrollment to", c.token)
	}
	if err := conn.AgentSecretsEnrollment(ctx, enrolled.ID); err != nil {
		return "", fmt.Errorf("hand %s its enrollment: %w", c.token, err)
	}
	r.enrollments[c.token] = liveEnrollment{id: enrolled.ID, incarnation: c.loc.Incarnation}
	return enrolled.ID, nil
}

// revoke is the daemon's letGo for c's enrollment; nothing to do when the harness holds none.
func (r *liveRig) revoke(c *liveClaim) error {
	e, ok := r.enrollments[c.token]
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	if err := r.secrets.Revoke(ctx, e.id); err != nil {
		return err
	}
	note("harness", "revoked %s's enrollment %s (pod %s)", c.token, e.id, short(e.incarnation))
	delete(r.enrollments, c.token)
	return nil
}

// agentSecrets runs `agent-secrets <args>` in c's worker container as the agent would, with the
// pod's own AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR, and returns stdout, the exit code, and
// stderr. kubectl exec answers exit 1 for any non-zero code, so the command's own code is echoed.
func (r *liveRig) agentSecrets(c *liveClaim, args ...string) (stdout string, code int, stderr string, err error) {
	script := "/opt/legion/bin/agent-secrets " + shellJoin(args) + " 2>/tmp/agent-secrets.err; code=$?; printf '\\n---exit %d---\\n' $code; cat /tmp/agent-secrets.err"
	out, err := r.exec(c, "sh", "-c", script)
	if err != nil && !strings.Contains(out, "---exit ") {
		return "", -1, "", err
	}
	head, tail, _ := strings.Cut(out, "\n---exit ")
	codeText, rest, _ := strings.Cut(tail, "---\n")
	fmt.Sscanf(codeText, "%d", &code)
	return strings.TrimSpace(head), code, strings.TrimSpace(rest), nil
}

// secrets-two-roles-enrolled: the root and the worker share one pod and ServiceAccount, yet each
// is enrolled from its own hello under a distinct role-generation slot, holds a private key (0600)
// and enrollment id, and `agent-secrets self` answers each own id. The pod's projected token for
// the broker's audience is alone in its volume, and the middleman token is untouched beside it.
func (r *liveRig) checkSecretsTwoRolesEnrolled() error {
	root, worker := r.claim("root"), r.claim("worker")
	for _, c := range []*liveClaim{root, worker} {
		if err := r.ensureRunning(c); err != nil {
			return err
		}
	}
	ids := map[claim.Token]string{}
	for _, c := range []*liveClaim{root, worker} {
		id, err := r.ensureEnrolled(c)
		if err != nil {
			return err
		}
		ids[c.token] = id
		pod, err := r.getPod(SandboxName(c.token))
		if err != nil {
			return err
		}
		tokens := 0
		for _, v := range pod.Spec.Volumes {
			if v.Projected != nil {
				for _, s := range v.Projected.Sources {
					if s.ServiceAccountToken != nil && s.ServiceAccountToken.Audience == "agent-secrets" {
						tokens++
						if len(v.Projected.Sources) != 1 {
							return fmt.Errorf("pod %s: the agent-secrets token shares its volume %s with %d other sources", pod.Name, v.Name, len(v.Projected.Sources)-1)
						}
					}
				}
			}
		}
		if tokens != 1 {
			return fmt.Errorf("pod %s carries %d agent-secrets tokens, want one", pod.Name, tokens)
		}
		mode, err := r.exec(c, "sh", "-c", "stat -c '%a %U' "+AgentSecretsKeyDir+"/key.pem && cat "+AgentSecretsKeyDir+"/enrollment")
		if err != nil {
			return err
		}
		lines := strings.Split(mode, "\n")
		if len(lines) != 2 || lines[0] != "600 legion" || lines[1] != id {
			return fmt.Errorf("pod %s key dir: %q, want a 0600 key owned by legion and enrollment %s", pod.Name, mode, id)
		}
		note("operator", "exec stat %s/key.pem: %s; enrollment file holds %s", AgentSecretsKeyDir, lines[0], id)
		self, code, stderr, err := r.agentSecrets(c, "self", "--json")
		if err != nil || code != 0 {
			return fmt.Errorf("agent-secrets self in %s: exit %d: %s (%v)", c.token, code, stderr, err)
		}
		var answer struct {
			EnrollmentID string `json:"enrollment_id"`
			Kind         string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(self), &answer); err != nil || answer.EnrollmentID != id || answer.Kind != "pod" {
			return fmt.Errorf("agent-secrets self in %s answered %q, want enrollment %s of kind pod", c.token, self, id)
		}
		note("runtime", "%s: agent-secrets self → %s (pod)", c.token, answer.EnrollmentID)
	}
	if ids[root.token] == ids[worker.token] {
		return fmt.Errorf("both role slots enrolled as %s", ids[root.token])
	}
	if root.loc.Sandbox.PodUID != worker.loc.Sandbox.PodUID {
		return fmt.Errorf("root and worker enrolled on different pod UIDs %s and %s, want one issue pod", root.loc.Sandbox.PodUID, worker.loc.Sandbox.PodUID)
	}
	return nil
}

// secrets-automatic-grant: the root requests LEGION_E2E_AUTO, an automatic rule for pods, and its
// command runs with the value — proven by the value's sha256, never the value — while the worker,
// asking for the same secret, gets a grant of its own with a different id.
func (r *liveRig) checkSecretsAutomaticGrant() error {
	root, worker := r.claim("root"), r.claim("worker")
	out, code, stderr, err := r.agentSecrets(root, "LEGION_E2E_AUTO", "--", "sh", "-c", `printf %s "$LEGION_E2E_AUTO" | sha256sum | cut -d' ' -f1`)
	if err != nil || code != 0 {
		return fmt.Errorf("agent-secrets LEGION_E2E_AUTO -- … in the root: exit %d: %s (%v)", code, stderr, err)
	}
	if out != r.env.agentSecretsAutoSHA {
		return fmt.Errorf("the root's command saw a value hashing to %s, want %s", out, r.env.agentSecretsAutoSHA)
	}
	note("runtime", "root: agent-secrets LEGION_E2E_AUTO -- sha256sum → %s (matches the operator's hash)", out[:12])
	rootGrant, _, _, err := r.agentSecrets(root, "request", "LEGION_E2E_AUTO", "--json")
	if err != nil {
		return err
	}
	workerGrant, code, stderr, err := r.agentSecrets(worker, "request", "LEGION_E2E_AUTO", "--json")
	if err != nil || code != 0 {
		return fmt.Errorf("the worker's request: exit %d: %s (%v)", code, stderr, err)
	}
	var a, b struct {
		State     string `json:"state"`
		RequestID string `json:"request_id"`
		GrantID   string `json:"grant_id"`
	}
	if err := errors.Join(json.Unmarshal([]byte(rootGrant), &a), json.Unmarshal([]byte(workerGrant), &b)); err != nil {
		return err
	}
	if a.State != "granted" || b.State != "granted" || a.GrantID == "" || a.GrantID == b.GrantID || a.RequestID == "" {
		return fmt.Errorf("root %+v, worker %+v: want two distinct automatic grants", a, b)
	}
	r.grants = map[claim.Token]string{root.token: a.GrantID, worker.token: b.GrantID}
	r.requests = map[claim.Token]string{root.token: a.RequestID, worker.token: b.RequestID}
	note("runtime", "root grant %s (request %s), worker grant %s", a.GrantID, a.RequestID, b.GrantID)
	return nil
}

// secrets-cross-pod-negative: the worker cannot touch the root's request or grant — reading the
// request (GET /v1/requests/{id}) or revoking the grant (POST /v1/grants/{id}/revoke) is 403
// NOT_YOURS, and the root's grant still works after the attempt — nor use its own key beside the
// root's enrollment id (401 PROOF_INVALID: the proof's thumbprint is not the enrollment's).
func (r *liveRig) checkSecretsCrossPodNegative() error {
	root, worker := r.claim("root"), r.claim("worker")
	for _, attempt := range [][]string{{"status", r.requests[root.token]}, {"revoke", r.grants[root.token]}} {
		_, code, stderr, err := r.agentSecrets(worker, attempt...)
		if err != nil {
			return err
		}
		if code == 0 || !strings.Contains(stderr, "NOT_YOURS") {
			return fmt.Errorf("the worker's agent-secrets %s %s: exit %d: %s; want NOT_YOURS", attempt[0], attempt[1], code, stderr)
		}
		note("runtime", "worker: agent-secrets %s <root's> → exit %d NOT_YOURS", attempt[0], code)
	}
	if out, code, stderr, err := r.agentSecrets(root, "LEGION_E2E_AUTO", "--", "sh", "-c", `printf %s "$LEGION_E2E_AUTO" | sha256sum | cut -d' ' -f1`); err != nil || code != 0 || out != r.env.agentSecretsAutoSHA {
		return fmt.Errorf("the root's grant no longer works after the worker's attempts: exit %d %s (%v)", code, stderr, err)
	}
	note("runtime", "root: its grant still works after the worker's attempts")
	rootID := r.enrollments[root.token].id
	script := fmt.Sprintf("mkdir -p /tmp/swapped && cp %s/key.pem /tmp/swapped/ && printf '%%s\\n' %s >/tmp/swapped/enrollment && AGENT_SECRETS_KEY_DIR=/tmp/swapped /opt/legion/bin/agent-secrets self 2>&1; echo \"---exit $?---\"", AgentSecretsKeyDir, rootID)
	out, err := r.exec(worker, "sh", "-c", script)
	if err != nil && !strings.Contains(out, "---exit ") {
		return err
	}
	if !strings.Contains(out, "PROOF_INVALID") || strings.Contains(out, "---exit 0---") {
		return fmt.Errorf("the worker's key with the root's enrollment id was accepted: %s", out)
	}
	note("runtime", "worker: own key + root's enrollment id → PROOF_INVALID")
	return nil
}

// secrets-copied-token-negative: a projected token copied from the shared issue pod does not prove
// another pod. The broker refuses an enrollment whose requested pod UID is not that token's pod.
func (r *liveRig) checkSecretsCopiedTokenNegative() error {
	root, worker := r.claim("root"), r.claim("worker")
	rootRegs, workerRegs := r.reg.registrations(root.token), r.reg.registrations(worker.token)
	rootID, workerID := rootRegs[len(rootRegs)-1].identity, workerRegs[len(workerRegs)-1].identity
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	_, err := r.secrets.Enroll(ctx, agentsecrets.PodEnrollment{
		PodUID: "another-pod-uid", Slot: agentSecretsSlot(worker), Thumbprint: workerID.Thumbprint, PodToken: rootID.PodToken, Session: "s4a-copied",
	})
	var api *agentsecrets.APIError
	if !errors.As(err, &api) || api.Status != 403 || api.Code != "POD_IDENTITY_MISMATCH" {
		return fmt.Errorf("enrolling another pod uid with the root's token: %v, want 403 POD_IDENTITY_MISMATCH", err)
	}
	note("harness", "POST /v1/enrollments (another pod uid, root token) → 403 POD_IDENTITY_MISMATCH")
	return nil
}

// secrets-self-enroll-negative: a pod cannot mint another enrollment through the agent-secrets
// CLI. The only CLI enrollment route is a helper-backed box enrollment; pod enrollment is the
// daemon's launcher-proof path, so the CLI refuses before it could present a credential.
func (r *liveRig) checkSecretsSelfEnrollNegative() error {
	root := r.claim("root")
	script := `mkdir -p /tmp/second && tp=$(/opt/legion/bin/agent-secrets keygen --out /tmp/second) && /opt/legion/bin/agent-secrets enroll --kind pod --runtime-id "$POD_UID" --thumbprint "$tp" 2>&1; echo "---exit $?---"`
	out, err := r.exec(root, "sh", "-c", script)
	if err != nil && !strings.Contains(out, "---exit ") {
		return err
	}
	if strings.Contains(out, "---exit 0---") || !strings.Contains(out, "--helper is required") {
		return fmt.Errorf("a self-enrollment from inside the pod was not refused by the CLI boundary: %s", out)
	}
	self, code, _, err := r.agentSecrets(root, "self", "--json")
	if err != nil || code != 0 || !strings.Contains(self, r.enrollments[root.token].id) {
		return fmt.Errorf("after the attempt, agent-secrets self answered %q (exit %d): the pod's own enrollment must be unchanged", self, code)
	}
	note("runtime", "root: direct agent-secrets enroll refused before broker access; the pod's enrollment is unchanged")
	return nil
}

// secrets-approval-ask: a request for the approval-gated secret comes back pending with a
// credential-request record id (ruling 16: the broker's rules pick the approver at request time,
// naming no issue); the harness prints it for the operator to approve on the Dispatch credential
// page and polls, exactly as newLiveRig's attended machine login does, until it settles granted
// (success), denied, or expired (both failures).
func (r *liveRig) checkSecretsApprovalAsk() error {
	worker := r.claim("worker")
	if err := r.ensureRunning(worker); err != nil {
		return err
	}
	if _, err := r.ensureEnrolled(worker); err != nil {
		return err
	}
	out, code, stderr, err := r.agentSecrets(worker, "request", "LEGION_E2E_APPROVAL", "--reason", "s4a approval probe", "--json")
	if err != nil || code != 75 {
		return fmt.Errorf("the worker's approval request: exit %d (want 75, pending): %s (%v)", code, stderr, err)
	}
	var pending struct {
		RequestID string  `json:"request_id"`
		State     string  `json:"state"`
		RecordID  *string `json:"record_id"`
	}
	if err := json.Unmarshal([]byte(out), &pending); err != nil || pending.State != "pending" || pending.RecordID == nil || *pending.RecordID == "" {
		return fmt.Errorf("pending request %q, want state pending with a non-empty record_id", out)
	}
	note("runtime", "worker: request LEGION_E2E_APPROVAL → pending, request %s, record %s", pending.RequestID, *pending.RecordID)
	fmt.Printf("STAGE4A: approve credential request %s for LEGION_E2E_APPROVAL on the Dispatch credential page as %s\n", *pending.RecordID, r.env.agentSecretsOperator)
	var state struct {
		State string `json:"state"`
	}
	pollErr := r.poll(10*time.Minute, "credential request "+pending.RequestID+" to be approved", func() (bool, error) {
		out, code, stderr, err := r.agentSecrets(worker, "status", pending.RequestID, "--json")
		if err != nil || code != 0 {
			return false, fmt.Errorf("status %s: exit %d %s (%v)", pending.RequestID, code, stderr, err)
		}
		if err := json.Unmarshal([]byte(out), &state); err != nil {
			return false, fmt.Errorf("decode status %s: %v", pending.RequestID, err)
		}
		return state.State != "pending", nil
	})
	switch {
	case pollErr != nil:
		return pollErr
	case state.State == "granted":
		note("runtime", "worker: request %s → granted", pending.RequestID)
		return nil
	case state.State == "denied":
		return fmt.Errorf("credential request %s was denied", pending.RequestID)
	case state.State == "expired":
		return fmt.Errorf("credential request %s expired before approval", pending.RequestID)
	default:
		return fmt.Errorf("credential request %s ended in unexpected state %q", pending.RequestID, state.State)
	}
}

// secrets-old-uid-negative and secrets-revoke-on-death: a separate issue pod is deleted and its
// claim resumed with a new pod UID. The old pod's enrollment is revoked; a key copied before its
// deletion then fails, and a new pod's identity cannot enroll under the old UID.
func (r *liveRig) checkSecretsOldUIDAndRevocation() error {
	worker := r.claim("orphan")
	if err := r.ensureRunning(worker); err != nil {
		return err
	}
	current, err := r.ensureEnrolled(worker)
	if err != nil {
		return err
	}
	copied := filepath.Join(r.env.work, "worker-keydir-"+short(worker.loc.Incarnation))
	if err := os.MkdirAll(copied, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"key.pem", "enrollment"} {
		raw, err := r.exec(worker, "cat", AgentSecretsKeyDir+"/"+name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(copied, name), []byte(raw+"\n"), 0o600); err != nil {
			return err
		}
	}
	devbox := func() (string, int) {
		out, code := runAgentSecretsOnDevbox(r, copied, "self", "--json")
		return out, code
	}
	if out, code := devbox(); code != 0 || !strings.Contains(out, current) {
		return fmt.Errorf("the copied key directory did not work from the devbox before the pod delete (exit %d: %s); the check cannot show the revocation", code, out)
	}
	note("harness", "devbox: agent-secrets self with the orphan's copied key → its enrollment (the accepted boundary: the key is the pod)")
	oldUID, oldIncarnation := worker.loc.Sandbox.PodUID, worker.loc.Incarnation
	mark := r.obs.mark()
	if _, err := r.kubectl("delete", "pod", SandboxName(worker.token), "--wait=false"); err != nil {
		return err
	}
	gone, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
		return o.Locator.Claim == worker.token && o.Locator.Incarnation == oldIncarnation && (o.Kind == runtime.Gone || o.Kind == runtime.NotRecordedProcess)
	})
	if !ok {
		return fmt.Errorf("no gone observation for %s (pod uid %s) within %s", worker.token, short(oldUID), liveGoneLimit)
	}
	note("runtime", "observed %s process %s in pod uid %s: %s", worker.token, short(oldIncarnation), short(oldUID), gone.Kind)
	if err := r.revoke(worker); err != nil {
		return err
	}
	if out, code := devbox(); code == 0 || !strings.Contains(out, "PROOF_INVALID") {
		return fmt.Errorf("the dead pod's key still works after revocation: exit %d: %s", code, out)
	}
	note("harness", "devbox: the same copied key after DELETE /v1/enrollments → PROOF_INVALID")
	worker.state = stateDead
	since := time.Now()
	loc, err := r.resume(worker, worker.marker)
	if err != nil {
		return err
	}
	worker.state, worker.loc = stateRunning, &loc
	reg, err := r.awaitRunning(worker, since)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	_, err = r.secrets.Enroll(ctx, agentsecrets.PodEnrollment{
		PodUID: oldUID, Slot: agentSecretsSlot(worker), Thumbprint: reg.identity.Thumbprint, PodToken: reg.identity.PodToken, Session: "s4a-old-uid",
	})
	var api *agentsecrets.APIError
	if !errors.As(err, &api) || api.Status != 403 || api.Code != "POD_IDENTITY_MISMATCH" {
		return fmt.Errorf("enrolling the replacement pod's key under the old uid %s: %v, want 403 POD_IDENTITY_MISMATCH", short(oldUID), err)
	}
	note("harness", "POST /v1/enrollments (old pod uid %s, replacement pod token) → 403 POD_IDENTITY_MISMATCH", short(oldUID))
	if _, err := r.enroll(worker, reg); err != nil {
		return err
	}
	out, code, stderr, err := r.agentSecrets(worker, "LEGION_E2E_AUTO", "--", "sh", "-c", `printf %s "$LEGION_E2E_AUTO" | sha256sum | cut -d' ' -f1`)
	if err != nil || code != 0 || out != r.env.agentSecretsAutoSHA {
		return fmt.Errorf("the resumed worker (pod uid %s) could not use its new enrollment: exit %d %s %s (%v)", short(loc.Sandbox.PodUID), code, out, stderr, err)
	}
	note("runtime", "the resumed worker (pod uid %s) is enrolled anew and gets the automatic secret", short(loc.Sandbox.PodUID))
	return nil
}

// runAgentSecretsOnDevbox runs the checkout's agent-secrets against the broker with a copied key
// directory, as a process of the harness's own: the pod's environment is reproduced from the run's
// inputs.
func runAgentSecretsOnDevbox(r *liveRig, keyDir string, args ...string) (string, int) {
	cmd := exec.CommandContext(r.ctx, r.env.agentSecretsBin, args...)
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_URL="+r.env.agentSecretsURL, "AGENT_SECRETS_KEY_DIR="+keyDir)
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		code = -1
	}
	return strings.TrimSpace(string(out)), code
}
