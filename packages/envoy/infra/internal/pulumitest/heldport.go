package pulumitest

import (
	"net"
	"strconv"
	"syscall"
	"testing"
)

// HeldPortSocket is the abstract unix socket on which HoldPort hands out the listening socket of
// port, and which the entrypoint of HeldPortImage (host/heldportwrapper) asks for it.
func HeldPortSocket(port int) string {
	return "@legion-envoy-held-port-" + strconv.Itoa(port)
}

// HoldPort opens a listening socket on a loopback port the kernel chooses and holds it until the
// test ends, so no other program can take the port. It serves HeldPortSocket(port), sending every
// connection a copy of the socket by SCM_RIGHTS: each start and restart of a HeldPortImage
// container on the host's network serves on that same socket, and the port stays held between
// them. The socket and the server close when the test ends, after every cleanup the test registers
// later, so a live test calls it before NewStack: the stack's destroy removes the container first.
// It returns the test's own copy of the socket.
func HoldPort(t testing.TB) *net.TCPListener {
	t.Helper()
	held, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	socket := HeldPortSocket(held.Addr().(*net.TCPAddr).Port)
	server, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		_ = held.Close()
		t.Fatalf("serve the held port on %s: %v", socket, err)
	}
	raw, err := held.SyscallConn()
	if err != nil {
		_ = server.Close()
		_ = held.Close()
		t.Fatalf("read the held socket's descriptor: %v", err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		for {
			conn, err := server.AcceptUnix()
			if err != nil {
				return
			}
			// A send that fails reaches the asking process as no descriptor, which it reports.
			_ = raw.Control(func(fd uintptr) {
				_, _, _ = conn.WriteMsgUnix([]byte{0}, syscall.UnixRights(int(fd)), nil)
			})
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-served
		_ = held.Close()
	})
	return held
}
