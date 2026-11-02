package bus

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
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
	return c.storedMessage(ctx, struct {
		Seq     uint64 `json:"seq"`
		NextFor string `json:"next_by_subj"`
	}{Seq: seq, NextFor: subject})
}

// LastStoredMessage reads the newest message the notification stream holds on a subject that
// subject matches (wildcards allowed), with STREAM.MSG.GET and last_by_subj. The listener's kept
// lanes are read this way: a role's kept subject and its drain cursor each hold the one message
// that counts, and that is the newest. It is the same request NextStoredMessage makes, over the
// same stored-message API subject (storedMessageAPI), so it needs no grant beyond the one the
// listener already holds for that read.
//
// Its errors are NextStoredMessage's: the stream's own API errors as *nats.APIError, so errors.Is
// matches nats.ErrMsgNotFound (the stream holds nothing on subject) and nats.ErrStreamNotFound (no
// such stream); nats.ErrNoResponders when no JetStream answers; ErrPublishDenied when the
// connection's user may not make the read; and ctx's error when ctx ends first.
func (c *Client) LastStoredMessage(ctx context.Context, subject string) (StoredMessage, error) {
	return c.storedMessage(ctx, struct {
		LastFor string `json:"last_by_subj"`
	}{LastFor: subject})
}

// KeptRoleClaimWindow is how long one walk's claim of a kept copy holds that role's kept lane
// against every other writer of the lane (KeptRoleCursor.Claiming, KeptRoleCursor.ClaimOutstanding):
// the walks of the other listeners of the deployment, and the handover Dispatch's outbox makes
// behind a keep of its own (internal/dispatch/outbox/role.go, keptCopyLeadsTheLane). A claim
// younger than it may still belong to a walk that is forwarding that copy right now, which
// nothing else must forward a second time, and a claim older than it belongs to a walk whose
// context has ended, so the copy it claimed is free for the next walk to forward.
//
// It lives here, in the one package both writers of a kept lane import, because two durations
// meant to be equal are how two writers of a lane drift apart. The listener bounds one walk of a
// lane by it too (cmd/listener/role_keep.go, roleKeepDrainWindow): a walk must not outlive its
// own claim, or a second walk would take a copy over while this one is still forwarding it, and a
// claim must not outlive the walk that wrote it, or a lane would stay held longer than the work
// behind the claim.
//
// The age is one process's clock against the time the stream stored the cursor, which is the
// server's: this window assumes the two are within a minute of each other, which a deployment
// whose hosts run NTP holds to by orders of magnitude. A listener an hour out of step drops every
// copy as past its own keep window anyway, and a clock that runs ahead costs one live claim taken
// over early - the duplicate the design already accepts - so no reader spends a round trip on the
// stream's own time.
const KeptRoleClaimWindow = 60 * time.Second

// KeptRoleCursor is the body of a role's drain cursor (KeptRoleCursorSubject): the two numbers
// that say how far that role's kept lane has been drained. Kept is the stream sequence on the
// role's kept subject the lane is finished with - every copy up to it was forwarded, dropped as
// expired or passed over - and a walk starts at the copy after it. Claiming is a copy one walk
// has claimed and not yet confirmed delivered, 0 when none is outstanding: a walk writes it
// before it forwards that copy and clears it with the same write that moves Kept past the copy,
// so a walk that ends between the two leaves Kept short of the copy and the next walk reads that
// copy again. A claim is also the lane's lock while it is outstanding (ClaimOutstanding).
//
// The listener's drain is its only writer, which writes it with Client.PublishCursorAt as this
// small body rather than an envelope (cmd/listener/role_keep.go, roleKeepLaneCursor). It lives
// here because Dispatch's outbox reads it as well, to tell whether the copy it has just kept is
// the one that lane's drain would hand over next (internal/dispatch/outbox/role.go,
// keptCopyLeadsTheLane): a body each package spelled out for itself would decode a drifted tag as
// zero, which reads as a lane drained to its start and leads every handover astray.
type KeptRoleCursor struct {
	Kept     uint64 `json:"kept"`
	Claiming uint64 `json:"claiming"`
}

// ClaimOutstanding reports whether the copy this cursor claims is still one walk's to forward: a
// claim is outstanding while it names a copy and is younger than KeptRoleClaimWindow. written is
// the time the stream stored the cursor, which is how old the claim it carries is.
//
// now is the reader's clock rather than a time already read, and it is called only while a claim
// is named at all: the listener's walk reads its clock once per copy it judges and once more for
// the claim it found, which a lane nothing claims must not be charged for (walkKeptRole).
func (c KeptRoleCursor) ClaimOutstanding(written time.Time, now func() time.Time) bool {
	return c.Claiming != 0 && now().Sub(written) < KeptRoleClaimWindow
}

// StoredKeptRoleCursor is a role's drain cursor as the stream holds it (Client.ReadKeptRoleCursor).
type StoredKeptRoleCursor struct {
	// Cursor is the cursor's body, its zero value for a lane that has never been drained.
	Cursor KeptRoleCursor
	// Sequence is the stream sequence of the cursor message itself, which the next
	// compare-and-swap write of the cursor must name (Client.PublishCursorAt); 0 for a lane
	// whose cursor has never been written.
	Sequence uint64
	// Written is the time the stream stored the cursor, which is how old the claim it carries is
	// (KeptRoleCursor.ClaimOutstanding); the zero time when there is no cursor.
	Written time.Time
}

// ReadKeptRoleCursor reads role's drain cursor: the one read of it both writers of that role's
// kept lane make - the listener's pass over the lane (cmd/listener/role_keep.go,
// readKeptLaneHead) and the handover behind Dispatch's outbox's own keep
// (internal/dispatch/outbox/role.go, keptCopyLeadsTheLane) - so that the two decode one body one
// way. A lane no walk has written a cursor for is no error but the zero StoredKeptRoleCursor its
// fields describe: every copy on such a lane is past its cursor.
//
// Its errors are LastStoredMessage's, named with the role and wrapped, and a cursor body no read
// can decode is one of them.
func (c *Client) ReadKeptRoleCursor(ctx context.Context, role string) (StoredKeptRoleCursor, error) {
	stored, err := c.LastStoredMessage(ctx, KeptRoleCursorSubject(role))
	if errors.Is(err, nats.ErrMsgNotFound) {
		return StoredKeptRoleCursor{}, nil
	}
	if err != nil {
		return StoredKeptRoleCursor{}, fmt.Errorf("read role %s's kept-lane cursor: %w", role, err)
	}
	read := StoredKeptRoleCursor{Sequence: stored.Sequence, Written: stored.Time}
	if err := json.Unmarshal(stored.Data, &read.Cursor); err != nil {
		return StoredKeptRoleCursor{}, fmt.Errorf("read role %s's kept-lane cursor stored at %d: %w", role, stored.Sequence, err)
	}
	return read, nil
}

// KeptRoleLanes reports the role of every kept lane the notification stream still holds a copy on
// (KeptRoleSubject), sorted, so each is read once and two passes over an unchanged stream read the
// same list. It is the listener's backstop over the lanes nobody is draining: a role whose kept
// copies were stranded by a listener that ended mid-drain is on it, and a role whose lane the
// stream holds nothing on is not, whatever its drain cursor says.
//
// It is one STREAM.INFO of the stream with the kept role subjects as its filter, which answers
// every such subject and how many messages it holds; the listener already holds that grant for the
// stream it owns (infra/core/permissions.go), and nats.go pages the answer itself, so a sweep
// costs one round trip whatever the deployment holds. A stream that holds no kept copy at all
// answers an empty list: the state carries a subject only while a message is stored on it.
//
// Its errors are the stream's own: nats.ErrStreamNotFound when the stream is gone,
// nats.ErrNoResponders when no JetStream answers, and ctx's error when ctx ends first.
func (c *Client) KeptRoleLanes(ctx context.Context) ([]string, error) {
	if err := c.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	info, err := c.JS().StreamInfo(Stream,
		&nats.StreamInfoRequest{SubjectsFilter: KeptRoleSubject(">")},
		nats.Context(ctx))
	if err != nil {
		return nil, fmt.Errorf("bus: read %s's kept role lanes: %w", Stream, err)
	}
	roles := make([]string, 0, len(info.State.Subjects))
	for subject, held := range info.State.Subjects {
		role, ok := strings.CutPrefix(subject, KeptRoleSubjectPrefix)
		if !ok || role == "" || held == 0 {
			continue
		}
		roles = append(roles, role)
	}
	slices.Sort(roles)
	return roles, nil
}

// storedMessage makes one stored-message read of the notification stream and reads its answer, for
// NextStoredMessage and LastStoredMessage alike: request is the body each builds, naming the
// message it wants. The stream's error answer comes back as the *nats.APIError it is, and an
// answer that names neither a message nor an error is this package's own error.
func (c *Client) storedMessage(ctx context.Context, request any) (StoredMessage, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return StoredMessage{}, err
	}
	reply, err := c.requestConfirmed(ctx, storedMessageAPI, body)
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
	conn := c.currentConn()
	inbox := conn.NewInbox()
	replies, err := conn.SubscribeSync(inbox)
	if err != nil {
		return nil, err
	}
	defer replies.Unsubscribe()
	err = c.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Reply: inbox, Data: data}, conn.PublishMsg)
	if err != nil && !errors.Is(err, ErrPublishUnconfirmed) {
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

// ReduceFilterSubjects returns subjects as a set of consumer filters no two of which overlap, which
// JetStream requires of one consumer's filters, matching every subject any of them matched: a
// subject another contains (SubjectContains) is dropped, and two that overlap without either
// containing the other are replaced by their longest common prefix of whole tokens followed by `>`
// (`>` alone when the first tokens differ), until no two overlap. `X` and `X.>` do not overlap (one
// matches X alone, the other only what lies under X), so both stay. The result is sorted and holds
// each subject once, so the same set always reduces to the same list.
func ReduceFilterSubjects(subjects []string) []string {
	reduced := slices.Clone(subjects)
	for {
		slices.Sort(reduced)
		reduced = slices.Compact(reduced)
		next, changed := reduceFilterPair(reduced)
		if !changed {
			return reduced
		}
		reduced = next
	}
}

// reduceFilterPair applies ReduceFilterSubjects's rule to the first pair of subjects that overlap,
// reporting false when none do.
func reduceFilterPair(subjects []string) ([]string, bool) {
	for i, a := range subjects {
		for j := i + 1; j < len(subjects); j++ {
			b := subjects[j]
			switch {
			case SubjectContains(a, b):
				return slices.Delete(slices.Clone(subjects), j, j+1), true
			case SubjectContains(b, a):
				return slices.Delete(slices.Clone(subjects), i, i+1), true
			case subjectsOverlap(a, b):
				merged := commonTokenPrefix(a, b) + ">"
				rest := slices.DeleteFunc(slices.Clone(subjects), func(s string) bool { return s == a || s == b })
				return append(rest, merged), true
			}
		}
	}
	return subjects, false
}

// commonTokenPrefix is the tokens a and b begin with alike, up to the first that differs or is `>`,
// with the dot after the last of them, or "" when their first tokens differ.
func commonTokenPrefix(a, b string) string {
	aTokens, bTokens := strings.Split(a, "."), strings.Split(b, ".")
	var prefix strings.Builder
	for index := 0; index < len(aTokens) && index < len(bTokens); index++ {
		if aTokens[index] != bTokens[index] || aTokens[index] == ">" {
			break
		}
		prefix.WriteString(aTokens[index] + ".")
	}
	return prefix.String()
}
