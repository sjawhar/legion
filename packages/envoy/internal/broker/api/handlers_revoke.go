// handlers_revoke.go: POST /v1/oauth/revoke — RFC 7009 token revocation, the route one of the
// broker's own access tokens is ended through, an exchanged one included. It is authenticated by
// the caller's own Kubernetes ServiceAccount token (accountAuth) and needs the relaysDecisions
// power: ending one of the broker's tokens is the same authority as ending a machine login or a
// grant, not the authority to read one back.
package api

import (
	"log/slog"
	"net/http"
)

// revokeOAuthToken is POST /v1/oauth/revoke: the RFC 7009 request, a form whose token parameter
// names the token to end, whose optional token_type_hint says which kind to look for first, and
// whose client_id names the client the token was issued to — dispatch-token-exchange for an
// exchanged token, the machine-login client for a dev box session's. The provider answers it: a
// token issued to another client than the one named is refused, and a token the broker never
// issued, like one it ends here, is 200 (Section 2.2), so the answer tells a caller holding a
// string at random nothing.
//
// It spends no bucket of its own: authenticate has already spent its source address's bucket
// (DefaultAccountLimit), the one every accountAuth route shares, before the caller's token was
// verified. The listed caller is logged, with the client whose token it named and never the
// token itself, because this route acts — it ends a credential someone is holding — as every
// other relayed revocation is logged.
func (s *server) revokeOAuthToken(w http.ResponseWriter, r *http.Request, callerName string) {
	if !readOAuthForm(w, r) {
		return
	}
	slog.Info("token revocation relayed", "caller", callerName, "client_id", r.PostForm.Get("client_id"),
		"token_type_hint", r.PostForm.Get("token_type_hint"))
	s.deps.OAuth.Revoke(w, withoutCallerToken(r))
}

// withoutCallerToken is r with its Authorization header removed, which is what the provider is
// handed.
//
// The broker has already read that header and is done with it: it carries the caller's own
// Kubernetes ServiceAccount token, this route's credential (accountAuth), which is what RFC 7009
// Section 2.1's client authentication is answered by here. The OAuth library reads any
// Authorization header as a client credential and refuses every scheme but Basic
// (client_authentication.go), so leaving it on would refuse, as a malformed client credential,
// every request this route had just authenticated. The form's own client_id is left alone: it
// names which client's tokens the request may end, and the library still refuses a token issued
// to any other.
func withoutCallerToken(r *http.Request) *http.Request {
	out := r.Clone(r.Context())
	out.Header.Del("Authorization")
	return out
}
