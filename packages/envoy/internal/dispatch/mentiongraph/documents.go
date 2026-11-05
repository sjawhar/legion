package mentiongraph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// documentMentions reads the document itself, not block_refs, identity_mentions or
// indexed excerpts, so deleting even every indexed identity row cannot hide a loss.
func documentMentions(ctx context.Context, q refs.Queryer, since time.Time, add func(string, string, string, string)) error {
	rows, err := q.Query(ctx, `select a.ref_key,v.markdown,
 coalesce((select jsonb_agg(jsonb_build_object('id',d.block_id,'type',d.type,'sum',d.text_sha256) order by d.block_id) from document_blocks d where d.artifact_id=a.id),'[]'::jsonb)
 from artifacts a
 join lateral (select markdown,created_at from artifact_versions where artifact_id=a.id and markdown is not null order by number desc limit 1) v on true
 where v.created_at >= $1 or exists (select 1 from document_blocks d where d.artifact_id=a.id and d.changed_at >= $1)`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, body string
		var raw []byte
		if err := rows.Scan(&key, &body, &raw); err != nil {
			return err
		}
		var indexed []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Sum  string `json:"sum"`
		}
		if err := json.Unmarshal(raw, &indexed); err != nil {
			return err
		}
		byText := map[string][]string{}
		for _, b := range indexed {
			if !pmdoc.IsTypedBlock(b.Type) {
				key := b.Type + "\x00" + b.Sum
				byText[key] = append(byText[key], b.ID)
			}
		}
		tree, err := pmdoc.ParseRendering(body)
		if err != nil {
			return fmt.Errorf("parse document %s for mention census: %w", key, err)
		}
		markdown, blocks, err := pmdoc.RenderWithBlockOffsets(tree)
		if err != nil {
			return fmt.Errorf("render document %s for mention census: %w", key, err)
		}
		seen := map[string]bool{}
		valid := blocks[:0]
		for _, block := range blocks {
			// Only typed ids survive a markdown rendering. Match other block ids using
			// their node metadata, never their indexed references or excerpts. Equal-text
			// blocks name the same targets, so pairing them in id order is unambiguous.
			if !pmdoc.IsTypedBlock(block.Type) {
				key := block.Type + "\x00" + refs.BlockTextSum(markdown[block.From:block.To])
				if ids := byText[key]; len(ids) > 0 {
					block.ID = ids[0]
					byText[key] = ids[1:]
				}
			}
			if block.ID == "" || seen[block.ID] || len(block.ID) > text.MaxBlockIDBytes || strings.ContainsRune(block.ID, 0) || !utf8.ValidString(block.ID) {
				continue
			}
			seen[block.ID] = true
			valid = append(valid, block)
		}
		for _, ref := range text.ExtractAt(markdown, "") {
			if !refs.IsIdentity(ref.Kind) {
				continue
			}
			holder := -1
			for i, b := range valid {
				if b.From <= ref.Offset && ref.Offset < b.To && (holder < 0 || b.To-b.From < valid[holder].To-valid[holder].From) {
					holder = i
				}
			}
			if holder >= 0 {
				add("block", key+"#"+valid[holder].ID, ref.Kind, text.TargetID(ref.Ref))
			}
		}
	}
	return rows.Err()
}
