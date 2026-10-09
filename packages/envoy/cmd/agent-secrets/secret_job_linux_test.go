//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// promptShell gives the prompt a parent interactive shell in the same session, so
// its process group is not orphaned and the kernel really stops it on Ctrl-Z.
type promptShell struct {
	t          *testing.T
	controller *os.File
	ctlFd      int // controller's fd, read once: (*os.File).Fd() would switch it to
	// blocking mode, silently breaking every later SetReadDeadline on controller.
	terminal int
	bash     *exec.Cmd
	out      bytes.Buffer
	pid      int
	history  string
	env      string // variables start sets for the helper, each followed by a space
}

// rawFd answers f's file descriptor through its SyscallConn, which, unlike
// (*os.File).Fd(), never switches f to blocking mode.
func rawFd(f *os.File) (int, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var fd int
	if ctlErr := raw.Control(func(sysfd uintptr) { fd = int(sysfd) }); ctlErr != nil {
		return 0, ctlErr
	}
	return fd, nil
}

func newPromptShell(t *testing.T) *promptShell {
	t.Helper()
	controller, terminal := openPTY(t)
	ctlFd, err := rawFd(controller)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Dup(terminal)
	if err != nil {
		t.Fatal(err)
	}
	tty := os.NewFile(uintptr(fd), "terminal")
	defer tty.Close()
	home := t.TempDir()
	cmd := exec.Command("bash", "--norc", "--noprofile", "-i")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "HISTFILE=" + filepath.Join(home, "history"), "PS1=PROMPT$ ", "TERM=dumb"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &promptShell{t: t, controller: controller, ctlFd: ctlFd, terminal: terminal, bash: cmd, history: filepath.Join(home, "history")}
	t.Cleanup(func() {
		if s.pid != 0 {
			_ = syscall.Kill(s.pid, syscall.SIGKILL)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	s.wait("PROMPT$ ")
	return s
}

func (s *promptShell) send(text string) {
	s.t.Helper()
	if _, err := s.controller.Write([]byte(text)); err != nil {
		s.t.Fatal(err)
	}
}

func (s *promptShell) wait(want string) {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 4096)
	for !strings.Contains(s.out.String(), want) {
		if time.Now().After(deadline) {
			s.t.Fatalf("waiting for %q; terminal showed %q", want, s.out.String())
		}
		_ = s.controller.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, _ := s.controller.Read(buf)
		s.out.Write(buf[:n])
	}
}

func (s *promptShell) start(wrapper bool, background bool) {
	s.t.Helper()
	command := s.env + "AGENT_SECRETS_JOB_HELPER=1 " + shellWord(os.Args[0]) + " -test.run='^TestPromptJobHelper$'"
	if background {
		// fg hands the job the terminal in the state bash hands every foreground
		// job, which is what the prompt must restore: not what the terminal holds
		// while the job waits behind bash's line editor.
		command = "AGENT_SECRETS_JOB_REFERENCE=" + s.foregroundState() + " " + command
	}
	if wrapper {
		command = "bash -c " + shellWord("trap '' TSTP; "+command+"; echo WRAPPER_DONE")
	}
	if background {
		command += " &"
	}
	s.out.Reset()
	s.send(command + "\r")
	s.wait("HELPER_PID=")
	s.wait("\r\nHELPER_READY\r\n")
	start := strings.Index(s.out.String(), "HELPER_PID=") + len("HELPER_PID=")
	pid, err := strconv.Atoi(strings.Fields(s.out.String()[start:])[0])
	if err != nil {
		s.t.Fatal(err)
	}
	s.pid = pid
	if background {
		s.wait("PROMPT_STARTING")
	} else {
		// The label shows once the prompt holds the terminal in its own mode: a stop
		// before it discards nothing, so each test's stop comes after it.
		s.wait("Value for DEMO_KEY: ")
	}
	s.out.Reset()
}

// foregroundState is the terminal state, as termiosKey writes it, that bash hands a
// foreground job: what a fresh foreground command reads at its start.
func (s *promptShell) foregroundState() string {
	s.t.Helper()
	s.out.Reset()
	s.send("AGENT_SECRETS_JOB_HELPER=1 AGENT_SECRETS_JOB_PROBE=1 " + shellWord(os.Args[0]) + " -test.run='^TestPromptJobHelper$'\r")
	s.wait("FOREGROUND_STATE=")
	s.wait(" END")
	s.wait("PROMPT$ ")
	start := strings.Index(s.out.String(), "FOREGROUND_STATE=") + len("FOREGROUND_STATE=")
	return strings.Fields(s.out.String()[start:])[0]
}

// termiosKey is every field of t, as one shell-safe word.
func termiosKey(t *unix.Termios) string {
	return fmt.Sprintf("%x.%x.%x.%x.%x.%x.%x.%x", t.Iflag, t.Oflag, t.Cflag, t.Lflag, t.Line, t.Cc[:], t.Ispeed, t.Ospeed)
}

func (s *promptShell) waitForeground(pid int) {
	s.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		foreground, err := unix.IoctlGetInt(s.ctlFd, unix.TIOCGPGRP)
		if err != nil {
			s.t.Fatal(err)
		}
		if foreground == pid {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("terminal foreground group = %d, want %d", foreground, pid)
		}
	}
}

func (s *promptShell) noShellValue(markers ...string) {
	s.t.Helper()
	s.wait("PROMPT$ ")
	s.wait("RESTORED=true LABEL_HELD=true")
	s.send("history -w; printf 'HISTORY_%s\\n' SAVED\r")
	s.wait("HISTORY_SAVED")
	history, err := os.ReadFile(s.history)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, marker := range markers {
		if strings.Contains(s.out.String(), marker) || bytes.Contains(history, []byte(marker)) {
			s.t.Fatalf("value reached the screen or shell history: terminal=%q history=%q", s.out.String(), history)
		}
	}
}

// labelCheck passes the prompt's writes to stderr and records whether its label
// reached the terminal only while the prompt held it: in the foreground, echo off.
type labelCheck struct {
	shown, held bool
}

func (c *labelCheck) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("Value for ")) {
		foreground, fgErr := unix.IoctlGetInt(0, unix.TIOCGPGRP)
		tio, tioErr := unix.IoctlGetTermios(0, unix.TCGETS)
		c.held = (!c.shown || c.held) && fgErr == nil && tioErr == nil &&
			foreground == unix.Getpgrp() && tio.Lflag&unix.ECHO == 0
		c.shown = true
	}
	return os.Stderr.Write(p)
}

func TestPromptJobHelper(t *testing.T) {
	if os.Getenv("AGENT_SECRETS_JOB_HELPER") == "" {
		t.Skip("subprocess helper")
	}
	if os.Getenv("AGENT_SECRETS_JOB_PROBE") != "" {
		tio, err := unix.IoctlGetTermios(0, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("FOREGROUND_STATE=%s END\n", termiosKey(tio))
		return
	}
	fmt.Printf("HELPER_PID=%d\nHELPER_READY\n", os.Getpid())
	if bound := os.Getenv("AGENT_SECRETS_JOB_PASTE_BOUND"); bound != "" {
		d, err := time.ParseDuration(bound)
		if err != nil {
			t.Fatal(err)
		}
		maxPasteDrain = d
	}
	reference := os.Getenv("AGENT_SECRETS_JOB_REFERENCE")
	if reference == "" {
		// Started in the foreground: bash has already handed it the terminal.
		saved, err := unix.IoctlGetTermios(0, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		reference = termiosKey(saved)
	} else {
		// Started with &: start the prompt only once bash's line editor holds the
		// terminal (canonical mode off), which a prompt in the background must
		// neither take as its baseline nor show its label over.
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			tio, err := unix.IoctlGetTermios(0, unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if tio.Lflag&unix.ICANON == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("bash's line editor never took the terminal")
			}
		}
		fmt.Println("PROMPT_STARTING")
		if os.Getenv("AGENT_SECRETS_JOB_AFTER_SHELL_EXIT") != "" {
			// Reach the prompt only once the shell's exit has taken the terminal from the
			// session, as a prompt descheduled that long on a loaded machine would.
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
				if _, err := unix.IoctlGetInt(0, unix.TIOCGPGRP); errors.Is(err, unix.ENOTTY) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the shell's exit never took the terminal")
				}
			}
		}
	}
	real := readTerminal
	quiet, readSome := false, false
	readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
		if timeout >= 0 && !quiet {
			quiet = true
			fmt.Println("QUIET_READY")
			if os.Getenv("AGENT_SECRETS_JOB_HOLD_QUIET") != "" {
				// Hold the first quiet window open until input comes, so what the test sends
				// after QUIET_READY arrives inside it however slowly this process runs.
				timeout = -1
			}
		}
		n, err := real(fd, wake, buf, timeout)
		if n > 0 && !readSome {
			readSome = true
			fmt.Println("READ_SOME")
		}
		return n, err
	}
	label := &labelCheck{}
	value, err := readSecretValue("DEMO_KEY", "agent-secrets secret set DEMO_KEY", label)
	restored, restoreErr := unix.IoctlGetTermios(0, unix.TCGETS)
	fmt.Printf("RETURNED %v MATCH=%t EMPTY=%t RESTORED=%t LABEL_HELD=%t\n", err, value == "headtail", value == "",
		restoreErr == nil && termiosKey(restored) == reference, label.shown && label.held)
}

func TestPromptJobBackgroundThenForegroundKeepsValueHidden(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("started-in-background=%t", background), func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, background)
			if !background {
				s.send("\x1a")
				s.wait("Stopped")
				s.wait("PROMPT$ ")
				s.out.Reset()
				s.send("bg\r")
				s.wait("PROMPT$ ")
			}
			// A stopped main thread is not proof that bash has reclaimed the tty.
			s.waitForeground(s.bash.Process.Pid)
			s.out.Reset()
			s.send("fg\r")
			s.wait("\x1b[?2004h")
			s.send("headtail\r")
			if background {
				s.wait("RETURNED <nil> MATCH=true")
			} else {
				s.wait("RETURNED nothing was stored:")
				s.wait("MATCH=false EMPTY=true")
			}
			s.noShellValue("head", "tail")
		})
	}
}

// A stop the prompt takes while it waits in the background, before its label,
// discards nothing: nothing has been typed for it yet.
func TestPromptJobStopBeforeTheLabelKeepsTheEntry(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, true)
	// Go leaves SIGTSTP at its default action until the prompt's watcher asks
	// for it, so a stop sent once the watcher catches it is the prompt's to handle.
	for deadline := time.Now().Add(5 * time.Second); !catches(t, s.pid, syscall.SIGTSTP); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the prompt never caught SIGTSTP")
		}
	}
	s.send("kill -TSTP %1\r")
	// The job is in the background, so bash never takes the terminal back for it, and
	// fg does not resume a job bash has not yet seen stop: wait for its job table.
	s.jobStopped()
	s.waitForeground(s.bash.Process.Pid)
	s.out.Reset()
	s.send("fg\r")
	s.wait("\x1b[?2004h")
	s.send("headtail\r")
	s.wait("RETURNED <nil> MATCH=true")
	s.noShellValue("head", "tail")
}

// jobStopped waits until bash's job table shows job 1 stopped: fg sends SIGCONT only to a
// job bash has already seen stop.
func (s *promptShell) jobStopped() {
	s.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		s.out.Reset()
		s.send("jobs %1\r")
		s.wait("PROMPT$ ")
		if strings.Contains(s.out.String(), "Stopped") {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("bash never saw the job stop: %q", s.out.String())
		}
	}
}

// A prompt whose process group is orphaned (the subshell that started it has exited) can
// never be brought to the foreground: it refuses, naming the pipe, rather than waiting forever,
// and shows no label over the shell's line.
func TestPromptJobOrphanedBackgroundPromptRefuses(t *testing.T) {
	s := newPromptShell(t)
	// Without job control a background command's standard input is /dev/null, so
	// the subshell names the terminal for it.
	command := "AGENT_SECRETS_JOB_REFERENCE=" + s.foregroundState() + " AGENT_SECRETS_JOB_HELPER=1 " +
		shellWord(os.Args[0]) + " -test.run='^TestPromptJobHelper$' </dev/tty"
	s.out.Reset()
	s.send("(" + command + " &)\r")
	s.wait("HELPER_PID=")
	start := strings.Index(s.out.String(), "HELPER_PID=") + len("HELPER_PID=")
	pid, err := strconv.Atoi(strings.Fields(s.out.String()[start:])[0])
	if err != nil {
		t.Fatal(err)
	}
	s.pid = pid
	s.wait("RETURNED no shell can bring the value prompt to the foreground of this terminal; pipe the value in: agent-secrets secret set DEMO_KEY < FILE")
	s.wait("LABEL_HELD=false")
	if strings.Contains(s.out.String(), "Value for") {
		t.Fatalf("an orphaned prompt showed its label: %q", s.out.String())
	}
	s.send("echo SHELL_ALIVE\r")
	s.wait("SHELL_ALIVE")
}

// A signal that ends the prompt while it waits in the background, after Ctrl-Z and bg, ends it by
// that signal at once, without fg: the terminal is the shell's then, and a restore from the
// background would stop the job until fg.
func TestPromptJobSignalInTheBackgroundEndsItWithoutFg(t *testing.T) {
	for _, tc := range []struct{ signal, report string }{{"TERM", "Terminated"}, {"HUP", "Hangup"}} {
		t.Run(tc.signal, func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, false)
			s.send("\x1a")
			s.wait("Stopped")
			s.wait("PROMPT$ ")
			s.out.Reset()
			s.send("bg\r")
			s.wait("PROMPT$ ")
			s.waitForeground(s.bash.Process.Pid)
			s.out.Reset()
			s.send("kill -" + tc.signal + " %1\r")
			// bash reaps a job that ends; one a restore stopped stays in /proc in state T.
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
				stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", s.pid))
				if os.IsNotExist(err) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the prompt was still there 5 s after SIG%s in the background: %q; terminal: %q", tc.signal, stat, s.out.String())
				}
			}
			s.pid = 0
			// bash prints a background job's end before its next prompt.
			s.send("printf 'SHELL_%s\\n' ALIVE\r")
			s.wait("SHELL_ALIVE\r\n")
			s.wait(tc.report)
			if strings.Contains(s.out.String(), "Exit") || strings.Contains(s.out.String(), "RETURNED") {
				t.Fatalf("the job ended by exiting, not by SIG%s: %q", tc.signal, s.out.String())
			}
			if strings.Contains(s.out.String(), "\x1b[?2004l") {
				t.Fatalf("the prompt wrote to the shell's terminal from the background: %q", s.out.String())
			}
		})
	}
}

// A prompt sent to the background with Ctrl-Z and bg, whose shell then exits, is orphaned: it
// refuses, naming the pipe command, and leaves the terminal to the shell that holds it now, with
// no write of its own (a bracketed-paste-off mark would land on that shell's line, and its line
// editor would read its next paste unbracketed).
func TestPromptJobOrphanedByItsShellsExitLeavesTheTerminalAlone(t *testing.T) {
	s := newPromptShell(t)
	s.send("bash --norc --noprofile -i\r")
	s.wait("PROMPT$ ")
	s.out.Reset()
	s.send("printf 'NESTED_%s\\n' READY\r")
	s.wait("NESTED_READY\r\n")
	s.wait("PROMPT$ ")
	s.start(false, false)
	s.send("\x1a")
	s.wait("Stopped")
	s.wait("PROMPT$ ")
	s.send("bg\r")
	s.wait("PROMPT$ ")
	s.out.Reset()
	s.send("exit\r")
	s.wait("RETURNED no shell can bring the value prompt to the foreground of this terminal; pipe the value in: agent-secrets secret set DEMO_KEY < FILE")
	s.pid = 0
	s.send("printf 'SHELL_%s\\n' ALIVE\r")
	s.wait("SHELL_ALIVE\r\n")
	if strings.Contains(s.out.String(), "\x1b[?2004l") {
		t.Fatalf("the orphaned prompt wrote to the terminal the outer shell holds: %q", s.out.String())
	}
}

// processState is the state letter /proc gives process pid ("T" while stopped).
func processState(t *testing.T, pid int) string {
	t.Helper()
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+2:]))[0]
}

// catches reports whether process pid has a handler of its own installed for sig.
func catches(t *testing.T, pid int, sig syscall.Signal) bool {
	t.Helper()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if mask, ok := strings.CutPrefix(line, "SigCgt:"); ok {
			bits, err := strconv.ParseUint(strings.TrimSpace(mask), 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			return bits&(1<<(uint(sig)-1)) != 0
		}
	}
	t.Fatalf("no SigCgt line in /proc/%d/status", pid)
	return false
}

func TestPromptJobFastForegroundDoesNotRestopOrExposeValue(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("bg-before-fg=%t", background), func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, false)
			s.send("\x1a")
			// Queue fg at the first stopped thread, without waiting for all of the
			// job's threads or bash to reclaim the terminal.
			for deadline := time.Now().Add(5 * time.Second); processState(t, s.pid) != "T"; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("prompt did not stop")
				}
			}
			if background {
				s.send("bg\r")
			}
			s.send("fg\r")
			s.wait("\x1b[?2004h")
			s.out.Reset()
			s.send("headtail\r")
			s.wait("RETURNED nothing was stored:")
			s.wait("MATCH=false EMPTY=true")
			// Bash's own "Stopped" notice can lag arbitrarily far behind a real
			// stop-then-resume it already handled (it only prints it once its own
			// job-table check next runs), so it is not evidence of a second stop by
			// itself. A second bracketed-paste-on write is: the reader emits
			// exactly one, from apply(), each time it (re)gains the terminal, so a
			// second one here means a second, unwanted stop-and-resume cycle.
			if strings.Contains(s.out.String(), "\x1b[?2004h") {
				t.Fatalf("prompt reapplied terminal settings after fg (stopped again): %q", s.out.String())
			}
			s.noShellValue("head", "tail")
		})
	}
}

func TestPromptJobControlByteDrainsBeforeReturningToShell(t *testing.T) {
	for _, control := range []string{"\t", "\x1b[D", "\x00"} {
		t.Run(fmt.Sprintf("%x", control), func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, false)
			s.send("prefix" + control)
			for _, c := range "private-tail" {
				time.Sleep(80 * time.Millisecond)
				s.send(string(c))
			}
			s.send("\r")
			s.wait("RETURNED the control byte")
			s.noShellValue("private-tail")
		})
	}
}

func TestPromptJobInheritedIgnoredStopStaysIgnored(t *testing.T) {
	s := newPromptShell(t)
	s.start(true, false)
	s.send("head\x1a")
	time.Sleep(100 * time.Millisecond)
	if lflag(t, s.terminal)&unix.ECHO != 0 {
		t.Fatal("ignored Ctrl-Z restored echo")
	}
	s.send("tail\r")
	s.wait("RETURNED <nil> MATCH=true")
	s.wait("WRAPPER_DONE")
	s.noShellValue("headtail")
}

func TestPromptControlByteInsideBracketedPasteDrainsThroughEnd(t *testing.T) {
	for _, control := range []string{"\x00", "\t", "\x01", "\x7f"} {
		controller, terminal, got := typeAtPromptInWrites(t, 300*time.Millisecond,
			"\x1b[200~prefix"+control, "private-tail\n\x1b[201~")
		if got.err == nil || !strings.Contains(got.err.Error(), "control byte") || got.line != nil {
			t.Fatalf("control %q: got %q, %v", control, got.line, got.err)
		}
		if unread(t, terminal) != 0 || bytes.Contains(shown(t, controller), []byte("private-tail")) {
			t.Fatal("paste tail leaked")
		}
	}
}

func TestPromptJobStopDuringQuietWindowDoesNotHang(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, false)
	s.send("headtail\r")
	s.wait("QUIET_READY")
	s.send("\x1a")
	s.wait("Stopped")
	s.out.Reset()
	s.send("fg\r")
	// No further input is needed to finish the quiet window after a resume.
	s.wait("RETURNED nothing was stored:")
	s.wait("MATCH=false EMPTY=true")
	s.noShellValue("headtail")
}

func TestPromptJobStopDiscardsQueuedInputWithoutLeaking(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, false)
	// A single write exercises the prompt's input flush without a scheduling gap.
	s.send("head\x1a")
	s.wait("Stopped")
	s.out.Reset()
	s.send("fg\r")
	s.wait("Nothing was stored. Press Enter")
	s.wait("run agent-secrets secret set DEMO_KEY again and type the whole value")
	s.send("tail\r")
	s.wait("RETURNED nothing was stored:")
	s.wait("MATCH=false EMPTY=true")
	s.noShellValue("head", "tail")

	s.start(false, false)
	s.send("headtail\r")
	s.wait("RETURNED <nil> MATCH=true")
	s.noShellValue("head", "tail")
}

// Ctrl-Z discards whatever follows it in the same write, as a fast typist's or a paste's keys
// arrive: the prompt stops with the entry refused, and none of it reaches the shell.
func TestPromptJobStopKeyDiscardsWhatFollowsIt(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, false)
	s.send("partone\x1aparttwo\r")
	s.wait("Stopped")
	s.wait("PROMPT$ ")
	s.out.Reset()
	s.send("fg\r")
	s.wait("Nothing was stored. Press Enter")
	s.send("\r")
	s.wait("RETURNED nothing was stored:")
	s.wait("MATCH=false EMPTY=true")
	s.noShellValue("partone", "parttwo")
}

// A signal key inside a bracketed paste is pasted text, not a keypress: the prompt refuses it as a
// control byte and discards the paste through its end, in one write or with the rest arriving
// later, so none of the paste reaches the shell.
func TestPromptJobSignalKeyInsideAPasteDrainsThroughItsEnd(t *testing.T) {
	for _, key := range []string{"\x03", "\x1a", "\x1c"} {
		for _, gap := range []time.Duration{0, 300 * time.Millisecond} {
			t.Run(fmt.Sprintf("%x/%s", key, gap), func(t *testing.T) {
				s := newPromptShell(t)
				s.start(false, false)
				head, tail := "\x1b[200~pastehead"+key, "pastetail\r\x1b[201~"
				if gap == 0 {
					s.send(head + tail)
				} else {
					s.send(head)
					time.Sleep(gap)
					s.send(tail)
				}
				s.wait(fmt.Sprintf("RETURNED the control byte 0x%02x cannot be typed at the prompt", key[0]))
				s.noShellValue("pastehead", "pastetail")
			})
		}
	}
}

// A paste that begins after the typed line, while the prompt drains what follows it, is read
// through its closing mark like one in the line: a signal key inside it is pasted text, and the
// paste's rest, arriving after the quiet window would have ended, is discarded rather than run by
// the shell. The entry is refused as more than one line.
func TestPromptJobPasteAfterTheLineDrainsThroughItsEnd(t *testing.T) {
	for _, key := range []string{"\x03", "\x1c", "\x1a", ""} {
		name := fmt.Sprintf("%x", key)
		if key == "" {
			name = "no-key"
		}
		t.Run(name, func(t *testing.T) {
			s := newPromptShell(t)
			s.env = "AGENT_SECRETS_JOB_HOLD_QUIET=1 "
			s.start(false, false)
			s.send("headtail\r")
			s.wait("QUIET_READY")
			s.send("\x1b[200~pastelate" + key)
			time.Sleep(300 * time.Millisecond)
			s.send("echo PASTE_$((2+3))\r\x1b[201~")
			s.wait("RETURNED a value of more than one line must be piped in")
			// Clear the shell's line, where a leaked paste mark would leave text that breaks the
			// history check's own command, so a leak shows as the markers below.
			s.send("\x15")
			s.noShellValue("pastelate", "PASTE_$((", "PASTE_5")
		})
	}
}

// A bracketed paste that begins in the same read as the line's end is read on through its closing
// mark, not given back to the shell after the line: the prompt keeps the terminal, so the paste's
// rest, arriving after the quiet window would have ended, is discarded rather than run by the shell.
// The entry is refused as more than one line. At f7e1c781 feed returned at the line's end and never
// saw the paste, so the prompt gave the terminal back after the quiet window and the rest ran.
func TestPromptJobPasteInTheLineEndReadDrainsThroughItsEnd(t *testing.T) {
	for _, key := range []string{"\x03", "\x1c", "\x1a", ""} {
		name := fmt.Sprintf("%x", key)
		if key == "" {
			name = "no-key"
		}
		t.Run(name, func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, false)
			// One write carries the line's end and the paste's start; the prompt reads both at
			// once, so a paste the prompt only watches for after the line would be missed.
			s.send("headtail\r\x1b[200~pastelate" + key)
			s.wait("QUIET_READY")
			// Longer than the quiet window: a prompt that did not see the paste gives the
			// terminal back before this arrives, and the shell runs it.
			time.Sleep(300 * time.Millisecond)
			s.send("echo PASTE_$((2+3))\r\x1b[201~")
			s.wait("RETURNED a value of more than one line must be piped in")
			// Clear the shell's line, where a leaked paste mark would leave text that breaks
			// the history check's own command, so a leak shows as the markers below.
			s.send("\x15")
			s.noShellValue("headtail", "pastelate", "PASTE_$((", "PASTE_5")
		})
	}
}

// A paste-start mark split so that only its first bytes arrive in the read that ends the line is
// held as pending and completed from the next read: the paste opens, so a Ctrl-C after its content
// is pasted text, not a keypress, and the entry is refused as more than one line rather than its
// content run by the shell. At f7e1c781 the split mark was dropped at the line's end, so the
// completion was plain text and the Ctrl-C a signal that killed the prompt. The helper shortens the
// paste bound so the refusal does not wait the full bound for a close mark that never comes.
func TestPromptJobSplitPasteMarkAfterTheLineIsRead(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_HOLD_QUIET=1 AGENT_SECRETS_JOB_PASTE_BOUND=1s "
	s.start(false, false)
	s.send("headtail\r\x1b[20")
	s.wait("QUIET_READY")
	s.send("0~pastelate\x03")
	s.wait("RETURNED a value of more than one line must be piped in")
	s.noShellValue("headtail", "pastelate")
}

// A second bracketed paste that begins in the same read that closes a pasted line opens like any
// other: the prompt keeps reading, so its rest, arriving after the quiet window would have ended,
// is discarded rather than run by the shell, and the pasted line is refused as more than one line.
// At f7e1c781 feed returned at the first paste's close and never saw the second, and the drain was
// skipped after a pasted line, so the first paste was stored and the rest ran in the shell.
func TestPromptJobSecondPasteInThePastedLinesCloseIsRead(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, false)
	s.send("\x1b[200~firstpaste\r\x1b[201~\x1b[200~pastelate")
	s.wait("QUIET_READY")
	time.Sleep(300 * time.Millisecond)
	s.send("echo PASTE_$((2+3))\r\x1b[201~")
	s.wait("RETURNED a value of more than one line must be piped in")
	s.send("\x15")
	s.noShellValue("firstpaste", "pastelate", "PASTE_$((", "PASTE_5")
}

// A paste whose closing mark never comes, as from a terminal that sends only the start mark, is
// given up once maxPasteDrain has passed since it began: the prompt ends, refusing the entry,
// though the signal keys pressed meanwhile are pasted text. The helper shortens the bound.
func TestPromptJobPasteThatNeverEndsIsGivenUp(t *testing.T) {
	for _, key := range []string{"\x03", "\x1c", "\x1a"} {
		t.Run(fmt.Sprintf("%x", key), func(t *testing.T) {
			s := newPromptShell(t)
			s.env = "AGENT_SECRETS_JOB_PASTE_BOUND=1s "
			s.start(false, false)
			s.send("\x1b[200~pasteopen" + key)
			s.wait("RETURNED read the value at the terminal: " + errPasteCutShort.Error())
			s.noShellValue("pasteopen")
		})
	}
}

// A signal key that follows a paste's closing mark in the same read is a keypress: it acts as it
// would in a read of its own, so the pasted value is not stored. Ctrl-C and Ctrl-\ end the
// process by their signals; Ctrl-Z stops it, and after fg the entry is refused.
func TestPromptJobKeyAfterAPasteInOneWriteActs(t *testing.T) {
	paste := "\x1b[200~pastevalue\r\x1b[201~"
	for _, tc := range []struct{ key, status string }{{"\x03", "130"}, {"\x1c", "131"}} {
		t.Run(fmt.Sprintf("%x", tc.key), func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, false)
			s.send(paste + tc.key)
			s.wait("PROMPT$ ")
			s.send("echo STATUS_$?\r")
			s.wait("STATUS_" + tc.status)
			if strings.Contains(s.out.String(), "RETURNED") {
				t.Fatalf("want the process ended by its signal (status %s), not a returned value: %q", tc.status, s.out.String())
			}
		})
	}
	t.Run("1a", func(t *testing.T) {
		s := newPromptShell(t)
		s.start(false, false)
		s.send(paste + "\x1a")
		s.wait("Stopped")
		s.wait("PROMPT$ ")
		s.out.Reset()
		s.send("fg\r")
		s.wait("Nothing was stored. Press Enter")
		s.send("\r")
		s.wait("RETURNED nothing was stored:")
		s.wait("MATCH=false EMPTY=true")
		s.noShellValue("pastevalue")
	})
}

// A prompt started with & whose shell then exits has no shell to bring it forward, though the
// terminal stays open: it refuses, naming the pipe, rather than showing its label to nobody and
// reading a value there.
func TestPromptJobShellGoneRefuses(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, true)
	s.send("exit\r")
	s.wait("RETURNED no shell can bring the value prompt to the foreground of this terminal; pipe the value in: agent-secrets secret set DEMO_KEY < FILE")
	s.wait("LABEL_HELD=false")
	if strings.Contains(s.out.String(), "Value for") {
		t.Fatalf("a prompt whose shell had gone showed its label: %q", s.out.String())
	}
}

// A prompt started with & that reaches the terminal only after its shell has exited finds a
// terminal that is no longer its controlling one: it refuses as when the shell exits under it,
// rather than take the terminal for one job control never applied to.
func TestPromptJobShellGoneBeforeThePromptStartsRefuses(t *testing.T) {
	s := newPromptShell(t)
	s.env = "AGENT_SECRETS_JOB_AFTER_SHELL_EXIT=1 "
	s.start(false, true)
	s.send("exit\r")
	s.wait("RETURNED no shell can bring the value prompt to the foreground of this terminal; pipe the value in: agent-secrets secret set DEMO_KEY < FILE")
	s.wait("LABEL_HELD=false")
	if strings.Contains(s.out.String(), "Value for") {
		t.Fatalf("a prompt that started after its shell had gone showed its label: %q", s.out.String())
	}
}

// A stop no handler sees (SIGSTOP from another process) lets the shell put its own mode back,
// echo on, while the job is stopped. After fg the prompt puts its own mode back before it reads
// again, so the rest of the value stays hidden.
func TestPromptJobExternalStopKeepsTheValueHidden(t *testing.T) {
	s := newPromptShell(t)
	s.start(false, false)
	s.send("head")
	// The prompt must have read the bytes first: the shell would read any still queued.
	s.wait("READ_SOME")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		queued, err := unix.IoctlGetInt(s.terminal, unix.TIOCINQ)
		if err != nil {
			t.Fatal(err)
		}
		if queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the prompt left %d bytes unread", queued)
		}
	}
	if err := syscall.Kill(s.pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	s.wait("Stopped")
	s.wait("PROMPT$ ")
	s.out.Reset()
	s.send("fg\r")
	// The prompt's mode is back once it turns bracketed paste on again.
	s.wait("\x1b[?2004h")
	s.send("tail\r")
	s.wait("RETURNED <nil> MATCH=true")
	s.noShellValue("head", "tail")
}

func TestPromptHangupRestoresAndAbortCannotDumpTheValue(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM, syscall.SIGABRT} {
		t.Run(sig.String(), func(t *testing.T) {
			controller, terminal := openPTY(t)
			fd, err := unix.Dup(terminal)
			if err != nil {
				t.Fatal(err)
			}
			tty := os.NewFile(uintptr(fd), "terminal")
			defer tty.Close()
			dir := t.TempDir()
			cmd := exec.Command("bash", "-c",
				`ulimit -c "$(ulimit -Hc)" && exec env --default-signal=HUP,TERM,ABRT "$0" -test.run='^TestPromptSignalHelper$'`,
				testBinary(t))
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "AGENT_SECRETS_PROMPT_HELPER=1", "GOTRACEBACK=crash")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			awaitEchoOff(t, terminal)
			if _, err := controller.Write([]byte("private-value")); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			// Keep consuming until the process exits; a fixed read deadline can fill
			// the pty halfway through Go's SIGABRT diagnostics and block the crash.
			output := make(chan []byte, 1)
			go func() {
				var out bytes.Buffer
				buf := make([]byte, 4096)
				for {
					n, err := controller.Read(buf)
					out.Write(buf[:n])
					if err != nil {
						output <- out.Bytes()
						return
					}
				}
			}()
			exitWithin := 10 * time.Second
			if sig == syscall.SIGABRT {
				// Go's GOTRACEBACK=crash relays SIGQUIT between threads. A
				// SIGQUIT subscriber can make that relay wait for its 10s
				// watchdog before the runtime raises the fatal SIGABRT.
				exitWithin = 30 * time.Second
			}
			select {
			case <-done:
			case <-time.After(exitWithin):
				_ = controller.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
				state, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", cmd.Process.Pid))
				t.Fatalf("signal did not end the prompt; process: %s; terminal: %q", state, <-output)
			}
			if err := controller.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			out := <-output
			status := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !status.Signaled() || status.Signal() != sig || status.CoreDump() {
				t.Fatalf("status=%v; want signal %v without a core", status, sig)
			}
			left, err := os.ReadDir(dir)
			if err != nil || len(left) != 0 {
				t.Fatalf("core directory=%v, err=%v", left, err)
			}
			if bytes.Contains(out, []byte("private-value")) {
				t.Fatal("signal output exposed the value")
			}
			if sig != syscall.SIGABRT && lflag(t, terminal)&(unix.ECHO|unix.ICANON) != unix.ECHO|unix.ICANON {
				t.Fatal("signal did not restore the terminal")
			}
		})
	}
}
