//go:build natslive

package core_test

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The tests in this file start nats-server containers through testnats, so they run only under the
// natslive tag: a plain `go test ./...`, as a release job runs it, needs no Docker. The
// envoy-infra-go job runs them with `go test -tags natslive ./core/ ./seed/`.

// liveUsers is an authorization in the placeholder's shape with fresh keys, whose seeds the
// real-server tests connect with; the fallback is grantable apart from everything else.
type liveUsers struct {
	listenerSeed, daemonSeed string
	authorization            core.Authorization
}

func newLiveUsers(t *testing.T, fallback bool) liveUsers {
	t.Helper()
	listenerSeed, listener := testnats.User(t)
	daemonSeed, daemon := testnats.User(t)
	_, fallbackKey := testnats.User(t)
	a := core.Authorization{Users: []core.User{
		{
			Name:      "example-listener",
			NKey:      listener,
			Publish:   &core.Rules{Allow: []string{"envoy.example.>"}, Deny: []string{"envoy.example.private"}},
			Subscribe: &core.Rules{Allow: []string{"_INBOX.>", "notifications.café.>"}},
			HasSeed:   true,
		},
		{
			Name:      "example-daemon",
			NKey:      daemon,
			Publish:   &core.Rules{Allow: []string{"$JS.API.>"}},
			Subscribe: &core.Rules{Allow: []string{"_INBOX.>", "legion.work workers"}},
			HasSeed:   true,
		},
		{
			Name:      "example-fallback",
			NKey:      fallbackKey,
			Publish:   &core.Rules{Allow: []string{"fallback.>"}},
			Subscribe: &core.Rules{Allow: []string{"_INBOX.>", "fallback.>"}},
		},
		{Name: "example-pending", HasSeed: true},
	}}
	if fallback {
		a.NoAuthUser = "example-fallback"
	}
	return liveUsers{listenerSeed: listenerSeed, daemonSeed: daemonSeed, authorization: a}
}

func (u liveUsers) render(t *testing.T) string {
	t.Helper()
	text, err := u.authorization.Render(header)
	require.NoError(t, err)
	return text
}

// connect connects to uri, as the user whose seed is seed when it is not empty, and returns the
// connection with the channel the server's asynchronous errors arrive on, in the order it sent
// them.
func connect(t *testing.T, uri, seed string) (*natsgo.Conn, <-chan error) {
	t.Helper()
	errs := make(chan error, 16)
	conn, err := testnats.ConnectSeed(t, uri, seed,
		natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errs <- err }))
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	return conn, errs
}

// requireViolation requires the next error the server sent to be exactly its permissions violation
// for want ("Publish to …", "Subscription to …"), as nats.go reports it. The whole text is
// compared because the server's violations nest: a refused queue subscription is "Subscription to
// "s" using queue "q"", which holds the plain subscription's text. Every allowed operation a test
// makes comes before the refused one, so a violation for it would arrive first and fail here.
func requireViolation(t *testing.T, conn *natsgo.Conn, errs <-chan error, want string) {
	t.Helper()
	require.NoError(t, conn.Flush())
	select {
	case err := <-errs:
		require.ErrorIs(t, err, natsgo.ErrPermissionViolation)
		require.EqualError(t, err, "nats: permissions violation: Permissions Violation for "+want)
	case <-time.After(5 * time.Second):
		t.Fatalf("the server reported no permissions violation within 5s, want one for %s", want)
	}
}

// The rendered block, as a server's whole configuration, on both the 2.10 image and the hub's.
func TestRenderedAuthorizationOnRealServers(t *testing.T) {
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			t.Run("without a fallback", func(t *testing.T) {
				users := newLiveUsers(t, false)
				_, uri := testnats.StartImageConfig(t, image.Ref, users.render(t), []string{"-js"}, false)

				t.Run("a client with its user's seed connects", func(t *testing.T) {
					conn, _ := connect(t, uri, users.listenerSeed)
					require.NoError(t, conn.Flush())
				})
				t.Run("a client with no credential is refused", func(t *testing.T) {
					_, err := testnats.ConnectSeed(t, uri, "")
					require.ErrorIs(t, err, natsgo.ErrAuthorization)
				})
				t.Run("a publish outside the user's grant is a permissions violation", func(t *testing.T) {
					conn, errs := connect(t, uri, users.listenerSeed)
					require.NoError(t, conn.Publish("envoy.example.allowed", nil))
					require.NoError(t, conn.Publish("outside.example", nil))
					requireViolation(t, conn, errs, `Publish to "outside.example"`)
					require.NoError(t, conn.Publish("envoy.example.private", nil))
					requireViolation(t, conn, errs, `Publish to "envoy.example.private"`)
				})
				t.Run("the subscribe grant holds the non-ASCII subject as written", func(t *testing.T) {
					conn, errs := connect(t, uri, users.listenerSeed)
					_, err := conn.SubscribeSync("notifications.café.x")
					require.NoError(t, err)
					_, err = conn.SubscribeSync("notifications.cafe.x")
					require.NoError(t, err)
					requireViolation(t, conn, errs, `Subscription to "notifications.cafe.x"`)
				})
				t.Run("the queue-scoped entry admits only its queue group", func(t *testing.T) {
					conn, errs := connect(t, uri, users.daemonSeed)
					_, err := conn.QueueSubscribeSync("legion.work", "workers")
					require.NoError(t, err)
					_, err = conn.SubscribeSync("legion.work")
					require.NoError(t, err)
					requireViolation(t, conn, errs, `Subscription to "legion.work"`)
				})
			})
			t.Run("with a fallback, a client with no credential has the fallback's grant", func(t *testing.T) {
				users := newLiveUsers(t, true)
				_, uri := testnats.StartImageConfig(t, image.Ref, users.render(t), []string{"-js"}, true)
				conn, errs := connect(t, uri, "")
				require.NoError(t, conn.Publish("fallback.allowed", nil))
				require.NoError(t, conn.Publish("envoy.example.listener-only", nil))
				requireViolation(t, conn, errs, `Publish to "envoy.example.listener-only"`)
			})
		})
	}
}

// A subscribe deny, on both images: the plain form Validate accepts refuses a queue subscription
// and a plain one to a subject it covers, and neither receives a message published there, while an
// allowed subscription does. The queue-scoped form, which nats-server before 2.15 enforces more
// loosely than it reads, never reaches a server: Render refuses it.
func TestSubscribeDenyOnRealServers(t *testing.T) {
	subscriberSeed, subscriber := testnats.User(t)
	publisherSeed, publisher := testnats.User(t)
	authorization := func(deny ...string) core.Authorization {
		return core.Authorization{Users: []core.User{
			{
				Name: "example-subscriber", NKey: subscriber, HasSeed: true,
				Publish:   &core.Rules{Allow: []string{"_INBOX.>"}},
				Subscribe: &core.Rules{Allow: []string{">"}, Deny: deny},
			},
			{
				Name: "example-publisher", NKey: publisher, HasSeed: true,
				Publish:   &core.Rules{Allow: []string{">"}},
				Subscribe: &core.Rules{Allow: []string{"_INBOX.>"}},
			},
		}}
	}
	_, err := authorization("secret.>", "secret.x q1").Render(header)
	require.ErrorContains(t, err, `users[0] "example-subscriber": subscribe.deny[1] "secret.x q1" names a queue group`)
	_, err = authorization("secret.x q1").Render(header)
	require.ErrorContains(t, err, `users[0] "example-subscriber": subscribe.deny[0] "secret.x q1" names a queue group`)

	config, err := authorization("secret.>").Render(header)
	require.NoError(t, err)
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			_, uri := testnats.StartImageConfig(t, image.Ref, config, []string{"-js"}, false)
			conn, errs := connect(t, uri, subscriberSeed)
			allowed, err := conn.SubscribeSync("public.x")
			require.NoError(t, err)
			queued, err := conn.QueueSubscribeSync("secret.x", "q2")
			require.NoError(t, err)
			requireViolation(t, conn, errs, `Subscription to "secret.x" using queue "q2"`)
			plain, err := conn.SubscribeSync("secret.x")
			require.NoError(t, err)
			requireViolation(t, conn, errs, `Subscription to "secret.x"`)

			pub, _ := connect(t, uri, publisherSeed)
			require.NoError(t, pub.Publish("secret.x", []byte("denied")))
			require.NoError(t, pub.Publish("public.x", []byte("allowed")))
			require.NoError(t, pub.Flush())
			msg, err := allowed.NextMsg(5 * time.Second)
			require.NoError(t, err, "the allowed subscription receives what the publisher sent")
			require.Equal(t, "allowed", string(msg.Data))
			for _, sub := range []*natsgo.Subscription{queued, plain} {
				_, err := sub.NextMsg(500 * time.Millisecond)
				require.ErrorIs(t, err, natsgo.ErrTimeout, "a denied subscription to %q receives nothing", sub.Subject)
			}
		})
	}
}
