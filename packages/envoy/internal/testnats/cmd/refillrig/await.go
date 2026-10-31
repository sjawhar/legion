package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

// logLine is one line of the listener's log: its own JSON records, and the text lines the bus
// writes through the default logger (with a time to the second).
type logLine struct {
	at      int64
	msg     string
	outcome string
	entries int
	offset  int64
}

func parseLogLine(line []byte) (logLine, bool) {
	if len(line) > 0 && line[0] == '{' {
		var record struct {
			Time    string `json:"time"`
			Msg     string `json:"msg"`
			Outcome string `json:"outcome"`
			Entries int    `json:"entries"`
		}
		if json.Unmarshal(line, &record) != nil {
			return logLine{}, false
		}
		at, err := time.Parse(time.RFC3339Nano, record.Time)
		if err != nil {
			return logLine{}, false
		}
		return logLine{at: at.UnixNano(), msg: record.Msg, outcome: record.Outcome, entries: record.Entries}, true
	}
	const layout = "2006/01/02 15:04:05"
	if len(line) < len(layout)+1 {
		return logLine{}, false
	}
	at, err := time.ParseInLocation(layout, string(line[:len(layout)]), time.Local)
	if err != nil {
		return logLine{}, false
	}
	return logLine{at: at.UnixNano(), msg: strings.TrimSpace(string(line[len(layout):]))}, true
}

// readLog reads the log lines from offset on, each with the offset just past it.
func readLog(path string, offset int64) ([]logLine, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	var lines []logLine
	reader := bufio.NewReaderSize(file, 1<<20)
	position := offset
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 && raw[len(raw)-1] == '\n' {
			position += int64(len(raw))
			if line, ok := parseLogLine(bytes.TrimSpace(raw)); ok {
				line.offset = position
				lines = append(lines, line)
			}
		}
		if err == io.EOF {
			return lines, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

var signals = map[string]syscall.Signal{"STOP": syscall.SIGSTOP, "CONT": syscall.SIGCONT, "KILL": syscall.SIGKILL, "TERM": syscall.SIGTERM}

// matches is a repeatable -match flag: each value is text a line's message contains, optionally
// followed by @outcome, the line's outcome attribute.
type matches []string

func (m *matches) String() string        { return strings.Join(*m, " or ") }
func (m *matches) Set(text string) error { *m = append(*m, text); return nil }

// any reports whether line is one of the lines m names.
func (m matches) any(line logLine) bool {
	for _, match := range m {
		text, outcome, hasOutcome := strings.Cut(match, "@")
		if strings.Contains(line.msg, text) && (!hasOutcome || line.outcome == outcome) {
			return true
		}
	}
	return false
}

// runAwait waits for the first log line from offset on that one of the -match values names and
// whose time is after after-ns, signals pid at once when asked to, and prints the line's time, the
// signal's time (0 without one) and the offset past the line. With -cont-after it then continues
// the process it stopped that long after the stop, so a script blocked on that process's work
// (refillrig recreate filling a stopped NATS) still gets it back.
func runAwait(args []string) error {
	flags := flag.NewFlagSet("await", flag.ContinueOnError)
	path := flags.String("log", "", "listener log")
	offset := flags.Int64("offset", 0, "byte offset to read from")
	var match matches
	flags.Var(&match, "match", "text the line's message contains, optionally followed by @<outcome attribute>; repeatable, any one matches")
	after := flags.Int64("after-ns", 0, "skip lines logged at or before this Unix time in nanoseconds")
	timeout := flags.Duration("timeout", time.Minute, "how long to wait")
	signalName := flags.String("signal", "", "signal to send pid when the line appears: STOP, CONT, KILL or TERM")
	pid := flags.Int("pid", 0, "process to signal")
	contAfter := flags.Duration("cont-after", 0, "with -signal STOP, send pid SIGCONT this long after the stop")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(match) == 0 {
		return errors.New("no -match")
	}
	var sig syscall.Signal
	if *signalName != "" {
		var ok bool
		if sig, ok = signals[*signalName]; !ok || *pid <= 0 {
			return fmt.Errorf("signal %q to pid %d: want STOP, CONT, KILL or TERM and a pid", *signalName, *pid)
		}
	}
	if *contAfter > 0 && sig != syscall.SIGSTOP {
		return errors.New("-cont-after needs -signal STOP")
	}
	file, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(*offset, io.SeekStart); err != nil {
		return err
	}
	deadline := time.Now().Add(*timeout)
	position := *offset
	var pending []byte
	buffer := make([]byte, 1<<16)
	for {
		n, readErr := file.Read(buffer)
		pending = append(pending, buffer[:n]...)
		for {
			end := bytes.IndexByte(pending, '\n')
			if end < 0 {
				break
			}
			raw := pending[:end+1]
			pending = pending[end+1:]
			position += int64(len(raw))
			line, ok := parseLogLine(bytes.TrimSpace(raw))
			if !ok || !match.any(line) || line.at <= *after {
				continue
			}
			var signalled int64
			if sig != 0 {
				if err := syscall.Kill(*pid, sig); err != nil {
					return fmt.Errorf("signal %d: %w", *pid, err)
				}
				signalled = time.Now().UnixNano()
			}
			fmt.Printf("%d %d %d\n", line.at, signalled, position)
			if *contAfter > 0 {
				time.Sleep(*contAfter)
				if err := syscall.Kill(*pid, syscall.SIGCONT); err != nil {
					return fmt.Errorf("continue %d: %w", *pid, err)
				}
			}
			return nil
		}
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if n == 0 {
			if time.Now().After(deadline) {
				return fmt.Errorf("no line naming %s in %s within %s", match.String(), *path, *timeout)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
}
