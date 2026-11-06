// Package redirects hands out the HTTP client a call carrying a credential is made on: one
// that refuses redirects.
package redirects

import "net/http"

// Refused is client with redirects refused, which is what every call carrying a credential of
// this deployment's own is made on - an api-key or x-api-key header, a bearer, and the Entra
// exchange's federated assertion. net/http strips Authorization on a redirect to another domain
// but strips neither api-key nor x-api-key, and a transport that sets Authorization itself puts
// it back after the strip, so a service answering 3xx - or anyone who can answer for it - would
// otherwise have this process present that credential to a host it was never configured with.
// The last response is handed back instead, which each caller reads as the non-2xx it is.
//
// It is a copy and never the caller's own client: the client a config carries belongs to
// whoever passed it (a test passes httptest's), and a CheckRedirect written onto it would
// change how that caller's other calls behave. The copy shares the transport, so it shares the
// connection pool and costs nothing.
func Refused(client *http.Client) *http.Client {
	refusing := *client
	refusing.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &refusing
}
