// Package listenfd serves on a listening TCP socket a process inherited from whatever started it,
// named by a descriptor number in an environment variable. A test harness that opens the socket on
// port 0 holds the port the kernel chose from that moment until the process serves on it, so no
// other program can take the port in between, as one can take a port number a harness found free
// and released before the process bound it.
//
// It is public because the Legion daemon's module imports it as well as envoy-dispatch and
// envoy-listener.
package listenfd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Socket is an inherited listening TCP socket, as Inspect found it.
type Socket struct {
	// name is the variable that named the descriptor.
	name string
	fd   int
	addr *net.TCPAddr
	// inet6 is whether the socket is AF_INET6, which an IPv4-mapped address on it does not change.
	inet6 bool
}

// Setting is one of a process's own settings for the address it serves on, which CheckAgainst
// compares with an inherited socket's address.
type Setting struct {
	// Name is what a refusal calls the setting, such as DISPATCH_PORT or legion.yaml's port.
	Name string
	// Value is the setting's value, compared only when Set.
	Value string
	// Set is whether the setting was given at all, as opposed to defaulted.
	Set bool
}

// Inspect reads the descriptor value names, which the variable name holds, and returns it when it
// is an open, listening IPv4 or IPv6 TCP socket. It takes no ownership: the descriptor stays open
// whatever it returns, and only Adopt closes it. Each refusal names name and what is wrong; none
// binds an address in the descriptor's place.
func Inspect(name, value string) (Socket, error) {
	// 31 bits, so the number fits an int on every platform; a sign, a space or any other character
	// is refused with the rest.
	n, err := strconv.ParseUint(value, 10, 31)
	if err != nil {
		return Socket{}, fmt.Errorf("%s=%q is not a descriptor number: it must be a decimal number of 3 or more", name, value)
	}
	if n < 3 {
		return Socket{}, fmt.Errorf("%s=%d names standard input, output or error, not an inherited socket: it must be 3 or more", name, n)
	}
	fd := int(n)
	addr, inet6, err := inspect(fd)
	if err != nil {
		return Socket{}, fmt.Errorf("%s=%d %w", name, fd, err)
	}
	return Socket{name: name, fd: fd, addr: addr, inet6: inet6}, nil
}

// Addr is the address the socket listens on.
func (s Socket) Addr() *net.TCPAddr {
	return s.addr
}

// String names the socket as refusals do: its variable, its descriptor and its address.
func (s Socket) String() string {
	return fmt.Sprintf("%s=%d, a socket listening on %s", s.name, s.fd, s.addr)
}

// CheckAgainst refuses the socket when a host or port setting that is set names another address
// than the socket's. It compares only the settings that are set, and fills in no default for one
// that is not.
//
// A set port must be a port number equal to the socket's; port 0 never matches. A set host must be
// an IP literal, in brackets or not, equal to the socket's IP and of its family: IPv4 and IPv6
// never match each other, so 0.0.0.0 matches only an IPv4 socket on every interface and :: only an
// IPv6 one. An empty host, which means every interface, matches only a socket on every interface,
// and a host name matches nothing. Each refusal names the setting, the socket's variable and its
// address.
func (s Socket) CheckAgainst(host, port Setting) error {
	var refusals []error
	if host.Set {
		refusals = append(refusals, s.checkHost(host))
	}
	if port.Set {
		refusals = append(refusals, s.checkPort(port))
	}
	return errors.Join(refusals...)
}

func (s Socket) checkHost(host Setting) error {
	if host.Value == "" {
		if s.addr.IP.IsUnspecified() {
			return nil
		}
		return fmt.Errorf("%s is empty, which means every interface, but %s, is on one address only", host.Name, s)
	}
	literal := host.Value
	if strings.HasPrefix(literal, "[") && strings.HasSuffix(literal, "]") {
		literal = literal[1 : len(literal)-1]
	}
	ip := net.ParseIP(literal)
	if ip == nil {
		return fmt.Errorf("%s=%q is not an IP address: with %s it must be the socket's IP, not a host name", host.Name, host.Value, s)
	}
	if strings.Contains(literal, ":") != s.inet6 || !ip.Equal(s.addr.IP) {
		return fmt.Errorf("%s=%s does not match %s", host.Name, host.Value, s)
	}
	return nil
}

func (s Socket) checkPort(port Setting) error {
	n, err := strconv.ParseUint(port.Value, 10, 16)
	if err != nil {
		return fmt.Errorf("%s=%q is not a port number: with %s it must be %d", port.Name, port.Value, s, s.addr.Port)
	}
	if n == 0 {
		return fmt.Errorf("%s=0 asks for a port the kernel chooses, which never names %s: set it to %d or leave it unset", port.Name, s, s.addr.Port)
	}
	if int(n) != s.addr.Port {
		return fmt.Errorf("%s=%d does not match %s", port.Name, n, s)
	}
	return nil
}

// Adopt returns a listener serving on the socket, and closes the inherited descriptor whether it
// succeeds or not. The listener serves on its own close-on-exec copy of the socket, so no process
// the server starts inherits it. A Socket is adopted once: its descriptor number is closed after.
func Adopt(s Socket) (net.Listener, error) {
	if s.addr == nil {
		// The zero Socket's descriptor 0 is standard input, which Adopt must not close.
		return nil, errors.New("listenfd: adopt a Socket Inspect did not return")
	}
	file := os.NewFile(uintptr(s.fd), s.name)
	listener, err := net.FileListener(file)
	closeErr := file.Close()
	if err != nil {
		return nil, fmt.Errorf("%s=%d: serve on the inherited socket: %w", s.name, s.fd, err)
	}
	if closeErr != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("%s=%d: close the inherited descriptor once its copy serves: %w", s.name, s.fd, closeErr)
	}
	return listener, nil
}
