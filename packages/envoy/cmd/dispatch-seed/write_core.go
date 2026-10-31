package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// seedActor is the author every seeded row carries that is not a session's. It is a user actor,
// the shape `author`, `created_by` and `actor` hold everywhere in the schema.
const seedActor = `{"kind":"user","id":"seed@dispatch.invalid","name":"Seed"}`

// sessionActor is the actor of a session, as events.actor carries one: the shape
// `events_session_actor_created_at_idx` is partial on and `agentLastActivity` reads.
func sessionActor(session string) string {
	return fmt.Sprintf(`{"kind":"session","id":%q}`, session)
}

// writeProjects writes the projects and their repository mappings. Each project has one default
// mapping, which an issue with no repository of its own resolves to, and one more that the issues
// carrying a repository of their own use.
func (r *seedRun) writeProjects(ctx context.Context) error {
	return r.inTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for index, project := range r.plan.projects {
			if _, err := tx.Exec(ctx, `
				insert into projects (key, name, next_number, created_at, last_seq)
				values ($1, $2, 1, $3, 0)
			`, project.Key, project.Name, timeAt(index, len(r.plan.projects))); err != nil {
				return fmt.Errorf("project %s: %w", project.Key, err)
			}
			for _, mapping := range []struct {
				repo    string
				primary bool
			}{{project.Repo, true}, {project.OwnRepo, false}} {
				if _, err := tx.Exec(ctx, `
					insert into repo_projects (repo, project, created_by, created_at, is_default)
					values ($1, $2, $3::jsonb, $4, $5)
				`, mapping.repo, project.Key, seedActor, baseTime, mapping.primary); err != nil {
					return fmt.Errorf("repo mapping %s: %w", mapping.repo, err)
				}
			}
		}
		return nil
	})
}

// writeArchitecture writes one architecture source and one snapshot for each project, the
// components of that snapshot, and the dependencies between them: the three tables the
// `component` arms of graph_edges read.
func (r *seedRun) writeArchitecture(ctx context.Context) error {
	return r.inTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for index, project := range r.plan.projects {
			snapshot := int64(index + 1)
			if _, err := tx.Exec(ctx, `
				insert into architecture_sources (project_key, repo, branch, installation_id, created_by, created_at, id)
				values ($1, $2, 'main', 1, $3::jsonb, $4, $5)
			`, project.Key, project.Repo, seedActor, baseTime, snapshot); err != nil {
				return fmt.Errorf("architecture source of %s: %w", project.Key, err)
			}
			if _, err := tx.Exec(ctx, `
				insert into architecture_snapshots (id, project_key, commit, imported_at, files)
				values ($1, $2, $3, $4, '[]'::jsonb)
			`, snapshot, project.Key, fmt.Sprintf("%040x", snapshot), baseTime); err != nil {
				return fmt.Errorf("architecture snapshot of %s: %w", project.Key, err)
			}
		}
		for _, component := range r.plan.components {
			project := r.plan.projects[component.Project]
			snapshot := int64(component.Project + 1)
			var parent *string
			if component.Parent != "" {
				parent = &component.Parent
			}
			if _, err := tx.Exec(ctx, `
				insert into components (project_key, id, title, prose, parent, paths, snapshot_id, source_id)
				values ($1, $2, $3, $4, $5, '{}', $6, $6)
			`, project.Key, component.ID, r.plan.vocabulary.phrase(r.random, 3),
				r.plan.vocabulary.phrase(r.random, 12), parent, snapshot); err != nil {
				return fmt.Errorf("component %s/%s: %w", project.Key, component.ID, err)
			}
		}
		for _, component := range r.plan.components {
			if component.Depends == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `
				insert into component_depends (project_key, from_id, to_id) values ($1, $2, $3)
			`, r.plan.projects[component.Project].Key, component.ID, component.Depends); err != nil {
				return fmt.Errorf("component dependency %s: %w", component.ID, err)
			}
		}
		return nil
	})
}

// writeIssues writes every issue, parents before children, so issues_parent_key_fkey holds on
// each row as it lands. The search vector, the title lexemes and the embedding queue row are the
// triggers' work, as they are on every other write.
func (r *seedRun) writeIssues(ctx context.Context) error {
	issues := r.plan.issues
	return r.inBatches(ctx, "issues", len(issues), batchRows, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		rows := make([][]any, 0, to-from)
		for index := from; index < to; index++ {
			issue := issues[index]
			var parent *string
			if issue.Parent >= 0 {
				parent = &issues[issue.Parent].Key
			}
			var repo *string
			if issue.OwnRepo {
				repo = &r.plan.projects[issue.Project].OwnRepo
			}
			var closedAt any
			if issue.Status == "done" {
				closedAt = issue.CreatedAt.Add(seedSpan / 10)
			}
			rows = append(rows, []any{
				issue.Key, r.plan.projects[issue.Project].Key, issue.Number,
				r.plan.vocabulary.phrase(r.random, 7), issue.Status, parent, seedActor,
				issue.CreatedAt, issue.CreatedAt, closedAt, issue.Rank, repo, int16(index % 4),
			})
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"issues"}, []string{
			"key", "project_key", "number", "title", "status", "parent_key", "created_by",
			"created_at", "updated_at", "closed_at", "rank", "repo", "priority",
		}, pgx.CopyFromRows(rows))
		return err
	})
}

// writeIssueLinks writes the blocked_by links. The heavily linked issue takes HubLinks of them,
// so it is blocked by that many issues; the rest are spread so that the total is exactly the
// figure the spec gives and a known share of the issues carry three or more.
func (r *seedRun) writeIssueLinks(ctx context.Context) error {
	links := r.plan.blockedByLinks()
	return r.inBatches(ctx, "issue links", len(links), batchRows, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		rows := make([][]any, 0, to-from)
		for _, link := range links[from:to] {
			rows = append(rows, []any{
				r.plan.issues[link[0]].Key, "blocked_by", r.plan.issues[link[1]].Key,
				r.plan.issues[link[0]].CreatedAt,
			})
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"issue_links"},
			[]string{"issue_key", "kind", "target_key", "created_at"}, pgx.CopyFromRows(rows))
		return err
	})
}

// blockedByLinks is every (issue, blocker) pair the seed writes, in issue order. The hub's are
// first; then each issue takes its share of what is left, which gives a quarter of them three
// blockers and so the "three or more blockers" the spec asks for.
func (p *seedPlan) blockedByLinks() [][2]int {
	links := make([][2]int, 0, p.sizes.BlockedBy)
	seen := make(map[[2]int]struct{}, p.sizes.BlockedBy)
	add := func(issue, blocker int) bool {
		if issue == blocker || issue < 0 || blocker < 0 {
			return false
		}
		pair := [2]int{issue, blocker}
		if _, held := seen[pair]; held {
			return false
		}
		seen[pair] = struct{}{}
		links = append(links, pair)
		return true
	}
	for step := 1; step <= p.sizes.HubLinks && len(links) < p.sizes.BlockedBy; step++ {
		add(p.hub, p.hub+step)
	}
	// Four blockers on every fourth issue and one on every other gives 1.75 per four issues,
	// close to the 19,800 links over 20,000 issues the spec gives, with a quarter of the issues
	// above the "three or more" line.
	for index := range p.issues {
		if len(links) >= p.sizes.BlockedBy {
			break
		}
		blockers := 1
		if index%4 == 3 {
			blockers = 4
		}
		project := p.projects[p.issues[index].Project]
		for step := 1; step <= blockers && len(links) < p.sizes.BlockedBy; step++ {
			if project.Issues < 2 {
				break
			}
			target := project.FirstIssue + (p.issues[index].Number-1+step*7)%project.Issues
			add(index, target)
		}
	}
	return links
}

// writeIssueComponents puts the explicit component rows on the issues that have children, so an
// issue's components resolve up its parent chain, and gives each of them members: the `affects`
// arm of graph_edges.
func (r *seedRun) writeIssueComponents(ctx context.Context) error {
	var ancestors []int
	for index, issue := range r.plan.issues {
		if issue.Children > 0 {
			ancestors = append(ancestors, index)
		}
	}
	perProject := max(1, len(r.plan.components)/len(r.plan.projects))
	return r.inBatches(ctx, "issue components", len(ancestors), batchRows, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		owners := make([][]any, 0, to-from)
		members := make([][]any, 0, 2*(to-from))
		for position, index := range ancestors[from:to] {
			issue := r.plan.issues[index]
			project := r.plan.projects[issue.Project]
			owners = append(owners, []any{issue.Key, "explicit", nil, issue.CreatedAt, issue.CreatedAt})
			for step := range min(2, perProject) {
				component := fmt.Sprintf("component-%d", (from+position+step)%perProject+1)
				members = append(members, []any{issue.Key, project.Key, component})
			}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"issue_components"},
			[]string{"issue_key", "mode", "reason", "created_at", "updated_at"},
			pgx.CopyFromRows(owners)); err != nil {
			return err
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"issue_component_members"},
			[]string{"issue_key", "project_key", "component_id"}, pgx.CopyFromRows(members))
		return err
	})
}
