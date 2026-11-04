package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const agentTokenPrefixLength = 8

type createAgentTokenResponse struct {
	model.AgentToken
	Token string `json:"token"`
}

func (s *server) listAgentTokens(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}

	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select id::text, name, prefix, scopes, created_at, last_used_at, revoked_at
		from agent_tokens
		where owner = $1
		order by created_at desc
	`, actor.ID)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list agent tokens: %w", err))
		return
	}
	defer rows.Close()

	tokens := []model.AgentToken{}
	for rows.Next() {
		token, err := scanAgentToken(rows)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, fmt.Errorf("iterate agent tokens: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, tokens)
}

// createAgentToken is POST /api/v1/me/agent-tokens. Its answer is the one time the token's
// plaintext is shown, so an Idempotency-Key never keeps it (keyedWrites, answerHoldsSecret): the
// insert commits in a transaction of its own, which marks the key committed, and a repeat under the
// key is 409 KEY_COMMITTED - the token was made and cannot be shown again - never a second token.
func (s *server) createAgentToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input struct {
		Name string `json:"name"`
		// Scopes is the set the new token holds. A body that names none at all leaves it to the
		// caller's own set, which is why the field is a pointer: an empty array is a token that
		// holds nothing, and that is a different request from one that asks for the default.
		Scopes *[]string `json:"scopes"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		writeError(w, "AGENT_TOKEN_INPUT", http.StatusBadRequest, "name is required")
		return
	}
	scopes, ok := tokenScopes(w, actor, input.Scopes)
	if !ok {
		return
	}

	plaintext, hash, err := newAgentToken()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	prefix := plaintext[len("dsp_") : len("dsp_")+agentTokenPrefixLength]
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	token, err := scanAgentToken(tx.QueryRow(r.Context(), `
		insert into agent_tokens (owner, name, token_hash, prefix, scopes)
		values ($1, $2, $3, $4, $5)
		returning id::text, name, prefix, scopes, created_at, last_used_at, revoked_at
	`, actor.ID, input.Name, hash[:], prefix, scopes))
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("create agent token: %w", err))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, fmt.Errorf("create agent token: %w", err))
		return
	}
	WriteJSON(w, http.StatusCreated, createAgentTokenResponse{AgentToken: token, Token: plaintext})
}

// updateAgentTokenScopes is PATCH /api/v1/me/agent-tokens/{id}: the whole set one of the
// caller's own tokens is to hold, with the mint's refusals. Its answer holds no plaintext — the
// token's secret was shown once, at the mint — so its key keeps that answer, which is why it is
// not in answerHoldsSecret (request_keys.go). The update runs in a transaction of the shared
// pool, as every keyed write does: the commit step marks the request's key committed with the
// write itself (markRequestKeyCommitted).
func (s *server) updateAgentTokenScopes(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "token") {
		return
	}
	var input struct {
		Scopes *[]string `json:"scopes"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Scopes == nil {
		writeError(w, codeTokenScopesInput, http.StatusBadRequest,
			"scopes is required: it is the whole set the token is to hold")
		return
	}
	scopes, ok := tokenScopes(w, actor, input.Scopes)
	if !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	token, err := scanAgentToken(tx.QueryRow(r.Context(), `
		update agent_tokens
		set scopes = $3
		where id = $1 and owner = $2
		returning id::text, name, prefix, scopes, created_at, last_used_at, revoked_at
	`, r.PathValue("id"), actor.ID, scopes))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("update agent token scopes: %w", err))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, fmt.Errorf("update agent token scopes: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, token)
}

// codeTokenScopesInput refuses a set no token may hold: a word that is no scope this deployment
// knows, or approve without person, which agent_tokens' own check refuses as well.
const codeTokenScopesInput = "TOKEN_SCOPES_INPUT"

// tokenScopes is the set a mint or an edit gives a token: the one the body names, or, where it
// names none, the set the caller's own credential holds. It writes its own refusal and answers
// false for a set the token may not hold (codeTokenScopesInput) and for one the caller does not
// hold itself (403 OAUTH_SCOPE_REQUIRED): a credential gives a token only what it holds, or a
// token narrowed to one scope would mint itself a wide one. A scope named twice is stored once.
func tokenScopes(w http.ResponseWriter, actor model.Actor, requested *[]string) ([]string, bool) {
	if requested == nil {
		return callerScopes(actor), true
	}
	// At most the two scopes this deployment knows, whatever the body holds: an unknown word is
	// refused and a repeat is folded, so a long array buys no room.
	scopes := make([]string, 0, len(model.Scopes))
	for _, scope := range *requested {
		if scope != model.ScopePerson && scope != model.ScopeApprove {
			writeError(w, codeTokenScopesInput, http.StatusBadRequest, fmt.Sprintf(
				"%q is no scope this deployment knows; the scopes are %s and %s",
				scope, model.ScopePerson, model.ScopeApprove))
			return nil, false
		}
		// Act is nil for a browser, whose person holds every scope, which is what Holds answers
		// for one; a token's act carries the set its own row names.
		if !actor.Act.Holds(scope) {
			refuseMissingScope(w, scope, fmt.Sprintf(
				"this request's credential does not hold the %s scope, so it cannot give it to a token", scope))
			return nil, false
		}
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	if slices.Contains(scopes, model.ScopeApprove) && !slices.Contains(scopes, model.ScopePerson) {
		writeError(w, codeTokenScopesInput, http.StatusBadRequest, fmt.Sprintf(
			"a token holding %s holds %s too: settling what a person settles needs a credential that acts as that person",
			model.ScopeApprove, model.ScopePerson))
		return nil, false
	}
	return scopes, true
}

// callerScopes is the set the caller's own credential holds: the scopes actingPerson copied onto
// Act for a token that acted for its person, and both scopes for a browser, whose cookie names
// no agent because the person themselves acted.
func callerScopes(actor model.Actor) []string {
	if actor.Act != nil {
		return actor.Act.Scopes
	}
	return model.Scopes
}

func (s *server) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "token") {
		return
	}

	result, err := s.deps.Store.Pool.Exec(r.Context(), `
		update agent_tokens
		set revoked_at = coalesce(revoked_at, now())
		where id = $1 and owner = $2
	`, r.PathValue("id"), actor.ID)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("revoke agent token: %w", err))
		return
	}
	if result.RowsAffected() == 0 {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) personalTokenActor(ctx context.Context, plaintext string) (model.Actor, error) {
	hash := sha256.Sum256([]byte(plaintext))
	var id, name, owner string
	var scopes []string
	var revokedAt *time.Time
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select id::text, name, owner, scopes, revoked_at
		from agent_tokens
		where token_hash = $1
	`, hash[:]).Scan(&id, &name, &owner, &scopes, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Actor{}, nil
	}
	if err != nil {
		return model.Actor{}, fmt.Errorf("look up agent token: %w", err)
	}
	if revokedAt != nil {
		return model.Actor{}, errorf(http.StatusUnauthorized, "TOKEN_REVOKED", "agent token has been revoked")
	}
	if _, err := s.deps.Store.Pool.Exec(ctx, `
		update agent_tokens
		set last_used_at = now()
		where token_hash = $1
		  and revoked_at is null
		  and (last_used_at is null or last_used_at < now() - interval '1 minute')
	`, hash[:]); err != nil {
		return model.Actor{}, fmt.Errorf("record agent token use: %w", err)
	}
	// The token's own row says what it may do, and it names the credential that acted: the
	// person's token, not the person at a browser.
	return model.Actor{
		Kind:       "session",
		Owner:      &owner,
		Credential: &model.Credential{Scopes: scopes, TokenID: id, TokenName: name},
	}, nil
}

func newAgentToken() (string, [sha256.Size]byte, error) {
	var randomBytes [32]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("generate agent token: %w", err)
	}
	plaintext := "dsp_" + base64.RawURLEncoding.EncodeToString(randomBytes[:])
	return plaintext, sha256.Sum256([]byte(plaintext)), nil
}

func scanAgentToken(row rowScanner) (model.AgentToken, error) {
	var token model.AgentToken
	if err := row.Scan(&token.ID, &token.Name, &token.Prefix, &token.Scopes, &token.CreatedAt, &token.LastUsedAt, &token.RevokedAt); err != nil {
		return model.AgentToken{}, fmt.Errorf("scan agent token: %w", err)
	}
	return token, nil
}
