package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// getArchitectureTree answers the project's component model with the work attached to it:
// every component with its counts and issue rows, and the issues no component counts —
// unassigned, declared not architectural, or linked only to retired components. 404
// SOURCE_NOT_FOUND when the project has no architecture source.
func (s *server) getArchitectureTree(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project := r.PathValue("key")
	pool := s.deps.Store.Pool
	var tree model.ArchitectureTree
	if err := pool.QueryRow(r.Context(), `
		select repo, branch, last_commit, last_sync_at, last_error from architecture_sources where project_key = $1
	`, project).Scan(&tree.Source.Repo, &tree.Source.Branch, &tree.Source.LastCommit, &tree.Source.LastSyncAt, &tree.Source.LastError); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "SOURCE_NOT_FOUND", http.StatusNotFound, "no architecture source configured for "+project)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	components, byID, err := loadTreeComponents(r.Context(), pool, project)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issues, err := loadTreeIssues(r.Context(), pool, project)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := attachTreeIssues(r.Context(), pool, project, issues, byID); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	tree.Components = make([]model.ArchitectureTreeComponent, 0, len(components))
	tree.Unassigned = []model.ArchitectureTreeIssueRef{}
	tree.NotArchitectural = []model.ArchitectureTreeNone{}
	tree.RetiredLinks = []model.ArchitectureTreeRetired{}
	for _, component := range components {
		for _, row := range component.Issues {
			component.Total++
			if row.Status == "done" {
				component.Done++
			}
			if row.Attached != "contained" {
				component.OwnTotal++
				if row.Status == "done" {
					component.OwnDone++
				}
			}
		}
		if component.Total == 0 && !component.External {
			tree.Totals.ComponentsWithoutWork++
		}
		tree.Components = append(tree.Components, *component)
	}
	for _, issue := range issues {
		tree.Totals.IssuesTotal++
		if issue.Status == "done" {
			tree.Totals.IssuesDone++
		}
		switch {
		case issue.Components.Mode == "inherit":
			tree.Unassigned = append(tree.Unassigned, model.ArchitectureTreeIssueRef{Key: issue.Key, Title: issue.Title, Status: issue.Status})
		case issue.Components.Mode == "none":
			reason := ""
			if issue.Components.Reason != nil {
				reason = *issue.Components.Reason
			}
			tree.NotArchitectural = append(tree.NotArchitectural, model.ArchitectureTreeNone{
				Key: issue.Key, Title: issue.Title, Status: issue.Status, Reason: reason, InheritedFrom: issue.Components.InheritedFrom,
			})
		}
		if len(issue.Components.Unknown) > 0 {
			tree.RetiredLinks = append(tree.RetiredLinks, model.ArchitectureTreeRetired{
				Key: issue.Key, Title: issue.Title, Status: issue.Status, IDs: issue.Components.Unknown,
			})
		}
	}
	tree.Totals.Unassigned = len(tree.Unassigned)
	tree.Totals.NotArchitectural = len(tree.NotArchitectural)
	tree.Totals.RetiredLinks = len(tree.RetiredLinks)
	WriteJSON(w, http.StatusOK, tree)
}

// loadTreeComponents reads the project's current model in id order, each component with its
// dependency ids and an empty issue list.
func loadTreeComponents(ctx context.Context, q queryer, project string) ([]*model.ArchitectureTreeComponent, map[string]*model.ArchitectureTreeComponent, error) {
	rows, err := q.Query(ctx, `
		select c.id, c.title, c.parent, c.external, c.paths, c.prose,
		       coalesce((select array_agg(d.to_id order by d.to_id) from component_depends d
		                 where d.project_key = c.project_key and d.from_id = c.id), '{}'::text[])
		from components c where c.project_key = $1 order by c.id
	`, project)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	components := []*model.ArchitectureTreeComponent{}
	byID := map[string]*model.ArchitectureTreeComponent{}
	for rows.Next() {
		component := &model.ArchitectureTreeComponent{Issues: []model.ArchitectureTreeIssue{}}
		if err := rows.Scan(&component.ID, &component.Title, &component.Parent, &component.External, &component.Paths, &component.Prose, &component.DependsOn); err != nil {
			return nil, nil, err
		}
		if component.Paths == nil {
			component.Paths = []string{}
		}
		components = append(components, component)
		byID[component.ID] = component
	}
	return components, byID, rows.Err()
}

// treeIssue is one project issue with its effective attachment, in the project list's order.
type treeIssue struct {
	model.ArchitectureTreeIssue
	Components model.IssueComponents
}

// loadTreeIssues reads every issue of the project — icebox and closed included — with its
// effective attachment resolved, in the issue list's order (lifecycle status, then rank).
func loadTreeIssues(ctx context.Context, q queryer, project string) ([]treeIssue, error) {
	rows, err := q.Query(ctx, `
		select i.key, i.title, i.status, i.priority, i.parent_key, i.updated_at,
		       coalesce((select json_agg(json_build_object('url', l.url, 'kind', l.kind) order by l.url)
		                 from issue_external_links l where l.issue_key = i.key), '[]'),
		       `+issueComponentsColumns+`
		from issues i
		`+issueComponentsLateral+`
		where i.project_key = $1
		order by `+issueStatusCase+`, i.rank asc, i.created_at asc
	`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []treeIssue{}
	for rows.Next() {
		var issue treeIssue
		var links []byte
		var components componentsScan
		targets := []any{&issue.Key, &issue.Title, &issue.Status, &issue.Priority, &issue.Parent, &issue.UpdatedAt, &links}
		if err := rows.Scan(append(targets, components.targets()...)...); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(links, &issue.ExternalLinks); err != nil {
			return nil, err
		}
		issue.Components = components.resolve(issue.Key)
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// attachTreeIssues joins every issue's effective set to the component containment closure
// (transitive over components.parent, depth-capped like the parent walks) and appends each
// issue once to every component it counts for, in the issues' order. When an issue qualifies
// several ways the strongest wins: direct (its own row names the component) over inherited (an
// ancestor's row does) over contained (its set names a component this one contains; via is
// that component). Retired ids match no component and count for nothing.
func attachTreeIssues(ctx context.Context, q queryer, project string, issues []treeIssue, byID map[string]*model.ArchitectureTreeComponent) error {
	rows, err := q.Query(ctx, `
		with recursive chain as (
			select i.key as start, i.key, i.parent_key, 0 as depth from issues i where i.project_key = $1
			union
			select c.start, p.key, p.parent_key, c.depth + 1
			from chain c join issues p on p.key = c.parent_key
			where c.depth < 32
		), owner as (
			select distinct on (c.start) c.start as key, c.key as owner_key
			from chain c join issue_components ic on ic.issue_key = c.key
			order by c.start, c.depth
		), effective as (
			select o.key, o.owner_key, m.component_id
			from owner o join issue_component_members m on m.issue_key = o.owner_key
		), containment as (
			select id as ancestor, id as descendant, 0 as depth from components where project_key = $1
			union
			select k.ancestor, x.id, k.depth + 1
			from containment k join components x on x.project_key = $1 and x.parent = k.descendant
			where k.depth < 32
		), qualified as (
			select e.key, k.ancestor as component_id, e.component_id as via,
			       case when k.depth > 0 then 2 when e.owner_key = e.key then 0 else 1 end as strength
			from effective e join containment k on k.descendant = e.component_id
		)
		select distinct on (key, component_id) key, component_id, via, strength
		from qualified order by key, component_id, strength, via
	`, project)
	if err != nil {
		return err
	}
	defer rows.Close()
	type attachment struct {
		via      string
		strength int
	}
	attached := map[string]map[string]attachment{}
	for rows.Next() {
		var key, componentID, via string
		var strength int
		if err := rows.Scan(&key, &componentID, &via, &strength); err != nil {
			return err
		}
		if attached[key] == nil {
			attached[key] = map[string]attachment{}
		}
		attached[key][componentID] = attachment{via: via, strength: strength}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, issue := range issues {
		for componentID, how := range attached[issue.Key] {
			component, ok := byID[componentID]
			if !ok {
				continue
			}
			row := issue.ArchitectureTreeIssue
			switch how.strength {
			case 0:
				row.Attached = "direct"
			case 1:
				row.Attached = "inherited"
			default:
				row.Attached = "contained"
				via := how.via
				row.Via = &via
			}
			component.Issues = append(component.Issues, row)
		}
	}
	return nil
}
