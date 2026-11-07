// packages/envoy/cmd/dev-airlock-stands/load.go
// The `load` subcommand: the load check k9 puts through the airlock proxy, and the plain tunnel
// its baseline goes through. Every mode but "tunnel" opens its connections with a CONNECT to the
// proxy it is given, so the same program measures the airlock and the plain tunnel and the two
// figures are comparable.
//
// It is a subcommand of this binary rather than a program scripts/airlock-check.sh writes out
// and builds, so that CI compiles and vets it. It is still a plain Go client over the standard
// library alone, reading the proxy it CONNECTs to, its trust roots and its header from its own
// flags, which is what makes the figures it prints a Go consumer's.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// loadOptions is what the flags say, and the roots read out of the file --ca names. Every mode
// reads the same set and ignores the ones it has no use for, as the one flag set of the program
// this was does.
type loadOptions struct {
	proxyURL string
	host     string
	caFile   string
	header   string
	rate     int
	seconds  int
	size     int
	gap      int
	listen   string
	to       string
	out      string
	pool     *x509.CertPool
}

// runLoad is `dev-airlock-stands load <mode>`.
func runLoad(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "dev-airlock-stands load: name a mode: load, tunnel, upload, gap or upgrade")
		return exitUsageError
	}
	mode := args[0]
	fs := flag.NewFlagSet("dev-airlock-stands load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o loadOptions
	fs.StringVar(&o.proxyURL, "proxy", "", "the proxy to CONNECT through, credential and all")
	fs.StringVar(&o.host, "host", "", "the bound host, which is reached on port 443")
	fs.StringVar(&o.caFile, "ca", "", "a PEM file of roots to verify the host with")
	fs.StringVar(&o.header, "header", "", "the Authorization header each request carries")
	fs.IntVar(&o.rate, "rate", 50, "connections a second")
	fs.IntVar(&o.seconds, "seconds", 600, "how long to keep opening them")
	fs.IntVar(&o.size, "size", 50<<20, "how many bytes to upload")
	fs.IntVar(&o.gap, "gap", 90, "the gap in seconds between the two events of one answer")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:0", "where the plain tunnel listens")
	fs.StringVar(&o.to, "to", "", "the address the plain tunnel dials, whatever host was asked for")
	fs.StringVar(&o.out, "out", "", "write the JSON result here as well as to stdout")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsageError
	}
	if o.caFile != "" {
		pem, err := os.ReadFile(o.caFile)
		if err != nil {
			fmt.Fprintln(stderr, "dev-airlock-stands load:", err)
			return 1
		}
		o.pool = x509.NewCertPool()
		o.pool.AppendCertsFromPEM(pem)
	}
	// The check stops the tunnel, and a run of its own it has given up on, with a signal, and
	// main's signal.NotifyContext has taken the default disposition away from this process. So
	// the signal ends the run here, as it ended the program this was, which handled none.
	go func() {
		<-ctx.Done()
		os.Exit(1)
	}()
	switch mode {
	case "load":
		o.load(stdout)
	case "tunnel":
		return o.tunnel(stdout, stderr)
	case "upload":
		o.upload(stdout)
	case "gap":
		o.gapMode(stdout)
	case "upgrade":
		o.upgrade(stdout)
	default:
		fmt.Fprintf(stderr, "dev-airlock-stands load: %s is no mode of this program\n", mode)
		return exitUsageError
	}
	return 0
}

// say writes one result as JSON, to the file --out names as well as to stdout.
func (o *loadOptions) say(stdout io.Writer, result map[string]any) {
	text, _ := json.MarshalIndent(result, "", " ")
	if o.out != "" {
		_ = os.WriteFile(o.out, text, 0o600)
	}
	fmt.Fprintln(stdout, string(text))
}

// connect opens one tunnel to the bound host through the proxy and answers the connection the
// CONNECT left behind.
func (o *loadOptions) connect(timeout time.Duration) (net.Conn, error) {
	u, err := url.Parse(o.proxyURL)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		return nil, err
	}
	target := o.host + ":443"
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if u.User != nil {
		password, _ := u.User.Password()
		credential := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
		request += "Proxy-Authorization: Basic " + credential + "\r\n"
	}
	request += "\r\n"
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte(request)); err != nil {
		conn.Close()
		return nil, err
	}
	answer, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if answer.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(answer.Body, 300))
		conn.Close()
		return nil, fmt.Errorf("the CONNECT answered %d %s", answer.StatusCode, strings.TrimSpace(string(body)))
	}
	return conn, nil
}

// speak is one tunnel with TLS to the bound host on top of it.
func (o *loadOptions) speak(timeout time.Duration) (net.Conn, *bufio.Reader, error) {
	raw, err := o.connect(timeout)
	if err != nil {
		return nil, nil, err
	}
	conn := tls.Client(raw, &tls.Config{ServerName: o.host, RootCAs: o.pool})
	_ = raw.SetDeadline(time.Now().Add(timeout))
	if err := conn.Handshake(); err != nil {
		raw.Close()
		return nil, nil, err
	}
	return conn, bufio.NewReader(conn), nil
}

// ask writes one HTTP/1.1 request on an open connection and reads its answer.
func (o *loadOptions) ask(conn net.Conn, reader *bufio.Reader, method, path string, body []byte, extra string) (*http.Response, error) {
	request := method + " " + path + " HTTP/1.1\r\nHost: " + o.host + "\r\n"
	if o.header != "" {
		request += "Authorization: Bearer " + o.header + "\r\n"
	}
	request += extra
	request += fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := conn.Write(append([]byte(request), body...)); err != nil {
		return nil, err
	}
	return http.ReadResponse(reader, nil)
}

// one call: a connection of its own, a request on it and the whole answer read.
func (o *loadOptions) call(timeout time.Duration) error {
	conn, reader, err := o.speak(timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	answer, err := o.ask(conn, reader, "GET", "/ok", nil, "")
	if err != nil {
		return err
	}
	defer answer.Body.Close()
	if _, err := io.Copy(io.Discard, answer.Body); err != nil {
		return err
	}
	if answer.StatusCode != http.StatusOK {
		return fmt.Errorf("the call answered %d", answer.StatusCode)
	}
	return nil
}

// load opens rate connections a second for seconds seconds and answers what each one took.
func (o *loadOptions) load(stdout io.Writer) {
	var mu sync.Mutex
	took := make([]float64, 0, o.rate*o.seconds)
	failures := map[string]int{}
	var wg sync.WaitGroup
	limit := make(chan struct{}, 400)
	ticker := time.NewTicker(time.Second / time.Duration(o.rate))
	defer ticker.Stop()
	deadline := time.Now().Add(time.Duration(o.seconds) * time.Second)
	started := time.Now()
	for time.Now().Before(deadline) {
		<-ticker.C
		limit <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-limit }()
			at := time.Now()
			err := o.call(30 * time.Second)
			ms := float64(time.Since(at).Microseconds()) / 1000
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				reason := err.Error()
				if len(reason) > 120 {
					reason = reason[:120]
				}
				failures[reason]++
				return
			}
			took = append(took, ms)
		}()
	}
	wg.Wait()
	sort.Float64s(took)
	at := func(q float64) float64 {
		if len(took) == 0 {
			return -1
		}
		i := int(q * float64(len(took)-1))
		return took[i]
	}
	total := 0
	for _, n := range failures {
		total += n
	}
	o.say(stdout, map[string]any{
		"calls": len(took) + total, "failed": total, "failures": failures,
		"wall_seconds": time.Since(started).Seconds(),
		"p50_ms":       at(0.5), "p90_ms": at(0.9), "p99_ms": at(0.99), "max_ms": at(1),
	})
}

// tunnel is the baseline: a CONNECT proxy that reads no byte of what it carries, so what the
// airlock adds is the difference between the same calls through this and through it.
func (o *loadOptions) tunnel(stdout, stderr io.Writer) int {
	listener, err := net.Listen("tcp", o.listen)
	if err != nil {
		fmt.Fprintln(stderr, "dev-airlock-stands load:", err)
		return 1
	}
	fmt.Fprintln(stdout, listener.Addr().String())
	if flusher, ok := stdout.(interface{ Sync() error }); ok {
		_ = flusher.Sync()
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return 0
		}
		go func() {
			defer conn.Close()
			reader := bufio.NewReader(conn)
			request, err := http.ReadRequest(reader)
			if err != nil || request.Method != http.MethodConnect {
				return
			}
			// The bound hosts are .test names no resolver answers, so the tunnel is told
			// where to dial rather than resolving what the CONNECT named.
			target := request.Host
			if o.to != "" {
				target = o.to
			}
			upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
				return
			}
			defer upstream.Close()
			if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
				return
			}
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(upstream, reader); done <- struct{}{} }()
			go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
			<-done
		}()
	}
}

func (o *loadOptions) upload(stdout io.Writer) {
	body := make([]byte, o.size)
	// Random bytes at the head, so the body is not one long run of zeroes that a transport
	// could compress away, and the rest left as it is: the check sends 50 MiB and what is
	// measured is the bytes on the wire rather than their content. The prefix is bounded by
	// the body, since --size may name fewer bytes than it.
	_, _ = rand.Read(body[:min(len(body), 4096)])
	at := time.Now()
	conn, reader, err := o.speak(60 * time.Second)
	if err != nil {
		o.say(stdout, map[string]any{"error": err.Error()})
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
	answer, err := o.ask(conn, reader, "POST", "/upload", body, "")
	if err != nil {
		o.say(stdout, map[string]any{"error": err.Error(), "seconds": time.Since(at).Seconds()})
		return
	}
	read, _ := io.Copy(io.Discard, answer.Body)
	o.say(stdout, map[string]any{"status": answer.StatusCode, "sent": len(body), "answer_bytes": read,
		"seconds": time.Since(at).Seconds()})
}

// gapMode is one answer with a long gap in the middle of it, as the check's streamed answer is:
// two events on one connection with the gap between them, which is what the idle limit bounds.
func (o *loadOptions) gapMode(stdout io.Writer) {
	conn, reader, err := o.speak(30 * time.Second)
	if err != nil {
		o.say(stdout, map[string]any{"error": err.Error()})
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Duration(o.gap+120) * time.Second))
	first, err := o.ask(conn, reader, "GET", "/first-event", nil, "")
	if err != nil {
		o.say(stdout, map[string]any{"error": "the first event: " + err.Error()})
		return
	}
	_, _ = io.Copy(io.Discard, first.Body)
	time.Sleep(time.Duration(o.gap) * time.Second)
	second, err := o.ask(conn, reader, "GET", "/second-event", nil, "")
	if err != nil {
		o.say(stdout, map[string]any{"first": first.StatusCode, "second_error": err.Error(), "gap": o.gap})
		return
	}
	_, _ = io.Copy(io.Discard, second.Body)
	o.say(stdout, map[string]any{"first": first.StatusCode, "second": second.StatusCode, "gap": o.gap})
}

// upgrade is one WebSocket upgrade and, when the service answers 101, one frame each way.
func (o *loadOptions) upgrade(stdout io.Writer) {
	conn, reader, err := o.speak(30 * time.Second)
	if err != nil {
		o.say(stdout, map[string]any{"error": err.Error()})
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	extra := "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	answer, err := o.ask(conn, reader, "GET", "/socket", nil, extra)
	if err != nil {
		o.say(stdout, map[string]any{"error": err.Error()})
		return
	}
	result := map[string]any{"status": answer.StatusCode, "upgrade": answer.Header.Get("Upgrade")}
	if answer.StatusCode == http.StatusSwitchingProtocols {
		if _, err := conn.Write([]byte("a frame from the command\n")); err != nil {
			result["frames"] = "the command could not write: " + err.Error()
		} else if line, err := reader.ReadString('\n'); err != nil {
			result["frames"] = "no frame came back: " + err.Error()
		} else {
			result["frames"] = strings.TrimSpace(line)
		}
	} else {
		body, _ := io.ReadAll(io.LimitReader(answer.Body, 200))
		result["body"] = strings.TrimSpace(string(body))
	}
	o.say(stdout, result)
}
