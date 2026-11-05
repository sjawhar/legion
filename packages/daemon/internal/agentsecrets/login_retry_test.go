package agentsecrets

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// loginRoute is the token login's path, which every cause below names, since do wraps a failed
// call as "broker <method> <path>: …".
const loginRoute = "/v1/launcher-credentials/token"

// deadAddress is an address nothing answers at: a port reserved and given back at once, the
// recipe internal/dispatch/unreachable_test.go uses for a dial the network refuses.
func deadAddress(t *testing.T) string {
	t.Helper()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	address := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatalf("give the port back: %v", err)
	}
	return address
}

// listening accepts every connection and hands it to serve, which decides what a peer that took
// the connection then does with it. It answers its address.
func listening(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return listener.Addr().String()
}

// quietTLSServer is an httptest server over TLS whose handshake failures go nowhere: a row drives
// one on purpose, and the default server writes each to the test's own output. Each function in
// with changes the server's TLS settings before it starts.
func quietTLSServer(t *testing.T, with ...func(*tls.Config)) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{}
	for _, set := range with {
		set(server.TLS)
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// nameServer is a DNS server of the test's own, so a row drives Go's real resolver through a real
// exchange rather than planting a *net.DNSError. It answers every question with rcode, echoing
// the question and carrying no record, which is all the resolver reads to tell "no such host"
// (RCODE 3, NXDOMAIN) from "server misbehaving" (RCODE 2, SERVFAIL).
//
// Every question goes here and none to the machine's own nameservers: the resolver's Dial drops
// the address it was handed and dials this server instead. That address is still one this
// machine's resolv.conf names, and the resolver puts it into the error it returns, so no row
// reads that error whole. Which nameserver a run names, and how many the resolver walks before
// it gives up, are the machine's to decide and not this test's: SERVFAIL sends Go on to the next
// nameserver where NXDOMAIN does not, so on a machine whose resolv.conf names two, this server
// answers both of a row's questions and the error names whichever the resolver asked with last.
//
// It answers over TCP as well as UDP, because a resolv.conf carrying `options use-vc` asks every
// question over TCP, and a port that only reads packets would refuse those rows their exchange.
//
// What a row does read is that this server answered at all, which is what makes the failure the
// row reads this exchange's and not the network's. That is also what a machine whose
// /etc/nsswitch.conf resolves host names out of /etc/hosts alone trips: such a run reaches no
// nameserver at all, and the row says so rather than reading a name the machine resolved.
func nameServer(t *testing.T, rcode byte) *net.Resolver {
	t.Helper()
	var answered atomic.Int64
	answerTo := func(question []byte) []byte {
		answer := make([]byte, len(question))
		copy(answer, question)
		answer[2] = 0x81                  // an answer to a question that asked for recursion
		answer[3] = 0x80 | (rcode & 0x0f) // recursion available, and this server's verdict
		clear(answer[6:12])               // no answer, authority or additional record
		answered.Add(1)
		return answer
	}
	packets, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for DNS questions over UDP: %v", err)
	}
	t.Cleanup(func() { packets.Close() })
	stream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for DNS questions over TCP: %v", err)
	}
	t.Cleanup(func() { stream.Close() })
	t.Cleanup(func() {
		if answered.Load() == 0 {
			t.Error("this row's own nameserver answered no question, so its failure is not the one it drove")
		}
	})
	go func() {
		question := make([]byte, 512)
		for {
			n, from, err := packets.ReadFrom(question)
			if err != nil {
				return
			}
			if n < 12 {
				continue
			}
			packets.WriteTo(answerTo(question[:n]), from)
		}
	}()
	go func() {
		for {
			conn, err := stream.Accept()
			if err != nil {
				return
			}
			// Over TCP each message is its own length in two bytes, then the message.
			go func() {
				defer conn.Close()
				for {
					var size [2]byte
					if _, err := io.ReadFull(conn, size[:]); err != nil {
						return
					}
					question := make([]byte, int(size[0])<<8|int(size[1]))
					if _, err := io.ReadFull(conn, question); err != nil || len(question) < 12 {
						return
					}
					if _, err := conn.Write(append(size[:], answerTo(question)...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			address := packets.LocalAddr().String()
			if strings.HasPrefix(network, "tcp") {
				address = stream.Addr().String()
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
}

// resolvingWith is an HTTP client whose dials resolve through resolver.
func resolvingWith(t *testing.T, resolver *net.Resolver) *http.Client {
	t.Helper()
	transport := &http.Transport{DialContext: (&net.Dialer{Resolver: resolver}).DialContext}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

// failedCall is how a failed call reads once do has wrapped it, which is what the log line
// carries: the route, then net/http's own wrapper naming the whole URL it posted to, then the
// layer that failed.
func failedCall(brokerURL, failure string) string {
	return fmt.Sprintf("broker POST %s: Post %q: %s", loginRoute, brokerURL+loginRoute, failure)
}

// fixed is a cause a rig knows before the login it drives has run.
func fixed(cause string) func() string { return func() string { return cause } }

// withoutTime drops the timestamp from a log line, so a row can read the line whole.
func withoutTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

// TestEachFailedLoginLogsItsClassItsScheduleAndItsCause drives the nine classes a failed token
// login falls in, each against a real server or a real resolver rather than a planted error, as
// internal/dispatch/unreachable_test.go drives transportFailure's own shapes.
//
// The four long-retry rows are what changed: a TLS handshake that fails and a name that does not
// resolve are faults on the daemon's own side, which asking again in seconds only repeats, so
// they now wait the schedule a refusal waits instead of the pod renewer's. The five short-retry
// rows — a refused connection, a connection reset after it was accepted, a timeout, a DNS failure
// that is not "not found", and a 5xx — are the behaviour this client already had, and are new
// coverage of it rather than proof of a change.
//
// Each row reads the whole log the login wrote, not a line of it: one failed login is one line,
// and a second line per failure would be a regression an operator sees.
func TestEachFailedLoginLogsItsClassItsScheduleAndItsCause(t *testing.T) {
	const (
		shortWait = 5 * time.Second
		longWait  = time.Hour
	)
	// Both waits outlast a row, so the loop's next attempt never writes a second line.
	withUnansweredRetry(t, shortWait, shortWait)
	withLoginRetry(t, longWait, longWait)
	for _, row := range []struct {
		name     string
		class    string
		schedule string
		// rig answers the broker's URL, the client to reach it with, and the whole cause the log
		// line must carry, read once the login has failed.
		rig func(t *testing.T) (brokerURL string, client *http.Client, cause func() string)
		// causeHas replaces cause for the rows that cannot know their cause in full. Every one of
		// its parts must be in the cause the line carries.
		causeHas []string
	}{
		{
			name:     "a TLS certificate that does not verify",
			class:    "daemon-side-fault",
			schedule: "long",
			rig: func(t *testing.T) (string, *http.Client, func() string) {
				// Nothing trusts the certificate httptest mints for itself, as the daemon trusts
				// nothing of a front door whose certificate it was never given.
				server := quietTLSServer(t)
				return server.URL, &http.Client{}, fixed(failedCall(server.URL,
					"tls: failed to verify certificate: x509: certificate signed by unknown authority"))
			},
		},
		{
			name:     "a TLS alert",
			class:    "daemon-side-fault",
			schedule: "long",
			rig: func(t *testing.T) (string, *http.Client, func() string) {
				// A front door that asks for a client certificate the daemon does not hold: the
				// server ends the handshake with its alert, which Go's own server sends as
				// handshake_failure. The exchange is pinned to TLS 1.2, where that alert ends the
				// handshake itself; at TLS 1.3 the server decides after the client's handshake is
				// done, so the client would meet the alert at whichever write or read reaches it
				// first.
				server := quietTLSServer(t, func(c *tls.Config) {
					c.ClientAuth, c.MaxVersion = tls.RequireAnyClientCert, tls.VersionTLS12
				})
				return server.URL, server.Client(), fixed(failedCall(server.URL, "remote error: tls: handshake failure"))
			},
		},
		{
			name:     "an answer that is not TLS",
			class:    "daemon-side-fault",
			schedule: "long",
			rig: func(t *testing.T) (string, *http.Client, func() string) {
				// An https:// broker URL pointed at a port that serves plain HTTP, which only a
				// real connection reveals: readAgentSecrets (internal/config) refuses a scheme
				// that is not https, or http to a loopback host, and cannot see this.
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
				t.Cleanup(server.Close)
				brokerURL := "https://" + strings.TrimPrefix(server.URL, "http://")
				return brokerURL, &http.Client{}, fixed(failedCall(brokerURL,
					"http: server gave HTTP response to HTTPS client"))
			},
		},
		{
			name:     "a name that does not resolve",
			class:    "daemon-side-fault",
			schedule: "long",
			rig: func(t *testing.T) (string, *http.Client, func() string) {
				const brokerURL = "https://broker.invalid"
				return brokerURL, resolvingWith(t, nameServer(t, 3)), nil
			},
			causeHas: []string{
				failedCall("https://broker.invalid", "dial tcp: lookup broker.invalid on "),
				": no such host",
			},
		},
		{
			name:     "a refused connection",
			class:    "broker-unavailable",
			schedule: "short",
			rig: func(t *testing.T) (string, *http.Client, func() string) {
				address := deadAddress(t)
				brokerURL := "http://" + address
				return brokerURL, &http.Client{}, fixed(failedCall(brokerURL,
					"dial tcp "+address+": connect: connection refused"))
			},
		},
		{
			name:     "a connection reset after it was accepted",
			class:    "broker-unavailable",
			schedule: "short",
			rig: func(t *testing.T) (string, *http.Client, func() string) {
				// The request goes in and the peer then resets the connection, as a broker whose
				// process dies mid-exchange does. The cause names the connection's own two
				// addresses, one of them the ephemeral port this client dialed from, so the row
				// reads the layer and not the whole line.
				address := listening(t, func(conn net.Conn) {
					defer conn.Close()
					if _, err := conn.Read(make([]byte, 1)); err != nil {
						return
					}
					if tcp, ok := conn.(*net.TCPConn); ok {
						tcp.SetLinger(0) // close with a reset, not with a FIN
					}
				})
				return "http://" + address, &http.Client{}, nil
			},
			causeHas: []string{"read: connection reset by peer"},
		},
		{
			name:     "a timeout",
			class:    "broker-unavailable",
			schedule: "short",
			rig: func(t *testing.T) (string, *http.Client, func() string) {
				// A front door that takes the connection and never answers, which a pod whose
				// node is gone leaves behind.
				done := t.Context().Done()
				address := listening(t, func(conn net.Conn) {
					<-done
					conn.Close()
				})
				brokerURL := "http://" + address
				return brokerURL, &http.Client{Timeout: 200 * time.Millisecond}, fixed(failedCall(brokerURL,
					"context deadline exceeded (Client.Timeout exceeded while awaiting headers)"))
			},
		},
		{
			name:     `a DNS failure that is not "not found"`,
			class:    "broker-unavailable",
			schedule: "short",
			rig: func(t *testing.T) (string, *http.Client, func() string) {
				const brokerURL = "https://broker.invalid"
				return brokerURL, resolvingWith(t, nameServer(t, 2)), nil
			},
			causeHas: []string{
				failedCall("https://broker.invalid", "dial tcp: lookup broker.invalid on "),
				": server misbehaving",
			},
		},
		{
			name:     "a 5xx",
			class:    "broker-unavailable",
			schedule: "short",
			rig: func(t *testing.T) (string, *http.Client, func() string) {
				// The broker answering a token login 500, as it does while its database is away.
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"code":"INTERNAL","error":"token login failed"}`)
				}))
				t.Cleanup(server.Close)
				return server.URL, server.Client(), fixed("broker answered 500 INTERNAL: token login failed")
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			brokerURL, httpClient, cause := row.rig(t)
			logged := &lockedLog{}
			client := &Client{
				URL: brokerURL, HTTP: httpClient, TokenFile: tokenFile(t, "token"),
				Log: slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{ReplaceAttr: withoutTime})),
			}
			ctx, cancel := context.WithCancel(context.Background())
			err := client.Start(ctx)
			cancel() // before the row reads the log, so the loop cannot write a second line into it
			if err == nil {
				t.Fatal("the login succeeded, want the failure this row drove")
			}
			got := strings.TrimSuffix(logged.String(), "\n")
			t.Logf("the line: %s", got)
			wait := longWait
			if row.schedule == "short" {
				wait = shortWait
			}
			const msg = "agent-secrets: the token login failed; retrying"
			head := fmt.Sprintf("level=ERROR msg=%q class=%s schedule=%s retry_in=%s error=",
				msg, row.class, row.schedule, wait)
			if !strings.HasPrefix(got, head) {
				t.Fatalf("the line\n\t%s\nwants the head\n\t%s", got, head)
			}
			gotCause, err := strconv.Unquote(strings.TrimPrefix(got, head))
			if err != nil {
				t.Fatalf("the line\n\t%s\ncarries an error the handler did not quote: %v", got, err)
			}
			if len(row.causeHas) > 0 {
				for _, part := range row.causeHas {
					if !strings.Contains(gotCause, part) {
						t.Fatalf("the cause\n\t%s\nwants a part naming %q", gotCause, part)
					}
				}
				return
			}
			if want := cause(); gotCause != want {
				t.Fatalf("the cause\n\t%s\nwant\n\t%s", gotCause, want)
			}
		})
	}
}
