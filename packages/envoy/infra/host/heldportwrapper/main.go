//go:build linux

// Command envoy-listener-heldport is the entrypoint of the image the envoy-infra live tests run
// (legion-envoy-smoke-heldport, which this directory's Dockerfile builds on top of the smoke
// image). It serves envoy-listener on the listening socket the test holds for the container's
// port, so the port the kernel chose for the test stays held from the moment the test opened it
// until the listener serves on it, through every start and restart Docker makes. Docker cannot pass
// a host descriptor into a container, so the wrapper fetches it.
//
// It reads PORT and connects to the abstract unix socket @legion-envoy-held-port-<PORT>, which the
// test serves (pulumitest.HoldPort). A container on the host's network shares the host's network
// namespace, and with it the abstract socket names. It receives one descriptor by SCM_RIGHTS,
// checks that it is a listening TCP socket (listenfd.Inspect), makes it descriptor 3 without
// close-on-exec, closes the copy it received, and executes the real listener in its own place with
// ENVOY_LISTEN_FD=3: /usr/local/libexec/envoy-listener, or the path in HELDPORT_WRAPPER_EXEC, which
// only this package's test sets. The listener adopts the socket close-on-exec and closes
// descriptor 3, so nothing it starts inherits it.
//
// Any failure exits 1 with a message naming the abstract socket, and Docker restarts the container
// (restart unless-stopped). No production image carries this wrapper.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/listenfd"
)

const (
	// socketPrefix and the container's PORT name the abstract unix socket the test hands the held
	// socket out on; pulumitest.HeldPortSocket spells the same name.
	socketPrefix = "@legion-envoy-held-port-"
	// listenFD is the descriptor the listener inherits the socket as, which ENVOY_LISTEN_FD names.
	listenFD = 3
	// defaultTarget is where the held-port image keeps the real envoy-listener.
	defaultTarget = "/usr/local/libexec/envoy-listener"
	// targetEnv names another listener binary to execute, for the wrapper's own test.
	targetEnv = "HELDPORT_WRAPPER_EXEC"
	// receiveTimeout bounds the wait for the descriptor once the abstract socket accepted the
	// connection, so a server that never sends ends the start rather than hanging it.
	receiveTimeout = 10 * time.Second
)

func main() {
	port := os.Getenv("PORT")
	if err := run(port); err != nil {
		fmt.Fprintf(os.Stderr, "envoy-listener-heldport: %s%s: %v\n", socketPrefix, port, err)
		os.Exit(1)
	}
}

// run takes the held socket for port and executes the listener on it. It returns only on failure.
func run(port string) error {
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return fmt.Errorf("PORT=%q is not a port number 1-65535, which names the socket to ask", port)
	}
	fd, err := receive(socketPrefix + port)
	if err != nil {
		return err
	}
	if _, err := listenfd.Inspect("the descriptor received", strconv.Itoa(fd)); err != nil {
		_ = syscall.Close(fd)
		return err
	}
	if err := inheritAsListenFD(fd); err != nil {
		return err
	}
	target := os.Getenv(targetEnv)
	if target == "" {
		target = defaultTarget
	}
	if err := os.Setenv("ENVOY_LISTEN_FD", strconv.Itoa(listenFD)); err != nil {
		return err
	}
	// The listener takes the wrapper's process, so the container's main process, the one Docker
	// signals to stop, is the listener.
	err = syscall.Exec(target, append([]string{target}, os.Args[1:]...), os.Environ())
	return fmt.Errorf("execute %s: %w", target, err)
}

// receive connects to the abstract unix socket (its name starts with @) and returns the one
// descriptor the server sends by SCM_RIGHTS, received close-on-exec.
//
// It uses raw system calls rather than package net, which would start the Go runtime's network
// poller: inheritAsListenFD then replaces whatever descriptor 3 is, and that must never be the
// poller's.
func receive(socket string) (int, error) {
	conn, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open a unix socket: %w", err)
	}
	defer syscall.Close(conn)
	timeout := syscall.NsecToTimeval(receiveTimeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(conn, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeout); err != nil {
		return -1, fmt.Errorf("bound the receive: %w", err)
	}
	// syscall reads a leading @ as the abstract namespace.
	if err := syscall.Connect(conn, &syscall.SockaddrUnix{Name: socket}); err != nil {
		return -1, fmt.Errorf("connect: %w (the test that holds the port serves this socket)", err)
	}
	data := make([]byte, 1)
	oob := make([]byte, syscall.CmsgSpace(4))
	_, oobn, flags, _, err := syscall.Recvmsg(conn, data, oob, syscall.MSG_CMSG_CLOEXEC)
	if err != nil {
		return -1, fmt.Errorf("receive the listening socket within %s: %w", receiveTimeout, err)
	}
	messages, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, fmt.Errorf("read what the server sent: %w", err)
	}
	var fds []int
	for _, message := range messages {
		if rights, err := syscall.ParseUnixRights(&message); err == nil {
			fds = append(fds, rights...)
		}
	}
	if len(fds) == 1 && flags&syscall.MSG_CTRUNC == 0 {
		return fds[0], nil
	}
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
	if flags&syscall.MSG_CTRUNC != 0 {
		return -1, errors.New("the server sent more than the one descriptor it should")
	}
	return -1, fmt.Errorf("the server sent %d descriptors, want exactly one", len(fds))
}

// inheritAsListenFD makes fd descriptor 3 without close-on-exec, so the listener the wrapper
// executes inherits it there, and closes fd. When fd already is 3 it only clears close-on-exec.
//
// Descriptor 3 may be one the Go runtime opened close-on-exec before main, such as its cgroup CPU
// limit file, which dup3 then closes: the runtime reads that file only to size GOMAXPROCS and
// ignores a failed read, and the exec that follows closes every close-on-exec descriptor anyway.
func inheritAsListenFD(fd int) error {
	if fd == listenFD {
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, 0); errno != 0 {
			return fmt.Errorf("clear close-on-exec on descriptor %d: %w", fd, errno)
		}
		return nil
	}
	if err := syscall.Dup3(fd, listenFD, 0); err != nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("make the listening socket descriptor %d: %w", listenFD, err)
	}
	return syscall.Close(fd)
}
