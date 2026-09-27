// Package launcher issues launcher credentials through a Dispatch standing-issue ask (AGENTC-833
// Task 12). A launcher (an operator's box, or an automated service like the Legion daemon) has no
// credential of its own yet, so it cannot authenticate to the enrollment routes; it instead opens
// a request here, which turns into an ask on the requesting operator's own "agent-secrets"
// standing issue in Dispatch. A human answers that ask from their own account, and the poller
// (Poller.Launcher, wired into requests.Poller.RunOnce) reconciles it: approved by the operator
// mints a real launcher credential through enroll.Service, anything else denies it.
package launcher

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/broker/dispatch"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/store"
)

// ErrNotFound is returned by Read when no request row matches the given pending id.
var ErrNotFound = errors.New("launcher credential request not found")

// standingLabel is the Dispatch label that marks an operator's standing secrets issue, and the
// label every ListIssues lookup filters on so an operator's ordinary issues are never mistaken
// for it.
const standingLabel = "agent-secrets"

// pendingTTL is how long an unanswered launcher-credential request stays open before Reconcile
// expires it. Unlike requests.Machine's PendingTTL, which is a caller-supplied policy varying by
// rules configuration, this flow has exactly one lifetime and no wiring supplies a different one.
const pendingTTL = 10 * time.Minute

// dispatchClient is the subset of *dispatch.Client this package needs: opening, reading and
// retracting the standing-issue ask (the same seam requests.Machine and requests.Poller use), plus
// the two issue routes that find or create an operator's standing issue. A small interface
// keeps launcher_test.go's fake self-contained instead of standing up an httptest server.
type dispatchClient interface {
	CreateAsk(ctx context.Context, issue, question string, options []dispatch.Option, urgency string) (dispatch.Ask, error)
	GetAsk(ctx context.Context, id string) (dispatch.Ask, error)
	ListIssues(ctx context.Context, project, label string) ([]dispatch.IssueSummary, error)
	CreateIssue(ctx context.Context, project, title string, assignee *string, labels []string) (string, error)
	RetractAsk(ctx context.Context, id, reason string) error
}

type Service struct {
	Store    *store.Store
	Dispatch dispatchClient
	Enroll   *enroll.Service
	Project  string

	// standing collapses concurrent Standing calls for one operator into one Dispatch lookup.
	standing singleflight.Group
}

// Pending is an opened launcher-credential request as its launcher sees it.
type Pending struct {
	// ID is the opaque capability the launcher polls Read with.
	ID string
	// ConfirmationCode is shown both in the launcher's own terminal and in the Dispatch ask, so
	// the approving human can tell the login they started from an identical-looking request
	// anyone else could open on this unauthenticated route.
	ConfirmationCode string
}

// Request opens a launcher-credential request: it finds or creates the operator's standing issue
// (Standing), opens an Approve/Deny ask on it naming the host (and, for a service credential, the
// service) and a fresh confirmation code, and records a pending row keyed by the sha256 of a
// freshly minted opaque pending id — the same "never store the bearer capability itself" shape
// enroll.Service uses for tokens. The pending id is returned to the caller (the launcher CLI),
// which polls Read with it; nothing about it identifies the eventual credential. No Postgres
// connection is held across either Dispatch call: the row is written in one short transaction
// once the ask exists.
//
// service, when non-nil and non-empty, requests a service credential (like the Legion daemon's
// shared enrollment authority) rather than a personal one: the ask still opens on operator's own
// standing issue (operator is who approves it), but the credential minted on approval (applyAsk)
// carries service with a nil operator instead of the reverse. A non-nil pointer to an empty
// string is normalized to nil right here — the one place service enters this package — so a
// caller (e.g. the HTTP handler, which decodes `{"service": ""}` into a non-nil *string) can never
// silently turn an ordinary operator request into a differently-scoped service credential behind
// ask text that only ever mentions the host; every other use of service in this file
// (launcherQuestion, the service column, and applyAsk's own defensive re-check) sees this same
// normalized value.
func (s *Service) Request(ctx context.Context, operator, host string, service *string) (Pending, error) {
	if service != nil && *service == "" {
		service = nil
	}
	operator = dispatch.CanonicalLogin(operator)
	code, err := confirmationCode()
	if err != nil {
		return Pending{}, err
	}
	issueKey, err := s.Standing(ctx, operator)
	if err != nil {
		return Pending{}, fmt.Errorf("find standing issue: %w", err)
	}
	ask, err := s.Dispatch.CreateAsk(ctx, issueKey, launcherQuestion(host, service, code), []dispatch.Option{
		{Label: "Approve", Description: "Issue this launcher credential."},
		{Label: "Deny", Description: "Refuse; the launcher is told the request was denied."},
	}, "med")
	if err != nil {
		return Pending{}, fmt.Errorf("open Dispatch ask: %w", err)
	}
	pendingID, err := randomToken()
	if err != nil {
		return Pending{}, err
	}
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Pending{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into launcher_credential_requests
		(pending_id_hash, operator, host, service, ask_id, ask_edited_at, state, expires_at)
		values ($1,$2,$3,$4,$5,$6,'pending',$7)`,
		hashPendingID(pendingID), operator, host, service, ask.ID, ask.EditedAt, time.Now().Add(pendingTTL)); err != nil {
		return Pending{}, err
	}
	if err := auditLauncher(ctx, tx, "launcher_request.created", "launcher:"+host, auditDetail(map[string]any{
		"operator": operator, "host": host, "service": service, "ask_id": ask.ID, "confirmation_code": code,
	})); err != nil {
		return Pending{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Pending{}, err
	}
	return Pending{ID: pendingID, ConfirmationCode: code}, nil
}

// Read answers the launcher's poll: "pending", "denied", "expired", or "issued" with the raw
// one-time token exactly once. It locks the row for update and clears token_once only after
// reading it back, and only when state is "issued"; a Read that observes "issued" a second time,
// after the first clear committed, finds token_once already null. credentialID is the launcher
// credential's id whenever state is "issued" (set in the same statement as the state
// transition), whether or not this call also returns its token: a caller that observes "issued"
// with no token (another reader already collected it) still learns which credential that was, so
// it can be named to an operator who needs to investigate and revoke it.
func (s *Service) Read(ctx context.Context, pendingID string) (state, token, credentialID string, err error) {
	h := hashPendingID(pendingID)
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return "", "", "", err
	}
	defer tx.Rollback(ctx)
	var tok *string
	var cred *string
	err = tx.QueryRow(ctx, `select state, token_once, credential_id from launcher_credential_requests where pending_id_hash=$1 for update`, h).Scan(&state, &tok, &cred)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", ErrNotFound
	}
	if err != nil {
		return "", "", "", err
	}
	if cred != nil {
		credentialID = *cred
	}
	if state != "issued" {
		return state, "", "", nil
	}
	if tok != nil {
		token = *tok
		if _, err := tx.Exec(ctx, `update launcher_credential_requests set token_once=null where pending_id_hash=$1`, h); err != nil {
			return "", "", "", err
		}
	}
	return state, token, credentialID, tx.Commit(ctx)
}

// Reconcile is the poller's own per-tick pass over every still-pending request: it expires
// whatever has outlived pendingTTL, then reads each remaining pending row's ask from Dispatch and
// applies it. A row Dispatch can't currently be read for (a transient error) is logged and left
// pending for the next tick, exactly like requests.Poller.RunOnce's own per-ask handling.
func (s *Service) Reconcile(ctx context.Context) error {
	if err := s.expirePending(ctx, time.Now()); err != nil {
		return err
	}
	rows, err := s.Store.Pool.Query(ctx, `select pending_id_hash, ask_id from launcher_credential_requests where state='pending'`)
	if err != nil {
		return err
	}
	type pendingRow struct {
		hash []byte
		ask  string
	}
	var all []pendingRow
	for rows.Next() {
		var p pendingRow
		if err := rows.Scan(&p.hash, &p.ask); err != nil {
			rows.Close()
			return err
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, p := range all {
		ask, err := s.Dispatch.GetAsk(ctx, p.ask)
		if err != nil {
			slog.Warn("launcher reconcile: read ask", "ask", p.ask, "error", err)
			continue
		}
		if err := s.applyAsk(ctx, p.hash, ask); err != nil {
			slog.Warn("launcher reconcile: apply ask", "ask", p.ask, "error", err)
		}
	}
	return s.retractExpiredAsks(ctx)
}

// retractExpiredAsks closes the Dispatch ask of every request that expired unanswered, so the
// operator is never left approving a login that can no longer be issued. No connection is held
// across a Dispatch call; an ask Dispatch cannot retract right now is tried again next pass.
func (s *Service) retractExpiredAsks(ctx context.Context) error {
	rows, err := s.Store.Pool.Query(ctx, `select pending_id_hash, ask_id from launcher_credential_requests where state='expired' and ask_retracted_at is null`)
	if err != nil {
		return err
	}
	type expiredAsk struct {
		hash []byte
		ask  string
	}
	var expired []expiredAsk
	for rows.Next() {
		var e expiredAsk
		if err := rows.Scan(&e.hash, &e.ask); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range expired {
		err := s.Dispatch.RetractAsk(ctx, e.ask, "the launcher credential request expired unanswered; approving it now would change nothing")
		if !dispatch.Retracted(err) {
			slog.Warn("launcher reconcile: retract ask", "ask", e.ask, "error", err)
			continue
		}
		if _, err := s.Store.Pool.Exec(ctx, `update launcher_credential_requests set ask_retracted_at=now() where pending_id_hash=$1`, e.hash); err != nil {
			return err
		}
	}
	return nil
}

// applyAsk moves one pending row on an ask read, exactly like requests.Machine.ApplyAnswer: a
// single transaction re-reads and locks the row (it may already have been decided or expired by a
// concurrent pass), decides granted/denied from the ask, and — on approval — mints the credential
// (MintLauncherCredentialTx, joined to this same transaction so a crash between minting and
// flipping this row to "issued" rolls back both together instead of orphaning an unrecoverable
// credential) and records it, and its own audit row, in the same transaction that commits the
// state change.
func (s *Service) applyAsk(ctx context.Context, pendingHash []byte, ask dispatch.Ask) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var operator, host, storedAskID string
	var recordedEdited, service *string
	err = tx.QueryRow(ctx, `select operator, host, ask_id, ask_edited_at, service
		from launcher_credential_requests where pending_id_hash=$1 and state='pending' for update`, pendingHash).
		Scan(&operator, &host, &storedAskID, &recordedEdited, &service)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already decided (or expired) since Reconcile listed it
	}
	if err != nil {
		return err
	}
	if service != nil && *service == "" {
		// Defense in depth: Request is the only writer of this column and already normalizes an
		// empty string to nil before it ever reaches storage, but a read-back guard costs nothing
		// and keeps this decision from silently depending on that alone.
		service = nil
	}
	decision, denyReason, err := dispatch.Verdict(ask, storedAskID, recordedEdited, operator)
	if err != nil || decision == dispatch.Undecided {
		return err
	}
	approved := decision == dispatch.Approved
	by, actor := "", "broker"
	if ask.Answer != nil {
		by = dispatch.CanonicalLogin(ask.Answer.User)
		actor = "human:" + by
	}
	if !approved {
		if _, err := tx.Exec(ctx, `update launcher_credential_requests set state='denied' where pending_id_hash=$1 and state='pending'`, pendingHash); err != nil {
			return err
		}
		if err := auditLauncher(ctx, tx, "launcher_request.denied", actor, auditDetail(map[string]any{
			"operator": operator, "host": host, "ask_id": storedAskID, "reason": denyReason,
		})); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var operatorArg, serviceArg *string
	if service != nil {
		serviceArg = service
	} else {
		operatorArg = &operator
	}
	id, token, err := s.Enroll.MintLauncherCredentialTx(ctx, tx, operatorArg, serviceArg, host, storedAskID)
	if err != nil {
		return fmt.Errorf("mint launcher credential: %w", err)
	}
	if _, err := tx.Exec(ctx, `update launcher_credential_requests set state='issued', credential_id=$2, token_once=$3
		where pending_id_hash=$1 and state='pending'`, pendingHash, id, token); err != nil {
		return err
	}
	if err := auditLauncher(ctx, tx, "launcher_request.issued", actor, auditDetail(map[string]any{
		"operator": operator, "service": service, "host": host, "ask_id": storedAskID, "credential_id": id.String(),
	})); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// expirePending expires every pending row past its expires_at and writes one audit row per
// expired request, all in one transaction (the same shape as requests.Machine.ExpirePending).
// The expired detail carries ask_id and service alongside operator/host — the same level of
// detail created/issued/denied rows carry — so two pending requests for the same operator+host
// expiring together produce distinguishable audit rows.
func (s *Service) expirePending(ctx context.Context, now time.Time) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `update launcher_credential_requests set state='expired'
		where state='pending' and expires_at < $1 returning operator, host, ask_id, service`, now)
	if err != nil {
		return err
	}
	type expiredRow struct {
		operator, host, askID string
		service               *string
	}
	var expired []expiredRow
	for rows.Next() {
		var r expiredRow
		if err := rows.Scan(&r.operator, &r.host, &r.askID, &r.service); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range expired {
		if r.service != nil && *r.service == "" {
			r.service = nil
		}
		if err := auditLauncher(ctx, tx, "launcher_request.expired", "launcher:"+r.host, auditDetail(map[string]any{
			"operator": r.operator, "host": r.host, "ask_id": r.askID, "service": r.service,
		})); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Standing finds operator's standing secrets issue (title "Secret requests: <login>", labeled
// agent-secrets) in s.Project, creating it once when it doesn't exist yet. requests.Machine's
// StandingIssue field is wired to this method.
//
// It touches no Postgres connection: every step is a Dispatch call, and a connection held across
// one (as a lock would be) lets a flood of slow calls on the unauthenticated launcher route drain
// the broker's whole pool. Dispatch's issue creation has no unique constraint on (project, title,
// label), so concurrent callers for the same never-before-seen operator could each see zero
// matches and each create one; s.standing collapses concurrent callers for one operator in this
// process into one list-then-create. The shared call is detached from the first caller's
// cancellation, so one caller going away cannot fail the others; the Dispatch client's own
// timeout bounds it.
func (s *Service) Standing(ctx context.Context, operator string) (string, error) {
	operator = dispatch.CanonicalLogin(operator)
	shared := context.WithoutCancel(ctx)
	key, err, _ := s.standing.Do(s.Project+"\x00"+operator, func() (any, error) {
		title := "Secret requests: " + operator
		issues, err := s.Dispatch.ListIssues(shared, s.Project, standingLabel)
		if err != nil {
			return "", err
		}
		for _, issue := range issues {
			if issue.Title == title {
				return issue.Key, nil
			}
		}
		return s.Dispatch.CreateIssue(shared, s.Project, title, &operator, []string{standingLabel})
	})
	if err != nil {
		return "", err
	}
	return key.(string), nil
}

func launcherQuestion(host string, service *string, code string) string {
	subject := "a launcher credential for host " + host
	if service != nil && *service != "" {
		subject = fmt.Sprintf("a launcher credential for service %q on host %s", *service, host)
	}
	return fmt.Sprintf("Issue %s? Approve only if you started this login yourself and your terminal shows confirmation code %s: anyone can open a request that reads like this one.", subject, code)
}

// confirmationAlphabet has 32 symbols, none easily confused with another (no 0/O, no 1/I), so a
// random byte maps onto it without bias.
const confirmationAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// confirmationCode is eight random symbols from confirmationAlphabet as XXXX-XXXX.
func confirmationCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := make([]byte, 0, 9)
	for i, b := range raw {
		if i == 4 {
			code = append(code, '-')
		}
		code = append(code, confirmationAlphabet[int(b)%len(confirmationAlphabet)])
	}
	return string(code), nil
}

func hashPendingID(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// auditLauncher writes one audit row for a launcher-credential-request state change. This table's
// enrollment_id/request_id/grant_id columns are all nullable and address rows in the enroll and
// requests packages' own uuid-keyed tables, which this bytea-keyed table has no matching row for,
// so all three are left null here; every identifying fact instead rides in detail, which must
// never carry a bearer token or a raw (unhashed) pending id.
func auditLauncher(ctx context.Context, tx pgx.Tx, kind, actor, detail string) error {
	_, err := tx.Exec(ctx, `insert into audit (kind, actor, detail) values ($1,$2,$3::jsonb)`, kind, actor, detail)
	return err
}

func auditDetail(fields map[string]any) string {
	b, err := json.Marshal(fields)
	if err != nil {
		// fields is always built from this package's own string/*string values, so Marshal
		// cannot fail in practice; fall back to an empty object rather than losing the whole
		// audit row over an unmarshalable field a future change might add.
		return "{}"
	}
	return string(b)
}
