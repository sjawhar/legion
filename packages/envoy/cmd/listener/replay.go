package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The bounds of one replay answer. The read budget stays well inside the HTTP server's 10 s write
// deadline (main.go), with the stream info read inside it.
const (
	replayMaxSubjects     = 32
	replayDefaultMessages = 100
	replayMaxMessages     = 500
	replayMaxDataBytes    = 2 << 20
	replayReadBudget      = 5 * time.Second
)

// The answer's wire values, shared with @legion/envoy-client's replay transport.
const (
	replayRefusedFollowed = "not_followed"
	replayRefusedRetained = "not_retained"
	replayUnavailableCode = "REPLAY_UNAVAILABLE"
	replayTimeLayout      = time.RFC3339Nano
)

type replaySubjectRequest struct {
	Subject string  `json:"subject"`
	After   *uint64 `json:"after"`
}

type replayRequest struct {
	SessionID   string                 `json:"session_id"`
	Subjects    []replaySubjectRequest `json:"subjects"`
	MaxMessages *int                   `json:"max_messages"`
}

type replayStream struct {
	Created  string `json:"created"`
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
}

type replayMessage struct {
	Seq     uint64 `json:"seq"`
	Subject string `json:"subject"`
	Time    string `json:"time"`
	Data    string `json:"data"`
}

type replaySubjectResult struct {
	Subject   string          `json:"subject"`
	Messages  []replayMessage `json:"messages"`
	NextAfter uint64          `json:"next_after"`
	More      bool            `json:"more"`
	Refused   string          `json:"refused,omitempty"`
}

type replayResponse struct {
	Stream   replayStream          `json:"stream"`
	Subjects []replaySubjectResult `json:"subjects"`
}

// replayUnavailable reports whether err is NATS refusing a replay outright rather than failing it
// once: the read is not granted to the listener's user, no JetStream answers, or there is no
// notification stream. The caller is told the listener cannot replay, not to retry at once.
func replayUnavailable(err error) bool {
	return errors.Is(err, bus.ErrPublishDenied) ||
		errors.Is(err, nats.ErrPermissionViolation) ||
		errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, nats.ErrStreamNotFound) ||
		errors.Is(err, nats.ErrJetStreamNotEnabled) ||
		errors.Is(err, nats.ErrJetStreamNotEnabledForAccount)
}

// writeReplayReadError answers a replay whose NATS read failed: 503 REPLAY_UNAVAILABLE naming
// NATS's error when NATS refuses it (replayUnavailable), and a plain 503 naming it otherwise.
func writeReplayReadError(w http.ResponseWriter, err error) {
	if replayUnavailable(err) {
		writeJSON(w, http.StatusServiceUnavailable, apiError{Code: replayUnavailableCode, Error: err.Error()})
		return
	}
	writeJSONError(w, http.StatusServiceUnavailable, err.Error())
}

// replayHandler answers POST /v1/replay: the messages the notification stream holds for a
// session's own agent subject and the topics registered as its interests, after a cursor per
// subject, so a session that receives on its own NATS subscriptions can read what it missed while
// they were down.
//
// The watermark is the stream's last sequence, read once at the start; each subject is read from
// its after+1 up to the watermark. A subject with no after returns nothing and the watermark, which
// starts its cursor. next_after is the watermark once a subject is read to it, and otherwise the
// last sequence returned (or after, when none was), with more set: the answer stops at
// max_messages (at most replayMaxMessages), replayMaxDataBytes of data, or replayReadBudget of
// reading, and every subject it cut short says so.
//
// A subject is answered only when it is the session's own agent subject, or a topic registered as
// its interest that lies under a shared stream subject (replayFollowed, bus.SharedStreamTopic);
// every other is refused not_followed: another session's agent subject or a wildcard over them,
// every exception lane (the session's own included), the Legion controller's notices, and any
// topic wider than one shared stream subject, such as notifications.>. An answered subject no
// subject of the deployed stream's configuration contains is refused not_retained. Either has no
// messages and next_after its after.
//
// The session_id must be one literal NATS subject token (replaySessionToken), else the request is
// refused 400: it becomes the agent subject as it is and is matched against requested subjects by
// equality, so a session_id of `>` or `*` would make a wildcard such as notifications.agent.> pass
// as the caller's own agent subject and read every other session's inbox.
func replayHandler(d *listenerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var body replayRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		body.SessionID = strings.TrimSpace(body.SessionID)
		if body.SessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		if !replaySessionToken(body.SessionID) {
			writeJSONError(w, http.StatusBadRequest,
				"session_id must be one NATS subject token: no ., *, >, whitespace or control character", "session_id")
			return
		}
		if len(body.Subjects) > replayMaxSubjects {
			writeJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("at most %d subjects per replay, got %d", replayMaxSubjects, len(body.Subjects)), "subjects")
			return
		}
		for _, requested := range body.Subjects {
			if err := bus.CheckFilterSubject(requested.Subject); err != nil {
				writeJSONError(w, http.StatusBadRequest, "subject: "+err.Error(), "subjects")
				return
			}
		}
		maxMessages := replayDefaultMessages
		if body.MaxMessages != nil {
			if *body.MaxMessages < 1 {
				writeJSONError(w, http.StatusBadRequest, "max_messages must be positive", "max_messages")
				return
			}
			maxMessages = min(*body.MaxMessages, replayMaxMessages)
		}

		ctx, cancel := context.WithTimeout(r.Context(), replayReadBudget)
		defer cancel()
		info, err := streamInfoWithin(ctx, d.streamInspector(), bus.Stream, "")
		if err != nil {
			writeReplayReadError(w, err)
			return
		}
		watermark := info.State.LastSeq
		response := replayResponse{
			Stream: replayStream{
				Created:  info.Created.UTC().Format(replayTimeLayout),
				FirstSeq: info.State.FirstSeq,
				LastSeq:  watermark,
			},
			Subjects: make([]replaySubjectResult, 0, len(body.Subjects)),
		}

		followed, err := d.replayFollowed(body.SessionID, body.Subjects)
		if err != nil {
			writeNATSError(w, err, http.StatusServiceUnavailable, err.Error())
			return
		}
		reader := replayReader{client: d.client, ctx: ctx, watermark: watermark, maxMessages: maxMessages}
		for _, requested := range body.Subjects {
			result := replaySubjectResult{Subject: requested.Subject, Messages: []replayMessage{}}
			after := uint64(0)
			if requested.After != nil {
				after = *requested.After
			}
			switch {
			case !followed(requested.Subject):
				result.Refused, result.NextAfter = replayRefusedFollowed, after
			case !slices.ContainsFunc(info.Config.Subjects, func(captured string) bool {
				return bus.SubjectContains(captured, requested.Subject)
			}):
				result.Refused, result.NextAfter = replayRefusedRetained, after
			case requested.After == nil:
				result.NextAfter = watermark
			default:
				if err := reader.read(&result, after); err != nil {
					writeReplayReadError(w, err)
					return
				}
			}
			response.Subjects = append(response.Subjects, result)
		}
		writeJSON(w, http.StatusOK, response)
	}
}

// replaySessionToken reports whether id is one literal NATS subject token, so that
// contracts.AgentSubject(id) names that one session's agent subject and no other: it is not empty
// and holds no `.`, `*`, `>`, whitespace or control character.
func replaySessionToken(id string) bool {
	return id != "" && !strings.ContainsFunc(id, func(r rune) bool {
		return r == '.' || r == '*' || r == '>' || unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// replayFollowed returns whether the session may replay a subject: it is the session's own agent
// subject exactly, or a topic registered as its interest that lies under a shared stream subject
// (bus.SharedStreamTopic: one that never carries an agent session's private payload). Nothing else
// is, whatever the interest store holds, since /v1/interests/subscribe stores any topic but one
// reaching another session's agent subject, and an interest written before or past that guard
// may name anything. The store is read only when some requested subject could pass as an
// interest; a session it holds no record of follows only its agent subject.
func (d *listenerDeps) replayFollowed(sessionID string, subjects []replaySubjectRequest) (func(string) bool, error) {
	agentSubject := contracts.AgentSubject(sessionID)
	var topics []string
	if slices.ContainsFunc(subjects, func(requested replaySubjectRequest) bool {
		return requested.Subject != agentSubject && bus.SharedStreamTopic(requested.Subject)
	}) {
		interest, err := d.registry.Get(sessionID)
		if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
			return nil, err
		}
		topics = interest.Topics
	}
	return func(subject string) bool {
		return subject == agentSubject || (bus.SharedStreamTopic(subject) && slices.Contains(topics, subject))
	}, nil
}

// replayReader reads one replay answer's messages and holds the bounds they share.
type replayReader struct {
	client      *bus.Client
	ctx         context.Context
	watermark   uint64
	maxMessages int
	messages    int
	dataBytes   int
}

// read fills result with the messages stored on its subject after after, up to the watermark or a
// bound. It returns only an error NATS answered a read with; a read the budget ended is a bound.
func (reader *replayReader) read(result *replaySubjectResult, after uint64) error {
	cursor := after
	for cursor < reader.watermark {
		if reader.messages >= reader.maxMessages || reader.ctx.Err() != nil {
			result.More, result.NextAfter = true, cursor
			return nil
		}
		stored, err := reader.client.NextStoredMessage(reader.ctx, result.Subject, cursor+1)
		switch {
		case errors.Is(err, nats.ErrMsgNotFound):
			result.NextAfter = reader.watermark
			return nil
		case err != nil && reader.ctx.Err() != nil:
			result.More, result.NextAfter = true, cursor
			return nil
		case err != nil:
			return err
		}
		if stored.Sequence > reader.watermark {
			result.NextAfter = reader.watermark
			return nil
		}
		// The first message of an answer goes in whatever its size, so an answer always moves on.
		if reader.dataBytes > 0 && reader.dataBytes+len(stored.Data) > replayMaxDataBytes {
			result.More, result.NextAfter = true, cursor
			return nil
		}
		result.Messages = append(result.Messages, replayMessage{
			Seq:     stored.Sequence,
			Subject: stored.Subject,
			Time:    stored.Time.UTC().Format(replayTimeLayout),
			Data:    string(stored.Data),
		})
		reader.messages++
		reader.dataBytes += len(stored.Data)
		cursor = stored.Sequence
	}
	result.NextAfter = reader.watermark
	return nil
}
