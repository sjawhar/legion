package record

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// testPeerFacts is one reading of the kernel's answer about an asking process, as the helper
// signs it: the values are literals, not values this test computed from its own process.
func testPeerFacts() PeerFacts {
	return PeerFacts{
		PID:        3224579,
		Executable: "/home/sami/.mise/installs/github-sjawhar-oh-my-pi/18.13.2/bin/omp",
		WorkingDir: "/home/sami/src/oss-verifier",
		Cgroup:     "0::/user.slice/user-1000.slice/session-3.scope",
		StartedAt:  1793402648,
		ReadAt:     1793402651,
	}
}

// TestARequestCarriesTheKernelsFactsAboutTheAskingProcess is the round trip: what the helper read
// through its pinned handle is signed into the request object and comes back out of verification
// unchanged, so the broker reads the helper's reading rather than anything the asking process
// said.
func TestARequestCarriesTheKernelsFactsAboutTheAskingProcess(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	compact, err := Sign(key, "https://secrets.test", []AuthorizationDetail{
		{Type: KindAgentSecret, Identifier: "DEEL_API_KEY", Actions: []string{"inject"}},
	}, "deel sync for ACME-1", "", nil, now, WithPeerFacts(testPeerFacts()))
	if err != nil {
		t.Fatal(err)
	}
	obj, err := VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
	if err != nil {
		t.Fatalf("VerifyRequestObject: %v", err)
	}
	if obj.Peer == nil {
		t.Fatal("the verified request object carries no peer facts")
	}
	if *obj.Peer != testPeerFacts() {
		t.Fatalf("peer facts = %+v, want %+v", *obj.Peer, testPeerFacts())
	}
}

// TestARequestWithoutPeerFacts is every request object signed before this claim existed, and
// every one signed where no handle was pinned: it verifies, and names no peer.
func TestARequestWithoutPeerFacts(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	compact, err := Sign(key, "https://secrets.test", []AuthorizationDetail{
		{Type: KindAgentSecret, Identifier: "DEEL_API_KEY", Actions: []string{"inject"}},
	}, "no handle was pinned", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
	if err != nil {
		t.Fatalf("VerifyRequestObject: %v", err)
	}
	if obj.Peer != nil {
		t.Fatalf("peer facts = %+v, want none", *obj.Peer)
	}
	if strings.Contains(compact, "peer") {
		t.Fatal("the compact JWS names a peer claim it was not given")
	}
}

// TestVerifyRefusesPeerFactsThatCannotBeAKernelReading refuses a peer claim that no reading of
// /proc could have produced, so a page never renders one: the broker trusts the helper's key, and
// a shape it will render must still be bounded.
func TestVerifyRefusesPeerFactsThatCannotBeAKernelReading(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for name, mutate := range map[string]func(*PeerFacts){
		"a pid no process has":                   func(f *PeerFacts) { f.PID = 0 },
		"a negative pid":                         func(f *PeerFacts) { f.PID = -1 },
		"an executable that is not a path":       func(f *PeerFacts) { f.Executable = "omp" },
		"a working directory that is not a path": func(f *PeerFacts) { f.WorkingDir = "src/oss-verifier" },
		"a control character in a path":          func(f *PeerFacts) { f.Executable = "/usr/bin/\u0007omp" },
		"a path over 4096 bytes":                 func(f *PeerFacts) { f.WorkingDir = "/" + strings.Repeat("d", 4096) },
		"a cgroup over 4096 bytes":               func(f *PeerFacts) { f.Cgroup = strings.Repeat("c", 4097) },
		"no start time":                          func(f *PeerFacts) { f.StartedAt = 0 },
		"no read time":                           func(f *PeerFacts) { f.ReadAt = 0 },
		"read before the process started":        func(f *PeerFacts) { f.ReadAt = f.StartedAt - 1 },
	} {
		t.Run(name, func(t *testing.T) {
			facts := testPeerFacts()
			mutate(&facts)
			compact, err := Sign(key, "https://secrets.test", []AuthorizationDetail{
				{Type: KindAgentSecret, Identifier: "DEEL_API_KEY", Actions: []string{"inject"}},
			}, "reason", "", nil, now, WithPeerFacts(facts))
			if err != nil {
				t.Fatal(err)
			}
			_, err = VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
			if !errors.Is(err, ErrRequestInvalid) {
				t.Fatalf("VerifyRequestObject = %v, want ErrRequestInvalid", err)
			}
		})
	}
}
