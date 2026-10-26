// Package claude reads a Claude Code transcript into the provider-neutral conversation model as
// the pinned Claude Code (PinnedVersion) rebuilds it on --resume, and appends a question and its
// answer to one as Claude Code writes lines.
//
// A transcript is Claude Code's JSONL under <CLAUDE_CONFIG_DIR>/projects/<cwd>/<session>.jsonl:
// one record per line, the conversation's lines (user, assistant, attachment, system) each naming
// its parent by uuid, beside metadata lines (titles, the last-prompt leaf checkpoint, file
// history, request records). The reader loads it as Claude Code's transcript loader does (progress
// lines re-parented away, sidechain lines left out, the last compaction boundary's preserved
// messages re-linked after its summary and everything before it dropped), picks the leaf a resume
// continues from, builds the chain from the root to it (the branches parallel tool calls leave
// put back), and shapes it as the request Claude Code sends: attachments, Claude Code's own
// context (environment, model identity, tool, agent and skill listings, the date, commit
// attribution guidance), kept out but for a prompt queued during a turn; a resumed turn that did
// not end closed as Claude Code records it; API error records dropped; the lines of one assistant
// message merged; a message whose tool calls the next prompt abandoned dropped; consecutive user
// messages merged. Like Claude Code it skips a line that is not a JSON object, a torn last line
// among them; unlike it, it refuses a well-formed line of a type, system subtype or content block
// it does not know (conversation.ErrUnknownFormat).
//
// No system message is in the neutral model, so what Claude Code sends as system-role messages
// (its context, a queued prompt's notice) is not rebuilt; a queued prompt is a user message of its
// own words.
package claude

import (
	"fmt"
	"os"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// PinnedVersion is the Claude Code release the reader and appender follow, the one the Tests
// workflow installs for the real-binary tests (.github/actions/install-claude-code).
const PinnedVersion = "2.1.310"

// anthropicMessages is the provider API every Claude Code turn runs on.
const anthropicMessages = "anthropic-messages"

// Session is a transcript read: its conversation as Claude Code rebuilds it on resume, and what an
// append after it needs.
type Session struct {
	conversation.Conversation
	// ID, Cwd, Version and GitBranch are the last conversation line's: the session, its working
	// directory, the Claude Code release and the git branch it ran on.
	ID        string
	Cwd       string
	Version   string
	GitBranch string
	// Title is the session's title as `/rename` records it (its last custom-title line), empty
	// when it has none.
	Title string
	// Leaf is the uuid of the chain's last line, which an appended line names as its parent; empty
	// for a transcript of no conversation.
	Leaf string
	// Size is the length in bytes of the content read; an append is written only after exactly
	// that content.
	Size int64
	// Torn is whether the content does not end in a newline (a line cut off as it was written): an
	// append then starts with one, leaving the torn line a malformed one Claude Code skips.
	Torn bool
}

// Read reads a transcript's content.
func Read(content []byte) (*Session, error) {
	entries, err := parseLines(content)
	if err != nil {
		return nil, err
	}
	l, err := load(entries)
	if err != nil {
		return nil, err
	}
	s := &Session{
		Title: l.title, Size: int64(len(content)), Torn: len(content) > 0 && content[len(content)-1] != '\n',
		Conversation: conversation.Conversation{API: anthropicMessages, ThinkingLevel: l.effort},
	}
	if l.model != "" {
		s.Model = "anthropic/" + l.model
	}
	for _, e := range entries {
		if transcriptTypes[e.Type] && !e.IsSidechain {
			s.ID, s.Cwd, s.Version, s.GitBranch = e.SessionID, e.Cwd, e.Version, e.GitBranch
		}
	}
	leaf, err := l.leaf(l.relink())
	if err != nil {
		return nil, err
	}
	if leaf == nil {
		return s, nil
	}
	chain := l.chain(leaf)
	s.Leaf = chain[len(chain)-1].UUID
	if s.Messages, err = buildRequest(chain); err != nil {
		return nil, err
	}
	return s, nil
}

// ReadFile reads the transcript at path.
func ReadFile(path string) (*Session, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Read(content)
	if err != nil {
		return nil, fmt.Errorf("transcript %s: %w", path, err)
	}
	return s, nil
}
