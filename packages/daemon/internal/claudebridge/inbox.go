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
)

// InboxVariable names the inbox socket in Claude Code's environment, which its MCP servers inherit:
// packages/claude-envoy's channel server writes each Envoy delivery there instead of a channel
// notification, which Claude Code's print mode never delivers.
const InboxVariable = "LEGION_CLAUDE_INBOX"

// InboxDelivery is one line a client writes to the inbox: an Envoy delivery, already rendered, that
// becomes a user turn. InboxAnswer is the line the bridge writes back once the delivery is queued:
// the receipt the channel server publishes for a forwarded lane means exactly that.
type InboxDelivery struct {
	ID      string `json:"id"`
	Content string `json:"content"`
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

// listenInbox opens the inbox and serves it until close, handing each delivery to the loop.
func listenInbox(events chan<- event, stop <-chan struct{}, logger *log.Logger) (*inbox, error) {
	dir, err := os.MkdirTemp("", "legion-claude-")
	if err != nil {
		return nil, fmt.Errorf("make the inbox directory: %w", err)
	}
	path := filepath.Join(dir, "inbox.sock")
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

// serveInbox reads one client's deliveries, each answered once the loop has queued it.
func serveInbox(conn net.Conn, events chan<- event, stop <-chan struct{}, logger *log.Logger) {
	defer conn.Close()
	go func() {
		<-stop
		_ = conn.Close()
	}()
	reader := bufio.NewReaderSize(conn, 64<<10)
	encoder := json.NewEncoder(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var d InboxDelivery
		if err := json.Unmarshal(line, &d); err != nil || d.ID == "" || d.Content == "" {
			answer := InboxAnswer{ID: d.ID, Error: "an inbox delivery is one JSON object with an id and content"}
			if encodeErr := encoder.Encode(answer); encodeErr != nil {
				return
			}
			continue
		}
		queued := make(chan error, 1)
		if !send(events, stop, event{delivery: &delivery{id: d.ID, content: d.Content, queued: queued}}) {
			return
		}
		answer := InboxAnswer{ID: d.ID, OK: true}
		if err := <-queued; err != nil {
			answer = InboxAnswer{ID: d.ID, Error: err.Error()}
		}
		if err := encoder.Encode(answer); err != nil {
			logger.Printf("could not answer inbox delivery %s: %v", d.ID, err)
			return
		}
	}
}

// close stops accepting, ends every client and removes the socket's directory.
func (in *inbox) close() {
	_ = in.listener.Close()
	in.wg.Wait()
	_ = os.RemoveAll(in.dir)
}
