package testpg

import (
	"bytes"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"
)

// Delayed is dsn reached through a TCP proxy on 127.0.0.1 that holds each chunk the client sends
// for delay before passing it to the server, and passes the server's answers back at once: the
// network between a daemon and a Postgres that is not on its machine. A client that waits for each
// answer before it writes again - every statement outside a pipeline - pays delay once per round
// trip, as many times as the statements it sends one after another. A chunk is held from the
// moment the proxy reads it, not queued behind the hold of the chunk before, so a client that
// writes without waiting is delayed once, not once per chunk. The proxy and every connection
// through it close when the test ends, before the database is dropped (Database's cleanup is
// registered first, so it runs after this one).
func Delayed(t testing.TB, dsn string, delay time.Duration) string {
	t.Helper()
	target, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse the database URL to proxy: %v", err)
	}
	upstream := target.Host
	if target.Port() == "" {
		upstream = net.JoinHostPort(target.Hostname(), "5432")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the delayed Postgres proxy: %v", err)
	}
	proxy := &delayedProxy{listener: listener, upstream: upstream, delay: delay, conns: map[net.Conn]struct{}{}}
	proxy.wg.Add(1)
	go proxy.serve()
	t.Cleanup(proxy.close)
	through := *target
	through.Host = listener.Addr().String()
	return through.String()
}

// delayedProxy is Delayed's proxy: conns are the client and server connections open through it,
// closed with it.
type delayedProxy struct {
	listener net.Listener
	upstream string
	delay    time.Duration

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

func (p *delayedProxy) serve() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = client.Close()
			continue
		}
		if !p.track(client, server) {
			return
		}
		p.wg.Add(2)
		go func() {
			defer p.wg.Done()
			_, _ = io.Copy(client, server)
			p.untrack(client, server)
		}()
		go func() {
			defer p.wg.Done()
			p.relayDelayed(client, server)
			p.untrack(client, server)
		}()
	}
}

// chunk is what one read of the client returned, and when it may be written to the server.
type chunk struct {
	data []byte
	due  time.Time
}

// relayDelayed copies client to server, each chunk written delay after it was read. A reader and
// a writer run apart, so a chunk read while an earlier one is held is held from its own read.
func (p *delayedProxy) relayDelayed(client, server net.Conn) {
	chunks := make(chan chunk, 64)
	go func() {
		defer close(chunks)
		buffer := make([]byte, 64<<10)
		for {
			n, err := client.Read(buffer)
			if n > 0 {
				chunks <- chunk{data: bytes.Clone(buffer[:n]), due: time.Now().Add(p.delay)}
			}
			if err != nil {
				return
			}
		}
	}()
	for next := range chunks {
		time.Sleep(time.Until(next.due))
		if _, err := server.Write(next.data); err != nil {
			// The reader stops once its connection is closed below; drain what it still sends.
			p.untrack(client, server)
			for range chunks {
			}
			return
		}
	}
}

// track records a pair of connections, and closes them instead when the proxy is already closed.
func (p *delayedProxy) track(client, server net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = client.Close()
		_ = server.Close()
		return false
	}
	p.conns[client], p.conns[server] = struct{}{}, struct{}{}
	return true
}

// untrack closes a pair of connections, once either direction ends.
func (p *delayedProxy) untrack(client, server net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, client)
	delete(p.conns, server)
	_ = client.Close()
	_ = server.Close()
}

func (p *delayedProxy) close() {
	p.mu.Lock()
	p.closed = true
	_ = p.listener.Close()
	for conn := range p.conns {
		_ = conn.Close()
	}
	clear(p.conns)
	p.mu.Unlock()
	p.wg.Wait()
}
