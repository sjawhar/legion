package bus

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
)

// storedMessageAPI is the JetStream API subject of a stored-message read of the notification
// stream. It is the one grant the read needs beyond STREAM.INFO: a read that cannot create, change
// or delete anything (packages/envoy/infra/core/permissions.go, ListenerPermissions).
const storedMessageAPI = "$JS.API.STREAM.MSG.GET." + Stream

// storedMessageWait bounds a stored-message read whose caller set no deadline of its own.
const storedMessageWait = 10 * time.Second

// StoredMessage is one message the notification stream holds: its stream sequence, the concrete
// subject it was published on, when the stream stored it, and its body.
type StoredMessage struct {
	Sequence uint64
	Subject  string
	Time     time.Time
	Data     []byte
}

// NextStoredMessage reads the first message the notification stream holds at or after sequence seq
// on a subject that subject matches (wildcards allowed), with STREAM.MSG.GET and next_by_subj. A
// sequence below the stream's first one reads from the first message it still holds.
//
// Its errors are the stream's own API errors as *nats.APIError, so errors.Is matches
// nats.ErrMsgNotFound (nothing at or after seq on subject) and nats.ErrStreamNotFound (no such
// stream); nats.ErrNoResponders when no JetStream answers; ErrPublishDenied, wrapping the server's
// permissions violation, when the connection's user may not make the read, reported as soon as the
// server refuses it rather than once the wait runs out; and ctx's error when ctx ends first.
func (c *Client) NextStoredMessage(ctx context.Context, subject string, seq uint64) (StoredMessage, error) {
	request, err := json.Marshal(struct {
		Seq     uint64 `json:"seq"`
		NextFor string `json:"next_by_subj"`
	}{Seq: seq, NextFor: subject})
	if err != nil {
		return StoredMessage{}, err
	}
	reply, err := c.requestConfirmed(ctx, storedMessageAPI, request)
	if err != nil {
		return StoredMessage{}, err
	}
	var response struct {
		Message *struct {
			Subject  string    `json:"subject"`
			Sequence uint64    `json:"seq"`
			Data     []byte    `json:"data"`
			Time     time.Time `json:"time"`
		} `json:"message"`
		Error *nats.APIError `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &response); err != nil {
		return StoredMessage{}, fmt.Errorf("bus: read the stored-message answer from %s: %w", Stream, err)
	}
	if response.Error != nil {
		return StoredMessage{}, response.Error
	}
	if response.Message == nil {
		return StoredMessage{}, fmt.Errorf("bus: the stored-message answer from %s holds no message", Stream)
	}
	return StoredMessage{
		Sequence: response.Message.Sequence,
		Subject:  response.Message.Subject,
		Time:     response.Message.Time,
		Data:     response.Message.Data,
	}, nil
}

// requestConfirmed sends data to subject as a request and returns its one reply. The publish is
// confirmed as RequestCoreTo's is (publishConfirmed), so a request the server refuses under the
// connection's publish permissions returns ErrPublishDenied at once; a plain request would wait
// out its whole deadline, since core NATS answers a denied publish to no one but the async error
// callback. One publishConfirmed cannot confirm for another reason still waits for its reply, and
// returns that reason if ctx ends first. A no-responders status reply is nats.ErrNoResponders.
func (c *Client) requestConfirmed(ctx context.Context, subject string, data []byte) (*nats.Msg, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, storedMessageWait)
		defer cancel()
	}
	if err := c.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	c.mu.Lock()
	conn := c.Conn
	c.mu.Unlock()
	inbox := conn.NewInbox()
	replies, err := conn.SubscribeSync(inbox)
	if err != nil {
		return nil, err
	}
	defer replies.Unsubscribe()
	err = c.publishConfirmed(conn, &nats.Msg{Subject: subject, Reply: inbox, Data: data}, deadline, conn.PublishMsg)
	if err != nil && !errors.Is(err, errPublishUnconfirmed) {
		return nil, err
	}
	reply, waitErr := replies.NextMsgWithContext(ctx)
	if waitErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, cmp.Or(err, ctxErr)
		}
		return nil, waitErr
	}
	if len(reply.Data) == 0 && reply.Header.Get("Status") == "503" {
		return nil, nats.ErrNoResponders
	}
	return reply, nil
}

// CheckFilterSubject refuses a subject a stored-message read cannot filter on: one NATS does not
// accept in a subject (checkSubject), or one with a `>` anywhere but as its last token.
func CheckFilterSubject(subject string) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	tokens := strings.Split(subject, ".")
	for _, token := range tokens[:len(tokens)-1] {
		if token == ">" {
			return fmt.Errorf("%w: %q has a `>` before its last token", ErrInvalidSubject, subject)
		}
	}
	return nil
}

// SubjectContains reports whether every subject that subject matches is also matched by pattern,
// under NATS wildcard rules: `*` matches one token and `>`, as the last token, one or more. Either
// may hold wildcards; a wildcard in subject is contained only by a wildcard in pattern at least as
// wide (`a.>` contains `a.*.b` and `a.>`, `a.*` contains `a.*` but not `a.>`).
func SubjectContains(pattern, subject string) bool {
	patternTokens, subjectTokens := strings.Split(pattern, "."), strings.Split(subject, ".")
	for index, patternToken := range patternTokens {
		if patternToken == ">" {
			return index < len(subjectTokens)
		}
		if index >= len(subjectTokens) {
			return false
		}
		subjectToken := subjectTokens[index]
		if subjectToken == ">" {
			return false
		}
		if patternToken != "*" && patternToken != subjectToken {
			return false
		}
	}
	return len(patternTokens) == len(subjectTokens)
}

// CoversAnotherAgentSubject reports whether topic reaches a session's agent subject other than
// sessionID's own: it overlaps notifications.agent.> under NATS wildcard rules (subjectsOverlap),
// so notifications.>, notifications.*.<x> and notifications.agent.* count, and it is not
// sessionID's own agent subject as one literal subject. A session id holding `*` or `>` makes a
// wildcard of its "own" agent subject, which is no session's alone and counts too.
func CoversAnotherAgentSubject(sessionID, topic string) bool {
	if !subjectsOverlap(topic, contracts.AgentTopicPrefix+">") {
		return false
	}
	return topic != contracts.AgentSubject(sessionID) || strings.ContainsAny(sessionID, "*>")
}
