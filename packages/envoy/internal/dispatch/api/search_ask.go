package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// attachAskStates fills Ask on every ask hit of results, in place: where the ask stands, its
// latest answer and its resolution. It reads the hits' rows by primary key after whatever query
// produced them, so it serves any result list (search's, the related-item check's) without a
// column in the shared search SQL, whose rows carry an issue key in place of a uuid for issue
// hits. A list with no ask hit costs nothing.
//
// asks.answer is always the latest answer, since a re-answer overwrites it. A resolved ask holds
// no answer on its row, but one that settlement retracted while it was answered still has its
// person's answer in its ask.answered events, so for resolved hits alone it reads those
// (loadAskAnswersByAsk) and keeps the last. An ask a concurrent delete removed between the search
// and this read keeps Ask nil, so nothing claims a state that was not read.
func (s *server) attachAskStates(ctx context.Context, q queryer, results []model.SearchResult) error {
	var ids []string
	for _, result := range results {
		if result.Kind == "ask" {
			ids = append(ids, result.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `select id::text, state, answer, resolution from asks where id = any($1::uuid[])`, ids)
	if err != nil {
		return fmt.Errorf("load ask states: %w", err)
	}
	defer rows.Close()
	states := make(map[string]*model.SearchAsk, len(ids))
	var resolved []string
	for rows.Next() {
		var id string
		var answer, resolution []byte
		state := &model.SearchAsk{}
		if err := rows.Scan(&id, &state.State, &answer, &resolution); err != nil {
			return fmt.Errorf("scan ask state: %w", err)
		}
		if len(answer) > 0 {
			var value model.AskAnswer
			if err := json.Unmarshal(answer, &value); err != nil {
				return fmt.Errorf("decode ask answer: %w", err)
			}
			state.Answer = &value
		}
		if len(resolution) > 0 {
			var value model.AskResolution
			if err := json.Unmarshal(resolution, &value); err != nil {
				return fmt.Errorf("decode ask resolution: %w", err)
			}
			state.Resolution = &value
		}
		if state.State == "resolved" {
			resolved = append(resolved, id)
		}
		states[id] = state
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load ask states: %w", err)
	}
	rows.Close()
	if len(resolved) > 0 {
		answers, err := s.loadAskAnswersByAsk(ctx, q, resolved)
		if err != nil {
			return err
		}
		for _, id := range resolved {
			if history := answers[id]; len(history) > 0 {
				last := history[len(history)-1]
				states[id].Answer = &last
			}
		}
	}
	for index := range results {
		if results[index].Kind != "ask" {
			continue
		}
		state, ok := states[results[index].ID]
		if !ok {
			continue
		}
		// Selected is never null on the wire: no option chosen is an empty list.
		if state.Answer != nil && state.Answer.Selected == nil {
			state.Answer.Selected = []string{}
		}
		results[index].Ask = state
	}
	return nil
}
