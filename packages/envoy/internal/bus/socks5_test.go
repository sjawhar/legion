package bus_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// unresolvable is the server every proxied case names: a host name that resolves nowhere, so only a
// proxy that ignores the name reaches the server.
const unresolvable = "nats.example.test:4222"

// fakeSOCKS5 is a SOCKS5 server (RFC 1928: no authentication, CONNECT only) that serves every
// request from one NATS server, whatever address the request names, so a name that resolves
// nowhere is served. It records each request's address type and address, counts every connection
// it accepts, and dropAll closes every connection it relays, so nats.go reconnects in place. With
// reply set (startRefusingSOCKS5) it answers every request with that reply code and relays nothing.
type fakeSOCKS5 struct {
	addr     string
	target   string
	reply    byte
	listener net.Listener
	accepted atomic.Int64

	mu       sync.Mutex
	requests []socksRequest
	relayed  []net.Conn
}

// socksRequest is one CONNECT a fakeSOCKS5 received: its address type (0x01 an IPv4 address, 0x03
// a domain name, 0x04 an IPv6 address) and the address it named, as host:port.
type socksRequest struct {
	addressType byte
	address     string
}

// startFakeSOCKS5 serves on a loopback port, relaying every request to target (host:port), until
// the test ends.
func startFakeSOCKS5(t *testing.T, target string) *fakeSOCKS5 {
	t.Helper()
	return startSOCKS5(t, &fakeSOCKS5{target: target})
}

// startRefusingSOCKS5 serves on a loopback port, answering every request with reply (RFC 1928's
// reply field: 0x04 host unreachable, 0x05 connection refused), until the test ends.
func startRefusingSOCKS5(t *testing.T, reply byte) *fakeSOCKS5 {
	t.Helper()
	return startSOCKS5(t, &fakeSOCKS5{reply: reply})
}

func startSOCKS5(t *testing.T, fake *fakeSOCKS5) *fakeSOCKS5 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the SOCKS5 fake: %v", err)
	}
	fake.addr, fake.listener = listener.Addr().String(), listener
	go fake.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		fake.dropAll()
	})
	return fake
}

func (f *fakeSOCKS5) serve() {
	for {
		client, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.accepted.Add(1)
		go f.handle(client)
	}
}

// handle reads the request, dials the target whatever the request named, answers success with a
// zero bound address, then copies bytes both ways until either side closes.
func (f *fakeSOCKS5) handle(client net.Conn) {
	// A client that does not speak SOCKS5 first (a NATS client dialling the fake directly waits for
	// the server's INFO) is dropped rather than held.
	_ = client.SetDeadline(time.Now().Add(time.Second))
	request, err := readSOCKS5Connect(client)
	if err != nil {
		_ = client.Close()
		return
	}
	_ = client.SetDeadline(time.Time{})
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	if f.reply != 0 {
		_, _ = client.Write([]byte{0x05, f.reply, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		_ = client.Close()
		return
	}
	server, err := net.Dial("tcp", f.target)
	if err != nil {
		_ = client.Close()
		return
	}
	if _, err := client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		_ = client.Close()
		_ = server.Close()
		return
	}
	f.mu.Lock()
	f.relayed = append(f.relayed, client, server)
	f.mu.Unlock()
	go func() {
		_, _ = io.Copy(server, client)
		_ = server.Close()
		_ = client.Close()
	}()
	_, _ = io.Copy(client, server)
	_ = client.Close()
	_ = server.Close()
}

// readSOCKS5Connect reads a client's greeting (05 <n> <methods>), answers that it needs no
// authentication (05 00), and reads its CONNECT request (05 01 00 <type> <address> <port>).
func readSOCKS5Connect(conn net.Conn) (socksRequest, error) {
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return socksRequest{}, err
	}
	if greeting[0] != 0x05 {
		return socksRequest{}, fmt.Errorf("greeting of SOCKS version %d", greeting[0])
	}
	if _, err := io.ReadFull(conn, make([]byte, greeting[1])); err != nil {
		return socksRequest{}, err
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return socksRequest{}, err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return socksRequest{}, err
	}
	if header[0] != 0x05 || header[1] != 0x01 {
		return socksRequest{}, fmt.Errorf("request %x is not a SOCKS5 CONNECT", header)
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
			return socksRequest{}, err
		}
		host = net.IP(ip).String()
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return socksRequest{}, err
		}
		name := make([]byte, length[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return socksRequest{}, err
		}
		host = string(name)
	default:
		return socksRequest{}, fmt.Errorf("address type %#x", header[3])
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return socksRequest{}, err
	}
	return socksRequest{addressType: header[3], address: net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port))))}, nil
}

// connects returns every request the fake has received, oldest first.
func (f *fakeSOCKS5) connects() []socksRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// dropAll closes every connection the fake relays.
func (f *fakeSOCKS5) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, conn := range f.relayed {
		_ = conn.Close()
	}
	f.relayed = nil
}

// waitForConnects waits for the fake to have received n requests, every one of them want.
func (f *fakeSOCKS5) waitForConnects(t *testing.T, n int, want socksRequest) {
	t.Helper()
	waitFor(t, 15*time.Second, fmt.Sprintf("%d requests at the proxy", n), func() bool { return len(f.connects()) >= n })
	requireConnects(t, f, slices.Repeat([]socksRequest{want}, n))
}

func requireConnects(t *testing.T, fake *fakeSOCKS5, want []socksRequest) {
	t.Helper()
	if got := fake.connects(); !slices.Equal(got, want) {
		t.Fatalf("the proxy received %+v, want %+v", got, want)
	}
}

// lookupOf is a connect's environment (WithEnvironment, Dial's environment) holding values and
// nothing else.
func lookupOf(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, set := values[key]
		return value, set
	}
}

// proxied is the environment of a run that sends NATS through the proxy at address and says it
// means another machine's NATS, as every deployment that sets a proxy does.
func proxied(address string) func(string) (string, bool) {
	return lookupOf(map[string]string{bus.SOCKS5ProxyEnvVar: address, bus.AllowRemoteEnvVar: "1"})
}

// hostOf is the host:port of the server uri names.
func hostOf(t *testing.T, uri string) string {
	t.Helper()
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("read the test server's URL %q: %v", uri, err)
	}
	return parsed.Host
}

// withHost is uri, with its account's user, naming host (host:port) as its server.
func withHost(t *testing.T, uri, host string) string {
	t.Helper()
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("read the test server's URL %q: %v", uri, err)
	}
	parsed.Host = host
	return parsed.String()
}

// clientConnect is bus.Connect or bus.ConnectOwningStream.
type clientConnect func([]string, ...bus.ConnectOption) (*bus.Client, error)

// Every connection the bus makes with a proxy set goes through the proxy, which is handed the
// server's host name as the URL spells it, unresolved: the first, nats.go's reconnect in place, and
// for a Client the replacement its recovery dials once the connection is closed. The name resolves
// nowhere, so a connection that went around the proxy would reach nothing.
func TestSOCKS5ProxyCarriesEveryConnectionWithTheHostUnresolved(t *testing.T) {
	want := socksRequest{addressType: 0x03, address: unresolvable}
	for name, connect := range map[string]clientConnect{"Connect": bus.Connect, "ConnectOwningStream": bus.ConnectOwningStream} {
		t.Run(name, func(t *testing.T) {
			uri := testnats.URL(t)
			fake := startFakeSOCKS5(t, hostOf(t, uri))
			client, err := connect([]string{withHost(t, uri, unresolvable)}, bus.WithEnvironment(proxied(fake.addr)))
			if err != nil {
				t.Fatalf("connect through the proxy: %v", err)
			}
			t.Cleanup(client.Close)
			requireConnects(t, fake, []socksRequest{want})

			const topic = "notifications.role.socks5"
			received := make(chan []byte, 64)
			if _, err := client.SubscribeCore(topic, func(msg *natsgo.Msg) { received <- msg.Data }); err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			roundTrip := func(marker string) {
				t.Helper()
				envelope := roleEnvelope(topic)
				envelope.EventID = marker
				waitFor(t, 15*time.Second, "a core publish to come back through the proxy "+marker, func() bool {
					if err := client.PublishCore(envelope); err != nil {
						return false
					}
					deadline := time.After(250 * time.Millisecond)
					for {
						select {
						case data := <-received:
							if bytes.Contains(data, []byte(marker)) {
								return true
							}
						case <-deadline:
							return false
						}
					}
				})
			}
			roundTrip("after-the-first-connect")

			fake.dropAll()
			fake.waitForConnects(t, 2, want)
			roundTrip("after-the-reconnect-in-place")

			// A connection nats.go has closed is replaced by the client's recovery, which dials
			// with the same proxy.
			client.Conn.Close()
			fake.waitForConnects(t, 3, want)
			waitFor(t, 15*time.Second, "the replacement connection", client.Connected)
			roundTrip("after-the-replacement")
		})
	}

	t.Run("Dial", func(t *testing.T) {
		uri := testnats.URL(t)
		fake := startFakeSOCKS5(t, hostOf(t, uri))
		conn, err := bus.Dial("socks5-test", []string{withHost(t, uri, unresolvable)}, proxied(fake.addr))
		if err != nil {
			t.Fatalf("dial through the proxy: %v", err)
		}
		t.Cleanup(conn.Close)
		requireConnects(t, fake, []socksRequest{want})

		const subject = "socks5.dial"
		sub, err := conn.SubscribeSync(subject)
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		roundTrip := func(marker string) {
			t.Helper()
			waitFor(t, 15*time.Second, "a publish to come back through the proxy "+marker, func() bool {
				if err := conn.Publish(subject, []byte(marker)); err != nil {
					return false
				}
				for {
					msg, err := sub.NextMsg(250 * time.Millisecond)
					if err != nil {
						return false
					}
					if string(msg.Data) == marker {
						return true
					}
				}
			})
		}
		roundTrip("after-the-first-connect")

		fake.dropAll()
		fake.waitForConnects(t, 2, want)
		roundTrip("after-the-reconnect-in-place")
	})
}

// Handed a name this machine resolves, the bus still hands the proxy the name, never an address:
// nats.go resolves a URL's host itself unless told not to, and the address it found would reach
// nothing through a proxy that knows names this machine's resolver does not.
func TestSOCKS5ProxyIsHandedANameTheResolverKnows(t *testing.T) {
	uri := testnats.URL(t)
	server := hostOf(t, uri)
	_, port, err := net.SplitHostPort(server)
	if err != nil {
		t.Fatalf("read the test server's port from %s: %v", server, err)
	}
	fake := startFakeSOCKS5(t, server)
	client, err := bus.Connect([]string{withHost(t, uri, net.JoinHostPort("localhost", port))}, bus.WithEnvironment(proxied(fake.addr)))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	requireConnects(t, fake, []socksRequest{{addressType: 0x03, address: "localhost:" + port}})
}

// A connect handed an environment that does not set the proxy dials the server directly, whatever
// the process environment names; one left on the process environment goes through the proxy it
// names. The connect's own lookup decides, never the process environment beside it.
func TestNoProxyIsDialedWhenTheSettingIsUnset(t *testing.T) {
	uri := testnats.URL(t)
	server := hostOf(t, uri)
	_, serverPort, err := net.SplitHostPort(server)
	if err != nil {
		t.Fatalf("read the test server's port from %s: %v", server, err)
	}
	fake := startFakeSOCKS5(t, server)
	t.Setenv(bus.SOCKS5ProxyEnvVar, fake.addr)

	direct, err := bus.Connect([]string{uri}, bus.WithEnvironment(lookupOf(nil)))
	if err != nil {
		t.Fatalf("connect with no proxy handed: %v", err)
	}
	t.Cleanup(direct.Close)
	if _, port, err := net.SplitHostPort(direct.Conn.ConnectedAddr()); err != nil || port != serverPort {
		t.Fatalf("connected to %s (%v), want the NATS server's own port %s", direct.Conn.ConnectedAddr(), err, serverPort)
	}
	if accepted := fake.accepted.Load(); accepted != 0 {
		t.Fatalf("the proxy the process environment names accepted %d connections, want none", accepted)
	}

	// The same connect on the process environment goes through the proxy it names.
	t.Setenv(bus.AllowRemoteEnvVar, "1")
	throughProxy, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect on the process environment: %v", err)
	}
	t.Cleanup(throughProxy.Close)
	if got := throughProxy.Conn.ConnectedAddr(); got != fake.addr {
		t.Fatalf("connected to %s, want the proxy %s", got, fake.addr)
	}
	host, _, _ := net.SplitHostPort(server)
	addressType := byte(0x03)
	if net.ParseIP(host) != nil {
		addressType = 0x01
	}
	requireConnects(t, fake, []socksRequest{{addressType: addressType, address: server}})
}

// A connect through the proxy that fails names the setting and the proxy's address, whatever the
// failure: a proxy nothing listens on, which nats.go answers with its bare ErrNoServers ("nats: no
// servers available for connection") so the dial's own error is named too; a proxy that closes the
// connection without answering; and a proxy that answers the request "host unreachable". nats.go
// hands back the last two as the dial's error, which names the proxy's address and not the
// setting. Each connect makes the bus's ten attempts a second apart, so each subtest takes about
// ten seconds; they run in parallel. Every proxy is a loopback address: nothing is looked up.
func TestAFailedConnectThroughAProxyNamesIt(t *testing.T) {
	urls := []string{"nats://" + unresolvable}
	silent := startFakeSOCKS5(t, "127.0.0.1:1")
	unreachable := startRefusingSOCKS5(t, 0x04)
	for _, proxy := range []struct {
		name, address string
		also          []string
	}{
		{"nothing listens", "127.0.0.1:1", []string{"connection refused"}},
		{"closes without answering", silent.addr, nil},
		{"answers host unreachable", unreachable.addr, []string{"host unreachable"}},
	} {
		for name, connect := range map[string]func(string) error{
			"Connect": func(address string) error {
				client, err := bus.Connect(urls, bus.WithEnvironment(proxied(address)))
				if err == nil {
					client.Close()
				}
				return err
			},
			"Dial": func(address string) error {
				conn, err := bus.Dial("socks5-test", urls, proxied(address))
				if err == nil {
					conn.Close()
				}
				return err
			},
		} {
			t.Run(proxy.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				err := connect(proxy.address)
				if err == nil {
					t.Fatalf("%s through a proxy that %s succeeded", name, proxy.name)
				}
				for _, want := range append([]string{bus.SOCKS5ProxyEnvVar, proxy.address}, proxy.also...) {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s error %q does not name %q", name, err, want)
					}
				}
			})
		}
	}
}

// A publish on a closed connection through a dead proxy fails when its own 5 s deadline ends,
// before the replacement dial has made its ten attempts, and still names the setting and the
// proxy's address: an operator reads a publish's error first.
func TestAPublishWhoseDeadlineEndsFirstNamesTheProxy(t *testing.T) {
	uri := testnats.URL(t)
	fake := startFakeSOCKS5(t, hostOf(t, uri))
	client, err := bus.Connect([]string{withHost(t, uri, unresolvable)}, bus.WithEnvironment(proxied(fake.addr)))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	// The proxy goes away, then the connection: the publish dials a replacement, and every attempt
	// is refused.
	_ = fake.listener.Close()
	fake.dropAll()
	client.Conn.Close()
	started := time.Now()
	err = client.PublishCore(roleEnvelope("notifications.role.socks5"))
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a publish through a dead proxy succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("publish error %q after %s is not its deadline's: the replacement dial ran out of attempts first", err, elapsed)
	}
	for _, want := range []string{bus.SOCKS5ProxyEnvVar, fake.addr} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("publish error %q does not name %q", err, want)
		}
	}
	if elapsed > 7*time.Second {
		t.Errorf("the publish failed after %s, past its 5 s deadline", elapsed)
	}
}

// A proxy address that is not host:port is refused at once, before anything dials, naming the
// setting: a URL, an address with no port, one with no host, ports that are not a port number or
// out of range, whitespace around the value or inside it, an IPv6 address with a zone (every proxy
// a deployment names is loopback, and a zone is free text), a host that is neither an IP address
// nor a DNS name (userinfo, a path, a control byte, an empty label, a label or a name too long),
// and a value set but empty. Any dial would take the bus's ten attempts a second apart, and a
// failed dial through a proxy names the setting too, so "at once" is under a second. The server URL
// is the fake itself, so a connect that ignored the setting and dialled directly would reach it.
func TestAMalformedProxyIsRefusedBeforeAnyDial(t *testing.T) {
	fake := startFakeSOCKS5(t, "127.0.0.1:1")
	urls := []string{"nats://" + fake.addr}
	connects := map[string]func(func(string) (string, bool)) error{
		"Connect": func(environment func(string) (string, bool)) error {
			client, err := bus.Connect(urls, bus.WithEnvironment(environment))
			if err == nil {
				client.Close()
			}
			return err
		},
		"Dial": func(environment func(string) (string, bool)) error {
			conn, err := bus.Dial("socks5-test", urls, environment)
			if err == nil {
				conn.Close()
			}
			return err
		},
	}
	malformed := []string{
		"socks5://127.0.0.1:1055", "127.0.0.1", ":1055", "127.0.0.1:socks", "127.0.0.1:+1",
		"127.0.0.1:0", "127.0.0.1:65536", "",
		" 127.0.0.1:1055", "\t127.0.0.1:1055", "127.0.0.1:1055 ", "127.0.0.1:1055\n", "127.0.0.1 :1055",
		"[fe80::1%eth0]:1055", "[::1%u:hunter2@x]:1055", "[fe80::1%a b]:1055",
		"a\nb:1055", "a\x00b:1055", "user@127.0.0.1:1055", "proxy/path:1055", "proxy..example:1055",
		strings.Repeat("a", 64) + ".example:1055", strings.Repeat("abcdefg.", 32) + "example:1055",
	}
	for _, value := range malformed {
		for name, connect := range connects {
			started := time.Now()
			err := connect(proxied(value))
			if err == nil || !strings.Contains(err.Error(), bus.SOCKS5ProxyEnvVar) {
				t.Fatalf("%s with %s=%q: %v, want a refusal naming %s", name, bus.SOCKS5ProxyEnvVar, value, err, bus.SOCKS5ProxyEnvVar)
			}
			if elapsed := time.Since(started); elapsed >= time.Second {
				t.Fatalf("%s with %s=%q took %s to fail (%v): it dialled", name, bus.SOCKS5ProxyEnvVar, value, elapsed, err)
			}
		}
	}
	for _, value := range []string{"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:99999999999999999999999"} {
		if err := bus.CheckSOCKS5Proxy(value); err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Errorf("CheckSOCKS5Proxy(%q) = %v, want the port refused as out of range", value, err)
		}
	}
	if accepted := fake.accepted.Load(); accepted != 0 {
		t.Fatalf("the server URL accepted %d connections, want none", accepted)
	}
}

// A host:port whose host is an IP address without a zone, or a DNS name as Go's resolver reads one
// (underscores allowed, as in a Compose service name), passes the check, as does a port written
// with leading zeros or the highest port.
func TestAWellFormedProxyIsAccepted(t *testing.T) {
	for _, value := range []string{
		"127.0.0.1:1055", "[::1]:1055", "localhost:1055", "localhost.:1055",
		"127.0.0.1:01055", "127.0.0.1:65535", "proxy.example.test:1080", "tailscale-sidecar:1055",
		"proxy_sidecar:1055", strings.Repeat("a", 63) + ".example:1055",
		strings.Repeat("abcdefg.", 30) + "example.test:1055",
	} {
		if err := bus.CheckSOCKS5Proxy(value); err != nil {
			t.Errorf("CheckSOCKS5Proxy(%q) = %v, want it accepted", value, err)
		}
	}
}

// A proxy value can carry credentials (an operator who writes it as a URL, as ALL_PROXY settings
// are written, or hides them in an IPv6 zone), and a refusal ends up in the process's log, so no
// refusal prints the value: not the remote-reach refusal of a run that has not opted in, and not
// the address check's.
func TestARefusalNeverPrintsTheProxyValue(t *testing.T) {
	const secret = "hunter2"
	for _, value := range []string{
		"socks5://user:" + secret + "@proxy.example:1080", "user:" + secret + "@127.0.0.1:1055",
		secret + "@127.0.0.1:1055", "[::1%u:" + secret + "@x]:1055", "[fe80::1%a " + secret + "]:1055",
	} {
		if err := bus.CheckSOCKS5Proxy(value); err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("CheckSOCKS5Proxy(%q) = %v, want a refusal that does not print the value", value, err)
		}
		for _, environment := range []map[string]string{
			{bus.SOCKS5ProxyEnvVar: value},
			{bus.SOCKS5ProxyEnvVar: value, bus.AllowRemoteEnvVar: "1"},
		} {
			errs := map[string]error{}
			_, errs["Connect"] = bus.Connect([]string{"nats://127.0.0.1:1"}, bus.WithEnvironment(lookupOf(environment)))
			_, errs["ConnectOwningStream"] = bus.ConnectOwningStream([]string{"nats://127.0.0.1:1"}, bus.WithEnvironment(lookupOf(environment)))
			_, errs["Dial"] = bus.Dial("socks5-test", []string{"nats://127.0.0.1:1"}, lookupOf(environment))
			for name, err := range errs {
				if err == nil || !strings.Contains(err.Error(), bus.SOCKS5ProxyEnvVar) || strings.Contains(err.Error(), secret) {
					t.Errorf("%s with %v: %v, want a refusal naming %s and not printing the value", name, environment, err, bus.SOCKS5ProxyEnvVar)
				}
			}
		}
	}
}
