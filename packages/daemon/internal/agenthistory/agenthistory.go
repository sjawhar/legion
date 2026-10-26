// Package agenthistory is the daemon's record of the conversations its agents have had
// (agent_conversations) and that record's mirror in the Envoy listener's Legion agent registry:
// every claim registration recorded as it happens (Recorder), the conversations of earlier releases
// found by their titles at boot (Backfill), and, while the answer loop is configured, every record
// written to the registry (Mirror), beside the project's `answering` heartbeat, which says whether
// the loop runs. The registry is how Envoy and Dispatch learn that a stopped session was a Legion
// agent whose daemon answers for it.
package agenthistory

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Store is the history table as this package writes and reads it (store.Store).
type Store interface {
	RegisterConversation(ctx context.Context, c store.AgentConversation) (store.AgentConversation, error)
	ScanConversation(ctx context.Context, c store.AgentConversation) (recorded bool, err error)
	AgentConversations(ctx context.Context, project string) ([]store.AgentConversation, error)
}

// Title is the title a Legion agent's session gives itself (packages/pi-legion/src/session-title.ts,
// and the Claude bridge's `/rename`, internal/claudebridge): `Legion <role> · <ISSUE>`, which for a
// root architect, whose issue is its tree, is `Legion architect · <tree>`.
func Title(role claim.Role, issue string) string { return "Legion " + string(role) + " · " + issue }

// Recorder records every registration a claim persists (supervise.Deps.Registered) as a
// registered conversation, and hands the row to Mirror when the answer loop is configured.
type Recorder struct {
	Store Store
	// Harness is the harness a role runs on (config.Config.BackendFor): the registering process
	// runs on it.
	Harness func(claim.Role) runtime.Backend
	Now     func() time.Time
	// Mirror is the registry mirror, nil when the answer loop is not configured.
	Mirror *Mirror
	Log    *slog.Logger
}

// Registered records c's registration. A claim on no issue role (the daemon's own controller) is
// no agent of an issue, and one with no session has nothing to record. A failed write is logged:
// the registration it records has already been persisted, and refusing it now would leave the
// agent without the answer its registration is owed.
func (r Recorder) Registered(ctx context.Context, c supervise.Claim) {
	if !claim.IsRole(c.Role) || c.Session == "" {
		return
	}
	now := r.Now().UTC()
	written, err := r.Store.RegisterConversation(ctx, store.AgentConversation{
		Session: c.Session, Project: c.Project, Issue: c.Issue, Role: c.Role, Token: c.Token,
		Harness: r.Harness(c.Role), Location: c.SessionFile, StartedAt: now, RecordedAt: now,
	})
	if err != nil {
		r.Log.Error("legion daemon could not record the registered conversation", "claim", c.Token, "session", c.Session, "error", err)
		return
	}
	if r.Mirror != nil {
		r.Mirror.Put(written)
	}
}
