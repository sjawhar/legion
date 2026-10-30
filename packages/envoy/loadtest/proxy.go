package main

// A TCP relay that holds every chunk for half the round trip in each direction, in order, so a
// Postgres round trip costs about rtt more, as it does through the Cloud SQL proxy. Same design
// as internal/dispatch/outbox/throughput_test.go startLatencyProxy. In a gVisor sandbox a short
// sleep oversleeps: -rtt 1ms measured about 4 ms added per round trip.
//
// With -stats it also reads the Postgres wire protocol of every connection (pgwire.go) and serves
// its counts as JSON at http://<stats>/stats (proxyStats). The parse reads the chunks the relay
// already copied, on its own goroutines, so the relay's timing is the same with or without it.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:15433", "listen address")
	target := fs.String("target", "127.0.0.1:15432", "target address")
	rtt := fs.Duration("rtt", time.Millisecond, "added round trip")
	stats := fs.String("stats", "", "loopback address that serves the wire counts at /stats (empty: no wire parsing)")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	var counters *wireCounters
	if *stats != "" {
		statsLn, err := net.Listen("tcp", *stats)
		if err != nil {
			return err
		}
		counters = &wireCounters{}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(counters.snapshot())
		})
		go func() { log.Printf("proxy stats: %v", http.Serve(statsLn, mux)) }()
		log.Printf("proxy stats on http://%s/stats", *stats)
	}
	log.Printf("proxy listening %s -> %s rtt=%s", *listen, *target, *rtt)
	delay := *rtt / 2
	for {
		client, err := ln.Accept()
		if err != nil {
			return err
		}
		server, err := net.Dial("tcp", *target)
		if err != nil {
			log.Printf("proxy: dial %s: %v", *target, err)
			_ = client.Close()
			continue
		}
		// The client's bytes are parsed as they reach the server, after the delay; the server's
		// as they reach the proxy. A version insert's time is thus Postgres's, without the delay.
		var toServer, fromServer *wireTap
		if counters != nil {
			wc := newWireConn(counters)
			toServer, fromServer = newWireTap(wc, wc.frontend), newWireTap(wc, wc.backend)
		}
		go relay(server, client, delay, nil, toServer)
		go relay(client, server, delay, fromServer, nil)
	}
}

// relay copies src to dst, holding each chunk for delay. read sees each chunk as it is read from
// src, written as it has been written to dst; either may be nil. Both get the chunk the queue
// holds, which nothing writes to, and neither blocks.
func relay(dst, src net.Conn, delay time.Duration, read, written *wireTap) {
	type chunk struct {
		data []byte
		due  time.Time
	}
	queue := make(chan chunk, 4096)
	go func() {
		defer close(queue)
		defer read.close()
		buf := make([]byte, 64<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				now := time.Now()
				data := bytes.Clone(buf[:n])
				queue <- chunk{data: data, due: now.Add(delay)}
				read.send(data, now)
			}
			if err != nil {
				return
			}
		}
	}()
	defer written.close()
	for c := range queue {
		if d := time.Until(c.due); d > 0 {
			time.Sleep(d)
		}
		if _, err := dst.Write(c.data); err != nil {
			break
		}
		written.send(c.data, time.Now())
	}
	_ = dst.Close()
	_ = src.Close()
}
