package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/rank"
)

type rankInput struct {
	Before *string `json:"before"`
	After  *string `json:"after"`
}

func lockProjectRankAllocation(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('issue-rank:' || $1))`, project); err != nil {
		return fmt.Errorf("lock project rank allocation: %w", err)
	}
	return nil
}

func (s *server) rankForInput(ctx context.Context, q queryer, project, issueKey string, input rankInput) (string, error) {
	var (
		previous string
		next     string
		err      error
	)
	if input.After != nil {
		previous, err = rankBoundary(ctx, q, project, issueKey, *input.After)
		if err != nil {
			return "", err
		}
	}
	if input.Before != nil {
		next, err = rankBoundary(ctx, q, project, issueKey, *input.Before)
		if err != nil {
			return "", err
		}
	}
	if previous != "" && next != "" && previous >= next {
		return "", errorf(http.StatusBadRequest, "RANK_INPUT", "rank neighbors are not ordered")
	}
	if previous != "" {
		next, err = nextProjectRank(ctx, q, project, issueKey, previous, next)
	} else if next != "" {
		previous, err = previousProjectRank(ctx, q, project, issueKey, next)
	} else {
		previous, err = lastProjectRank(ctx, q, project, issueKey)
	}
	if err != nil {
		return "", err
	}
	return rank.Between(previous, next), nil
}

func nextProjectRank(ctx context.Context, q queryer, project, issueKey, previous, next string) (string, error) {
	var rank string
	err := q.QueryRow(ctx, `
		select rank from issues
		where project_key = $1 and key != $2 and rank > $3 and ($4 = '' or rank < $4)
		order by rank
		limit 1
	`, project, issueKey, previous, next).Scan(&rank)
	if errors.Is(err, pgx.ErrNoRows) {
		return next, nil
	}
	return rank, err
}

func previousProjectRank(ctx context.Context, q queryer, project, issueKey, next string) (string, error) {
	var rank string
	err := q.QueryRow(ctx, `
		select rank from issues
		where project_key = $1 and key != $2 and rank < $3
		order by rank desc
		limit 1
	`, project, issueKey, next).Scan(&rank)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return rank, err
}

func lastProjectRank(ctx context.Context, q queryer, project, issueKey string) (string, error) {
	var rank string
	err := q.QueryRow(ctx, `
		select rank from issues
		where project_key = $1 and key != $2
		order by rank desc
		limit 1
	`, project, issueKey).Scan(&rank)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return rank, err
}

func rankBoundary(ctx context.Context, q queryer, project, issueKey, raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" || key == issueKey {
		return "", errorf(http.StatusBadRequest, "RANK_INPUT", "rank neighbor must name another issue")
	}
	var (
		neighborProject string
		neighborRank    string
	)
	if err := q.QueryRow(ctx, `select project_key, rank from issues where key = $1 for update`, key).Scan(&neighborProject, &neighborRank); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errorf(http.StatusBadRequest, "RANK_INPUT", "rank neighbor does not exist")
		}
		return "", err
	}
	if neighborProject != project {
		return "", errorf(http.StatusBadRequest, "RANK_INPUT", "rank neighbor must be in the same project")
	}
	return neighborRank, nil
}
