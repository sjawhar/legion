// packages/envoy/internal/brokerclient/signinauthority.go
//
// Which launcher credential a machine's sessions enrol under: the person signed in on the box,
// or the machine itself.
//
// A machine's own credential, the one its identity token trades for, names its fleet's service
// and no person at all, so the sessions it enrols read that service's secrets and nobody's own.
// A person's sign-in on the box trades for a credential of its own carrying that person
// (enroll.Service.IssueTokenLoginTx), and the sessions it enrols are theirs. Both are launcher
// credentials, and a process that enrols sessions holds the key of whichever it signs with,
// which is why one type (Authority) describes both.
//
// The rule, which `agent-secrets-helper` applies at every enrolment, is the one the issue's
// architect decided: a session enrols under the person signed in as the user the enroller runs
// as, when there is one, and under the machine otherwise. It follows the box rather than the
// moment the process started, so a sign-in made or revoked while the helper runs changes what
// the next session enrols under and needs no restart.
//
// A session of one user never reaches another user's sign-in: the records are one directory per
// user, 0700, under that user's own key directory (signinstore.go), and the only store read
// here is the one the reading process's own key directory names.

package brokerclient

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
)

// SignInsDir is the directory under a key directory that holds one record per person, so
// `agent-secrets login`, which writes them, and every reader of them name one directory in one
// place.
const SignInsDir = "signins"

// SignInsIn is the sign-in store of a key directory: a box, a pod and a person's own machine
// each keep their sign-ins where everything else of that session's identity lives.
func SignInsIn(keyDir string) SignInStore {
	return SignInStore{Dir: filepath.Join(keyDir, SignInsDir)}
}

// Authority is a launcher credential as the process holding its key sees it: the credential the
// broker minted, the key every launcher proof under it is signed with, and the person it acts
// for.
//
// Operator is empty for a machine's own credential, which carries no person, and an enrolment
// signed with such a credential must state no operator at all: the broker records the
// credential's own operator whatever is sent, and refuses an enrolment that states one the
// credential does not carry — any operator at all under a credential that carries none
// (enroll.Service.Create, ErrOperatorMismatch).
type Authority struct {
	// Key is the key the credential was minted for, which signs every launcher proof under it:
	// the box key of a person's sign-in, the process's own key for a machine.
	Key *ecdsa.PrivateKey
	// CredentialID is the launcher credential's id, the "lid" of those proofs.
	CredentialID string
	// Operator is the person the credential acts for, empty where it acts for none.
	Operator string
}

// SignInAuthority is the launcher credential of the person signed in on this box, kept live for
// a process that enrols sessions under it. The zero value answers nothing: Store, Machine, Flow
// and BrokerURL are all required.
type SignInAuthority struct {
	// Store is where this machine's sign-ins live: SignInsIn of this process's key directory,
	// and never another user's.
	Store SignInStore
	// Person is what PersonVar says in this process's own environment, which picks one of
	// several sign-ins of this user; empty where it says nothing. It is never read from a
	// session's environment: a session does not choose whose authority it enrols under.
	Person string
	// Machine is this machine's own identity, presented as the actor token of every trade a
	// person makes here (RFC 8693 Section 2.1).
	Machine Source
	// Flow is the device-flow client the sign-ins were made with, which mints a person's next
	// identity token from the refresh token their record holds.
	Flow *DeviceFlow
	// BrokerURL, HTTP and Log are the trade's, as Config's own members of those names. The
	// session name is not: it is the person's, and SessionFor settles it.
	BrokerURL string
	HTTP      *http.Client
	Log       *slog.Logger

	mu sync.Mutex
	// held is the client of the record heldFor names, and heldCredential the credential its
	// last trade minted, which Refused is told about. A record the store no longer answers,
	// or answers with another box key, is a different sign-in and gets a client of its own.
	held           *Client
	heldFor        string
	heldCredential string
	// refused names every record whose sign-in the broker has refused, so a box whose person
	// has revoked their sign-in asks once and then enrols under the machine. Only a new
	// `agent-secrets login`, which makes a box key of its own, clears it, because the key is
	// part of the name.
	refused map[string]bool
}

// Current is the launcher credential a session enrolling now acts under, or nil where this box
// offers none and the caller's own machine credential is what the session enrols under.
//
// It reads the store on every call rather than once at start, so a sign-in made or revoked
// while the process runs changes the next enrolment with no restart: the answer is never older
// than the enrolment it decides. Asking costs a read of one small directory, not a trade — the
// client holds one access token live and trades again only before that token lapses.
//
// Nobody signed in here is nil and no error, and so is a sign-in refused for good — the person
// revoked it, or the issuer no longer honours their refresh token — which is remembered so the
// next enrolment does not ask again. A refusal a later trade could still cure, the broker's 429
// and a clock-skewed DPoP proof among them, is an error rather than that: remembering one of
// those would enrol every later session under this machine for the life of the process. Every
// other failure is an error too, which a caller must not read as "nobody is signed in": a
// broker that did not answer says nothing about who is signed in here, and a box with two
// sign-ins and nothing naming which is refused by name rather than acting as either
// (SignInStore.Choose).
func (a *SignInAuthority) Current(ctx context.Context) (*Authority, error) {
	if a.Machine == nil || a.Flow == nil {
		return nil, errors.New("brokerclient: a sign-in's authority needs this machine's own identity and the sign-in client that renews the person's")
	}
	record, err := a.Store.Choose(a.Person)
	switch {
	case errors.Is(err, ErrNoSignIn):
		return nil, nil
	case err != nil:
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	name := record.Person + "\x00" + record.Thumbprint
	if a.refused[name] {
		return nil, nil
	}
	if a.held == nil || a.heldFor != name {
		key, err := record.Key()
		if err != nil {
			return nil, err
		}
		client, err := New(Config{
			BrokerURL: a.BrokerURL,
			Key:       key,
			Subject:   &PersonIdentity{Flow: a.Flow, RefreshToken: record.RefreshToken, Person: record.Person},
			Actor:     a.Machine,
			Session:   SessionFor(record),
			HTTP:      a.HTTP,
			Log:       a.Log,
		})
		if err != nil {
			return nil, err
		}
		a.held, a.heldFor, a.heldCredential = client, name, ""
	}
	token, err := a.held.Token(ctx)
	var refusal *RefusedError
	switch {
	case errors.As(err, &refusal) && refusal.Permanent():
		// A refusal a retry could cure — the broker's 429, a skewed DPoP proof — is not this
		// sign-in ending: marking it refused on one of those would enrol every later session
		// under this machine for the life of the process.
		a.markRefused(name, record.Person, refusal.Code)
		return nil, nil
	case errors.Is(err, ErrSignInEnded):
		// The issuer, not the broker, is what refused: a refresh token Google no longer
		// honours is as final as a revoke, and asking it again at every enrolment would fail
		// every enrolment on this box rather than fall back to the machine.
		a.markRefused(name, record.Person, "invalid_grant")
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trade %s's sign-in on this machine: %w", record.Person, err)
	}
	if token.CredentialID == "" {
		return nil, fmt.Errorf("the broker took %s's sign-in but named no launcher credential, so no session can enrol under it", record.Person)
	}
	a.heldCredential = token.CredentialID
	return &Authority{Key: a.held.Key(), CredentialID: token.CredentialID, Operator: record.Person}, nil
}

// SessionFor is the session name a person's trade on this box asks under. The exchange fixes
// none for a sign-in and requires the request to name one (exchange.Set.Person,
// ErrSessionRequired), and a session name belongs to one identity, so the name this machine
// trades its own identity under (SessionVar) is one a person's trade is refused for: a trade
// under it is refused permanently once the machine has claimed it, and claims it away from the
// machine when the person trades first. This name is the person's own and the same on every
// trade of that sign-in: their address and the box key's thumbprint, which is what tells two
// boxes of one person apart on the sign-ins page itself. It is never read back here — a caller
// of this package wants the launcher credential, not the access token — but the broker records
// who a name belongs to, so it must be theirs.
//
// It is exported because `agent-secrets login` makes the very first trade of a sign-in, before
// any record is written, and that trade must ask under the same name every later one does.
func SessionFor(record SignInRecord) string {
	name := "signin-" + record.Person
	if thumbprint := record.Thumbprint; thumbprint != "" {
		name += "-" + thumbprint[:min(12, len(thumbprint))]
	}
	return name
}

// Refused is how a caller tells this authority that the broker rejected a launcher proof signed
// with the credential Current answered: the sign-in behind it has ended, or the credential has
// lapsed, so the token in hand is not worth presenting again. The next Current trades afresh,
// which either answers a live credential or is refused and falls back to the machine, so one
// enrolment settles in one call rather than failing for the caller to retry. A credential this
// authority no longer holds — a trade since has replaced it — is nothing to do.
func (a *SignInAuthority) Refused(credentialID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if credentialID != "" && credentialID == a.heldCredential {
		a.held, a.heldFor, a.heldCredential = nil, "", ""
	}
}

// markRefused records that this record's sign-in has been refused for good — by the broker, or
// by the issuer that no longer honours the person's refresh token — and logs it once with the
// code that named the refusal: the person reads it on Dispatch's sign-ins page, and the box
// goes on working under its own authority. The caller holds a.mu.
func (a *SignInAuthority) markRefused(name, person, code string) {
	if a.refused == nil {
		a.refused = map[string]bool{}
	}
	a.refused[name] = true
	a.held, a.heldFor, a.heldCredential = nil, "", ""
	if a.Log != nil {
		a.Log.Warn("this machine's sign-in was refused; sessions enrol under this machine's own authority until somebody signs in again",
			"person", person, "code", code, "command", SignInCommand)
	}
}
