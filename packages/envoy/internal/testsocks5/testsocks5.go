// Package testsocks5 is the in-process SOCKS5 server Envoy's tests reach NATS through, as testnats
// is the NATS they reach: one implementation of the proxy side of RFC 1928 (no authentication,
// CONNECT only) for every suite that tests ENVOY_NATS_SOCKS5_PROXY, so the bus's proof and the
// Dispatch settings table's agree on the protocol. It listens on loopback only and resolves nothing.
package testsocks5

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Request is one CONNECT a Server received: its address type (0x01 an IPv4 address, 0x03 a domain
// name, 0x04 an IPv6 address) and the address it named, as host:port.
type Request struct {
	AddressType byte
	Address     string
}

// Server is a SOCKS5 server on a loopback port. Started with Start, it serves every request from
// one target, whatever address the request names, so a name that resolves nowhere is served; started
// with StartRefusing, it answers every request with one reply code and relays nothing. Either way it
// records each request (Requests) and counts every connection it accepts (Accepted).
type Server struct {
	// Addr is the server's host:port, the value an ENVOY_NATS_SOCKS5_PROXY names.
	Addr string

	target   string
	reply    byte
	listener net.Listener
	accepted atomic.Int64

	mu       sync.Mutex
	requests []Request
	relayed  []net.Conn
}

// Start serves until the test ends, relaying every request to target (host:port).
func Start(t testing.TB, target string) *Server {
	t.Helper()
	return start(t, &Server{target: target})
}

// StartRefusing serves until the test ends, answering every request with reply, RFC 1928's reply
// field (0x04 host unreachable, 0x05 connection refused), and relaying nothing.
func StartRefusing(t testing.TB, reply byte) *Server {
	t.Helper()
	return start(t, &Server{reply: reply})
}

func start(t testing.TB, server *Server) *Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the SOCKS5 test server: %v", err)
	}
	server.Addr, server.listener = listener.Addr().String(), listener
	go server.serve()
	t.Cleanup(server.Close)
	return server
}

// Requests returns every request the server has received, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Accepted is how many connections the server has accepted, whether or not each spoke SOCKS5.
func (s *Server) Accepted() int64 {
	return s.accepted.Load()
}

// DropAll closes every connection the server relays, so a client's connection through it ends as
// a lost link does (nats.go then reconnects in place).
func (s *Server) DropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.relayed {
		_ = conn.Close()
	}
	s.relayed = nil
}

// Close stops the server: it accepts nothing more, and every connection it relays is closed.
func (s *Server) Close() {
	_ = s.listener.Close()
	s.DropAll()
}

func (s *Server) serve() {
	for {
		client, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.accepted.Add(1)
		go s.handle(client)
	}
}

// handle reads the request and records it. Refusing, it answers the reply code with a zero bound
// address and closes. Relaying, it dials the target whatever the request named, answers success with
// a zero bound address, then copies bytes both ways until either side closes.
func (s *Server) handle(client net.Conn) {
	// A client that does not speak SOCKS5 first (a NATS client dialling the server directly waits
	// for the server's INFO) is dropped rather than held.
	_ = client.SetDeadline(time.Now().Add(time.Second))
	request, err := readConnect(client)
	if err != nil {
		_ = client.Close()
		return
	}
	_ = client.SetDeadline(time.Time{})
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	if s.reply != 0 {
		_, _ = client.Write([]byte{0x05, s.reply, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		_ = client.Close()
		return
	}
	server, err := net.Dial("tcp", s.target)
	if err != nil {
		_ = client.Close()
		return
	}
	if _, err := client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		_ = client.Close()
		_ = server.Close()
		return
	}
	s.mu.Lock()
	s.relayed = append(s.relayed, client, server)
	s.mu.Unlock()
	go func() {
		_, _ = io.Copy(server, client)
		_ = server.Close()
		_ = client.Close()
	}()
	_, _ = io.Copy(client, server)
	_ = client.Close()
	_ = server.Close()
}

// readConnect reads a client's greeting (05 <n> <methods>), answers that it needs no
// authentication (05 00), and reads its CONNECT request (05 01 00 <type> <address> <port>).
func readConnect(conn net.Conn) (Request, error) {
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return Request{}, err
	}
	if greeting[0] != 0x05 {
		return Request{}, fmt.Errorf("greeting of SOCKS version %d", greeting[0])
	}
	if _, err := io.ReadFull(conn, make([]byte, greeting[1])); err != nil {
		return Request{}, err
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return Request{}, err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return Request{}, err
	}
	if header[0] != 0x05 || header[1] != 0x01 {
		return Request{}, fmt.Errorf("request %x is not a SOCKS5 CONNECT", header)
	}
	var host string
	switch header[3] {
	case 0x01, 0x04:
		size := net.IPv4len
		if header[3] == 0x04 {
			size = net.IPv6len
		}
		ip := make([]byte, size)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return Request{}, err
		}
		host = net.IP(ip).String()
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return Request{}, err
		}
		name := make([]byte, length[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return Request{}, err
		}
		host = string(name)
	default:
		return Request{}, fmt.Errorf("address type %#x", header[3])
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return Request{}, err
	}
	return Request{AddressType: header[3], Address: net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port))))}, nil
}
