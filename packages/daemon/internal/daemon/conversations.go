package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/agenthistory"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ompdirs"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// conversationProofTimeout bounds the Cloud SQL proxy's proof dial when the backfill opens a
// cloudsql-iam conversation store (sessionproxy.Config.ProofTimeout).
const conversationProofTimeout = 30 * time.Second

// conversationHistory is the daemon's record of its agents' conversations (agenthistory): the
// recorder every registration writes through, whatever the configuration, and, with `answers` set,
// the boot's backfill by title and the agent registry's mirror, which run beside the daemon from
// its boot record on (start).
type conversationHistory struct {
	recorder agenthistory.Recorder
	// mirror and backfill are nil without `answers`.
	mirror   *agenthistory.Mirror
	backfill func(ctx context.Context) error
	log      *slog.Logger
}

// newConversationHistory builds the history over the daemon's store. With `answers` set, the
// mirror writes to the daemon's own Envoy listener with its own credential, and the backfill reads
// the conversation store the loop reads: Oh My Pi's session table under a database store (the URL
// boot read, or, under cloudsql-iam, a proxy of the daemon's own), else the pane home's session
// files; and under tmux the Claude Code transcripts under the state directory too. environ is the
// daemon's environment, which the pane home's profile root is resolved from.
func newConversationHistory(cfg config.Config, p plan, st *store.Store, environ []string, log *slog.Logger) *conversationHistory {
	h := &conversationHistory{
		recorder: agenthistory.Recorder{Store: st, Harness: cfg.BackendFor, Now: p.clock.Now, Log: log},
		log:      log,
	}
	if cfg.Answers == nil {
		return h
	}
	h.mirror = agenthistory.NewMirror(agenthistory.MirrorConfig{
		BaseURL: cfg.EnvoyURL, Token: p.secrets["ENVOY_TOKEN"], Project: p.project, Store: st, Clock: agenthistory.RealClock{}, Log: log,
	})
	h.recorder.Mirror = h.mirror
	backfill := agenthistory.Backfill{Project: p.project, Store: st, Now: p.clock.Now, Log: log}
	var open func(ctx context.Context) (*pgx.Conn, func(), error)
	switch database := cfg.Answers.ConversationStore.Database; {
	case p.answers.conversationDSN != "":
		dsn := p.answers.conversationDSN
		open = func(ctx context.Context) (*pgx.Conn, func(), error) {
			conn, err := ompsessions.ConnectURL(ctx, dsn)
			if err != nil {
				return nil, nil, err
			}
			return conn, func() { _ = conn.Close(context.Background()) }, nil
		}
	case database != (ompsessions.Database{}):
		open = func(ctx context.Context) (*pgx.Conn, func(), error) { return openCloudSQLConversations(ctx, database) }
	}
	h.backfill = func(ctx context.Context) error {
		if open != nil {
			conn, closeConn, err := open(ctx)
			if err != nil {
				return fmt.Errorf("open the conversation store: %w", err)
			}
			err = backfill.Table(ctx, conn)
			closeConn()
			if err != nil {
				return err
			}
		}
		if cfg.Runtime.Name != "tmux" {
			return nil
		}
		if open == nil {
			root, _, err := ompdirs.ProfileRoot(tmux.PaneEnvironment(environ, cfg.StateDir), cfg.StateDir)
			if err != nil {
				return fmt.Errorf("resolve the panes' Oh My Pi profile root: %w", err)
			}
			if err := backfill.Files(ctx, agenthistory.OMPSessionsDir(root)); err != nil {
				return err
			}
		}
		// Every Claude Code claim's directory (tmux.ClaudeConfigDir) lies under `<state_dir>/claude`.
		return backfill.Claude(ctx, filepath.Join(cfg.StateDir, "claude"))
	}
	return h
}

// start runs the backfill, then the mirror, until the stop it returns, which waits for both to
// end. Without `answers` it starts nothing. A failed backfill is logged, and the mirror writes what
// the history holds: the next boot backfills again.
func (h *conversationHistory) start(ctx context.Context) (stop func()) {
	if h.mirror == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := h.backfill(ctx); err != nil && ctx.Err() == nil {
			h.log.Error("legion daemon could not backfill its conversation history by title; the next boot tries again", "error", err)
		}
		h.mirror.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// openCloudSQLConversations connects to a cloudsql-iam conversation store as the daemon signs in
// to it: a session proxy of its own (sessionproxy, the launcher's own) on a socket in a private
// directory, dialing the instance over the daemon's cloud identity as database's login. The close
// it returns ends the connection, the proxy and the directory.
func openCloudSQLConversations(ctx context.Context, database ompsessions.Database) (*pgx.Conn, func(), error) {
	dir, err := os.MkdirTemp("", "legion-conv-")
	if err != nil {
		return nil, nil, fmt.Errorf("make the conversation store proxy's directory: %w", err)
	}
	endpoint := ompsessions.Endpoint{SocketDir: dir, Database: database.Name, User: database.User}
	proxy, err := sessionproxy.Start(ctx, sessionproxy.Config{Socket: endpoint.Socket(), Instance: database.Instance, ProofTimeout: conversationProofTimeout})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	conn, err := ompsessions.ConnectURL(ctx, endpoint.ClientURL())
	if err != nil {
		_ = proxy.Close()
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	return conn, func() {
		_ = conn.Close(context.Background())
		_ = proxy.Close()
		_ = os.RemoveAll(dir)
	}, nil
}
