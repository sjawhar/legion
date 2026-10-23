package api

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// deviceAuthorization is POST /v1/oauth/device_authorization: a machine login's device
// authorization request (RFC 8628), public. It spends its source address's launcher bucket before
// the provider does any work, as the token login does (refuseAddress); then, as POST
// /v1/launcher-credentials does, it verifies the request object to read the person its login_hint
// names and spends that person's bucket (launcherLoginKey). A refused request is 429 with
// Retry-After. Deps.DeviceLogin then opens the login in the provider's transaction, and its
// refusals are OAuth errors (invalid_request) whose description names the broker's code: a request
// object that does not verify, repeats a jti or names another key than dpop_jkt REQUEST_INVALID,
// one naming a service SERVICE_LOGS_IN_WITH_TOKEN.
func (s *server) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if !s.launcherLimiter.addressAllows(r) {
		oauth.WriteRefusal(w, rateLimitedRefusal(s.launcherLimiter.perAddress.Every()))
		return
	}
	s.deps.OAuth.DeviceAuthorize(w, r, func(ctx context.Context, form url.Values, userCode string) error {
		if err := s.spendDeviceLoginBucket(form); err != nil {
			return deviceLoginRefusal(err)
		}
		return deviceLoginRefusal(s.deps.DeviceLogin(ctx, form, userCode))
	})
}

// spendDeviceLoginBucket verifies form's request object, refusing one naming a service before it
// reaches the limiter, and spends one request of the person:<login> bucket it names: a
// RATE_LIMITED refusal when that bucket is empty.
func (s *server) spendDeviceLoginBucket(form url.Values) error {
	obj, err := record.VerifyRequestObject(form.Get("request"), s.deps.MachineLogin.Audience, s.deps.MachineLogin.Skew, time.Now())
	if err != nil {
		return err
	}
	if obj.Service() != "" {
		return machine.ErrServiceLogsInWithToken
	}
	if !s.launcherLimiter.perLogin.AllowAt(launcherLoginKey(obj), time.Now()) {
		return rateLimitedRefusal(s.launcherLimiter.perLogin.Every())
	}
	return nil
}

// deviceLoginRefusal is the OAuth refusal a device login's error answers with the status and code
// POST /v1/launcher-credentials answers it with. An error already a refusal, and one the broker
// has no code for, pass through (the provider answers the second 500 server_error).
func deviceLoginRefusal(err error) error {
	var refusal *oauth.Refusal
	switch {
	case err == nil, errors.As(err, &refusal):
		return err
	case errors.Is(err, machine.ErrServiceLogsInWithToken):
		return &oauth.Refusal{Status: http.StatusBadRequest, Code: "SERVICE_LOGS_IN_WITH_TOKEN", Message: err.Error()}
	case errors.Is(err, record.ErrRequestInvalid):
		return &oauth.Refusal{Status: http.StatusBadRequest, Code: "REQUEST_INVALID", Message: err.Error()}
	default:
		return err
	}
}

// rateLimitedRefusal is a device login's 429 RATE_LIMITED refusal from a bucket refilling one
// request every every.
func rateLimitedRefusal(every time.Duration) *oauth.Refusal {
	return &oauth.Refusal{
		Status: http.StatusTooManyRequests, Code: "RATE_LIMITED",
		Message:    "too many launcher credential requests; try again later",
		RetryAfter: retryAfterSeconds(every),
	}
}

// oauthToken is POST /v1/oauth/token: a machine login's token endpoint, public. It answers the
// device code grant once the login's person has approved it, and the refresh grant, each with a
// DPoP proof from the machine's key, and its answers are OAuth's own (RFC 6749 section 5). The
// provider checks the proof's htu against the URL it rebuilds from the request's scheme, Host and
// X-Forwarded-Proto, so the request it is handed carries BROKER_PUBLIC_URL's scheme and host and
// no forwarding headers: the proof names PublicURL + /v1/oauth/token, as every other broker proof
// names PublicURL + its path, whatever the hop in front of the broker sends.
func (s *server) oauthToken(w http.ResponseWriter, r *http.Request) {
	s.deps.OAuth.Token(w, s.atPublicURL(r), s.deps.TokenHooks)
}

// atPublicURL is r as if it had reached BROKER_PUBLIC_URL directly: its URL's scheme and host and
// its Host are the public URL's, it carries TLS state exactly when that URL is https, and it has
// no X-Forwarded-Proto or X-Forwarded-Host.
func (s *server) atPublicURL(r *http.Request) *http.Request {
	public, err := url.Parse(s.deps.PublicURL)
	if err != nil {
		// config.Load refuses a BROKER_PUBLIC_URL that does not parse.
		panic("api: PublicURL does not parse: " + err.Error())
	}
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host, out.Host = public.Scheme, public.Host, public.Host
	out.TLS = nil
	if public.Scheme == "https" {
		out.TLS = &tls.ConnectionState{}
	}
	out.Header.Del("X-Forwarded-Proto")
	out.Header.Del("X-Forwarded-Host")
	return out
}
