package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/mcpbridge"
)

func main() {
	configPath := strings.TrimSpace(os.Getenv("ENVOY_MCP_CONFIG"))
	if configPath == "" {
		log.Fatal("ENVOY_MCP_CONFIG is required")
	}
	if strings.TrimSpace(os.Getenv("ENVOY_MACHINE_ID")) == "" {
		log.Fatal("ENVOY_MACHINE_ID is required")
	}
	natsRaw := strings.TrimSpace(os.Getenv("NATS_URLS"))
	if natsRaw == "" {
		log.Fatal("NATS_URLS is required")
	}
	natsURLs := strings.FieldsFunc(natsRaw, func(r rune) bool { return r == ',' })
	for i, u := range natsURLs {
		natsURLs[i] = strings.TrimSpace(u)
	}

	port := 9012
	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("invalid PORT: %v", err)
		}
		port = p
	}

	// Load and validate config.
	cfg, err := mcpbridge.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Connect to NATS. The bridge publishes what its MCP servers report and owns nothing on the
	// bus, so it never reconciles ENVOY_NOTIFICATIONS, and reaches only this machine's NATS
	// unless ENVOY_ALLOW_REMOTE_NATS=1.
	client, err := bus.Connect(natsURLs)
	if err != nil {
		log.Fatalf("nats: %v", err)
	}
	defer client.Close()

	// Create and start bridge.
	bridge := mcpbridge.NewBridge(cfg, client)
	if err := bridge.Start(); err != nil {
		log.Fatalf("bridge start: %v", err)
	}

	// Health check endpoint.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !bridge.Healthy() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:         ":" + strconv.Itoa(port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Printf("envoy-mcp listening on %d", port)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-sigCh
	log.Printf("envoy-mcp shutting down")
	bridge.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}
