// packages/broker-client/src/wire.ts
//
// The names the secrets broker's RFC 8693 token exchange is spoken in, as both clients spell them:
// the Go client's `internal/broker/oauthwire` holds the same constants, and
// `packages/envoy/internal/brokerclient/testdata/wire.json` is the corpus that holds the two to
// one wire. A value here that drifts from that file fails this package's own suite.

/** The broker's token endpoint, where every trade is posted. */
export const TOKEN_PATH = "/v1/oauth/token";

/** RFC 8693's own grant name. */
export const GRANT_TYPE_TOKEN_EXCHANGE = "urn:ietf:params:oauth:grant-type:token-exchange";

/** The OAuth client every caller of the exchange asks as (`oauthwire.ExchangeClientID`). */
export const EXCHANGE_CLIENT_ID = "dispatch-token-exchange";

/** The broker's own form parameter: the session name the minted token writes under. RFC 8693 has
 *  none, and an entry that fixes no session requires it. */
export const FORM_SESSION = "session";

/** The `subject_token_type` a pod presents its projected Kubernetes service-account token under. */
export const SUBJECT_TOKEN_TYPE_KUBERNETES =
  "urn:envoy:params:oauth:token-type:kubernetes-service-account";

/** The `subject_token_type` a machine presents its own Google identity token under. */
export const SUBJECT_TOKEN_TYPE_GOOGLE = "urn:envoy:params:oauth:token-type:google-identity";

/** The `subject_token_type` a person presents their own Google identity token under, which only a
 *  sign-in carries. */
export const SUBJECT_TOKEN_TYPE_PERSON = "urn:envoy:params:oauth:token-type:person-google-identity";

/** The `actor_token_type` a machine's Google identity token takes beside a person's, under a type
 *  of its own because what it proves there is not who is asking but which box they ask from. */
export const ACTOR_TOKEN_TYPE_MACHINE = "urn:envoy:params:oauth:token-type:machine-google-identity";

/** The only `token_type` this grant mints: every token it answers is bound to the key the caller
 *  proved, and is presented as `Authorization: DPoP <token>` beside a proof of that key. */
export const TOKEN_TYPE_DPOP = "DPoP";
