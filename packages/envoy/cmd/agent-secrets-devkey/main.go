// Command agent-secrets-devkey is a software WebAuthn authenticator driven from the command
// line: the local dev stack's stand-in for a human's YubiKey, so the AGENTC-393 credential-request
// broker (packages/envoy/cmd/broker) can be exercised end to end without AWS or real hardware
// (Task 12). It is built on internal/broker/webauthntest, the software authenticator Tasks 3/4/9
// built and tested against — every one of its constructors and methods takes a testing.TB and
// calls Fatalf/Helper internally, so this command never touches those methods directly; the tb
// shim below stands in for a live *testing.T (see its own doc comment for why that works at all).
//
//	agent-secrets-devkey register --login <login> --state <dir> --seed [--origin <https-origin>] [--nonce <64hex>]
//	agent-secrets-devkey register --login <login> --state <dir> --broker <url> --ui-token <token> [--origin <https-origin>]
//	agent-secrets-devkey approve   --record <id>  --state <dir> --broker <url> --ui-token <token> [--origin <https-origin>]
//	agent-secrets-devkey deny      --record <id>  --state <dir> --broker <url> --ui-token <token> [--origin <https-origin>]
//	agent-secrets-devkey endorse   --login <login> --new-credential-id <b64url> --state <dir> --broker <url> --ui-token <token> [--credential-id <b64url>] [--origin <https-origin>]
//	agent-secrets-devkey machine-approve --code XXXX-XXXX --state <dir> --broker <url> --ui-token <token> [--deny] [--origin <https-origin>]
//
// --state persists one identity's key material (0600, dev-only by definition — a private key on
// disk in the clear) across separate invocations, since register/approve/deny/endorse/
// machine-approve are each their own process and must all act as the same authenticator; use a
// fresh directory per identity. Environment defaults: AGENT_SECRETS_URL, AGENT_SECRETS_UI_TOKEN,
// AGENT_SECRETS_UI_ORIGIN, AGENT_SECRETS_DEVKEY_STATE.
//
// "register --seed" is the one offline mode: it generates a fresh CA and Authenticator (no broker
// call — there is no live broker yet at bootstrap) and prints the JSON scripts/dev-broker.sh needs
// to seed a rules file's approvers section and the matching approver_key_seeds row (contract v9
// ruling 9, the break-glass path). Every other action authenticates against a live broker with the
// UI bearer token and, besides "register" itself (online), signs a WebAuthn assertion over the
// broker's own challenge for the action requested.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/webauthntest"
)

const exitUsageError = 2

// defaultOrigin is devkey's own default WebAuthn origin when neither --origin nor
// AGENT_SECRETS_UI_ORIGIN names one: an RFC 2606 ".invalid" host, so it is unmistakably not a
// real service — origin here is a pure signing-domain value the broker compares by exact string
// match (BROKER_UI_ORIGIN), never a URL anything actually connects to, since devkey builds its own
// clientDataJSON rather than driving a browser.
const defaultOrigin = "https://agent-secrets.invalid"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage())
		return exitUsageError
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage())
		return 0
	case "register":
		return cmdRegister(args[1:], stdout, stderr)
	case "approve":
		return cmdDecide(true, args[1:], stdout, stderr)
	case "deny":
		return cmdDecide(false, args[1:], stdout, stderr)
	case "endorse":
		return cmdEndorse(args[1:], stdout, stderr)
	case "machine-approve":
		return cmdMachineApprove(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "agent-secrets-devkey: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(stderr, usage())
		return exitUsageError
	}
}

func usage() string {
	return `usage:
  agent-secrets-devkey register --login <login> --state <dir> --seed [--origin <https-origin>] [--nonce <64hex>]
  agent-secrets-devkey register --login <login> --state <dir> --broker <url> --ui-token <token> [--origin <https-origin>]
  agent-secrets-devkey approve   --record <id>  --state <dir> --broker <url> --ui-token <token> [--origin <https-origin>]
  agent-secrets-devkey deny      --record <id>  --state <dir> --broker <url> --ui-token <token> [--origin <https-origin>]
  agent-secrets-devkey endorse   --login <login> --new-credential-id <b64url> --state <dir> --broker <url> --ui-token <token> [--credential-id <b64url>] [--origin <https-origin>]
  agent-secrets-devkey machine-approve --code XXXX-XXXX --state <dir> --broker <url> --ui-token <token> [--deny] [--origin <https-origin>]

Environment defaults: AGENT_SECRETS_URL, AGENT_SECRETS_UI_TOKEN, AGENT_SECRETS_UI_ORIGIN,
AGENT_SECRETS_DEVKEY_STATE.
`
}

func exitUsage(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return exitUsageError
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// hostOf is the WebAuthn rpId every ceremony this command builds embeds: origin's host with no
// port, matching internal/broker/api's own uiOriginHost() on the other end.
func hostOf(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("--origin %q is not a valid absolute URL", origin)
	}
	return u.Hostname(), nil
}

// ---------------------------------------------------------------------------
// shared flags
// ---------------------------------------------------------------------------

// commonFlags is every action subcommand's shared connection and identity configuration.
type commonFlags struct {
	broker  string
	uiToken string
	origin  string
	state   string
}

func registerCommonFlags(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.broker, "broker", os.Getenv("AGENT_SECRETS_URL"), "broker base URL (default $AGENT_SECRETS_URL)")
	fs.StringVar(&c.uiToken, "ui-token", os.Getenv("AGENT_SECRETS_UI_TOKEN"), "UI bearer token (default $AGENT_SECRETS_UI_TOKEN)")
	fs.StringVar(&c.origin, "origin", orDefault(os.Getenv("AGENT_SECRETS_UI_ORIGIN"), defaultOrigin), "WebAuthn origin; must equal the broker's BROKER_UI_ORIGIN")
	fs.StringVar(&c.state, "state", os.Getenv("AGENT_SECRETS_DEVKEY_STATE"), "directory holding this identity's persisted key material (default $AGENT_SECRETS_DEVKEY_STATE)")
	return c
}

// validateForOnline checks what every subcommand that actually calls the broker needs; "register
// --seed" alone never calls it (it needs only --state).
func (c *commonFlags) validateForOnline() error {
	if c.broker == "" {
		return errors.New("--broker (or AGENT_SECRETS_URL) is required")
	}
	if c.uiToken == "" {
		return errors.New("--ui-token (or AGENT_SECRETS_UI_TOKEN) is required")
	}
	if c.state == "" {
		return errors.New("--state (or AGENT_SECRETS_DEVKEY_STATE) is required")
	}
	return nil
}

// ---------------------------------------------------------------------------
// testing.TB shim
// ---------------------------------------------------------------------------

// tb is a minimal testing.TB shim. testing.TB carries an unexported method (private()), so no
// type outside the testing package can implement the interface directly — but embedding the
// *interface* (rather than a concrete type) promotes every one of its methods, including the
// unexported one, satisfying testing.TB while this type overrides only the two methods
// webauthntest's API actually calls: Helper (a no-op — there is no test to report a helper frame
// to) and Fatalf (recovered as an ordinary Go error instead of failing a test that does not
// exist). This is how the whole webauthntest package — built for tests, every constructor and
// method taking testing.TB — runs from an ordinary CLI process with no live *testing.T.
type tb struct {
	testing.TB
	err error
}

func (t *tb) Helper() {}

func (t *tb) Fatalf(format string, args ...any) {
	t.err = fmt.Errorf(format, args...)
	panic(t)
}

// withTB calls fn with a fresh tb shim and turns a Fatalf inside it into an ordinary returned
// error instead of a process-ending test failure. A panic that is not this call's own shim (which
// should never happen — webauthntest never panics on its own) propagates unchanged rather than
// being silently swallowed.
func withTB[R any](fn func(testing.TB) R) (result R, err error) {
	shim := &tb{}
	defer func() {
		if r := recover(); r != nil {
			if p, ok := r.(*tb); ok && p == shim {
				err = fmt.Errorf("webauthntest: %w", p.err)
				return
			}
			panic(r)
		}
	}()
	return fn(shim), nil
}

// ---------------------------------------------------------------------------
// persisted identity (--state)
// ---------------------------------------------------------------------------

// identityFile is one devkey identity's full material, persisted as --state/identity.json (0600):
// enough to reconstruct the exact same webauthntest.Authenticator (via webauthntest.Restore) in a
// later, separate process invocation. Authenticator's own key/attCert/attKey fields are
// unexported, so this is the one place that material is ever serialized outside the package.
type identityFile struct {
	Login              string `json:"login"`
	CredentialID       string `json:"credential_id"` // base64url raw credential id
	AAGUID             string `json:"aaguid"`
	CredentialKeyDER   string `json:"credential_key_der"`   // base64 std, PKCS8
	AttestationCertDER string `json:"attestation_cert_der"` // base64 std, DER
	AttestationKeyDER  string `json:"attestation_key_der"`  // base64 std, PKCS8
	SignCount          uint32 `json:"sign_count"`
}

func identityPath(dir string) string { return filepath.Join(dir, "identity.json") }

func identityExists(dir string) bool {
	_, err := os.Stat(identityPath(dir))
	return err == nil
}

func loadIdentity(dir string) (identityFile, error) {
	data, err := os.ReadFile(identityPath(dir))
	if err != nil {
		return identityFile{}, fmt.Errorf("read %s: %w (run `agent-secrets-devkey register --seed` first to create an identity)", identityPath(dir), err)
	}
	var f identityFile
	if err := json.Unmarshal(data, &f); err != nil {
		return identityFile{}, fmt.Errorf("%s: %w", identityPath(dir), err)
	}
	return f, nil
}

func saveIdentity(dir string, f identityFile) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(identityPath(dir), data, 0o600)
}

// updatedIdentity is id with SignCount refreshed from auth's own counter, for saving back after
// every Assert call (Register never advances it).
func updatedIdentity(id identityFile, auth *webauthntest.Authenticator) identityFile {
	id.SignCount = auth.SignCount()
	return id
}

func encodeECDSAKey(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

func decodeECDSAKey(b64 string) (*ecdsa.PrivateKey, error) {
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an ECDSA private key")
	}
	return key, nil
}

// newIdentityFrom captures a freshly generated Authenticator's full material for persisting.
func newIdentityFrom(login string, auth *webauthntest.Authenticator) (identityFile, error) {
	keyB64, err := encodeECDSAKey(auth.Key())
	if err != nil {
		return identityFile{}, fmt.Errorf("marshal credential key: %w", err)
	}
	attKeyB64, err := encodeECDSAKey(auth.AttestationKey())
	if err != nil {
		return identityFile{}, fmt.Errorf("marshal attestation key: %w", err)
	}
	return identityFile{
		Login:              login,
		CredentialID:       base64.RawURLEncoding.EncodeToString(auth.CredentialID),
		AAGUID:             auth.AAGUID.String(),
		CredentialKeyDER:   keyB64,
		AttestationCertDER: base64.StdEncoding.EncodeToString(auth.AttestationCertDER()),
		AttestationKeyDER:  attKeyB64,
		SignCount:          auth.SignCount(),
	}, nil
}

// restoreAuthenticator reconstructs f's Authenticator via webauthntest.Restore, so this process
// can keep signing as the exact same credential a previous invocation generated or last used.
func restoreAuthenticator(f identityFile) (*webauthntest.Authenticator, error) {
	credentialID, err := base64.RawURLEncoding.DecodeString(f.CredentialID)
	if err != nil {
		return nil, fmt.Errorf("credential_id: %w", err)
	}
	aaguid, err := uuid.Parse(f.AAGUID)
	if err != nil {
		return nil, fmt.Errorf("aaguid: %w", err)
	}
	key, err := decodeECDSAKey(f.CredentialKeyDER)
	if err != nil {
		return nil, fmt.Errorf("credential_key_der: %w", err)
	}
	attCertDER, err := base64.StdEncoding.DecodeString(f.AttestationCertDER)
	if err != nil {
		return nil, fmt.Errorf("attestation_cert_der: %w", err)
	}
	attKey, err := decodeECDSAKey(f.AttestationKeyDER)
	if err != nil {
		return nil, fmt.Errorf("attestation_key_der: %w", err)
	}
	return webauthntest.Restore(credentialID, aaguid, key, attCertDER, attKey, f.SignCount)
}

// ---------------------------------------------------------------------------
// UI-bearer HTTP client
// ---------------------------------------------------------------------------

type uiClient struct {
	base  string
	token string
	http  *http.Client
}

func newUIClient(base, token string) *uiClient {
	return &uiClient{base: base, http: &http.Client{Timeout: 30 * time.Second}, token: token}
}

// apiError is the broker's {"code":...,"error":...} error envelope, plus the HTTP status it came
// with.
type apiError struct {
	Status int
	Code   string
	Reason string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Reason, e.Status)
}

// do sends a UI-bearer-authenticated request and, on a 2xx response, returns its raw body; any
// other status decodes the broker's error envelope into an *apiError.
func (c *uiClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var envelope struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		return nil, &apiError{Status: resp.StatusCode, Code: envelope.Code, Reason: envelope.Error}
	}
	return raw, nil
}

// writeVerbatim prints raw (a broker response body, or devkey's own constructed JSON) followed by
// a newline, for terminal readability.
func writeVerbatim(w io.Writer, raw []byte) {
	w.Write(raw)
	fmt.Fprintln(w)
}

// ---------------------------------------------------------------------------
// wire-shape mirrors (contract v9), for decoding just the fields this command needs
// ---------------------------------------------------------------------------

type registerBeginResponse struct {
	CeremonyID string `json:"ceremony_id"`
	PublicKey  struct {
		RP struct {
			ID string `json:"id"`
		} `json:"rp"`
		Challenge string `json:"challenge"`
	} `json:"publicKey"`
}

type endorseBeginResponse struct {
	CeremonyID string `json:"ceremony_id"`
	PublicKey  struct {
		RPID      string `json:"rpId"`
		Challenge string `json:"challenge"`
	} `json:"publicKey"`
}

type recordChallenges struct {
	Approve string `json:"approve"`
	Deny    string `json:"deny"`
}

// recordResponse is GET /v1/credential-requests/{id}'s shape, reused verbatim for
// POST /v1/machine-logins/lookup (which shares the same fields, per contract v9).
type recordResponse struct {
	RecordID   string            `json:"record_id"`
	Kind       string            `json:"kind"`
	State      string            `json:"state"`
	Challenges *recordChallenges `json:"challenges"`
}

// ---------------------------------------------------------------------------
// register
// ---------------------------------------------------------------------------

var nonceHexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func randomNonceHex() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func cmdRegister(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-secrets-devkey register", flag.ContinueOnError)
	fs.SetOutput(stderr)
	login := fs.String("login", "", "approver login (required)")
	seed := fs.Bool("seed", false, "bootstrap a fresh identity offline (no broker call); prints seed-ready JSON")
	nonce := fs.String("nonce", "", "64-hex registration nonce (--seed only; random if omitted)")
	c := registerCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage(err)
	}
	canonLogin := record.CanonicalLogin(*login)
	if canonLogin == "" {
		fmt.Fprintln(stderr, "agent-secrets-devkey register: --login is required")
		return 1
	}
	if c.state == "" {
		fmt.Fprintln(stderr, "agent-secrets-devkey register: --state (or AGENT_SECRETS_DEVKEY_STATE) is required")
		return 1
	}
	rpID, err := hostOf(c.origin)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register: %v\n", err)
		return 1
	}
	if *seed {
		return cmdRegisterSeed(canonLogin, c.origin, rpID, c.state, *nonce, stdout, stderr)
	}
	return cmdRegisterOnline(canonLogin, c, rpID, stdout, stderr)
}

// seedOutput is "register --seed"'s printed JSON: everything scripts/dev-broker.sh needs to embed
// a rules-file approvers.logins.<login>.keys entry (with "seed: true") and insert the matching
// approver_key_seeds row before the broker's first boot (contract v9 ruling 9).
type seedOutput struct {
	Login          string          `json:"login"`
	CredentialID   string          `json:"credential_id"`
	AAGUID         string          `json:"aaguid"`
	ChallengeNonce string          `json:"challenge_nonce"`
	Registration   json.RawMessage `json:"registration"`
	CAPEM          string          `json:"ca_pem"`
}

func cmdRegisterSeed(login, origin, rpID, state, nonce string, stdout, stderr io.Writer) int {
	if identityExists(state) {
		fmt.Fprintf(stderr, "agent-secrets-devkey register --seed: an identity already exists at %s; pick a fresh --state directory\n", state)
		return 1
	}
	if nonce == "" {
		var err error
		if nonce, err = randomNonceHex(); err != nil {
			fmt.Fprintf(stderr, "agent-secrets-devkey register --seed: generate nonce: %v\n", err)
			return 1
		}
	} else if !nonceHexPattern.MatchString(nonce) {
		fmt.Fprintln(stderr, "agent-secrets-devkey register --seed: --nonce must be 64 lowercase hex characters")
		return 1
	}
	ca, err := withTB(func(t testing.TB) *webauthntest.CA { return webauthntest.NewCA(t) })
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register --seed: %v\n", err)
		return 1
	}
	aaguid := uuid.New()
	auth, err := withTB(func(t testing.TB) *webauthntest.Authenticator { return ca.NewAuthenticator(t, aaguid) })
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register --seed: %v\n", err)
		return 1
	}
	challenge := record.RegisterChallenge(login, nonce)
	regJSON, err := withTB(func(t testing.TB) json.RawMessage { return auth.Register(t, rpID, origin, challenge[:]) })
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register --seed: %v\n", err)
		return 1
	}
	id, err := newIdentityFrom(login, auth)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register --seed: %v\n", err)
		return 1
	}
	if err := saveIdentity(state, id); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register --seed: %v\n", err)
		return 1
	}
	out, err := json.MarshalIndent(seedOutput{
		Login: login, CredentialID: id.CredentialID, AAGUID: aaguid.String(),
		ChallengeNonce: nonce, Registration: regJSON, CAPEM: string(ca.RootPEM),
	}, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register --seed: %v\n", err)
		return 1
	}
	writeVerbatim(stdout, out)
	return 0
}

func cmdRegisterOnline(login string, c *commonFlags, rpID string, stdout, stderr io.Writer) int {
	if err := c.validateForOnline(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register: %v\n", err)
		return 1
	}
	id, err := loadIdentity(c.state)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register: %v\n", err)
		return 1
	}
	auth, err := restoreAuthenticator(id)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register: restore identity: %v\n", err)
		return 1
	}
	client := newUIClient(c.broker, c.uiToken)
	ctx := context.Background()
	path := "/v1/approvers/" + url.PathEscape(login) + "/keys/register/"
	beginRaw, err := client.do(ctx, http.MethodPost, path+"begin", nil)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register: begin: %v\n", err)
		return 1
	}
	var begin registerBeginResponse
	if err := json.Unmarshal(beginRaw, &begin); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register: decode begin response: %v\n", err)
		return 1
	}
	challenge, err := base64.RawURLEncoding.DecodeString(begin.PublicKey.Challenge)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register: decode challenge: %v\n", err)
		return 1
	}
	regJSON, err := withTB(func(t testing.TB) json.RawMessage { return auth.Register(t, rpID, c.origin, challenge) })
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register: %v\n", err)
		return 1
	}
	finishRaw, err := client.do(ctx, http.MethodPost, path+"finish",
		map[string]any{"ceremony_id": begin.CeremonyID, "response": regJSON})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey register: finish: %v\n", err)
		return 1
	}
	writeVerbatim(stdout, finishRaw)
	return 0
}

// ---------------------------------------------------------------------------
// approve / deny
// ---------------------------------------------------------------------------

func cmdDecide(approve bool, args []string, stdout, stderr io.Writer) int {
	name := "deny"
	if approve {
		name = "approve"
	}
	fs := flag.NewFlagSet("agent-secrets-devkey "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	recordID := fs.String("record", "", "credential-request record id (required)")
	c := registerCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage(err)
	}
	if *recordID == "" {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: --record is required\n", name)
		return 1
	}
	if err := c.validateForOnline(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: %v\n", name, err)
		return 1
	}
	rpID, err := hostOf(c.origin)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: %v\n", name, err)
		return 1
	}
	id, err := loadIdentity(c.state)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: %v\n", name, err)
		return 1
	}
	auth, err := restoreAuthenticator(id)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: restore identity: %v\n", name, err)
		return 1
	}
	client := newUIClient(c.broker, c.uiToken)
	ctx := context.Background()
	recRaw, err := client.do(ctx, http.MethodGet, "/v1/credential-requests/"+url.PathEscape(*recordID), nil)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: read record: %v\n", name, err)
		return 1
	}
	var rec recordResponse
	if err := json.Unmarshal(recRaw, &rec); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: decode record: %v\n", name, err)
		return 1
	}
	if rec.Challenges == nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: record %s carries no challenges (not pending, or a machine login — use `machine-approve`)\n", name, *recordID)
		return 1
	}
	challengeB64 := rec.Challenges.Deny
	if approve {
		challengeB64 = rec.Challenges.Approve
	}
	challenge, err := base64.RawURLEncoding.DecodeString(challengeB64)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: decode challenge: %v\n", name, err)
		return 1
	}
	assertion, err := withTB(func(t testing.TB) json.RawMessage { return auth.Assert(t, rpID, c.origin, challenge) })
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: %v\n", name, err)
		return 1
	}
	if err := saveIdentity(c.state, updatedIdentity(id, auth)); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: persist advanced sign count: %v\n", name, err)
		return 1
	}
	decideRaw, err := client.do(ctx, http.MethodPost, "/v1/credential-requests/"+url.PathEscape(*recordID)+"/"+name,
		map[string]any{"assertion": json.RawMessage(assertion)})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey %s: %v\n", name, err)
		return 1
	}
	writeVerbatim(stdout, decideRaw)
	return 0
}

// ---------------------------------------------------------------------------
// endorse
// ---------------------------------------------------------------------------

func cmdEndorse(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-secrets-devkey endorse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	login := fs.String("login", "", "the login whose key set is being extended (required)")
	newCredentialID := fs.String("new-credential-id", "", "the new key's base64url credential id, e.g. from `register`'s printed credential_id (required)")
	credentialID := fs.String("credential-id", "", "the endorsing (existing, live) key's own credential id; defaults to this identity's own")
	c := registerCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage(err)
	}
	canonLogin := record.CanonicalLogin(*login)
	if canonLogin == "" || *newCredentialID == "" {
		fmt.Fprintln(stderr, "agent-secrets-devkey endorse: --login and --new-credential-id are required")
		return 1
	}
	if err := c.validateForOnline(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: %v\n", err)
		return 1
	}
	rpID, err := hostOf(c.origin)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: %v\n", err)
		return 1
	}
	id, err := loadIdentity(c.state)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: %v\n", err)
		return 1
	}
	auth, err := restoreAuthenticator(id)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: restore identity: %v\n", err)
		return 1
	}
	existingCredentialID := *credentialID
	if existingCredentialID == "" {
		existingCredentialID = id.CredentialID
	}
	newCredBytes, err := base64.RawURLEncoding.DecodeString(*newCredentialID)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: --new-credential-id: %v\n", err)
		return 1
	}
	keyHash := sha256.Sum256(newCredBytes)
	client := newUIClient(c.broker, c.uiToken)
	ctx := context.Background()
	path := "/v1/approvers/" + url.PathEscape(canonLogin) + "/keys/endorse/"
	beginRaw, err := client.do(ctx, http.MethodPost, path+"begin",
		map[string]any{"credential_id": existingCredentialID, "key_hash": hex.EncodeToString(keyHash[:])})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: begin: %v\n", err)
		return 1
	}
	var begin endorseBeginResponse
	if err := json.Unmarshal(beginRaw, &begin); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: decode begin response: %v\n", err)
		return 1
	}
	challenge, err := base64.RawURLEncoding.DecodeString(begin.PublicKey.Challenge)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: decode challenge: %v\n", err)
		return 1
	}
	assertion, err := withTB(func(t testing.TB) json.RawMessage { return auth.Assert(t, rpID, c.origin, challenge) })
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: %v\n", err)
		return 1
	}
	if err := saveIdentity(c.state, updatedIdentity(id, auth)); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: persist advanced sign count: %v\n", err)
		return 1
	}
	finishRaw, err := client.do(ctx, http.MethodPost, path+"finish",
		map[string]any{"ceremony_id": begin.CeremonyID, "response": json.RawMessage(assertion)})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey endorse: finish: %v\n", err)
		return 1
	}
	writeVerbatim(stdout, finishRaw)
	return 0
}

// ---------------------------------------------------------------------------
// machine-approve
// ---------------------------------------------------------------------------

func cmdMachineApprove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-secrets-devkey machine-approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	code := fs.String("code", "", "the machine login's confirmation code, XXXX-XXXX (required)")
	deny := fs.Bool("deny", false, "deny the machine login instead of approving it")
	c := registerCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage(err)
	}
	if *code == "" {
		fmt.Fprintln(stderr, "agent-secrets-devkey machine-approve: --code is required")
		return 1
	}
	if err := c.validateForOnline(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: %v\n", err)
		return 1
	}
	rpID, err := hostOf(c.origin)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: %v\n", err)
		return 1
	}
	id, err := loadIdentity(c.state)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: %v\n", err)
		return 1
	}
	auth, err := restoreAuthenticator(id)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: restore identity: %v\n", err)
		return 1
	}
	client := newUIClient(c.broker, c.uiToken)
	ctx := context.Background()
	lookupRaw, err := client.do(ctx, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": *code})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: lookup: %v\n", err)
		return 1
	}
	var rec recordResponse
	if err := json.Unmarshal(lookupRaw, &rec); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: decode lookup: %v\n", err)
		return 1
	}
	if rec.Challenges == nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: code %s carries no challenges (already decided or expired)\n", *code)
		return 1
	}
	action, challengeB64 := "approve", rec.Challenges.Approve
	if *deny {
		action, challengeB64 = "deny", rec.Challenges.Deny
	}
	challenge, err := base64.RawURLEncoding.DecodeString(challengeB64)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: decode challenge: %v\n", err)
		return 1
	}
	assertion, err := withTB(func(t testing.TB) json.RawMessage { return auth.Assert(t, rpID, c.origin, challenge) })
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: %v\n", err)
		return 1
	}
	if err := saveIdentity(c.state, updatedIdentity(id, auth)); err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: persist advanced sign count: %v\n", err)
		return 1
	}
	decideRaw, err := client.do(ctx, http.MethodPost, "/v1/credential-requests/"+url.PathEscape(rec.RecordID)+"/"+action,
		map[string]any{"assertion": json.RawMessage(assertion), "code": *code})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets-devkey machine-approve: %v\n", err)
		return 1
	}
	writeVerbatim(stdout, decideRaw)
	return 0
}
