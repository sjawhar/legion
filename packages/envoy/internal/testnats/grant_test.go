package testnats

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// Each fixture is read with the machine id in place of every placeholder: the listener's durable,
// queue and lease key name the machine, and nothing names the placeholder.
func TestTheGrantFixturesNameTheMachine(t *testing.T) {
	for name, grant := range map[string]Grant{"listener": ListenerGrant(t, "box-a"), "host": HostGrant(t, "box-a")} {
		for _, want := range []string{
			"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.listener-box-a",
			"$KV.envoy_listener.lease.listener-box-a",
		} {
			if !slices.Contains(grant.Publish.Allow, want) {
				t.Errorf("the %s grant does not allow publishing to %s: %q", name, want, grant.Publish.Allow)
			}
		}
		if !slices.Contains(grant.Subscribe.Allow, "notifications.role.> envoy-listener-box-a") {
			t.Errorf("the %s grant does not allow the machine's role-lane queue: %q", name, grant.Subscribe.Allow)
		}
		for _, subject := range slices.Concat(grant.Publish.Allow, grant.Subscribe.Allow) {
			if strings.Contains(subject, MachinePlaceholder) {
				t.Errorf("the %s grant still names the placeholder: %q", name, subject)
			}
		}
	}
	if host := HostGrant(t, "box-a"); !slices.Contains(host.Subscribe.Allow, "notifications.>") {
		t.Errorf("the host grant does not allow the sessions' subscription to every topic: %q", host.Subscribe.Allow)
	}
}

// A granted user may publish what its grant allows and is refused anything else, while the admin
// publishes anything; a client with no credential is refused.
func TestAGrantedServerHoldsEachUserToItsGrant(t *testing.T) {
	seed, public := User(t)
	granted := StartGranted(t, GrantedUser{Public: public, Grant: Grant{
		Publish:   GrantRules{Allow: []string{"allowed.>"}},
		Subscribe: GrantRules{Allow: []string{"_INBOX.>"}},
	}})
	if strings.Contains(granted.URL(), "@") {
		t.Fatalf("the server's URL %s names a user", granted.URL())
	}
	if _, err := ConnectSeed(t, granted.URL(), ""); !errors.Is(err, natsgo.ErrAuthorization) {
		t.Fatalf("a client with no credential: %v, want refused", err)
	}

	errs := make(chan error, 4)
	user, err := ConnectSeed(t, granted.URL(), seed, natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errs <- err }))
	if err != nil {
		t.Fatalf("connect as the granted user: %v", err)
	}
	t.Cleanup(user.Close)
	admin, err := natsgo.Connect(granted.AdminURL, natsgo.Timeout(dialTimeout), natsgo.NoReconnect())
	if err != nil {
		t.Fatalf("connect as the admin: %v", err)
	}
	t.Cleanup(admin.Close)
	watch, err := admin.SubscribeSync(">")
	if err != nil {
		t.Fatalf("the admin's subscription: %v", err)
	}
	if err := admin.Flush(); err != nil {
		t.Fatalf("flush the admin: %v", err)
	}

	for _, subject := range []string{"allowed.one", "refused.one"} {
		if err := user.Publish(subject, nil); err != nil {
			t.Fatalf("publish %s: %v", subject, err)
		}
	}
	if err := user.Flush(); err != nil {
		t.Fatalf("flush the user: %v", err)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, natsgo.ErrPermissionViolation) || !strings.Contains(err.Error(), `Publish to "refused.one"`) {
			t.Fatalf("the server's answer to the refused publish: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not refuse the publish outside the grant")
	}
	if err := admin.Publish("refused.admin", nil); err != nil {
		t.Fatalf("the admin's publish: %v", err)
	}
	var got []string
	for range 2 {
		msg, err := watch.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("the admin received %q, then: %v", got, err)
		}
		got = append(got, msg.Subject)
	}
	if want := []string{"allowed.one", "refused.admin"}; !slices.Equal(got, want) {
		t.Fatalf("the admin received %q, want %q", got, want)
	}
}
