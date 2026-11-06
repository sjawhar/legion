package testnats

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// MachinePlaceholder stands for the machine id in the grant fixtures: each fixture holds the grant
// envoy-infra renders for the machine id MachinePlaceholder, and ListenerGrant and HostGrant put a
// machine id in its place.
const MachinePlaceholder = "@MACHINE_ID@"

// The grant fixtures, in testdata/: the grants envoy-infra's core.ListenerPermissions and
// core.HostPermissions render for MachinePlaceholder, so a test here can run a process under
// exactly the grant a deployment gives it. This module cannot import envoy-infra, so the infra
// module's TestTheTestGrantFixturesAreTheRenderedGrants (packages/envoy/infra/core) renders each
// grant and fails unless its fixture holds the same bytes (EncodeGrant). After a change to a grant,
// rewrite them from packages/envoy/infra with
//
//	go test ./core -run TestTheTestGrantFixturesAreTheRenderedGrants -update
const (
	ListenerGrantFixture = "listener-permissions.json"
	HostGrantFixture     = "host-permissions.json"
)

//go:embed testdata/listener-permissions.json
var listenerGrantFixture []byte

//go:embed testdata/host-permissions.json
var hostGrantFixture []byte

// Grant is one NATS user's permissions as a grant fixture holds them, in the shape of a
// nats-server user's permissions object.
type Grant struct {
	Publish   GrantRules `json:"publish"`
	Subscribe GrantRules `json:"subscribe"`
}

// GrantRules are one direction's subjects. A subscribe entry may be "<subject> <queue>", which
// scopes it to that queue group.
type GrantRules struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny,omitempty"`
}

// EncodeGrant is the bytes of the grant fixture holding grant: indented JSON, its subjects'
// characters unescaped, and a final newline.
func EncodeGrant(grant Grant) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(grant); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ListenerGrant is envoy-listener's grant as machine machineID (core.ListenerPermissions), read
// from its fixture.
func ListenerGrant(t testing.TB, machineID string) Grant {
	t.Helper()
	return fixtureGrant(t, ListenerGrantFixture, listenerGrantFixture, machineID)
}

// HostGrant is the grant of a host's one NATS user as machine machineID, which its listener and
// every session on the host share (core.HostPermissions), read from its fixture.
func HostGrant(t testing.TB, machineID string) Grant {
	t.Helper()
	return fixtureGrant(t, HostGrantFixture, hostGrantFixture, machineID)
}

// fixtureGrant decodes the fixture name holds as data, with machineID in place of
// MachinePlaceholder in every subject.
func fixtureGrant(t testing.TB, name string, data []byte, machineID string) Grant {
	t.Helper()
	var grant Grant
	if err := json.Unmarshal(data, &grant); err != nil {
		t.Fatalf("decode the grant fixture %s: %v", name, err)
	}
	if len(grant.Publish.Allow) == 0 || len(grant.Subscribe.Allow) == 0 {
		t.Fatalf("the grant fixture %s allows no publish or no subscribe: a server reads an empty allow as everything", name)
	}
	for _, rules := range []*GrantRules{&grant.Publish, &grant.Subscribe} {
		for _, subjects := range [][]string{rules.Allow, rules.Deny} {
			for index, subject := range subjects {
				subjects[index] = strings.ReplaceAll(subject, MachinePlaceholder, machineID)
			}
		}
	}
	return grant
}

// GrantedUser is one nkey user of a server StartGranted runs: its public key (User) and the grant
// it holds.
type GrantedUser struct {
	Public string
	Grant  Grant
}

// grantedAdmin is the name and the password of the one user of a StartGranted server that holds
// every permission.
const grantedAdmin = "admin"

// Granted is a NATS test server StartGranted runs. Its URL names no user: a granted user dials it
// with its seed. AdminURL is the same server as its admin, a user holding every permission, which
// the test's own clients use (a publisher, a reader of what a process wrote).
type Granted struct {
	*Server
	AdminURL string
}

// StartGranted runs a NATS server with JetStream whose users are each of users, holding exactly
// its grant, and the admin, shut down when the test ends, and returns it once it refuses a client
// with no credential.
func StartGranted(t testing.TB, users ...GrantedUser) *Granted {
	t.Helper()
	entries := []string{fmt.Sprintf("{ user: %q, password: %q }", grantedAdmin, grantedAdmin)}
	for _, user := range users {
		entries = append(entries, fmt.Sprintf("{ nkey: %q, permissions: { publish: %s, subscribe: %s } }",
			user.Public, grantRulesConfig(user.Grant.Publish), grantRulesConfig(user.Grant.Subscribe)))
	}
	server, uri := StartConfig(t, nkeyConfig(strings.Join(entries, ", ")), []string{"-js"}, false)
	admin, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse the server's URL %s: %v", uri, err)
	}
	admin.User = url.UserPassword(grantedAdmin, grantedAdmin)
	return &Granted{Server: server, AdminURL: admin.String()}
}

// grantRulesConfig is rules as a nats-server configuration's permissions of one direction.
func grantRulesConfig(rules GrantRules) string {
	config := "{ allow: " + quotedList(rules.Allow)
	if len(rules.Deny) > 0 {
		config += ", deny: " + quotedList(rules.Deny)
	}
	return config + " }"
}

func quotedList(subjects []string) string {
	quoted := make([]string, len(subjects))
	for index, subject := range subjects {
		quoted[index] = fmt.Sprintf("%q", subject)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
