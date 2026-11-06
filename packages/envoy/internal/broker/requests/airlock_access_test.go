package requests

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/airlock"
)

// accessSlot is one slot the proxy encodes an exchange into, in the shape airlockproxy's
// azure.go writes: the prefix, and base64url with no padding of the endpoint, the scope and the
// application. It is written out here rather than built, because this package must never
// import the proxy that writes it - the proxy imports this one.
var accessSlot = SlotAzureAccessPrefix + base64.RawURLEncoding.EncodeToString([]byte(
	`{"endpoint":"https://login.microsoftonline.com/the-tenant/oauth2/v2.0/token","scope":"https://management.azure.com/.default","client_id":"the-application"}`))

// TestAnAccessPlaceholderIsMintedOnceAndGoesWithTheGrant: the proxy exchanges a federated token
// for an access token and hands the command a placeholder for it, so a second exchange of that
// scope - the same pod again, or another pod - has to answer the command the string it already
// holds. The row is the grant's own like every other placeholder: it opens a connection of that
// grant, it carries the slot the exchange was encoded into, and it goes when the grant ends.
//
// The name here is the fixture's ordinary one: what this answers is about the grant's row and
// the slot, and nothing of it reads the secret's kind.
func TestAnAccessPlaceholderIsMintedOnceAndGoesWithTheGrant(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("AUTO_TOKEN", "")}})
	_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	release, err := m.Values(t.Context(), grantID, enr, nil)
	if err != nil {
		t.Fatalf("the release: %v", err)
	}
	released := placeholderRows(t, m, grantID)

	first, err := m.AirlockAccessPlaceholder(t.Context(), grantID, "AUTO_TOKEN", accessSlot)
	if err != nil {
		t.Fatalf("AirlockAccessPlaceholder: %v", err)
	}
	wellFormed(t, "the access placeholder", first)
	if first == release.Airlock.Names["AUTO_TOKEN"].Slots[SlotValue] {
		t.Fatalf("the access placeholder is the name's own value placeholder %q; want one of its own", first)
	}
	second, err := m.AirlockAccessPlaceholder(t.Context(), grantID, "AUTO_TOKEN", accessSlot)
	if err != nil || second != first {
		t.Fatalf("the second exchange of this scope was answered %q, %v; want the first's %q and no new row", second, err, first)
	}

	rows := placeholderRows(t, m, grantID)
	if got := rows["AUTO_TOKEN/"+accessSlot]; got != first {
		t.Fatalf("the grant's rows hold %q under AUTO_TOKEN/%s; want the minted %q", got, accessSlot, first)
	}
	if len(rows) != len(released)+1 {
		t.Fatalf("the grant holds %d rows; want the release's %d and the one access row", len(rows), len(released))
	}

	// The row goes with the grant: it opens a connection of that grant, and the name it opens
	// carries the slot the exchange was encoded into, which is what lets another pod serve a
	// request that carries this placeholder.
	conn, err := m.AirlockConnection(t.Context(), first)
	if err != nil {
		t.Fatalf("AirlockConnection with the access placeholder: %v", err)
	}
	if conn.GrantID != grantID {
		t.Fatalf("the access placeholder opened grant %q; want %q", conn.GrantID, grantID)
	}
	if got := conn.Names["AUTO_TOKEN"].Slots[accessSlot]; got != first {
		t.Fatalf("the connection's AUTO_TOKEN slots = %v; want %s holding %q", conn.Names["AUTO_TOKEN"].Slots, accessSlot, first)
	}

	// A slot that is no access slot is refused: every other slot of a name is the broker's own
	// to mint, and this is the one the proxy writes.
	if _, err := m.AirlockAccessPlaceholder(t.Context(), grantID, "AUTO_TOKEN", SlotValue); err == nil {
		t.Fatalf("AirlockAccessPlaceholder minted a %q slot; want a refusal", SlotValue)
	}

	// And it ends with the grant, as every placeholder of it does.
	if err := m.RevokeGrant(t.Context(), grantID, enr); err != nil {
		t.Fatalf("revoke the grant: %v", err)
	}
	if deleted, err := m.deleteSpentPlaceholders(t.Context()); err != nil || deleted != int64(len(rows)) {
		t.Fatalf("the sweep deleted %d rows of the revoked grant, %v; want its %d", deleted, err, len(rows))
	}
	if _, err := m.AirlockConnection(t.Context(), first); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("the access placeholder of an ended grant opened %v; want ErrGrantNotLive", err)
	}
}

// TestAnAccessPlaceholderOfAGrantThatIsGoneIsRefused: the proxy mints one while it serves a
// request, and a grant revoked in the middle of that is no grant to write a row for.
func TestAnAccessPlaceholderOfAGrantThatIsGoneIsRefused(t *testing.T) {
	m, _, _, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("AUTO_TOKEN", "")}})
	if _, err := m.AirlockAccessPlaceholder(t.Context(), "00000000-0000-0000-0000-000000000000", "AUTO_TOKEN", accessSlot); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("AirlockAccessPlaceholder for no grant = %v; want ErrGrantNotLive", err)
	}
}
