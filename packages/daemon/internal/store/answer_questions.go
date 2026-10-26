package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// QuestionState is where a question the answer loop holds stands (answer_questions.state).
type QuestionState string

const (
	// QuestionReceived is a question recorded and not yet answered.
	QuestionReceived QuestionState = "received"
	// QuestionGenerating is a question whose answer is being generated: a restart answers it again,
	// with the gh calls the earlier run made.
	QuestionGenerating QuestionState = "generating"
	// QuestionAppended is a question whose reply is decided (ReplyText): the model's, appended to
	// the conversation, or the daemon's own. A restart posts it and never answers again.
	QuestionAppended QuestionState = "appended"
	// QuestionPosted is a question whose reply was posted where it was asked.
	QuestionPosted QuestionState = "posted"
	// QuestionPassedOn is a question whose reply was posted and which was passed to the controller.
	QuestionPassedOn QuestionState = "passed_on"
	// QuestionForwarded is a question handed to the agent that runs its conversation, delivered once
	// ForwardDelivered.
	QuestionForwarded QuestionState = "forwarded"
	// QuestionDropped is a question the loop ended with nothing posted.
	QuestionDropped QuestionState = "dropped"
)

// AnswerQuestion is one question the answer loop holds (answer_questions): its id; the Legion
// project token; its identity (dispatch:<message or comment id>, or an Envoy dedupe key); the
// conversation that answers it (its session, role token, issue and role); where it came from
// (Kind: message, mention, ask_reply, envoy); whether the agent may reply that it asks nothing;
// the hand-off as received; its state; the reply decided for it (its kind, its text, why it is
// passed on); whether a forward reached the running agent; how many hand-offs carried it; and when
// it was first and last handed off and last changed.
type AnswerQuestion struct {
	ID               string
	Project          string
	Identity         string
	Session          string
	Token            claim.Token
	Issue            string
	Role             claim.Role
	Kind             string
	MayAskNothing    bool
	Handoff          json.RawMessage
	State            QuestionState
	ReplyKind        string
	ReplyText        string
	PassReason       string
	ForwardDelivered bool
	Deliveries       int
	ReceivedAt       time.Time
	LastSeenAt       time.Time
	UpdatedAt        time.Time
}

const questionColumns = `id, project, identity, session, token, issue, role, kind, may_ask_nothing, handoff, state, reply_kind,
	reply_text, pass_reason, forward_delivered, deliveries, received_at, last_seen_at, updated_at`

// ErrQuestionMoved is UpdateQuestion's refusal of a question no longer in a state the update moves
// it from: another writer moved it first.
var ErrQuestionMoved = errors.New("the question is no longer in the state the update expects")

// ReceiveQuestion records q, received at q.ReceivedAt, and returns the row and whether this call
// recorded it. A question whose project, identity and session are recorded already is the same
// question handed off again: its deliveries and last_seen_at are bumped and the row is otherwise
// left as it is.
func (s *Store) ReceiveQuestion(ctx context.Context, q AnswerQuestion) (AnswerQuestion, bool, error) {
	rows, err := s.pool.Query(ctx, `insert into answer_questions (id, project, identity, session, token, issue, role, kind,
			may_ask_nothing, handoff, state, received_at, last_seen_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'received', $11, $11, $11)
		on conflict (project, identity, session) do update set deliveries = answer_questions.deliveries + 1,
			last_seen_at = excluded.last_seen_at
		returning `+questionColumns+`, xmax = 0`,
		q.ID, q.Project, q.Identity, q.Session, string(q.Token), q.Issue, string(q.Role), q.Kind, q.MayAskNothing, []byte(q.Handoff), q.ReceivedAt)
	if err != nil {
		return AnswerQuestion{}, false, fmt.Errorf("record the question %s: %w", q.Identity, err)
	}
	var inserted bool
	found, err := scanQuestions(rows, &inserted)
	if err != nil {
		return AnswerQuestion{}, false, fmt.Errorf("record the question %s: %w", q.Identity, err)
	}
	if len(found) != 1 {
		return AnswerQuestion{}, false, fmt.Errorf("record the question %s: %d rows written, want 1", q.Identity, len(found))
	}
	return found[0], inserted, nil
}

// Question is the question id, and false when there is none.
func (s *Store) Question(ctx context.Context, id string) (AnswerQuestion, bool, error) {
	rows, err := s.pool.Query(ctx, `select `+questionColumns+` from answer_questions where id = $1`, id)
	if err != nil {
		return AnswerQuestion{}, false, fmt.Errorf("read the question %s: %w", id, err)
	}
	found, err := scanQuestions(rows, nil)
	if err != nil {
		return AnswerQuestion{}, false, fmt.Errorf("read the question %s: %w", id, err)
	}
	if len(found) == 0 {
		return AnswerQuestion{}, false, nil
	}
	return found[0], true, nil
}

// OpenQuestions is every question of the project the loop still has work on, oldest first: one
// received, generating or appended, and one forwarded that its agent's role topic has not taken.
func (s *Store) OpenQuestions(ctx context.Context, project string) ([]AnswerQuestion, error) {
	rows, err := s.pool.Query(ctx, `select `+questionColumns+` from answer_questions
		where project = $1 and (state in ('received', 'generating', 'appended') or (state = 'forwarded' and not forward_delivered))
		order by received_at, id`, project)
	if err != nil {
		return nil, fmt.Errorf("read the open questions of %s: %w", project, err)
	}
	found, err := scanQuestions(rows, nil)
	if err != nil {
		return nil, fmt.Errorf("read the open questions of %s: %w", project, err)
	}
	return found, nil
}

// UpdateQuestion writes q's state, reply, pass reason and forward delivery, at q.UpdatedAt, when
// the question is in one of the states from; otherwise it changes nothing and is ErrQuestionMoved.
func (s *Store) UpdateQuestion(ctx context.Context, q AnswerQuestion, from ...QuestionState) error {
	states := make([]string, len(from))
	for i, state := range from {
		states[i] = string(state)
	}
	tag, err := s.pool.Exec(ctx, `update answer_questions set state = $2, reply_kind = $3, reply_text = $4, pass_reason = $5,
			forward_delivered = $6, updated_at = $7
		where id = $1 and state = any($8)`,
		q.ID, string(q.State), q.ReplyKind, q.ReplyText, q.PassReason, q.ForwardDelivered, q.UpdatedAt, states)
	if err != nil {
		return fmt.Errorf("move the question %s to %s: %w", q.ID, q.State, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("move the question %s to %s from %v: %w", q.ID, q.State, from, ErrQuestionMoved)
	}
	return nil
}

// AnswerToolCalls is every recorded gh call of question, in seq order.
func (s *Store) AnswerToolCalls(ctx context.Context, question string) ([]AnswerToolCall, error) {
	rows, err := s.pool.Query(ctx, `select seq, args, app, owner, started_at, result, exit, finished_at
		from answer_tool_calls where question = $1 order by seq`, question)
	if err != nil {
		return nil, fmt.Errorf("read the gh calls of %s: %w", question, err)
	}
	defer rows.Close()
	var calls []AnswerToolCall
	for rows.Next() {
		call := AnswerToolCall{Question: question}
		var args []byte
		if err := rows.Scan(&call.Seq, &args, &call.App, &call.Owner, &call.StartedAt, &call.Result, &call.Exit, &call.FinishedAt); err != nil {
			return nil, fmt.Errorf("read the gh calls of %s: %w", question, err)
		}
		if err := json.Unmarshal(args, &call.Args); err != nil {
			return nil, fmt.Errorf("read gh call %s.%d: decode its arguments: %w", question, call.Seq, err)
		}
		calls = append(calls, call)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the gh calls of %s: %w", question, err)
	}
	return calls, nil
}

// AnswerSpend is what the project's answers cost on the UTC day of at, in US dollars.
func (s *Store) AnswerSpend(ctx context.Context, project string, at time.Time) (float64, error) {
	var usd float64
	err := s.pool.QueryRow(ctx, `select usd from answer_spend where project = $1 and day = $2`, project, utcDay(at)).Scan(&usd)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the answer spend of %s: %w", project, err)
	}
	return usd, nil
}

// AddAnswerSpend adds usd to the project's spend on the UTC day of at.
func (s *Store) AddAnswerSpend(ctx context.Context, project string, at time.Time, usd float64) error {
	if usd < 0 {
		return fmt.Errorf("add %f USD to the answer spend of %s: a spend is never negative", usd, project)
	}
	if _, err := s.pool.Exec(ctx, `insert into answer_spend (project, day, usd) values ($1, $2, $3)
		on conflict (project, day) do update set usd = answer_spend.usd + excluded.usd`, project, utcDay(at), usd); err != nil {
		return fmt.Errorf("add to the answer spend of %s: %w", project, err)
	}
	return nil
}

// utcDay is at's UTC calendar day, as a date column takes it.
func utcDay(at time.Time) time.Time {
	y, m, d := at.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// QuestionPassedSource is the processed_events source the daemon marks a question passed to the
// controller under, by the question's id, in the transaction that queues its agent-question notice:
// the mark is what passes a question once, and when.
const QuestionPassedSource = "agent-question"

// PassedQuestion is a question the daemon passed to the controller, and when it did.
type PassedQuestion struct {
	AnswerQuestion
	PassedAt time.Time
}

// PassedQuestionsTx is the project's questions passed to the controller at since or later, newest
// first and at most limit of them, read through tx: each question with a QuestionPassedSource mark,
// at the mark's time.
func (s *Store) PassedQuestionsTx(ctx context.Context, tx pgx.Tx, project string, since time.Time, limit int) ([]PassedQuestion, error) {
	// No column of processed_events shares a name with one of answer_questions.
	rows, err := tx.Query(ctx, `select `+questionColumns+`, processed_at
		from answer_questions join processed_events on source = $1 and event_id = id
		where project = $2 and processed_at >= $3
		order by processed_at desc, id desc limit $4`, QuestionPassedSource, project, since, limit)
	if err != nil {
		return nil, fmt.Errorf("read the questions %s passed to the controller: %w", project, err)
	}
	defer rows.Close()
	var passed []PassedQuestion
	for rows.Next() {
		var p PassedQuestion
		if p.AnswerQuestion, err = scanQuestion(rows, &p.PassedAt); err != nil {
			return nil, fmt.Errorf("read the questions %s passed to the controller: %w", project, err)
		}
		passed = append(passed, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the questions %s passed to the controller: %w", project, err)
	}
	return passed, nil
}

func scanQuestions(rows pgx.Rows, inserted *bool) ([]AnswerQuestion, error) {
	defer rows.Close()
	var extra []any
	if inserted != nil {
		extra = append(extra, inserted)
	}
	var found []AnswerQuestion
	for rows.Next() {
		q, err := scanQuestion(rows, extra...)
		if err != nil {
			return nil, err
		}
		found = append(found, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return found, nil
}

// scanQuestion is the current row's questionColumns, with each column after them scanned into
// extra.
func scanQuestion(rows pgx.Rows, extra ...any) (AnswerQuestion, error) {
	var q AnswerQuestion
	var token, role, state string
	var handoff []byte
	dest := []any{&q.ID, &q.Project, &q.Identity, &q.Session, &token, &q.Issue, &role, &q.Kind, &q.MayAskNothing, &handoff,
		&state, &q.ReplyKind, &q.ReplyText, &q.PassReason, &q.ForwardDelivered, &q.Deliveries, &q.ReceivedAt, &q.LastSeenAt, &q.UpdatedAt}
	if err := rows.Scan(append(dest, extra...)...); err != nil {
		return AnswerQuestion{}, err
	}
	q.Token, q.Role, q.State, q.Handoff = claim.Token(token), claim.Role(role), QuestionState(state), handoff
	return q, nil
}
