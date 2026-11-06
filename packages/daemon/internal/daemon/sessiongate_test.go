package daemon

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// sessionGateUnder is the tmux daemon's gate over gateUnder's pane profile, run through the image
// fake f (whose pane lane answers the load probe and whose session plan answers the session-storage
// probe), under the session database at dsn ("" for the files store), and what it logs.
func sessionGateUnder(t *testing.T, f imageOmp, dsn string) (pluginGate, *bytes.Buffer) {
	t.Helper()
	g, logged := gateUnder(t, fakeOmp{path: f.path}, contractCurrent)
	g.prefix = nil
	g.sessionDSN = dsn
	return g, logged
}

// Under runtime.tmux.session_store postgres the tmux gate runs, after the plugin probes, the
// session-storage probe on the Oh My Pi every pane runs, then the session table probe: a build that
// predates the setting is refused before the database is tried, and a session database that does
// not answer refuses the boot naming runtime.tmux.session_dsn_file and never the URL's password.
// Under the files store neither runs.
func TestTheTmuxGateRefusesASessionDatabaseItsPanesCouldNotUse(t *testing.T) {
	const unreachable = "postgres://legion:fixture-session-password@127.0.0.1:1/legion?connect_timeout=5"
	for _, tc := range []struct {
		name, dsn string
		session   []string
		want      string
		sessions  int
	}{
		{"the files store", "", []string{"refuses"}, "", 0},
		{"a build that predates the setting", unreachable, []string{"accepts"}, "predates the session.storage setting", 1},
		{"a database that does not answer", unreachable, []string{"refuses"}, "boot gate: the session database runtime.tmux.session_dsn_file names does not answer", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImageOmp(t, []string{"available"}, []string{"yes"}, tc.session)
			gate, _ := sessionGateUnder(t, f, tc.dsn)

			err := gate.verifyPanes(context.Background())

			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("verifyPanes = %v, want a pass", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("verifyPanes = %v, want a refusal saying %q", err, tc.want)
			case err != nil && strings.Contains(err.Error(), "fixture-session-password"):
				t.Errorf("the refusal carries the URL's password: %v", err)
			}
			if n := f.attempts(t, "load"); n != 1 {
				t.Errorf("the load probe ran %d times, want 1", n)
			}
			if n := f.attempts(t, "session"); n != tc.sessions {
				t.Errorf("the session-storage probe ran %d times, want %d", n, tc.sessions)
			}
		})
	}
}

// A session database the panes can use passes the gate: one whose session table exists, and one
// no Oh My Pi has opened yet, whose table the first pane's Oh My Pi creates, which the gate logs.
func TestTheTmuxGatePassesASessionDatabaseItsPanesCanUse(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.Database(t, "legion_tmux_session_gate_test")
	f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	g, logged := sessionGateUnder(t, f, dsn)

	if err := g.verifyPanes(ctx); err != nil {
		t.Fatalf("verifyPanes on a database no Oh My Pi has opened = %v, want a pass", err)
	}
	if !strings.Contains(logged.String(), "holds no session table yet") {
		t.Errorf("the gate did not log the missing table:\n%s", logged)
	}

	conn, err := ompsessions.ConnectURL(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := ompsessions.Import(ctx, conn, "/state/home/.omp/agent/sessions/s.jsonl", []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	logged.Reset()
	if err := g.verifyPanes(ctx); err != nil {
		t.Fatalf("verifyPanes on a database holding the session table = %v, want a pass", err)
	}
	if strings.Contains(logged.String(), "holds no session table yet") {
		t.Errorf("the gate logged a missing table the database holds:\n%s", logged)
	}
}
