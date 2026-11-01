package listenfd

import (
	"net"
	"strings"
	"testing"
)

func TestCheckAgainst(t *testing.T) {
	socket := func(ip string, inet6 bool) Socket {
		addr := net.ParseIP(ip)
		if !inet6 {
			addr = addr.To4()
		}
		return Socket{name: "TEST_LISTEN_FD", fd: 3, addr: &net.TCPAddr{IP: addr, Port: 40123}, inet6: inet6}
	}
	loopback4, any4 := socket("127.0.0.1", false), socket("0.0.0.0", false)
	loopback6, any6 := socket("::1", true), socket("::", true)
	// An AF_INET6 socket bound to an IPv4-mapped address, which net.IP.Equal alone takes for the
	// IPv4 address.
	mapped6 := socket("::ffff:127.0.0.1", true)

	host := func(value string) Setting { return Setting{Name: "TEST_HOST", Value: value, Set: true} }
	port := func(value string) Setting { return Setting{Name: "TEST_PORT", Value: value, Set: true} }
	// Unset settings carry values that would be refused if compared.
	noHost := Setting{Name: "TEST_HOST", Value: "10.9.9.9"}
	noPort := Setting{Name: "TEST_PORT", Value: "1"}

	cases := []struct {
		name       string
		socket     Socket
		host, port Setting
		// refused lists the settings a refusal must name; none means the check passes.
		refused []string
	}{
		{"unset", loopback4, noHost, noPort, nil},
		{"port only", loopback4, noHost, port("40123"), nil},
		{"port mismatch", loopback4, noHost, port("40124"), []string{"TEST_PORT=40124"}},
		{"port 0", loopback4, noHost, port("0"), []string{"TEST_PORT=0"}},
		{"port not a number", loopback4, noHost, port("http"), []string{`TEST_PORT="http"`}},
		{"port out of range", loopback4, noHost, port("105659"), []string{`TEST_PORT="105659"`}},
		{"empty port", loopback4, noHost, port(""), []string{`TEST_PORT=""`}},
		{"host only", loopback4, host("127.0.0.1"), noPort, nil},
		{"host mismatch", loopback4, host("127.0.0.2"), noPort, []string{"TEST_HOST=127.0.0.2"}},
		{"both", loopback4, host("127.0.0.1"), port("40123"), nil},
		{"both mismatch", loopback4, host("127.0.0.2"), port("40124"), []string{"TEST_HOST=127.0.0.2", "TEST_PORT=40124"}},
		{"host matches, port does not", loopback4, host("127.0.0.1"), port("40124"), []string{"TEST_PORT=40124"}},
		{"IPv6", loopback6, host("::1"), port("40123"), nil},
		{"bracketed IPv6", loopback6, host("[::1]"), port("40123"), nil},
		{"bracketed IPv6 mismatch", loopback6, host("[::2]"), noPort, []string{"TEST_HOST=[::2]"}},
		{"IPv4 host, IPv6 socket on its mapped address", mapped6, host("127.0.0.1"), noPort, []string{"TEST_HOST=127.0.0.1"}},
		{"IPv6 mapped host, IPv4 socket", loopback4, host("::ffff:127.0.0.1"), noPort, []string{"TEST_HOST=::ffff:127.0.0.1"}},
		{"IPv6 mapped host, IPv6 socket on it", mapped6, host("::ffff:127.0.0.1"), noPort, nil},
		{"IPv4 loopback host, IPv6 loopback socket", loopback6, host("127.0.0.1"), noPort, []string{"TEST_HOST=127.0.0.1"}},
		{"0.0.0.0, IPv4 socket on every interface", any4, host("0.0.0.0"), noPort, nil},
		{"::, IPv6 socket on every interface", any6, host("::"), noPort, nil},
		{"0.0.0.0, IPv6 socket on every interface", any6, host("0.0.0.0"), noPort, []string{"TEST_HOST=0.0.0.0"}},
		{"::, IPv4 socket on every interface", any4, host("::"), noPort, []string{"TEST_HOST=::"}},
		{"0.0.0.0, specific socket", loopback4, host("0.0.0.0"), noPort, []string{"TEST_HOST=0.0.0.0"}},
		{"::, specific socket", loopback6, host("[::]"), noPort, []string{"TEST_HOST=[::]"}},
		{"specific host, socket on every interface", any4, host("127.0.0.1"), noPort, []string{"TEST_HOST=127.0.0.1"}},
		{"empty host, IPv4 socket on every interface", any4, host(""), noPort, nil},
		{"empty host, IPv6 socket on every interface", any6, host(""), noPort, nil},
		{"empty host, specific socket", loopback4, host(""), noPort, []string{"TEST_HOST is empty"}},
		{"host name", loopback4, host("localhost"), noPort, []string{`TEST_HOST="localhost"`}},
		{"bracketed host name", loopback4, host("[localhost]"), noPort, []string{`TEST_HOST="[localhost]"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.socket.CheckAgainst(tc.host, tc.port)
			if len(tc.refused) == 0 {
				if err != nil {
					t.Fatalf("CheckAgainst(%+v, %+v) on %s: %v; want accepted", tc.host, tc.port, tc.socket, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckAgainst(%+v, %+v) on %s accepted; want refused naming %q", tc.host, tc.port, tc.socket, tc.refused)
			}
			// Every refusal names the setting, the socket's variable and the socket's address.
			for _, want := range append(tc.refused, "TEST_LISTEN_FD=3", tc.socket.addr.String()) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
		})
	}
}
