package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A reload cannot recover an unversioned pointer's introducing writer. Immutable history can
// prove a value was authorized before; otherwise settlement removes only the unproven attribute.
func loadedDecisionClaims(ctx context.Context, query Queryer, room string, tree *pmdoc.Node) (map[string]decisionClaim, error) {
	persons := decisionPersonClaims(tree)
	if len(persons) == 0 {
		return nil, nil
	}
	claims := make(map[string]decisionClaim, len(persons))
	for block, person := range persons {
		claims[block] = decisionClaim{person: person, lostAuthor: true}
	}
	recover := func(prior *pmdoc.Node) {
		for block, person := range decisionPersonClaims(prior) {
			if claim, exists := claims[block]; exists && claim.person == person {
				claims[block] = decisionClaim{person: person, authorized: true}
				delete(persons, block)
			}
		}
	}
	var markdown string
	err := query.QueryRow(ctx, `select markdown from artifact_versions where artifact_id=$1 order by number desc limit 1`, room).Scan(&markdown)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read reloaded decision attribution: %w", err)
	}
	if strings.Contains(markdown, "decided_by=") {
		prior, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			return nil, fmt.Errorf("read latest version's decision attribution: %w", err)
		}
		recover(prior)
	}
	if len(persons) == 0 {
		return claims, nil
	}
	// The block-text history records changed blocks, not a copy of the whole document at every
	// version. A join to immutable versions excludes an index row with no version behind it.
	blocks := make([]string, 0, len(persons))
	for block := range persons {
		blocks = append(blocks, block)
	}
	rows, err := query.Query(ctx, `
		select h.block_id, h.text from document_block_texts h
		join artifact_versions v on v.artifact_id=h.artifact_id and v.number=h.version
		where h.artifact_id=$1 and h.block_id=any($2::text[]) and position('decided_by=' in h.text)>0
	`, room, blocks)
	if err != nil {
		return nil, fmt.Errorf("read historical decision attribution: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var block, body string
		if err := rows.Scan(&block, &body); err != nil {
			return nil, err
		}
		if _, needed := persons[block]; !needed {
			continue
		}
		prior, err := pmdoc.ParseRendering(body)
		if err != nil {
			return nil, fmt.Errorf("read historical decision block %q: %w", block, err)
		}
		recover(prior)
	}
	return claims, rows.Err()
}

func (s *Service) lostDecisionClaims(room string, tree *pmdoc.Node) map[string]string {
	claims := decisionPersonClaims(tree)
	if len(claims) == 0 {
		return nil
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	for block, person := range claims {
		claim, exists := state.decisionClaims[block]
		if !exists || !claim.lostAuthor || claim.person != person {
			delete(claims, block)
		}
	}
	return claims
}

// dropDecisionClaims matches each planned value again, so a concurrent change to the pointer
// survives. record, when non-nil, receives only values this live repair actually removed.
func dropDecisionClaims(tree *pmdoc.Node, claims map[string]string, record map[string]string) bool {
	changed := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "decision" {
			block, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			person, _ := node.Attrs["decided_by"].(string)
			if person != "" && claims[block] == person {
				delete(node.Attrs, "decided_by")
				if record != nil {
					record[block] = person
				}
				changed = true
			}
		}
		return true
	})
	return changed
}
