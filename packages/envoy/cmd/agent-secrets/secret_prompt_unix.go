// packages/envoy/cmd/agent-secrets/secret_prompt_unix.go
//go:build (linux || darwin) && (amd64 || arm64)

// The value prompt's reader on Linux and macOS. The reader alone changes the terminal;
// the signal watcher reports a resume or a terminating signal without touching it.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// pasteGapDeciseconds is how long, in tenths of a second, the terminal must stay quiet after the
// line before the reader takes it as the whole value, on a terminal that does not bracket pastes:
// the rest of a paste can arrive in a later write than its first line.
const pasteGapDeciseconds = 2

// maxPasteDrain bounds how long the reader goes on reading, and so discarding, input that keeps
// arriving after the line.
const maxPasteDrain = 10 * time.Second

// The bracketed-paste sequences (xterm's mode 2004): the reader turns the mode on while it reads,
// and a terminal that supports it then sends each paste between pasteStart and pasteEnd.
var (
	bracketedPasteOn  = []byte("\x1b[?2004h")
	bracketedPasteOff = []byte("\x1b[?2004l")
	pasteStart        = []byte("\x1b[200~")
	pasteEnd          = []byte("\x1b[201~")
)

// readHiddenAtTerminal reads one hidden line, drains refused input, and restores the
// terminal only after joining its signal watcher. Bracketed pastes are read through
// their closing mark; unbracketed input is drained through a 200 ms quiet window.
// prompt runs once the reader holds the terminal with echo off, to show the label:
// a prompt started in the background shows nothing until fg gives it the terminal.
func readHiddenAtTerminal(fd int, prompt, onStop func()) (line []byte, err error) {
	if err := disableCoreDumps(); err != nil {
		return nil, err
	}
	wake, notify, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer wake.Close()
	defer notify.Close()
	events, stop, ignoredControls, err := watchPromptSignals(notify)
	if err != nil {
		return nil, err
	}
	tty := promptTerminal{fd: fd, wake: int(wake.Fd()), events: events, wait: -1, onStop: onStop}
	var saved *unix.Termios // nil until the reader holds the terminal: nothing to restore before
	defer func() {
		pending := stop()
		if pending.sig != 0 && !promptStopSignal(pending.sig) {
			tty.death = pending.sig
		}
		if pending.err != nil && err == nil {
			line, err = nil, pending.err
		}
		if tty.stopped || promptStopSignal(pending.sig) {
			line = nil
			if err == nil {
				err = errPromptStopped
			}
		}
		// A tcsetattr from the background blocks, by the kernel's own job control
		// (SIGTTOU is no longer caught), until the job is foregrounded, then
		// completes: that block is the ordinary outcome here, not a race to route
		// around with a foreground snapshot of our own. It can only fail to ever
		// complete when the group is orphaned (EIO, discarded rather than
		// blocked) or fd is not this process's controlling terminal at all
		// (ENOTTY, as the unit tests' bare pseudo-terminal): neither leaves
		// anything to restore to.
		if saved != nil {
			_, _ = unix.Write(fd, bracketedPasteOff)
			if restoreErr := tty.restoreTerminal(saved); restoreErr != nil && err == nil {
				line, err = nil, restoreErr
			}
		}
		if tty.death != 0 {
			endByPromptSignal(tty.death)
		}
	}()
	if saved, err = tty.holdTerminal(); err != nil {
		return nil, err
	}
	tty.current = *saved
	// Keep the kernel's signal flush: NOFLSH would leave unread secret bytes for
	// bash when a stop gives it the foreground. Every caught stop invalidates the entry.
	tty.current.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.NOFLSH
	tty.current.Lflag |= unix.ISIG
	var ignoredKeys [3]byte
	for i, cc := range ignoredControls {
		if key := saved.Cc[cc]; key != 0 && key != 0xff {
			ignoredKeys[i] = key
			// Even an ignored tty signal flushes input. Disable its tty character
			// and let the reader consume it without flushing the value.
			tty.current.Cc[cc] = disabledControlByte
		}
	}
	// Poll owns both waits. Reads themselves never block, including after resume.
	tty.current.Cc[unix.VMIN], tty.current.Cc[unix.VTIME] = 0, 0
	if err := tty.apply(); err != nil {
		return nil, err
	}
	if prompt != nil {
		prompt()
	}
	r := promptReader{ignored: ignoredKeys, special: func(c byte, index int) bool {
		v := saved.Cc[index]
		return v != 0 && v != 0xff && c == v
	}}
	buf := make([]byte, 512)
	for {
		n, err := tty.read(buf)
		if err != nil {
			return nil, err
		}
		if tty.stopped {
			// The kernel may have flushed bytes or paste marks, with no count of
			// what was lost. Drain a fresh line instead of keeping a partial value.
			r = promptReader{special: r.special, ignored: r.ignored, err: errPromptStopped}
			tty.stopped = false
		}
		if n == 0 {
			switch {
			case r.err != nil:
				return nil, r.err
			case r.more:
				return nil, errMoreThanOneLine
			case r.inPaste:
				return nil, errPasteCutShort
			}
			return nil, errValueCutShort
		}
		if r.feed(buf[:n]) {
			break
		}
	}
	if !r.pasted || r.err != nil {
		more, err := moreAfterTheLine(&tty, buf)
		if err != nil {
			return nil, err
		}
		r.more = r.more || more
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.more {
		return nil, errMoreThanOneLine
	}
	return r.line, nil
}

type promptTerminal struct {
	fd      int
	current unix.Termios
	wake    int
	events  <-chan promptSignal
	death   syscall.Signal
	wait    int // poll timeout in milliseconds; -1 until the line ends
	stopped bool
	onStop  func()
	resumed bool
	notice  bool
	applied bool // whether t.current and bracketed paste are already in effect
}

// apply is a no-op once the terminal already carries t.current and bracketed
// paste: a stop-then-resume can reach here through more than one path for the
// very same kernel event (the watcher's own channel delivery, and a bare
// EINTR on whichever other blocked syscall that same resume also woke), and
// neither termios nor paste mode is touched by a stop or a resume by
// themselves, so repeating the ioctl and the paste-on write would only be
// redundant, and visibly so over a real terminal. signal clears applied the
// moment it records a new stop, so the next genuine resume still reapplies.
func (t *promptTerminal) apply() error {
	if t.applied {
		return nil
	}
	for {
		err := unix.IoctlSetTermios(t.fd, ioctlSetTermios, &t.current)
		if errors.Is(err, unix.EINTR) {
			if err := t.waitForeground(); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	// A terminal opened read-only cannot enable bracketed paste.
	_, _ = unix.Write(t.fd, bracketedPasteOn)
	t.applied = true
	return nil
}

// restoreTerminal puts the terminal back to saved, reached only after the signal watcher has
// joined. A tcsetattr from a background process group blocks, by the kernel's own job control,
// until the job is foregrounded, then completes normally; ENOTTY (fd is not this process's
// controlling terminal, as the unit tests' bare pseudo-terminal) or EIO (the group is orphaned,
// which the kernel discards rather than blocks) mean there is nothing to restore to.
func (t *promptTerminal) restoreTerminal(saved *unix.Termios) error {
	for {
		err := unix.IoctlSetTermios(t.fd, ioctlSetTermiosFlush, saved)
		if errors.Is(err, unix.EINTR) {
			if err := t.waitForeground(); err != nil {
				return err
			}
			continue
		}
		if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EIO) {
			return nil
		}
		return err
	}
}

func (t *promptTerminal) signal(event promptSignal) error {
	if event.err != nil {
		return event.err
	}
	if promptStopSignal(event.sig) {
		t.stopped, t.notice, t.applied = true, true, false
	} else if event.sig != syscall.SIGCONT {
		t.death = event.sig
		return fmt.Errorf("value prompt ended by %s", event.sig)
	}
	t.resumed = true
	return nil
}

// SIGCONT from bg resumes the process, not its ownership of the terminal. Do not
// apply termios or read again until fg has actually handed the terminal back.
//
// A background start races a shell's single fg handshake: the kernel's own
// SIGTTOU stop on the first termios-changing ioctl can resume and recheck
// foreground ownership entirely inside that one blocked syscall, with no
// EINTR ever reaching apply()'s own retry. If that recheck still finds the
// old group in front (tcsetpgrp has not yet taken effect), the process stops
// itself again before fg's single SIGCONT-and-wait ever returns control to
// us, and nothing sends a second SIGCONT. So every caller touches the
// terminal only after this has independently confirmed foreground with a
// read-only ioctl of its own, which never triggers that stop.
func (t *promptTerminal) waitForeground() error {
	if sid, err := unix.Getsid(0); err == nil && sid == unix.Getpid() {
		// A session leader is never stopped by this terminal: it either just
		// acquired it (already foreground, by the kernel's own TIOCSCTTY rule)
		// or its process group is orphaned, where the kernel skips job control
		// entirely (tty_check_change's own orphan check) rather than stopping
		// it. Either way nothing will ever change what TIOCGPGRP answers here.
		return nil
	}
	var retry *time.Ticker
	defer func() {
		if retry != nil {
			retry.Stop()
		}
	}()
	for {
		foreground, err := unix.IoctlGetInt(t.fd, unix.TIOCGPGRP)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ENOTTY) {
			// fd is not this process's controlling terminal at all (a bare
			// pseudo-terminal opened directly, as the unit tests do): job
			// control cannot apply to it.
			return nil
		}
		if err != nil {
			return err
		}
		if foreground == unix.Getpgrp() {
			return nil
		}
		// fg need not send SIGCONT to a job that bg already left running.
		// A timed wake covers that handoff; signals still wake us immediately.
		if retry == nil {
			retry = time.NewTicker(50 * time.Millisecond)
		}
		select {
		case event := <-t.events:
			t.drainWake()
			if err := t.signal(event); err != nil {
				return err
			}
		case <-retry.C:
		}
	}
}

// drainWake reads the one wake-pipe byte the watcher wrote for an event this
// call already consumed directly from t.events, bypassing readTerminal's own
// drain. The watcher guarantees exactly one byte per event, written before
// posting it (a stop signal, so the reader never touches the terminal while
// it takes effect) or just after (every other signal), so this always has
// one to read, immediately or a moment later; left undrained, it would wake
// a later, unrelated readTerminal call into waiting for an event that already
// came and went.
func (t *promptTerminal) drainWake() {
	var token [1]byte
	_, _ = unix.Read(t.wake, token[:])
}

func (t *promptTerminal) resume() error {
	if err := t.waitForeground(); err != nil {
		return err
	}
	if err := t.apply(); err != nil {
		return err
	}
	if t.notice {
		t.notice = false
		if t.onStop != nil {
			t.onStop()
		}
	}
	return nil
}

func (t *promptTerminal) read(buf []byte) (int, error) {
	for {
		select {
		case event := <-t.events:
			t.drainWake()
			if err := t.signal(event); err != nil {
				return 0, err
			}
			if err := t.resume(); err != nil {
				return 0, err
			}
		default:
		}
		if t.resumed {
			if err := t.waitForeground(); err != nil {
				return 0, err
			}
		}
		n, err := readTerminal(t.fd, t.wake, buf, t.wait)
		if errors.Is(err, unix.EINTR) {
			if err := t.resume(); err != nil {
				return 0, err
			}
			continue
		}
		if n == -2 {
			// The watcher's wake pipe fired: either it already posted a
			// processed event, or it is about to stop the process and woke
			// us first so we never race a read against that stop. Either
			// way, wait for the event it posts rather than polling the
			// terminal again, since the terminal may become unsafe to touch
			// (backgrounded) before that event arrives.
			event := <-t.events
			if err := t.signal(event); err != nil {
				return 0, err
			}
			if err := t.resume(); err != nil {
				return 0, err
			}
			continue
		}
		if n < 0 && err == nil {
			continue
		}
		return n, err
	}
}

// promptReader is the state of one read at the prompt, fed each read's bytes in turn.
type promptReader struct {
	special func(c byte, index int) bool // whether c is the terminal's control character index
	ignored [3]byte                      // disabled tty characters of inherited ignored signals
	line    []byte                       // the value read so far
	pending []byte                       // bytes that may begin a paste mark split across reads
	inPaste bool                         // inside a bracketed paste
	ended   bool                         // the line has ended; only line endings may follow
	more    bool                         // something but line endings followed the line
	pasted  bool                         // the line was ended inside a bracketed paste
	err     error                        // a refusal whose remaining input must be drained
}

// feed takes one read's bytes and reports whether the read is over: the line ended outside a paste,
// or a paste in which it ended has closed.
func (r *promptReader) feed(b []byte) bool {
	b = append(r.pending, b...)
	r.pending = nil
	for i := 0; i < len(b); i++ {
		if b[i] == 0x1b {
			rest := b[i:]
			if !r.inPaste && bytes.HasPrefix(rest, pasteStart) {
				r.inPaste = true
				i += len(pasteStart) - 1
				continue
			}
			if r.inPaste && bytes.HasPrefix(rest, pasteEnd) {
				r.inPaste = false
				i += len(pasteEnd) - 1
				if r.ended {
					return true
				}
				continue
			}
			if bytes.HasPrefix(pasteStart, rest) || bytes.HasPrefix(pasteEnd, rest) {
				r.pending = append(r.pending, rest...)
				return false
			}
		}
		c := b[i]
		if r.ended {
			// The line ended inside a paste: outside one, feed returned as it ended.
			if c != '\r' && c != '\n' {
				r.more = true
			}
			continue
		}
		switch {
		case !r.inPaste && c != 0 && bytes.IndexByte(r.ignored[:], c) >= 0:
			// Preserve inherited SIG_IGN without the tty driver's input flush.
		case c == '\r' || c == '\n' || (!r.inPaste && r.special(c, unix.VEOF)):
			r.ended = true
			if r.inPaste {
				r.pasted = true
				continue
			}
			r.more = r.more || !onlyLineEndings(b[i+1:])
			return true
		case !r.inPaste && (r.special(c, unix.VERASE) || c == 0x7f || c == '\b'):
			if len(r.line) > 0 {
				_, size := utf8.DecodeLastRune(r.line)
				r.line = r.line[:len(r.line)-size]
			}
		case !r.inPaste && r.special(c, unix.VWERASE):
			r.line = eraseWord(r.line)
		case !r.inPaste && r.special(c, unix.VKILL):
			r.line = r.line[:0]
		case c < 0x20 || c == 0x7f:
			if r.err == nil {
				r.err = promptControlByteError(c)
				r.line = nil
			}
		default:
			if r.err == nil {
				r.line = append(r.line, c)
			}
		}
	}
	return false
}

// eraseWord removes trailing blanks, then the preceding word, by rune as canonical terminal
// VWERASE does. The separating blank stays for the word that follows.
func eraseWord(b []byte) []byte {
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if !unicode.IsSpace(r) {
			break
		}
		b = b[:len(b)-size]
	}
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if unicode.IsSpace(r) {
			break
		}
		b = b[:len(b)-size]
	}
	return b
}

// moreAfterTheLine reports whether anything but line endings follows the line on a terminal that
// does not bracket pastes: what it receives until it has been quiet for pasteGapDeciseconds (for at
// most maxPasteDrain), all of which it reads into buf and so discards.
func moreAfterTheLine(tty *promptTerminal, buf []byte) (bool, error) {
	tty.wait = pasteGapDeciseconds * 100
	more := false
	for deadline := time.Now().Add(maxPasteDrain); time.Now().Before(deadline); {
		n, err := tty.read(buf)
		if err != nil {
			return false, err
		}
		if n == 0 {
			break
		}
		more = more || !onlyLineEndings(buf[:n])
	}
	return more, nil
}

// onlyLineEndings reports whether b holds nothing but carriage returns and newlines: what a
// terminal sends for a pasted line ending in CR LF.
func onlyLineEndings(b []byte) bool {
	for _, c := range b {
		if c != '\r' && c != '\n' {
			return false
		}
	}
	return true
}

// readTerminal waits for input, a watcher event or the quiet window's end. A
// result of -1 means retry (input gone before read); -2 means the watcher's
// wake pipe fired and the caller must wait for its processed event on t.events
// before touching the terminal again, since a stop may be about to take effect.
// Tests wrap it to place signals and hang-ups at these boundaries.
var readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(wake), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, timeout)
	if err != nil || n == 0 {
		return 0, err
	}
	if fds[1].Revents != 0 {
		var token [1]byte
		_, err := unix.Read(wake, token[:])
		return -2, err
	}
	n, err = unix.Read(fd, buf)
	if errors.Is(err, unix.EAGAIN) || (n == 0 && err == nil && fds[0].Revents&unix.POLLHUP == 0) {
		return -1, nil
	}
	return n, err
}

type promptSignal struct {
	sig syscall.Signal
	err error
}

// watchPromptSignals never changes the terminal. The pipe wakes the reader when
// the channel carries a resume or death signal. Cancellation joins the watcher,
// so no late resume can race the reader's final restore.
func watchPromptSignals(wake *os.File) (<-chan promptSignal, func() promptSignal, []int, error) {
	// SIGCONT is a wake only; SIGTTIN and SIGTTOU retain their default actions.
	watched := []os.Signal{syscall.SIGCONT}
	var ignoredControls []int
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGTSTP} {
		ignored, err := promptSignalIgnored(sig)
		if err != nil {
			return nil, nil, nil, err
		}
		if !ignored {
			watched = append(watched, sig)
		} else {
			switch sig {
			case syscall.SIGINT:
				ignoredControls = append(ignoredControls, unix.VINTR)
			case syscall.SIGQUIT:
				ignoredControls = append(ignoredControls, unix.VQUIT)
			case syscall.SIGTSTP:
				ignoredControls = append(ignoredControls, unix.VSUSP)
			}
		}
	}
	signals := make(chan os.Signal, 8)
	events := make(chan promptSignal, 8)
	done, joined := make(chan struct{}), make(chan struct{})
	var pending promptSignal // written by the watcher, read only after joined
	signal.Notify(signals, watched...)
	go func() {
		defer close(joined)
		for {
			select {
			case sig := <-signals:
				event := promptSignal{sig: sig.(syscall.Signal)}
				earlyWoke := false
				if promptStopSignal(sig) {
					// Wake the reader out of any poll/read on the tty before the
					// real stop begins: a poll already in flight can report the
					// tty ready for a reason unrelated to real input (the signal's
					// own queue flush), and a read issued on that stale readiness
					// can be caught mid-syscall by the group-stop this is about to
					// cause, then re-raise SIGTTIN once bg leaves it backgrounded.
					// Waking it onto the pipe first makes it retry instead and
					// wait for this event, rather than touching the terminal.
					_, _ = wake.Write([]byte{1})
					earlyWoke = true
					event.err = stopBy(event.sig)
					// On Darwin stopBy must clear os/signal's handler bookkeeping
					// before Notify can reinstall it. On Linux it is still installed.
					signal.Notify(signals, sig)
				}
				select {
				case events <- event:
					if !earlyWoke {
						_, _ = wake.Write([]byte{1})
					}
				case <-done:
					if event.sig != syscall.SIGCONT {
						pending = event
					}
					return
				}
			case <-done:
				return
			}
		}
	}()
	return events, func() promptSignal {
		close(done)
		<-joined
		signal.Stop(signals)
		// A death arriving at the read's end still takes effect after restoration,
		// including one Notify queued but the watcher did not receive.
		for len(events) != 0 {
			event := <-events
			if event.sig == syscall.SIGCONT {
				continue
			}
			if event.err != nil {
				pending.err = event.err
			}
			if pending.sig == 0 || !promptStopSignal(event.sig) {
				pending.sig = event.sig
			}
		}
		for len(signals) != 0 {
			sig := (<-signals).(syscall.Signal)
			if sig == syscall.SIGCONT {
				continue
			}
			if pending.sig == 0 || !promptStopSignal(sig) {
				pending.sig = sig
			}
		}
		return pending
	}, ignoredControls, nil
}

func promptStopSignal(sig os.Signal) bool {
	return sig == syscall.SIGTSTP
}

// endByPromptSignal runs on the reader, after the watcher has stopped and the
// terminal has been restored. The fallback exit is only for a failed re-raise.
func endByPromptSignal(sig syscall.Signal) {
	signal.Reset(sig)
	if sig == syscall.SIGQUIT {
		if err := quitWithoutCore(); err != nil {
			fmt.Fprintf(os.Stderr, "agent-secrets: %v\n", err)
			os.Exit(1)
		}
	}
	_ = syscall.Kill(os.Getpid(), sig)
	time.Sleep(time.Second)
	switch sig {
	case syscall.SIGHUP:
		os.Exit(exitHangup)
	case syscall.SIGTERM:
		os.Exit(exitTerminated)
	case syscall.SIGQUIT:
		os.Exit(exitQuit)
	default:
		os.Exit(exitInterrupted)
	}
}
