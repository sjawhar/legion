// Package wire is the broker's response shapes: the JSON bodies the routes Dispatch relays
// answer with, each declared once here and exported so that Dispatch's OpenAPI emitter can name
// the shapes its relay routes answer. Dispatch relays them with relayBrokerResponse
// (packages/envoy/internal/dispatch/api/credential_requests.go), which writes the broker's bytes
// unparsed, so the shape a generated client is given has to be named from the broker's own
// declaration rather than read off the relay.
//
// It is a leaf: data and its doc comments, importing nothing of the broker's API package and
// nothing holding a store, an OAuth client or a database.
package wire

import "time"

// PendingEntry is one undecided record in GET /v1/pending's answer.
type PendingEntry struct {
	// The credential-request record's id, which the approval page and the machine login's approve
	// and deny routes take.
	RecordID string `json:"record_id"`
	// "agent_secret" (a secret request) or "launcher_credential" (a machine login).
	Kind string `json:"kind"`
	// The secrets a secret request asks for, or the machine a machine login names.
	Identifiers []string `json:"identifiers"`
	// When it was asked.
	RequestedAt time.Time `json:"requested_at"`
	// The session this request names, read independently of each other (LEGION-587); see
	// RecordSessionResp, this row's exact shape here too.
	Session RecordSessionResp `json:"session"`
	// A secret request's page on the broker, <BROKER_PUBLIC_URL>/approve/<record>, where a person
	// signed in to the broker decides it; absent for a machine login, which is decided with its
	// typed code.
	ApproveURL string `json:"approve_url,omitempty"`
}

// PendingResponse is GET /v1/pending's answer.
type PendingResponse struct {
	// The undecided records the named person decides, newest first.
	Pending []PendingEntry `json:"pending"`
}

// ApproverGrantResp is one grant in GET /v1/grants?approver=<login>: a grant on a session the login
// operates, automatic or approved by anyone, or a grant the login approved on anyone's session.
type ApproverGrantResp struct {
	// The grant's id, which the revoke route takes.
	GrantID string `json:"grant_id"`
	// How the session got it: "automatic" (the policy gave it without asking) or "approval".
	Granted string `json:"granted"`
	// The credential-request record its approval rests on; null for an automatic grant.
	RecordID *string `json:"record_id"`
	// The session holding it.
	Enrollment RecordEnrollmentResp `json:"enrollment"`
	// The secrets it covers.
	Names []string `json:"names"`
	// The person who approved it; null for an automatic grant.
	Approver *string `json:"approver"`
	// When it expires; null for a grant that lasts until its session ends.
	ExpiresAt *time.Time `json:"expires_at"`
	// When it was granted.
	CreatedAt time.Time `json:"created_at"`
}

// ApproverGrantsResponse is GET /v1/grants's answer.
type ApproverGrantsResponse struct {
	// The live grants of the named person's sessions, and those the person approved, newest first.
	Grants []ApproverGrantResp `json:"grants"`
}

// LauncherCredentialResp is one machine login in GET /v1/launcher-credentials?approver=<email> and
// GET /v1/operator/machines.
type LauncherCredentialResp struct {
	// The launcher credential's id, which the revoke route takes.
	CredentialID string `json:"credential_id"`
	// The machine's host name, as a person's machine login named it; for a token login, its
	// ServiceAccount's <namespace>/<name>, or a machine's <project>/<zone>/<instance name>.
	Host string `json:"host"`
	// The service a service's login is for (legion-daemon), whose sessions are its pods, or the
	// fleet a machine belongs to; null for a person's own machine.
	Service *string `json:"service"`
	// What a token login proved: its ServiceAccount
	// (system:serviceaccount:<namespace>:<name>), or a machine as the token exchange names it
	// (exchange.MachineSubject); null for every login a person approved.
	Subject *string `json:"subject"`
	// The login of the person who approved the login (anyone signed in may have approved a service's
	// login before token logins); "" for a service's token login, which no person approves.
	ApprovedBy string `json:"approved_by"`
	// When the login was approved, or a token login's first login.
	IssuedAt time.Time `json:"issued_at"`
	// When the credential expires. Each refresh of a person's device login moves it to
	// BROKER_LAUNCHER_CREDENTIAL_SECONDS from then; a login through the earlier route never
	// refreshes; a token login moves it later at each login, at most an hour away.
	ExpiresAt time.Time `json:"expires_at"`
	// True once expires_at has passed: the login enrolls nothing more, but sessions it enrolled
	// still run (each renews with its own key), and revoking it ends them.
	Expired bool `json:"expired"`
}

// LauncherCredentialsResponse is GET /v1/launcher-credentials's and GET /v1/operator/machines's
// answer.
type LauncherCredentialsResponse struct {
	// The machine logins that are not revoked and are either unexpired or expired with a session
	// still running, newest first: every service's and the named person's own machines' on
	// Dispatch's page, the calling operator's own machines' alone on the operator route.
	Credentials []LauncherCredentialResp `json:"credentials"`
}

// PersonSignInResp is one live sign-in in GET /v1/person-signins?person=<email>.
type PersonSignInResp struct {
	// The sign-in's id, which the revoke route takes.
	SignInID string `json:"signin_id"`
	// The Google account of the machine the person signed in on, as BROKER_VM_SERVICES lists it:
	// the fleet's account, which is what names the box on the person's own list.
	Machine string `json:"machine"`
	// The RFC 7638 thumbprint of the box key the sign-in is bound to, which is what tells this
	// sign-in from the person's others on the same fleet. It is public: it is what the box's
	// DPoP proofs already carry.
	Thumbprint string `json:"thumbprint"`
	// When the person first signed in on that box.
	CreatedAt time.Time `json:"created_at"`
	// When that box last traded a token under this sign-in.
	LastUsedAt time.Time `json:"last_used_at"`
}

// PersonSignInsResponse is GET /v1/person-signins's answer.
type PersonSignInsResponse struct {
	// The named person's live sign-ins, newest first: one per box key they signed in with and
	// have not revoked. A revoked sign-in is not listed, and neither is anyone else's.
	SignIns []PersonSignInResp `json:"signins"`
}

// PersonSignInRevokedResponse is POST /v1/person-signins/{id}/revoke's answer.
type PersonSignInRevokedResponse struct {
	// Always "revoked": the sign-in no longer trades, whether this call ended it or an earlier
	// one did.
	State string `json:"state"`
	// How many of that person's live access tokens this call ended with it, the ones minted on
	// that box under that sign-in. It is 0 for a sign-in revoked already, and for one whose
	// tokens had all expired.
	TokensEnded int `json:"tokens_ended"`
}

// RecordSessionResp is a credential request's two possibly-differing session ids, read
// independently (LEGION-587): Enrollment is the id the requesting session's own enrollment
// stated when it enrolled, Request is the id the request itself stated as an unsigned override
// in its body. Either is null when that id was never set; a machine login (which has no request
// row and no requesting enrollment) always answers both null, so every record POST
// /v1/machine-logins/lookup answers carries two nulls. GET /v1/pending's own
// PendingEntry.Session carries this same shape, where a secret request's row names the ids it
// has.
type RecordSessionResp struct {
	// The id the request itself stated as an unsigned override in its body; null when it named
	// none.
	Request *string `json:"request"`
	// The id the requesting session's own enrollment stated when it enrolled; null when it named
	// none, or for a machine login, which has no requesting enrollment at all.
	Enrollment *string `json:"enrollment"`
}

// RecordEnrollmentResp is a record's or a grant's requesting enrollment. Slot is a pod
// enrollment's slot, one of several independent identities in one pod, and null for every
// enrollment without one.
type RecordEnrollmentResp struct {
	// "session" (an agent session on a machine its person logged in) or "pod".
	Kind string `json:"kind"`
	// The session's runtime: whatever names the session on its machine (the helper's
	// host:pid:start time, a container's id), or a pod's UID.
	RuntimeID string `json:"runtime_id"`
	// The person the session runs for; empty for a pod.
	Operator string `json:"operator"`
	// The pod slot it holds; null for every enrollment without one.
	Slot *string `json:"slot"`
}

// RecordDecisionResp is a decided record's terminal event. CredentialID is null unless the
// decision minted a launcher credential: an agent_secret record's approval names none.
type RecordDecisionResp struct {
	// "approved", "denied", "expired" or "cancelled".
	Event string `json:"event"`
	// When it was decided.
	At time.Time `json:"at"`
	// The machine credential an approved machine login minted; null otherwise.
	CredentialID *string `json:"credential_id"`
}

// RecordResponse is a credential-request record as POST /v1/machine-logins/lookup answers it.
type RecordResponse struct {
	// The record's id: the SHA-256 of its text.
	RecordID string `json:"record_id"`
	// "agent_secret" (a secret request) or "launcher_credential" (a machine login).
	Kind string `json:"kind"`
	// "pending", or how it ended: "approved", "denied", "expired", "cancelled" or "revoked".
	State string `json:"state"`
	// The one login that may decide it, or "anyone": any signed-in person may decide a request
	// for a shared human-tier secret, and may deny a service's machine login (which only a broker
	// before token logins opened, and which nobody approves any more), whatever person it names.
	Approver string `json:"approver"`
	// The session asking; null for a machine login.
	Enrollment *RecordEnrollmentResp `json:"enrollment"`
	// The session this record names, read independently of each other; null fields where that id
	// was never set (LEGION-587).
	Session RecordSessionResp `json:"session"`
	// The secrets asked for, or the machine logging in.
	Identifiers []string `json:"identifiers"`
	// The service a machine login is for, when it logs a service in rather than a person's
	// machine; null otherwise.
	Service *string `json:"service"`
	// The reason the session gave, verbatim.
	Reason string `json:"reason"`
	// How many seconds the grant or credential lasts once approved; null for a grant that lasts
	// until its session ends. A machine login's credential always names one.
	LifetimeSeconds *int `json:"lifetime_seconds"`
	// The version of the secret policy it was decided under (the SHA-256 of every agent secret's
	// name, owner, tier and ARN); for a machine login, the version current when it was asked.
	RulesVersion string `json:"rules_version"`
	// When it expires undecided.
	ExpiresAt time.Time `json:"expires_at"`
	// When it was asked.
	RequestedAt time.Time `json:"requested_at"`
	// How it was decided; null while pending.
	Decided *RecordDecisionResp `json:"decided"`
}

// DecisionResponse is what a machine login's approval answers.
type DecisionResponse struct {
	// The machine credential the approval minted.
	CredentialID *string `json:"credential_id"`
	// "approved".
	State string `json:"state"`
}

// StateResponse is what a denial, a cancel or a revoke answers.
type StateResponse struct {
	// The state the call left the request, record or grant in: "denied", "cancelled" or "revoked".
	State string `json:"state"`
}

// MachineLookupRequest is the body POST /v1/machine-logins/lookup takes, which Dispatch relays
// to the broker unparsed (its own POST /api/v1/credential-requests/machine-lookup).
type MachineLookupRequest struct {
	// The XXXX-XXXX confirmation code the machine shows, as the approver typed it.
	Code string `json:"code" required:"true"`
}
