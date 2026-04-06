package mcpbridge

import (
	"log"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// Bridge manages multiple MCP server connections and publishes events to NATS.
type Bridge struct {
	cfg     *Config
	client  *bus.Client
	servers []*ManagedServer
	mu      sync.Mutex
	stopCh  chan struct{}
}

// NewBridge creates a bridge from config and a NATS client.
func NewBridge(cfg *Config, client *bus.Client) *Bridge {
	return &Bridge{
		cfg:    cfg,
		client: client,
		stopCh: make(chan struct{}),
	}
}

// Start spawns all configured MCP servers and begins processing notifications.
func (b *Bridge) Start() error {
	for i := range b.cfg.Servers {
		serverCfg := b.cfg.Servers[i]
		s := NewManagedServer(serverCfg, b.makeHandler(&serverCfg))
		if err := s.Start(); err != nil {
			// Stop already-started servers.
			for _, started := range b.servers {
				started.Stop()
			}
			return err
		}
		b.mu.Lock()
		b.servers = append(b.servers, s)
		b.mu.Unlock()

		// Monitor this server for unexpected exit.
		go b.monitor(s, &serverCfg)
	}
	return nil
}

// Stop gracefully shuts down all managed servers.
func (b *Bridge) Stop() {
	close(b.stopCh)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.servers {
		s.Stop()
	}
}

// Healthy returns true if all servers are ready and NATS is flushable.
func (b *Bridge) Healthy() bool {
	if err := b.client.Conn.FlushTimeout(3 * time.Second); err != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.servers {
		if s.State() != StateReady {
			return false
		}
	}
	return true
}

func (b *Bridge) makeHandler(cfg *ServerConfig) func(uri string) {
	return func(uri string) {
		b.handleNotification(cfg, uri)
	}
}

func (b *Bridge) handleNotification(cfg *ServerConfig, uri string) {
	// Find the managed server for this config.
	b.mu.Lock()
	var server *ManagedServer
	for _, s := range b.servers {
		if s.cfg.Name == cfg.Name {
			server = s
			break
		}
	}
	b.mu.Unlock()

	if server == nil {
		log.Printf("mcp-bridge: no server for %s", cfg.Name)
		return
	}

	// Check if URI matches the pattern.
	if _, err := cfg.RenderTopic(uri); err != nil {
		log.Printf("mcp-bridge: %s: URI does not match pattern: %s", cfg.Name, uri)
		return
	}

	// Read the resource content.
	contents, err := server.ReadResource(uri)
	if err != nil {
		log.Printf("mcp-bridge: %s: resources/read failed for %s: %v", cfg.Name, uri, err)
		return
	}

	// Build and publish envelope.
	envelope, err := BuildEnvelope(cfg, uri, contents)
	if err != nil {
		log.Printf("mcp-bridge: %s: envelope construction failed: %v", cfg.Name, err)
		return
	}

	if err := envelope.Validate(); err != nil {
		log.Printf("mcp-bridge: %s: invalid envelope: %v", cfg.Name, err)
		return
	}

	if err := b.client.Publish(envelope); err != nil {
		log.Printf("mcp-bridge: %s: publish failed: %v", cfg.Name, err)
		return
	}

	log.Printf("mcp-bridge: %s: published to %s", cfg.Name, envelope.Topic)
}

func (b *Bridge) monitor(s *ManagedServer, cfg *ServerConfig) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		// Wait for exit.
		err := s.WaitForExit()

		select {
		case <-b.stopCh:
			return
		default:
		}

		log.Printf("mcp-bridge: %s: process exited: %v", cfg.Name, err)

		// Restart with exponential backoff.
		for {
			select {
			case <-b.stopCh:
				return
			case <-time.After(backoff):
			}

			log.Printf("mcp-bridge: %s: restarting (backoff=%v)", cfg.Name, backoff)
			newServer := NewManagedServer(*cfg, b.makeHandler(cfg))
			if err := newServer.Start(); err != nil {
				log.Printf("mcp-bridge: %s: restart failed: %v", cfg.Name, err)
				backoff = min(backoff*2, maxBackoff)
				continue
			}

			// Replace the old server.
			b.mu.Lock()
			for i, existing := range b.servers {
				if existing == s {
					b.servers[i] = newServer
					break
				}
			}
			b.mu.Unlock()

			s = newServer
			backoff = time.Second
			break
		}
	}
}
