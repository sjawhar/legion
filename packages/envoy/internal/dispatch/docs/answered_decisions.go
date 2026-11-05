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

func refuseRemovingAnsweredDecisions(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor, target *pmdoc.Node) error {
	if actor.Kind == "system" || actor.Kind == "user" && actor.Act == nil {
		return nil
	}
	rows, err := tx.Query(ctx, `select id::text, block_id from asks
 where block_artifact_id=$1::uuid and state='answered' and block_id is not null order by id`, artifactID)
	if err != nil {
		return err
	}
	defer rows.Close()
	held, _ := askNodes(target)
	for rows.Next() {
		var askID, blockID string
		if err := rows.Scan(&askID, &blockID); err != nil {
			return err
		}
		node, found := held[blockID]
		if !found || node.Attrs["copied_from"] != nil && node.Attrs["copied_from"] != "" {
			return &ErrAnsweredDecisionRemoved{AskID: askID, BlockID: blockID}
		}
	}
	return rows.Err()
}
