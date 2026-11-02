// packages/envoy/cmd/agent-secrets/login.go
//
// `agent-secrets login`, and the sessions it makes possible: a person signs in on a machine once,
// with one command, and from then on every agent-secrets form on that machine acts for them.
//
// The sign-in is Google's device authorization grant (RFC 8628) through the token client
// (internal/brokerclient), asking for `openid` and `email` and nothing else. What it leaves
// behind is one record per person under the key directory: a box key this machine made at
// sign-in, and Google's refresh token beside it, in a file only the person's own user can read
// (0600 in a 0700 directory, written through writeFileAtomic, as every key this binary writes
// is). The box key is what tells this machine's sign-in from the person's others: the broker
// records its thumbprint at the first trade and refuses a proof by any other key afterwards, so
// the record is the sign-in and a copy of it elsewhere proves nothing without the key.
//
// A session then trades that person's identity for an access token, with this machine's own
// identity as the actor token, and enrols the box key under the launcher credential the trade
// minted. That enrolment is the person's: it reads that person's secrets and the shared
// agent-tier ones, and no other person's and none of the fleet's.
//
// Which person a session acts for: AGENT_SECRETS_PERSON when it is set, the one sign-in on the
// box when there is exactly one, and otherwise a refusal naming every person who has signed in
// here. A second person signs in beside the first and neither disturbs the other.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
)

const (
	// signInIssuerVar names the identity provider a person signs in at, Google's own when unset.
	// The repository's dev stack points it at its test issuer, which serves the same grant.
	signInIssuerVar = "AGENT_SECRETS_SIGNIN_ISSUER"
	// signInClientIDVar is the OAuth client of the device grant, the client whose audience the
	// broker's own sign-in setting names. `login` needs it.
	signInClientIDVar = "AGENT_SECRETS_SIGNIN_CLIENT_ID"
	// signInClientSecretFileVar is a file holding that client's secret, for a provider that
	// wants one of a device-flow client; unset where the client is public.
	signInClientSecretFileVar = "AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE"
	// personVar names which sign-in on this box a session acts for, where more than one person
	// has signed in on it.
	personVar = "AGENT_SECRETS_PERSON"
	// signInDirName is the directory of sign-in records under the key directory, one file per
	// person, and signInSuffix the extension each file carries.
	signInDirName = "signins"
	signInSuffix  = ".json"
	// signInDeadline bounds the whole device grant, the wait on the person included: a code
	// nobody types in lapses rather than holding a terminal for ever.
	signInDeadline = 15 * time.Minute
	// tradeDeadline bounds one trade and the enrolment that follows it.
	tradeDeadline = 60 * time.Second
)

// signInRecord is one person's sign-in on this machine: who they are, the provider they signed
// in at and its two endpoints, the refresh token their next identity token is minted with, and
// the box key this machine made for them, which every proof of theirs on this box is signed
// with. It is written 0600 and read by nothing but this binary.
type signInRecord struct {
	Person        string    `json:"person"`
	Issuer        string    `json:"issuer"`
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"`
	DeviceAuthURL string    `json:"device_authorization_endpoint"`
	TokenURL      string    `json:"token_endpoint"`
	RefreshToken  string    `json:"refresh_token"`
	BoxKeyPEM     string    `json:"box_key_pem"`
	SignedInAt    time.Time `json:"signed_in_at"`
}

// signInDir is where this machine keeps its sign-ins: under the key directory, which is already
// the one place a session's own key lives.
func signInDir() string { return filepath.Join(keyDir(), signInDirName) }

// signInPath is person's record. The address is lower-cased and every character that is not a
// letter, a digit, a dash, a dot or an at sign becomes a dash, so one address makes one file
// name and no address can name a path of its own choosing.
func signInPath(person string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '@':
			return r
		default:
			return '-'
		}
	}, strings.ToLower(person))
	return filepath.Join(signInDir(), safe+signInSuffix)
}

// loadSignIns reads every sign-in on this box, in the order of the person's address, and answers
// none at all where the directory is absent. A file that is not a sign-in is an error naming it:
// a session must never silently act for a person other than the one it would have.
func loadSignIns() ([]signInRecord, error) {
	entries, err := os.ReadDir(signInDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []signInRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), signInSuffix) {
			continue
		}
		path := filepath.Join(signInDir(), entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var record signInRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("%s is not a sign-in record: %w; sign in again with: agent-secrets login", path, err)
		}
		if record.Person == "" || record.RefreshToken == "" || record.BoxKeyPEM == "" {
			return nil, fmt.Errorf("%s names no person, no refresh token or no box key; sign in again with: agent-secrets login", path)
		}
		records = append(records, record)
	}
	slices.SortFunc(records, func(a, b signInRecord) int { return strings.Compare(a.Person, b.Person) })
	return records, nil
}

// currentSignIn is the sign-in this session acts for: the one AGENT_SECRETS_PERSON names, the
// only one on the box, or none at all where nobody has signed in here. With more than one and no
// person named it refuses, naming every one of them, because acting for the wrong person would
// read the wrong person's secrets.
func currentSignIn() (*signInRecord, error) {
	records, err := loadSignIns()
	if err != nil || len(records) == 0 {
		return nil, err
	}
	wanted := strings.ToLower(strings.TrimSpace(os.Getenv(personVar)))
	if wanted != "" {
		for _, record := range records {
			if record.Person == wanted {
				return &record, nil
			}
		}
		return nil, fmt.Errorf("%s names %s, who has not signed in on this machine; the sign-ins here are %s. Sign in with: agent-secrets login",
			personVar, wanted, strings.Join(people(records), ", "))
	}
	if len(records) > 1 {
		return nil, fmt.Errorf("more than one person has signed in on this machine (%s); name whose session this is with %s. A new person signs in with: agent-secrets login",
			strings.Join(people(records), ", "), personVar)
	}
	return &records[0], nil
}

// people is every record's person, for a message that names them.
func people(records []signInRecord) []string {
	names := make([]string, len(records))
	for i, record := range records {
		names[i] = record.Person
	}
	return names
}

// save writes the record, owner-readable only, in a directory of the same: the box key and
// Google's refresh token are in it, and either alone would let whoever read it act as the person
// on this machine.
func (r *signInRecord) save() (string, error) {
	if err := os.MkdirAll(signInDir(), 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	path := signInPath(r.Person)
	if err := writeFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// boxKey is the key this machine signed the person in with, as a key.
func (r *signInRecord) boxKey() (*ecdsa.PrivateKey, error) {
	key, err := parseKeyPEM([]byte(r.BoxKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("%s's box key cannot be read (%w); sign in again with: agent-secrets login", r.Person, err)
	}
	return key, nil
}

// subject is the person's own identity token, minted again for every trade from the refresh
// token this record holds.
func (r *signInRecord) subject() *brokerclient.PersonSource {
	return &brokerclient.PersonSource{
		Provider:     brokerclient.Provider{DeviceAuthURL: r.DeviceAuthURL, TokenURL: r.TokenURL},
		ClientID:     r.ClientID, ClientSecret: r.ClientSecret,
		RefreshToken: r.RefreshToken, Person: r.Person,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

// cmdLogin implements "login": Google's device authorization grant, the box key it makes, the
// record it keeps, and the first trade, which is what records the sign-in at the broker. The
// person types one code on a device of their own and nothing else; this machine prints that code
// and the page to type it at, and waits.
func cmdLogin(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet("login", stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets login: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	base := brokerBaseURL()
	if base == "" {
		fmt.Fprintf(stderr, "agent-secrets login: %s is required: it names the secrets broker this sign-in is recorded at\n", brokerURLVar)
		return exitUsageError
	}
	clientID := strings.TrimSpace(os.Getenv(signInClientIDVar))
	if clientID == "" {
		fmt.Fprintf(stderr, "agent-secrets login: %s is required: it names the OAuth client this machine runs the device sign-in as, the one the broker's own sign-in setting names\n", signInClientIDVar)
		return exitUsageError
	}
	secret := ""
	if file := strings.TrimSpace(os.Getenv(signInClientSecretFileVar)); file != "" {
		read, err := readTrimmed(file)
		if err != nil {
			fmt.Fprintf(stderr, "agent-secrets login: %s: %v\n", signInClientSecretFileVar, err)
			return exitUsageError
		}
		secret = read
	}
	issuer := strings.TrimSpace(os.Getenv(signInIssuerVar))
	if issuer == "" {
		issuer = brokerclient.GoogleIssuer
	}
	ctx, cancel := context.WithTimeout(context.Background(), signInDeadline)
	defer cancel()
	webClient := &http.Client{Timeout: 30 * time.Second}
	provider, err := brokerclient.Discover(ctx, issuer, webClient)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets login: %v\n", err)
		return 1
	}
	signIn := &brokerclient.SignIn{Provider: provider, ClientID: clientID, ClientSecret: secret, HTTP: webClient}
	answer, err := signIn.Run(ctx, func(auth *oauth2.DeviceAuthResponse) {
		fmt.Fprintf(stdout, "sign-in code: %s\n", auth.UserCode)
		where := auth.VerificationURI
		if auth.VerificationURIComplete != "" {
			where = auth.VerificationURIComplete
		}
		fmt.Fprintf(stdout, "enter it at %s — this terminal waits until you have\n", where)
	})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets login: %v\n", err)
		return 1
	}
	key, err := proof.NewKey()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets login: make this machine's box key: %v\n", err)
		return 1
	}
	keyPEM, err := encodeKeyPEM(key)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets login: %v\n", err)
		return 1
	}
	record := &signInRecord{
		Person: answer.Person, Issuer: issuer, ClientID: clientID, ClientSecret: secret,
		DeviceAuthURL: provider.DeviceAuthURL, TokenURL: provider.TokenURL,
		RefreshToken: answer.RefreshToken, BoxKeyPEM: string(keyPEM), SignedInAt: time.Now().UTC(),
	}
	path, err := record.save()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets login: keep the sign-in: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "signed in as %s; this machine's box key and the refresh token are in %s, readable by you alone\n", record.Person, path)

	// The first trade is what records the sign-in at the broker: the person's own identity
	// token with this machine's as the actor, proving the box key just made. The record is kept
	// whatever it answers, so a refusal is fixed and the person is not asked to sign in again.
	tradeCtx, stop := context.WithTimeout(context.Background(), tradeDeadline)
	defer stop()
	enrollmentID, _, err := signInSigner(tradeCtx, base, record)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets login: the sign-in is kept, but this machine could not trade it for a session: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "this machine's sign-in is recorded at the broker; its enrolment is %s\n", enrollmentID)
	return 0
}

// signInSigner is a session acting for the person record names: it trades their identity for an
// access token, with this machine's own identity as the actor token, and enrols the box key
// under the launcher credential that trade minted. The enrolment is idempotent — the same
// credential, kind, runtime id and key answer the live one — so every form on this machine
// reaches the same session rather than making one of its own.
func signInSigner(ctx context.Context, base string, record *signInRecord) (string, Signer, error) {
	key, err := record.boxKey()
	if err != nil {
		return "", nil, err
	}
	actor, err := brokerclient.MachineSource(os.Getenv)
	if err != nil {
		return "", nil, err
	}
	if actor == nil {
		return "", nil, fmt.Errorf("%s's sign-in needs this machine's own identity as its actor token, and %s names none; set it to metadata, file:<path> or pool",
			record.Person, brokerclient.MachineIdentityVar)
	}
	local, _, _ := strings.Cut(record.Person, "@")
	session, err := brokerclient.SessionName(os.Getenv, local)
	if err != nil {
		return "", nil, err
	}
	client := &brokerclient.Client{
		BrokerURL: base, Key: key, Subject: record.subject(), Actor: actor, Session: session,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
	token, err := client.Trade(ctx)
	if err != nil {
		return "", nil, err
	}
	if token.CredentialID == "" {
		return "", nil, fmt.Errorf("the broker's answer names no launcher credential for %s's sign-in, so this session cannot enrol", record.Person)
	}
	id, err := enrollSignIn(ctx, newClient(base), key, token.CredentialID, session)
	if err != nil {
		return "", nil, err
	}
	return id, &fileSigner{key: key, enrollmentID: id}, nil
}

// launcherSignerOf signs one broker call with a launcher credential this process holds itself —
// the one a trade minted — rather than through the helper's socket.
type launcherSignerOf struct {
	key          *ecdsa.PrivateKey
	credentialID string
}

func (l *launcherSignerOf) Sign(method, url string) (string, error) {
	return proof.SignLauncher(l.key, l.credentialID, method, url, time.Now())
}

// enrollSignIn registers the box key as a session under the credential the trade minted, and
// answers the enrolment id. It states no operator: an enrolment's operator is its credential's,
// and the credential a person's trade mints already carries them.
func enrollSignIn(ctx context.Context, c *client, key *ecdsa.PrivateKey, credentialID, session string) (string, error) {
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return "", err
	}
	body := map[string]any{"kind": "session", "runtime_id": session, "thumbprint": thumbprint}
	if id := requestSessionID(); id != "" {
		body["session_id"] = id
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	data, err := c.doProof(ctx, &launcherSignerOf{key: key, credentialID: credentialID}, http.MethodPost, "/v1/enrollments", encoded)
	if err != nil {
		return "", err
	}
	var answer struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	if err := json.Unmarshal(data, &answer); err != nil || answer.EnrollmentID == "" {
		return "", fmt.Errorf("the broker's enrolment answer names no enrollment_id")
	}
	return answer.EnrollmentID, nil
}
