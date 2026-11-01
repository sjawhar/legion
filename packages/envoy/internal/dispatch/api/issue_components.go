package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const maxIssueComponents = 50

// componentsInput is the decoded `components` field of an issue write. Mode "inherit" deletes
// the issue's own row (the default state); "explicit" replaces it with IDs; "none" records a
// Reason the issue is not architectural.
type componentsInput struct {
	Mode   string
	IDs    []string
	Reason string
}

// parseIssueComponents decodes the tri-state `components` field of an issue write. Absent →
// (nil, false): leave it alone. JSON null or {"mode":"inherit"} → back to inheriting.
// {"mode":"explicit","ids":[...]} needs one or more bare component ids (`web`, `daemon`);
// {"mode":"none","reason":"..."} needs a non-blank reason. Anything else → 400 COMPONENTS_INPUT.
func parseIssueComponents(raw json.RawMessage) (*componentsInput, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return &componentsInput{Mode: "inherit"}, true, nil
	}
	var input struct {
		Mode   *string   `json:"mode"`
		IDs    *[]string `json:"ids"`
		Reason *string   `json:"reason"`
	}
	if err := json.Unmarshal(raw, &input); err != nil || input.Mode == nil {
		return nil, true, errorf(http.StatusBadRequest, "COMPONENTS_INPUT", "components must be null or an object with mode inherit, explicit, or none")
	}
	parsed := &componentsInput{Mode: strings.TrimSpace(*input.Mode)}
	switch parsed.Mode {
	case "inherit":
		return parsed, true, nil
	case "explicit":
		if input.IDs == nil || len(*input.IDs) == 0 {
			return nil, true, errorf(http.StatusBadRequest, "COMPONENTS_INPUT", "components.ids must name at least one component for mode explicit")
		}
		if len(*input.IDs) > maxIssueComponents {
			return nil, true, countExceededError("COMPONENTS_INPUT", "components.ids", len(*input.IDs), maxIssueComponents)
		}
		seen := make(map[string]struct{}, len(*input.IDs))
		for _, raw := range *input.IDs {
			id := strings.TrimSpace(raw)
			if !text.IsComponentID(id) {
				return nil, true, errorf(http.StatusBadRequest, "COMPONENTS_INPUT", "%q is not a component id (a lowercase slug such as web or dispatch-server)", raw)
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			parsed.IDs = append(parsed.IDs, id)
		}
		sort.Strings(parsed.IDs)
		return parsed, true, nil
	case "none":
		if input.Reason == nil || strings.TrimSpace(*input.Reason) == "" {
			return nil, true, errorf(http.StatusBadRequest, "COMPONENTS_INPUT", "components.reason must say why the issue is not architectural for mode none")
		}
		parsed.Reason = strings.TrimSpace(*input.Reason)
		return parsed, true, nil
	}
	return nil, true, errorf(http.StatusBadRequest, "COMPONENTS_INPUT", "components.mode must be inherit, explicit, or none")
}

// writeIssueComponents replaces the issue's own attachment row inside tx. For mode explicit
// every id must be a component of the issue's project that is not external; the 400 names each
// id that is unknown (never imported, or retired by a re-import) and each that is external.
func writeIssueComponents(ctx context.Context, tx pgx.Tx, key, project string, input componentsInput) error {
	if input.Mode == "explicit" {
		rows, err := tx.Query(ctx, `select id, external from components where project_key = $1 and id = any($2)`, project, input.IDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		present := make(map[string]bool, len(input.IDs))
		for rows.Next() {
			var id string
			var external bool
			if err := rows.Scan(&id, &external); err != nil {
				return err
			}
			present[id] = external
		}
		if err := rows.Err(); err != nil {
			return err
		}
		var unknown, external []string
		for _, id := range input.IDs {
			isExternal, ok := present[id]
			switch {
			case !ok:
				unknown = append(unknown, id)
			case isExternal:
				external = append(external, id)
			}
		}
		if len(unknown) > 0 {
			return errorf(http.StatusBadRequest, "COMPONENTS_INPUT", "%s has no component %s in its current architecture model", project, strings.Join(unknown, ", "))
		}
		if len(external) > 0 {
			return errorf(http.StatusBadRequest, "COMPONENTS_INPUT", "external components cannot be attached: %s", strings.Join(external, ", "))
		}
	}
	if _, err := tx.Exec(ctx, `delete from issue_components where issue_key = $1`, key); err != nil {
		return err
	}
	if input.Mode == "inherit" {
		return nil
	}
	var reason any
	if input.Mode == "none" {
		reason = input.Reason
	}
	if _, err := tx.Exec(ctx, `insert into issue_components (issue_key, mode, reason) values ($1, $2, $3)`, key, input.Mode, reason); err != nil {
		return err
	}
	if input.Mode == "explicit" {
		if _, err := tx.Exec(ctx, `
			insert into issue_component_members (issue_key, project_key, component_id)
			select $1, $2, unnest($3::text[])
		`, key, project, input.IDs); err != nil {
			return err
		}
	}
	return nil
}

// issueComponentsTerms resolve the effective attachment of every issue of `subjects`, the
// one-column term of issue keys each statement that splices these defines before them: for each
// of those issues the nearest issue on its parent chain (itself first) with an issue_components
// row of either mode, that row's members split into ids still in the project's component model
// and ids a re-import retired. One walk climbs every subject's chain together and one grouped
// read splits every owner's members, so a listing pays one set of joins rather than a walk and
// two subqueries per row; `comp` holds a row for each issue an ancestor chose for, and
// issueComponentsJoin leaves the columns null for an issue none chose for, as the per-row lateral
// this replaced did. The walk climbs with `union all` bounded by parentDepthCap, like
// issueRepoTerms, so it terminates even if a raced reparent ever commits a cycle: a row carries
// its depth, so no two rows of one chain are equal and `union`'s distinct step would remove
// nothing.
var issueComponentsTerms = `comp_chain as (
		select s.key as start, i.key, i.parent_key, 0 as depth
		from subjects s join issues i on i.key = s.key
		union all
		select c.start, p.key, p.parent_key, c.depth + 1
		from comp_chain c join issues p on p.key = c.parent_key
		where c.depth < ` + parentDepthCapSQL + `
	), comp_owner as (
		select distinct on (c.start) c.start as key, c.key as owner_key, ic.mode, ic.reason
		from comp_chain c join issue_components ic on ic.issue_key = c.key
		order by c.start, c.depth
	), comp as (
		select o.key, o.owner_key, o.mode, o.reason,
		       coalesce(array_agg(m.component_id order by m.component_id) filter (where x.id is not null), '{}'::text[]) as ids,
		       coalesce(array_agg(m.component_id order by m.component_id) filter (where m.component_id is not null and x.id is null), '{}'::text[]) as unknown
		from comp_owner o
		left join issue_component_members m on m.issue_key = o.owner_key
		left join components x on x.project_key = m.project_key and x.id = m.component_id
		group by o.key, o.owner_key, o.mode, o.reason
	)`

// issueComponentsJoin attaches issueComponentsTerms' answer to the issue aliased `i` in the
// enclosing query. It yields one row per issue, all null when no ancestor chose.
const issueComponentsJoin = `left join comp on comp.key = i.key`

// issueComponentsColumns are comp's columns in the order componentsScan expects.
const issueComponentsColumns = `comp.owner_key, comp.mode, comp.reason, comp.ids, comp.unknown`

// componentsScan receives issueComponentsColumns for one issue.
type componentsScan struct {
	ownerKey *string
	mode     *string
	reason   *string
	ids      []string
	unknown  []string
}

func (c *componentsScan) targets() []any {
	return []any{&c.ownerKey, &c.mode, &c.reason, &c.ids, &c.unknown}
}

// resolve turns the scanned columns into the issue's effective attachment.
func (c componentsScan) resolve(issueKey string) model.IssueComponents {
	components := model.IssueComponents{Mode: "inherit", IDs: []string{}, Unknown: []string{}}
	if c.ownerKey == nil {
		return components
	}
	components.Mode = *c.mode
	components.Reason = c.reason
	if c.ids != nil {
		components.IDs = c.ids
	}
	if c.unknown != nil {
		components.Unknown = c.unknown
	}
	if *c.ownerKey != issueKey {
		components.InheritedFrom = c.ownerKey
	}
	return components
}
