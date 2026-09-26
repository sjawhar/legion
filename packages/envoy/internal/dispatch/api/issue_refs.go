package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *server) resolveIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key, err := s.resolveIssueRef(r.Context(), r.URL.Query().Get("ref"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"key": key})
}

// resolveIssueRef resolves an existing native key or external GitHub link.
// Creating a missing external issue is the responsibility of POST /issues.
func (s *server) resolveIssueRef(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if issueKeyPattern.MatchString(ref) {
		var key string
		err := s.deps.Store.Pool.QueryRow(ctx, `select key from issues where key = $1`, ref).Scan(&key)
		if err != nil {
			return "", err
		}
		return key, nil
	}
	repo, number, err := externalRef(ref)
	if err != nil {
		return "", err
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select distinct issue_key from issue_external_links where url = any($1) order by issue_key
	`, externalURLs(repo, number))
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return "", err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(keys) {
	case 0:
		return "", pgx.ErrNoRows
	case 1:
		return keys[0], nil
	default:
		return "", errorf(
			http.StatusBadRequest,
			"AMBIGUOUS_ISSUE_REF",
			"external issue reference %q is linked to multiple Dispatch issues: %s",
			ref,
			strings.Join(keys, ", "),
		)
	}
}

func validateExternalURL(raw string) error {
	value, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || !value.IsAbs() || value.Host == "" || (value.Scheme != "http" && value.Scheme != "https") {
		return errorf(http.StatusBadRequest, "INVALID_URL", "external link URL must be an absolute HTTP(S) URL")
	}
	return nil
}
