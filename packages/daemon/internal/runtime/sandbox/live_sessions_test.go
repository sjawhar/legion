//go:build e2e

// The Stage 4a harness's session-store check: the runtime under runtime.kubernetes.session_store
// postgres — the one store the daemon loads, which every runtime of the run is built on — against
// the scratch Postgres the script started on the devbox's private address
// (LEGION_E2E_SESSION_DB_PORT). The rig is live_test.go.

package sandbox

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// liveSessionsSecretKey is the providers Secret's key the script writes the scratch database's URL
// under: the run's runtime.kubernetes.session_dsn_secret.
const liveSessionsSecretKey = "stage4a_sessions"

// postgres-resume: the run keeps every session in the scratch database (session_store postgres,
// the rig's sessionStore), and a new issue pod's role starts with Oh My Pi's two session variables
// and the URL file mounted, and never the URL itself, in its agent's environment. The harness
// writes this one role's session into the table itself, as `legion sessions import` copies one
// whose disk is gone, dated before the pod's workspace was created, under a path the issue's disk
// holds no file at. Suspended, the role is resumed from that path: a resume of a path the table
// lacks is refused by the role's launcher, naming the table, before any child runs; the resume from
// the copied path registers, though the disk holds no such file, since the launcher looked the
// session up in the table through the pod's URL file; and the agent's environment says the
// workspace was recreated since the session was last written (launcher.WorkspaceRecreatedVariable),
// which the Legion plugin tells the agent before its next turn, where the fresh generation's said
// it was not.
func (r *liveRig) checkPostgresResume() error {
	c := r.claim("sessions")
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	if c.state != stateNone {
		return fmt.Errorf("%s was launched before this check, which needs its first pod of the run", c.name)
	}
	note("runtime", "every runtime of the run keeps sessions in the scratch database: providers Secret key %s at %s", liveSessionsSecretKey, ProvidersDir+"/"+sessionDSNFile)
	since := time.Now()
	if _, err := r.spawn(c, true); err != nil {
		return err
	}
	fresh, err := r.awaitRunning(c, since)
	if err != nil {
		return err
	}
	note("harness", "hello registered %s at generation %d (fresh) in pod uid %s", c.token, fresh.gen, c.loc.Sandbox.PodUID)
	agent, err := r.agentEnviron(c)
	if err != nil {
		return err
	}
	if got := agent[launcher.WorkspaceRecreatedVariable]; got != "false" {
		return fmt.Errorf("the fresh agent has %s=%q, want false", launcher.WorkspaceRecreatedVariable, got)
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
		return fmt.Errorf("the issue pod's disk holds %s (%q, %v), want no file there: the resume must be the table's alone", session, out, err)
	}
	written := since.Add(-time.Hour)
	content := `{"type":"session","version":3,"id":"` + string(c.token) + `"}` + "\n"
	if err := r.writeSession(session, content, written); err != nil {
		return err
	}
	note("harness", "the session table holds %s (%d bytes, mtime_ms %s, before the pod's workspace was created); the issue pod's disk holds no such file", session, len(content), written.UTC().Format(time.RFC3339))

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
	note("harness", "hello registered %s at generation %d, resumed from the table in pod uid %s", c.token, resumed.gen, loc.Sandbox.PodUID)
	agent, err = r.agentEnviron(c)
	if err != nil {
		return err
	}
	if agent[launcher.WorkspaceRecreatedVariable] != "true" {
		return fmt.Errorf("the resumed agent has %s=%q, want true: its workspace was created after the session was last written at %s",
			launcher.WorkspaceRecreatedVariable, agent[launcher.WorkspaceRecreatedVariable], written.UTC().Format(time.RFC3339))
	}
	note("operator", "the resumed agent's environment: %s=true", launcher.WorkspaceRecreatedVariable)
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
