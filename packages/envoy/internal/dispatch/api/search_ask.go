package api

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// attachAskStates sets Ask, in place, on every ask hit of a search page: the ask's state, its
// answer and its resolution, read from its row through docs.ScanAsk. A resolved ask's
// answer is the last one its ask.answered events recorded (loadAskAnswersByAsk), since a row
// that document settlement retracted while it was answered keeps its answer nowhere else; those
// events are read only when the page holds a resolved ask. One query reads every ask hit's row,
// and a page with no ask hit reads nothing. An ask removed after the search ran has no row left
// and keeps Ask nil.
func (s *server) attachAskStates(ctx context.Context, q queryer, results []model.SearchResult) error {
	indexByID := map[string]int{}
	var askIDs []string
	for index := range results {
		if results[index].Kind == "ask" {
			indexByID[results[index].ID] = index
			askIDs = append(askIDs, results[index].ID)
		}
	}
	if len(askIDs) == 0 {
		return nil
	}

	rows, err := q.Query(ctx, `select `+docs.AskColumns+` from asks a where a.id = any($1::uuid[])`, askIDs)
	if err != nil {
		return fmt.Errorf("load search ask states: %w", err)
	}
	defer rows.Close()
	var resolvedIDs []string
	for rows.Next() {
		ask, err := docs.ScanAsk(rows)
		if err != nil {
			return fmt.Errorf("scan search ask state: %w", err)
		}
		if ask.State == "resolved" {
			resolvedIDs = append(resolvedIDs, ask.ID)
		}
		results[indexByID[ask.ID]].Ask = &model.SearchAsk{State: ask.State, Answer: ask.Answer, Resolution: ask.Resolution}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load search ask states: %w", err)
	}

	if len(resolvedIDs) > 0 {
		answers, err := s.loadAskAnswersByAsk(ctx, q, resolvedIDs)
		if err != nil {
			return fmt.Errorf("load search ask answers: %w", err)
		}
		for _, id := range resolvedIDs {
			if history := answers[id]; len(history) > 0 {
				results[indexByID[id]].Ask.Answer = &history[len(history)-1]
			}
		}
	}

	// An answer that chose no option reads as an empty list, never null: a row or an event
	// written with no selected stores null there.
	for _, id := range askIDs {
		if state := results[indexByID[id]].Ask; state != nil && state.Answer != nil && state.Answer.Selected == nil {
			state.Answer.Selected = []string{}
		}
	}
	return nil
}
