package logging_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/logging"
)

// lockedBuffer is a destination the handler's writer writes while the test reads it.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	// delay is how long each write takes, standing in for a slow destination.
	delay time.Duration
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	time.Sleep(b.delay)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// stalled is a handler writing to a pipe nobody reads, filled until it dropped a record: the pipe
// takes no more writes, the handler's writer is blocked writing to it and its channel is full.
// Each filling line carries 4 KiB, so the pipe holds few of them and draining it later frees few
// slots in the channel.
type stalled struct {
	// reader and writer are the pipe's ends; writer is the handler's destination.
	reader, writer *os.File
	handler        *logging.Handler
	logger         *slog.Logger
	// filled is how many INFO records the filling logged.
	filled int
}

func stalledPipe(t *testing.T) *stalled {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("open a pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	s := &stalled{reader: reader, writer: writer, handler: logging.New(writer, nil)}
	s.logger = slog.New(s.handler)
	padding := strings.Repeat("x", 4<<10)
	for s.handler.Dropped(slog.LevelInfo) == 0 {
		if s.filled == 1_000_000 {
			t.Fatalf("a million records into a pipe nobody reads dropped none")
		}
		s.logger.Info("filling", "padding", padding)
		s.filled++
	}
	return s
}

var droppedLine = regexp.MustCompile(`level=WARN msg="dropped (\d+) log records" dropped\.debug=(\d+) dropped\.info=(\d+) dropped\.warn=(\d+) dropped\.error=(\d+)`)

// While the destination takes no writes - a stderr pipe nobody reads - a burst of errors from a
// thousand goroutines returns at once, and every record the handler did not write is one it counts
// as dropped under its level. The dropped lines written once the pipe is read again report every
// drop, each once.
func TestErrorsReturnAtOnceWhileTheDestinationTakesNoWrites(t *testing.T) {
	pipe := stalledPipe(t)
	handler, logger := pipe.handler, pipe.logger

	const callers = 1000
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for caller := range callers {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			logger.Error("concurrent", "caller", caller, "error", errors.New("boom"))
		}()
	}
	ready.Wait()
	began := time.Now()
	close(start)
	done.Wait()
	elapsed := time.Since(began)
	t.Logf("%d concurrent errors returned in %v; %d dropped", callers, elapsed, handler.Dropped(slog.LevelError))
	if elapsed > 100*time.Millisecond {
		t.Errorf("%d concurrent errors took %v to return while nobody read the destination, want under 100ms", callers, elapsed)
	}
	if handler.Dropped(slog.LevelError) == 0 {
		t.Fatalf("no error was dropped: the filling left room in the channel, so the test proves nothing")
	}

	var output bytes.Buffer
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(&output, pipe.reader)
		copied <- err
	}()
	if !handler.Flush(30 * time.Second) {
		t.Fatalf("the handler did not flush once its pipe was read")
	}
	_ = pipe.writer.Close()
	if err := <-copied; err != nil {
		t.Fatalf("read the pipe: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	var fillingWritten, concurrentWritten int
	var reported [len(logging.Levels)]uint64
	for _, line := range lines {
		switch {
		case strings.Contains(line, " msg=filling "):
			fillingWritten++
		case strings.Contains(line, " msg=concurrent "):
			concurrentWritten++
		default:
			match := droppedLine.FindStringSubmatch(line)
			if match == nil {
				t.Errorf("unexpected line %q", line)
				continue
			}
			var sum uint64
			for i := range reported {
				count, _ := strconv.ParseUint(match[2+i], 10, 64)
				reported[i] += count
				sum += count
			}
			if total, _ := strconv.ParseUint(match[1], 10, 64); total != sum {
				t.Errorf("dropped line %q: total %d, levels sum to %d", line, total, sum)
			}
		}
	}
	if dropped := handler.Dropped(slog.LevelInfo); uint64(fillingWritten)+dropped != uint64(pipe.filled) {
		t.Errorf("INFO: %d written + %d dropped, want the %d logged", fillingWritten, dropped, pipe.filled)
	}
	if dropped := handler.Dropped(slog.LevelError); uint64(concurrentWritten)+dropped != callers {
		t.Errorf("ERROR: %d written + %d dropped, want the %d logged", concurrentWritten, dropped, callers)
	}
	for i, level := range logging.Levels {
		if reported[i] != handler.Dropped(level) {
			t.Errorf("%s: dropped lines report %d, the counter %d", level, reported[i], handler.Dropped(level))
		}
	}
}

// Flush returns once every record logged before it is written, even to a destination that takes
// its time over each write.
func TestFlushWritesWhatWasBuffered(t *testing.T) {
	destination := &lockedBuffer{delay: time.Millisecond}
	handler := logging.New(destination, nil)
	logger := slog.New(handler)
	const records = 100
	for n := range records {
		logger.Info("record", "n", n)
	}
	if !handler.Flush(30 * time.Second) {
		t.Fatalf("Flush gave up on a destination that takes every write")
	}
	lines := strings.Split(strings.TrimSuffix(destination.String(), "\n"), "\n")
	if len(lines) != records {
		t.Fatalf("Flush returned with %d of %d lines written:\n%s", len(lines), records, destination.String())
	}
	for n, line := range lines {
		if !strings.HasSuffix(line, fmt.Sprintf(" msg=record n=%d", n)) {
			t.Errorf("line %d = %q, want record %d in the order logged", n, line, n)
		}
	}
}

// A process exits through Flush, so Flush gives up at its timeout when the destination takes no
// writes rather than holding the exit forever.
func TestFlushGivesUpAtItsTimeoutWhileTheDestinationTakesNoWrites(t *testing.T) {
	handler := stalledPipe(t).handler
	began := time.Now()
	if handler.Flush(50 * time.Millisecond) {
		t.Fatalf("Flush reported a full channel of 4 KiB lines written to a pipe nobody reads")
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Errorf("Flush took %v to give up, want about its 50ms timeout", elapsed)
	}
}

// Log parsers and the tests that read Dispatch's stderr read slog.TextHandler's format, so the
// handler writes exactly what slog.TextHandler writes for the same logger calls, and leaves out
// what it leaves out below the configured level.
func TestTheHandlerWritesWhatTheTextHandlerWrites(t *testing.T) {
	for _, options := range []*slog.HandlerOptions{nil, {Level: slog.LevelWarn}, {AddSource: true}} {
		var want bytes.Buffer
		got := &lockedBuffer{}
		handler := logging.New(got, options)
		loggers := []*slog.Logger{slog.New(slog.NewTextHandler(&want, options)), slog.New(handler)}
		for _, logger := range loggers {
			derived := logger.With("service", "dispatch").WithGroup("request").With("id", 7)
			derived.Error("dispatch: write handler error", "error", errors.New("pool: acquire timed out"),
				slog.Group("db", "pool", "requests", "waited", 75*time.Millisecond), "quoted", `a "b" c`)
			derived.Info("dispatch: request", "status", 200)
			derived.Warn("dispatch: slow", "elapsed", time.Second)
			derived.Debug("dispatch: detail")
			logger.Log(context.Background(), slog.LevelInfo+2, "between levels")
		}
		if !handler.Flush(30 * time.Second) {
			t.Fatalf("Flush gave up on a destination that takes every write")
		}
		// The time differs between the two calls, so it is compared with the rest of the line
		// once each line's own time is removed.
		timestamp := regexp.MustCompile(`time=\S+ `)
		source := regexp.MustCompile(`source=\S+:\d+ `)
		normalize := func(output string) string {
			return source.ReplaceAllString(timestamp.ReplaceAllString(output, "time=T "), "source=S ")
		}
		if normalize(got.String()) != normalize(want.String()) {
			t.Errorf("options %+v: the handler wrote\n%s\nslog.TextHandler wrote\n%s", options, got.String(), want.String())
		}
		if strings.Contains(got.String(), "dispatch: detail") {
			t.Errorf("options %+v: a DEBUG record was written", options)
		}
	}
}
