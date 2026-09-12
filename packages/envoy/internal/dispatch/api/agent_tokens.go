package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
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
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}

	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select id::text, name, prefix, created_at, last_used_at, revoked_at
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
	writeJSON(w, http.StatusOK, tokens)
}

func (s *server) createAgentToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		Name string `json:"name"`
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

	plaintext, hash, err := newAgentToken()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	prefix := plaintext[len("dsp_") : len("dsp_")+agentTokenPrefixLength]
	var token model.AgentToken
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		insert into agent_tokens (owner, name, token_hash, prefix)
		values ($1, $2, $3, $4)
		returning id::text, name, prefix, created_at, last_used_at, revoked_at
	`, actor.ID, input.Name, hash[:], prefix).Scan(
		&token.ID, &token.Name, &token.Prefix, &token.CreatedAt, &token.LastUsedAt, &token.RevokedAt,
	); err != nil {
		s.writeHandlerError(w, fmt.Errorf("create agent token: %w", err))
		return
	}
	writeJSON(w, http.StatusCreated, createAgentTokenResponse{AgentToken: token, Token: plaintext})
}

func (s *server) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
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
	var owner string
	var revokedAt *time.Time
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select owner, revoked_at
		from agent_tokens
		where token_hash = $1
	`, hash[:]).Scan(&owner, &revokedAt)
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
	return model.Actor{Kind: "session", Owner: &owner}, nil
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
	if err := row.Scan(&token.ID, &token.Name, &token.Prefix, &token.CreatedAt, &token.LastUsedAt, &token.RevokedAt); err != nil {
		return model.AgentToken{}, fmt.Errorf("scan agent token: %w", err)
	}
	return token, nil
}
