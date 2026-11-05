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

func refuseDecisionPersonClaim(target, prior *pmdoc.Node, actor model.Actor) error {
	// A settlement without one credited writer represents no person's authority.
	if actor.Kind == "system" && actor.ID != SettlementActor.ID {
		return nil
	}
	login := ""
	if actor.Kind == "user" && actor.Act == nil {
		login = model.CanonicalLogin(actor.ID)
	}
	held := decisionPersonClaims(prior)
	var attribution *ErrDecisionAttribution
	pmdoc.Walk(target, func(node *pmdoc.Node) bool {
		if node.Type == "decision" {
			decider, _ := node.Attrs["decided_by"].(string)
			block, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			if decider != "" && decider != held[block] && model.CanonicalLogin(decider) != login {
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

func refuseAgentDecisionChanges(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor, target, prior *pmdoc.Node) error {
	if err := refuseDecisionPersonClaim(target, prior, actor); err != nil {
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

// decisionPersonClaims retains only the person pointers, not the rest of a live tree.
func decisionPersonClaims(tree *pmdoc.Node) map[string]string {
	var claims map[string]string
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "decision" {
			if decider, _ := node.Attrs["decided_by"].(string); decider != "" {
				if claims == nil {
					claims = make(map[string]string)
				}
				block, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				claims[block] = decider
			}
		}
		return true
	})
	return claims
}

type decisionClaim struct {
	person     string
	authorized bool
}

// A browser claim is authorized by the update that introduced its value, never by a later
// editor. Published request writes have already checked that update's introduced claims.
func (s *Service) observeDecisionClaims(room string, claims map[string]string, origin any) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	_, published := origin.(*liveWriteOrigin)
	writer, ambiguous := soleConnectedActor(state.connected)
	if captured, exists := state.decisionOrigins[origin]; exists {
		writer, ambiguous = captured, captured == nil
	}
	if len(claims) == 0 {
		state.decisionClaims = nil
		return
	}
	if state.decisionClaims == nil {
		state.decisionClaims = make(map[string]decisionClaim, len(claims))
	}
	for block := range state.decisionClaims {
		if _, held := claims[block]; !held {
			delete(state.decisionClaims, block)
		}
	}
	for block, person := range claims {
		if previous, held := state.decisionClaims[block]; held && previous.person == person {
			continue
		}
		authorized := published || !s.originCreditsNoOne(origin) && !ambiguous && writer != nil &&
			writer.Kind == "user" && writer.Act == nil && model.CanonicalLogin(writer.ID) == model.CanonicalLogin(person)
		state.decisionClaims[block] = decisionClaim{person: person, authorized: authorized}
	}
}

func introducedDecisionClaims(target, prior *pmdoc.Node) map[string]string {
	claims, held := decisionPersonClaims(target), decisionPersonClaims(prior)
	for block, person := range claims {
		if held[block] == person {
			delete(claims, block)
		}
	}
	return claims
}

// An immutable version may include browser content a request did not introduce. Its room's
// per-value authorization is required unless this transaction itself validated that value.
func (s *Service) refuseVersionDecisionClaims(ctx context.Context, artifactID string, target, prior *pmdoc.Node, approved map[string]string) error {
	claims := introducedDecisionClaims(target, prior)
	if len(claims) == 0 {
		return nil
	}
	if ledger := ledgerFrom(ctx); ledger != nil {
		if write := ledger.liveWriteFor(artifactID); write != nil {
			for block, person := range claims {
				if write.decisionClaims[block] == person {
					delete(claims, block)
				}
			}
		}
	}
	state := s.lockState(artifactID)
	defer s.unlockState(artifactID, state)
	for block, person := range claims {
		if approved[block] == person {
			continue
		}
		claim, found := state.decisionClaims[block]
		if !found || claim.person != person || !claim.authorized {
			return &ErrDecisionAttribution{BlockID: block, Decider: person}
		}
	}
	return nil
}

// A version without pointers needs no prior pointer baseline. Some historic renderings do not
// read back; when a pointer comparison is needed, report that parse failure instead of treating
// every existing pointer as newly introduced.
func decisionPriorTree(markdown string, target *pmdoc.Node) (*pmdoc.Node, error) {
	if len(pmdoc.BlockPointers(target)) == 0 {
		return nil, nil
	}
	return pmdoc.ParseRendering(markdown)
}
