package daemon

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// terminalJob is the terminal a probe attempt runs at as its foreground job: `legion controller
// start` run at an operator's shell. A launch prefix there may read the terminal (a PIN) or change
// its modes, which a background process group is stopped for (SIGTTIN, SIGTTOU). A nil
// *terminalJob is an attempt run as the daemon's are, in a background process group of its own
// with its stderr only captured, and each method is then a no-op.
type terminalJob struct {
	// fd is the terminal and owner the process group that held its foreground before the attempt.
	fd, owner int
	// echo also receives the attempt's stderr, where a prefix's prompt goes.
	echo io.Writer
	// ignored is whether SIGTTOU was already ignored when attach ran.
	ignored bool
}

// foregroundOf is the terminal stdin is, when it is one and this process's group is its foreground
// job; anything else - a file, a pipe, a terminal this process does not hold the foreground of - is
// nil.
func foregroundOf(stdin io.Reader, echo io.Writer) *terminalJob {
	file, ok := stdin.(*os.File)
	if !ok {
		return nil
	}
	fd := int(file.Fd())
	if !term.IsTerminal(fd) {
		return nil
	}
	owner, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil || owner != syscall.Getpgrp() {
		return nil
	}
	return &terminalJob{fd: fd, owner: owner, echo: echo}
}

// script is the attempt's shell script. At the terminal the attempt inherits SIGTSTP ignored: a
// Ctrl-Z would stop it as the terminal's foreground job while this process, a background job that
// waits only for its exit, stays where it is, leaving a terminal nothing answers until the budget
// kills the attempt. Only the attempt ignores it; this process's own disposition is untouched, so
// a Ctrl-Z of the controller it launches later still stops the command. A Ctrl-C still ends the
// attempt (interrupted).
func (j *terminalJob) script(script string) string {
	if j == nil {
		return script
	}
	return "trap '' TSTP; " + script
}

// attach makes cmd the terminal's foreground job, and copies its stderr to echo without the load
// probe's marker lines. Until release, this process ignores SIGTTOU: while the attempt holds the
// foreground this process is a background job, and its copying of the attempt's stderr to the
// terminal (under TOSTOP) and its taking the foreground back would otherwise stop it.
func (j *terminalJob) attach(cmd *exec.Cmd) {
	if j == nil {
		return
	}
	cmd.SysProcAttr.Foreground, cmd.SysProcAttr.Ctty = true, j.fd
	if j.echo != nil {
		cmd.Stderr = io.MultiWriter(cmd.Stderr, &markerFilter{w: j.echo})
	}
	j.ignored = signal.Ignored(syscall.SIGTTOU)
	if !j.ignored {
		signal.Ignore(syscall.SIGTTOU)
	}
}

// release takes the terminal's foreground back, then ends the SIGTTOU immunity. signal.Reset does
// not undo signal.Ignore for SIGTTOU: the runtime installed no handler to go back to, so SIG_IGN
// would stay, and every later child - the controller's Oh My Pi and all it starts - would inherit
// it and draw on the terminal from the background instead of stopping. So the signal moves to the
// runtime's own handler, which an exec resets to the default action. This process is left not
// stopped by SIGTTOU, and it writes nothing to the terminal from the background.
func (j *terminalJob) release() {
	if j == nil {
		return
	}
	_ = unix.IoctlSetPointerInt(j.fd, unix.TIOCSPGRP, j.owner)
	if !j.ignored {
		handled := make(chan os.Signal, 1)
		signal.Notify(handled, syscall.SIGTTOU)
		signal.Stop(handled)
	}
}

// interrupted is whether the terminal's Ctrl-C ended the attempt. At the terminal the SIGINT
// reaches the attempt and not this process, and an attempt it ended is the operator's answer, not
// a transient failure to retry: a retry would go on to mint, revoking the controller the operator
// stopped for. The attempt either dies of the signal (a prefix's `sleep`) or answers it and exits
// 128+SIGINT, as Oh My Pi does, which catches SIGINT and exits 130 — after the load marker, that
// exit would otherwise read as the plugin loaded and Oh My Pi dying under load.
func (j *terminalJob) interrupted(state *os.ProcessState) bool {
	if j == nil || state == nil {
		return false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	return ok && (status.Signaled() && status.Signal() == syscall.SIGINT ||
		status.Exited() && status.ExitStatus() == 128+int(syscall.SIGINT))
}

// marker begins each line of the load probe's own report on stderr (loadedMarker,
// notLoadedMarker, loadedFromMarker).
var marker = []byte("LEGION_PLUGIN_LOADED")

// markerFilter copies an attempt's stderr to the operator's terminal without the load probe's
// report: from wherever marker begins to the end of its line is dropped, which after a prompt with
// no newline ("Enter PIN: ") is mid-line. Every other byte is written as it arrives, so a prompt
// shows at once; only a tail that may still begin a marker is held back, until the next byte
// settles it (a prompt ends in ": " or "? ", which cannot). A failed write to the terminal is not
// the probe's failure: the captured copy is what the probe judges, so the filter always reports
// the whole write taken.
type markerFilter struct {
	w io.Writer
	// held is the tail of the stream that may still begin a marker, and dropping whether a marker
	// has begun and its line has not ended.
	held     []byte
	dropping bool
}

func (f *markerFilter) Write(p []byte) (int, error) {
	var out []byte
	for _, b := range p {
		if f.dropping {
			f.dropping = b != '\n'
			continue
		}
		f.held = append(f.held, b)
		settled := 0
		for !bytes.HasPrefix(marker, f.held[settled:]) {
			settled++
		}
		out = append(out, f.held[:settled]...)
		f.held = f.held[settled:]
		if len(f.held) == len(marker) {
			f.held, f.dropping = nil, true
		}
	}
	if len(out) > 0 {
		_, _ = f.w.Write(out)
	}
	return len(p), nil
}
