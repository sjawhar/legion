// Command natstail writes Dispatch and Envoy envelopes from a NATS subject to stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/config"
)

func main() {
	flags := flag.NewFlagSet("natstail", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	subject := flags.String("subject", "", "NATS subject pattern to subscribe to")
	count := flags.Int("count", 1, "number of envelopes to print")
	timeout := flags.Duration("timeout", 30*time.Second, "maximum time to wait for envelopes")
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}
	if *subject == "" || *count <= 0 || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "natstail: -subject, a positive -count, and a positive -timeout are required")
		os.Exit(2)
	}

	cfg, err := config.Load(config.LoadOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "natstail: load envoy config: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := tail(ctx, cfg.NatsURLs, *subject, *count, os.Stdout); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(os.Stderr, "natstail: timed out waiting for envelopes")
		} else {
			fmt.Fprintf(os.Stderr, "natstail: %v\n", err)
		}
		os.Exit(1)
	}
}

// tail writes count envelopes received on subject as one JSON line each.
func tail(ctx context.Context, urls []string, subject string, count int, out io.Writer) error {
	if count <= 0 {
		return fmt.Errorf("count must be positive")
	}
	client, err := bus.Connect(urls)
	if err != nil {
		return fmt.Errorf("connect NATS: %w", err)
	}
	defer client.Close()

	messages := make(chan []byte)
	done := make(chan struct{})
	subscription, err := client.SubscribeCore(subject, func(message *natsgo.Msg) {
		select {
		case messages <- message.Data:
		case <-done:
		}
	})
	if err != nil {
		return fmt.Errorf("subscribe %q: %w", subject, err)
	}
	defer subscription.Unsubscribe()
	defer close(done)
	if err := client.Conn.Flush(); err != nil {
		return fmt.Errorf("flush subscription: %w", err)
	}

	encoder := json.NewEncoder(out)
	for range count {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case data := <-messages:
			if err := encoder.Encode(json.RawMessage(data)); err != nil {
				return fmt.Errorf("write envelope: %w", err)
			}
		}
	}
	return nil
}
