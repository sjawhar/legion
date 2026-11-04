package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type unifiedInboxItem struct {
	Cause        string      `json:"cause"`
	Kind         string      `json:"kind"`
	ID           string      `json:"id"`
	ThreadID     string      `json:"thread_id"`
	CreatedAt    time.Time   `json:"created_at"`
	Body         string      `json:"body"`
	Author       model.Actor `json:"author"`
	Href         string      `json:"href"`
	Subject      string      `json:"subject"`
	SnoozedUntil *time.Time  `json:"snoozed_until"`
}

type unifiedInboxPage struct {
	Person     string             `json:"person"`
	Rows       []unifiedInboxItem `json:"rows"`
	Total      int                `json:"total"`
	Limit      int                `json:"limit"`
	Offset     int                `json:"offset"`
	Horizon    int64              `json:"horizon"`
	Expansions []unifiedInboxItem `json:"expansions"`
}

// requireOwnInbox rejects identity selectors rather than treating a spelling of
// another login as an ignored filter. There is no caller-supplied person id.
func (s *server) requireOwnInbox(w http.ResponseWriter, r *http.Request) (model.Actor, bool) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return actor, false
	}
	for key := range r.URL.Query() {
		if key != "limit" && key != "offset" && key != "mentions" {
			writeError(w, "OWN_INBOX_ONLY", http.StatusBadRequest, "this read is the signed-in person's own inbox; identity and assignee filters are not accepted")
			return actor, false
		}
	}
	return actor, true
}

func readUnifiedInbox(ctx context.Context, q queryer, actorID string, limit, offset int) (unifiedInboxPage, error) {
	page := unifiedInboxPage{Person: model.CanonicalLogin(actorID), Rows: []unifiedInboxItem{}, Expansions: []unifiedInboxItem{}, Limit: limit, Offset: offset}
	args := append(unreadDirectRepliesArgs(actorID, nil), limit, offset)
	var raw []byte
	if err := q.QueryRow(ctx, unifiedInboxQuery, args...).Scan(&page.Total, &raw); err != nil {
		return page, fmt.Errorf("read unified inbox: %w", err)
	}
	if err := json.Unmarshal(raw, &page.Rows); err != nil {
		return page, fmt.Errorf("decode unified inbox: %w", err)
	}
	return page, nil
}

func (s *server) listUnifiedInbox(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireOwnInbox(w, r)
	if !ok {
		return
	}
	limit, offset, err := parseMyAnswersPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	mentions := r.URL.Query().Get("mentions")
	if mentions != "" && mentions != "false" && mentions != "true" {
		writeError(w, "INVALID_QUERY", 400, "mentions must be true or false")
		return
	}
	// The graph has no owed queue: source rows and user_threads commit together.
	// Taking the snapshot after this shared Horizon means every event the feed can
	// announce already has its node here, without hiding the caller's later writes.
	safe, err := s.horizon().Safe(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := s.deps.Store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	page, err := readUnifiedInbox(r.Context(), tx, actor.ID, limit, offset)
	if err == nil && mentions == "true" {
		page.Expansions, err = readInboxMentions(r.Context(), tx, actor.ID, limit, offset)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	page.Horizon = safe
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, page)
}

// Mention expansion is one bounded, default-off arm. Its rows never alter the
// legacy membership census. Direct messages have no public issue/document node;
// their replies remain on the independently scoped direct-conversation arm.
func readInboxMentions(ctx context.Context, q queryer, actorID string, limit, offset int) ([]unifiedInboxItem, error) {
	args := append(unreadDirectRepliesArgs(actorID, nil), limit, offset)
	rows, err := q.Query(ctx, unifiedInboxCandidates+`, candidates as (`+inboxMentionPagesSQL+`), mentioned as (
  select * from candidates order by created_at desc,from_kind,from_id limit $4 offset $5
 ) select x.from_kind,x.from_id,x.created_at,
   coalesce(c.body,m.body,b.excerpt,''), coalesce(c.author,m.author,'{"kind":"system","id":"document"}'::jsonb),
   coalesce(c.issue_key,m.issue_key,a.issue_key),a.project_key,a.slug,b.block_id,coalesce(a.is_primary,false)
 from mentioned x
 left join comments c on x.from_kind='comment' and c.id::text=x.from_id
 left join messages m on x.from_kind='message' and m.id::text=x.from_id and m.issue_key is not null
 left join document_blocks b on x.from_kind='block' and b.node_id=x.from_id
 left join artifacts a on a.id=coalesce(c.artifact_id,b.artifact_id)
 where c.id is not null or m.id is not null or b.node_id is not null`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []unifiedInboxItem{}
	for rows.Next() {
		var item unifiedInboxItem
		var author []byte
		var issue, project, slug, block *string
		var primary bool
		if err := rows.Scan(&item.Kind, &item.ID, &item.CreatedAt, &item.Body, &author, &issue, &project, &slug, &block, &primary); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(author, &item.Author); err != nil {
			return nil, err
		}
		item.Cause = "mention"
		item.Subject = "Mentioned you"
		switch {
		case item.Kind == "block" && slug != nil:
			if issue != nil {
				item.Href = issueDocumentHref("/issues/"+*issue, &model.SearchArtifact{Slug: *slug}, primary)
			} else if project != nil {
				item.Href = "/projects/" + *project + "/documents/" + url.PathEscape(*slug)
			}
			if block != nil {
				item.Href += "#b-" + text.EncodeBlockID(*block)
			}
		case issue != nil:
			item.Href = "/issues/" + *issue + "/" + item.Kind + "s/" + item.ID
		case project != nil && slug != nil:
			item.Href = "/projects/" + *project + "/documents/" + url.PathEscape(*slug) + "?" + item.Kind + "=" + item.ID
		}
		if strings.HasPrefix(item.Href, "/") {
			items = append(items, item)
		}
	}
	return items, rows.Err()
}
