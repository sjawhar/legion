package bus_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testsocks5"
)

// unresolvable is the server every proxied case names: a host name that resolves nowhere, so only a
// proxy that ignores the name reaches the server.
const unresolvable = "nats.example.test:4222"

// waitForConnects waits for the proxy to have received n requests, every one of them want.
func waitForConnects(t *testing.T, proxy *testsocks5.Server, n int, want testsocks5.Request) {
	t.Helper()
	waitFor(t, 15*time.Second, fmt.Sprintf("%d requests at the proxy", n), func() bool { return len(proxy.Requests()) >= n })
	requireConnects(t, proxy, slices.Repeat([]testsocks5.Request{want}, n))
}

func requireConnects(t *testing.T, proxy *testsocks5.Server, want []testsocks5.Request) {
	t.Helper()
	if got := proxy.Requests(); !slices.Equal(got, want) {
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
	want := testsocks5.Request{AddressType: 0x03, Address: unresolvable}
	for name, connect := range map[string]clientConnect{"Connect": bus.Connect, "ConnectOwningStream": bus.ConnectOwningStream} {
		t.Run(name, func(t *testing.T) {
			uri := testnats.URL(t)
			fake := testsocks5.Start(t, hostOf(t, uri))
			client, err := connect([]string{withHost(t, uri, unresolvable)}, bus.WithEnvironment(proxied(fake.Addr)))
			if err != nil {
				t.Fatalf("connect through the proxy: %v", err)
			}
			t.Cleanup(client.Close)
			requireConnects(t, fake, []testsocks5.Request{want})

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

			fake.DropAll()
			waitForConnects(t, fake, 2, want)
			roundTrip("after-the-reconnect-in-place")

			// A connection nats.go has closed is replaced by the client's recovery, which dials
			// with the same proxy.
			client.Conn.Close()
			waitForConnects(t, fake, 3, want)
			waitFor(t, 15*time.Second, "the replacement connection", client.Connected)
			roundTrip("after-the-replacement")
		})
	}

	t.Run("Dial", func(t *testing.T) {
		uri := testnats.URL(t)
		fake := testsocks5.Start(t, hostOf(t, uri))
		conn, err := bus.Dial("socks5-test", []string{withHost(t, uri, unresolvable)}, proxied(fake.Addr))
		if err != nil {
			t.Fatalf("dial through the proxy: %v", err)
		}
		t.Cleanup(conn.Close)
		requireConnects(t, fake, []testsocks5.Request{want})

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

		fake.DropAll()
		waitForConnects(t, fake, 2, want)
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
	fake := testsocks5.Start(t, server)
	client, err := bus.Connect([]string{withHost(t, uri, net.JoinHostPort("localhost", port))}, bus.WithEnvironment(proxied(fake.Addr)))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	requireConnects(t, fake, []testsocks5.Request{{AddressType: 0x03, Address: "localhost:" + port}})
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
	fake := testsocks5.Start(t, server)
	t.Setenv(bus.SOCKS5ProxyEnvVar, fake.Addr)

	direct, err := bus.Connect([]string{uri}, bus.WithEnvironment(lookupOf(nil)))
	if err != nil {
		t.Fatalf("connect with no proxy handed: %v", err)
	}
	t.Cleanup(direct.Close)
	if _, port, err := net.SplitHostPort(direct.Conn.ConnectedAddr()); err != nil || port != serverPort {
		t.Fatalf("connected to %s (%v), want the NATS server's own port %s", direct.Conn.ConnectedAddr(), err, serverPort)
	}
	if accepted := fake.Accepted(); accepted != 0 {
		t.Fatalf("the proxy the process environment names accepted %d connections, want none", accepted)
	}

	// The same connect on the process environment goes through the proxy it names.
	t.Setenv(bus.AllowRemoteEnvVar, "1")
	throughProxy, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect on the process environment: %v", err)
	}
	t.Cleanup(throughProxy.Close)
	if got := throughProxy.Conn.ConnectedAddr(); got != fake.Addr {
		t.Fatalf("connected to %s, want the proxy %s", got, fake.Addr)
	}
	host, _, _ := net.SplitHostPort(server)
	addressType := byte(0x03)
	if net.ParseIP(host) != nil {
		addressType = 0x01
	}
	requireConnects(t, fake, []testsocks5.Request{{AddressType: addressType, Address: server}})
	// The count the direct connect read as zero counts this one, so a direct connect that dialled
	// the proxy too would have shown there.
	if accepted := fake.Accepted(); accepted < 1 {
		t.Fatalf("the proxy accepted %d connections after a connect through it, want at least 1", accepted)
	}
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
	silent := testsocks5.Start(t, "127.0.0.1:1")
	unreachable := testsocks5.StartRefusing(t, 0x04)
	for _, proxy := range []struct {
		name, address string
		also          []string
	}{
		{"nothing listens", "127.0.0.1:1", []string{"connection refused"}},
		{"closes without answering", silent.Addr, nil},
		{"answers host unreachable", unreachable.Addr, []string{"host unreachable"}},
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
	fake := testsocks5.Start(t, hostOf(t, uri))
	client, err := bus.Connect([]string{withHost(t, uri, unresolvable)}, bus.WithEnvironment(proxied(fake.Addr)))
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	// The proxy goes away, then the connection: the publish dials a replacement, and every attempt
	// is refused.
	fake.Close()
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
	for _, want := range []string{bus.SOCKS5ProxyEnvVar, fake.Addr} {
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
	fake := testsocks5.Start(t, "127.0.0.1:1")
	urls := []string{"nats://" + fake.Addr}
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
	if accepted := fake.Accepted(); accepted != 0 {
		t.Fatalf("the server URL accepted %d connections, want none", accepted)
	}
	// A dial to the server URL is counted, so the zero above means no connect dialled it.
	conn, err := net.Dial("tcp", fake.Addr)
	if err != nil {
		t.Fatalf("dial the server URL: %v", err)
	}
	_ = conn.Close()
	waitFor(t, 5*time.Second, "the server URL to count a dial", func() bool { return fake.Accepted() >= 1 })
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

// printsSome reports whether message carries any eight characters in a row of secret, as written
// or in lower case.
func printsSome(message, secret string) bool {
	for _, s := range []string{secret, strings.ToLower(secret)} {
		for i := 0; i+8 <= len(s); i++ {
			if strings.Contains(message, s[i:i+8]) {
				return true
			}
		}
	}
	return false
}

// A proxy value that holds an nkey seed is refused at once, before anything dials, naming the
// setting and saying it holds a seed, and no refusal prints any of it. A seed is a valid DNS label,
// so `<seed>:1055` and `<seed>.example.test:1055` are host:port, and a connect through either
// would print the value in its dial error, which names the address an operator set. The seed counts
// as the whole value, as a host, as one label of a longer name, inside a longer label, split across
// labels by '.', '-' or '_', in lower case, and of any type (bus.HoldsSeed). The check decodes
// rather than matching a shape: a 58-letter base32 label that is a user seed with one character
// changed, which its checksum catches, is still a proxy address, whole or split across labels, and
// a connect through it dials (about ten seconds, the bus's ten attempts). The rows run in parallel.
func TestAProxyHoldingAnNkeySeedIsRefused(t *testing.T) {
	seedOf := func(create func() (nkeys.KeyPair, error)) string {
		t.Helper()
		pair, err := create()
		if err != nil {
			t.Fatalf("create an nkey: %v", err)
		}
		seed, err := pair.Seed()
		if err != nil {
			t.Fatalf("read the nkey's seed: %v", err)
		}
		return string(seed)
	}
	user, account := seedOf(nkeys.CreateUser), seedOf(nkeys.CreateAccount)
	lower := strings.ToLower

	for _, row := range []struct{ name, value, seed string }{
		{"the whole value", user, user},
		{"the host", user + ":1055", user},
		{"a label", user + ".example.test:1055", user},
		{"a lower-case label", "sidecar." + lower(user) + ".example.test:1055", user},
		{"an account seed", account + ":1055", account},
		{"a label with a base32 letter after the seed", user + "A.example.test:1055", user},
		{"a label with a base32 letter before an account seed", "x" + account + ".example.test:1055", account},
		{"a lower-case label with a letter after the seed", lower(user) + "b.example.test:1055", user},
		{"split across labels", user[:29] + "." + user[29:] + ":1055", user},
		{"split by a hyphen", user[:20] + "-" + user[20:] + ".example.test:1055", user},
		{"split by an underscore", user[:40] + "_" + user[40:] + ":1055", user},
		{"an account seed split across labels", account[:30] + "." + account[30:] + ":1055", account},
		{"lower case, split across labels", "sidecar." + lower(user[:25]) + "." + lower(user[25:]) + ".example.test:1055", user},
	} {
		t.Run("seed/"+row.name, func(t *testing.T) {
			t.Parallel()
			if err := bus.CheckSOCKS5Proxy(row.value); err == nil || !strings.Contains(err.Error(), "nkey seed") || printsSome(err.Error(), row.seed) {
				t.Errorf("CheckSOCKS5Proxy(%q) = %v, want a refusal saying it holds an nkey seed, printing none of it", row.value, err)
			}
			for _, environment := range []map[string]string{
				{bus.SOCKS5ProxyEnvVar: row.value},
				{bus.SOCKS5ProxyEnvVar: row.value, bus.AllowRemoteEnvVar: "1"},
			} {
				errs := map[string]error{}
				started := time.Now()
				_, errs["Connect"] = bus.Connect([]string{"nats://127.0.0.1:1"}, bus.WithEnvironment(lookupOf(environment)))
				_, errs["ConnectOwningStream"] = bus.ConnectOwningStream([]string{"nats://127.0.0.1:1"}, bus.WithEnvironment(lookupOf(environment)))
				_, errs["Dial"] = bus.Dial("socks5-test", []string{"nats://127.0.0.1:1"}, lookupOf(environment))
				if elapsed := time.Since(started); elapsed >= time.Second {
					t.Errorf("three connects with %v took %s to fail: one dialled", environment, elapsed)
				}
				_, optedIn := environment[bus.AllowRemoteEnvVar]
				for name, err := range errs {
					if err == nil {
						t.Errorf("%s with %v connected, want a refusal", name, environment)
						continue
					}
					if !strings.Contains(err.Error(), bus.SOCKS5ProxyEnvVar) || printsSome(err.Error(), row.seed) {
						t.Errorf("%s with %v: %v, want a refusal naming %s and printing none of the seed", name, environment, err, bus.SOCKS5ProxyEnvVar)
					}
					if optedIn && !strings.Contains(err.Error(), "nkey seed") {
						t.Errorf("%s with %v: %v, want the refusal to say the value holds an nkey seed", name, environment, err)
					}
				}
			}
		})
	}

	changed := byte('A')
	if user[30] == 'A' {
		changed = 'B'
	}
	label := user[:30] + string(changed) + user[31:]
	if _, _, err := nkeys.DecodeSeed([]byte(label)); err == nil {
		t.Fatalf("the control label %s decodes as a seed", label)
	}
	for _, row := range []struct{ name, value string }{
		{"a label that is not a seed", label + ".example.test:1055"},
		{"a label that is not a seed, split across labels", label[:29] + "." + label[29:] + ".example.test:1055"},
	} {
		t.Run("control/"+row.name, func(t *testing.T) {
			t.Parallel()
			if err := bus.CheckSOCKS5Proxy(row.value); err != nil {
				t.Fatalf("CheckSOCKS5Proxy(%q) = %v, want it accepted", row.value, err)
			}
			_, err := bus.Connect([]string{"nats://" + unresolvable}, bus.WithEnvironment(proxied(row.value)))
			if err == nil || !strings.Contains(err.Error(), "NATS connect through the SOCKS5 proxy "+bus.SOCKS5ProxyEnvVar+"="+row.value+" failed") {
				t.Fatalf("connect through %s: %v, want the failed dial through it, naming it", row.value, err)
			}
		})
	}
}
