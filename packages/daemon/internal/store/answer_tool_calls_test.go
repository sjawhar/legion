package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// A gh call is recorded before it runs, with no result, and finished after with what the model
// read and gh's exit; a call number recorded once is refused a second time, a finished call is not
// finished again, and a refused call that never ran is finished with no exit.
func TestAnswerToolCallsAreRecordedBeforeAndFinishedAfter(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	started := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	call := AnswerToolCall{Question: "dispatch:m-1", Seq: 1, Args: []string{"pr", "view", "5", "-R", "acme/widgets"}, App: "implement", Owner: "acme", StartedAt: started}
	if err := s.StartAnswerToolCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.AnswerToolCall(ctx, "dispatch:m-1", 1)
	if err != nil || !ok {
		t.Fatalf("AnswerToolCall after the start = %+v, %t, %v; want the row", got, ok, err)
	}
	if !slices.Equal(got.Args, call.Args) || got.App != "implement" || got.Owner != "acme" || !got.StartedAt.Equal(started) || got.Result != nil || got.Exit != nil || got.FinishedAt != nil {
		t.Errorf("the started call reads %+v, want %+v with no result, exit or end", got, call)
	}
	if err := s.StartAnswerToolCall(ctx, call); !errors.Is(err, ErrAnswerToolCallRecorded) {
		t.Errorf("a second start of dispatch:m-1.1 = %v, want ErrAnswerToolCallRecorded", err)
	}

	finished := started.Add(2 * time.Second)
	exit := 1
	if err := s.FinishAnswerToolCall(ctx, "dispatch:m-1", 1, "no pull requests found", &exit, finished); err != nil {
		t.Fatal(err)
	}
	got, _, err = s.AnswerToolCall(ctx, "dispatch:m-1", 1)
	if err != nil || got.Result == nil || *got.Result != "no pull requests found" || got.Exit == nil || *got.Exit != 1 || got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("the finished call reads %+v, %v; want its result, exit 1 and end", got, err)
	}
	if err := s.FinishAnswerToolCall(ctx, "dispatch:m-1", 1, "again", &exit, finished); err == nil {
		t.Errorf("finishing a finished call succeeded, want a refusal")
	}
	if err := s.FinishAnswerToolCall(ctx, "dispatch:m-1", 9, "never started", nil, finished); err == nil {
		t.Errorf("finishing a call never started succeeded, want a refusal")
	}

	refused := AnswerToolCall{Question: "dispatch:m-1", Seq: 2, Args: []string{"auth", "token"}, App: "implement", StartedAt: finished}
	if err := s.StartAnswerToolCall(ctx, refused); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAnswerToolCall(ctx, "dispatch:m-1", 2, "refused", nil, finished); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.AnswerToolCall(ctx, "dispatch:m-1", 2); err != nil || got.Exit != nil || got.Result == nil || got.Owner != "" {
		t.Errorf("the refused call reads %+v, %v; want a result and no exit or owner", got, err)
	}
	if _, ok, err := s.AnswerToolCall(ctx, "dispatch:m-2", 1); ok || err != nil {
		t.Errorf("AnswerToolCall of a question with no calls = %t, %v; want none", ok, err)
	}
}
