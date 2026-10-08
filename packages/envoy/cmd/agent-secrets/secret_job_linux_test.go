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
	if !background {
		s.wait("\x1b[?2004h")
	}
	s.out.Reset()
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
	s.wait("RESTORED=true")
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

func TestPromptJobHelper(t *testing.T) {
	if os.Getenv("AGENT_SECRETS_JOB_HELPER") == "" {
		t.Skip("subprocess helper")
	}
	fmt.Printf("HELPER_PID=%d\nHELPER_READY\n", os.Getpid())
	saved, err := unix.IoctlGetTermios(0, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
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
	value, err := readSecretValue("DEMO_KEY", "agent-secrets secret set DEMO_KEY", os.Stderr)
	restored, restoreErr := unix.IoctlGetTermios(0, unix.TCGETS)
	fmt.Printf("RETURNED %v MATCH=%t EMPTY=%t RESTORED=%t\n", err, value == "headtail", value == "", restoreErr == nil && *saved == *restored)
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

func TestPromptJobFastForegroundDoesNotRestopOrExposeValue(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("bg-before-fg=%t", background), func(t *testing.T) {
			s := newPromptShell(t)
			s.start(false, false)
			s.send("\x1a")
			// Queue fg at the first stopped thread, without waiting for all of the
			// job's threads or bash to reclaim the terminal.
			for deadline := time.Now().Add(5 * time.Second); ; {
				stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", s.pid))
				if err != nil {
					t.Fatal(err)
				}
				if fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+2:])); fields[0] == "T" {
					break
				}
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
				os.Args[0])
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
