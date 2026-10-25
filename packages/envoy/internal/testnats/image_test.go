//go:build natslive

package testnats

import (
	"errors"
	"fmt"
	"testing"

	natsgo "github.com/nats-io/nats.go"
)

// StartImageConfig gives the server config as its whole configuration and serverArgs after it, on
// either image: the server refuses a client with no credential, as config's one nkey user makes it,
// admits that user by its seed, and answers JetStream, which the -js among serverArgs turns on.
func TestStartImageConfigServesTheConfigWithTheArguments(t *testing.T) {
	for _, image := range Images {
		t.Run(image.Name, func(t *testing.T) {
			seed, public := User(t)
			config := fmt.Sprintf("authorization {\n  users: [ { nkey: %q } ]\n}\n", public)
			_, uri := StartImageConfig(t, image.Ref, config, []string{"-js"}, false)
			conn, err := ConnectSeed(t, uri, seed)
			if err != nil {
				t.Fatalf("connect with the seed: %v", err)
			}
			defer conn.Close()
			js, err := conn.JetStream()
			if err != nil {
				t.Fatalf("JetStream context: %v", err)
			}
			waitForJetStream(t, func() error {
				_, err := js.AccountInfo()
				return err
			})
		})
	}
}

// With no serverArgs the server runs without JetStream: StartImageConfig replaces the NATS
// module's own default arguments (-DV -js) rather than adding to them. nats-server enables
// JetStream before it accepts a client (server/server.go Start), so the first answer is final.
func TestStartImageConfigRunsNoArgumentItWasNotGiven(t *testing.T) {
	for _, image := range Images {
		t.Run(image.Name, func(t *testing.T) {
			seed, public := User(t)
			config := fmt.Sprintf("authorization {\n  users: [ { nkey: %q } ]\n}\n", public)
			_, uri := StartImageConfig(t, image.Ref, config, nil, false)
			conn, err := ConnectSeed(t, uri, seed)
			if err != nil {
				t.Fatalf("connect with the seed: %v", err)
			}
			defer conn.Close()
			js, err := conn.JetStream()
			if err != nil {
				t.Fatalf("JetStream context: %v", err)
			}
			if _, err := js.AccountInfo(); !errors.Is(err, natsgo.ErrJetStreamNotEnabled) {
				t.Fatalf("JetStream account info on a server given no -js: %v, want %v", err, natsgo.ErrJetStreamNotEnabled)
			}
		})
	}
}

// With anonymous set, StartImageConfig returns once the server admits a client with no
// credential, as a configuration naming a no_auth_user makes it, and that client stays admitted.
func TestStartImageConfigWaitsForAnAnonymousServerToAdmitAClient(t *testing.T) {
	for _, image := range Images {
		t.Run(image.Name, func(t *testing.T) {
			_, public := User(t)
			config := fmt.Sprintf("authorization {\n  users: [ { nkey: %q } ]\n}\nno_auth_user: %q\n", public, public)
			_, uri := StartImageConfig(t, image.Ref, config, nil, true)
			conn, err := ConnectSeed(t, uri, "")
			if err != nil {
				t.Fatalf("a client with no credential was refused after the wait: %v", err)
			}
			conn.Close()
		})
	}
}
