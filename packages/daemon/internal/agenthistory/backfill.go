package agenthistory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/conversation/claude"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Backfill records, at boot, the conversations the daemon's agents had before their registrations
// were recorded, each found by the title its session gave itself (Title). A conversation is
// recorded, as a scanned one, only when its title names an issue role and issue (never the
// controller's), it is no subagent's, and the session itself claimed that role's token for this
// project: an Oh My Pi session's last envoy-role-claim entry, or the claim directory a Claude Code
// transcript lies under. Every other conversation is skipped with one log line naming its path and
// why. A conversation already recorded is never rewritten (store.Store.ScanConversation).
type Backfill struct {
	// Project is the Legion project token (claim.ProjectToken) every token is built with.
	Project string
	Store   Store
	Now     func() time.Time
	Log     *slog.Logger
}

// Table backfills from Oh My Pi's session table (omp_session_files) under a database store: each
// session's title from the row's title column or its content's head (ompsessions.Titles), and its
// role claim from its whole content, read only for a session whose title and header qualify it.
func (b Backfill) Table(ctx context.Context, conn *pgx.Conn) error {
	titled, err := ompsessions.Titles(ctx, conn)
	if err != nil {
		return err
	}
	for _, session := range titled {
		role, issue, why := b.qualify(session.Description)
		if why != "" {
			b.skip(session.Path, why)
			continue
		}
		content, found, err := ompsessions.Read(ctx, conn, session.Path)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.skip(session.Path, fmt.Sprintf("its content could not be read: %v", err))
			continue
		case !found:
			b.skip(session.Path, "the session table no longer holds it")
			continue
		}
		d := session.Description
		d.RoleClaim = ompsessions.Describe(content).RoleClaim
		if err := b.recordOMP(ctx, session.Path, d, role, issue); err != nil {
			return err
		}
	}
	return nil
}

// Files backfills from the Oh My Pi session files under dir, the pane home's sessions directory
// (`<profile root>/agent/sessions`, ompdirs.ProfileRoot over the pane environment), at any depth: a
// subagent's session lies in its parent's directory and is skipped as one. A dir that does not
// exist holds nothing.
func (b Backfill) Files(ctx context.Context, dir string) error {
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == dir {
				return filepath.SkipDir
			}
			b.skip(path, fmt.Sprintf("it could not be listed: %v", err))
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		head, err := ompsessions.DescribeFile(path, false)
		if err != nil {
			b.skip(path, fmt.Sprintf("it could not be read: %v", err))
			return nil
		}
		role, issue, why := b.qualify(head)
		if why != "" {
			b.skip(path, why)
			return nil
		}
		whole, err := ompsessions.DescribeFile(path, true)
		if err != nil {
			b.skip(path, fmt.Sprintf("it could not be read: %v", err))
			return nil
		}
		return b.recordOMP(ctx, path, whole, role, issue)
	})
	if err != nil {
		return fmt.Errorf("backfill the Oh My Pi sessions under %s: %w", dir, err)
	}
	return nil
}

// Claude backfills from the Claude Code transcripts under root, the state directory's `claude`
// (runtime/tmux ClaudeConfigDir): `<root>/<claim token>/projects/<cwd>/<session>.jsonl`, where the
// claim directory is the token the transcript's title must name. A root that does not exist holds
// nothing.
func (b Backfill) Claude(ctx context.Context, root string) error {
	transcripts, err := filepath.Glob(filepath.Join(root, "*", "projects", "*", "*.jsonl"))
	if err != nil {
		return fmt.Errorf("backfill the Claude Code transcripts under %s: %w", root, err)
	}
	for _, path := range transcripts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		directory := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
		d, err := claude.DescribeFile(path)
		if err != nil {
			b.skip(path, fmt.Sprintf("it could not be read: %v", err))
			continue
		}
		role, issue, why := parseTitle(d.Title)
		if why != "" {
			b.skip(path, why)
			continue
		}
		if d.SessionID == "" {
			b.skip(path, "it holds no conversation line naming its session")
			continue
		}
		started, err := time.Parse(time.RFC3339Nano, d.Timestamp)
		if err != nil {
			b.skip(path, fmt.Sprintf("its first conversation line's timestamp %q is not a time", d.Timestamp))
			continue
		}
		token, err := claim.NewToken(b.Project, issue, role)
		if err != nil {
			b.skip(path, fmt.Sprintf("its title names no role token of project %s: %v", b.Project, err))
			continue
		}
		if directory != string(token) {
			b.skip(path, fmt.Sprintf("it lies under the claim directory %s, not %s, which its title names", directory, token))
			continue
		}
		if err := b.record(ctx, store.AgentConversation{
			Session: d.SessionID, Project: b.Project, Issue: issue, Role: role, Token: token,
			Harness: runtime.BackendClaude, Location: path, StartedAt: started,
		}); err != nil {
			return err
		}
	}
	return nil
}

// qualify is what an Oh My Pi session's title and header say before its role claim is read: the
// role and issue its title names, or why it is skipped.
func (b Backfill) qualify(d ompsessions.Description) (role claim.Role, issue, why string) {
	role, issue, why = parseTitle(d.Title)
	switch {
	case why != "":
	case d.ParentSession != "":
		why = fmt.Sprintf("it is a subagent's session (its parent is %s)", d.ParentSession)
	case d.ID == "":
		why = "it has no session header naming its id"
	}
	return role, issue, why
}

// recordOMP records the qualified Oh My Pi session at path, whose whole content d describes, once
// its role claim names the token its title does.
func (b Backfill) recordOMP(ctx context.Context, path string, d ompsessions.Description, role claim.Role, issue string) error {
	token, err := claim.NewToken(b.Project, issue, role)
	if err != nil {
		b.skip(path, fmt.Sprintf("its title names no role token of project %s: %v", b.Project, err))
		return nil
	}
	if d.RoleClaim != string(token) {
		claimed := "no role"
		if d.RoleClaim != "" {
			claimed = d.RoleClaim
		}
		b.skip(path, fmt.Sprintf("its envoy-role-claim names %s, not %s, which its title names", claimed, token))
		return nil
	}
	started, err := time.Parse(time.RFC3339Nano, d.Timestamp)
	if err != nil {
		b.skip(path, fmt.Sprintf("its header's timestamp %q is not a time", d.Timestamp))
		return nil
	}
	return b.record(ctx, store.AgentConversation{
		Session: d.ID, Project: b.Project, Issue: issue, Role: role, Token: token,
		Harness: runtime.BackendOmp, Location: path, StartedAt: started,
	})
}

func (b Backfill) record(ctx context.Context, c store.AgentConversation) error {
	c.StartedAt, c.RecordedAt = c.StartedAt.UTC(), b.Now().UTC()
	recorded, err := b.Store.ScanConversation(ctx, c)
	if err != nil {
		return err
	}
	if recorded {
		b.Log.Info("legion daemon recorded a conversation found by its title", "session", c.Session, "claim", c.Token, "path", c.Location)
	}
	return nil
}

func (b Backfill) skip(path, why string) {
	b.Log.Info("legion daemon's conversation backfill skipped a session: "+why, "path", path)
}

// parseTitle is the issue role and issue a Legion agent's title names (Title), or why the title
// names none: an untitled session, a title of no Legion agent, and the controller's own.
func parseTitle(title string) (role claim.Role, issue, why string) {
	if title == "" {
		return "", "", "it has no title"
	}
	rest, legion := strings.CutPrefix(title, "Legion ")
	named, key, cut := strings.Cut(rest, " · ")
	switch {
	case legion && cut && claim.Role(named) == claim.RoleController:
		return "", "", fmt.Sprintf("it is a controller's session (title %q)", title)
	case !legion || !cut || !claim.IsRole(claim.Role(named)) || !claim.IsIssueKey(key):
		return "", "", fmt.Sprintf("its title %q names no Legion role and issue", title)
	}
	return claim.Role(named), key, ""
}

// OMPSessionsDir is where an Oh My Pi whose profile root (ompdirs.ProfileRoot) is root keeps its
// sessions.
func OMPSessionsDir(root string) string { return filepath.Join(root, "agent", "sessions") }
