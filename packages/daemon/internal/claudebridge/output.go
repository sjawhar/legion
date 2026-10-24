package claudebridge

import (
	"io"
	"log"
)

const (
	// framesHighWater is how many frames may wait for the shim before the loop stops taking
	// Claude Code's output: past it Claude Code waits on its own stdout, as it did when the bridge
	// wrote each frame before reading on, until the shim reads again. The shim stops reading while
	// its write to the daemon waits (shim's outbox), which the daemon's own pace decides, so the
	// bridge waits with it rather than end a session the daemon will catch up with. The frames that
	// wait only smooth a shim that pauses: 64 holds a burst, an assistant message's tool calls and
	// a turn's edges, and holding more would cost memory, up to shimwire.MaxFrameBytes a frame,
	// for nothing. The frames the daemon's own requests and Envoy's deliveries add are queued past
	// it: their pace is the daemon's and the inbox's, and a reply is never refused.
	framesHighWater = 64
	// claudeQueueLength is how many lines may wait for Claude Code before the bridge counts it as
	// having stopped reading and ends it. A Claude Code that reads its stdin has at most two of the
	// bridge's lines waiting: a user message, which the bridge writes only to start a turn or, in
	// one, once the last step's result is in, and at most one interrupt in that step; and one
	// refusal for each control request Claude Code has made and not had its answer to, a request
	// it waits on. While the shim holds Claude Code's output back, none of it is read, so nothing
	// that answers it (a next step, a refusal) is written either. 1000 waiting lines is a Claude
	// Code that has stopped reading.
	claudeQueueLength = 1000
)

// output is one of the bridge's two pipes out: Claude Code's stdin, and its own stdout to the
// shim. The loop owns its queue and hands its writer one line at a time, the next once the writer
// reports the last one's write ended (wrote), so the loop never waits on the pipe's reader. Only
// the loop calls the methods.
type output struct {
	reader  string // who reads the pipe, for the log: "Claude Code" or "the shim"
	pending []line
	writing bool
	next    chan line     // holds one line, so handing a line to an idle writer never waits
	wrote   chan struct{} // the writer's report that a line's write ended
	log     *log.Logger
	// limit, when not zero, is how many lines may wait (claudeQueueLength): a put that finds that
	// many is logged once and calls full, and nothing more is queued, so no line is written out of
	// order and none is dropped unsaid.
	limit   int
	full    func()
	stalled bool
	// closed is no more lines: after runs once the last is written.
	closed bool
	after  func()
}

// line is one line to write, its newline included; frame names it, when it is a frame to the shim,
// for the log line should its write fail.
type line struct {
	data  []byte
	frame string
}

// newOutput starts the writer of w, which ends once stop is closed, or, should a write of its own
// never end, with the process.
func newOutput(reader string, w io.Writer, stop <-chan struct{}, logger *log.Logger) *output {
	o := &output{reader: reader, next: make(chan line, 1), wrote: make(chan struct{}), log: logger}
	go func() {
		for {
			var l line
			select {
			case l = <-o.next:
			case <-stop:
				return
			}
			if _, err := w.Write(l.data); err != nil {
				if l.frame != "" {
					logger.Printf("could not write a %s frame to %s: %v", l.frame, reader, err)
				} else {
					logger.Printf("could not write to %s: %v", reader, err)
				}
			}
			select {
			case o.wrote <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()
	return o
}

func (o *output) put(data []byte, frame string) {
	switch {
	case o.stalled:
		return
	case o.closed:
		o.log.Printf("could not write to %s: its input is closed", o.reader)
		return
	case o.limit > 0 && o.waiting() >= o.limit:
		o.stalled = true
		o.log.Printf("%s has left %d lines unread; ending Claude Code, and this line and every later one to %s are not written",
			o.reader, o.limit, o.reader)
		o.full()
		return
	}
	o.pending = append(o.pending, line{data: data, frame: frame})
	o.hand()
}

// hand gives an idle writer the next line, or, with none left after close, runs after.
func (o *output) hand() {
	switch {
	case o.writing:
	case len(o.pending) > 0:
		o.next <- o.pending[0]
		o.pending[0] = line{}
		o.pending = o.pending[1:]
		o.writing = true
	case o.closed && o.after != nil:
		o.after()
		o.after = nil
	}
}

// written is the writer's report that its line's write ended.
func (o *output) written() {
	o.writing = false
	o.hand()
}

// waiting is how many lines the reader has yet to take, the one being written included.
func (o *output) waiting() int {
	if o.writing {
		return len(o.pending) + 1
	}
	return len(o.pending)
}

// close queues nothing more; after runs once what is queued is written.
func (o *output) close(after func()) {
	o.closed, o.after = true, after
	o.hand()
}
