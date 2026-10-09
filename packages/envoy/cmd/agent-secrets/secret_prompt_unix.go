// packages/envoy/cmd/agent-secrets/secret_prompt_unix.go
//go:build (linux || darwin) && (amd64 || arm64)

// The value prompt's reader on Linux and macOS. The reader alone changes the terminal's settings;
// the signal watcher reports each signal, and before a stop discards what the terminal holds unread.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
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
// arriving after the line, and how long it waits for an open bracketed paste's closing mark. Tests
// shorten it.
var maxPasteDrain = 10 * time.Second

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
// their closing mark; what follows the line is drained through a 200 ms quiet window,
// and a paste that begins there through its closing mark.
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
	tty := promptTerminal{fd: fd, controlling: controlling, wait: -1, onStop: onStop}
	watch, ignored, err := watchPromptSignals(tty.flushInput)
	if err != nil {
		return nil, err
	}
	tty.watch = watch
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
		// The prompt can end while another process group holds the terminal: a signal after
		// Ctrl-Z and bg, or an orphaned group after bg and its shell's exit. The terminal is then
		// as that group has it, and a restore from the background would stop the job until fg, or
		// write the paste-off mark onto that group's line. When the check itself fails (EIO on a
		// hung-up terminal), the restore runs, and answers that failure as having nothing to
		// restore to.
		restore := saved != nil
		if restore {
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
	// The reader acts on the terminal's signal keys itself (promptReader.keys), so ISIG is off: the
	// kernel's own handling discards only what is queued before a key, leaves what follows it in
	// the same write for the shell, and acts on a key inside a paste, where it is pasted text.
	mode.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG
	// IEXTEN is off too: macOS acts on Ctrl-V (VLNEXT) and Ctrl-O (VDISCARD) under it whatever
	// ICANON and ISIG say (bsd/kern/tty.c, ttyinput), so a pasted Ctrl-V would vanish from the
	// value and Ctrl-O would toggle output discard. Linux reads it only in canonical mode, and the
	// reader does its own word erase.
	mode.Lflag &^= unix.IEXTEN
	// Ctrl-S and Ctrl-Q reach the reader as control bytes, refused like any other: with IXON on,
	// the terminal would take them, so a pasted one would vanish from the value, and a lone
	// Ctrl-S would leave the prompt waiting with its output suspended.
	mode.Iflag &^= unix.IXON
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
	r := newPromptReader(saved, controlling, ignored)
	buf := make([]byte, 512)
	var pasteSince time.Time // when the open paste began; zero outside one
	for {
		tty.wait = -1
		if r.inPaste {
			if pasteSince.IsZero() {
				pasteSince = time.Now()
			}
			tty.wait = int((time.Until(pasteSince.Add(maxPasteDrain)) + time.Millisecond - 1).Milliseconds())
			tty.wait = max(0, tty.wait)
		} else {
			pasteSince = time.Time{}
		}
		n, err := tty.read(buf)
		if err != nil {
			return nil, err
		}
		if tty.stopped {
			// The stop discarded unread bytes or paste marks, with no count of what
			// was lost. Drain a fresh line instead of keeping a partial value.
			r = promptReader{special: r.special, keys: r.keys, err: errPromptStopped}
			tty.stopped = false
		}
		if n == 0 {
			if r.inPaste && time.Since(pasteSince) >= maxPasteDrain {
				// The paste's closing mark never came, so the signal keys pressed since were
				// pasted text: give the paste up rather than read it forever.
				return nil, errPasteCutShort
			}
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
		done, sig := r.feed(buf[:n])
		if sig != 0 {
			if err := tty.raise(sig); err != nil {
				return nil, err
			}
			continue
		}
		if done {
			break
		}
	}
	if !r.pasted || len(r.pending) > 0 || r.err != nil {
		if err := drainAfterTheLine(&tty, &r, buf); err != nil {
			return nil, err
		}
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
// ioctl that answers ENOTTY for any other terminal (Linux's tiocgpgrp, XNU's isctty check). A
// terminal that is not, in a session whose leader has exited, may have been the controlling
// terminal until that exit, which took it from the session: that is errNoForeground, since no
// shell can hand it back, and ENOTTY alone cannot tell it from a terminal that never was.
func controllingTerminal(fd int) (bool, error) {
	for {
		_, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ENOTTY) {
			if gone, err := sessionLeaderGone(); err != nil || gone {
				if err == nil {
					err = errNoForeground
				}
				return false, err
			}
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

// raise sends sig to this process's group, as the kernel does for a signal key under ISIG, and
// takes the watcher's event for it before the reader touches the terminal again: a stop is over,
// or a terminating signal answers its error, when raise returns.
func (t *promptTerminal) raise(sig syscall.Signal) error {
	if err := unix.Kill(0, sig); err != nil {
		return fmt.Errorf("send %s: %w", sig, err)
	}
	for {
		event, took, err := t.watch.next(-1)
		if err != nil {
			return err
		}
		if !took {
			continue
		}
		if err := t.signal(event); err != nil || event.sig == sig {
			return err
		}
	}
}

// flushInput discards what the terminal holds unread while this process's group holds it, as the
// kernel's own handling of a signal key would: what arrived with or before Ctrl-Z would otherwise
// reach the shell once the stop gives it the terminal. Keys typed after the stop has taken effect
// go to the shell. From the background a flush would stop the job (tty_check_change), so it is
// skipped there.
func (t *promptTerminal) flushInput() {
	if held, err := t.holdsTerminal(); err == nil && held {
		_ = discardInput(t.fd)
	}
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
	keys    [3]promptKey                 // the terminal's signal keys (signalKeys)
	line    []byte                       // the value read so far
	pending []byte                       // bytes that may begin a paste mark split across reads
	inPaste bool                         // inside a bracketed paste
	ended   bool                         // the line has ended; only line endings may follow
	more    bool                         // something but line endings followed the line
	pasted  bool                         // the line was ended inside a bracketed paste
	err     error                        // a refusal whose remaining input must be drained
}

// promptKey is one of the terminal's signal keys (VINTR, VQUIT, VSUSP) as the reader handles it
// outside a paste: c sends sig to the process group, as the kernel would under ISIG, or does
// nothing when sig is 0, as the inherited SIG_IGN would have it. c is 0 for a key not set.
type promptKey struct {
	c   byte
	sig syscall.Signal
}

// newPromptReader is a read at the prompt under saved, the settings the shell handed it: its
// editing characters, and its signal keys (signalKeys).
func newPromptReader(saved *unix.Termios, controlling bool, ignored []int) promptReader {
	return promptReader{keys: signalKeys(saved, controlling, ignored), special: func(c byte, index int) bool {
		v := saved.Cc[index]
		return v != 0 && v != 0xff && c == v
	}}
}

// signalKeys answers the terminal's signal keys from saved, the settings the shell handed the
// prompt. On a terminal that is not this process's controlling terminal the kernel would send
// their signals to that terminal's foreground group rather than this one, so there they are
// control bytes like any other.
func signalKeys(saved *unix.Termios, controlling bool, ignored []int) [3]promptKey {
	var keys [3]promptKey
	if !controlling {
		return keys
	}
	for i, k := range [3]struct {
		cc  int
		sig syscall.Signal
	}{{unix.VINTR, syscall.SIGINT}, {unix.VQUIT, syscall.SIGQUIT}, {unix.VSUSP, syscall.SIGTSTP}} {
		c := saved.Cc[k.cc]
		if c == 0 || c == 0xff {
			continue // not set: _POSIX_VDISABLE is 0 on Linux and 0xff on Darwin
		}
		keys[i] = promptKey{c: c, sig: k.sig}
		if slices.Contains(ignored, k.cc) {
			keys[i].sig = 0
		}
	}
	return keys
}

// key answers the signal key c is, if it is one.
func (r *promptReader) key(c byte) (promptKey, bool) {
	for _, k := range r.keys {
		if k.c != 0 && k.c == c {
			return k, true
		}
	}
	return promptKey{}, false
}

// feed scans a whole read's bytes and reports whether the read is over: the line has ended and no
// bracketed paste is open (r.ended && !r.inPaste). The line's end does not stop the scan, so a
// paste, a paste mark split across reads, or more text that begins in the same read as the line's
// end is handled as it would be in a read of its own: the paste opens and is read on through its
// close, and anything but line endings after the line is more than one line (r.more). A signal key
// outside a paste answers its signal and ends the scan there, the bytes after it discarded. Inside
// a paste it is pasted text, a control byte refused like any other.
func (r *promptReader) feed(b []byte) (bool, syscall.Signal) {
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
				continue
			}
			if bytes.HasPrefix(pasteStart, rest) || bytes.HasPrefix(pasteEnd, rest) {
				r.pending = append(r.pending, rest...)
				return r.ended && !r.inPaste, 0
			}
		}
		c := b[i]
		if k, ok := r.key(c); ok && !r.inPaste {
			if k.sig != 0 {
				return r.ended, k.sig
			}
			continue // an inherited ignored signal: its key does nothing
		}
		if r.ended {
			// The line has ended; anything but line endings after it, typed or pasted, is
			// more than one line.
			if c != '\r' && c != '\n' {
				r.more = true
			}
			continue
		}
		switch {
		case c == '\r' || c == '\n' || (!r.inPaste && r.special(c, unix.VEOF)):
			r.ended = true
			if r.inPaste {
				r.pasted = true
			}
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
	return r.ended && !r.inPaste, 0
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

// drainAfterTheLine reads, and so discards, what follows the line, feeding each read to r, until
// the terminal has been quiet for pasteGapDeciseconds outside a paste, for at most maxPasteDrain. A
// paste that begins there is read through its closing mark however far apart its writes arrive,
// within that bound, so a signal key inside it is pasted text; a signal key outside a paste acts as
// it does in the line, and the rest of that read is discarded. Anything but line endings, and a
// paste or paste mark still open when the drain ends, is more than one line (r.more).
func drainAfterTheLine(tty *promptTerminal, r *promptReader, buf []byte) error {
	deadline := time.Now().Add(maxPasteDrain)
	for {
		left := time.Until(deadline).Milliseconds()
		if left <= 0 {
			break
		}
		tty.wait = int(left)
		if !r.inPaste {
			tty.wait = min(tty.wait, pasteGapDeciseconds*100)
		}
		n, err := tty.read(buf)
		if err != nil {
			return err
		}
		if tty.stopped {
			// The stop discarded what the terminal held unread, a paste's closing mark perhaps,
			// so the drain waits for none, and the entry is refused.
			r.inPaste, r.pending, tty.stopped = false, nil, false
			if r.err == nil {
				r.err = errPromptStopped
			}
		}
		if n == 0 {
			break
		}
		if _, sig := r.feed(buf[:n]); sig != 0 {
			if err := tty.raise(sig); err != nil {
				return err
			}
		}
	}
	r.more = r.more || r.inPaste || len(r.pending) > 0
	return nil
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

// promptWatch is the prompt's signal watcher. It never changes the terminal's settings: for each
// signal it writes one byte, the signal's number, to the wake pipe, so an event and the wake-up that
// announces it are one thing, taken only by next. A stop's byte is written before the stop, under
// stopping, which the watcher holds until the process resumes: the reader can keep a stop off its
// baseline read and label (whileHeld), and a reader that takes the byte waits the stop out.
type promptWatch struct {
	fd           int // the wake pipe's read end, polled with the terminal
	wake, notify *os.File
	signals      chan os.Signal
	done, joined chan struct{}
	flush        func() // discards the terminal's unread input before a stop (flushInput)
	stopping     sync.Mutex
	stopErr      error // why a stop failed, written under stopping
}

// watchPromptSignals starts the watcher. SIGCONT is a wake only; SIGTTIN and SIGTTOU retain their
// default actions, and a signal whose kernel disposition is SIG_IGN stays ignored, the terminal
// control character of its key answered in ignored.
func watchPromptSignals(flush func()) (*promptWatch, []int, error) {
	watched := []os.Signal{syscall.SIGCONT}
	var ignored []int
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGTSTP} {
		isIgnored, err := promptSignalIgnored(sig)
		if err != nil {
			return nil, nil, err
		}
		if !isIgnored {
			watched = append(watched, sig)
		} else {
			switch sig {
			case syscall.SIGINT:
				ignored = append(ignored, unix.VINTR)
			case syscall.SIGQUIT:
				ignored = append(ignored, unix.VQUIT)
			case syscall.SIGTSTP:
				ignored = append(ignored, unix.VSUSP)
			}
		}
	}
	wake, notify, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	w := &promptWatch{fd: int(wake.Fd()), wake: wake, notify: notify, signals: make(chan os.Signal, 8),
		done: make(chan struct{}), joined: make(chan struct{}), flush: flush}
	signal.Notify(w.signals, watched...)
	go w.run()
	return w, ignored, nil
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
			// takes effect: a poll in flight can report the terminal ready for input the flush
			// below discards, and a read on that readiness can be caught mid-syscall by the
			// stop, then raise SIGTTIN once bg leaves the job in the background.
			_, _ = w.notify.Write([]byte{byte(sig)})
			// What the terminal holds unread is the entry the stop discards; left there it
			// would reach the shell once the stop gives it the terminal.
			w.flush()
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
