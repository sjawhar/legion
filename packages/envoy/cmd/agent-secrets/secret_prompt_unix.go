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
	"sync"
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
	controlling, err := controllingTerminal(fd)
	if err != nil {
		return nil, err
	}
	watch, ignoredControls, err := watchPromptSignals()
	if err != nil {
		return nil, err
	}
	tty := promptTerminal{fd: fd, controlling: controlling, watch: watch, wait: -1, onStop: onStop}
	var saved *unix.Termios // nil until the reader holds the terminal: nothing to restore before
	defer func() {
		pending := watch.stop()
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
		// A signal that ends the prompt while another process group holds the terminal leaves
		// the terminal as that group has it: a restore from the background would stop the job
		// until fg. When the check itself fails (EIO on a hung-up terminal), the restore runs,
		// and answers that failure as having nothing to restore to.
		restore := saved != nil
		if restore && tty.death != 0 {
			held, err := tty.holdsTerminal()
			restore = held || err != nil
		}
		if restore {
			_, _ = unix.Write(fd, bracketedPasteOff)
			if restoreErr := tty.restoreTerminal(saved); restoreErr != nil && err == nil {
				line, err = nil, restoreErr
			}
		}
		if tty.death != 0 {
			endByPromptSignal(tty.death)
		}
	}()
	// The settings the reader restores are read only once its group holds the terminal: a
	// prompt started in the background waits behind the shell's line editor, whose settings
	// are not the ones fg hands back.
	if err := tty.whenHeld(func() (err error) {
		saved, err = unix.IoctlGetTermios(fd, ioctlGetTermios)
		return err
	}); err != nil {
		return nil, err
	}
	mode := *saved
	// Keep the kernel's signal flush: NOFLSH would leave unread secret bytes for
	// bash when a stop gives it the foreground. Every caught stop invalidates the entry.
	mode.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.NOFLSH
	mode.Lflag |= unix.ISIG
	// Ctrl-S and Ctrl-Q reach the reader as control bytes, refused like any other: with IXON on,
	// the terminal would take them, so a pasted one would vanish from the value, and a lone
	// Ctrl-S would leave the prompt waiting with its output suspended.
	mode.Iflag &^= unix.IXON
	var ignoredKeys [3]byte
	for i, cc := range ignoredControls {
		if key := saved.Cc[cc]; key != 0 && key != 0xff {
			ignoredKeys[i] = key
			// Even an ignored tty signal flushes input. Disable its tty character
			// and let the reader consume it without flushing the value.
			mode.Cc[cc] = disabledControlByte
		}
	}
	// Poll owns both waits. Reads themselves never block, including after resume.
	mode.Cc[unix.VMIN], mode.Cc[unix.VTIME] = 0, 0
	tty.current = &mode
	// The label shows once the reader holds the terminal in its own mode. A stop before it
	// discards nothing: nothing has been typed for the entry yet.
	if err := tty.whenHeld(func() error {
		if prompt != nil {
			prompt()
		}
		return nil
	}); err != nil {
		return nil, err
	}
	tty.stopped, tty.notice = false, false
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
	fd          int
	controlling bool          // whether fd was this process's controlling terminal when the prompt began
	current     *unix.Termios // the reader's own mode; nil until it has read the settings it restores
	watch       *promptWatch
	death       syscall.Signal
	wait        int // poll timeout in milliseconds; -1 until the line ends
	stopped     bool
	onStop      func()
	// mayBeBackground is set at the first stop or resume and never cleared: the reader may
	// since have been put in the background, so it holds the terminal again before each poll.
	mayBeBackground bool
	notice          bool
	applied         bool // whether t.current and bracketed paste are already in effect
}

// apply puts the reader's mode and bracketed paste in effect. One resume can reach here more than
// once (the watcher's event, and an EINTR on whichever other blocked syscall the same resume
// woke), so once applied it rereads the mode, a read that never stops the process, and reapplies
// only when the mode differs, rather than repeat a paste-on write that shows over a real terminal.
// A caught stop clears applied (signal); a stop no handler sees (SIGSTOP from another process)
// lets the shell put its own mode back, echo on, while the job is stopped, and only the reread
// shows that.
func (t *promptTerminal) apply() error {
	if t.applied {
		now, err := unix.IoctlGetTermios(t.fd, ioctlGetTermios)
		if err != nil {
			return err
		}
		if sameMode(now, t.current) {
			return nil
		}
	}
	for {
		err := unix.IoctlSetTermios(t.fd, ioctlSetTermios, t.current)
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

// sameMode reports whether a and b set the same input, output and local modes and control
// characters. The control modes and speeds are left out: they are the line's hardware settings,
// which neither the prompt nor a shell changes, and which a driver may adjust as it applies them.
func sameMode(a, b *unix.Termios) bool {
	return a.Iflag == b.Iflag && a.Oflag == b.Oflag && a.Lflag == b.Lflag && a.Cc == b.Cc
}

// restoreTerminal puts the terminal back to saved, once the signal watcher has joined. A
// tcsetattr from a background process group blocks, by the kernel's own job control, until the
// job is foregrounded, then completes; ENOTTY (fd is not this process's controlling terminal, as
// the unit tests' bare pseudo-terminal) or EIO (the group is orphaned, which the kernel discards
// rather than blocks) mean there is nothing to restore to.
func (t *promptTerminal) restoreTerminal(saved *unix.Termios) error {
	for {
		err := unix.IoctlSetTermios(t.fd, ioctlSetTermiosFlush, saved)
		if errors.Is(err, unix.EINTR) {
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
	t.mayBeBackground = true
	return nil
}

// whenHeld runs f once this process's group holds the terminal, in the reader's own mode once it
// has one, with every event the watcher has reported taken and no stop begun since (whileHeld). A
// prompt started in the background, or sent there by a stop and bg, waits here for fg.
func (t *promptTerminal) whenHeld(f func() error) error {
	for {
		if err := t.settle(); err != nil {
			return err
		}
		if t.current != nil {
			if err := t.apply(); err != nil {
				return err
			}
		}
		if held, err := t.whileHeld(f); err != nil || held {
			return err
		}
	}
}

// settle waits until this process's group holds the terminal with every event the watcher has
// reported taken.
func (t *promptTerminal) settle() error {
	for {
		if err := t.waitForeground(); err != nil {
			return err
		}
		if took, err := t.nextEvent(0); err != nil || !took {
			return err
		}
	}
}

// whileHeld runs f only while this process's group holds the terminal and no stop has begun since
// the reader last took an event, and reports whether it ran. The watcher writes a stop's event
// before the stop and holds t.watch.stopping until the stop is over, so f never runs in a prompt
// such a stop has sent to the background: either the event is already there, or the stop waits
// for f. A stop no handler sees (SIGSTOP) is not held off; the foreground check narrows it to the
// moment between that check and f.
func (t *promptTerminal) whileHeld(f func() error) (bool, error) {
	t.watch.stopping.Lock()
	defer t.watch.stopping.Unlock()
	if pending, err := t.watch.ready(0); err != nil || pending {
		return false, err
	}
	if held, err := t.holdsTerminal(); err != nil || !held {
		return false, err
	}
	return true, f()
}

// holdsTerminal reports whether this process's group holds the terminal, by a read-only
// ioctl that never stops it.
func (t *promptTerminal) holdsTerminal() (bool, error) {
	if sid, err := unix.Getsid(0); err == nil && sid == unix.Getpid() {
		// A session leader is never stopped by this terminal: it either just
		// acquired it (already foreground, by the kernel's own TIOCSCTTY rule)
		// or its process group is orphaned, where the kernel skips job control
		// entirely (tty_check_change's own orphan check) rather than stopping
		// it. Either way nothing will ever change what TIOCGPGRP answers here.
		return true, nil
	}
	for {
		foreground, err := unix.IoctlGetInt(t.fd, unix.TIOCGPGRP)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ENOTTY) {
			if !t.controlling {
				// fd was never this process's controlling terminal (a bare pseudo-terminal
				// opened directly, as the unit tests do): job control cannot apply to it.
				return true, nil
			}
			// The terminal was this process's controlling terminal and is no longer: its
			// session lost it when the leader, the shell, exited. Only a session leader can
			// take a controlling terminal, so no shell can hand this one back.
			return false, errNoForeground
		}
		if err != nil {
			return false, err
		}
		return foreground == unix.Getpgrp(), nil
	}
}

// controllingTerminal reports whether fd is this process's controlling terminal, by a read-only
// ioctl that answers ENOTTY for any other terminal (Linux's tiocgpgrp, XNU's isctty check).
func controllingTerminal(fd int) (bool, error) {
	for {
		_, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ENOTTY) {
			return false, nil
		}
		return err == nil, err
	}
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
	for {
		held, err := t.holdsTerminal()
		if err != nil {
			return err
		}
		if held {
			return nil
		}
		// No shell can hand an orphaned group the terminal (its starting shell has
		// gone), and the kernel discards the stops it would take, so waiting would
		// never end.
		if orphaned, err := processGroupOrphaned(); err != nil {
			return err
		} else if orphaned {
			return errNoForeground
		}
		// fg need not send SIGCONT to a job that bg already left running, so the
		// wait rechecks every 50 ms; an event ends it at once.
		if _, err := t.nextEvent(50); err != nil {
			return err
		}
	}
}

// nextEvent takes the watcher's next event, if one comes within timeout milliseconds, and records
// it (signal). Every event the reader acts on comes through here.
func (t *promptTerminal) nextEvent(timeout int) (bool, error) {
	event, took, err := t.watch.next(timeout)
	if err != nil || !took {
		return false, err
	}
	return true, t.signal(event)
}

// noticeStop tells the person, once the reader holds the terminal again, that a stop discarded
// the entry.
func (t *promptTerminal) noticeStop() error {
	if t.notice {
		t.notice = false
		if t.onStop != nil {
			t.onStop()
		}
	}
	return nil
}

func (t *promptTerminal) read(buf []byte) (int, error) {
	event := false
	for {
		// An event, perhaps a stop about to take effect, is taken before the terminal is
		// touched again, and once a stop or resume has come the reader may have been put
		// in the background since: either way it goes on only once it holds the terminal
		// in its own mode.
		if event || t.mayBeBackground {
			if err := t.whenHeld(t.noticeStop); err != nil {
				return 0, err
			}
		}
		n, err := readTerminal(t.fd, t.watch.fd, buf, t.wait)
		event = n == -2
		if event || errors.Is(err, unix.EINTR) || (n == -1 && err == nil) {
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

// readTerminal waits for input, a watcher event or the quiet window's end. A result of -1 means
// retry (input gone before read); -2 means the watcher has an event the reader must take before it
// touches the terminal again, since a stop may be about to take effect. Tests wrap it to place
// signals and hang-ups at these boundaries.
var readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(wake), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, timeout)
	if err != nil || n == 0 {
		return 0, err
	}
	if fds[1].Revents != 0 {
		return -2, nil
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

// promptWatch is the prompt's signal watcher. It never changes the terminal: for each signal it
// writes one byte, the signal's number, to the wake pipe, so an event and the wake-up that
// announces it are one thing, taken only by next. A stop's byte is written before the stop, under
// stopping, which the watcher holds until the process resumes: the reader can keep a stop off its
// baseline read and label (whileHeld), and a reader that takes the byte waits the stop out.
type promptWatch struct {
	fd           int // the wake pipe's read end, polled with the terminal
	wake, notify *os.File
	signals      chan os.Signal
	done, joined chan struct{}
	stopping     sync.Mutex
	stopErr      error // why a stop failed, written under stopping
}

// watchPromptSignals starts the watcher. SIGCONT is a wake only; SIGTTIN and SIGTTOU retain their
// default actions, and a signal whose kernel disposition is SIG_IGN stays ignored, its terminal
// control character answered in ignoredControls.
func watchPromptSignals() (*promptWatch, []int, error) {
	watched := []os.Signal{syscall.SIGCONT}
	var ignoredControls []int
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGTSTP} {
		ignored, err := promptSignalIgnored(sig)
		if err != nil {
			return nil, nil, err
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
	wake, notify, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	w := &promptWatch{fd: int(wake.Fd()), wake: wake, notify: notify, signals: make(chan os.Signal, 8),
		done: make(chan struct{}), joined: make(chan struct{})}
	signal.Notify(w.signals, watched...)
	go w.run()
	return w, ignoredControls, nil
}

func (w *promptWatch) run() {
	defer close(w.joined)
	for {
		select {
		case s := <-w.signals:
			sig := s.(syscall.Signal)
			if !promptStopSignal(sig) {
				_, _ = w.notify.Write([]byte{byte(sig)})
				continue
			}
			w.stopping.Lock()
			// The byte goes first, so the reader never touches the terminal while the stop
			// takes effect: a poll in flight can report the terminal ready for the stop's own
			// input flush, and a read on that readiness can be caught mid-syscall by the
			// stop, then raise SIGTTIN once bg leaves the job in the background.
			_, _ = w.notify.Write([]byte{byte(sig)})
			if err := stopBy(sig); err != nil && w.stopErr == nil {
				w.stopErr = err
			}
			// On Darwin stopBy must clear os/signal's handler bookkeeping before Notify can
			// reinstall it. On Linux it is still installed.
			signal.Notify(w.signals, sig)
			w.stopping.Unlock()
		case <-w.done:
			return
		}
	}
}

// ready reports whether the watcher has an event the reader has not taken, waiting up to timeout
// milliseconds for one.
func (w *promptWatch) ready(timeout int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return n > 0, err
	}
}

// next takes the watcher's next event, if one comes within timeout milliseconds. A stop's event is
// answered only once the stop is over, with its failure if it failed.
func (w *promptWatch) next(timeout int) (promptSignal, bool, error) {
	if ready, err := w.ready(timeout); err != nil || !ready {
		return promptSignal{}, false, err
	}
	var b [1]byte
	if _, err := unix.Read(w.fd, b[:]); err != nil {
		return promptSignal{}, false, err
	}
	event := promptSignal{sig: syscall.Signal(b[0])}
	if promptStopSignal(event.sig) {
		w.stopping.Lock()
		event.err, w.stopErr = w.stopErr, nil
		w.stopping.Unlock()
	}
	return event, true, nil
}

// stop joins the watcher, so no late resume can race the reader's final restore, and answers
// what the reader has not taken: a death arriving at the read's end, which still takes effect
// after restoration, including one Notify queued but the watcher did not receive; else a stop.
func (w *promptWatch) stop() promptSignal {
	close(w.done)
	<-w.joined
	signal.Stop(w.signals)
	defer w.wake.Close()
	defer w.notify.Close()
	var pending promptSignal
	note := func(sig syscall.Signal) {
		if sig != syscall.SIGCONT && (pending.sig == 0 || !promptStopSignal(sig)) {
			pending.sig = sig
		}
	}
	for {
		event, took, err := w.next(0)
		if err != nil || !took {
			break
		}
		if event.err != nil {
			pending.err = event.err
		}
		note(event.sig)
	}
	for len(w.signals) != 0 {
		note((<-w.signals).(syscall.Signal))
	}
	return pending
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
