package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/ratelimit"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// withOAuth wires the OAuth provider on the test server's own database, its device login
// answering deviceLogin.
func withOAuth(t *testing.T, deviceLogin func(context.Context, url.Values, string) error) func(*api.Deps) {
	return func(d *api.Deps) {
		store := oauthstore.New(d.MachineLogin.Store.Pool)
		secret, err := store.Secret(context.Background())
		if err != nil {
			t.Fatalf("Secret: %v", err)
		}
		provider, err := oauth.New(oauth.Config{
			Secret: secret, DeviceLifetime: 15 * time.Minute, RefreshLifetime: time.Hour,
			VerificationURL: "https://dispatch.example/credentials/machine",
		}, store)
		if err != nil {
			t.Fatalf("oauth.New: %v", err)
		}
		d.OAuth = provider
		d.DeviceLogin = deviceLogin
		d.TokenHooks = oauth.TokenHooks{
			Before: func(context.Context, oauth2.GrantType, map[string]any) error { return nil },
			After:  func(context.Context, oauth2.GrantType, map[string]any) (map[string]any, error) { return nil, nil },
		}
	}
}

func postForm(t *testing.T, target string, form url.Values) (int, http.Header, map[string]any) {
	t.Helper()
	resp, err := http.Post(target, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

var deviceForm = url.Values{"client_id": {oauthwire.ClientID}, "scope": {"machine"}}

// signedDeviceForm is deviceForm carrying a person's fresh signed login request for ts, which the
// route verifies to spend that person's bucket before the device login runs.
func signedDeviceForm(t *testing.T, ts *testServer) url.Values {
	t.Helper()
	form := url.Values{"client_id": {oauthwire.ClientID}, "scope": {"machine"}}
	form.Set("request", signMachineLoginRequest(t, newSigningKey(t), ts.URL, testApprover, "example-host-devbox", ""))
	return form
}

func TestTheOAuthRoutesNeedTheProvider(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/v1/oauth/device_authorization", "/v1/oauth/token"} {
		if status, _, _ := postForm(t, ts.URL+path, deviceForm); status != http.StatusNotFound {
			t.Errorf("POST %s without a provider = %d, want 404", path, status)
		}
	}
}

// TestRegisterRefusesAProviderWithoutItsSteps: an OAuth provider wired without MachineLogin,
// DeviceLogin or either token hook would answer every request with a nil call, so Register refuses
// it at wiring.
func TestRegisterRefusesAProviderWithoutItsSteps(t *testing.T) {
	provider, err := oauth.New(oauth.Config{
		Secret: make([]byte, 32), DeviceLifetime: time.Minute, RefreshLifetime: time.Hour,
		VerificationURL: "https://dispatch.example/credentials/machine",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	login := func(context.Context, url.Values, string) error { return nil }
	before := func(context.Context, oauth2.GrantType, map[string]any) error { return nil }
	after := func(context.Context, oauth2.GrantType, map[string]any) (map[string]any, error) { return nil, nil }
	svc := &machine.Service{}
	for name, deps := range map[string]api.Deps{
		"no MachineLogin":      {OAuth: provider, DeviceLogin: login, TokenHooks: oauth.TokenHooks{Before: before, After: after}},
		"no DeviceLogin":       {OAuth: provider, MachineLogin: svc, TokenHooks: oauth.TokenHooks{Before: before, After: after}},
		"no TokenHooks.Before": {OAuth: provider, MachineLogin: svc, DeviceLogin: login, TokenHooks: oauth.TokenHooks{After: after}},
		"no TokenHooks.After":  {OAuth: provider, MachineLogin: svc, DeviceLogin: login, TokenHooks: oauth.TokenHooks{Before: before}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register with %s did not panic", name)
				}
			}()
			api.Register(http.NewServeMux(), deps)
		}()
	}
}

func TestADeviceAuthorizationSpendsItsAddressesLauncherBucket(t *testing.T) {
	adjust := withOAuth(t, func(context.Context, url.Values, string) error { return nil })
	ts := newTestServerWith(t, func(d *api.Deps) {
		adjust(d)
		d.LauncherLimits = &api.LauncherLimits{
			PerAddress: ratelimit.Limit{Every: 1500 * time.Millisecond, Burst: 1},
			PerLogin:   ratelimit.Limit{Every: time.Hour, Burst: 100},
		}
	})
	if status, _, body := postForm(t, ts.URL+"/v1/oauth/device_authorization", signedDeviceForm(t, ts)); status != http.StatusOK {
		t.Fatalf("first device authorization = %d %v, want 200", status, body)
	}
	status, header, body := postForm(t, ts.URL+"/v1/oauth/device_authorization", signedDeviceForm(t, ts))
	desc, _ := body["error_description"].(string)
	if status != http.StatusTooManyRequests || header.Get("Retry-After") != "2" || body["error"] != "invalid_request" || !strings.HasPrefix(desc, "RATE_LIMITED: ") {
		t.Fatalf("second device authorization = %d Retry-After=%q %v, want 429 invalid_request RATE_LIMITED with Retry-After 2", status, header.Get("Retry-After"), body)
	}
}

func TestADeviceLoginsRefusalNamesItsBrokerCode(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: bad signature", record.ErrRequestInvalid), http.StatusBadRequest, "REQUEST_INVALID"},
		{machine.ErrServiceLogsInWithToken, http.StatusBadRequest, "SERVICE_LOGS_IN_WITH_TOKEN"},
	} {
		ts := newTestServerWith(t, withOAuth(t, func(context.Context, url.Values, string) error { return c.err }))
		status, _, body := postForm(t, ts.URL+"/v1/oauth/device_authorization", signedDeviceForm(t, ts))
		if want := c.code + ": " + c.err.Error(); status != c.status || body["error"] != "invalid_request" || body["error_description"] != want {
			t.Errorf("refused with %v = %d %v, want %d invalid_request %q", c.err, status, body, c.status, want)
		}
	}
}

func TestTheTokenRouteIsTheProvidersTokenEndpoint(t *testing.T) {
	ts := newTestServerWith(t, withOAuth(t, func(context.Context, url.Values, string) error { return nil }))
	status, _, body := postForm(t, ts.URL+"/v1/oauth/token", url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {"no-such-code"}, "client_id": {oauthwire.ClientID},
	})
	if status != http.StatusBadRequest || body["error"] == nil {
		t.Fatalf("token with an unknown device code = %d %v, want a 400 OAuth error", status, body)
	}
}
