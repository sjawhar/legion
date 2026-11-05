package api

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// decisionDocumentsQuery is a named, issue-scoped graph query. The block type selects the
// decision nodes; part_of selects their documents. It never follows a mention into a different
// issue, project document or direct message. The graph remains a view of the existing index.
const decisionDocumentsQuery = `
 select distinct a.id::text, a.is_primary, a.created_at
 from artifacts a
 join document_blocks b on b.artifact_id = a.id and b.type in ('decision','ask')
 join lateral (
   select 1 from graph_edges g
   where g.from_kind = 'block' and g.from_id = b.node_id
     and g.kind = 'part_of' and g.to_kind = 'artifact' and g.to_id = a.ref_key
   limit 1
 ) member on true
 where a.issue_key = $1 and a.kind = 'doc' and a.name <> 'decision-history-'||a.id::text||'.md'
 order by a.is_primary desc, a.created_at, a.id::text`

func (s *server) issueDecisionQuestions(ctx context.Context, key string) ([]issueAskRow, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select `+askReadColumns+` `+askReadFrom+`
    where a.issue_key = $1 and a.state = 'open' and exists (
      select 1 from artifacts d join document_blocks b on b.artifact_id=d.id
      where d.issue_key=$1 and d.kind='doc' and d.id=a.block_artifact_id
        and b.block_id=a.block_id and b.type='ask'
    )
		order by a.created_at, a.id
  `, key)
	if err != nil {
		return nil, err
	}
	questions := []issueAskRow{}
	for rows.Next() {
		ask, reply, err := scanAskRead(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		questions = append(questions, issueAskRow{Ask: ask, LastReply: reply})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	asks := make([]*model.Ask, len(questions))
	for index := range questions {
		asks[index] = &questions[index].Ask
	}
	if err := s.attachOpenedEventIDs(ctx, s.deps.Store.Pool, asks); err != nil {
		return nil, err
	}
	if err := attachAskBacklinkCounts(ctx, s.deps.Store.Pool, asks); err != nil {
		return nil, err
	}
	return questions, nil
}

func (s *server) issueDecisionNodes(ctx context.Context, key string) (issueDecisions, error) {
	answer := issueDecisions{OpenQuestions: []issueAskRow{}, Decisions: []model.Decision{}}
	questions, err := s.issueDecisionQuestions(ctx, key)
	if err != nil {
		return answer, err
	}
	answer.OpenQuestions = questions
	rows, err := s.deps.Store.Pool.Query(ctx, decisionDocumentsQuery, key)
	if err != nil {
		return answer, err
	}
	var ids []string
	for rows.Next() {
		var id string
		var primary bool
		var at time.Time
		if err := rows.Scan(&id, &primary, &at); err != nil {
			rows.Close()
			return answer, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return answer, err
	}
	spec, found, err := s.issueSpecDocument(ctx, key)
	if err != nil {
		return answer, err
	}
	specID := ""
	if found {
		specID = spec.ID
	}
	for _, id := range ids {
		document, err := s.loadArtifact(ctx, s.deps.Store.Pool, id)
		if err != nil {
			return answer, err
		}
		reference := artifactReference(document)
		decisions, err := s.readDecisionNodes(ctx, document, reference, specID)
		if err != nil {
			return answer, err
		}
		answer.Decisions = append(answer.Decisions, decisions...)
		if answer.Document == nil && len(decisions) > 0 {
			answer.Document = &reference
		}
	}
	return answer, nil
}

// readDecisionNodes reads each typed node's own text. Only the ask row can supply a person's
// decision: from_ask is provenance for an agent's choice, never permission to restate an answer.
func (s *server) readDecisionNodes(ctx context.Context, document model.Artifact, reference, specID string) ([]model.Decision, error) {
	markdown, blocks, err := s.deps.Docs.TextWithBlocks(ctx, document.ID)
	if err != nil {
		return nil, err
	}
	changes, err := refs.BlockChanges(ctx, s.deps.Store.Pool, document.ID)
	if err != nil {
		return nil, err
	}
	keeps, err := decisionKeeps(ctx, s.deps.Store.Pool, document.ID)
	if err != nil {
		return nil, err
	}
	var links []model.BlockLink
	if specID != "" {
		links, err = refs.BlockLinks(ctx, s.deps.Store.Pool, document.ID, specID)
		if err != nil {
			return nil, err
		}
		removed, err := s.removedDecisionPassages(ctx, document.ID, specID)
		if err != nil {
			return nil, err
		}
		links = append(links, removed...)
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `select `+askReadColumns+` `+askReadFrom+` where a.block_artifact_id=$1::uuid and a.state in ('answered','resolved')`, document.ID)
	if err != nil {
		return nil, err
	}
	asks := map[string]model.Ask{}
	for rows.Next() {
		ask, _, err := scanAskRead(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if ask.BlockID != nil {
			asks[*ask.BlockID] = ask
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].From < blocks[j].From })
	decisions := []model.Decision{}
	var agentNodes []int
	for _, block := range blocks {
		if block.Type != "decision" && block.Type != "ask" {
			continue
		}
		decision := model.Decision{BlockID: block.ID, Ref: refs.BlockRef(reference, block.ID), Kind: "agent", Asks: []string{}, Links: []model.DecisionLink{}}
		if change, ok := changes[block.ID]; ok {
			at := change.At
			decision.ChangedVersion = change.Version
			decision.ChangedAt = &at
		}
		if block.Type == "ask" {
			ask, found := asks[block.ID]
			if !found {
				continue
			}
			if ask.State == "answered" && ask.Answer != nil {
				decision.Kind = "person"
			} else if ask.State == "resolved" {
				decision.Replaced = true
				if ask.Resolution != nil && ask.Resolution.Actor.Kind == "user" {
					decision.Kind = "person"
				}
			} else {
				continue
			}
			decision.Heading = ask.Question
			decision.Asks = []string{ask.ID}
			decision.Text = askDecisionText(ask)
		} else {
			decision.Heading = blockAttribute(block, "title")
			decision.Replaced = blockAttribute(block, "replaced") == "true"
			decision.Text = decisionNodeText(sliceMarkdown(markdown, block.From, block.To))
			if decision.Heading == "" {
				decision.Heading = strings.SplitN(decision.Text, "\n", 2)[0]
			}
			decision.Uncertainty = blockAttribute(block, "uncertainty")
			decision.UncertaintyReason = blockAttribute(block, "uncertainty_reason")
			decision.Unsure = decision.Uncertainty != "" ||
				strings.HasPrefix(decision.Text, "Unsure:") || strings.Contains(decision.Text, "\n\nUnsure:")
			if keep, ok := keeps[block.ID]; ok {
				decision.Kept = &keep
			}
		}
		var pins map[string]int
		for _, value := range strings.Fields(blockAttribute(block, "shaped")) {
			if target, valid := text.CanonicalBlockRef(value); valid && target.Pin > 0 {
				if pins == nil {
					pins = map[string]int{}
				}
				pins[text.TargetID(target)] = target.Pin
			}
		}
		for _, link := range links {
			if link.Kind == "shaped" && link.From.BlockID == block.ID {
				target := link.To
				if version := pins[target.ID]; version > 0 {
					target.Pin = &model.BlockPin{Version: version}
				}
				decision.Links = append(decision.Links, model.DecisionLink{FromBlock: block.ID, To: target, CreatedVersion: link.CreatedVersion, CreatedAt: link.CreatedAt})
			}
		}
		decision.TextSHA256 = decisionTextSum(decision.Text)
		decision.WaitingReview = decision.Kind == "agent" && !decision.Replaced && (decision.Kept == nil || decision.Kept.TextSHA256 != decision.TextSHA256)
		if block.Type == "decision" {
			agentNodes = append(agentNodes, len(decisions))
		}
		decisions = append(decisions, decision)
	}
	if err := s.markNodeLinksStale(ctx, document.ID, decisions, agentNodes); err != nil {
		return nil, err
	}
	return decisions, nil
}

func blockAttribute(block model.ArtifactBlock, name string) string {
	for _, attribute := range block.Attributes {
		if attribute.Name == name {
			return attribute.Value
		}
	}
	return ""
}

func askDecisionText(ask model.Ask) string {
	var body strings.Builder
	body.WriteString(ask.Question)
	for _, option := range ask.Options {
		body.WriteString("\n\n- " + option.Label)
		if option.Description != "" {
			body.WriteString(": " + option.Description)
		}
	}
	if ask.Answer != nil {
		if len(ask.Answer.Selected) > 0 {
			body.WriteString("\n\nSelected: " + strings.Join(ask.Answer.Selected, ", "))
		}
		if ask.Answer.Text != nil {
			body.WriteString("\n\n" + *ask.Answer.Text)
		}
	}
	if ask.State == "resolved" && ask.Resolution != nil {
		body.WriteString("\n\nClosed: " + ask.Resolution.Reason)
	}
	return body.String()
}

// decisionNodeText unwraps the typed block, not a heading section. The heading remains in the
// response text for compatibility, and every byte of the node's body is retained.
func decisionNodeText(markdown string) string {
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil || len(tree.Children) != 1 || tree.Children[0].Type != "decision" {
		return strings.TrimSpace(markdown)
	}
	node := tree.Children[0]
	pmdoc.Walk(node, func(child *pmdoc.Node) bool {
		kept := child.Children[:0]
		for _, descendant := range child.Children {
			if descendant.Type != "interpretation" {
				kept = append(kept, descendant)
			}
		}
		child.Children = kept
		return true
	})
	body, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: node.Children})
	if err != nil {
		return strings.TrimSpace(markdown)
	}
	title, _ := node.Attrs["title"].(string)
	body = strings.TrimRight(body, "\n")
	if title != "" {
		prefix := "## "
		if node.Attrs["replaced"] == true {
			prefix = "### "
		}
		if body == "" {
			return prefix + title
		}
		return prefix + title + "\n\n" + body
	}
	return body
}

// The same text-at-two-instants rule as Keep's legacy read, now keyed by stable block identity.
// One batched query reads all link histories of this document, independent of its version count.
func (s *server) markNodeLinksStale(ctx context.Context, artifactID string, decisions []model.Decision, agentNodes []int) error {
	var ids []string
	var versions []int
	var times []time.Time
	type at struct{ decision, link int }
	var places []at
	for _, i := range agentNodes {
		decision := decisions[i]
		for j, link := range decision.Links {
			if link.To.ChangedAt == nil {
				continue
			}
			ids = append(ids, decision.BlockID)
			versions = append(versions, link.CreatedVersion)
			times = append(times, *link.To.ChangedAt)
			places = append(places, at{i, j})
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
 select input.ord, first.text, latest.number, changed.text,
   exists(select 1 from document_indexed_versions indexed where indexed.artifact_id=$1::uuid and indexed.version=latest.number)
 from unnest($2::text[],$3::int[],$4::timestamptz[]) with ordinality input(block,version,at,ord)
 left join lateral (select t.text from document_block_texts t where t.artifact_id=$1::uuid and t.block_id=input.block and t.version<=input.version order by t.version desc limit 1) first on true
 left join lateral (select v.number from artifact_versions v where v.artifact_id=$1::uuid and v.created_at<=input.at order by v.number desc limit 1) latest on true
 left join lateral (select t.text from document_block_texts t where t.artifact_id=$1::uuid and t.block_id=input.block and t.version<=latest.number order by t.version desc limit 1) changed on true
 `, artifactID, ids, versions, times)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ord int
		var first, changed *string
		var version *int
		var indexed bool
		if err := rows.Scan(&ord, &first, &version, &changed, &indexed); err != nil {
			return err
		}
		place := places[ord-1]
		decision := &decisions[place.decision]
		if first == nil || decisionNodeText(*first) == decision.Text {
			continue
		}
		decision.Links[place.link].Stale = version == nil || indexed && (changed == nil || decisionNodeText(*changed) != decision.Text)
	}
	return rows.Err()
}
