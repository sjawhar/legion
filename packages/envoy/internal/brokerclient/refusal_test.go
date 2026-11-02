// packages/envoy/internal/brokerclient/refusal_test.go
//
// What a person running a deployment is told when the broker refuses the trade. Every caller of
// this package prints a broker failure through internal/redact, whose allow-list prints an
// error's own text only when it can prove that text carries nothing of the URL the deployment
// configured. A refusal that did not read as the answered HTTP refusal it is printed as "the
// request failed", and the broker's own words — the ones that say the request names no session,
// or that no entry lists this account — never reached the person, who had nothing to act on.

package brokerclient

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
)

// refusalBody is the broker's own words for the refusal that costs the most to diagnose: the
// deployment's entry fixes no session, so a trade that names none mints nothing.
const refusalBody = "the fleet example-fleet is listed with no session of its own, so the request names one"

// TestARefusalCarriesTheBrokersWordsThroughTheRedaction: the refusal reaches the caller as the
// answered HTTP refusal it is, so redact prints it whole beside a broker URL that carries no
// credential — a caller reads why its trade was refused without attaching a debugger.
func TestARefusalCarriesTheBrokersWordsThroughTheRedaction(t *testing.T) {
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 400, map[string]any{"error": "invalid_request", "error_description": refusalBody}
	})
	err := refusalClient(t, broker).Run(context.Background(), func(context.Context, *Token) error {
		t.Error("the loop handed its caller a token the broker refused to mint")
		return nil
	})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Run = %v, want the broker's refusal", err)
	}
	printed := redact.Error(err, broker.URL()).Error()
	if !strings.Contains(printed, refusalBody) || !strings.Contains(printed, "invalid_request") {
		t.Errorf("the redacted refusal is %q, want the broker's own words and its OAuth code in it", printed)
	}
}

// TestARefusalBesideABrokerURLThatCarriesACredentialNamesTheStatus: the same refusal where the
// configured URL holds a credential, which redact never prints any part of. The caller is told
// the broker answered and with what status — not merely that the request failed — and no byte of
// that URL, of the identity traded, or of the proof is in the message.
func TestARefusalBesideABrokerURLThatCarriesACredentialNamesTheStatus(t *testing.T) {
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 403, map[string]any{"error": "access_denied", "error_description": refusalBody}
	})
	err := refusalClient(t, broker).Run(context.Background(), func(context.Context, *Token) error {
		t.Error("the loop handed its caller a token the broker refused to mint")
		return nil
	})
	const carriesACredential = "https://machine:hunter2pw@secrets.example"
	printed := redact.Error(err, carriesACredential).Error()
	if printed != "the broker answered HTTP 403" {
		t.Errorf("the redacted refusal is %q, want the status alone: no text of a URL this one may not print", printed)
	}
	if strings.Contains(printed, "hunter2pw") || strings.Contains(printed, "projected.service.account.token") {
		t.Errorf("the redacted refusal is %q, which carries a credential", printed)
	}
}
