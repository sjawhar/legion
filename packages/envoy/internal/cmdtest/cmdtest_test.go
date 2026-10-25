package cmdtest

import (
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// 500 draws repeat no port only because FreeTCPPort keeps the ports it handed out: random picks
// from the 12,768 ports of its range repeat one after about 140 draws on average, and within 500
// draws more than 9,999 times in 10,000. Half the draws are a subtest's, so a port stays out of
// another test's hands while the test it went to still runs, not only out of its own.
func TestFreeTCPPortHandsOutNoPortTwiceWhileItsTestRuns(t *testing.T) {
	seen := map[int]bool{}
	draw := func(t *testing.T) {
		for range 250 {
			port := FreeTCPPort(t)
			if seen[port] {
				t.Fatalf("FreeTCPPort handed out port %d a second time, after %d other ports, while the test it first went to still ran", port, len(seen))
			}
			seen[port] = true
		}
	}
	draw(t)
	t.Run("another test", draw)
}

// A port goes back once the test it went to ends, so a long run never runs out of ports.
func TestFreeTCPPortTakesItsPortsBackWhenTheirTestEnds(t *testing.T) {
	var ports []int
	t.Run("holder", func(t *testing.T) {
		for range 50 {
			ports = append(ports, FreeTCPPort(t))
		}
	})
	handedOutMu.Lock()
	defer handedOutMu.Unlock()
	for _, port := range ports {
		if handedOut[port] {
			t.Errorf("port %d is still held for a test that has ended", port)
		}
	}
}

// The second process of a pair draws its port while the first still boots, so the first one's port
// is free and can come up again as a candidate, as the kernel offered it in the CI run
// FreeTCPPort's comment names. The draw must not bind it even for a moment, since the first
// process binds it whenever its boot gets there. Here every other candidate is that port, and a
// goroutine stands in for the booting process, binding the port over and over while the draws run:
// none of its binds may be refused.
func TestFreeTCPPortNeverBindsAPortWhileTheProcessItWentToBindsIt(t *testing.T) {
	booting := FreeTCPPort(t)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(booting))
	proposals := 0
	candidate := func() int {
		proposals++
		if proposals%2 == 1 {
			return booting
		}
		return freePortLow + proposals%(freePortHigh-freePortLow+1)
	}

	var bound, refused atomic.Int64
	var lastRefusal atomic.Value
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			listener, err := net.Listen("tcp", address)
			if err != nil {
				refused.Add(1)
				lastRefusal.Store(err.Error())
				continue
			}
			bound.Add(1)
			_ = listener.Close()
		}
	}()
	for range 1000 {
		freeTCPPortFrom(t, candidate)
	}
	close(stop)
	<-stopped

	t.Logf("the booting process bound port %d %d times while 1000 draws were offered it", booting, bound.Load())
	if refused.Load() > 0 {
		t.Errorf("the booting process's bind of port %d was refused %d times while the draws ran; the last: %v", booting, refused.Load(), lastRefusal.Load())
	}
}

// The ports FreeTCPPort hands out lie below the range this machine's kernel takes ports from for
// itself, which is what keeps a listener on port 0 or an outgoing connection, the booting
// process's own among them, off them.
func TestFreeTCPPortsLieOutsideTheKernelsOwnPortRange(t *testing.T) {
	configured, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Skipf("this machine shows no Linux port range: %v", err)
	}
	bounds := strings.Fields(string(configured))
	if len(bounds) != 2 {
		t.Fatalf("ip_local_port_range reads %q, want two ports", configured)
	}
	low, err := strconv.Atoi(bounds[0])
	if err != nil {
		t.Fatalf("ip_local_port_range reads %q: %v", configured, err)
	}
	if freePortHigh >= low {
		t.Errorf("FreeTCPPort hands out ports up to %d, inside this kernel's own range from %d", freePortHigh, low)
	}
}
