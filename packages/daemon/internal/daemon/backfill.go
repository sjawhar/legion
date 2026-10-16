package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// backfillRepositories fills every record of project written before the record carried its
// repository (record.Store.UnfilledIssues) with the repository Dispatch resolves the issue to now
// (its own, an ancestor's, or the project's default) and that repository's default branch as GitHub
// names it, read once per repository. One read of Dispatch's listing serves every record. A record
// Dispatch resolves to no repository, or does not list, refuses the boot, naming each one, before
// any is filled: the operator sets the project's default repository in Dispatch and restarts.
//
// The record keeps the spelling of the shared clone under stateDir that spells the repository
// case-insensitively, since the issue's workspace and its sessions were made under that spelling
// (a tmux daemon's; Kubernetes pods provision their own and leave none), and otherwise takes
// GitHub's. Two clones that spell one repository leave no spelling to keep, and refuse the boot,
// naming both.
func backfillRepositories(ctx context.Context, pool *pgxpool.Pool, records record.Store, client dispatch.Client, project, stateDir string, named *defaultBranches, log *slog.Logger) error {
	var unfilled []record.Issue
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		unfilled, err = records.UnfilledIssues(ctx, tx, project)
		return err
	}); err != nil {
		return fmt.Errorf("read the records with no repository: %w", err)
	}
	if len(unfilled) == 0 {
		return nil
	}
	listed, err := client.ListIssues(ctx, project, nil)
	if err != nil {
		return fmt.Errorf("list Dispatch issues to fill the records with no repository: %w", err)
	}
	resolved := make(map[string]string, len(listed))
	for _, summary := range listed {
		resolved[summary.Key] = summary.ResolvedRepo
	}
	var none []string
	for _, issue := range unfilled {
		if resolved[issue.Key] == "" {
			none = append(none, issue.Key)
		}
	}
	if len(none) > 0 {
		return fmt.Errorf("%d issue records have no repository and Dispatch resolves none for them (%s): set the project's default repository in Dispatch (Settings → Repositories → Projects) and restart",
			len(none), strings.Join(none, ", "))
	}
	clones, err := sharedClones(stateDir)
	if err != nil {
		return err
	}
	type filled struct{ key, repo, branch string }
	fills := make([]filled, 0, len(unfilled))
	for _, issue := range unfilled {
		repo, err := ghrepo.Parse("the repository Dispatch resolves "+issue.Key+" to", resolved[issue.Key])
		if err != nil {
			return err
		}
		read, err := named.Named(ctx, repo)
		if err != nil {
			return fmt.Errorf("fill the repository of %s: %w", issue.Key, err)
		}
		spelling := read.Name.String()
		var spelled []string
		for _, clone := range clones {
			if strings.EqualFold(clone.Repo.String(), repo.String()) {
				spelling = clone.Repo.String()
				spelled = append(spelled, clone.Clone)
			}
		}
		if len(spelled) > 1 {
			return fmt.Errorf("fill the repository of %s: two shared clones spell %s (%s); remove the one no issue works in and restart",
				issue.Key, repo, strings.Join(spelled, ", "))
		}
		fills = append(fills, filled{issue.Key, spelling, read.DefaultBranch})
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, fill := range fills {
			if err := records.FillIssueRepository(ctx, tx, fill.key, fill.repo, fill.branch); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, fill := range fills {
		log.Info("legion workflow backfill filled "+fill.key+" with "+fill.repo+"@"+fill.branch, "issue", fill.key, "repo", fill.repo, "defaultBranch", fill.branch)
	}
	return nil
}

// deref is a Dispatch parent pointer as a key, "" for none.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
