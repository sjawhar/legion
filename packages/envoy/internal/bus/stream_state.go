// What SessionConsumers reads of the notification stream itself: its identity and sequences, read
// once for every call waiting on a read.

package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// sessionStreamState is what SessionConsumers reads of the stream: its identity, the first sequence
// it still holds and the last it stored.
type sessionStreamState struct {
	id                string
	firstSeq, lastSeq uint64
}

// sessionStreamReadWait bounds one read of the stream's state (streamState).
const sessionStreamReadWait = 5 * time.Second

// streamRead is one read of the stream's state, which every streamState call waiting on it shares.
type streamRead struct {
	// done is closed once state and err are set.
	done  chan struct{}
	state sessionStreamState
	err   error
}

// streamState reads the stream's identity and sequences with its info. Calls made at once share a
// read: a call waits for the next read to begin, after the one in flight answers, and every call
// waiting when it begins gets its answer. So no call gets a state read before it was made, and a
// burst of calls, such as the restarts of every session at a listener's start, sends one stream
// info per round trip rather than one per call. A call returns ctx's error if ctx ends first.
func (s *SessionConsumers) streamState(ctx context.Context) (sessionStreamState, error) {
	return s.joinStreamRead().wait(ctx)
}

// joinStreamRead returns the next read of the stream's state to begin (streamState), starting the
// goroutine that sends the reads when none runs.
func (s *SessionConsumers) joinStreamRead() *streamRead {
	s.streamMu.Lock()
	read := s.streamNext
	if read == nil {
		read = &streamRead{done: make(chan struct{})}
		s.streamNext = read
	}
	start := !s.streamReading
	s.streamReading = true
	s.streamMu.Unlock()
	if start {
		go s.readStreamStates()
	}
	return read
}

// wait returns what the read read, once it has answered, or ctx's error if ctx ends first.
func (r *streamRead) wait(ctx context.Context) (sessionStreamState, error) {
	select {
	case <-r.done:
		return r.state, r.err
	case <-ctx.Done():
		return sessionStreamState{}, ctx.Err()
	}
}

// readStreamStates sends the stream reads streamState calls wait for, one at a time, each within
// sessionStreamReadWait, until none waits.
func (s *SessionConsumers) readStreamStates() {
	for {
		s.streamMu.Lock()
		read := s.streamNext
		s.streamNext = nil
		if read == nil {
			s.streamReading = false
			s.streamMu.Unlock()
			return
		}
		s.streamMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), sessionStreamReadWait)
		read.state, read.err = s.readStreamState(ctx)
		cancel()
		close(read.done)
	}
}

// readStreamState reads the stream's identity and sequences with one stream info.
func (s *SessionConsumers) readStreamState(ctx context.Context) (sessionStreamState, error) {
	reply, err := s.client.requestConfirmed(ctx, "$JS.API.STREAM.INFO."+Stream, nil)
	if err != nil {
		return sessionStreamState{}, err
	}
	var response struct {
		Config struct {
			Metadata map[string]string `json:"metadata"`
		} `json:"config"`
		State struct {
			FirstSeq uint64 `json:"first_seq"`
			LastSeq  uint64 `json:"last_seq"`
		} `json:"state"`
		Error *jetstream.APIError `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &response); err != nil {
		return sessionStreamState{}, fmt.Errorf("bus: read the info of %s: %w", Stream, err)
	}
	if response.Error != nil {
		return sessionStreamState{}, fmt.Errorf("bus: read the info of %s: %w", Stream, response.Error)
	}
	return sessionStreamState{
		id:       response.Config.Metadata[StreamIDMetadataKey],
		firstSeq: response.State.FirstSeq,
		lastSeq:  response.State.LastSeq,
	}, nil
}

// firstStoredOn is the sequence of the first message the stream still holds on subject, and 0 when
// it holds none there. It is one message get by subject ($JS.API.STREAM.MSG.GET with next_by_subj,
// which every listener's grant holds), read rather than a consumer so it creates nothing on the
// server, and it reads the sequence alone: the message's own body is never decoded here.
//
// Ensure starts a session's consumer here when it is given no position, so the session is handed
// what the stream holds for it rather than only what is stored from its registration on.
func (s *SessionConsumers) firstStoredOn(ctx context.Context, subject string) (uint64, error) {
	request, err := json.Marshal(struct {
		Seq     uint64 `json:"seq"`
		NextFor string `json:"next_by_subj"`
	}{Seq: 1, NextFor: subject})
	if err != nil {
		return 0, fmt.Errorf("bus: ask %s for its first message on %s: %w", Stream, subject, err)
	}
	reply, err := s.client.requestConfirmed(ctx, "$JS.API.STREAM.MSG.GET."+Stream, request)
	if err != nil {
		return 0, err
	}
	var response struct {
		Message struct {
			Seq uint64 `json:"seq"`
		} `json:"message"`
		Error *jetstream.APIError `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &response); err != nil {
		return 0, fmt.Errorf("bus: read %s's first message on %s: %w", Stream, subject, err)
	}
	if response.Error != nil {
		// The stream holds nothing on the subject at all, which is no failure: a session nothing
		// was ever sent to has an empty agent subject.
		if response.Error.ErrorCode == jetstream.JSErrCodeMessageNotFound {
			return 0, nil
		}
		return 0, fmt.Errorf("bus: read %s's first message on %s: %w", Stream, subject, response.Error)
	}
	return response.Message.Seq, nil
}
