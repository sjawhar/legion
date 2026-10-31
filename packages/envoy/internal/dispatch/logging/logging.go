// Package logging is Dispatch's slog handler, which never makes a caller wait on its destination.
//
// slog.TextHandler formats a record in its caller and then writes the line under one mutex that
// every logger derived from it shares, so a destination that stops taking writes - a full stderr
// pipe, a slow log shipper - queues every goroutine that logs behind the one writing. The load
// reproduction caught 1,058 request goroutines queued on that mutex at ten times capacity
// (INFRA-592). Handler formats each record in its caller, in exactly slog.TextHandler's format,
// with no lock any other caller holds, and hands the line to one writer goroutine through a bounded
// channel. When the channel is full the line is dropped and counted under its level (Dropped), and
// the writer writes one `dropped N log records` line, at WARN, once the channel has room again.
// Flush waits for the lines already handed over, which a process calls before it exits, since
// os.Exit does not wait for the writer.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// bufferedRecords is how many formatted lines wait for the writer before the next one is dropped:
// about 1 MiB at a few hundred bytes a line, so a destination that stalls through a burst of
// requests, each writing its access-log line, loses nothing until the stall outlasts thousands of
// lines, and the backlog never holds more memory than that.
const bufferedRecords = 4096

// maxPooledLine is the largest line buffer kept for reuse once its line is written; a larger one,
// from an unusually long record, goes to the garbage collector rather than staying pinned.
const maxPooledLine = 64 << 10

// Levels are the levels Dropped counts under, lowest first: a record of a level between two of
// them counts under the lower (INFO+2 under INFO), one below DEBUG under DEBUG and one above ERROR
// under ERROR.
var Levels = [...]slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}

// Handler is a slog.Handler that formats records as slog.TextHandler does and writes them from
// one goroutine of its own, dropping a record rather than waiting when that goroutine is
// bufferedRecords lines behind. Every Handler derived from one New returns (WithAttrs, WithGroup)
// shares its writer, its counts and its Flush.
type Handler struct {
	writer *writer
	opts   slog.HandlerOptions
	// derive is every WithAttrs and WithGroup call that made this Handler, in order; each
	// formatter replays them on a text handler of its own, so its output is the text handler's
	// own output for the same calls.
	derive []func(slog.Handler) slog.Handler
	// formatters holds this Handler's idle formatters. A formatter serves one record at a time,
	// so no caller waits on another's formatting; slog.TextHandler's mutex is the formatter's
	// own and only ever taken by the caller holding it.
	formatters *sync.Pool
}

// New starts a Handler that writes to out, with opts as slog.NewTextHandler takes them (nil is the
// zero value: INFO and above, no source). Its writer goroutine runs for the rest of the process.
func New(out io.Writer, opts *slog.HandlerOptions) *Handler {
	h := &Handler{writer: newWriter(out, opts)}
	if opts != nil {
		h.opts = *opts
	}
	h.formatters = h.newFormatters()
	go h.writer.run()
	return h
}

// Enabled reports whether a record of level is written: at or above opts.Level, INFO when unset,
// as the writer's own slog.TextHandler, made with the same opts, decides.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.writer.report.Enabled(ctx, level)
}

// Handle formats r in the caller and hands the line to the writer, or drops it when the writer is
// bufferedRecords lines behind. It never waits on the destination.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	f := h.formatters.Get().(*formatter)
	f.level = r.Level
	err := f.text.Handle(ctx, r)
	h.formatters.Put(f)
	return err
}

// WithAttrs returns a Handler whose records carry attrs, as slog.TextHandler.WithAttrs does.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.derived(func(text slog.Handler) slog.Handler { return text.WithAttrs(attrs) })
}

// WithGroup returns a Handler that puts later attributes under name, as
// slog.TextHandler.WithGroup does.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.derived(func(text slog.Handler) slog.Handler { return text.WithGroup(name) })
}

// Dropped is how many records under level (Levels says which level a record counts under) this
// Handler and every Handler derived from it have dropped since New.
func (h *Handler) Dropped(level slog.Level) uint64 {
	return h.writer.dropped[levelIndex(level)].Load()
}

// Flush waits up to timeout for every line handed over before it was called to be written, with
// the `dropped N log records` line when any was dropped, and reports whether they were. It gives
// up at timeout when the destination does not take them, so an exit never hangs on its log.
func (h *Handler) Flush(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	done := make(chan struct{})
	select {
	case h.writer.flushes <- done:
	case <-timer.C:
		return false
	}
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (h *Handler) derived(step func(slog.Handler) slog.Handler) *Handler {
	d := &Handler{writer: h.writer, opts: h.opts, derive: append(slices.Clip(h.derive), step)}
	d.formatters = d.newFormatters()
	return d
}

func (h *Handler) newFormatters() *sync.Pool {
	return &sync.Pool{New: func() any {
		f := &formatter{writer: h.writer}
		var text slog.Handler = slog.NewTextHandler(f, &h.opts)
		for _, step := range h.derive {
			text = step(text)
		}
		f.text = text
		return f
	}}
}

// formatter is one caller's text handler, writing into the writer's channel rather than the
// destination. Its text handler calls Write once per record, with the whole line.
type formatter struct {
	text   slog.Handler
	writer *writer
	// level is the level of the record text is formatting, which a drop is counted under.
	level slog.Level
}

func (f *formatter) Write(line []byte) (int, error) {
	f.writer.enqueue(f.level, line)
	return len(line), nil
}

// writer owns the destination: its goroutine (run) is the only one that writes to out.
type writer struct {
	out   io.Writer
	lines chan *[]byte
	// wake tells run that a line was dropped, so the dropped line is written even when the line
	// that would have followed never comes.
	wake chan struct{}
	// flushes carries each Flush's done channel, closed once the lines buffered when run took it
	// are written.
	flushes chan chan struct{}
	// dropped counts every line dropped since New, by Levels index; unreported counts the ones
	// no dropped line has reported yet.
	dropped    [len(Levels)]atomic.Uint64
	unreported [len(Levels)]atomic.Uint64
	// report formats the dropped line straight to out; only run uses it.
	report slog.Handler
	// buffers holds line buffers for reuse, so a record costs no allocation once warm.
	buffers sync.Pool
}

func newWriter(out io.Writer, opts *slog.HandlerOptions) *writer {
	return &writer{
		out:     out,
		lines:   make(chan *[]byte, bufferedRecords),
		wake:    make(chan struct{}, 1),
		flushes: make(chan chan struct{}),
		report:  slog.NewTextHandler(out, opts),
		buffers: sync.Pool{New: func() any { return new([]byte) }},
	}
}

func (w *writer) enqueue(level slog.Level, line []byte) {
	// A full channel drops the line before it is copied; a line that finds room here and none at
	// the send, taken by another caller meanwhile, is dropped there.
	if len(w.lines) == cap(w.lines) {
		w.drop(level)
		return
	}
	buffer := w.buffers.Get().(*[]byte)
	*buffer = append((*buffer)[:0], line...)
	select {
	case w.lines <- buffer:
	default:
		w.buffers.Put(buffer)
		w.drop(level)
	}
}

// drop counts a line of level dropped and wakes run to report it.
func (w *writer) drop(level slog.Level) {
	i := levelIndex(level)
	w.dropped[i].Add(1)
	w.unreported[i].Add(1)
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *writer) run() {
	for {
		select {
		case line := <-w.lines:
			w.write(line)
		case <-w.wake:
		case done := <-w.flushes:
			for n := len(w.lines); n > 0; n-- {
				w.write(<-w.lines)
			}
			w.reportDropped()
			close(done)
			continue
		}
		if len(w.lines) < cap(w.lines) {
			w.reportDropped()
		}
	}
}

func (w *writer) write(line *[]byte) {
	_, _ = w.out.Write(*line)
	if cap(*line) <= maxPooledLine {
		w.buffers.Put(line)
	}
}

// reportDropped writes one WARN line counting the lines dropped since the last one, by level
// (dropped.debug, dropped.info, dropped.warn, dropped.error), when any were.
func (w *writer) reportDropped() {
	var counts [len(Levels)]uint64
	var total uint64
	for i := range w.unreported {
		if w.unreported[i].Load() == 0 {
			continue
		}
		counts[i] = w.unreported[i].Swap(0)
		total += counts[i]
	}
	if total == 0 {
		return
	}
	attrs := make([]any, len(Levels))
	for i, level := range Levels {
		attrs[i] = slog.Uint64(strings.ToLower(level.String()), counts[i])
	}
	record := slog.NewRecord(time.Now(), slog.LevelWarn, fmt.Sprintf("dropped %d log records", total), 0)
	record.AddAttrs(slog.Group("dropped", attrs...))
	_ = w.report.Handle(context.Background(), record)
}

func levelIndex(level slog.Level) int {
	switch {
	case level < slog.LevelInfo:
		return 0
	case level < slog.LevelWarn:
		return 1
	case level < slog.LevelError:
		return 2
	default:
		return 3
	}
}
