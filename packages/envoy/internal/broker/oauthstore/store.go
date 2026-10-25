// Package oauthstore is the Postgres storage behind the broker's OAuth 2.0 provider
// (authelia.com/provider/oauth2), which runs a machine login as a device authorization grant
// (RFC 8628) with DPoP-bound (RFC 9449), rotating refresh tokens and an RFC 8693 token exchange
// minting Dispatch access tokens. It keeps the device codes, the access and refresh token
// sessions, DPoP proof replay records and nonces, the provider's HMAC secret, which identity
// holds a session name, and the exchange's replay records, in the tables migrations 0016 and
// 0018 create.
//
// A stored request is the JSON of its oauth2.Request fields plus its client's id; reading it back
// re-hydrates the client with GetClient and the session into the kind the caller hands it, an
// *oauth2.DefaultSession or the *ExchangeSession an exchanged token runs on. Every machine
// login's token row names the launcher credential its session's Extra["credential_id"] carries,
// so EndCredential ends a revoked credential's tokens in the revoke's own transaction. A token
// exchanged for a machine access token names the same credential, because the library reads the
// subject token's session into the session the new token is issued on; one exchanged for a pod's
// or a workflow run's token follows no login and its row names none.
//
// Every method runs in the transaction the context carries (WithTx, or BeginTX), and on the pool
// when it carries none.
package oauthstore

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"authelia.com/provider/oauth2"
	hoauth2 "authelia.com/provider/oauth2/handler/oauth2"
	"authelia.com/provider/oauth2/handler/rfc8628"
	"authelia.com/provider/oauth2/handler/rfc8693"
	"authelia.com/provider/oauth2/handler/rfc9449"
	"authelia.com/provider/oauth2/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

var (
	_ oauth2.ClientManager           = (*Store)(nil)
	_ rfc8628.Storage                = (*Store)(nil)
	_ hoauth2.CoreStorage            = (*Store)(nil)
	_ hoauth2.TokenRevocationStorage = (*Store)(nil)
	_ rfc9449.Storage                = (*Store)(nil)
	_ rfc8693.Storage                = (*Store)(nil)
	_ storage.Transactional          = (*Store)(nil)
)

// refreshGrace is how long a rotated refresh token stays redeemable, so a client whose refresh
// response was lost in transit can retry with the token it still holds (the helper retries every
// 15 s until an answer settles it). Its DPoP binding means only the holder of the machine's key
// can use it.
const refreshGrace = 5 * time.Minute

// sweepBatch bounds how many rows one of Sweep's statements deletes; a variable so tests can sweep
// a backlog of several batches.
var sweepBatch = 1000

// sweepMaxBatches bounds how many statements one Sweep runs per kind, so a backlog takes several
// sweeper ticks rather than one long one; a variable so tests can exceed it.
var sweepMaxBatches = 10

// sweepStatements are Sweep's statements for state deleted row by row, $1 the batch. Each
// selects its batch by primary key through 0016's or 0018's *_expires index on the predicate it
// sweeps by, oldest first, so a table of mostly live rows is never read whole.
var sweepStatements = []struct{ what, sql string }{
	{"device codes", `delete from oauth_device_codes where signature in (select signature from oauth_device_codes
		where expires_at < now() - interval '1 hour' order by expires_at limit $1)`},
	{"DPoP proofs", `delete from oauth_dpop_proofs where (jti, htu) in (select jti, htu from oauth_dpop_proofs
		where expires_at <= now() order by expires_at limit $1)`},
	{"DPoP nonces", `delete from oauth_dpop_nonces where nonce in (select nonce from oauth_dpop_nonces
		where expires_at <= now() order by expires_at limit $1)`},
	{"access tokens", `delete from oauth_access_tokens where signature in (select signature from oauth_access_tokens
		where expires_at <= now() order by expires_at limit $1)`},
	{"session bindings", `delete from oauth_session_bindings where session in (select session from oauth_session_bindings
		where expires_at <= now() order by expires_at limit $1)`},
	{"exchanged JWT ids", `delete from oauth_exchange_jtis where jti in (select jti from oauth_exchange_jtis
		where expires_at <= now() order by expires_at limit $1)`},
}

// sweepRefreshCandidates reads the next $1 refresh tokens after the cursor ($2, $3) in
// (expires_at, signature) order, through 0016's oauth_refresh_tokens_sweep index.
const sweepRefreshCandidates = `select signature, expires_at from oauth_refresh_tokens
	where (expires_at, signature) > ($2, $3) order by expires_at, signature limit $1`

// sweepRefreshDelete deletes those of the candidates $1 that are not redeemable on their own
// terms (inactive, expired, or past their grace) and whose family (rows sharing request_id) has
// no redeemable row: active, unexpired, and in its rotation's grace or never rotated.
const sweepRefreshDelete = `delete from oauth_refresh_tokens t where signature = any($1)
	and (not t.active or t.expires_at <= now() or t.grace_until < now())
	and not exists (select 1 from oauth_refresh_tokens r where r.request_id = t.request_id
		and r.active and r.expires_at > now() and (r.grace_until is null or r.grace_until >= now()))`

// sweepCursor is where the refresh sweep's walk over oauth_refresh_tokens stands: the last
// (expires_at, signature) it examined. Its zero value sorts before every row.
type sweepCursor struct {
	expiresAt time.Time
	signature string
}

// Sweep deletes spent OAuth state: device codes an hour past their expiry (so a helper still
// polling one is answered expired_token, not invalid_grant as for an unknown code), DPoP proofs
// and nonces past theirs, access tokens past theirs, lapsed session bindings and the two replay
// stores' remembered JWT ids past theirs, and every refresh token of a family (rows sharing
// request_id) none of whose rows is still redeemable (active, unexpired, and in its
// rotation's grace or never rotated). A family with a redeemable row keeps all its rows, so
// replaying one of its rotated tokens still finds it and revokes the family. Each kind is
// worked in batches of sweepBatch, each its own statements, until a batch comes up short or
// sweepMaxBatches have run; what is left waits for the next Sweep. Refresh tokens are walked
// with a cursor held on the Store: each batch examines the sweepBatch rows after the last one
// examined, deleted or kept, and a short batch returns the cursor to the start, so a live
// family's rows are read once per pass over the table rather than every tick.
func (s *Store) Sweep(ctx context.Context) error {
	db := s.db(ctx)
	for _, q := range sweepStatements {
		for range sweepMaxBatches {
			tag, err := db.Exec(ctx, q.sql, sweepBatch)
			if err != nil {
				return fmt.Errorf("sweep %s: %w", q.what, err)
			}
			if tag.RowsAffected() < int64(sweepBatch) {
				break
			}
		}
	}
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	for range sweepMaxBatches {
		rows, err := db.Query(ctx, sweepRefreshCandidates, sweepBatch, s.sweepAt.expiresAt, s.sweepAt.signature)
		if err != nil {
			return fmt.Errorf("sweep refresh tokens: %w", err)
		}
		var signatures []string
		next := s.sweepAt
		for rows.Next() {
			if err := rows.Scan(&next.signature, &next.expiresAt); err != nil {
				rows.Close()
				return fmt.Errorf("sweep refresh tokens: %w", err)
			}
			signatures = append(signatures, next.signature)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("sweep refresh tokens: %w", err)
		}
		if len(signatures) > 0 {
			if _, err := db.Exec(ctx, sweepRefreshDelete, signatures); err != nil {
				return fmt.Errorf("sweep refresh tokens: %w", err)
			}
		}
		if len(signatures) < sweepBatch {
			s.sweepAt = sweepCursor{}
			break
		}
		s.sweepAt = next
	}
	return nil
}

// deviceInvalidated is the oauth_device_codes status of a device code already exchanged for
// tokens; the library's own statuses (new, approved, denied) are 0 to 2.
const deviceInvalidated = -1

// Store is the provider's storage on a broker Postgres pool.
type Store struct {
	pool *pgxpool.Pool

	// Reused ends the launcher credential a reused refresh token's family names, in the
	// transaction the library's reuse response runs in (RevokeRefreshToken). It must be set before
	// the provider serves a refresh: enroll.Service.RevokeReusedCredential.
	Reused func(ctx context.Context, tx pgx.Tx, credentialID string) error

	// ExchangeSubject answers who a verified subject token is, for the token types whose subject
	// the library cannot read off the token itself (GetSubjectForTokenExchange). The store holds
	// no policy of its own, so the provider wiring sets this to the decision the exchange makes;
	// it must be set before the provider serves the token-exchange grant.
	ExchangeSubject func(ctx context.Context, request oauth2.Requester, subjectToken map[string]any) (string, error)

	// sweepMu serialises Sweep's refresh walk, which reads and advances sweepAt.
	sweepMu sync.Mutex
	sweepAt sweepCursor
}

// New returns the store on db, whose schema the broker's migrations have created.
func New(db *pgxpool.Pool) *Store {
	return &Store{pool: db}
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txKey struct{}

// txState is the transaction a context carries, and whether the BeginTX that put it there opened
// it (so only that BeginTX's Commit or Rollback ends it).
type txState struct {
	tx    pgx.Tx
	owned bool
}

// WithTx returns ctx carrying tx, so every store method called with it runs in tx and the
// library's own BeginTX/Commit/Rollback join tx without ending it: the caller commits or rolls
// back tx itself.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, txState{tx: tx})
}

// TxFrom is the transaction ctx carries (from WithTx or BeginTX), so a caller handed the
// provider's context writes its own rows in the same transaction as the provider's.
func TxFrom(ctx context.Context) (pgx.Tx, bool) {
	state, ok := ctx.Value(txKey{}).(txState)
	return state.tx, ok
}

func (s *Store) db(ctx context.Context) querier {
	if state, ok := ctx.Value(txKey{}).(txState); ok {
		return state.tx
	}
	return s.pool
}

// BeginTX opens a transaction and returns ctx carrying it. When ctx already carries one (from
// WithTx or an earlier BeginTX), the returned context joins it instead, and its Commit and Rollback
// leave that transaction to whoever opened it.
func (s *Store) BeginTX(ctx context.Context) (context.Context, error) {
	if state, ok := ctx.Value(txKey{}).(txState); ok {
		return context.WithValue(ctx, txKey{}, txState{tx: state.tx}), nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ctx, fmt.Errorf("begin OAuth storage transaction: %w", err)
	}
	return context.WithValue(ctx, txKey{}, txState{tx: tx, owned: true}), nil
}

// Commit commits the transaction ctx's BeginTX opened; for a transaction it joined it does nothing.
func (s *Store) Commit(ctx context.Context) error {
	state, err := txOf(ctx)
	if err != nil || !state.owned {
		return err
	}
	return state.tx.Commit(ctx)
}

// Rollback rolls back the transaction ctx's BeginTX opened; for a transaction it joined it does
// nothing.
func (s *Store) Rollback(ctx context.Context) error {
	state, err := txOf(ctx)
	if err != nil || !state.owned {
		return err
	}
	return state.tx.Rollback(ctx)
}

func txOf(ctx context.Context) (txState, error) {
	state, ok := ctx.Value(txKey{}).(txState)
	if !ok {
		return txState{}, errors.New("oauthstore: the context carries no transaction")
	}
	return state, nil
}

// storedRequest is a request's JSON in a session row: its oauth2.Request fields, its client's id
// (oauth2.Request does not encode its client), and, for a device code, the device fields the
// library reads back.
type storedRequest struct {
	ClientID string `json:"client_id"`
	*oauth2.Request
	Status      oauth2.DeviceAuthorizeStatus `json:"status,omitempty"`
	LastChecked time.Time                    `json:"last_checked,omitzero"`
}

// sessionOf is the *oauth2.DefaultSession at the heart of the request's session, which is the
// session itself for every grant but the exchange, and the one an *ExchangeSession carries the
// exchange's claims around. It is what the row's own columns (its expiry, its credential) are
// read from; the session written into the row's JSON is whichever of the two the request holds,
// so every field each kind carries is stored and read back.
func sessionOf(request oauth2.Requester) (*oauth2.DefaultSession, error) {
	switch session := request.GetSession().(type) {
	case *ExchangeSession:
		if session != nil && session.DefaultSession != nil {
			return session.DefaultSession, nil
		}
	case *oauth2.DefaultSession:
		if session != nil {
			return session, nil
		}
	}
	return nil, fmt.Errorf("oauthstore stores *oauth2.DefaultSession and *ExchangeSession sessions, got %T", request.GetSession())
}

// flatten is the request as a row's JSON, its session the one the request holds rather than the
// *oauth2.DefaultSession inside it, so an exchanged token's claims survive the round trip.
func flatten(request oauth2.Requester) (storedRequest, error) {
	client := request.GetClient()
	if client == nil {
		return storedRequest{}, errors.New("oauthstore: the request names no client")
	}
	return storedRequest{
		ClientID: client.GetID(),
		Request: &oauth2.Request{
			ID:                request.GetID(),
			RequestedAt:       request.GetRequestedAt(),
			RequestedScope:    request.GetRequestedScopes(),
			GrantedScope:      request.GetGrantedScopes(),
			Form:              request.GetRequestForm(),
			Session:           request.GetSession(),
			RequestedAudience: request.GetRequestedAudience(),
			GrantedAudience:   request.GetGrantedAudience(),
			RequestedResource: request.GetRequestedResource(),
			GrantedResource:   request.GetGrantedResource(),
		},
	}, nil
}

// hydrate decodes raw into stored.Request, which the caller points at the request to fill, with
// its session decoded into session (a new *oauth2.DefaultSession when nil, and otherwise either
// kind the store writes; any other type is an error) and its client re-hydrated by id. An
// *ExchangeSession is handed its *oauth2.DefaultSession first, so a row written from either kind
// decodes into it.
//
// A row naming a client the provider does not register is a row it cannot decode, and its error
// does not wrap GetClient's oauth2.ErrNotFound: the library reads that sentinel from a token read
// as "no such token", and an introspection would answer a stored token it failed to read as
// simply not live.
func (s *Store) hydrate(ctx context.Context, raw []byte, session oauth2.Session, stored *storedRequest) error {
	switch typed := session.(type) {
	case nil:
		session = &oauth2.DefaultSession{}
	case *oauth2.DefaultSession:
	case *ExchangeSession:
		if typed.DefaultSession == nil {
			typed.DefaultSession = &oauth2.DefaultSession{}
		}
	default:
		return fmt.Errorf("oauthstore reads *oauth2.DefaultSession and *ExchangeSession sessions, got %T", session)
	}
	stored.Session = session
	if err := json.Unmarshal(raw, stored); err != nil {
		return fmt.Errorf("decode stored OAuth request: %w", err)
	}
	client, err := s.GetClient(ctx, stored.ClientID)
	if err != nil {
		return fmt.Errorf("stored OAuth request names client %q, which the provider does not register (%v)", stored.ClientID, err)
	}
	stored.Client = client
	return nil
}

// credentialOf is the launcher credential id a token session's Extra names, and "" for a token
// that follows no machine login: a token exchanged for a pod's or a workflow run's token, or an
// asserted one, is issued against a subject token or an assertion, no credential was ever issued
// for it, and its row holds none, which is what 0016 leaves the column nullable for. A token
// exchanged for a machine access token carries that token's credential id over, so its row names
// the credential and a revoke ends it. A machine login's tokens are issued only after an
// approval, which names the credential, so a session of that client naming none is still an
// error: its row would hold no credential a revoke could end it by.
func credentialOf(request oauth2.Requester, session *oauth2.DefaultSession) (string, error) {
	if id, ok := session.Extra[oauthwire.ExtraCredentialID].(string); ok && id != "" {
		return id, nil
	}
	if client := request.GetClient(); client == nil || client.GetID() != oauthwire.ClientID {
		return "", nil
	}
	return "", fmt.Errorf("oauthstore: a machine login's token session has %s %#v, want a credential id", oauthwire.ExtraCredentialID, session.Extra[oauthwire.ExtraCredentialID])
}

// expiry is the session's expiry for kind; a token with none (zero) never expires.
func expiry(session *oauth2.DefaultSession, kind oauth2.TokenType) pgtype.Timestamptz {
	at := session.GetExpiresAt(kind)
	if at.IsZero() {
		return pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	}
	return pgtype.Timestamptz{Time: at, Valid: true}
}

// CreateDeviceCodeSession stores a new device code under its device and user code signatures.
func (s *Store) CreateDeviceCodeSession(ctx context.Context, signature string, request oauth2.DeviceAuthorizeRequester) error {
	raw, session, err := deviceJSON(request)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `insert into oauth_device_codes (signature, user_code_signature, request, status, expires_at)
		values ($1, $2, $3, $4, $5)`,
		signature, request.GetUserCodeSignature(), raw, int(request.GetStatus()), expiry(session, oauth2.DeviceCode))
	if err != nil {
		return fmt.Errorf("store device code: %w", err)
	}
	return nil
}

// UpdateDeviceCodeSession rewrites a stored device code's request (its status and last poll); an
// unknown signature changes nothing. A code already decided (approved or denied) or invalidated
// keeps its status and stored request, and only its last poll moves: the library records a
// pending poll by writing back the code it read before the poll's transaction, so that write can
// land after an approval committed, and it must not undo the approval.
func (s *Store) UpdateDeviceCodeSession(ctx context.Context, signature string, request oauth2.DeviceAuthorizeRequester) error {
	raw, session, err := deviceJSON(request)
	if err != nil {
		return err
	}
	lastChecked, err := json.Marshal(request.GetLastChecked())
	if err != nil {
		return fmt.Errorf("encode device code poll time: %w", err)
	}
	_, err = s.db(ctx).Exec(ctx, `update oauth_device_codes set
		user_code_signature = case when status = $6 then $2 else user_code_signature end,
		request = case when status = $6 then $3 else request || jsonb_build_object('last_checked', $7::jsonb) end,
		expires_at = case when status = $6 then $5 else expires_at end,
		status = case when status = $6 then $4 else status end
		where signature = $1`,
		signature, request.GetUserCodeSignature(), raw, int(request.GetStatus()), expiry(session, oauth2.DeviceCode),
		int(oauth2.DeviceAuthorizeStatusNew), lastChecked)
	if err != nil {
		return fmt.Errorf("update device code: %w", err)
	}
	return nil
}

func deviceJSON(request oauth2.DeviceAuthorizeRequester) ([]byte, *oauth2.DefaultSession, error) {
	session, err := sessionOf(request)
	if err != nil {
		return nil, nil, err
	}
	flat, err := flatten(request)
	if err != nil {
		return nil, nil, err
	}
	flat.Status, flat.LastChecked = request.GetStatus(), request.GetLastChecked()
	raw, err := json.Marshal(flat)
	if err != nil {
		return nil, nil, fmt.Errorf("encode device code request: %w", err)
	}
	return raw, session, nil
}

// GetDeviceCodeSession reads a device code by its device code signature.
func (s *Store) GetDeviceCodeSession(ctx context.Context, signature string, session oauth2.Session) (oauth2.DeviceAuthorizeRequester, error) {
	return s.getDevice(ctx, `where signature = $1`, signature, session)
}

// GetDeviceCodeSessionByUserCode reads a device code by its user code signature.
func (s *Store) GetDeviceCodeSessionByUserCode(ctx context.Context, signature string, session oauth2.Session) (oauth2.DeviceAuthorizeRequester, error) {
	return s.getDevice(ctx, `where user_code_signature = $1`, signature, session)
}

// getDevice reads the device code the where clause selects: oauth2.ErrNotFound for none, and for
// one already exchanged. (The library's device token handler drops the requester the storage
// contract pairs with oauth2.ErrInvalidatedDeviceCode and answers that pairing 500; ErrNotFound is
// invalid_grant at the token endpoint and at the user code's approval alike.)
func (s *Store) getDevice(ctx context.Context, where, signature string, session oauth2.Session) (oauth2.DeviceAuthorizeRequester, error) {
	var (
		deviceSignature, userSignature string
		raw                            []byte
		status                         int
	)
	err := s.db(ctx).QueryRow(ctx, `select signature, user_code_signature, request, status from oauth_device_codes `+where, signature).
		Scan(&deviceSignature, &userSignature, &raw, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, oauth2.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read device code: %w", err)
	}
	request := oauth2.NewDeviceAuthorizeRequest()
	stored := storedRequest{Request: &request.Request}
	if err := s.hydrate(ctx, raw, session, &stored); err != nil {
		return nil, err
	}
	request.DeviceCodeSignature = deviceSignature
	request.UserCodeSignature = userSignature
	request.Status = stored.Status
	request.LastChecked = stored.LastChecked
	if status == deviceInvalidated {
		return nil, oauth2.ErrNotFound
	}
	return request, nil
}

// InvalidateDeviceCodeSession marks a device code exchanged. Only one exchange wins: a code already
// invalidated answers oauth2.ErrInvalidatedDeviceCode, so a second token request racing the first
// is refused.
func (s *Store) InvalidateDeviceCodeSession(ctx context.Context, signature string) error {
	db := s.db(ctx)
	tag, err := db.Exec(ctx, `update oauth_device_codes set status = $2 where signature = $1 and status <> $2`, signature, deviceInvalidated)
	if err != nil {
		return fmt.Errorf("invalidate device code: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := db.QueryRow(ctx, `select exists (select 1 from oauth_device_codes where signature = $1)`, signature).Scan(&exists); err != nil {
		return fmt.Errorf("invalidate device code: %w", err)
	}
	if exists {
		return oauth2.ErrInvalidatedDeviceCode
	}
	return oauth2.ErrNotFound
}

// CreateAuthorizeCodeSession is refused: the provider runs only the device and refresh grants,
// which never issue an authorization code.
func (s *Store) CreateAuthorizeCodeSession(context.Context, string, oauth2.Requester) error {
	return oauth2.ErrInvalidRequest.WithHint("This provider issues no authorization codes.")
}

// GetAuthorizeCodeSession is oauth2.ErrNotFound: no authorization code is ever stored.
func (s *Store) GetAuthorizeCodeSession(context.Context, string, oauth2.Session) (oauth2.Requester, error) {
	return nil, oauth2.ErrNotFound
}

// InvalidateAuthorizeCodeSession is oauth2.ErrNotFound: no authorization code is ever stored.
func (s *Store) InvalidateAuthorizeCodeSession(context.Context, string) error {
	return oauth2.ErrNotFound
}

// sessionJSON is the row a token session is stored as: its JSON, the *oauth2.DefaultSession its
// expiry is read from, and the launcher credential its row names, which is nil for a token that
// follows no machine login.
func sessionJSON(request oauth2.Requester) ([]byte, *oauth2.DefaultSession, any, error) {
	session, err := sessionOf(request)
	if err != nil {
		return nil, nil, nil, err
	}
	credentialID, err := credentialOf(request, session)
	if err != nil {
		return nil, nil, nil, err
	}
	var credential any
	if credentialID != "" {
		credential = credentialID
	}
	flat, err := flatten(request)
	if err != nil {
		return nil, nil, nil, err
	}
	raw, err := json.Marshal(flat)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode token request: %w", err)
	}
	return raw, session, credential, nil
}

// CreateAccessTokenSession stores an access token's session.
func (s *Store) CreateAccessTokenSession(ctx context.Context, signature string, request oauth2.Requester) error {
	raw, session, credential, err := sessionJSON(request)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `insert into oauth_access_tokens (signature, request_id, credential_id, request, expires_at)
		values ($1, $2, $3, $4, $5)`, signature, request.GetID(), credential, raw, expiry(session, oauth2.AccessToken))
	if err != nil {
		return fmt.Errorf("store access token: %w", err)
	}
	return nil
}

// GetAccessTokenSession reads an access token's session: oauth2.ErrNotFound for none, and the
// request with oauth2.ErrInactiveToken once it has been revoked.
func (s *Store) GetAccessTokenSession(ctx context.Context, signature string, session oauth2.Session) (oauth2.Requester, error) {
	return s.getToken(ctx, `select request, active from oauth_access_tokens where signature = $1`, signature, session)
}

// DeleteAccessTokenSession removes an access token's session; an unknown signature is no error.
func (s *Store) DeleteAccessTokenSession(ctx context.Context, signature string) error {
	if _, err := s.db(ctx).Exec(ctx, `delete from oauth_access_tokens where signature = $1`, signature); err != nil {
		return fmt.Errorf("delete access token: %w", err)
	}
	return nil
}

// RevokeAccessToken deactivates every access token issued for the request.
func (s *Store) RevokeAccessToken(ctx context.Context, requestID string) error {
	if _, err := s.db(ctx).Exec(ctx, `update oauth_access_tokens set active = false where request_id = $1 and active`, requestID); err != nil {
		return fmt.Errorf("revoke access tokens: %w", err)
	}
	return nil
}

// CreateRefreshTokenSession stores a refresh token's session alongside the access token issued
// with it.
func (s *Store) CreateRefreshTokenSession(ctx context.Context, signature string, accessSignature string, request oauth2.Requester) error {
	raw, session, credential, err := sessionJSON(request)
	if err != nil {
		return err
	}
	var access any
	if accessSignature != "" {
		access = accessSignature
	}
	_, err = s.db(ctx).Exec(ctx, `insert into oauth_refresh_tokens (signature, request_id, access_signature, credential_id, request, expires_at)
		values ($1, $2, $3, $4, $5, $6)`, signature, request.GetID(), access, credential, raw, expiry(session, oauth2.RefreshToken))
	if err != nil {
		return fmt.Errorf("store refresh token: %w", err)
	}
	return nil
}

// GetRefreshTokenSession reads a refresh token's session: oauth2.ErrNotFound for none, and the
// request with oauth2.ErrInactiveToken once it is revoked or its rotation's grace has passed.
func (s *Store) GetRefreshTokenSession(ctx context.Context, signature string, session oauth2.Session) (oauth2.Requester, error) {
	return s.getToken(ctx, `select request, active and (grace_until is null or grace_until >= now())
		from oauth_refresh_tokens where signature = $1`, signature, session)
}

func (s *Store) getToken(ctx context.Context, query, signature string, session oauth2.Session) (oauth2.Requester, error) {
	var (
		raw    []byte
		active bool
	)
	err := s.db(ctx).QueryRow(ctx, query, signature).Scan(&raw, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, oauth2.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read token session: %w", err)
	}
	request := oauth2.NewRequest()
	if err := s.hydrate(ctx, raw, session, &storedRequest{Request: request}); err != nil {
		return nil, err
	}
	if !active {
		return request, oauth2.ErrInactiveToken
	}
	return request, nil
}

// DeleteRefreshTokenSession removes a refresh token's session; an unknown signature is no error.
func (s *Store) DeleteRefreshTokenSession(ctx context.Context, signature string) error {
	if _, err := s.db(ctx).Exec(ctx, `delete from oauth_refresh_tokens where signature = $1`, signature); err != nil {
		return fmt.Errorf("delete refresh token: %w", err)
	}
	return nil
}

// RotateRefreshToken starts the redeemed refresh token's grace (refreshGrace from its first
// rotation; a retry within it does not extend it) and revokes the request's access tokens. A
// token redeemed again within its grace (a client retrying a refresh whose answer it lost) ends
// every other token of its family first: the branch its earlier redemption started, so the family
// never has two live branches and a token of the ended one presented later is a reuse. The
// redeemed token's row lock, taken first, serializes two redemptions of it.
func (s *Store) RotateRefreshToken(ctx context.Context, requestID string, signature string) error {
	var again bool
	err := s.db(ctx).QueryRow(ctx, `select grace_until is not null from oauth_refresh_tokens where signature = $1 for update`, signature).Scan(&again)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("rotate refresh token: %w", err)
	}
	if again {
		if _, err := s.db(ctx).Exec(ctx, `update oauth_refresh_tokens set active = false where request_id = $1 and signature <> $2 and active`, requestID, signature); err != nil {
			return fmt.Errorf("end the earlier redemption's branch: %w", err)
		}
	}
	if _, err := s.db(ctx).Exec(ctx, `update oauth_refresh_tokens set grace_until = coalesce(grace_until, now() + make_interval(secs => $2))
		where signature = $1`, signature, refreshGrace.Seconds()); err != nil {
		return fmt.Errorf("rotate refresh token: %w", err)
	}
	return s.RevokeAccessToken(ctx, requestID)
}

// RevokeRefreshToken deactivates every refresh token of the request, the whole rotation family
// (which shares the request id), and ends the launcher credential the family names (Reused). The
// library calls it only when a refresh presents a rotated token past its grace (its reuse
// response, in its own transaction): this provider mounts no revocation endpoint and issues no
// authorization code. The library has already deleted the presented token's row in this
// transaction before it calls here, so Reused locks the credential's row after a token row, the
// reverse of the order a revoke, a retire (enroll.Service.RetireCredential, which locks the
// credential and then deletes its token rows) or a refresh takes them. A revoke, a retire, or a
// second redemption inside the grace, racing this can therefore deadlock with it: Postgres aborts
// one of them, which answers 500 and clears on a retry, and the credential ends either way.
func (s *Store) RevokeRefreshToken(ctx context.Context, requestID string) error {
	state, err := txOf(ctx)
	if err != nil {
		return err
	}
	if s.Reused == nil {
		return errors.New("oauthstore: a reused refresh token's credential has no Reused step to end it")
	}
	rows, err := state.tx.Query(ctx, `select distinct credential_id::text from oauth_refresh_tokens where request_id = $1 and credential_id is not null`, requestID)
	if err != nil {
		return fmt.Errorf("read a reused family's credential: %w", err)
	}
	credentials, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read a reused family's credential: %w", err)
	}
	for _, credentialID := range credentials {
		if err := s.Reused(ctx, state.tx, credentialID); err != nil {
			return fmt.Errorf("end a reused family's credential: %w", err)
		}
	}
	if _, err := state.tx.Exec(ctx, `update oauth_refresh_tokens set active = false where request_id = $1 and active`, requestID); err != nil {
		return fmt.Errorf("revoke refresh tokens: %w", err)
	}
	return nil
}

// ErrCredentialEnded is a launcher credential LockCredential found revoked, past its expiry, or
// not there at all: there is no login left for a token to follow.
var ErrCredentialEnded = errors.New("oauthstore: the launcher credential is revoked or past its expiry")

// LockCredential holds launcher credential id's row lock until the transaction ctx carries ends,
// leaving the row as it is, and reports ErrCredentialEnded for a credential revoked or past its
// expiry. Expiry is compared with clock_timestamp(), the statement's own moment, not now(), the
// transaction's start, so a retire that commits after the token request's transaction began still
// ends the login for it.
//
// It is what keeps a token issued for the credential from stepping over its revoke: the revoke
// (enroll.Service.RevokeCredential) locks the same row before EndCredential, so it either commits
// first, and the lock finds no live row, or waits for the token to commit and then ends it. An
// exchange of a machine access token takes it, because its minted token names the same
// credential; the device code grant takes the same lock in machine.Service.Live.
func LockCredential(ctx context.Context, credentialID string) error {
	state, err := txOf(ctx)
	if err != nil {
		return err
	}
	var one int
	err = state.tx.QueryRow(ctx, `select 1 from launcher_credentials
		where id = $1 and revoked_at is null and expires_at > clock_timestamp() for no key update`, credentialID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCredentialEnded
	}
	if err != nil {
		return fmt.Errorf("lock the launcher credential: %w", err)
	}
	return nil
}

// EndCredential deactivates, in tx, every access and refresh token whose session names the
// launcher credential, so revoking the credential also ends the machine login's tokens.
func EndCredential(ctx context.Context, tx pgx.Tx, credentialID string) error {
	if _, err := tx.Exec(ctx, `update oauth_refresh_tokens set active = false where credential_id = $1 and active`, credentialID); err != nil {
		return fmt.Errorf("end the credential's refresh tokens: %w", err)
	}
	if _, err := tx.Exec(ctx, `update oauth_access_tokens set active = false where credential_id = $1 and active`, credentialID); err != nil {
		return fmt.Errorf("end the credential's access tokens: %w", err)
	}
	return nil
}

// DeleteCredentialTokens deletes, in tx, every access and refresh token whose session names the
// launcher credential, for a login retired rather than revoked. A deleted refresh token presented
// later is unknown to the provider, a plain invalid_grant, where an inactive one would take the
// library's reuse path and revoke the credential, ending the sessions the retire keeps.
func DeleteCredentialTokens(ctx context.Context, tx pgx.Tx, credentialID string) error {
	if _, err := tx.Exec(ctx, `delete from oauth_refresh_tokens where credential_id = $1`, credentialID); err != nil {
		return fmt.Errorf("delete the credential's refresh tokens: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from oauth_access_tokens where credential_id = $1`, credentialID); err != nil {
		return fmt.Errorf("delete the credential's access tokens: %w", err)
	}
	return nil
}

// jtiRetentionMargin is how long past a DPoP proof's expiry its jti is remembered, mirroring the
// broker's other replay stores: exp is dated on the broker's clock and compared with the
// database's, so the margin covers skew between the two.
const jtiRetentionMargin = time.Minute

// CheckAndSetDPoPProofUsed records a DPoP proof as used until exp plus jtiRetentionMargin, keyed
// on its jti and target URI (htu), and reports whether it already was. The insert, or the takeover
// of an expired record of the same key, is one statement, so two requests presenting the same
// proof cannot both see it unused. Sweep deletes expired records.
func (s *Store) CheckAndSetDPoPProofUsed(ctx context.Context, jti, _, _, _, htu string, exp time.Time) (bool, error) {
	var recorded bool
	err := s.db(ctx).QueryRow(ctx, `insert into oauth_dpop_proofs (jti, htu, expires_at) values ($1, $2, $3)
		on conflict (jti, htu) do update set expires_at = excluded.expires_at
		where oauth_dpop_proofs.expires_at <= now()
		returning true`, jti, htu, exp.Add(jtiRetentionMargin)).Scan(&recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("record DPoP proof: %w", err)
	}
	return false, nil
}

// CreateDPoPNonce stores a nonce the provider issued, valid until exp; Sweep deletes expired ones.
func (s *Store) CreateDPoPNonce(ctx context.Context, nonce string, exp time.Time) error {
	if _, err := s.db(ctx).Exec(ctx, `insert into oauth_dpop_nonces (nonce, expires_at) values ($1, $2)
		on conflict (nonce) do update set expires_at = excluded.expires_at`, nonce, exp); err != nil {
		return fmt.Errorf("store DPoP nonce: %w", err)
	}
	return nil
}

// IsDPoPNonceValid reports whether the provider issued nonce and it has not expired.
func (s *Store) IsDPoPNonceValid(ctx context.Context, nonce string) (bool, error) {
	var valid bool
	err := s.db(ctx).QueryRow(ctx, `select expires_at > now() from oauth_dpop_nonces where nonce = $1`, nonce).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read DPoP nonce: %w", err)
	}
	return valid, nil
}

// Secret is the provider's 32-byte HMAC secret, which signs every code and token it issues. The
// first call creates it; every later call, from any broker replica, reads the same one.
func (s *Store) Secret(ctx context.Context) ([]byte, error) {
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("generate OAuth provider secret: %w", err)
	}
	db := s.db(ctx)
	if _, err := db.Exec(ctx, `insert into oauth_provider_secret (secret) values ($1) on conflict do nothing`, fresh); err != nil {
		return nil, fmt.Errorf("store OAuth provider secret: %w", err)
	}
	var secret []byte
	if err := db.QueryRow(ctx, `select secret from oauth_provider_secret`).Scan(&secret); err != nil {
		return nil, fmt.Errorf("read OAuth provider secret: %w", err)
	}
	return secret, nil
}
