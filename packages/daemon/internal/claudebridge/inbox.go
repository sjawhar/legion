package claudebridge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// InboxVariable names the inbox socket in Claude Code's environment, which its MCP servers inherit:
// packages/claude-envoy's channel server writes each Envoy delivery there instead of a channel
// notification, which Claude Code's print mode never delivers.
const InboxVariable = "LEGION_CLAUDE_INBOX"

// InboxDelivery is one line a client writes to the inbox: an Envoy delivery, already rendered, that
// becomes a user turn. InboxAnswer is the line the bridge writes back once Claude Code has taken the
// delivery, re-emitting it as the user message it reads (--replay-user-messages), or with an error
// once it never will: the delivery is in the transcript, which is what claude-envoy's `done` says.
type InboxDelivery struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	// Blocks is the delivery as Claude Code's own content blocks, which a line carries where the
	// delivery is more than its text: a person's own message and the pictures it embeds
	// (packages/claude-envoy, src/legion-inbox.ts). The bridge reads none of it — each block goes
	// to Claude Code as it came — and Content stays the delivery's text, so a line without the
	// field is the line it always was and a bridge of any age delivers the text.
	Blocks []json.RawMessage `json:"blocks,omitempty"`
}

type InboxAnswer struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// inbox is the bridge's listener: a unix socket in a directory of its own, 0700, so only this user
// reaches it, made under the system's temporary directory because a socket's path is bounded (108
// bytes on Linux) and a claim's directory under a state directory need not fit.
type inbox struct {
	dir      string
	path     string
	listener net.Listener
	wg       sync.WaitGroup
}

// inboxDirPrefix names the inbox's directory, which os.MkdirTemp makes under the process TMPDIR,
// and inboxSocketName the socket the bridge binds in it.
const (
	inboxDirPrefix  = "legion-claude-"
	inboxSocketName = "inbox.sock"
)

// InboxLongestPath is the longest path the bridge makes under the process TMPDIR, written
// relative to it: the inbox directory and the socket in it. The ten digits are os.MkdirTemp's own
// suffix at its widest, a random number below 1e10. A test that runs the bridge in-process hands
// this to testtmp.Dir as the room the TMPDIR it places needs, so that the socket's path stays
// inside the 107 bytes Linux holds.
const InboxLongestPath = inboxDirPrefix + "0123456789" + "/" + inboxSocketName

// listenInbox opens the inbox and serves it until close, handing each delivery to the loop.
func listenInbox(events chan<- event, stop <-chan struct{}, logger *log.Logger) (*inbox, error) {
	dir, err := os.MkdirTemp("", inboxDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("make the inbox directory: %w", err)
	}
	path := filepath.Join(dir, inboxSocketName)
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("listen on the inbox: %w", err)
	}
	in := &inbox{dir: dir, path: path, listener: listener}
	in.wg.Add(1)
	go func() {
		defer in.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					logger.Printf("the inbox stopped accepting: %v", err)
				}
				return
			}
			in.wg.Add(1)
			go func() {
				defer in.wg.Done()
				serveInbox(conn, events, stop, logger)
			}()
		}
	}()
	return in, nil
}

const (
	// inboxPending bounds the deliveries one client has sent and the bridge has not yet answered;
	// past it the inbox reads that client's next line once the oldest is answered.
	inboxPending = 64
	// inboxFlushTimeout bounds writing what a client is owed once the inbox closes: the answers
	// the loop gave before closing it, each one short line a reading client takes at once.
	inboxFlushTimeout = 100 * time.Millisecond
	malformedDelivery = "an inbox delivery is one JSON object with an id and content"
)

// owed is one answer a client is owed, in the order it sent its deliveries.
type owed struct {
	id     string
	answer <-chan error
}

// serveInbox reads one client's deliveries, handing each to the loop as it arrives, and answers
// them in the order they came, each once the loop has answered it. The loop answers every delivery
// it took before the inbox closes, so those answers are written then too, within inboxFlushTimeout.
func serveInbox(conn net.Conn, events chan<- event, stop <-chan struct{}, logger *log.Logger) {
	defer conn.Close()
	go func() {
		<-stop
		_ = conn.SetReadDeadline(time.Now())
		_ = conn.SetWriteDeadline(time.Now().Add(inboxFlushTimeout))
	}()
	pending := make(chan owed, inboxPending)
	go func() {
		defer close(pending)
		reader := bufio.NewReaderSize(conn, 64<<10)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			answer := make(chan error, 1)
			var d InboxDelivery
			if err := json.Unmarshal(line, &d); err != nil || d.ID == "" || d.Content == "" {
				answer <- errors.New(malformedDelivery)
			} else if !send(events, stop, event{delivery: &delivery{id: d.ID, content: d.Content, blocks: d.Blocks, answer: answer}}) {
				return
			}
			select {
			case pending <- owed{id: d.ID, answer: answer}:
			case <-stop:
				return
			}
		}
	}()
	encoder := json.NewEncoder(conn)
	for o := range pending {
		var err error
		select {
		case err = <-o.answer:
		case <-stop:
			select {
			case err = <-o.answer:
			default:
				return
			}
		}
		answer := InboxAnswer{ID: o.id, OK: true}
		if err != nil {
			answer = InboxAnswer{ID: o.id, Error: err.Error()}
		}
		if err := encoder.Encode(answer); err != nil {
			logger.Printf("could not answer inbox delivery %s: %v", o.id, err)
			return
		}
	}
}

// close stops accepting, waits for every client, which closing stop ends, and removes the socket's
// directory.
func (in *inbox) close() {
	_ = in.listener.Close()
	in.wg.Wait()
	_ = os.RemoveAll(in.dir)
}
