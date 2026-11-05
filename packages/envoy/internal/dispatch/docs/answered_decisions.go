package docs

import (
	"context"
	"fmt"

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
	return fmt.Sprintf("decision block %q claims decided_by=%q; an agent cannot name a person as its decider, and a person's decision is their answered ask", e.BlockID, e.Decider)
}

func refuseAgentDecisionChanges(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor, target *pmdoc.Node) error {
	if actor.Kind == "system" || actor.Kind == "user" && actor.Act == nil {
		return nil
	}
	var attribution *ErrDecisionAttribution
	pmdoc.Walk(target, func(node *pmdoc.Node) bool {
		if node.Type == "decision" {
			decider, _ := node.Attrs["decided_by"].(string)
			if decider != "" {
				block, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				attribution = &ErrDecisionAttribution{BlockID: block, Decider: decider}
			}
		}
		return attribution == nil
	})
	if attribution != nil {
		return attribution
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
