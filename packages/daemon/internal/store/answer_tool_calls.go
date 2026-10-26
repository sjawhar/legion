package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AnswerToolCall is one gh call the answer loop ran for a stopped agent (answer_tool_calls): the
// question it answered and the call's place among that answer's calls, the gh arguments as run, the
// App role whose token ran it and the owner whose gh directory it ran with, and when it started.
// Result, Exit and FinishedAt are what FinishAnswerToolCall wrote: Result nil is a call whose
// outcome is unknown, since the daemon stopped while it ran; Exit nil a call gh never started.
type AnswerToolCall struct {
	Question   string
	Seq        int
	Args       []string
	App        string
	Owner      string
	StartedAt  time.Time
	Result     *string
	Exit       *int
	FinishedAt *time.Time
}

// ErrAnswerToolCallRecorded is StartAnswerToolCall's refusal of a call whose question and seq
// already have a row: the call was made once, and is not made again under the same number.
var ErrAnswerToolCallRecorded = errors.New("is already recorded")

// StartAnswerToolCall records call before it runs, with no result. A call whose question and seq
// are recorded already is refused (ErrAnswerToolCallRecorded) and the row is left as it is.
func (s *Store) StartAnswerToolCall(ctx context.Context, call AnswerToolCall) error {
	args, err := json.Marshal(call.Args)
	if err != nil {
		return fmt.Errorf("record gh call %s.%d: encode its arguments: %w", call.Question, call.Seq, err)
	}
	tag, err := s.pool.Exec(ctx, `insert into answer_tool_calls (question, seq, args, app, owner, started_at)
		values ($1, $2, $3, $4, $5, $6) on conflict (question, seq) do nothing`,
		call.Question, call.Seq, args, call.App, call.Owner, call.StartedAt)
	if err != nil {
		return fmt.Errorf("record gh call %s.%d: %w", call.Question, call.Seq, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("gh call %s.%d %w", call.Question, call.Seq, ErrAnswerToolCallRecorded)
	}
	return nil
}

// FinishAnswerToolCall records the outcome of the call question.seq: what the model read, gh's exit
// status (nil when gh never started) and when it ended. Only a started, unfinished call is
// finished; any other is an error and nothing changes.
func (s *Store) FinishAnswerToolCall(ctx context.Context, question string, seq int, result string, exit *int, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `update answer_tool_calls set result = $3, exit = $4, finished_at = $5
		where question = $1 and seq = $2 and result is null`, question, seq, result, exit, at)
	if err != nil {
		return fmt.Errorf("finish gh call %s.%d: %w", question, seq, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finish gh call %s.%d: no started, unfinished call has that number", question, seq)
	}
	return nil
}

// AnswerToolCall is the recorded call question.seq, and false when there is none.
func (s *Store) AnswerToolCall(ctx context.Context, question string, seq int) (AnswerToolCall, bool, error) {
	call := AnswerToolCall{Question: question, Seq: seq}
	var args []byte
	err := s.pool.QueryRow(ctx, `select args, app, owner, started_at, result, exit, finished_at
		from answer_tool_calls where question = $1 and seq = $2`, question, seq).
		Scan(&args, &call.App, &call.Owner, &call.StartedAt, &call.Result, &call.Exit, &call.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AnswerToolCall{}, false, nil
	}
	if err != nil {
		return AnswerToolCall{}, false, fmt.Errorf("read gh call %s.%d: %w", question, seq, err)
	}
	if err := json.Unmarshal(args, &call.Args); err != nil {
		return AnswerToolCall{}, false, fmt.Errorf("read gh call %s.%d: decode its arguments: %w", question, seq, err)
	}
	return call, true, nil
}
