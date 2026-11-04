package docs

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/decisionmigration"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type DecisionConversion struct {
	Documents int
	Moved     int
	Dropped   int
	Restored  int
	Review    int
	Failed    int
}

type legacyDecisionDocument struct{ id, key, spec string }

// ConvertDecisionDocuments is deliberately not a boot repair. Without apply it only reads and
// reports. Each issue is converted in one joined document transaction, so retrying never
// appends a second node. Source versions, reviews and resolved conversations remain on the
// retired history document. Old builds refuse documents containing decision nodes.
func (s *Service) ConvertDecisionDocuments(ctx context.Context, apply bool, out io.Writer) (DecisionConversion, error) {
	ctx = store.WithTransactionTracking(ctx)
	var report DecisionConversion
	var failures []error
	rows, err := s.store.Pool.Query(ctx, `
 select a.id::text, a.issue_key, coalesce(spec.id::text,'')
 from artifacts a left join artifacts spec on spec.issue_key=a.issue_key and spec.is_primary and spec.kind='doc'
 where a.kind='doc' and (a.name='decisions.md' or a.slug='decisions-md')
 order by a.issue_key,a.id`)
	if err != nil {
		return report, err
	}
	var sources []legacyDecisionDocument
	for rows.Next() {
		var source legacyDecisionDocument
		var key *string
		if err := rows.Scan(&source.id, &key, &source.spec); err != nil {
			rows.Close()
			return report, err
		}
		if key == nil {
			report.Documents++
			report.Failed++
			failure := fmt.Errorf("legacy decisions document %s has no issue/spec; left untouched", source.id)
			fmt.Fprintln(out, failure)
			failures = append(failures, failure)
			continue
		}
		source.key = *key
		sources = append(sources, source)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}
	converted := map[string]bool{}
	for first := 0; first < len(sources); {
		last := first + 1
		for last < len(sources) && sources[last].key == sources[first].key {
			last++
		}
		group := sources[first:last]
		report.Documents += len(group)
		converted[sources[first].key] = true
		var invalid error
		for _, source := range group {
			if source.spec == "" || source.id == source.spec {
				invalid = fmt.Errorf("%s decisions document %s has no separate spec; left untouched", source.key, source.id)
				fmt.Fprintln(out, invalid)
			}
		}
		var result DecisionConversion
		if invalid == nil {
			result, invalid = s.convertDecisionIssue(ctx, group, apply, out)
		}
		if invalid != nil {
			report.Failed++
			failures = append(failures, fmt.Errorf("%s: %w", sources[first].key, invalid))
		} else {
			report.Moved += result.Moved
			report.Dropped += result.Dropped
			report.Restored += result.Restored
			report.Review += result.Review
		}
		first = last
	}
	// An answered ask removed from a spec remains attributable to it through its block id and
	// recorded block text. Also restore these on issues with no legacy decisions document.
	rows, err = s.store.Pool.Query(ctx, `
 select distinct spec.issue_key,spec.id::text
 from artifacts spec join asks a on a.issue_key=spec.issue_key
 where spec.is_primary and spec.kind='doc' and a.state='answered' and a.block_id is not null
 and exists (select 1 from document_block_texts t where t.artifact_id=spec.id and t.block_id=a.block_id)
 and not exists (select 1 from document_blocks b where b.artifact_id=spec.id and b.block_id=a.block_id and b.type='ask')
 order by spec.issue_key,spec.id::text`)
	if err != nil {
		return report, err
	}
	var repairs []legacyDecisionDocument
	for rows.Next() {
		var source legacyDecisionDocument
		if err := rows.Scan(&source.key, &source.spec); err != nil {
			rows.Close()
			return report, err
		}
		if !converted[source.key] {
			repairs = append(repairs, source)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}
	for _, source := range repairs {
		result, err := s.convertDecisionIssue(ctx, []legacyDecisionDocument{source}, apply, out)
		if err != nil {
			report.Failed++
			failures = append(failures, err)
		} else {
			report.Restored += result.Restored
		}
	}
	fmt.Fprintf(out, "convert-decisions: documents=%d moved=%d dropped=%d restored=%d review=%d failed_issues=%d apply=%t\n", report.Documents, report.Moved, report.Dropped, report.Restored, report.Review, report.Failed, apply)
	fmt.Fprintln(out, "Restoration can detect retained ask rows with spec block history. Decisions removed before block history existed, or asks whose rows were deleted, require a person to identify the original source; nothing is inferred.")
	return report, errors.Join(failures...)
}

func (s *Service) convertDecisionIssue(ctx context.Context, sources []legacyDecisionDocument, apply bool, out io.Writer) (DecisionConversion, error) {
	var report DecisionConversion
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	if apply {
		var key string
		if err := tx.QueryRow(ctx, `select key from issues where key=$1 for update`, sources[0].key).Scan(&key); err != nil {
			return report, err
		}
	}
	joined, ledger := s.Join(withLifecycleWrite(withOwnerVerified(ctx)), tx)
	defer ledger.Discard()
	var blocked []string
	for _, source := range sources {
		if source.id == "" {
			continue
		}
		fmt.Fprintf(out, "%s decisions.md (%s) -> spec (%s)\n", source.key, source.id, source.spec)
		open, err := conversionOpenConversations(joined, tx, source.id)
		if err != nil {
			return report, err
		}
		if len(open) > 0 {
			fmt.Fprintf(out, "  refuse %s: open conversations: %s\n", source.id, strings.Join(open, ", "))
			blocked = append(blocked, source.id)
		}
	}
	if len(blocked) > 0 {
		return report, fmt.Errorf("source documents have open conversations: %s; issue left untouched", strings.Join(blocked, ", "))
	}
	for _, source := range sources {
		result, err := s.convertDecisionDocument(joined, tx, source, apply, out)
		if err != nil {
			return report, err
		}
		report.Moved += result.Moved
		report.Dropped += result.Dropped
		report.Restored += result.Restored
		report.Review += result.Review
	}
	if apply {
		if err := ledger.Commit(ctx); err != nil {
			return report, err
		}
	}
	return report, nil
}

func (s *Service) convertDecisionDocument(joined context.Context, tx pgx.Tx, source legacyDecisionDocument, apply bool, out io.Writer) (DecisionConversion, error) {
	var report DecisionConversion
	var entries []decisionmigration.Entry
	var legacyBlocks []model.ArtifactBlock
	var legacyMarkdown string
	if source.id != "" {
		markdown, blocks, err := s.TextWithBlocks(joined, source.id)
		if err != nil {
			return report, err
		}
		legacyBlocks = blocks
		legacyMarkdown = markdown
		entries, err = decisionmigration.Plan(markdown, source.id)
		if err != nil {
			return report, err
		}
	}
	_, specBlocks, err := s.TextWithBlocks(joined, source.spec)
	if err != nil {
		return report, err
	}
	held := map[string]bool{}
	for _, block := range specBlocks {
		held[block.ID] = true
	}
	var specSlug string
	if err := tx.QueryRow(joined, `select slug from artifacts where id=$1::uuid`, source.spec).Scan(&specSlug); err != nil {
		return report, err
	}
	for i := range entries {
		shaped := entries[i].Shaped[:0]
		for _, value := range entries[i].Shaped {
			target, valid := text.CanonicalBlockRef(value)
			if valid && target.IssueKey == source.key && (target.ID == "spec" || target.ID == specSlug) && held[target.Block] {
				shaped = append(shaped, value)
			} else {
				fmt.Fprintf(out, "  retain inactive/non-spec citation %s in source prose/history, not an active shaped relation\n", value)
			}
		}
		entries[i].Shaped = shaped
	}
	var askIDs []string
	for _, entry := range entries {
		askIDs = append(askIDs, entry.Asks...)
	}
	rows, err := tx.Query(joined, `select `+AskColumns+` from asks a where a.issue_key=$1 and (
 a.id::text=any($2::text[]) or a.block_artifact_id=$3::uuid or
 (a.state='answered' and a.block_id is not null and exists (
 select 1 from document_block_texts t where t.artifact_id=$4::uuid and t.block_id=a.block_id))) order by a.id`, source.key, askIDs, nullableUUID(source.id), source.spec)
	if err != nil {
		return report, err
	}
	var asks []model.Ask
	for rows.Next() {
		ask, err := ScanAsk(rows)
		if err != nil {
			rows.Close()
			return report, err
		}
		asks = append(asks, ask)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}
	knownAsks := map[string]bool{}
	for _, ask := range asks {
		knownAsks[ask.ID] = true
	}
	for i := range entries {
		if entries[i].Drop {
			for _, id := range entries[i].Asks {
				if !knownAsks[id] {
					entries[i].Drop = false
					entries[i].Review = true
				}
			}
		}
	}
	restoredMarkdown := map[string]string{}
	changed := false
	additions := []string{}
	for _, ask := range asks {
		var shaped []string
		for _, entry := range entries {
			if entry.Drop && slices.Contains(entry.Asks, ask.ID) {
				for _, ref := range entry.Shaped {
					if !slices.Contains(shaped, ref) {
						shaped = append(shaped, ref)
					}
				}
			}
		}
		if ask.BlockID != nil && ask.BlockArtifactID != nil && *ask.BlockArtifactID == source.spec && held[*ask.BlockID] {
			if len(shaped) > 0 {
				for _, block := range specBlocks {
					if block.ID == *ask.BlockID {
						for _, attribute := range block.Attributes {
							if attribute.Name == "shaped" {
								for _, ref := range strings.Fields(attribute.Value) {
									if !slices.Contains(shaped, ref) {
										shaped = append(shaped, ref)
									}
								}
							}
						}
					}
				}
				fmt.Fprintf(out, "  relate ask %s -> spec blocks %s\n", ask.ID, strings.Join(shaped, " "))
				if apply {
					if err := s.SetBlockAttributes(joined, source.spec, *ask.BlockID, map[string]any{"shaped": shaped}, conversionActor); err != nil {
						return report, err
					}
					changed = true
				}
			}
			continue
		}
		// Move only the legacy source, a row-only cited ask, or a missing spec ask whose history
		// establishes provenance. A current ask in another document stays its source there.
		restore := ask.BlockArtifactID == nil || source.id != "" && ask.BlockArtifactID != nil && *ask.BlockArtifactID == source.id
		if ask.BlockID != nil && !held[*ask.BlockID] {
			var historical bool
			if err := tx.QueryRow(joined, `select exists(select 1 from document_block_texts where artifact_id=$1::uuid and block_id=$2)`, source.spec, *ask.BlockID).Scan(&historical); err != nil {
				return report, err
			}
			restore = restore || historical
		}
		if !restore {
			continue
		}
		id := "ask-" + ask.ID
		if ask.BlockID != nil {
			id = *ask.BlockID
		}
		if held[id] {
			return report, fmt.Errorf("cannot restore ask %s: spec block %s already has a different owner", ask.ID, id)
		}
		node, err := conversionAskNode(ask, id)
		if err != nil {
			return report, err
		}
		// A restatement's shaped edges move to the actual ask, never to a copy of its answer.
		if len(shaped) > 0 {
			node.Attrs["shaped"] = shaped
		}
		markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{node}})
		if err != nil {
			return report, err
		}
		restoredMarkdown[ask.ID] = markdown
		held[id] = true
		report.Restored++
		fmt.Fprintf(out, "  restore ask %s -> spec#b-%s (same ask row and answer)\n", ask.ID, id)
		if apply {
			if ask.BlockArtifactID != nil && *ask.BlockArtifactID != source.spec && *ask.BlockArtifactID != source.id {
				if _, err := s.ApplyOps(joined, *ask.BlockArtifactID, []model.EditOp{{Op: "delete", Block: id}}, conversionActor, nil); err != nil {
					return report, err
				}
				if _, err := s.conversionVersion(joined, tx, *ask.BlockArtifactID, "Restore the answered decision to its spec"); err != nil {
					return report, err
				}
			}
			if _, err := tx.Exec(joined, `update asks set block_artifact_id=$2::uuid,block_id=$3 where id=$1::uuid`, ask.ID, source.spec, id); err != nil {
				return report, err
			}
		}
	}
	for _, entry := range entries {
		if entry.Drop {
			for _, id := range entry.Asks {
				if markdown, found := restoredMarkdown[id]; found {
					additions = append(additions, markdown)
					delete(restoredMarkdown, id)
				}
			}
			report.Dropped++
			fmt.Fprintf(out, "  drop ask-only record %q (asks %s)\n", entry.Title, strings.Join(entry.Asks, ","))
			continue
		}
		if held[entry.ID] {
			return report, fmt.Errorf("spec already holds conversion block %s while source still exists; left untouched", entry.ID)
		}
		markdown, err := entry.Markdown(func(id string) string { return "dispatch://" + source.key + "/ask/" + id })
		if err != nil {
			return report, err
		}
		additions = append(additions, markdown)
		held[entry.ID] = true
		report.Moved++
		fmt.Fprintf(out, "  move %q -> spec#b-%s as agent's record\n", entry.Title, entry.ID)
		if entry.Review {
			report.Review++
			fmt.Fprintf(out, "    review attribution: narrative cites an ask; preserved, not presented as the person's words\n")
		}
	}
	for _, ask := range asks {
		if markdown, found := restoredMarkdown[ask.ID]; found {
			additions = append(additions, markdown)
		}
	}
	if source.id != "" {
		fmt.Fprintf(out, "  retire source as decision-history-%s.md (versions, reviews and closed conversations retained)\n", source.id)
	}
	if !apply {
		return report, nil
	}
	if len(additions) > 0 {
		if _, err := s.ApplyOps(joined, source.spec, []model.EditOp{{Op: "insert", After: "end", Markdown: strings.Join(additions, "\n\n")}}, conversionActor, nil); err != nil {
			return report, err
		}
		changed = true
	}
	if changed {
		version, err := s.conversionVersion(joined, tx, source.spec, "Decision nodes replace the separate decision document")
		if err != nil {
			return report, err
		}
		if err := moveConvertedDecisionKeeps(joined, tx, source, entries, legacyMarkdown, legacyBlocks, version.Version.Number); err != nil {
			return report, err
		}
	}
	if source.id != "" {
		history := "decision-history-" + source.id
		if _, err := tx.Exec(joined, `update artifacts set name=$2,slug=$3 where id=$1::uuid`, source.id, history+".md", history); err != nil {
			return report, err
		}
		if _, err := s.ReplaceText(joined, source.id, "Decision nodes are in dispatch://"+source.key+"/spec. Earlier versions retain the conversion history.\n", conversionActor); err != nil {
			return report, err
		}
		if _, err := s.conversionVersion(joined, tx, source.id, "Retire the separate decision document"); err != nil {
			return report, err
		}
	}
	return report, nil
}

var conversionActor = model.Actor{Kind: "system", ID: "decision-node-conversion"}

// Conversion is a document writer like the HTTP routes: append and stamp its version event in
// the same transaction, and let the joined ledger publish only after that transaction commits.
func (s *Service) conversionVersion(ctx context.Context, tx pgx.Tx, id, message string) (VersionResult, error) {
	written, err := s.NamedVersion(ctx, id, message, conversionActor)
	if err != nil {
		return VersionResult{}, err
	}
	owner, _, err := lockArtifactOwner(ctx, tx, id)
	if err != nil {
		return VersionResult{}, err
	}
	event, err := s.events.Append(ctx, tx, documentAskEvent(owner, id, "artifact.version", conversionActor, ArtifactVersionEventPayload(id, owner.Name, written.Version, nil, written.Changes)))
	if err != nil {
		return VersionResult{}, err
	}
	if err := refs.Stamp(ctx, tx, "artifact", id, event.ID); err != nil {
		return VersionResult{}, err
	}
	collectEvent(ctx, event)
	return written, nil
}

func nullableUUID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func conversionAskNode(ask model.Ask, id string) (*pmdoc.Node, error) {
	node := &pmdoc.Node{Type: "ask", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: id, "urgency": ask.Urgency, "multiple": ask.Multiple}, Children: []*pmdoc.Node{{Type: "paragraph"}}}
	children, err := askBlockChildren(askBlock{node: node}, AskBlockEdit{Question: &ask.Question, Options: &ask.Options}, pmdoc.NewWriteBudget())
	if err != nil {
		return nil, err
	}
	node.Children = children
	setAskServerAttributes(node, ask, "", false)
	pmdoc.EnsureBlockIDs(node)
	return node, nil
}

func conversionOpenConversations(ctx context.Context, tx pgx.Tx, artifactID string) ([]string, error) {
	rows, err := tx.Query(ctx, `select 'comment',id::text from comments where not resolved and (artifact_id=$1::uuid or anchor->>'artifact_id'=$1::text)
 union all select 'ask',id::text from asks where state='open' and (block_artifact_id=$1::uuid or anchor->>'artifact_id'=$1::text)`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var open []string
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			return nil, err
		}
		open = append(open, kind+" "+id)
	}
	return open, rows.Err()
}
