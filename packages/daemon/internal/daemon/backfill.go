package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
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
func backfillRepositories(ctx context.Context, pool *pgxpool.Pool, records record.Store, client dispatch.Client, project, stateDir string, named *defaultBranches, renames ghrepo.Renames, log *slog.Logger) error {
	if err := fillRepositories(ctx, pool, records, client, project, stateDir, named, renames, log); err != nil {
		return err
	}
	return nameRepositories(ctx, pool, records, project, named, renames, log)
}

// fillRepositories fills every record of project written before the record carried its
// repository (record.Store.UnfilledIssues) with the repository Dispatch resolves the issue to now
// (its own, an ancestor's, or the project's default), that repository's default branch and GitHub's
// spelling of it, both from one read of GitHub per repository. One read of Dispatch's listing
// serves every record. A record Dispatch resolves to no repository, or does not list, refuses the
// boot, naming each one, before any is filled: the operator sets the project's default repository
// in Dispatch and restarts. So does a record whose pull request, or failing one, whose workspace (a
// tmux daemon's, issueWorkspaces), is in another repository than the one Dispatch resolves, as
// GitHub names either: it names the repository the issue worked in, and a record on another would
// create its branch there, disarm a stranger's pull request, and strand the workspace's unpushed
// work; the operator sets the issue's repository in Dispatch to that one.
//
// The record keeps the spelling of the shared clone under stateDir that spells the repository
// case-insensitively, since the issue's workspace and its sessions were made under that spelling
// (a tmux daemon's; Kubernetes pods provision their own and leave none), and otherwise takes
// GitHub's. Two clones that spell one repository leave no spelling to keep, and refuse the boot,
// naming both. GitHub's own spelling is kept beside it either way. A repository GitHub renamed,
// which Dispatch still resolves under its old name, takes GitHub's new name, unless a shared
// clone keeps the old one: that refuses the boot, naming the clone, since the record's spelling
// and GitHub's would name two repositories; the operator maps the new name in Dispatch or removes
// the clone. A rename the deployment configures (renames) names one repository, so a transfer the
// pair already covers refuses no boot: the record is filled with the name its clone keeps and
// with the pair's spelling of that same name as GitHub's spelling, never with the name the
// repository moved to, since a record's two repository columns must name one repository
// (record.Store.FillIssueRepository). The record keeps the name it was recorded under for its
// whole life, and heardRepositories hears the pair's other name too, so the events GitHub now
// publishes under the name it moved to still reach it.
func fillRepositories(ctx context.Context, pool *pgxpool.Pool, records record.Store, client dispatch.Client, project, stateDir string, named *defaultBranches, renames ghrepo.Renames, log *slog.Logger) error {
	var unfilled []record.Issue
	pulls := map[string]record.PullRequest{}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if unfilled, err = records.UnfilledIssues(ctx, tx, project); err != nil {
			return err
		}
		for _, issue := range unfilled {
			pull, err := records.PullRequest(ctx, tx, issue.Key)
			if err != nil {
				return err
			}
			if pull != nil {
				pulls[issue.Key] = *pull
			}
		}
		return nil
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
	var elsewhere []string
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
		if !renames.Same(spelling, read.Name.String()) {
			return fmt.Errorf("fill the repository of %s: GitHub renamed %s to %s, and the shared clone %s still has the old name: map %s in Dispatch (Settings → Repositories → Projects), or remove that clone if no issue works in it, and restart",
				issue.Key, repo, read.Name, spelled[0], read.Name)
		}
		if pull, ok := pulls[issue.Key]; ok && !renames.Same(pull.Repo, repo.String()) && !renames.Same(pull.Repo, read.Name.String()) {
			elsewhere = append(elsewhere, fmt.Sprintf("%s: Dispatch resolves it to %s, but its pull request #%d is on %s: set the issue's repository in Dispatch to %s (dispatch issue-update --issue %s --repo %s)",
				issue.Key, repo, pull.Number, pull.Repo, pull.Repo, issue.Key, pull.Repo))
			continue
		}
		workspaces, err := issueWorkspaces(stateDir, issue.Key)
		if err != nil {
			return fmt.Errorf("fill the repository of %s: %w", issue.Key, err)
		}
		if dir, in, ok := workspaceElsewhere(workspaces, repo, read.Name, renames); ok {
			elsewhere = append(elsewhere, fmt.Sprintf("%s: Dispatch resolves it to %s, but its workspace %s is in %s: set the issue's repository in Dispatch to %s (dispatch issue-update --issue %s --repo %s)",
				issue.Key, repo, dir, in, in, issue.Key, in))
			continue
		}
		fills = append(fills, filled{issue.Key, spelling, read.DefaultBranch, recordedSpelling(renames, spelling, read.Name)})
	}
	if len(elsewhere) > 0 {
		return fmt.Errorf("fill the repository of %s and restart", strings.Join(elsewhere, "; "))
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

// issueWorkspaces is every workspace a tmux daemon provisioned for issue under stateDir, by the
// repository its path names (workspace.Location: <state>/workspaces/<owner>/<name>/<issue>).
// Kubernetes pods provision their own, so a daemon on that runtime has none.
func issueWorkspaces(stateDir, issue string) (map[string]string, error) {
	dirs, err := filepath.Glob(filepath.Join(stateDir, "workspaces", "*", "*", strings.ToLower(issue)))
	if err != nil {
		return nil, fmt.Errorf("list the workspaces of %s: %w", issue, err)
	}
	workspaces := map[string]string{}
	for _, dir := range dirs {
		name := filepath.Dir(dir)
		workspaces[filepath.Base(filepath.Dir(name))+"/"+filepath.Base(name)] = dir
	}
	return workspaces, nil
}

// workspaceElsewhere is a workspace of workspaces in another repository than resolved or GitHub's
// name of it, as renames compares two names (without case, and a configured rename's two names as
// one repository), with that repository.
func workspaceElsewhere(workspaces map[string]string, resolved, named ghrepo.Repository, renames ghrepo.Renames) (dir, in string, ok bool) {
	for repo, dir := range workspaces {
		if !renames.Same(repo, resolved.String()) && !renames.Same(repo, named.String()) {
			return dir, repo, true
		}
	}
	return "", "", false
}

// recordedSpelling is what the backfill writes into a record's GitHubRepo beside the name the
// record keeps, `recorded`: `github`, GitHub's own name, when the two name one repository without
// case, and the configured pair's spelling of `recorded`'s own side when they are a pair. In that
// second case the value is not GitHub's spelling of anything GitHub still serves — after the
// transfer GitHub publishes nothing under it — which is why the helper is not called after
// GitHub's spelling, and which record.Issue.GitHubRepo's own doc says.
//
// A record's two repository columns must name one repository, compared without case
// (record.Store.FillIssueRepository and NameIssueRepository refuse anything else), and the
// record keeps the name it was recorded under for its whole life. So the pair's own side is what
// goes in: the record stays one repository and the boot goes on, and the events GitHub publishes
// under the name the repository moved to still reach the record, because heardRepositories hears
// the pair's other name as well.
func recordedSpelling(renames ghrepo.Renames, recorded string, github ghrepo.Repository) string {
	if partner, paired := renames.Partner(github.String()); paired && strings.EqualFold(partner.String(), recorded) {
		return partner.String()
	}
	return github.String()
}

// nameRepositories names every record of project that carries its repository but was written
// before the record carried GitHub's spelling of it (record.Store.UnnamedIssues) with that
// spelling, read from GitHub once per repository: SQL cannot know it, since a record may keep a
// clone's spelling that differs from GitHub's in case. A read GitHub refuses refuses the boot,
// naming the record, so the boot never subscribes to a repository in a spelling its events are not
// published under. So does a repository GitHub renamed, since the record's repository never
// changes; only a pre-release build of this daemon left records with no GitHub spelling. A rename
// the deployment configures (renames) is no such rename: GitHub holds the record's repository
// under the pair's other name, and the record is named with the pair's spelling of its own name
// (recordedSpelling), which keeps it one repository and lets the boot go on.
func nameRepositories(ctx context.Context, pool *pgxpool.Pool, records record.Store, project string, named *defaultBranches, renames ghrepo.Renames, log *slog.Logger) error {
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
		repo, err := issue.Repository()
		if err != nil {
			return err
		}
		read, err := named.Named(ctx, repo)
		if err != nil {
			return fmt.Errorf("name the repository of %s: %w", issue.Key, err)
		}
		if !renames.Same(read.Name.String(), issue.Repo) {
			return fmt.Errorf("name the repository of %s: GitHub renamed %s to %s, and a record's repository never changes; only a pre-release build of this daemon leaves a record with no GitHub spelling, so recreate that build's database",
				issue.Key, issue.Repo, read.Name)
		}
		names[issue.Key] = recordedSpelling(renames, issue.Repo, read.Name)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, issue := range unnamed {
			if err := records.NameIssueRepository(ctx, tx, issue.Key, issue.Repo, names[issue.Key]); err != nil {
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
