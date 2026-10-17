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

// backfillRepositories brings every record of project to carry its repository, its default branch
// and GitHub's spelling of the repository before the boot subscribes to any repository's events,
// reading GitHub and Dispatch outside every fact transaction: fillRepositories fills the records
// written before the record carried its repository, and nameRepositories the records written
// before it carried GitHub's spelling.
func backfillRepositories(ctx context.Context, pool *pgxpool.Pool, records record.Store, client dispatch.Client, project, stateDir string, named *defaultBranches, log *slog.Logger) error {
	if err := fillRepositories(ctx, pool, records, client, project, stateDir, named, log); err != nil {
		return err
	}
	return nameRepositories(ctx, pool, records, project, named, log)
}

// fillRepositories fills every record of project written before the record carried its
// repository (record.Store.UnfilledIssues) with the repository Dispatch resolves the issue to now
// (its own, an ancestor's, or the project's default), that repository's default branch and GitHub's
// spelling of it, both from one read of GitHub per repository. One read of Dispatch's listing
// serves every record. A record Dispatch resolves to no repository, or does not list, refuses the
// boot, naming each one, before any is filled: the operator sets the project's default repository
// in Dispatch and restarts.
//
// The record keeps the spelling of the shared clone under stateDir that spells the repository
// case-insensitively, since the issue's workspace and its sessions were made under that spelling
// (a tmux daemon's; Kubernetes pods provision their own and leave none), and otherwise takes
// GitHub's. Two clones that spell one repository leave no spelling to keep, and refuse the boot,
// naming both. GitHub's own spelling is kept beside it either way.
func fillRepositories(ctx context.Context, pool *pgxpool.Pool, records record.Store, client dispatch.Client, project, stateDir string, named *defaultBranches, log *slog.Logger) error {
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
	type filled struct{ key, repo, branch, github string }
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
		fills = append(fills, filled{issue.Key, spelling, read.DefaultBranch, read.Name.String()})
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, fill := range fills {
			if err := records.FillIssueRepository(ctx, tx, fill.key, fill.repo, fill.branch, fill.github); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, fill := range fills {
		log.Info("legion workflow backfill filled "+fill.key+" with "+fill.repo+"@"+fill.branch, "issue", fill.key, "repo", fill.repo,
			"defaultBranch", fill.branch, "githubRepo", fill.github)
	}
	return nil
}

// nameRepositories names every record of project that carries its repository but was written
// before the record carried GitHub's spelling of it (record.Store.UnnamedIssues) with that
// spelling, read from GitHub once per repository: SQL cannot know it, since a record may keep a
// clone's spelling that differs from GitHub's in case. A read GitHub refuses refuses the boot,
// naming the record, so the boot never subscribes to a repository in a spelling its events are not
// published under.
func nameRepositories(ctx context.Context, pool *pgxpool.Pool, records record.Store, project string, named *defaultBranches, log *slog.Logger) error {
	var unnamed []record.Issue
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		unnamed, err = records.UnnamedIssues(ctx, tx, project)
		return err
	}); err != nil {
		return fmt.Errorf("read the records with no GitHub spelling of their repository: %w", err)
	}
	names := make(map[string]string, len(unnamed))
	for _, issue := range unnamed {
		repo, err := ghrepo.Parse("the repository of "+issue.Key, issue.Repo)
		if err != nil {
			return err
		}
		read, err := named.Named(ctx, repo)
		if err != nil {
			return fmt.Errorf("name the repository of %s: %w", issue.Key, err)
		}
		names[issue.Key] = read.Name.String()
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, issue := range unnamed {
			if err := records.NameIssueRepository(ctx, tx, issue.Key, names[issue.Key]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, issue := range unnamed {
		log.Info("legion workflow backfill named "+issue.Key+"'s repository "+issue.Repo+" as GitHub spells it, "+names[issue.Key],
			"issue", issue.Key, "repo", issue.Repo, "githubRepo", names[issue.Key])
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
