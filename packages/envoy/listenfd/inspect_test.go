//go:build linux || darwin

package listenfd

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const variable = "TEST_LISTEN_FD"

// rawFD returns a descriptor of its own for what filer holds, which the caller closes, as a process
// holds the descriptor it inherited.
func rawFD(t *testing.T, filer interface{ File() (*os.File, error) }) int {
	t.Helper()
	file, err := filer.File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fd, err := unix.Dup(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

// owned closes fd when the test ends.
func owned(t *testing.T, fd int) string {
	t.Cleanup(func() { _ = unix.Close(fd) })
	return strconv.Itoa(fd)
}

func listen(t *testing.T, network, address string) net.Listener {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func TestInspectRefuses(t *testing.T) {
	value := func(v string) func(*testing.T) string { return func(*testing.T) string { return v } }
	cases := []struct {
		name   string
		value  func(*testing.T) string
		reason string
	}{
		{"not a number", value("three"), "is not a descriptor number"},
		{"empty", value(""), "is not a descriptor number"},
		{"negative", value("-1"), "is not a descriptor number"},
		{"signed", value("+3"), "is not a descriptor number"},
		{"spaced", value(" 3"), "is not a descriptor number"},
		{"standard input", value("0"), "names standard input, output or error"},
		{"standard output", value("1"), "names standard input, output or error"},
		{"standard error", value("2"), "names standard input, output or error"},
		{"closed descriptor", func(t *testing.T) string {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			_ = writer.Close()
			fd := int(reader.Fd())
			_ = reader.Close()
			return strconv.Itoa(fd)
		}, "is not an open descriptor"},
		{"regular file", func(t *testing.T) string {
			file, err := os.Create(filepath.Join(t.TempDir(), "file"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			return strconv.Itoa(int(file.Fd()))
		}, "is not a socket"},
		{"pipe", func(t *testing.T) string {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
			return strconv.Itoa(int(reader.Fd()))
		}, "is not a socket"},
		{"UDP socket", func(t *testing.T) string {
			conn, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			return owned(t, rawFD(t, conn.(*net.UDPConn)))
		}, "is not a stream socket"},
		{"unix listener", func(t *testing.T) string {
			// A short directory, since a socket path has a length limit and t.TempDir's may pass it.
			dir, err := os.MkdirTemp("", "lfd")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			listener := listen(t, "unix", filepath.Join(dir, "s"))
			return owned(t, rawFD(t, listener.(*net.UnixListener)))
		}, "is a unix-domain socket"},
		{"connected TCP socket", func(t *testing.T) string {
			listener := listen(t, "tcp", "127.0.0.1:0")
			conn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			return owned(t, rawFD(t, conn.(*net.TCPConn)))
		}, "that is not listening"},
		{"bound TCP socket that is not listening", func(t *testing.T) string {
			fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			value := owned(t, fd)
			if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
				t.Fatal(err)
			}
			return value
		}, "that is not listening"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.value(t)
			socket, err := Inspect(variable, value)
			if err == nil {
				t.Fatalf("Inspect(%s=%q) accepted %s; want refused: %s", variable, value, socket, tc.reason)
			}
			if !strings.HasPrefix(err.Error(), variable+"=") || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("Inspect(%s=%q) refused %q; want it to name %s and say %q", variable, value, err, variable, tc.reason)
			}
		})
	}
}

func TestInspectAcceptsAListeningTCPSocket(t *testing.T) {
	for _, tc := range []struct{ name, network, address string }{
		{"IPv4", "tcp4", "127.0.0.1:0"},
		{"IPv6", "tcp6", "[::1]:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen(tc.network, tc.address)
			if err != nil {
				if tc.network == "tcp6" {
					t.Skipf("this host has no ::1: %v", err)
				}
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			fd := rawFD(t, listener.(*net.TCPListener))
			value := owned(t, fd)

			socket, err := Inspect(variable, value)
			if err != nil {
				t.Fatalf("Inspect(%s=%s) of a socket listening on %s: %v", variable, value, listener.Addr(), err)
			}
			if got, want := socket.Addr().String(), listener.Addr().String(); got != want {
				t.Fatalf("Inspect(%s=%s).Addr() = %s; want %s", variable, value, got, want)
			}
			if err := socket.CheckAgainst(
				Setting{Name: "TEST_HOST", Value: socket.Addr().IP.String(), Set: true},
				Setting{Name: "TEST_PORT", Value: strconv.Itoa(socket.Addr().Port), Set: true},
			); err != nil {
				t.Fatalf("CheckAgainst its own address: %v", err)
			}
			// Inspect takes no ownership: the descriptor is still open.
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
				t.Fatalf("descriptor %d after Inspect: %v; want still open", fd, err)
			}
		})
	}
}

// After Adopt only the listener's own close-on-exec copy holds the socket, so no process the server
// starts inherits it, and that copy serves.
func TestAdoptLeavesOnlyItsCloseOnExecCopy(t *testing.T) {
	original := listen(t, "tcp", "127.0.0.1:0")
	fd := rawFD(t, original.(*net.TCPListener))
	// The inherited descriptor alone holds the socket from here, as in a process a harness started.
	_ = original.Close()

	socket, err := Inspect(variable, strconv.Itoa(fd))
	if err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	listener, adoptErr := Adopt(socket)
	// Read before anything else can open a descriptor and be given fd's number.
	_, fcntlErr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if adoptErr != nil {
		t.Fatalf("Adopt(%s): %v", socket, adoptErr)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if !errors.Is(fcntlErr, syscall.EBADF) {
		t.Fatalf("fcntl(F_GETFD) on the inherited descriptor %d after Adopt: %v; want EBADF", fd, fcntlErr)
	}

	raw, err := listener.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	if err := raw.Control(func(own uintptr) { flags, fcntlErr = unix.FcntlInt(own, unix.F_GETFD, 0) }); err != nil {
		t.Fatal(err)
	}
	if fcntlErr != nil {
		t.Fatalf("fcntl(F_GETFD) on the adopted listener's descriptor: %v", fcntlErr)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("adopted listener's descriptor flags %#x; want FD_CLOEXEC", flags)
	}

	if got, want := listener.Addr().String(), socket.Addr().String(); got != want {
		t.Fatalf("adopted listener serves on %s; want %s", got, want)
	}
	conn, err := net.Dial("tcp", socket.Addr().String())
	if err != nil {
		t.Fatalf("dial the adopted socket: %v", err)
	}
	defer conn.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept on the adopted listener: %v", err)
	}
	_ = accepted.Close()
}

// A descriptor Adopt cannot serve on is closed all the same.
func TestAdoptClosesTheDescriptorWhenItFails(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	fd, err := unix.Dup(int(reader.Fd()))
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Not a Socket Inspect would return: only a descriptor net.FileListener refuses makes Adopt fail.
	listener, adoptErr := Adopt(Socket{name: variable, fd: fd, addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1).To4(), Port: 1}})
	_, fcntlErr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if adoptErr == nil {
		_ = listener.Close()
		t.Fatalf("Adopt of a pipe served on it; want refused")
	}
	if !strings.Contains(adoptErr.Error(), variable+"=") {
		t.Errorf("Adopt refused %q; want it to name %s", adoptErr, variable)
	}
	if !errors.Is(fcntlErr, syscall.EBADF) {
		t.Fatalf("fcntl(F_GETFD) on descriptor %d after a failed Adopt: %v; want EBADF", fd, fcntlErr)
	}
}

func TestAdoptRefusesTheZeroSocket(t *testing.T) {
	if _, err := unix.FcntlInt(uintptr(0), unix.F_GETFD, 0); err != nil {
		t.Skipf("this test binary has no standard input to protect: %v", err)
	}
	if _, err := Adopt(Socket{}); err == nil {
		t.Fatal("Adopt(Socket{}) served; want refused")
	}
	if _, err := unix.FcntlInt(uintptr(0), unix.F_GETFD, 0); err != nil {
		t.Fatalf("standard input after Adopt(Socket{}): %v; want still open", err)
	}
}
