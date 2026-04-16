package mcpbridge

import (
	"encoding/json"
	"log"
	"sync"
	"time"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

type Bridge struct {
	cfg     *Config
	client  *bus.Client
	servers []Server
	mu      sync.Mutex
	stopCh  chan struct{}
}

func NewBridge(cfg *Config, client *bus.Client) *Bridge {
	return &Bridge{cfg: cfg, client: client, stopCh: make(chan struct{})}
}

func (b *Bridge) Start() error {
	for i := range b.cfg.Servers {
		serverCfg := b.cfg.Servers[i]
		s := NewServer(serverCfg, b.makeHandler(&serverCfg))
		if err := s.Start(); err != nil {
			for _, started := range b.servers { started.Stop() }
			return err
		}
		b.mu.Lock()
		b.servers = append(b.servers, s)
		b.mu.Unlock()
		go b.monitor(s, &serverCfg)
	}
	return nil
}

func (b *Bridge) Stop() {
	close(b.stopCh)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.servers { s.Stop() }
}

func (b *Bridge) Healthy() bool {
	if err := b.client.Conn.FlushTimeout(3 * time.Second); err != nil { return false }
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.servers {
		if s.State() != StateReady { return false }
	}
	return true
}

func (b *Bridge) makeHandler(cfg *ServerConfig) func(uri string) {
	return func(uri string) { b.handleNotification(cfg, uri) }
}

func (b *Bridge) handleNotification(cfg *ServerConfig, uri string) {
	b.mu.Lock()
	var server Server
	for _, s := range b.servers {
		if s.Name() == cfg.Name { server = s; break }
	}
	b.mu.Unlock()
	if server == nil { log.Printf("mcp-bridge: no server for %s", cfg.Name); return }

	contents, err := server.ReadResource(uri)
	if err != nil { log.Printf("mcp-bridge: %s: resources/read failed for %s: %v", cfg.Name, uri, err); return }

	// Payload routing: parse resource content as JSON array, publish one
	// envelope per item with topic derived from payload fields.
	if len(cfg.PayloadRouting) > 0 {
		b.handlePayloadRouted(cfg, uri, contents)
		return
	}

	// URI routing: topic derived from the notification URI.
	if _, err := cfg.RenderTopic(uri); err != nil { log.Printf("mcp-bridge: %s: URI does not match pattern: %s", cfg.Name, uri); return }
	envelope, err := BuildEnvelope(cfg, uri, contents)
	if err != nil { log.Printf("mcp-bridge: %s: envelope construction failed: %v", cfg.Name, err); return }
	if err := envelope.Validate(); err != nil { log.Printf("mcp-bridge: %s: invalid envelope: %v", cfg.Name, err); return }
	if err := b.client.Publish(envelope); err != nil { log.Printf("mcp-bridge: %s: publish failed: %v", cfg.Name, err); return }
	log.Printf("mcp-bridge: %s: published to %s", cfg.Name, envelope.Topic)
}

func (b *Bridge) handlePayloadRouted(cfg *ServerConfig, uri string, contents []resourceContent) {
	for _, c := range contents {
		if c.Text == "" { continue }
		var items []map[string]interface{}
		if err := json.Unmarshal([]byte(c.Text), &items); err != nil {
			var single map[string]interface{}
			if err := json.Unmarshal([]byte(c.Text), &single); err != nil {
				log.Printf("mcp-bridge: %s: payload not JSON array or object: %v", cfg.Name, err)
				continue
			}
			items = []map[string]interface{}{single}
		}
		for _, item := range items {
			topic, err := cfg.RenderTopicFromPayload(item)
			if err != nil { log.Printf("mcp-bridge: %s: payload routing: %v", cfg.Name, err); continue }
			itemJSON, _ := json.Marshal(item)
			summary := truncateSummary(string(itemJSON), 200)
			envelope := contracts.Envelope{
				EventID: id.New(), Source: cfg.Source, SourceEventID: uri,
				Topic: topic, DedupeKey: buildDedupeKey(cfg.Source, topic, summary),
				IssuedAt: contracts.NowMillis(), PayloadSummary: summary,
				PayloadRef: uri, TraceID: id.New(),
			}
			if err := envelope.Validate(); err != nil { log.Printf("mcp-bridge: %s: invalid envelope: %v", cfg.Name, err); continue }
			if err := b.client.Publish(envelope); err != nil { log.Printf("mcp-bridge: %s: publish to %s failed: %v", cfg.Name, topic, err); continue }
			log.Printf("mcp-bridge: %s: published to %s", cfg.Name, topic)
		}
	}
}

func (b *Bridge) monitor(s Server, cfg *ServerConfig) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		err := s.WaitForExit()
		select {
		case <-b.stopCh: return
		default:
		}
		if httpServer, ok := s.(*HTTPServer); ok && httpServer.Stopped() { return }
		log.Printf("mcp-bridge: %s: exited: %v", cfg.Name, err)
		for {
			select {
			case <-b.stopCh: return
			case <-time.After(backoff):
			}
			log.Printf("mcp-bridge: %s: restarting (backoff=%v)", cfg.Name, backoff)
			newServer := NewServer(*cfg, b.makeHandler(cfg))
			if err := newServer.Start(); err != nil {
				log.Printf("mcp-bridge: %s: restart failed: %v", cfg.Name, err)
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			b.mu.Lock()
			for i, existing := range b.servers {
				if existing == s { b.servers[i] = newServer; break }
			}
			b.mu.Unlock()
			s = newServer
			backoff = time.Second
			break
		}
	}
}
