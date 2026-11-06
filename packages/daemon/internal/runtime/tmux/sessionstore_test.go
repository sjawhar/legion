package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// Under runtime.tmux.session_store postgres a resume of an Oh My Pi session is held to the session
// table, as a pod's launcher holds it: a session the table lacks is refused, never a fresh agent —
// in a database no Oh My Pi has opened yet, and even with a file at its path — before anything is
// written or tmux runs; a session the table holds resumes, with no file at its path at all.
func TestAResumeUnderASessionDatabaseIsHeldToTheSessionTable(t *testing.T) {
	_, dsnFile := testpg.DSNFile(t, "legion_tmux_sessions_test")
	ctx := context.Background()
	stateDir := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{
		Project: "omp", StateDir: stateDir, StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", Launchers: ompOnly("omp"), StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=" + bin},
		Executable: func() (string, error) { return "/opt/legion", nil }, SessionDSNFile: dsnFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	rt.run = func(context.Context, []string) (result, error) { ran++; return result{}, errors.New("tmux ran") }
	prompt := filepath.Join(t.TempDir(), "tester.md")
	if err := os.WriteFile(prompt, []byte("tester"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Prompt.RolePromptPaths = []string{prompt}
	sessions := filepath.Join(stateDir, "home", ".omp", "agent", "sessions")
	held, lacked := filepath.Join(sessions, "held.jsonl"), filepath.Join(sessions, "lacked.jsonl")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lacked, []byte(`{"type":"session","version":3,"id":"lacked"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refusal := func(file string) string {
		return "Refusing to start LEGION-43 fresh while respawning: recorded OMP session is not in the session table: " + file
	}
	resume := func(file string) error {
		resumed := spec
		resumed.ResumeSessionFile = file
		_, err := rt.Resume(ctx, nil, resumed)
		return err
	}

	if err := resume(held); err == nil || err.Error() != refusal(held) {
		t.Errorf("Resume before any Oh My Pi made the table = %v, want %q", err, refusal(held))
	}
	conn, err := ompsessions.Connect(ctx, dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := ompsessions.Import(ctx, conn, held, []byte(`{"type":"session","version":3,"id":"held"}`+"\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := resume(lacked); err == nil || err.Error() != refusal(lacked) {
		t.Errorf("Resume of a session the table lacks, its file present = %v, want %q", err, refusal(lacked))
	}
	if ran != 0 {
		t.Errorf("a refused resume ran tmux %d times", ran)
	}
	if entries, _ := os.ReadDir(filepath.Join(stateDir, "secrets")); len(entries) != 0 {
		t.Errorf("a refused resume wrote %d secret files", len(entries))
	}

	if err := resume(held); ran == 0 {
		t.Errorf("a resume of a session the table holds never reached tmux: %v", err)
	}
}

// A resume whose session database does not answer is a launch failure naming the session, not a
// refusal to resume and not a fresh agent: the look-up said nothing about the session.
func TestAResumeWhoseSessionDatabaseDoesNotAnswerFails(t *testing.T) {
	stateDir := t.TempDir()
	dsnFile := filepath.Join(t.TempDir(), "OMP_SESSION_SQL_DSN")
	if err := os.WriteFile(dsnFile, []byte("postgres://legion@127.0.0.1:1/legion?connect_timeout=5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{
		Project: "omp", StateDir: stateDir, StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", Launchers: ompOnly("omp"), StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=" + bin},
		Executable: func() (string, error) { return "/opt/legion", nil }, SessionDSNFile: dsnFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.run = func(context.Context, []string) (result, error) { return result{}, errors.New("tmux ran") }
	prompt := filepath.Join(t.TempDir(), "tester.md")
	if err := os.WriteFile(prompt, []byte("tester"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Prompt.RolePromptPaths = []string{prompt}
	spec.ResumeSessionFile = filepath.Join(stateDir, "session.jsonl")

	_, err = rt.Resume(context.Background(), nil, spec)
	want := "spawn legion-omp-legion-43-tester: resume session " + spec.ResumeSessionFile + ": "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Resume against a database that does not answer = %v, want an error beginning %q", err, want)
	}
}
