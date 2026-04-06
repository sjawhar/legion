package mcpbridge

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

type ServerState int32

const (
	StateStarting ServerState = iota
	StateReady
	StateDead
)

type Server interface {
	Name() string
	Start() error
	Stop()
	State() ServerState
	WaitForExit() error
	ReadResource(uri string) ([]resourceContent, error)
}

func NewServer(cfg ServerConfig, onNotify func(uri string)) Server {
	switch cfg.Transport {
	case "http":
		return NewHTTPServer(cfg, onNotify)
	default:
		return NewManagedServer(cfg, onNotify)
	}
}

type ManagedServer struct {
	cfg       ServerConfig
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	scanner   *bufio.Scanner
	session   *session
	state     atomic.Int32
	onNotify  func(uri string)
	stopCh    chan struct{}
	closeOnce sync.Once
}

func NewManagedServer(cfg ServerConfig, onNotify func(uri string)) *ManagedServer {
	s := &ManagedServer{cfg: cfg, onNotify: onNotify, stopCh: make(chan struct{})}
	s.state.Store(int32(StateStarting))
	return s
}

func (s *ManagedServer) Name() string      { return s.cfg.Name }
func (s *ManagedServer) State() ServerState { return ServerState(s.state.Load()) }

func (s *ManagedServer) Start() error {
	cmd := exec.Command(s.cfg.Command[0], s.cfg.Command[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	for k, v := range s.cfg.Env { cmd.Env = append(cmd.Env, k+"="+v) }
	stdin, err := cmd.StdinPipe()
	if err != nil { return fmt.Errorf("stdin pipe: %w", err) }
	stdout, err := cmd.StdoutPipe()
	if err != nil { return fmt.Errorf("stdout pipe: %w", err) }
	if err := cmd.Start(); err != nil { return fmt.Errorf("start %s: %w", s.cfg.Name, err) }
	s.cmd = cmd
	s.stdin = stdin
	s.scanner = bufio.NewScanner(stdout)
	s.scanner.Buffer(make([]byte, 1<<20), 1<<20)
	s.session = newSession(s.cfg.Name, func(data []byte) error {
		data = append(data, '\n')
		_, err := s.stdin.Write(data)
		return err
	}, s.onNotify, s.stopCh)
	go s.readLoop()
	if err := s.session.initialize(); err != nil { s.kill(); return fmt.Errorf("initialize %s: %w", s.cfg.Name, err) }
	for _, uri := range s.cfg.Resources {
		if err := s.session.subscribe(uri); err != nil { s.kill(); return fmt.Errorf("subscribe %s to %s: %w", s.cfg.Name, uri, err) }
	}
	s.state.Store(int32(StateReady))
	log.Printf("mcp-bridge: server %s ready", s.cfg.Name)
	return nil
}

func (s *ManagedServer) Stop() {
	s.closeOnce.Do(func() { close(s.stopCh) })
	s.state.Store(int32(StateDead))
	if s.stdin != nil { s.stdin.Close() }
	if s.cmd != nil && s.cmd.Process != nil {
		done := make(chan struct{})
		go func() { s.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second): s.cmd.Process.Kill()
		}
	}
}

func (s *ManagedServer) WaitForExit() error {
	if s.cmd == nil { return fmt.Errorf("not started") }
	return s.cmd.Wait()
}

func (s *ManagedServer) ReadResource(uri string) ([]resourceContent, error) { return s.session.readResource(uri) }

func (s *ManagedServer) kill() {
	if s.stdin != nil { s.stdin.Close() }
	if s.cmd != nil && s.cmd.Process != nil { s.cmd.Process.Kill(); s.cmd.Wait() }
	s.state.Store(int32(StateDead))
}

func (s *ManagedServer) readLoop() {
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) == 0 { continue }
		s.session.handleMessage(line)
	}
	if err := s.scanner.Err(); err != nil { log.Printf("mcp-bridge: %s: read error: %v", s.cfg.Name, err) }
	s.state.Store(int32(StateDead))
}
