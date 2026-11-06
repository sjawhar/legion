//go:build linux || darwin

package listenfd

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"golang.org/x/sys/unix"
)

// inspect checks that fd is an open, listening IPv4 or IPv6 stream socket and returns its address
// and whether it is IPv6. Its refusals follow the descriptor's variable and number in a sentence.
func inspect(fd int) (*net.TCPAddr, bool, error) {
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		if errors.Is(err, unix.EBADF) {
			return nil, false, errors.New("is not an open descriptor")
		}
		return nil, false, fmt.Errorf("cannot be read: %w", err)
	}
	soType, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	if errors.Is(err, unix.ENOTSOCK) {
		return nil, false, errors.New("is not a socket: it must be a listening TCP socket")
	}
	if err != nil {
		return nil, false, fmt.Errorf("is a socket whose type cannot be read: %w", err)
	}
	if soType != unix.SOCK_STREAM {
		return nil, false, fmt.Errorf("is not a stream socket (SO_TYPE %d): it must be a listening TCP socket", soType)
	}
	sockaddr, err := unix.Getsockname(fd)
	if err != nil {
		return nil, false, fmt.Errorf("is a socket whose address cannot be read: %w", err)
	}
	var addr *net.TCPAddr
	var inet6 bool
	switch sockaddr := sockaddr.(type) {
	case *unix.SockaddrInet4:
		addr = &net.TCPAddr{IP: net.IP(sockaddr.Addr[:]), Port: sockaddr.Port}
	case *unix.SockaddrInet6:
		addr = &net.TCPAddr{IP: net.IP(sockaddr.Addr[:]), Port: sockaddr.Port, Zone: zoneName(sockaddr.ZoneId)}
		inet6 = true
	case *unix.SockaddrUnix:
		return nil, false, errors.New("is a unix-domain socket: it must be a listening TCP socket")
	default:
		return nil, false, fmt.Errorf("is a %T socket, neither IPv4 nor IPv6: it must be a listening TCP socket", sockaddr)
	}
	// Linux answers 1 for a listening socket and darwin the option's own bit, so any non-zero
	// answer is listening.
	accepting, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if err != nil {
		return nil, false, fmt.Errorf("is a TCP socket on %s whose listening state cannot be read: %w", addr, err)
	}
	if accepting == 0 {
		return nil, false, fmt.Errorf("is a TCP socket on %s that is not listening: it is connected or only bound", addr)
	}
	return addr, inet6, nil
}

// zoneName is the interface an IPv6 zone index names, as net.TCPAddr carries it.
func zoneName(index uint32) string {
	if index == 0 {
		return ""
	}
	if iface, err := net.InterfaceByIndex(int(index)); err == nil {
		return iface.Name
	}
	return strconv.FormatUint(uint64(index), 10)
}
