// packages/envoy/internal/broker/helper/broker.go
//go:build linux

package helper

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// noCredentialMsg is the exact instruction returned whenever the helper has no machine
// credential to authenticate the launcher routes with: never having logged in, and having dropped
// its credential (drop), because the broker refused it with 401 LAUNCHER_INVALID (clearOnInvalid),
// refused its refresh (refresh), or it reached its expiry without a refresh (watchExpiry). It
// never auto-relogins; a login is a human ceremony.
const noCredentialMsg = "no machine credential; run: agent-secrets machine login"

var errNoCredential = errors.New(noCredentialMsg)

// The refresh loop's defaults: a refresh a day at most, and after a refresh whose outcome the
// helper did not learn (it did not reach the broker, its answer was lost, or the broker answered
// 5xx) a retry 15 s later, doubling to at most an hour. The first retry lands well inside the
// broker's five-minute rotation grace, during which the refresh token the helper still holds is
// redeemable again even if the lost answer had rotated it.
const (
	defaultRefreshEvery = 24 * time.Hour
	defaultRefreshRetry = 15 * time.Second
	maxRefreshRetry     = time.Hour
)

// machineCredential is the helper's launcher identity: the machine key, the launcher credential
// the broker minted once a human approved the machine login, when that credential lapses unless a
// refresh moves it, and the refresh token the next refresh spends. The same login is kept on disk
// (MachineFile) so a restart resumes it. A refresh replaces the held machineCredential with one
// carrying the new expiry and refresh token; gone closes when the helper stops holding it.
type machineCredential struct {
	key          *ecdsa.PrivateKey
	id           string
	expiresAt    time.Time
	refreshToken string
	gone         chan struct{}
}

// credential is the in-memory credential f describes.
func (f *MachineFile) credential() *machineCredential {
	return &machineCredential{key: f.Key, id: f.CredentialID, expiresAt: f.ExpiresAt.UTC(), refreshToken: f.RefreshToken, gone: make(chan struct{})}
}

// loginState is the machine login as login-status reports it: the most recent login's
// human-facing confirmation code and its state, beside whether the helper holds a launcher
// credential and when that credential expires. Those are separate facts, since a re-login that is
// denied, expires unapproved or is still pending leaves the credential an earlier login installed
// in place, and the helper keeps enrolling sessions with it. A login the helper resumed from its
// machine file at start reads "issued" with no code. Dropped is why the helper dropped the
// credential it held (dropRefused, dropRefreshRefused or dropExpired), set while no login has been
// recorded since, so the helper holds none; Refused is Dropped != "", the wire's login_refused.
// State then reads "expired" whatever the most recent login's was, unless that login is still
// pending. "expired" is the word the dotfiles launcher gate matches and the one an older client
// prints its refusal line for, and an older client must never read "issued" for a credential the
// helper no longer holds. A login pending at the drop that then settles is recorded after it, so it
// reports its own outcome, which `machine login` prints. "expired" is otherwise a pending login
// nobody approved in time.
type loginState struct {
	Code, State         string    // the most recent login; State: pending|issued|denied|expired
	VerificationURI     string    // where a person types Code: the device authorization's verification_uri; set while the login is pending
	CredentialHeld      bool      // the helper holds a launcher credential, whichever login installed it
	CredentialExpiresAt time.Time // when the held credential lapses without a refresh; zero while none is held
	Refused             bool      // Dropped != ""
	Dropped             string    // why the helper dropped the credential it held, while no login has been recorded since
}

// Broker is the helper's view of the secrets broker: the launcher routes (Enroll, Revoke)
// authenticate with a machineCredential minted by a human-approved machine login (Login, an OAuth
// device grant on the broker's own token endpoint), kept at MachineFile and refreshed daily
// (Start), and Renew authenticates with the session's own proof, exactly as before.
type Broker struct {
	URL          string
	OperatorFile string // login_hint source; read per login, as before
	// MachineFile is where the helper keeps its machine login across restarts (AGENT_SECRETS_MACHINE_FILE).
	MachineFile string
	HTTP        *http.Client
	// Log records every change of the launcher credential: a machine login installing one, a
	// restart resuming one, a refresh moving its expiry, the warning a day before it lapses, and
	// the helper dropping it, when the broker refuses it or its refresh or it reaches its expiry.
	// Its lines carry the credential id, its expiry, the operator the login was signed with (its
	// login_hint) and the broker's refusal code, which are identifiers, never a proof, a token, a
	// request object or key material. Nil logs nothing.
	Log *slog.Logger

	cred    atomic.Pointer[machineCredential]
	loginMu sync.Mutex                 // one login at a time
	login   atomic.Pointer[loginState] // most recent login: code, state
	// credMu is held by whatever changes the held login and its machine file together: a login's
	// save and install, a start's install, and a refresh from its request to its save and swap, so
	// a refresh never races a login's install and the file always names the credential in memory.
	credMu sync.Mutex
	// stateMu is held by every write of cred, login and dropped, and by LoginStatus while it reads
	// the three, so login-status never pairs one moment's login with another's credential, and a
	// drop is recorded with the clear that caused it. cred and login stay atomics so a reader of
	// only one (HasCredential, launcherProof, Login's pending check) takes no lock.
	stateMu sync.Mutex
	dropped string // why drop cleared cred (dropRefused, dropRefreshRefused, dropExpired); "" once a login is recorded since; guarded by stateMu
	// testClearHook, when set, runs in drop between clearing the credential and recording why.
	// testBeforeDrop, when set, runs in dropOutsideRefresh before it waits for credMu. They exist
	// only so a test can read login-status there, or know a drop is waiting on a refresh; no
	// production caller sets them.
	testClearHook, testBeforeDrop func()
	// testAfter, when set, gives watchExpiry a test's channel in place of time.After for a
	// credential it returns one for (nil keeps time.After), so a test fires an expiry's timers
	// without waiting for them. No production caller sets it.
	testAfter func(cred *machineCredential, d time.Duration) <-chan time.Time
	// expiryWarning is how long before the credential's expiry the helper warns that no refresh
	// has moved it; zero means a day. Only tests shorten it.
	expiryWarning time.Duration
	// refreshEvery is the longest the refresh loop waits after a refresh that reached the broker
	// or a login's install (nextRefresh), and refreshRetry its first wait after one whose outcome
	// it did not learn (doubling to an hour); zero means a day and 15 s. Only tests shorten them.
	refreshEvery, refreshRetry time.Duration

	// signalsOnce makes signals, the refresh loop's wake-ups (refreshSignals).
	signalsOnce sync.Once
	signals     refreshSignals

	installedMu sync.Mutex
	installed   chan struct{} // closed, then replaced, each time a credential is installed
}

// BrokerError is a non-success answer, with the contract's code.
type BrokerError struct {
	Status  int
	Code    string
	Message string
}

func (e *BrokerError) Error() string {
	return fmt.Sprintf("broker %d %s: %s", e.Status, e.Code, e.Message)
}

// readOperator reads OperatorFile, trimmed, per call: it names the login_hint for a machine
// login and the "operator" field of every enrollment.
func (b *Broker) readOperator() (string, error) {
	data, err := os.ReadFile(b.OperatorFile)
	if err != nil {
		return "", fmt.Errorf("%s: %w", b.OperatorFile, err)
	}
	operator := strings.TrimSpace(string(data))
	if operator == "" {
		return "", fmt.Errorf("%s is empty", b.OperatorFile)
	}
	return operator, nil
}

// Operator reads OperatorFile for display only (the "operator" field on `sessions` and
// `register` socket responses): it returns "" on any read failure rather than an error, since
// this field is cosmetic and never security-relevant — unlike readOperator, whose callers use
// its value to build authenticated broker requests and so must see the error.
func (b *Broker) Operator() string {
	operator, _ := b.readOperator()
	return operator
}

// oauthConfig is the OAuth client the helper logs a machine in and refreshes its login with: the
// broker's device authorization and token endpoints, the client's id sent in the form.
func (b *Broker) oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID: oauthwire.ClientID, Scopes: []string{oauthwire.Scope},
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: b.URL + "/v1/oauth/device_authorization",
			TokenURL:      b.URL + "/v1/oauth/token",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

// httpClient is b.HTTP, or http.DefaultClient when it is nil, as net/http reads a nil client.
func (b *Broker) httpClient() *http.Client {
	if b.HTTP == nil {
		return http.DefaultClient
	}
	return b.HTTP
}

// tokenContext is ctx carrying the client x/oauth2 sends token requests with: b.HTTP's transport
// and timeout, with a DPoP proof of key on every request, as the broker's token endpoint requires.
func (b *Broker) tokenContext(ctx context.Context, key *ecdsa.PrivateKey) context.Context {
	client := b.httpClient()
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: NewDPoPTransport(key, base), Timeout: client.Timeout})
}

// oauthBrokerError is err as a BrokerError when the broker answered it with an OAuth error: the
// device authorization endpoint's own refusals (invalid_request, described "<CODE>: <message>")
// carry the broker's code and message, any other OAuth error its error code and description.
func oauthBrokerError(err error) error {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return err
	}
	be := &BrokerError{Code: re.ErrorCode, Message: re.ErrorDescription}
	if re.Response != nil {
		be.Status = re.Response.StatusCode
	}
	if code, message, ok := strings.Cut(re.ErrorDescription, ": "); ok && re.ErrorCode == "invalid_request" {
		be.Code, be.Message = code, message
	}
	return be
}

// machineFileFrom is the machine login a token answer issues for key: the refresh token and the
// broker's extras naming the launcher credential and when it lapses without a refresh.
func machineFileFrom(key *ecdsa.PrivateKey, tok *oauth2.Token) (*MachineFile, error) {
	id, _ := tok.Extra(oauthwire.ExtraCredentialID).(string)
	raw, _ := tok.Extra(oauthwire.ExtraCredentialExpiresAt).(string)
	expiresAt, err := time.Parse(time.RFC3339, raw)
	if id == "" || err != nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("the broker's token answer lacks a %s, an RFC 3339 %s or a refresh_token", oauthwire.ExtraCredentialID, oauthwire.ExtraCredentialExpiresAt)
	}
	return &MachineFile{Key: key, CredentialID: id, ExpiresAt: expiresAt.UTC(), RefreshToken: tok.RefreshToken}, nil
}

// Login starts (or reports the running) machine login, an OAuth device grant: a fresh key, signed
// as a launcher_credential request object naming hostname and the OperatorFile's login as
// login_hint, sent to the device authorization endpoint with the key's thumbprint (dpop_jkt). It
// returns the user code for the CLI to print (login-status carries the answer's verification_uri
// beside it while the login is pending), and a background goroutine polls the token endpoint
// until a human decides it (pollLogin), keeping and installing the login on approval or recording
// denial/expiry. It never retries past a terminal state, and a later Login while one is still
// pending just reports it. A login while a credential is held mints a new key and a new login,
// which replaces the held one only once it is issued.
func (b *Broker) Login(ctx context.Context, hostname string) (string, error) {
	b.loginMu.Lock()
	defer b.loginMu.Unlock()
	if ls := b.login.Load(); ls != nil && ls.State == "pending" {
		return ls.Code, nil
	}
	key, err := proof.NewKey()
	if err != nil {
		return "", err
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return "", err
	}
	operator, err := b.readOperator()
	if err != nil {
		return "", err
	}
	details := []record.AuthorizationDetail{{Type: record.KindLauncherCredential, Identifier: hostname}}
	compact, err := record.Sign(key, b.URL, details, "", operator, nil, time.Now())
	if err != nil {
		return "", err
	}
	cfg := b.oauthConfig()
	da, err := cfg.DeviceAuth(context.WithValue(ctx, oauth2.HTTPClient, b.httpClient()),
		oauth2.SetAuthURLParam("request", compact), oauth2.SetAuthURLParam("dpop_jkt", thumbprint))
	if err != nil {
		return "", oauthBrokerError(err)
	}
	if da.DeviceCode == "" || da.UserCode == "" {
		return "", errors.New("broker returned no device_code/user_code")
	}
	b.recordLogin(&loginState{Code: da.UserCode, State: "pending", VerificationURI: da.VerificationURI})
	go b.pollLogin(key, cfg, da, operator)
	return da.UserCode, nil
}

// LoginStatus reports the most recent machine login, current or settled, whether the helper holds
// a launcher credential and when that credential lapses without a refresh; the zero value means no
// login has run or been resumed. A login that did not issue says nothing about the credential an
// earlier one installed: CredentialHeld does. Once the helper has dropped the credential (drop: the
// broker refused it or its refresh, or it reached its expiry), and until a login is recorded (one
// starts, or one pending at the drop settles), every state but pending reads "expired", refused and
// why it was dropped; an issued login is among them, since only drop ever clears a credential an
// issued login installed. The login, the credential and the drop are read under stateMu, so the
// answer is all three of one moment.
func (b *Broker) LoginStatus() loginState {
	b.stateMu.Lock()
	ls, cred, dropped := b.login.Load(), b.cred.Load(), b.dropped
	b.stateMu.Unlock()
	if ls == nil {
		return loginState{}
	}
	out := *ls
	out.CredentialHeld = cred != nil
	if cred != nil {
		out.CredentialExpiresAt = cred.expiresAt
	}
	if !out.CredentialHeld && dropped != "" {
		out.Refused, out.Dropped = true, dropped
		if out.State != "pending" {
			out.State = "expired"
		}
	}
	return out
}

// noCredentialReason says why the helper holds no launcher credential, for the error lines that
// say a session cannot enroll: NoCredentialReason of this moment's login-status.
func (b *Broker) noCredentialReason() string {
	ls := b.LoginStatus()
	return NoCredentialReason(ls.State, ls.Refused, ls.Dropped)
}

// recordLogin records ls as the current machine login. Whether the login starts or settles, its
// state is newer than any drop recorded before it, so the same write ends that record and the
// login reports its own state.
func (b *Broker) recordLogin(ls *loginState) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	b.login.Store(ls)
	b.dropped = ""
}

// HasCredential reports whether the helper holds a launcher credential, the one thing every
// enrollment needs.
func (b *Broker) HasCredential() bool {
	return b.cred.Load() != nil
}

// CredentialInstalled returns a channel that closes the next time a machine login installs a
// credential, so a retry that failed for want of one can try again at once rather than when its
// backoff comes round.
func (b *Broker) CredentialInstalled() <-chan struct{} {
	b.installedMu.Lock()
	defer b.installedMu.Unlock()
	if b.installed == nil {
		b.installed = make(chan struct{})
	}
	return b.installed
}

// installCredential makes cred the helper's launcher credential and issued the login that minted
// it, in one write under stateMu that also ends any record of a dropped credential, then wakes
// every waiter on CredentialInstalled, watches cred's expiry (watchExpiry) and has the refresh
// loop schedule its next refresh from cred's expiry (nextRefresh). A credential it replaces is gone from then on, so its expiry watch says nothing more about it.
func (b *Broker) installCredential(cred *machineCredential, issued *loginState) {
	b.stateMu.Lock()
	old := b.cred.Swap(cred)
	b.login.Store(issued)
	b.dropped = ""
	b.stateMu.Unlock()
	if old != nil {
		close(old.gone)
	}
	go b.watchExpiry(cred)
	select {
	case b.refreshSignals().reschedule <- struct{}{}:
	default:
	}
	b.installedMu.Lock()
	defer b.installedMu.Unlock()
	if b.installed != nil {
		close(b.installed)
	}
	b.installed = make(chan struct{})
}

// pollLogin polls the token endpoint for a pending machine login until a human decides it, at the
// interval the broker named. On approval it keeps the login (keepLogin). access_denied records
// "denied"; expired_token, a device code the broker no longer knows (invalid_grant) and the
// code's own lifetime passing record "expired". A poll that does not reach the broker, or that it
// answers with anything else, polls again.
func (b *Broker) pollLogin(key *ecdsa.PrivateKey, cfg *oauth2.Config, da *oauth2.DeviceAuthResponse, operator string) {
	ctx := b.tokenContext(context.Background(), key)
	settle := func(state string) { b.recordLogin(&loginState{Code: da.UserCode, State: state}) }
	for {
		tok, err := cfg.DeviceAccessToken(ctx, da)
		if err == nil {
			b.keepLogin(key, tok, da.UserCode, operator)
			return
		}
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			switch re.ErrorCode {
			case "access_denied":
				settle("denied")
				return
			case "expired_token", "invalid_grant":
				settle("expired")
				return
			}
		}
		if !da.Expiry.IsZero() && !time.Now().Before(da.Expiry) {
			settle("expired")
			return
		}
	}
}

// keepLogin keeps the login an approved device grant issued at MachineFile and only then installs
// it, under credMu, so the file names the login the helper holds and no refresh runs between the
// two; it logs the credential with operator, the login_hint the login was signed with, whatever
// the operator file says by then. An approval the helper cannot keep (a token answer without the
// credential's extras, or a machine file it cannot write) is logged at ERROR and recorded
// "expired", leaving cred untouched.
func (b *Broker) keepLogin(key *ecdsa.PrivateKey, tok *oauth2.Token, code, operator string) {
	f, err := machineFileFrom(key, tok)
	if err == nil {
		b.credMu.Lock()
		if err = f.Save(b.MachineFile); err == nil {
			b.installCredential(f.credential(), &loginState{Code: code, State: "issued"})
		}
		b.credMu.Unlock()
	}
	if err != nil {
		b.logger().Error("machine login approved, but the helper could not keep it; run: agent-secrets machine login again", "error", err)
		b.recordLogin(&loginState{Code: code, State: "expired"})
		return
	}
	b.logger().Info("machine login issued; the helper holds a launcher credential",
		"credential_id", f.CredentialID, "operator", operator, "expires_at", f.ExpiresAt.Format(time.RFC3339))
}

// Start resumes the machine login kept at MachineFile, if there is one, and starts the refresh
// loop (refreshLoop) until ctx ends; serve runs it before it listens. A kept login is refreshed
// once before it is installed, within b.HTTP's timeout, so a restarted helper never reports a
// login the broker has ended: a refresh the broker answers is saved and installed as an issued
// login with no code; one it refuses (invalid_grant: revoked, lapsed or reused) drops the login
// (dropRefreshRefused) and deletes the file; one whose outcome the helper did not learn (the broker
// unreachable, a lost answer, a 5xx) installs the kept login and leaves the refresh loop to retry
// it. A file that is there but is not a machine login is an error naming it.
func (b *Broker) Start(ctx context.Context) error {
	f, err := LoadMachineFile(b.MachineFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		go b.refreshLoop(ctx, b.refreshEveryOrDefault())
		return nil
	case err != nil:
		return err
	}
	kept := f.credential()
	b.credMu.Lock()
	next, refusal, err := b.redeem(ctx, kept)
	first := time.Duration(0)
	switch {
	case refusal != "":
		b.installCredential(kept, &loginState{State: "issued"})
		b.drop(kept, dropRefreshRefused, "code", refusal)
		b.removeRefusedFile()
	case err != nil:
		b.installCredential(kept, &loginState{State: "issued"})
		first = b.refreshRetryOrDefault()
	default:
		if err := next.Save(b.MachineFile); err != nil {
			b.logger().Error("the helper could not keep the machine login's refresh on disk; retrying",
				"credential_id", next.CredentialID, "machine_file", b.MachineFile, "error", err)
			first = b.refreshRetryOrDefault()
		}
		f = next
		b.installCredential(next.credential(), &loginState{State: "issued"})
	}
	b.credMu.Unlock()
	if refusal == "" {
		b.logger().Info("the helper resumed the machine login it kept", "credential_id", f.CredentialID,
			"expires_at", f.ExpiresAt.Format(time.RFC3339), "machine_file", b.MachineFile)
	}
	if first == 0 {
		first = b.nextRefresh(b.refreshEveryOrDefault())
	}
	go b.refreshLoop(ctx, first)
	return nil
}

// refreshSignals wake the refresh loop: reschedule when a credential is installed, so the next
// refresh follows the new credential's expiry (nextRefresh), and refused with a credential a
// launcher call's 401 LAUNCHER_INVALID dropped, so the loop settles its machine file at once
// (settleRefused) rather than at the next scheduled refresh.
type refreshSignals struct {
	reschedule chan struct{}
	refused    chan *machineCredential
}

func (b *Broker) refreshSignals() refreshSignals {
	b.signalsOnce.Do(func() {
		b.signals = refreshSignals{reschedule: make(chan struct{}, 1), refused: make(chan *machineCredential, 1)}
	})
	return b.signals
}

// wakeRefused hands cred, just dropped for a 401 LAUNCHER_INVALID, to the refresh loop; a
// credential handed earlier and not yet taken is replaced, since only the latest drop's file
// matters.
func (b *Broker) wakeRefused(cred *machineCredential) {
	ch := b.refreshSignals().refused
	for {
		select {
		case ch <- cred:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}

func (b *Broker) refreshEveryOrDefault() time.Duration {
	if b.refreshEvery == 0 {
		return defaultRefreshEvery
	}
	return b.refreshEvery
}

func (b *Broker) refreshRetryOrDefault() time.Duration {
	if b.refreshRetry == 0 {
		return defaultRefreshRetry
	}
	return b.refreshRetry
}

// nextRefresh is how long the refresh loop waits before refreshing the held credential: half the
// time left until its expiry, at most every, so a credential the broker mints for less than two
// days is still refreshed well before it lapses.
func (b *Broker) nextRefresh(every time.Duration) time.Duration {
	cred := b.cred.Load()
	if cred == nil {
		return every
	}
	return min(every, max(0, time.Until(cred.expiresAt)/2))
}

// refreshLoop refreshes the held machine login after first, then at nextRefresh after each
// refresh that reached the broker and each install of a credential; one whose outcome it did not
// learn is retried refreshRetry later (15 s), the wait doubling to an hour. A credential a
// launcher call's refusal dropped is settled at once (settleRefused), and retried the same way
// until it is. It ends with ctx.
func (b *Broker) refreshLoop(ctx context.Context, first time.Duration) {
	every, retry := b.refreshEveryOrDefault(), b.refreshRetryOrDefault()
	sig := b.refreshSignals()
	// An install before the loop started asked for a reschedule first already accounts for.
	select {
	case <-sig.reschedule:
	default:
	}
	backoff := retry
	timer := time.NewTimer(first)
	defer timer.Stop()
	var settle *machineCredential
	for {
		select {
		case <-ctx.Done():
			return
		case <-sig.reschedule:
			if settle == nil {
				backoff = retry
				timer.Reset(b.nextRefresh(every))
			}
			continue
		case settle = <-sig.refused:
			timer.Reset(0)
			continue
		case <-timer.C:
		}
		var err error
		if settle != nil {
			err = b.settleRefused(ctx, settle)
			if err == nil || b.HasCredential() {
				// Settled, or settled in memory with its save failed: the held login is then
				// refreshed and saved like any other, at the retry below.
				settle = nil
			}
			// settleRefused's install asked for a reschedule the retry below replaces.
			select {
			case <-sig.reschedule:
			default:
			}
		} else {
			err = b.refresh(ctx)
		}
		if err != nil {
			timer.Reset(backoff)
			backoff = min(2*backoff, maxRefreshRetry)
			continue
		}
		backoff = retry
		timer.Reset(b.nextRefresh(every))
	}
}

// redeem spends cred's refresh token for the rotated login: the machine file the broker's answer
// names, or refusal, the broker's invalid_grant code, when it has ended the login (revoked, lapsed
// or reused), or another error, logged, when the helper did not learn the outcome or cannot keep
// the answer. The caller holds credMu.
func (b *Broker) redeem(ctx context.Context, cred *machineCredential) (next *MachineFile, refusal string, err error) {
	tok, err := b.oauthConfig().TokenSource(b.tokenContext(ctx, cred.key),
		&oauth2.Token{RefreshToken: cred.refreshToken, Expiry: time.Unix(1, 0)}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			return nil, re.ErrorCode, err
		}
		b.logger().Warn("the machine login's refresh did not reach the broker; retrying", "credential_id", cred.id, "error", err)
		return nil, "", err
	}
	next, err = machineFileFrom(cred.key, tok)
	if err != nil {
		b.logger().Error("the machine login's refresh answer cannot be kept; retrying", "credential_id", cred.id, "error", err)
		return nil, "", err
	}
	return next, "", nil
}

// removeRefusedFile deletes the machine file of a login the broker refused to refresh.
func (b *Broker) removeRefusedFile() {
	if err := RemoveMachineFile(b.MachineFile); err != nil {
		b.logger().Error("removing the refused machine login's file", "machine_file", b.MachineFile, "error", err)
	}
}

// refresh spends the held login's refresh token for a new one and the credential's new expiry,
// under credMu, so it never races a login's install. It saves the new login to MachineFile, then
// swaps it in for the credential it refreshed. The broker's invalid_grant (the login was revoked,
// lapsed, or its refresh token was reused) drops the credential (dropRefreshRefused) and deletes
// the machine file; any other failure keeps both and is returned, so the loop retries. The old
// refresh token is spent once the broker answers, so a new login the file cannot take is still
// swapped in, and the failed save is logged and returned to be retried with the next refresh.
// Holding no credential, it does nothing.
func (b *Broker) refresh(ctx context.Context) error {
	b.credMu.Lock()
	defer b.credMu.Unlock()
	cred := b.cred.Load()
	if cred == nil {
		return nil
	}
	f, refusal, err := b.redeem(ctx, cred)
	if refusal != "" {
		b.drop(cred, dropRefreshRefused, "code", refusal)
		b.removeRefusedFile()
		return nil
	}
	if err != nil {
		return err
	}
	saveErr := f.Save(b.MachineFile)
	if saveErr != nil {
		b.logger().Error("the helper could not keep the machine login's refresh on disk; a restart before the next refresh loses the login",
			"credential_id", f.CredentialID, "machine_file", b.MachineFile, "error", saveErr)
	}
	next := f.credential()
	b.stateMu.Lock()
	swapped := b.cred.CompareAndSwap(cred, next)
	b.stateMu.Unlock()
	if swapped {
		close(cred.gone)
		go b.watchExpiry(next)
		b.logger().Info("machine login refreshed", "credential_id", f.CredentialID, "expires_at", f.ExpiresAt.Format(time.RFC3339))
	}
	return saveErr
}

// settleRefused refreshes cred, a credential the helper dropped because the broker answered a
// launcher call 401 LAUNCHER_INVALID, so its machine file follows the broker within seconds: a
// refresh the broker refuses (invalid_grant) deletes the file; one it answers keeps the rotated
// login in the file, since a 401 can also be a proof the broker could not verify (clock skew, an
// AGENT_SECRETS_URL mismatch), and a restart resumes it. The helper then still holds no
// credential, unless that save fails: the rotated login is then installed, so the helper keeps
// working and the refresh loop retries its save, and the save's error is returned. Once a newer
// login holds the file it does nothing; any other error is the refresh's whose outcome it did not
// learn, for the loop to retry.
func (b *Broker) settleRefused(ctx context.Context, cred *machineCredential) error {
	b.credMu.Lock()
	defer b.credMu.Unlock()
	if b.cred.Load() != nil {
		return nil
	}
	f, refusal, err := b.redeem(ctx, cred)
	switch {
	case refusal != "":
		b.logger().Error(dropRefreshRefused+"; removed the machine file", "credential_id", cred.id, "code", refusal, "machine_file", b.MachineFile)
		b.removeRefusedFile()
		return nil
	case err != nil:
		return err
	}
	if err := f.Save(b.MachineFile); err != nil {
		b.logger().Error("the helper could not keep the machine login's refresh on disk; holding it and retrying",
			"credential_id", f.CredentialID, "machine_file", b.MachineFile, "error", err)
		b.installCredential(f.credential(), &loginState{State: "issued"})
		return err
	}
	return nil
}

// watchExpiry warns a day (expiryWarning) before cred lapses, at once when less than that is left,
// and drops cred when it reaches its expiry (dropExpired), as a broker refusal would: the login
// renews itself only while a refresh reaches the broker, and from then on no session can enroll
// until a human approves a new machine login. It ends early, silently, once the helper stops
// holding cred (a refusal, a refresh, or a newer login).
func (b *Broker) watchExpiry(cred *machineCredential) {
	lead := b.expiryWarning
	if lead == 0 {
		lead = 24 * time.Hour
	}
	select {
	case <-b.after(cred, time.Until(cred.expiresAt.Add(-lead))):
	case <-cred.gone:
		return
	}
	if b.isGone(cred) {
		return
	}
	b.logger().Warn(expiresSoonMsg,
		"credential_id", cred.id, "expires_at", cred.expiresAt.Format(time.RFC3339), "in", time.Until(cred.expiresAt).Round(time.Second))
	select {
	case <-b.after(cred, time.Until(cred.expiresAt)):
		b.dropOutsideRefresh(cred, dropExpired, "expires_at", cred.expiresAt.Format(time.RFC3339))
	case <-cred.gone:
	}
}

// isGone reports whether the helper has stopped holding cred, so a timer that fired as it was
// replaced says nothing about it.
func (b *Broker) isGone(cred *machineCredential) bool {
	select {
	case <-cred.gone:
		return true
	default:
		return false
	}
}

// after is time.After(d), or testAfter's channel for cred when a test sets it.
func (b *Broker) after(cred *machineCredential, d time.Duration) <-chan time.Time {
	if b.testAfter != nil {
		if ch := b.testAfter(cred, d); ch != nil {
			return ch
		}
	}
	return time.After(d)
}

// dropOutsideRefresh is drop for a caller that does not hold credMu (watchExpiry, clearOnInvalid):
// it waits for a refresh in flight to finish, so a refresh that succeeds always ends with the
// helper holding the refreshed credential. A drop of the credential that refresh replaced then
// finds it gone and drops nothing. It reports whether it dropped cred and whether the helper holds
// a credential afterwards.
func (b *Broker) dropOutsideRefresh(cred *machineCredential, why string, attrs ...any) (dropped, held bool) {
	if b.testBeforeDrop != nil {
		b.testBeforeDrop()
	}
	b.credMu.Lock()
	defer b.credMu.Unlock()
	return b.drop(cred, why, attrs...), b.HasCredential()
}

// expiresSoonMsg is the WARN watchExpiry logs a day before the held credential lapses.
const expiresSoonMsg = "the launcher credential lapses soon: no refresh has moved its expiry; the machine login renews itself while the helper reaches the broker, otherwise run: agent-secrets machine login, and have a human approve it"

// enrollBody is POST /v1/enrollments's shape in the shared broker contract (the v8 "approver"
// field is gone: the broker's policy picks a request's approver, never the enrollment).
type enrollBody struct {
	Kind       string  `json:"kind"`
	RuntimeID  string  `json:"runtime_id"`
	Operator   string  `json:"operator"`
	Thumbprint string  `json:"thumbprint"`
	SessionID  *string `json:"session_id"`
	PodToken   *string `json:"pod_token"`
}

type leaseReply struct {
	EnrollmentID   string    `json:"enrollment_id"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// launcherProof signs a launcher proof with the current machine credential, or fails with
// errNoCredential if there is none yet. It returns the credential it signed with, so a caller
// whose call the broker then refuses clears exactly that one (clearOnInvalid).
func (b *Broker) launcherProof(method, url string) (string, *machineCredential, error) {
	cred := b.cred.Load()
	if cred == nil {
		return "", nil, errNoCredential
	}
	compact, err := proof.SignLauncher(cred.key, cred.id, method, url, time.Now())
	return compact, cred, err
}

// clearOnInvalid drops the machine credential (dropRefused) and reports it missing whenever err
// is the broker's 401 LAUNCHER_INVALID. The broker answers that for any launcher proof it
// rejects: an expired or revoked credential, and also a proof it cannot verify, such as one
// signed outside its clock-skew window or for a URL other than its own (an AGENT_SECRETS_URL that
// does not match the broker's public URL). It never auto-relogins. cred is the credential the
// refused call was signed with, and only that one is dropped, after any refresh in flight
// (dropOutsideRefresh). A refusal of a credential a newer login or a refresh has already replaced
// drops nothing and is returned as it came: the helper holds a credential, and the caller's retry
// uses it. A credential it drops is handed to the refresh loop, which refreshes it at once so
// its machine file is deleted if the broker has ended the login (settleRefused).
func (b *Broker) clearOnInvalid(cred *machineCredential, err error) error {
	var be *BrokerError
	if !errors.As(err, &be) || be.Status != http.StatusUnauthorized || be.Code != "LAUNCHER_INVALID" {
		return err
	}
	dropped, held := b.dropOutsideRefresh(cred, dropRefused, "code", be.Code)
	if dropped {
		b.wakeRefused(cred)
	}
	if !dropped && held {
		return err
	}
	return errNoCredential
}

// drop stops holding cred, if the helper still does, records why, logs at ERROR that it did,
// naming why and the credential (with attrs), and reports whether it did: a refusal or an expiry
// that arrives after another login has installed a new credential leaves the new one alone and
// says nothing. The clear and the record of why are one write under stateMu, so no LoginStatus
// reads a credential gone without why: until a login is recorded, it reports the credential
// dropped, whatever the most recent login's state, and a login recorded meanwhile ends that
// record after it, never before. login-status — the probe every launcher decides on — then stops
// reporting a credential the helper no longer holds.
func (b *Broker) drop(cred *machineCredential, why string, attrs ...any) bool {
	b.stateMu.Lock()
	cleared := b.cred.CompareAndSwap(cred, nil)
	if cleared {
		if b.testClearHook != nil {
			b.testClearHook()
		}
		b.dropped = why
	}
	b.stateMu.Unlock()
	if !cleared {
		return false
	}
	close(cred.gone)
	b.logger().Error(why+"; cleared: no session can enroll until a human approves a new machine login (run: agent-secrets machine login)",
		append([]any{"credential_id", cred.id}, attrs...)...)
	return true
}

// logger is b.Log, or a logger that writes nowhere when there is none.
func (b *Broker) logger() *slog.Logger {
	if b.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return b.Log
}

// Enroll registers the session's key. 201 is a new enrollment; 200 is the live one for the same
// credential, kind, runtime_id and thumbprint (the contract's idempotent enroll).
func (b *Broker) Enroll(ctx context.Context, s *Session) (string, time.Time, error) {
	return b.enroll(ctx, s.RuntimeID, s.Thumbprint, nil, "host")
}

// EnrollSessionKey registers a key the helper does not hold, one a launcher enrolls for a session
// on this machine (a box's, a container's): a pass-through broker call with no registry Session,
// so it takes the key's runtime_id, thumbprint and optional session_id directly. Otherwise it is
// Enroll: the same fail-closed check when there is no machine credential yet, the same launcher
// proof, the same 200/201 handling and the same 401 LAUNCHER_INVALID handling.
func (b *Broker) EnrollSessionKey(ctx context.Context, runtimeID, thumbprint string, sessionID *string) (string, time.Time, error) {
	return b.enroll(ctx, runtimeID, thumbprint, sessionID, "box")
}

// enroll enrolls a key as kind session. A broker from before that kind (every release before
// the one that records one kind of machine session) answers it 400 INVALID_KIND, and enroll asks
// it once more with legacyKind, the kind that broker knew the enrollment by: host for the
// helper's own sessions, box for a key a launcher brings. That retry goes once the production
// broker records the session kind.
func (b *Broker) enroll(ctx context.Context, runtimeID, thumbprint string, sessionID *string, legacyKind string) (string, time.Time, error) {
	id, lease, err := b.enrollAs(ctx, "session", runtimeID, thumbprint, sessionID)
	var be *BrokerError
	if errors.As(err, &be) && be.Status == http.StatusBadRequest && be.Code == "INVALID_KIND" {
		return b.enrollAs(ctx, legacyKind, runtimeID, thumbprint, sessionID)
	}
	return id, lease, err
}

// enrollAs is one POST /v1/enrollments naming kind, signed with the current machine credential;
// it reads the operator with readOperator, which propagates its error, never the cosmetic
// Operator().
func (b *Broker) enrollAs(ctx context.Context, kind, runtimeID, thumbprint string, sessionID *string) (string, time.Time, error) {
	url := b.URL + "/v1/enrollments"
	compact, cred, err := b.launcherProof(http.MethodPost, url)
	if err != nil {
		return "", time.Time{}, err
	}
	operator, err := b.readOperator()
	if err != nil {
		return "", time.Time{}, err
	}
	body, err := json.Marshal(enrollBody{Kind: kind, RuntimeID: runtimeID, Operator: operator, Thumbprint: thumbprint, SessionID: sessionID})
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Proof", compact)
	req.Header.Set("Content-Type", "application/json")
	var out leaseReply
	if err := b.do(req, []int{http.StatusOK, http.StatusCreated}, &out); err != nil {
		return "", time.Time{}, b.clearOnInvalid(cred, err)
	}
	if out.EnrollmentID == "" {
		return "", time.Time{}, fmt.Errorf("broker returned no enrollment_id")
	}
	return out.EnrollmentID, out.LeaseExpiresAt, nil
}

// Renew extends the lease with a proof signed by the session key. Unchanged: it always
// authenticates with the session's own key, never the launcher credential.
func (b *Broker) Renew(ctx context.Context, s *Session) (time.Time, error) {
	id := s.EnrollmentID()
	url := b.URL + "/v1/enrollments/" + id + "/renew"
	compact, err := proof.Sign(s.Key, id, http.MethodPost, url, time.Now())
	if err != nil {
		return time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Proof", compact)
	var out leaseReply
	if err := b.do(req, []int{http.StatusOK}, &out); err != nil {
		return time.Time{}, err
	}
	return out.LeaseExpiresAt, nil
}

// Revoke deletes the enrollment and every grant under it. 204 and 404 both mean done.
func (b *Broker) Revoke(ctx context.Context, enrollmentID string) error {
	url := b.URL + "/v1/enrollments/" + enrollmentID
	compact, cred, err := b.launcherProof(http.MethodDelete, url)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Proof", compact)
	if err := b.do(req, []int{http.StatusNoContent, http.StatusNotFound}, nil); err != nil {
		return b.clearOnInvalid(cred, err)
	}
	return nil
}

func (b *Broker) do(req *http.Request, ok []int, out any) error {
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if !slices.Contains(ok, resp.StatusCode) {
		var e struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return &BrokerError{Status: resp.StatusCode, Code: e.Code, Message: e.Error}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
