// packages/envoy/cmd/agent-secrets/secret_prompt_reader_test.go
//go:build (linux || darwin) && (amd64 || arm64)

// The value prompt's reader state (promptReader) and the signal keys it reads from the terminal's
// settings (signalKeys), without a terminal: each read's bytes fed in turn, as the reader feeds
// them.
package main

import (
	"errors"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// promptSettings is a terminal's settings as a shell hands them to the prompt: Ctrl-C, Ctrl-\ and
// Ctrl-Z as the signal keys, Ctrl-D as end of file, DEL as erase, Ctrl-W as word erase and Ctrl-U
// as kill.
func promptSettings() *unix.Termios {
	var t unix.Termios
	t.Cc[unix.VINTR], t.Cc[unix.VQUIT], t.Cc[unix.VSUSP] = 0x03, 0x1c, 0x1a
	t.Cc[unix.VEOF], t.Cc[unix.VERASE], t.Cc[unix.VWERASE], t.Cc[unix.VKILL] = 0x04, 0x7f, 0x17, 0x15
	return &t
}

// feedStep is one read's bytes and what feed answers for it.
type feedStep struct {
	in   string
	done bool
	sig  syscall.Signal
}

func TestPromptReaderFeed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ignored []int
		steps   []feedStep
		line    string
		err     error
		more    bool
		inPaste bool
	}{
		{name: "a typed line", steps: []feedStep{{in: "value\r", done: true}}, line: "value"},
		{name: "a line in two reads", steps: []feedStep{{in: "val"}, {in: "ue\r", done: true}}, line: "value"},
		{name: "Ctrl-D ends the line", steps: []feedStep{{in: "value\x04", done: true}}, line: "value"},
		{name: "erase and kill edit it", steps: []feedStep{{in: "vax\x7fl\x15value\r", done: true}}, line: "value"},
		{name: "word erase", steps: []feedStep{{in: "abc def\x17ghi\r", done: true}}, line: "abc ghi"},
		{
			name:  "Ctrl-Z sends SIGTSTP and discards the rest of the read",
			steps: []feedStep{{in: "partone\x1aparttwo\r", sig: syscall.SIGTSTP}},
			line:  "partone",
		},
		{name: "Ctrl-C sends SIGINT", steps: []feedStep{{in: "val\x03ue\r", sig: syscall.SIGINT}}, line: "val"},
		{name: "Ctrl-\\ sends SIGQUIT", steps: []feedStep{{in: "\x1c", sig: syscall.SIGQUIT}}},
		{
			name:    "an ignored key does nothing",
			ignored: []int{unix.VSUSP},
			steps:   []feedStep{{in: "part\x1aone\r", done: true}},
			line:    "partone",
		},
		{
			name:  "a key inside a paste is a control byte",
			steps: []feedStep{{in: "\x1b[200~paste\x1atail\r\x1b[201~", done: true}},
			err:   promptControlByteError(0x1a),
		},
		{
			name:  "a key inside a paste split across reads is a control byte",
			steps: []feedStep{{in: "\x1b[200~paste\x03"}, {in: "tail\r\x1b[201~", done: true}},
			err:   promptControlByteError(0x03),
		},
		{
			name:  "a pasted line ends at the paste's close",
			steps: []feedStep{{in: "\x1b[200~value\r\n"}, {in: "\x1b[201~", done: true}},
			line:  "value",
		},
		{
			name:  "paste marks split across reads",
			steps: []feedStep{{in: "\x1b[20"}, {in: "0~value\n\x1b[2"}, {in: "01~", done: true}},
			line:  "value",
		},
		{
			name:  "a key after a paste's close in the same read sends its signal",
			steps: []feedStep{{in: "\x1b[200~value\r\x1b[201~\x03", done: true, sig: syscall.SIGINT}},
			line:  "value",
		},
		{
			name:  "a key after the line in the same read sends its signal",
			steps: []feedStep{{in: "value\r\x1a", done: true, sig: syscall.SIGTSTP}},
			line:  "value",
			more:  true,
		},
		{
			name:  "a key in a paste beginning after the line is pasted text",
			steps: []feedStep{{in: "value\r\x1b[200~more\x03", done: true}},
			line:  "value",
			more:  true,
		},
		{
			name:    "after the line, a paste that begins is read through its close",
			steps:   []feedStep{{in: "value\r", done: true}, {in: "\x1b[200~late\x03"}},
			line:    "value",
			more:    true,
			inPaste: true,
		},
		{
			name:  "after the line, a key outside a paste sends its signal",
			steps: []feedStep{{in: "value\r", done: true}, {in: "x\x1ctail", sig: syscall.SIGQUIT}},
			line:  "value",
			more:  true,
		},
		{
			name:    "a paste with no closing mark stays open",
			steps:   []feedStep{{in: "\x1b[200~open\x03\x1a\x1c"}},
			err:     promptControlByteError(0x03),
			inPaste: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPromptReader(promptSettings(), true, tc.ignored)
			for i, step := range tc.steps {
				done, sig := r.feed([]byte(step.in))
				if done != step.done || sig != step.sig {
					t.Fatalf("read %d %q: feed = %t, %v; want %t, %v", i, step.in, done, sig, step.done, step.sig)
				}
			}
			if string(r.line) != tc.line || !errors.Is(r.err, tc.err) || r.more != tc.more || r.inPaste != tc.inPaste {
				t.Fatalf("line %q, err %v, more %t, inPaste %t; want %q, %v, %t, %t",
					r.line, r.err, r.more, r.inPaste, tc.line, tc.err, tc.more, tc.inPaste)
			}
		})
	}
}

func TestPromptSignalKeys(t *testing.T) {
	all := [3]promptKey{{0x03, syscall.SIGINT}, {0x1c, syscall.SIGQUIT}, {0x1a, syscall.SIGTSTP}}
	unset := promptSettings()
	unset.Cc[unix.VQUIT], unset.Cc[unix.VSUSP] = 0, 0xff
	for _, tc := range []struct {
		name        string
		saved       *unix.Termios
		controlling bool
		ignored     []int
		want        [3]promptKey
	}{
		{"the controlling terminal's keys", promptSettings(), true, nil, all},
		{"a terminal that is not the controlling one has none", promptSettings(), false, nil, [3]promptKey{}},
		{"an ignored signal's key does nothing", promptSettings(), true, []int{unix.VINTR}, [3]promptKey{{0x03, 0}, all[1], all[2]}},
		{"a key not set (0 or 0xff) is absent", unset, true, nil, [3]promptKey{all[0], {}, {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := signalKeys(tc.saved, tc.controlling, tc.ignored); got != tc.want {
				t.Fatalf("signalKeys = %v; want %v", got, tc.want)
			}
		})
	}
}
