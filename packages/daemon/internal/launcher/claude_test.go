package launcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// claudeSession is the session a Claude Code generation under test starts or resumes.
const claudeSession = "0b9c2f4e-2d3a-4c55-9a1e-6f7d8e9a0b1c"

// claudeRig is a launcher under test that runs Claude Code generations, as the sandbox runtime
// starts them, against a session table of the test's own. The test plays Claude Code and its
// plugin: it writes the transcript and the phase-reminder record where they would, while the
// generation's child stands in for the harness's process.
type claudeRig struct {
	*rig
	dsnFile    string
	conn       *pgx.Conn
	configDir  string
	transcript string
	record     string
}

func newClaudeRig(t *testing.T, cfg Config) *claudeRig {
	t.Helper()
	_, dsnFile := testpg.DSNFile(t, "legion_launcher_claude_test")
	return claudeRigOn(t, newSessionRig(t, cfg), dsnFile)
}

func claudeRigOn(t *testing.T, g *rig, dsnFile string) *claudeRig {
	t.Helper()
	conn, err := ompsessions.Connect(context.Background(), dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if err := ompsessions.CreateTables(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	return &claudeRig{rig: g, dsnFile: dsnFile, conn: conn, configDir: configDir,
		transcript: filepath.Join(configDir, "projects", "-legion-workspaces-legion-208", claudeSession+".jsonl"),
		record:     filepath.Join(configDir, "legion-phase-stall-"+claudeSession+".json")}
}

// command is the start of a Claude Code generation running script: the environment the sandbox
// runtime gives it (its configuration directory, the session, and Oh My Pi's two session
// variables), resuming the transcript when resume is set.
func (c *claudeRig) command(generation uint64, script string, resume bool, extra ...string) shimwire.LauncherStart {
	env := append([]string{"CLAUDE_CONFIG_DIR=" + c.configDir, claudelaunch.SessionVariable + "=" + claudeSession,
		ompsessions.StorageVariable + "=" + ompsessions.SQLStorage, ompsessions.DSNFileVariable + "=" + c.dsnFile}, extra...)
	command := shimwire.LauncherStart{ID: "start-" + strconv.FormatUint(generation, 10), Generation: generation,
		Argv: []string{"/bin/sh", "-c", script}, Env: env}
	if resume {
		command.ResumeFile = c.transcript
	}
	return command
}

func (c *claudeRig) launch(generation uint64, script string, resume bool, extra ...string) {
	c.t.Helper()
	if got := c.start(c.command(generation, script, resume, extra...)); !got.OK {
		c.t.Fatalf("start of generation %d = %#v", generation, got)
	}
}

// seeing is a child that publishes to seen what was at path when it started, "missing" for
// nothing, then waits.
func seeing(path, seen string) string {
	return fmt.Sprintf(`{ cat %q 2>/dev/null || printf missing; } > %q.tmp && mv %q.tmp %q; exec sleep 600`, path, seen, seen, seen)
}

// seen is what a seeing child published.
func seen(t *testing.T, file string) string {
	t.Helper()
	waitForFile(t, file)
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// row is the session the table holds at path.
func (c *claudeRig) row(path string) (string, bool) {
	c.t.Helper()
	content, ok, err := ompsessions.Read(context.Background(), c.conn, path)
	if err != nil {
		c.t.Fatal(err)
	}
	return string(content), ok
}

// awaitRow waits, past a few saves, for the table to hold want at path.
func (c *claudeRig) awaitRow(path, want string) {
	c.t.Helper()
	deadline := time.Now().Add(4 * saveInterval)
	for {
		got, ok := c.row(path)
		if ok && got == want {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("the table holds %q (row %t) at %s, want %q", got, ok, path, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// seed saves content at path as an earlier generation's launcher would have.
func (c *claudeRig) seed(path, content string, written time.Time) {
	c.t.Helper()
	if err := ompsessions.Replace(context.Background(), c.conn, path, ompsessions.Absent, []byte(content), written); err != nil {
		c.t.Fatal(err)
	}
}

// write writes content to path whole, as Claude Code and its plugin first write each file: aside,
// with mtime, then renamed into place, so no save sees it half-made.
func write(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	aside := path + ".aside"
	if err := os.WriteFile(aside, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(aside, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(aside, path); err != nil {
		t.Fatal(err)
	}
}

// appendTo appends text to path, as Claude Code appends to its transcript.
func appendTo(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// A Claude Code resume restores the session's files from their rows before its child starts, so
// the child finds the transcript the table holds whatever the pod's disk lost: a missing file is
// written, as is one that holds only part of the row. A file longer than its row, which a relaunch
// into the same pod leaves when the last save missed its last lines, is never shortened: the child
// finds it whole, and the next save adds what the row lacked.
func TestAClaudeResumeRestoresBeforeTheChildStartsAndNeverShortensALongerFile(t *testing.T) {
	c := newClaudeRig(t, Config{})
	saved := "{\"n\":1}\n{\"n\":2}\n"
	c.seed(c.transcript, saved, time.Now())
	for generation, tc := range []struct {
		name, local, want string
	}{
		{"a lost disk", "", saved},
		{"a file holding part of the row", "{\"n\":1}\n", saved},
		{"a file longer than the row", saved + "{\"n\":3}\n", saved + "{\"n\":3}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.RemoveAll(filepath.Join(c.configDir, "projects"))
			if tc.local != "" {
				write(t, c.transcript, tc.local, time.Now())
			}
			told := filepath.Join(t.TempDir(), "seen")
			c.launch(uint64(generation+1), seeing(c.transcript, told), true)
			if got := seen(t, told); got != tc.want {
				t.Errorf("the child found the transcript %q at its start, want %q", got, tc.want)
			}
			c.awaitRow(c.transcript, tc.want)
			c.stop(uint64(generation + 1))
		})
	}
}

// A fresh transcript is saved as it grows, and only up to its last newline: a line Claude Code has
// begun and not finished is never in the table, and joins it once finished.
func TestAClaudeSaveEndsAtANewline(t *testing.T) {
	c := newClaudeRig(t, Config{})
	c.launch(1, "exec sleep 600", false)
	write(t, c.transcript, "{\"n\":1}\n{\"n\":", time.Now())
	c.awaitRow(c.transcript, "{\"n\":1}\n")
	time.Sleep(saveInterval + saveInterval/2)
	if got, _ := c.row(c.transcript); got != "{\"n\":1}\n" {
		t.Fatalf("with a line half written the table holds %q, want the lines before it alone", got)
	}
	appendTo(t, c.transcript, "2}\n")
	c.awaitRow(c.transcript, "{\"n\":1}\n{\"n\":2}\n")
	c.stop(1)
}

// A launcher saves over only what it last saved: once another writer has moved the row, a writer
// the pod's replacement holds, this launcher stops saving that file and says so, and neither its
// periodic saves nor its last one overwrite what the other wrote.
func TestAClaudeSaveStopsWhenAnotherWriterMovedTheRow(t *testing.T) {
	c := newClaudeRig(t, Config{})
	c.launch(1, "exec sleep 600", false)
	write(t, c.transcript, "{\"n\":1}\n", time.Now())
	c.awaitRow(c.transcript, "{\"n\":1}\n")
	other := "{\"writer\":\"other\"}\n"
	if err := ompsessions.Append(context.Background(), c.conn, c.transcript, int64(len("{\"n\":1}\n")), []byte(other), time.Now()); err != nil {
		t.Fatal(err)
	}
	appendTo(t, c.transcript, "{\"n\":2}\n")
	deadline := time.Now().Add(4 * saveInterval)
	for !strings.Contains(c.stderr.String(), "stops saving "+c.transcript) {
		if time.Now().After(deadline) {
			t.Fatalf("the launcher logged %q, want it to say it stops saving %s", c.stderr.String(), c.transcript)
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.stop(1)
	if got, _ := c.row(c.transcript); got != "{\"n\":1}\n"+other {
		t.Errorf("the table holds %q, want the other writer's row %q untouched", got, "{\"n\":1}\n"+other)
	}
}

// The phase reminder's record comes back with its count on a resume, so a worker in a new pod is
// reminded where its last one left off, and a record the plugin replaces is saved again.
func TestAClaudeResumeRestoresThePhaseReminderRecordWithItsCount(t *testing.T) {
	c := newClaudeRig(t, Config{})
	c.seed(c.transcript, "{\"n\":1}\n", time.Now())
	record := `{"state":"open","count":2,"reported":false}`
	c.seed(c.record, record, time.Now())
	told := filepath.Join(t.TempDir(), "seen")
	c.launch(1, seeing(c.record, told), true)
	if got := seen(t, told); got != record {
		t.Fatalf("the child found the phase-reminder record %q, want %q", got, record)
	}
	next := `{"state":"open","count":3,"reported":false}`
	write(t, c.record, next, time.Now())
	c.awaitRow(c.record, next)
	c.stop(1)
}

// A resume is told its workspace was recreated from the transcript's last save: the row's
// mtime_ms is the transcript's mtime when it was saved, so a workspace created after it was
// recreated since and one created before it was not.
func TestAClaudeResumeIsToldWhetherItsWorkspaceWasRecreatedSinceTheLastSave(t *testing.T) {
	c := newClaudeRig(t, Config{})
	dir := newWorkspace(t)
	c.launch(1, "exec sleep 600", false)
	written := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	write(t, c.transcript, "{\"n\":1}\n", written)
	c.awaitRow(c.transcript, "{\"n\":1}\n")
	c.stop(1)
	if at, ok, err := ompsessions.Written(context.Background(), c.conn, c.transcript); err != nil || !ok || !at.Equal(written) {
		t.Fatalf("the transcript's row was last written %v (%t, %v), want the file's mtime %v", at, ok, err, written)
	}
	told := func(generation uint64) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), "told")
		script := `printf '%s' "$` + WorkspaceRecreatedVariable + `" > "$TOLD.tmp" && mv "$TOLD.tmp" "$TOLD"; exec sleep 600`
		c.launch(generation, script, true, "LEGION_WORKSPACE="+dir, "TOLD="+file)
		value := seen(t, file)
		c.stop(generation)
		return value
	}
	if err := workspace.RecordCreated(dir, written.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := told(2); got != "true" {
		t.Errorf("a resume in a workspace created after the last save was told %q, want true", got)
	}
	if err := workspace.RecordCreated(dir, written.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := told(3); got != "false" {
		t.Errorf("a resume in a workspace created before the last save was told %q, want false", got)
	}
}

// A fresh transcript is marked saved only once the table holds it, which is what claude-envoy
// waits for before the daemon records it: while the session database refuses the save, here by
// holding a lock the save waits on, no marker is written and the child runs on; once the save
// commits the marker follows.
func TestAClaudeSaveMarksAFreshTranscriptOnlyAfterItsFirstCommit(t *testing.T) {
	c := newClaudeRig(t, Config{})
	pidFile := filepath.Join(t.TempDir(), "pid")
	c.launch(1, sleeping(pidFile), false)
	pid := pidOf(t, pidFile)
	marker := c.transcript + SavedMarkerSuffix
	ctx := context.Background()
	tx, err := c.conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE omp_session_files IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	write(t, c.transcript, "{\"n\":1}\n", time.Now())
	time.Sleep(saveInterval + saveInterval/2)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the transcript was marked saved while no save could commit (%v)", err)
	}
	if !alive(pid) {
		t.Fatal("the child is not running while its save waits")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, marker)
	if got, ok := c.row(c.transcript); !ok || got != "{\"n\":1}\n" {
		t.Errorf("once the transcript is marked saved the table holds %q (row %t), want the transcript", got, ok)
	}
	c.stop(1)
}

// sleeping is a child that writes its pid to file, then waits.
func sleeping(file string) string {
	return fmt.Sprintf(`printf '%%s' "$$" > %q.tmp && mv %q.tmp %q; exec sleep 600`, file, file, file)
}

// pidOf is the pid a sleeping child wrote.
func pidOf(t *testing.T, file string) int {
	t.Helper()
	pid, err := strconv.Atoi(seen(t, file))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// The last save runs once the child has exited, inside the stop's grace at the configuration's
// floor (shim.MinStopGrace) less the proxy's drain, after a child that spent its whole agent grace
// (shim.DefaultGrace) and wrote a last line on its way out: the table then holds either that line,
// the last save having landed, or exactly what the last periodic save left. Nothing else, and
// never a line cut short.
func TestAClaudeFinalSaveAtTheFloorGraceLandsOrKeepsTheLastPeriodicSave(t *testing.T) {
	c := newClaudeRig(t, Config{StopGrace: shim.MinStopGrace})
	script := fmt.Sprintf(`trap 'sleep %d; printf "{\"n\":\"last\"}\n" >> %q; exit 0' TERM; sleep 600 & wait`,
		int(shim.DefaultGrace/time.Second), c.transcript)
	c.launch(1, script, false)
	write(t, c.transcript, "{\"n\":1}\n", time.Now())
	c.awaitRow(c.transcript, "{\"n\":1}\n")
	got, ok := c.answer(shimwire.LauncherStop{ID: "stop-1", Generation: 1, GraceMs: int(shim.MinStopGrace.Milliseconds())}).(shimwire.LauncherStopResult)
	if !ok || !got.OK {
		t.Fatalf("stop result = %#v", got)
	}
	switch row, _ := c.row(c.transcript); row {
	case "{\"n\":1}\n{\"n\":\"last\"}\n":
		t.Log("the last save landed")
	case "{\"n\":1}\n":
		t.Log("the last save did not land; the last periodic save stands")
	default:
		t.Errorf("after the stop the table holds %q, want the last line saved or the last periodic save's %q", row, "{\"n\":1}\n")
	}
}

// Under a session database of the pod's own the launcher keeps the session through its
// generation's proxy socket, the one the child's URL file names in the agent's spelling.
func TestAClaudeSaveReachesTheTableThroughTheGenerationsProxy(t *testing.T) {
	dsn := testpg.Database(t, "legion_launcher_claude_proxy_test")
	database, dialer := sessionDatabaseAt(t, dsn)
	g := newSessionRig(t, Config{SessionDatabase: database, SessionDialer: dialer.build})
	file := filepath.Join(t.TempDir(), "OMP_SESSION_SQL_DSN")
	if err := os.WriteFile(file, []byte(dsn), 0o600); err != nil {
		t.Fatal(err)
	}
	c := claudeRigOn(t, g, file)
	command := c.command(1, "exec sleep 600", false)
	command.Env = append(command.Env, ompsessions.DSNFileVariable+"="+filepath.Join(g.generationDir(1), ompsessions.DSNFileName))
	if got := c.start(command); !got.OK {
		t.Fatalf("start = %#v", got)
	}
	write(t, c.transcript, "{\"n\":1}\n", time.Now())
	c.awaitRow(c.transcript, "{\"n\":1}\n")
	c.stop(1)
}

const launcherProcessEnv = "LEGION_LAUNCHER_TEST_PROCESS"

// TestLauncherProcess runs a launcher in a process of its own, so a test can kill it with
// SIGKILL as a node does a pod's.
func TestLauncherProcess(t *testing.T) {
	if os.Getenv(launcherProcessEnv) != "1" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	err := Run(ctx, Config{
		Connect: os.Getenv("LEGION_LAUNCHER_TEST_ADDRESS"), PrivateDir: os.Getenv("LEGION_LAUNCHER_TEST_PRIVATE"),
		Token: "launcher-token", Sandbox: "legion-legion-legion-208", Role: "tester", PodUID: "pod-1", Stderr: os.Stderr,
	})
	if err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
}

// processRig is a launcher in a process of its own, and kill its SIGKILL.
func processRig(t *testing.T) (g *rig, kill func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	private, err := os.MkdirTemp("", "lch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(private) })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(self, "-test.run=^TestLauncherProcess$")
	command.Env = append(os.Environ(), launcherProcessEnv+"=1", "LEGION_LAUNCHER_TEST_ADDRESS=tcp://"+listener.Addr().String(),
		"LEGION_LAUNCHER_TEST_PRIVATE="+private)
	// The launcher's output goes to a file rather than a pipe: its child inherits it, and a pipe
	// would hold the launcher's Wait open for as long as the child outlives the launcher.
	output, err := os.Create(filepath.Join(t.TempDir(), "launcher.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = command.Wait(); close(exited) }()
	kill = func() {
		_ = command.Process.Signal(syscall.SIGKILL)
		<-exited
	}
	t.Cleanup(kill)
	g = &rig{t: t, listener: listener, private: private, result: make(chan error, 1), stderr: &lockedBuffer{}}
	g.accept()
	return g, kill
}

// A pod killed outright, after a save, makes no last save: the table holds what its last save
// committed, a prefix of the transcript ending at a newline. A resume in the pod that replaces it,
// on a disk that holds nothing of the session, restores exactly that.
func TestAClaudeResumeAfterASIGKILLRestoresExactlyTheSavedPrefix(t *testing.T) {
	_, dsnFile := testpg.DSNFile(t, "legion_launcher_claude_kill_test")
	killed, kill := processRig(t)
	c := claudeRigOn(t, killed, dsnFile)
	pidFile := filepath.Join(t.TempDir(), "pid")
	c.launch(1, sleeping(pidFile), false)
	pid := pidOf(t, pidFile)
	saved := "{\"n\":1}\n{\"n\":2}\n"
	write(t, c.transcript, saved, time.Now())
	c.awaitRow(c.transcript, saved)
	appendTo(t, c.transcript, "{\"n\":3}\n{\"n\":")
	kill()
	// The kill takes the whole pod; the child, in a process group of its own, goes with it.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	held, _ := c.row(c.transcript)
	if held != saved && held != saved+"{\"n\":3}\n" {
		t.Fatalf("after the kill the table holds %q, want a newline-ended prefix of what was written", held)
	}
	if err := os.RemoveAll(c.configDir); err != nil {
		t.Fatal(err)
	}
	c.rig = newRig(t)
	told := filepath.Join(t.TempDir(), "seen")
	c.launch(1, seeing(c.transcript, told), true)
	if got := seen(t, told); got != held {
		t.Errorf("the replacement's child found the transcript %q, want exactly the saved %q", got, held)
	}
	c.stop(1)
}
