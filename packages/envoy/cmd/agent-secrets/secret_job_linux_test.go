//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
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
	command := "AGENT_SECRETS_JOB_HELPER=1 " + shellWord(os.Args[0]) + " -test.run='^TestPromptJobHelper$'"
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
	}
	real := readTerminal
	quiet := false
	readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
		if timeout >= 0 && !quiet {
			quiet = true
			fmt.Println("QUIET_READY")
		}
		return real(fd, wake, buf, timeout)
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
	// A single write exercises the kernel's input flush without a scheduling gap.
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
