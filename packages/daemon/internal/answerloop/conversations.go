package answerloop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/conversation/claude"
	"github.com/sjawhar/legion/daemon/internal/conversation/omp"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Conversations reads a recorded conversation from where its harness keeps it.
type Conversations interface {
	Open(ctx context.Context, c store.AgentConversation) (Session, error)
}

// Session is one conversation as read: what the model is resumed on, and the append of an
// exchange after exactly what was read (conversation.ErrMoved otherwise). Close releases what the
// read holds.
type Session interface {
	Conversation() conversation.Conversation
	Append(ctx context.Context, a conversation.Append, now time.Time) error
	Close()
}

// conversationMissing is a conversation its store no longer holds.
var conversationMissing = errors.New("the conversation is no longer kept")

// Harnesses is where the daemon's conversations live: an Oh My Pi session in Oh My Pi's session
// table under a database store (OpenSQL, the conversation store's connection; nil under tmux's
// files store), else the session file at its location; a Claude Code transcript at its path.
type Harnesses struct {
	OpenSQL func(ctx context.Context) (*pgx.Conn, func(), error)
	// OMP is how the conversation's endpoint treats replayed thinking (omp.Options).
	OMP omp.Options
}

// Open reads c's conversation as its harness rebuilds it on resume. A conversation its store no
// longer holds is conversationMissing; one the reader refuses is conversation.ErrUnknownFormat.
func (h Harnesses) Open(ctx context.Context, c store.AgentConversation) (Session, error) {
	switch {
	case c.Harness == runtime.BackendClaude:
		s, err := claude.ReadFile(c.Location)
		if err != nil {
			return nil, missing(err)
		}
		return claudeSession{path: c.Location, s: s}, nil
	case c.Harness == runtime.BackendOmp && h.OpenSQL != nil:
		conn, closeConn, err := h.OpenSQL(ctx)
		if err != nil {
			return nil, fmt.Errorf("open the conversation store: %w", err)
		}
		s, err := omp.ReadSQL(ctx, conn, c.Location, h.OMP)
		if err != nil {
			closeConn()
			return nil, missing(err)
		}
		return ompSQLSession{conn: conn, close: closeConn, path: c.Location, s: s}, nil
	case c.Harness == runtime.BackendOmp:
		s, err := omp.ReadFile(c.Location, h.OMP)
		if err != nil {
			return nil, missing(err)
		}
		return ompFileSession{path: c.Location, s: s}, nil
	default:
		return nil, fmt.Errorf("%w: the harness %q", conversation.ErrUnknownFormat, c.Harness)
	}
}

func missing(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", conversationMissing, err)
	}
	return err
}

type ompFileSession struct {
	path string
	s    *omp.Session
}

func (o ompFileSession) Conversation() conversation.Conversation { return o.s.Conversation }
func (o ompFileSession) Append(_ context.Context, a conversation.Append, now time.Time) error {
	return omp.AppendFile(o.path, o.s, a, now)
}
func (o ompFileSession) Close() {}

type ompSQLSession struct {
	conn  *pgx.Conn
	close func()
	path  string
	s     *omp.Session
}

func (o ompSQLSession) Conversation() conversation.Conversation { return o.s.Conversation }
func (o ompSQLSession) Append(ctx context.Context, a conversation.Append, now time.Time) error {
	return omp.AppendSQL(ctx, o.conn, o.path, o.s, a, now)
}
func (o ompSQLSession) Close() { o.close() }

type claudeSession struct {
	path string
	s    *claude.Session
}

func (c claudeSession) Conversation() conversation.Conversation { return c.s.Conversation }
func (c claudeSession) Append(_ context.Context, a conversation.Append, now time.Time) error {
	return claude.AppendFile(c.path, c.s, a, now)
}
func (c claudeSession) Close() {}
