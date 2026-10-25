package cmdtest

import "testing"

// 500 draws repeat no port only because FreeTCPPort keeps the ports it handed out: Linux draws a
// listen-and-close port from the odd ports of the lower half of its ephemeral range, about 7,000
// under the default range, so plain draws repeat one after about a hundred on average and within a
// few hundred (measured on Linux 7.0). Half the draws are a subtest's, so a port stays out of
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
