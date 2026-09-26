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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

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

// dispatchClient is the subset of *dispatch.Client this package needs: opening and reading the
// standing-issue ask (the same askOpener/askReader seam requests.Machine and requests.Poller use),
// plus the two issue routes that find or create an operator's standing issue. A small interface
// keeps launcher_test.go's fake self-contained instead of standing up an httptest server.
type dispatchClient interface {
	CreateAsk(ctx context.Context, issue, question string, options []dispatch.Option, urgency string) (dispatch.Ask, error)
	GetAsk(ctx context.Context, id string) (dispatch.Ask, error)
	ListIssues(ctx context.Context, project, label string) ([]dispatch.IssueSummary, error)
	CreateIssue(ctx context.Context, project, title string, assignee *string, labels []string) (string, error)
}

type Service struct {
	Store    *store.Store
	Dispatch dispatchClient
	Enroll   *enroll.Service
	Project  string
}

// Request opens a launcher-credential request: it finds or creates the operator's standing issue
// (Standing), opens an Approve/Deny ask on it naming the host (and, for a service credential, the
// service), and records a pending row keyed by the sha256 of a freshly minted opaque pending id —
// the same "never store the bearer capability itself" shape enroll.Service uses for tokens. The
// pending id is returned to the caller (the launcher CLI), which polls Read with it; nothing about
// it identifies the eventual credential, so leaking a pending id before it is issued reveals
// nothing but "someone can watch whether this specific request gets approved".
//
// service, when non-nil, requests a service credential (like the Legion daemon's shared
// enrollment authority) rather than a personal one: the ask still opens on operator's own standing
// issue (operator is who approves it), but the credential minted on approval carries service with
// a nil operator instead of the reverse. The schema this table shares with enroll.Service has no
// column for that in-flight fact, so it rides in token_once — otherwise unused while the row is
// pending — until Reconcile overwrites it with the real one-time token at the moment of minting;
// Read never exposes it early because its own clearing update only ever fires once state is
// "issued" (see Read).
func (s *Service) Request(ctx context.Context, operator, host string, service *string) (string, error) {
	issueKey, err := s.Standing(ctx, operator)
	if err != nil {
		return "", fmt.Errorf("find standing issue: %w", err)
	}
	ask, err := s.Dispatch.CreateAsk(ctx, issueKey, launcherQuestion(host, service), []dispatch.Option{
		{Label: "Approve", Description: "Issue this launcher credential."},
		{Label: "Deny", Description: "Refuse; the launcher is told the request was denied."},
	}, "med")
	if err != nil {
		return "", fmt.Errorf("open Dispatch ask: %w", err)
	}
	pendingID, err := randomToken()
	if err != nil {
		return "", err
	}
	var serviceScratch *string
	if service != nil && *service != "" {
		serviceScratch = service
	}
	_, err = s.Store.Pool.Exec(ctx, `insert into launcher_credential_requests
		(pending_id_hash, operator, host, ask_id, ask_edited_at, state, token_once, expires_at)
		values ($1,$2,$3,$4,$5,'pending',$6,$7)`,
		hashPendingID(pendingID), operator, host, ask.ID, ask.EditedAt, serviceScratch, time.Now().Add(pendingTTL))
	if err != nil {
		return "", err
	}
	return pendingID, nil
}

// Read answers the launcher's poll: "pending", "denied", "expired", or "issued" with the raw
// one-time token exactly once. It locks the row for update and clears token_once only after
// reading it back, and only when state is "issued" (a pending, denied, or expired row is never
// mutated here, so a Read while pending never disturbs Request's service scratch value sitting in
// token_once); a Read that observes "issued" a second time, after the first clear committed,
// therefore finds token_once already null.
func (s *Service) Read(ctx context.Context, pendingID string) (state, token string, err error) {
	h := hashPendingID(pendingID)
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	var tok *string
	err = tx.QueryRow(ctx, `select state, token_once from launcher_credential_requests where pending_id_hash=$1 for update`, h).Scan(&state, &tok)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if state != "issued" {
		return state, "", nil
	}
	if tok != nil {
		token = *tok
		if _, err := tx.Exec(ctx, `update launcher_credential_requests set token_once=null where pending_id_hash=$1`, h); err != nil {
			return "", "", err
		}
	}
	return state, token, tx.Commit(ctx)
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
	return nil
}

// applyAsk moves one pending row on an ask read, exactly like requests.Machine.ApplyAnswer: a
// single transaction re-reads and locks the row (it may already have been decided or expired by a
// concurrent pass), decides granted/denied from the ask, and — on approval — mints the credential
// and records it in the same update that flips state to "issued".
func (s *Service) applyAsk(ctx context.Context, pendingHash []byte, ask dispatch.Ask) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var operator, host, storedAskID string
	var recordedEdited, serviceScratch *string
	err = tx.QueryRow(ctx, `select operator, host, ask_id, ask_edited_at, token_once
		from launcher_credential_requests where pending_id_hash=$1 and state='pending' for update`, pendingHash).
		Scan(&operator, &host, &storedAskID, &recordedEdited, &serviceScratch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already decided (or expired) since Reconcile listed it
	}
	if err != nil {
		return err
	}
	if ask.State == "open" {
		return nil
	}
	approved := storedAskID == ask.ID &&
		ask.State == "answered" && ask.Answer != nil &&
		sameEdit(recordedEdited, ask.EditedAt) &&
		ask.Answer.User == operator &&
		len(ask.Answer.Selected) == 1 && ask.Answer.Selected[0] == "Approve"
	if !approved {
		if _, err := tx.Exec(ctx, `update launcher_credential_requests set state='denied' where pending_id_hash=$1 and state='pending'`, pendingHash); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var operatorArg, serviceArg *string
	if serviceScratch != nil {
		serviceArg = serviceScratch
	} else {
		operatorArg = &operator
	}
	id, token, err := s.Enroll.MintLauncherCredential(ctx, operatorArg, serviceArg, host, storedAskID)
	if err != nil {
		return fmt.Errorf("mint launcher credential: %w", err)
	}
	if _, err := tx.Exec(ctx, `update launcher_credential_requests set state='issued', credential_id=$2, token_once=$3
		where pending_id_hash=$1 and state='pending'`, pendingHash, id, token); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) expirePending(ctx context.Context, now time.Time) error {
	_, err := s.Store.Pool.Exec(ctx, `update launcher_credential_requests set state='expired'
		where state='pending' and expires_at < $1`, now)
	return err
}

// Standing finds operator's standing secrets issue (title "Secret requests: <login>", labeled
// agent-secrets) in s.Project, creating it once when it doesn't exist yet. It is a method value,
// not a struct field, so Task 14's requests.Machine.StandingIssue field (func(ctx, operator
// string) (string, error)) can be wired directly as ls.Standing.
func (s *Service) Standing(ctx context.Context, operator string) (string, error) {
	title := "Secret requests: " + operator
	issues, err := s.Dispatch.ListIssues(ctx, s.Project, standingLabel)
	if err != nil {
		return "", err
	}
	for _, issue := range issues {
		if issue.Title == title {
			return issue.Key, nil
		}
	}
	return s.Dispatch.CreateIssue(ctx, s.Project, title, &operator, []string{standingLabel})
}

func launcherQuestion(host string, service *string) string {
	if service != nil && *service != "" {
		return fmt.Sprintf("Issue a launcher credential for service %q on host %s?", *service, host)
	}
	return fmt.Sprintf("Issue a launcher credential for host %s?", host)
}

func sameEdit(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
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
