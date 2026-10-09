//go:build e2e

// The Stage 4a harness's session-store check: the runtime under runtime.kubernetes.session_store
// postgres, against the scratch Postgres the script started on the devbox's private address
// (LEGION_E2E_SESSION_DB_PORT). The rig is live_test.go.

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// liveSessionsSecretKey is the providers Secret's key the script writes the scratch database's URL
// under: the run's runtime.kubernetes.session_dsn_secret.
const liveSessionsSecretKey = "stage4a_sessions"

// postgres-resume: with the runtime replaced by one that keeps sessions in the scratch database
// (session_store postgres), a new issue pod's role starts with Oh My Pi's two session variables and
// the URL file mounted, and never the URL itself, in its agent's environment. The harness writes the
// role's session into the table, as `legion sessions import` copies one whose volume is gone, dated
// before the pod's workspace was created, under a path the tree volume holds no file at. Suspended,
// the role is resumed from that path: a resume of a path the table lacks is refused by the role's
// launcher, naming the table, before any child runs; the resume from the copied path registers,
// though the volume holds no such file, since the launcher looked the session up in the table
// through the pod's URL file; and its shim's hello, and the agent's environment, say the workspace
// was recreated since the session was last written, which the daemon tells the agent ahead of its
// next task.
func (r *liveRig) checkPostgresResume() error {
	c := r.claim("sessions")
	if r.sessionStore == "" {
		r.sessionStore = liveSessionsSecretKey
		r.stopRuntime()
		if err := r.startRuntime(); err != nil {
			return err
		}
		note("runtime", "listener and runtime replaced with session_store postgres: providers Secret key %s at %s", liveSessionsSecretKey, ProvidersDir+"/"+sessionDSNFile)
	}
	if c.state != stateNone {
		return fmt.Errorf("%s was launched before this check, which needs its first pod created under session_store postgres", c.name)
	}
	since := time.Now()
	if _, err := r.spawn(c, true); err != nil {
		return err
	}
	fresh, err := r.awaitRunning(c, since)
	if err != nil {
		return err
	}
	if fresh.recreated {
		return fmt.Errorf("the fresh generation %d's hello said its workspace was recreated, want a fresh agent told nothing", fresh.gen)
	}
	note("harness", "hello registered %s at generation %d (fresh, workspace not recreated) in pod uid %s", c.token, fresh.gen, c.loc.Sandbox.PodUID)
	agent, err := r.agentEnviron(c)
	if err != nil {
		return err
	}
	if agent[ompsessions.StorageVariable] != ompsessions.SQLStorage || agent[ompsessions.DSNFileVariable] != ProvidersDir+"/"+sessionDSNFile {
		return fmt.Errorf("the agent has %s=%q and %s=%q, want %s and %s", ompsessions.StorageVariable, agent[ompsessions.StorageVariable],
			ompsessions.DSNFileVariable, agent[ompsessions.DSNFileVariable], ompsessions.SQLStorage, ProvidersDir+"/"+sessionDSNFile)
	}
	if value, held := agent[sessionDSNFile]; held {
		return fmt.Errorf("the agent's environment holds %s (%d bytes): the session database's URL reached Oh My Pi's environment", sessionDSNFile, len(value))
	}
	note("operator", "the agent's environment: %s=%s, %s=%s, no %s", ompsessions.StorageVariable, agent[ompsessions.StorageVariable],
		ompsessions.DSNFileVariable, agent[ompsessions.DSNFileVariable], sessionDSNFile)

	session := ompSessionsDir + "/" + SandboxName(c.token) + "-postgres.jsonl"
	if out, err := r.exec(c, "sh", "-c", `test -e "$1" || echo absent`, "absent", session); err != nil || out != "absent" {
		return fmt.Errorf("the tree volume holds %s (%q, %v), want no file there: the resume must be the table's alone", session, out, err)
	}
	written := since.Add(-time.Hour)
	content := `{"type":"session","version":3,"id":"` + string(c.token) + `"}` + "\n"
	if err := r.writeSession(session, content, written); err != nil {
		return err
	}
	note("harness", "the session table holds %s (%d bytes, mtime_ms %s, before the pod's workspace was created); the tree volume holds no such file", session, len(content), written.UTC().Format(time.RFC3339))

	if err := r.suspend(c); err != nil {
		return err
	}
	absent := ompSessionsDir + "/absent-" + SandboxName(c.token) + "-postgres.jsonl"
	_, err = r.resume(c, absent)
	if err == nil {
		return fmt.Errorf("Resume naming %s, which the table lacks, started an agent, want the role launcher's refusal before any child runs", absent)
	}
	if want := "resume session " + absent + ": the session table holds no such session"; !strings.Contains(err.Error(), want) {
		return fmt.Errorf("Resume naming %s failed with %q, want the table's refusal %q (the scratch database at %s must be reachable from the Legion nodes, as the worker stream is)",
			absent, err, want, r.env.sessionDBAddress)
	}
	note("runtime", "Resume naming %s refused before any child started: %s", absent, oneLine(err.Error()))
	if regs := r.reg.registrations(c.token); len(regs) > 0 && regs[len(regs)-1].gen == c.gen {
		return errors.New("the refused generation registered a hello")
	}

	since = time.Now()
	loc, err := r.resume(c, session)
	if err != nil {
		return fmt.Errorf("Resume from %s, which the table holds: %w", session, err)
	}
	resumed, err := r.awaitRunning(c, since)
	if err != nil {
		return err
	}
	if resumed.hash != tokenHash(c.bootToken) {
		return fmt.Errorf("the registration's token hash %s is not generation %d's", short(resumed.hash), c.gen)
	}
	if !resumed.recreated {
		return fmt.Errorf("the resumed generation %d's hello did not say its workspace was recreated since its session was last written at %s", resumed.gen, written.UTC().Format(time.RFC3339))
	}
	note("harness", "hello registered %s at generation %d, resumed from the table in pod uid %s, saying its workspace was recreated since the session was last written", c.token, resumed.gen, loc.Sandbox.PodUID)
	agent, err = r.agentEnviron(c)
	if err != nil {
		return err
	}
	if agent[shimwire.WorkspaceRecreatedVariable] != "true" {
		return fmt.Errorf("the resumed agent has %s=%q, want true", shimwire.WorkspaceRecreatedVariable, agent[shimwire.WorkspaceRecreatedVariable])
	}
	note("operator", "the resumed agent's environment: %s=true", shimwire.WorkspaceRecreatedVariable)
	return nil
}

// agentEnviron is the environment of the claim's stub agent, read from /proc as the operator.
func (r *liveRig) agentEnviron(c *liveClaim) (map[string]string, error) {
	pid, err := r.agentPid(c)
	if err != nil {
		return nil, err
	}
	return r.procEnviron(c, pid)
}

// writeSession puts content in the scratch database's session table at path, last written at
// written, as `legion sessions import` copies a session file (ompsessions.Import), through the URL
// file the script wrote.
func (r *liveRig) writeSession(path, content string, written time.Time) error {
	ctx, cancel := context.WithTimeout(r.ctx, time.Minute)
	defer cancel()
	conn, err := ompsessions.Connect(ctx, r.env.sessionDSNFile)
	if err != nil {
		return fmt.Errorf("the scratch database at %s: %w", r.env.sessionDBAddress, err)
	}
	defer conn.Close(context.Background())
	outcome, err := ompsessions.Import(ctx, conn, path, []byte(content), written)
	if err != nil {
		return err
	}
	if outcome != ompsessions.Copied {
		return fmt.Errorf("the session table already held %s (%s), want a fresh copy", path, outcome)
	}
	return nil
}
