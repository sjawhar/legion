//go:build natslive

package core_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The live proof of the host grant a hub's listener is given: the listener binary of this branch,
// run against a real server with the environment gcp:NatsHub renders for a hub whose tailnet name
// is set, answers a /v1 request carrying the hub's own tailnet address as its Host, which is how a
// tailnet client reaches it at all; it still refuses a name it was never told, so a page that
// rebinds its own name to the listener's address gains nothing; and the same listener started
// without ENVOY_ALLOWED_HOSTS — the environment the plugin rendered before this change — refuses
// the hub's own address with the same answer, while serving loopback, so the refusal is the host
// guard (cmd/listener/apiauth.go, servesHost) and not a listener that failed to come up. That last
// listener is the bug this issue fixes, reproduced beside the fix.

// hubTailnetDNSName is the tailnet DNS name of the hub under test, as the plugin's own tests name
// one; MagicDNS gives the hub <hostname>.<tailnetDnsName>, and core.TailnetHostName composes it.
const hubTailnetDNSName = "example-tailnet.ts.net"

// subscribeBody is a /v1 body that needs no state of its own, posted only to read the status the
// host guard decided; /v1/interests/subscribe is the route postListener's callers use, and the
// guard exempts /healthz, /metrics and /webhook/, so none of those would show anything.
const subscribeBody = `{"session_id":"` + liveSession + `","topics":[],"self_subscribed":true}`

func TestHubListenerServesItsTailnetName(t *testing.T) {
	listener := buildEnvoyCommand(t, "listener")
	own := core.TailnetHostName(liveMachineID, hubTailnetDNSName)
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Parallel()
			// The users a hub with a listener renders: the listener's, with a seed, and the
			// credential-free session user.
			seed, _, uri := startSplitServer(t, image.Ref)
			held := cmdtest.HoldPort(t)
			served := core.ListenerConfig{
				MachineID:    liveMachineID,
				ListenHost:   "127.0.0.1",
				AllowedHosts: []string{own},
			}
			process := startListenerConfig(t, listener, uri, seed, held, served)
			// Nothing is read from a listener still answering "starting": it must hold its durable
			// before a refusal can be the host guard's doing.
			t.Logf("healthy /healthz body: %s", requireHealthy(t, process, held.Port, liveHealthWindow, listenerHealthy))

			status, body := postListenerHost(t, held.Port, "/v1/interests/subscribe", subscribeBody, own)
			require.Equal(t, http.StatusOK, status,
				"the hub's listener refused its own tailnet address %s: %s\n%s", own, body, process.Output.String())

			other := "other." + hubTailnetDNSName
			status, body = postListenerHost(t, held.Port, "/v1/interests/subscribe", subscribeBody, other)
			require.Equal(t, http.StatusMisdirectedRequest, status,
				"the listener served a Host it was never told, %s: %s\n%s", other, body, process.Output.String())
			require.Contains(t, body, "ENVOY_ALLOWED_HOSTS",
				"the refusal does not name the list that would have served %s", other)

			// The negative control, the environment the plugin rendered before this change: the same
			// config without the list. It gets a server of its own, since two listeners of the same
			// machine would share one durable.
			bugSeed, _, bugURI := startSplitServer(t, image.Ref)
			bugHeld := cmdtest.HoldPort(t)
			unserved := served
			unserved.AllowedHosts = nil
			bug := startListenerConfig(t, listener, bugURI, bugSeed, bugHeld, unserved)
			t.Logf("healthy /healthz body of the listener without the list: %s",
				requireHealthy(t, bug, bugHeld.Port, liveHealthWindow, listenerHealthy))

			status, body = postListenerHost(t, bugHeld.Port, "/v1/interests/subscribe", subscribeBody, own)
			require.Equal(t, http.StatusMisdirectedRequest, status,
				"a listener without ENVOY_ALLOWED_HOSTS served the hub's own address %s: %s\n%s", own, body, bug.Output.String())
			require.Contains(t, body, "ENVOY_ALLOWED_HOSTS",
				"the refusal does not name the list the hub failed to render")

			// It answers loopback, so what it refused above is the Host and not a listener that
			// cannot serve anything.
			loopback := "127.0.0.1:" + strconv.Itoa(bugHeld.Port)
			status, body = postListenerHost(t, bugHeld.Port, "/v1/interests/subscribe", subscribeBody, loopback)
			require.Equal(t, http.StatusOK, status,
				"the listener without the list refused loopback too, so its refusal of %s proves nothing: %s\n%s",
				own, body, bug.Output.String())
		})
	}
}
