package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

type askTransition struct {
	EventType string
	Apply     func(context.Context, pgx.Tx, model.Ask) (model.Ask, error)
	// After runs once the transition's own event is appended, still inside the
	// transaction; the events it returns are published with it.
	After func(context.Context, pgx.Tx, model.Ask) ([]model.Event, error)
}

// answerRevision is the question revision a human reviewed before choosing an answer.
// A nil EditedAt represents an ask that had not yet been edited.
type answerRevision struct {
	EditedAt *string
}

// answerTransition records a human answer. Approval asks have their review options and always name
// the document's latest settled version, since every version write moves the open request to it
// (docs.MoveApprovalAsk) and stamps edited_at, so revision, which the answer route requires, refuses
// an answer to a version the human was not shown (ASK_EDITED). Questions accept their configured
// options.
func answerTransition(
	actor model.Actor,
	selected []string,
	text *string,
	revision *answerRevision,
	writeBlock func(context.Context, pgx.Tx, model.Ask, model.AskAnswer) error,
) askTransition {
	// A block ask's answer is written into its document, where text reaches the parser with line
	// feeds alone (pmdoc.LineFeeds); the ask's row takes the same text, so settlement, which writes
	// the row's answer onto the block, agrees with it.
	if text != nil {
		lined := pmdoc.LineFeeds(*text)
		text = &lined
	}
	return askTransition{
		EventType: "ask.answered",
		Apply: func(ctx context.Context, tx pgx.Tx, ask model.Ask) (model.Ask, error) {
			if revision != nil && !stringPointersEqual(revision.EditedAt, ask.EditedAt) {
				return model.Ask{}, errorf(
					http.StatusConflict,
					"ASK_EDITED",
					"the question changed after you reviewed it; review the latest version and confirm your answer",
				)
			}
			hasText := text != nil && strings.TrimSpace(*text) != ""
			switch ask.Kind {
			case "approval":
				if _, _, err := reviewFromAnswer(selected, text); err != nil {
					return model.Ask{}, err
				}
			default:
				if !ask.Multiple && len(selected) > 1 {
					return model.Ask{}, errorf(http.StatusBadRequest, "INVALID_ANSWER", "single-select asks accept at most one selected answer")
				}
				if len(selected) > 0 && !selectedOptions(ask.Options, selected) {
					return model.Ask{}, errorf(http.StatusBadRequest, "INVALID_ANSWER", "selected answers must be ask option labels")
				}
				if len(selected) == 0 && !hasText {
					return model.Ask{}, errorf(http.StatusBadRequest, "INVALID_ANSWER", "answer requires a selected option or free-text answer")
				}
			}
			answer := model.AskAnswer{User: actor.ID, Selected: selected, Text: text, At: time.Now().UTC()}
			if writeBlock != nil {
				if err := writeBlock(ctx, tx, ask, answer); err != nil {
					return model.Ask{}, err
				}
			}
			answerJSON, err := encodeJSON(answer)
			if err != nil {
				return model.Ask{}, err
			}
			if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, ask.ID, answerJSON); err != nil {
				return model.Ask{}, err
			}
			ask.State = "answered"
			ask.Answer = &answer
			return ask, nil
		},
	}
}

func (s *server) answerAsk(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "ask") {
		return
	}
	var input struct {
		Selected         []string        `json:"selected"`
		Text             *string         `json:"text"`
		ExpectedEditedAt json.RawMessage `json:"expected_edited_at"`
		Actor            *model.Actor    `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.ExpectedEditedAt == nil {
		writeError(w, "ASK_REVISION_REQUIRED", http.StatusBadRequest, "expected_edited_at is required")
		return
	}
	var expectedEditedAt *string
	if err := json.Unmarshal(input.ExpectedEditedAt, &expectedEditedAt); err != nil {
		writeError(w, "INVALID_ANSWER", http.StatusBadRequest, "expected_edited_at must be an RFC3339 timestamp or null")
		return
	}
	transition := answerTransition(
		actor,
		input.Selected,
		input.Text,
		&answerRevision{EditedAt: expectedEditedAt},
		func(ctx context.Context, tx pgx.Tx, ask model.Ask, answer model.AskAnswer) error {
			if ask.BlockID == nil {
				return nil
			}
			blockArtifact, err := blockArtifactOf(ask)
			if err != nil {
				return err
			}
			attributes := map[string]any{
				"state":       "answered",
				"answered_by": answer.User,
				"answered_at": timestampValue(answer.At),
				"selected":    answer.Selected,
			}
			if answer.Text != nil {
				attributes["answer"] = *answer.Text
			}
			return s.deps.Docs.SetBlockAttributes(ctx, blockArtifact, *ask.BlockID, attributes, actor)
		},
	)
	// An approval ask's answer is a review of the document it names, pinned to the version its
	// question named: the latest settled one, which every version write moves the request to, and
	// the one the human reviewed (revision).
	transition.After = func(ctx context.Context, tx pgx.Tx, ask model.Ask) ([]model.Event, error) {
		if ask.Kind != "approval" {
			return nil, nil
		}
		state, reason, err := reviewFromAnswer(input.Selected, input.Text)
		if err != nil {
			return nil, err
		}
		artifact, err := s.loadArtifact(ctx, tx, ask.Approval.ArtifactID)
		if err != nil {
			return nil, err
		}
		_, event, err := s.writeReview(ctx, tx, artifact, ask.Approval.Version, state, actor, reason, new(ask.ID))
		if err != nil {
			return nil, err
		}
		return []model.Event{event}, nil
	}
	ask, err := s.closeAsk(r.Context(), r.PathValue("id"), actor, transition)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, ask)
}

func (s *server) resolveAsk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	if !requireUUIDPath(w, r, "ask") {
		return
	}
	var input struct {
		Kind   string       `json:"kind"`
		Reason string       `json:"reason"`
		Actor  *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	kind := strings.TrimSpace(input.Kind)
	reason := strings.TrimSpace(input.Reason)
	if (kind != "retracted" && kind != "resolved") || reason == "" {
		writeError(w, "INVALID_RESOLUTION", http.StatusBadRequest, "resolution requires a kind and reason")
		return
	}
	// Settlement's own retraction reason is how a resolution written before this rule is
	// recognised as settlement's, and such a resolution is restored when its block comes back.
	// A caller writing that reason would have their retract undone, so it is not theirs to
	// write.
	if strings.HasPrefix(reason, docs.SettlementRetractionReason) {
		writeError(w, "INVALID_RESOLUTION", http.StatusBadRequest,
			"reason may not begin with "+strconv.Quote(docs.SettlementRetractionReason)+", which marks a retraction the document's settlement wrote")
		return
	}
	ask, err := s.closeAsk(r.Context(), r.PathValue("id"), actor, askTransition{
		EventType: "ask.resolved",
		Apply: func(ctx context.Context, tx pgx.Tx, ask model.Ask) (model.Ask, error) {
			// A block ask's closed state belongs in its block too, written by whoever closed
			// it: left to settlement, the repair lands on whoever next touches the document.
			if ask.BlockID != nil {
				blockArtifact, err := blockArtifactOf(ask)
				if err != nil {
					return model.Ask{}, err
				}
				if err := s.deps.Docs.SetBlockAttributes(ctx, blockArtifact, *ask.BlockID, map[string]any{
					"state": "resolved",
				}, actor); err != nil {
					return model.Ask{}, err
				}
			}
			return docs.WriteAskResolution(ctx, tx, ask, kind, reason, actor)
		},
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, ask)
}

func (s *server) closeAsk(ctx context.Context, id string, actor model.Actor, transition askTransition) (model.Ask, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Ask{}, err
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	ask, events, err := s.closeAskTx(documentCtx, tx, id, actor, transition)
	if err != nil {
		return model.Ask{}, err
	}
	if err := ledger.Commit(ctx); err != nil {
		return model.Ask{}, err
	}
	s.publish(events...)
	return ask, nil
}

// closeAskTx applies a transition to an open ask inside the caller's transaction, appends the
// transition's event and runs its After; the caller commits and publishes the events.
func (s *server) closeAskTx(ctx context.Context, tx pgx.Tx, id string, actor model.Actor, transition askTransition) (model.Ask, []model.Event, error) {
	ask, err := s.transitionAskTx(ctx, tx, id, transition)
	if err != nil {
		return model.Ask{}, nil, err
	}
	// A transition writes no question text, so it moves no references and says so.
	event, err := s.appendEvent(ctx, tx, ownerOf(ask.IssueKey, ask.ArtifactID).event(
		transition.EventType, actor, model.NewAskEventPayload(ask, model.ReferenceChanges{}),
	))
	if err != nil {
		return model.Ask{}, nil, err
	}
	events := []model.Event{event}
	if transition.After != nil {
		more, err := transition.After(ctx, tx, ask)
		if err != nil {
			return model.Ask{}, nil, err
		}
		events = append(events, more...)
	}
	return ask, events, nil
}

// transitionAskTx locks an open ask and applies a transition inside the caller's
// transaction; it appends no event.
func (s *server) transitionAskTx(ctx context.Context, tx pgx.Tx, id string, transition askTransition) (model.Ask, error) {
	unlockedAsk, err := s.loadAsk(ctx, tx, id)
	if err != nil {
		return model.Ask{}, err
	}
	if err := s.requireOpenOwner(ctx, tx, ownerOf(unlockedAsk.IssueKey, unlockedAsk.ArtifactID)); err != nil {
		return model.Ask{}, err
	}
	ask, err := s.lockAskForTransition(ctx, tx, id)
	if err != nil {
		return model.Ask{}, err
	}
	switch ask.State {
	case "open":
	case "answered":
		if transition.EventType == "ask.answered" {
			return model.Ask{}, errorf(http.StatusConflict, "ASK_CLOSED", "ask is already answered")
		}
		return model.Ask{}, errorf(http.StatusConflict, "ASK_ANSWERED", "ask is already answered")
	case "resolved":
		return model.Ask{}, errorf(http.StatusConflict, "ASK_RESOLVED", "ask is already resolved")
	default:
		return model.Ask{}, errorf(http.StatusInternalServerError, "ASK_STATE_INVALID", "ask has an invalid state")
	}
	return transition.Apply(ctx, tx, ask)
}
func selectedOptions(options []model.AskOption, selected []string) bool {
	labels := make(map[string]struct{}, len(options))
	for _, option := range options {
		labels[option.Label] = struct{}{}
	}
	seen := make(map[string]struct{}, len(selected))
	for _, value := range selected {
		if _, exists := labels[value]; !exists {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}
