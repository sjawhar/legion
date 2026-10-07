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
func movedSince(t *testing.T, launched, now *Runtime) []string {
	t.Helper()
	pod := &corev1.Pod{Spec: podOf(t, launched, workerSpec(t), false)}
	record, err := launched.recordFor(1)
	if err != nil {
		t.Fatal(err)
	}
	s := &sandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{addressesAnnotation(claim.RoleTester): record}}}
	return slices.Concat(now.movedStream(pod, string(claim.RoleTester)), now.movedEnvironment(s, claim.RoleTester, 1))
}

// Each address the runtime hands a role process is compared on its own: a process launched under a
// configuration that differs in that one address alone, whether moved, configured only now or no
// longer configured, is named for exactly that address, and one launched under the configuration
// the runtime has now is named for none, a `$` the kubelet would expand included. No name carries a
// URL's userinfo, path, query or fragment.
func TestEachAddressARoleIsHandedIsComparedAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		launched, now func(*Options)
		want          string
	}{
		"nothing moved": {},
		"the worker stream moved": {
			launched: moveStream,
			want:     connectFlag + " " + movedStreamURL + ", now " + testOptions().StreamURL,
		},
		"the daemon's API moved": {
			launched: func(o *Options) { o.DaemonURL = movedDaemonURL },
			want:     "LEGION_DAEMON_URL " + movedDaemonURL + ", now " + testOptions().DaemonURL,
		},
		"NATS moved": {
			launched: func(o *Options) { o.NATSURLs = []string{"nats://192.0.2.9:4222", "nats://192.0.2.10:4222"} },
			want:     "ENVOY_NATS_URL nats://192.0.2.9:4222,nats://192.0.2.10:4222, now nats://192.0.2.250:4222",
		},
		"Envoy moved": {
			launched: func(o *Options) { o.EnvoyURL = "http://192.0.2.9:9020" },
			want:     "ENVOY_URL http://192.0.2.9:9020, now " + testOptions().EnvoyURL,
		},
		"Dispatch moved": {
			launched: func(o *Options) { o.DispatchURL = "https://dispatch-old.internal.example" },
			want:     "DISPATCH_URL https://dispatch-old.internal.example, now " + testDispatchURL,
		},
		"the secrets broker moved": {
			launched: func(o *Options) { o.AgentSecrets.URL = "https://secrets-old.internal.example" },
			want:     "AGENT_SECRETS_URL https://secrets-old.internal.example, now " + testBrokerURL,
		},
		"NATS moved, naming no user or password": {
			launched: func(o *Options) {
				o.NATSURLs = []string{"nats://legion:nats-password@192.0.2.9:4222", "nats://nats-token@192.0.2.10:4222"}
			},
			want: "ENVOY_NATS_URL nats://xxxxx@192.0.2.9:4222,nats://xxxxx@192.0.2.10:4222, now nats://192.0.2.250:4222",
		},
		"NATS moved, naming no userinfo with a raw comma in it": {
			launched: func(o *Options) {
				o.NATSURLs = []string{
					"nats://user,more:password@192.0.2.9:4222",
					"nats://user:password,more@192.0.2.10:4222",
					"nats://user,more:password,more@192.0.2.11:4222",
				}
			},
			want: "ENVOY_NATS_URL nats://xxxxx@192.0.2.9:4222,nats://xxxxx@192.0.2.10:4222,nats://xxxxx@192.0.2.11:4222, now nats://192.0.2.250:4222",
		},
		"NATS configured now names no raw-comma userinfo": {
			now: func(o *Options) {
				o.NATSURLs = []string{
					"nats://user,more:password@192.0.2.9:4222",
					"nats://user:password,more@192.0.2.10:4222",
					"nats://user,more:password,more@192.0.2.11:4222",
				}
			},
			want: "ENVOY_NATS_URL nats://192.0.2.250:4222, now nats://xxxxx@192.0.2.9:4222,nats://xxxxx@192.0.2.10:4222,nats://xxxxx@192.0.2.11:4222",
		},
		"NATS moved, naming no query token": {
			launched: func(o *Options) { o.NATSURLs = []string{"nats://192.0.2.9:4222?token=nats-old-token"} },
			want:     "ENVOY_NATS_URL nats://192.0.2.9:4222, now nats://192.0.2.250:4222",
		},
		"NATS configured now names no query token": {
			now:  func(o *Options) { o.NATSURLs = []string{"nats://192.0.2.9:4222?token=nats-current-token"} },
			want: "ENVOY_NATS_URL nats://192.0.2.250:4222, now nats://192.0.2.9:4222",
		},
		"Envoy moved to a URL with a query token": {
			launched: func(o *Options) { o.EnvoyURL = "http://192.0.2.9:9020?access_token=envoy-old-token" },
			want:     "ENVOY_URL http://192.0.2.9:9020, now " + testOptions().EnvoyURL,
		},
		"Envoy configured now names no query token": {
			now:  func(o *Options) { o.EnvoyURL = "http://192.0.2.9:9020?access_token=envoy-current-token" },
			want: "ENVOY_URL " + testOptions().EnvoyURL + ", now http://192.0.2.9:9020",
		},
		"NATS moved to a percent-encoded credential-shaped path": {
			launched: func(o *Options) { o.NATSURLs = []string{"nats://192.0.2.9:4222/tenant%2Fnats-old-credential"} },
			want:     "ENVOY_NATS_URL nats://192.0.2.9:4222, now nats://192.0.2.250:4222",
		},
		"Envoy path change is stale without naming either path": {
			launched: func(o *Options) { o.EnvoyURL = "http://192.0.2.9:9020/tenant/old" },
			now:      func(o *Options) { o.EnvoyURL = "http://192.0.2.9:9020/tenant/new" },
			want:     "ENVOY_URL http://192.0.2.9:9020, now http://192.0.2.9:9020",
		},
		"Envoy moved to a URL with a fragment token": {
			launched: func(o *Options) { o.EnvoyURL = "http://192.0.2.9:9020#access_token=envoy-old-token" },
			want:     "ENVOY_URL http://192.0.2.9:9020, now " + testOptions().EnvoyURL,
		},
		"NATS moved to a path containing an at sign": {
			launched: func(o *Options) { o.NATSURLs = []string{"nats://192.0.2.9:4222/route@blue"} },
			want:     "ENVOY_NATS_URL nats://192.0.2.9:4222, now nats://192.0.2.250:4222",
		},
		"Envoy moved to a URL with raw commas in its userinfo": {
			launched: func(o *Options) { o.EnvoyURL = "http://user,more:password,more@192.0.2.9:9020" },
			want:     "ENVOY_URL http://xxxxx@192.0.2.9:9020, now " + testOptions().EnvoyURL,
		},
		"the daemon's API moved to a URL with raw commas in its userinfo": {
			now:  func(o *Options) { o.DaemonURL = "http://user,more:password,more@192.0.2.9:13370" },
			want: "LEGION_DAEMON_URL " + testOptions().DaemonURL + ", now http://xxxxx@192.0.2.9:13370",
		},
		"Dispatch moved to a URL with a user and password, naming neither": {
			now:  func(o *Options) { o.DispatchURL = "https://legion:dispatch-password@dispatch.internal.example" },
			want: "DISPATCH_URL " + testDispatchURL + ", now https://xxxxx@dispatch.internal.example",
		},
		"Dispatch configured only now": {
			launched: func(o *Options) { o.DispatchURL, o.DispatchToken = "", "" },
			want:     "DISPATCH_URL (unset), now " + testDispatchURL,
		},
		"the secrets broker no longer configured": {
			now:  func(o *Options) { o.AgentSecrets = nil },
			want: "AGENT_SECRETS_URL " + testBrokerURL + ", now (unset)",
		},
		"an address the kubelet would expand, unmoved": {
			launched: func(o *Options) { o.EnvoyURL = "http://192.0.2.250:9020/$(HOME)" },
			now:      func(o *Options) { o.EnvoyURL = "http://192.0.2.250:9020/$(HOME)" },
		},
	} {
		t.Run(name, func(t *testing.T) {
			moved := movedSince(t, addressRuntime(t, tc.launched), addressRuntime(t, tc.now))
			var want []string
			if tc.want != "" {
				want = []string{tc.want}
			}
			if !slices.Equal(moved, want) {
				t.Fatalf("moved addresses %q, want %q", moved, want)
			}
		})
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
	record, err := r.recordFor(7)
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
