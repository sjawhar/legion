package main

// A TCP relay that holds every chunk for half the round trip in each direction, in order, so a
// Postgres round trip costs about rtt more, as it does through the Cloud SQL proxy. Same design
// as internal/dispatch/outbox/throughput_test.go startLatencyProxy. In a gVisor sandbox a short
// sleep oversleeps: -rtt 1ms measured about 4 ms added per round trip.

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net"
	"time"
)

func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:15433", "listen address")
	target := fs.String("target", "127.0.0.1:15432", "target address")
	rtt := fs.Duration("rtt", time.Millisecond, "added round trip")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
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
		go relay(server, client, delay)
		go relay(client, server, delay)
	}
}

func relay(dst, src net.Conn, delay time.Duration) {
	type chunk struct {
		data []byte
		due  time.Time
	}
	queue := make(chan chunk, 4096)
	go func() {
		defer close(queue)
		buf := make([]byte, 64<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				queue <- chunk{data: bytes.Clone(buf[:n]), due: time.Now().Add(delay)}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range queue {
		if d := time.Until(c.due); d > 0 {
			time.Sleep(d)
		}
		if _, err := dst.Write(c.data); err != nil {
			break
		}
	}
	_ = dst.Close()
	_ = src.Close()
}
