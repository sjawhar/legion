package sandbox

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

const (
	testDispatchURL = "https://dispatch.internal.example"
	testBrokerURL   = "https://secrets.internal.example"
)

// addressRuntime is a runtime with testOptions, Dispatch and the secrets broker configured, then
// edited by edit (nil for none).
func addressRuntime(t *testing.T, edit func(*Options)) *Runtime {
	t.Helper()
	opts := testOptions()
	opts.DispatchURL, opts.DispatchToken = testDispatchURL, "dispatch-bearer"
	opts.AgentSecrets = &AgentSecrets{URL: testBrokerURL, Audience: "agent-secrets", TokenExpiry: time.Hour}
	if edit != nil {
		edit(&opts)
	}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// movedSince is what a runtime named now reads of a tester launched under launched: the stream its
// issue pod's launcher dials, then its generation-1 record.
func movedSince(t *testing.T, launched, now *Runtime) []movedAddress {
	t.Helper()
	pod := &corev1.Pod{Spec: podOf(t, launched, workerSpec(t))}
	record, err := launched.recordFor(claim.RoleTester, 1)
	if err != nil {
		t.Fatal(err)
	}
	s := &sandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{addressesAnnotation(claim.RoleTester): record}}}
	var moved []movedAddress
	if stream, ok := now.movedStream(pod, string(claim.RoleTester)); ok {
		moved = append(moved, stream)
	}
	environment, err := now.movedEnvironment(s, claim.RoleTester, 1)
	if err != nil {
		t.Fatal(err)
	}
	return append(moved, environment...)
}

// Each address the runtime hands a role process is compared on its own: a process launched under a
// configuration that differs in that one address alone, whether moved, configured only now or no
// longer configured, is named for exactly that address, held and handed each as namedURL names it
// (TestANamedAddressShowsOnlyItsSchemeHostAndPort holds what that name keeps), and one launched
// under the configuration the runtime has now is named for none, a `$` the kubelet would expand
// included.
func TestEachAddressARoleIsHandedIsComparedAlone(t *testing.T) {
	commas := []string{"nats://user,more:password@192.0.2.9:4222", "nats://user:password,more@192.0.2.10:4222"}
	credentialed := "https://legion:dispatch-password@dispatch.internal.example"
	for name, tc := range map[string]struct {
		launched, now func(*Options)
		want          *movedAddress
	}{
		"nothing moved": {},
		"the worker stream moved": {
			launched: moveStream,
			want:     &movedAddress{connectFlag, movedStreamURL, testOptions().StreamURL},
		},
		"the daemon's API moved": {
			launched: func(o *Options) { o.DaemonURL = movedDaemonURL },
			want:     &movedAddress{"LEGION_DAEMON_URL", movedDaemonURL, testOptions().DaemonURL},
		},
		"NATS moved": {
			launched: func(o *Options) { o.NATSURLs = []string{"nats://192.0.2.9:4222", "nats://192.0.2.10:4222"} },
			want:     &movedAddress{"ENVOY_NATS_URL", "nats://192.0.2.9:4222,nats://192.0.2.10:4222", "nats://192.0.2.250:4222"},
		},
		"Envoy moved": {
			launched: func(o *Options) { o.EnvoyURL = "http://192.0.2.9:9020" },
			want:     &movedAddress{"ENVOY_URL", "http://192.0.2.9:9020", testOptions().EnvoyURL},
		},
		"Dispatch moved": {
			launched: func(o *Options) { o.DispatchURL = "https://dispatch-old.internal.example" },
			want:     &movedAddress{"DISPATCH_URL", "https://dispatch-old.internal.example", testDispatchURL},
		},
		"the secrets broker moved": {
			launched: func(o *Options) { o.AgentSecrets.URL = "https://secrets-old.internal.example" },
			want:     &movedAddress{"AGENT_SECRETS_URL", "https://secrets-old.internal.example", testBrokerURL},
		},
		"Dispatch configured only now": {
			launched: func(o *Options) { o.DispatchURL, o.DispatchToken = "", "" },
			want:     &movedAddress{"DISPATCH_URL", "(unset)", testDispatchURL},
		},
		"the secrets broker no longer configured": {
			now:  func(o *Options) { o.AgentSecrets = nil },
			want: &movedAddress{"AGENT_SECRETS_URL", testBrokerURL, "(unset)"},
		},
		"a path change is stale though both names are the same": {
			launched: func(o *Options) { o.EnvoyURL = "http://192.0.2.9:9020/tenant/old" },
			now:      func(o *Options) { o.EnvoyURL = "http://192.0.2.9:9020/tenant/new" },
			want:     &movedAddress{"ENVOY_URL", "http://192.0.2.9:9020", "http://192.0.2.9:9020"},
		},
		"a moved address is named, never quoted": {
			now:  func(o *Options) { o.DispatchURL = credentialed },
			want: &movedAddress{"DISPATCH_URL", testDispatchURL, namedURL(credentialed)},
		},
		"a moved NATS list is named entry by entry, never split on its commas": {
			launched: func(o *Options) { o.NATSURLs = commas },
			want:     &movedAddress{"ENVOY_NATS_URL", namedNATSURLs(commas), "nats://192.0.2.250:4222"},
		},
		"an address the kubelet would expand, unmoved": {
			launched: func(o *Options) { o.EnvoyURL = "http://192.0.2.250:9020/$(HOME)" },
			now:      func(o *Options) { o.EnvoyURL = "http://192.0.2.250:9020/$(HOME)" },
		},
	} {
		t.Run(name, func(t *testing.T) {
			moved := movedSince(t, addressRuntime(t, tc.launched), addressRuntime(t, tc.now))
			var want []movedAddress
			if tc.want != nil {
				want = []movedAddress{*tc.want}
			}
			if !slices.Equal(moved, want) {
				t.Fatalf("moved addresses %+v, want %+v", moved, want)
			}
		})
	}
}

// A name is all an observation's detail and the address record ever carry of an address: its
// scheme, host and port, `xxxxx@` when it carries userinfo, `(unset)` for none, and `xxxxx` whole
// when it yields no scheme and host. Userinfo, path, query and fragment never enter it, a raw comma
// in the userinfo and an at sign in the path included, and a NATS list is named entry by entry.
func TestANamedAddressShowsOnlyItsSchemeHostAndPort(t *testing.T) {
	for address, want := range map[string]string{
		"":                      "(unset)",
		"nats://192.0.2.9:4222": "nats://192.0.2.9:4222",
		"nats://legion:nats-password@192.0.2.9:4222":                 "nats://xxxxx@192.0.2.9:4222",
		"nats://nats-token@192.0.2.10:4222":                          "nats://xxxxx@192.0.2.10:4222",
		"nats://user,more:password,more@192.0.2.11:4222":             "nats://xxxxx@192.0.2.11:4222",
		"nats://192.0.2.9:4222?token=nats-old-token":                 "nats://192.0.2.9:4222",
		"nats://192.0.2.9:4222/tenant%2Fnats-old-credential":         "nats://192.0.2.9:4222",
		"nats://192.0.2.9:4222/route@blue":                           "nats://192.0.2.9:4222",
		"http://192.0.2.9:9020?access_token=envoy-old-token":         "http://192.0.2.9:9020",
		"http://192.0.2.9:9020#access_token=envoy-old-token":         "http://192.0.2.9:9020",
		"http://192.0.2.9:9020/tenant/old":                           "http://192.0.2.9:9020",
		"http://user,more:password,more@192.0.2.9:9020":              "http://xxxxx@192.0.2.9:9020",
		"https://legion:dispatch-password@dispatch.internal.example": "https://xxxxx@dispatch.internal.example",
		"192.0.2.9:4222": "xxxxx",
	} {
		if got := namedURL(address); got != want {
			t.Errorf("namedURL(%q) = %q, want %q", address, got, want)
		}
	}
	list := []string{"nats://user,more:password@192.0.2.9:4222", "nats://192.0.2.10:4222?token=nats-query"}
	if got, want := namedNATSURLs(list), "nats://xxxxx@192.0.2.9:4222,nats://192.0.2.10:4222"; got != want {
		t.Errorf("namedNATSURLs(%q) = %q, want %q", list, got, want)
	}
	if got := namedNATSURLs(nil); got != "(unset)" {
		t.Errorf("namedNATSURLs(nil) = %q, want (unset)", got)
	}
}

// An observation's detail names each moved address as `<variable> <held>, now <handed>`, one after
// another (docs/kubernetes.md, "A pod whose address moved").
func TestAMovedAddressIsDescribedAsItHoldsThenWhatItIsHandedNow(t *testing.T) {
	moved := []movedAddress{{connectFlag, "tcp://192.0.2.5:13371", "tcp://192.0.2.7:13371"}, {"ENVOY_URL", "(unset)", "https://envoy.internal.example"}}
	want := "--connect tcp://192.0.2.5:13371, now tcp://192.0.2.7:13371; ENVOY_URL (unset), now https://envoy.internal.example"
	if got := describeMoved(moved); got != want {
		t.Fatalf("describeMoved = %q, want %q", got, want)
	}
}

// A record that cannot be read proves nothing about the generation's addresses: it is an error, which
// evaluate reads as stale, rather than a generation with nothing moved.
func TestAnUnreadableAddressRecordIsAnError(t *testing.T) {
	r := addressRuntime(t, nil)
	s := &sandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{addressesAnnotation(claim.RoleTester): "{not json"}}}
	if moved, err := r.movedEnvironment(s, claim.RoleTester, 1); err == nil || !strings.Contains(err.Error(), "is unreadable") {
		t.Fatalf("movedEnvironment = %+v, %v; want the record's unreadability", moved, err)
	}
}

// The record is a cluster object anyone who can read the Sandbox reads, so it holds no address's
// credentials: no userinfo, path, query or fragment, only each address's scheme, host and port and a
// digest of its whole value.
func TestTheAddressRecordHoldsNoCredential(t *testing.T) {
	r := addressRuntime(t, func(o *Options) {
		o.DaemonURL = "http://legion:daemon-password@192.0.2.5:13370"
		o.NATSURLs = []string{"nats://legion:nats-password@192.0.2.9:4222/tenant%2Fnats-path", "nats://192.0.2.10:4222?token=nats-query"}
		o.EnvoyURL = "http://envoy-user@192.0.2.6:9020/envoy-path#envoy-fragment"
		o.DispatchURL = "https://legion:dispatch-password@dispatch.internal.example"
		o.AgentSecrets.URL = "https://secrets.internal.example/?token=broker-query"
	})
	record, err := r.recordFor(claim.RoleTester, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"daemon-password", "nats-password", "nats-path", "nats-query", "envoy-user", "envoy-path", "envoy-fragment",
		"dispatch-password", "broker-query", "tenant", "token=",
	} {
		if strings.Contains(record, secret) {
			t.Errorf("the record %s holds %q", record, secret)
		}
	}
	for _, want := range []string{
		`"generation":7`, "http://xxxxx@192.0.2.5:13370", "nats://xxxxx@192.0.2.9:4222,nats://192.0.2.10:4222",
		"http://xxxxx@192.0.2.6:9020", addressDigest(r.envoyURL),
	} {
		if !strings.Contains(record, want) {
			t.Errorf("the record %s lacks %q", record, want)
		}
	}
}
