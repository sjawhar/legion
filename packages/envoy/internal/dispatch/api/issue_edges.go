package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// filingSource accepts a public conversation item in this project, on an issue or project document.
// The canonical reference is stored once, by creation; incidental citations are not provenance.
func (s *server) filingSource(ctx context.Context, q queryer, project, raw string) (any, error) {
	if raw == "" {
		return nil, nil
	}
	raw = strings.TrimSpace(raw)
	ref, ok := text.ParseRef(raw, s.deps.ServerURL)
	if !ok || ref.Block != "" || ref.Pin != 0 || (ref.Kind != "message" && ref.Kind != "ask" && ref.Kind != "comment") || (ref.IssueKey == "" && ref.Project == "") {
		return nil, errorf(http.StatusBadRequest, "FILED_FROM_INPUT", "filed_from must name a public message, ask or comment")
	}
	node, found, err := refs.Node(ctx, q, ref.Kind, ref.ID, false)
	if err != nil {
		return nil, err
	}
	issueMatches := ref.IssueKey != "" && node.IssueKey != nil && *node.IssueKey == ref.IssueKey
	projectMatches := ref.Project != "" && node.IssueKey == nil && node.Project == ref.Project
	if projectMatches {
		// Item references omit their document slug after parsing; validate that owner too.
		var documentRef string
		if strings.HasPrefix(raw, "dispatch://") {
			documentRef, _, _ = strings.Cut(raw, "/"+ref.Kind+"/")
		} else if page, err := url.Parse(raw); err == nil {
			page.RawQuery, page.Fragment = "", ""
			documentRef = page.String()
		}
		document, valid := text.ParseRef(documentRef, s.deps.ServerURL)
		ownerRef, _, _ := strings.Cut(node.Ref, "/"+ref.Kind+"/")
		projectMatches = valid && document.Kind == "artifact" &&
			ownerRef == "dispatch://"+document.Project+"/artifact/"+document.ID
	}
	if !found || (!issueMatches && !projectMatches) || node.Project != project || node.Ref == "" {
		return nil, errorf(http.StatusBadRequest, "FILED_FROM_INPUT", "filed_from must name an existing conversation item in project %s", project)
	}
	return node.Ref, nil
}

func (s *server) getIssueLinks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	node, found, err := refs.Node(r.Context(), s.deps.Store.Pool, "issue", key, false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !found {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "issue not found")
		return
	}
	edges, err := refs.Edges(r.Context(), s.deps.Store.Pool, refs.Query{Kind: "issue", ID: key, Direction: "out", All: true})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, model.GraphReferences{Node: node, Edges: edges})
}

// Compare the common edge rows independently against their source records, in one snapshot.
// New kinds are intentionally outside this equality; they have their own derivation tests.
const issueLinksShadowSQL = `with expected as (
 select kind,to_kind,to_id,created_at,source_seq from refs where from_kind='issue' and from_id=$1
 union all select 'child_of','issue',parent_key,created_at,null from issues where key=$1 and parent_key is not null
 union all select 'blocked_by','issue',target_key,created_at,null from issue_links where issue_key=$1 and kind='blocked_by'
 union all select 'affects','component',m.project_key||'/'||m.component_id,c.updated_at,null
 from issue_component_members m join issue_components c using (issue_key) where m.issue_key=$1
), actual as (
 select kind,to_kind,to_id,created_at,source_seq from ` + refs.AllEdgesSQL + ` g
 where from_kind='issue' and from_id=$1 and kind not in ('duplicate_of','filed_from','authored_by','delivered_to')
), missing as (select * from expected except all select * from actual), extra as (select * from actual except all select * from expected)
select (select count(*) from expected),(select count(*) from actual),(select count(*) from missing)+(select count(*) from extra)`

func (s *server) getIssueLinksShadow(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	var exists bool
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select exists(select 1 from issues where key=$1)`, key).Scan(&exists); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !exists {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "issue not found")
		return
	}
	var legacy, graph, mismatches int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), issueLinksShadowSQL, key).Scan(&legacy, &graph, &mismatches); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"issue": key, "legacy": legacy, "graph": graph, "mismatches": mismatches})
}
