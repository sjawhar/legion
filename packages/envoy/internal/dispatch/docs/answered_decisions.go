package docs

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// ErrAnsweredDecisionRemoved names the person's answer an agent's document write would hide.
// The joined write has not recorded its fork, so refusing leaves both the document and row alone.
type ErrAnsweredDecisionRemoved struct {
	AskID   string
	BlockID string
}

func (e *ErrAnsweredDecisionRemoved) Error() string {
	return fmt.Sprintf("ask %s is answered; an agent cannot remove its decision block %q from this document", e.AskID, e.BlockID)
}

type ErrDecisionAttribution struct {
	BlockID string
	Decider string
}

func (e *ErrDecisionAttribution) Error() string {
	return fmt.Sprintf("block %q has unauthorized attribution to %q. Decisions may name only their writing person. Answered asks must match their authoritative rows", e.BlockID, e.Decider)
}

func refuseDecisionPersonClaim(target *pmdoc.Node, actor model.Actor) error {
	// A settlement without one credited writer represents no person's authority.
	if actor.Kind == "system" && actor.ID != SettlementActor.ID {
		return nil
	}
	login := ""
	if actor.Kind == "user" && actor.Act == nil {
		login = model.CanonicalLogin(actor.ID)
	}
	var attribution *ErrDecisionAttribution
	pmdoc.Walk(target, func(node *pmdoc.Node) bool {
		if node.Type == "decision" {
			decider, _ := node.Attrs["decided_by"].(string)
			if decider != "" && decider != login {
				block, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				attribution = &ErrDecisionAttribution{BlockID: block, Decider: decider}
			}
		}
		return attribution == nil
	})
	if attribution != nil {
		return attribution
	}
	return nil
}

// A version may read a browser update after ask reconciliation. Only the ask row, not that
// update's server-owned attributes, can establish who answered a block or its copied source.
func refuseUnbackedAnsweredAttribution(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node) error {
	var blocks, questions []string
	var copied []uuid.UUID
	var claimed map[string]string
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" || node.Attrs["state"] != "answered" {
			return true
		}
		author, _ := node.Attrs["answered_by"].(string)
		if author == "" {
			return true
		}
		block, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		source, _ := node.Attrs["copied_from"].(string)
		question := ""
		var sourceID uuid.UUID
		if source != "" {
			sourceID, _ = uuid.Parse(source)
			if parsed, err := parseAskBlock(node); err == nil {
				question = parsed.question
			}
		}
		blocks, copied, questions = append(blocks, block), append(copied, sourceID), append(questions, question)
		if claimed == nil {
			claimed = make(map[string]string)
		}
		claimed[block] = model.CanonicalLogin(author)
		return true
	})
	if len(blocks) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		select b.block_id, coalesce(case when own.state='answered' then own.answer->>'user' end, source.answer->>'user', '')
		from unnest($2::text[], $3::uuid[], $4::text[]) b(block_id, copied_from, question)
		join artifacts writing on writing.id=$1
		left join asks own on own.block_artifact_id=$1 and own.block_id=b.block_id
		left join asks source on own.id is null and source.state='answered'
			and source.id=b.copied_from and source.block_id=b.block_id and source.question=b.question
			and exists (
				select 1 from artifacts d where d.id=source.block_artifact_id
				and d.issue_key is not distinct from writing.issue_key
				and d.project_key is not distinct from writing.project_key
			)
	`, artifactID, blocks, copied, questions)
	if err != nil {
		return fmt.Errorf("read version's authoritative answer attribution: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var block, author string
		if err := rows.Scan(&block, &author); err != nil {
			return err
		}
		if author == "" || model.CanonicalLogin(author) != claimed[block] {
			return &ErrDecisionAttribution{BlockID: block, Decider: claimed[block]}
		}
	}
	return rows.Err()
}

func refuseAgentDecisionChanges(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor, target *pmdoc.Node) error {
	if err := refuseDecisionPersonClaim(target, actor); err != nil {
		return err
	}
	if actor.Kind == "system" || actor.Kind == "user" && actor.Act == nil {
		return nil
	}
	rows, err := tx.Query(ctx, `select id::text, block_id from asks
 where block_artifact_id=$1::uuid and state='answered' and block_id is not null order by id`, artifactID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var held map[string]*pmdoc.Node
	for rows.Next() {
		if held == nil {
			held, _ = askNodes(target)
		}
		var askID, blockID string
		if err := rows.Scan(&askID, &blockID); err != nil {
			return err
		}
		if _, found := held[blockID]; !found {
			return &ErrAnsweredDecisionRemoved{AskID: askID, BlockID: blockID}
		}
	}
	return rows.Err()
}
