# Claude Code Agent Teams Research

**Date:** 2026-02-06
**Agent:** Claude Code teams feature researcher
**Focus:** Anthropic's Agent Teams feature, multi-agent coordination patterns, hooks, skills

---

## 1. Claude Code's Team/Swarm Feature

### Overview
Anthropic released the **Agent Teams** feature with Claude Opus 4.6 on February 5, 2026. Experimental feature (disabled by default).

**Enable with:**
```json
{
  "env": {
    "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1"
  }
}
```

### Architecture

| Component | Role |
|-----------|------|
| **Team Lead** | Main Claude Code session that creates team, spawns teammates, coordinates |
| **Teammates** | Separate Claude Code instances, each with own context window |
| **Task List** | Shared work queue with dependency tracking and auto-unblocking |
| **Mailbox** | JSON-based messaging for inter-agent communication |

**Storage locations:**
- Team config: `~/.claude/teams/{team-name}/config.json`
- Task list: `~/.claude/tasks/{team-name}/`

### Coordination Model

**Communication Methods:**
1. **Direct messaging** (`SendMessage` with `type: "message"`) - One-to-one
2. **Broadcast** (`type: "broadcast"`) - All teammates (expensive, scales with team size)
3. **Structured messages** - shutdown_request/response, plan_approval_request/response, idle_notification, task_completed

**Key principle:** Teammates' text output is NOT visible to others. Must use `SendMessage` explicitly.

**Task Coordination:**
- **TaskCreate** - Generate work items with dependencies
- **TaskUpdate** - Manage status (pending → in progress → completed)
- **Auto-unblocking** - Completed tasks auto-unblock dependents
- **Self-claiming** - Teammates claim next available unblocked task
- **Lock-based claiming** - File locking prevents race conditions

### Display Modes

1. **In-process** (default) - All teammates in main terminal. Shift+Up/Down to select.
2. **Split panes** - Each teammate in own pane. Requires tmux or iTerm2.

### Context Management

Each teammate:
- Isolated context window (no shared history)
- Loads project context automatically (CLAUDE.md, MCP servers, skills)
- Receives spawn prompt from lead
- Does NOT inherit lead's conversation history

### Limitations

1. No session resumption (`/resume` and `/rewind` don't restore teammates)
2. Task status lag (teammates sometimes fail to mark tasks complete)
3. Slow shutdown (agents finish current request before stopping)
4. One team per session
5. No nested teams (teammates cannot spawn their own teams)
6. Fixed lead (cannot promote teammate)
7. Permissions set at spawn (all inherit lead's mode)

---

## 2. Workflow Tips from Anthropic

### When to Use Agent Teams

**Best use cases:**
- Research and review — investigate different aspects simultaneously
- New modules/features — each teammate owns separate piece
- Debugging with competing hypotheses — test different theories in parallel
- Cross-layer coordination — frontend, backend, tests each owned by different teammate

**When NOT to use:**
- Sequential tasks
- Same-file edits
- Work with many dependencies
- Routine tasks (single session more cost-effective)

### Subagents vs Agent Teams

| Aspect | Subagents | Agent Teams |
|--------|-----------|-------------|
| Context | Own context, results return to caller | Fully independent |
| Communication | Report back to main agent only | Message each other directly |
| Coordination | Main agent manages all work | Shared task list with self-coordination |
| Best for | Focused tasks where only result matters | Complex work requiring discussion |
| Token cost | Lower (results summarized) | Higher (each teammate separate instance) |

**Rule of thumb:** Subagents for quick focused workers. Teams when teammates need to share findings and challenge each other.

### Task Sizing

- **Too small** — coordination overhead exceeds benefit
- **Too large** — teammates work too long without check-ins
- **Just right** — self-contained units producing clear deliverable
- **Best practice:** 5-6 tasks per teammate

### Plan Approval Mode

Require teammates to plan before implementing:
- Teammate works in read-only plan mode until lead approves
- Lead makes approval decisions autonomously but can be influenced with criteria

### Delegate Mode

Press `Shift+Tab` to restrict lead to coordination-only tools, preventing it from implementing instead of waiting for teammates.

---

## 3. Case Study: C Compiler Project

Anthropic's engineering blog documents building a 100,000-line Rust compiler with 16 Claude agents over 2,000 sessions at $20,000 cost.

**Key learnings:**

1. **Autonomous Task Loop** — Continuous bash loop: "When it finishes one task, it immediately picks up the next"
2. **Parallel specialization** — 16 instances worked simultaneously via lock files
3. **Test quality critical** — Poor test harnesses caused agents to "solve the wrong problem"
4. **Monolithic tasks problematic** — When Linux kernel compilation became single task, all agents hit same bugs
5. **GCC as oracle** — Randomly mixed Claude/GCC compilation to enable parallel bug-fixing
6. **Context window limits** — Agents experienced "time blindness" and context pollution
7. **Agent-centric documentation** — Extensive READMEs updated frequently helped orientation
8. **Structured output** — ERROR messages on single lines for grep
9. **Pre-computed statistics** — `--fast` flag running 1-10% test samples

**Design principle:** "Log all important information to a file so Claude can find it when needed"

---

## 4. Community Experiences

### Git Worktrees Integration

Major trend: Git worktrees used for parallel agent development.
- Multiple Claude sessions work in parallel on different branches
- Each worktree is isolated development environment
- Shares single .git directory
- [ccpm project](https://github.com/automazeio/ccpm): Project management using GitHub Issues and Git worktrees

### Three Spawn Patterns

From [comprehensive gist](https://gist.github.com/kieranklaassen/4f2aba89594a4aea4ad64d753984b2ea):

1. **Subagents** (Task only) - Short-lived, return results synchronously
2. **Teammates** (Task + team_name + name) - Persistent, join teams, access shared tasks
3. **Specialized types** - compound-engineering plugin provides review, research, design agents

### What Works Well

1. Competing hypotheses debugging — 5 agents debate adversarially
2. Parallel code review — security, performance, test coverage examined simultaneously
3. Cross-layer features — frontend/backend/test agents work independently
4. Context isolation — narrow scope for better reasoning
5. Natural checkpoints — phase transitions provide verification
6. Graceful degradation — one agent failure doesn't cascade

### What Doesn't Work Well

1. Token costs — significantly higher than single session
2. Coordination overhead — not worth it for small/sequential tasks
3. File conflicts — requires careful ownership planning
4. Lead doing work — sometimes implements instead of delegating
5. Context pollution — "time blindness" in long sessions
6. Same-file edits — two teammates editing same file leads to overwrites

### MCP Tool Search (January 2026)

Dynamic tool loading — only loads tools into context when needed.
- 85% token overhead reduction (Anthropic engineering)
- 46.9% reduction in practice (51K → 8.5K tokens)

---

## 5. Claude Code Hooks

**Hooks** = Shell commands at specific lifecycle points.

### Available Hook Events

| Event | When | Orchestration use |
|-------|------|-------------------|
| `SessionStart` | Session begins/resumes | Re-inject context after compaction |
| `UserPromptSubmit` | Before processing prompt | Validate/transform requests |
| `PreToolUse` | Before tool executes | Block protected operations |
| `PostToolUse` | After tool succeeds | Auto-format, log, notify |
| `SubagentStart` | Subagent spawned | Initialize agent-specific config |
| `SubagentStop` | Subagent finishes | Collect results, cleanup |
| `Stop` | Claude finishes responding | Verify task completion |
| `PreCompact` | Before context compaction | Save critical state |

### Hook Types

1. **Command hooks** (`type: "command"`) - Shell scripts
2. **Prompt hooks** (`type: "prompt"`) - Single LLM call
3. **Agent hooks** (`type: "agent"`) - Spawn subagent with tool access

---

## 6. Integration Recommendations for Legion

1. **Adopt native Agent Teams API** — built-in task lists, messaging, plan approval
2. **Leverage git worktrees pattern** — Legion already does this with jj
3. **Use hooks for state transitions** — replace 30s polling with event-driven
4. **Map phases to native task system** — dependency tracking + auto-unblocking
5. **Plan approval workflow** — native API for LEG-66 (human approval gate)
6. **Delegate mode** — prevent controller from doing work itself
7. **Monitor token costs** — start with 2-4 agents, track per phase

---

## Sources

- [Anthropic releases Opus 4.6 with new 'agent teams' | TechCrunch](https://techcrunch.com/2026/02/05/anthropic-releases-opus-4-6-with-new-agent-teams/)
- [Orchestrate teams of Claude Code sessions](https://code.claude.com/docs/en/agent-teams)
- [Building a C compiler with parallel Claudes](https://www.anthropic.com/engineering/building-c-compiler)
- [Claude Code Swarm Orchestration Skill](https://gist.github.com/kieranklaassen/4f2aba89594a4aea4ad64d753984b2ea)
- [Automate workflows with hooks](https://code.claude.com/docs/en/hooks-guide)
- [Claude Code's Hidden Multi-Agent System](https://paddo.dev/blog/claude-code-hidden-swarm/)
- [AddyOsmani.com - Claude Code Swarms](https://addyosmani.com/blog/claude-code-agent-teams/)
- [Git Worktrees with Claude Code](https://medium.com/@dtunai/mastering-git-worktrees-with-claude-code-for-parallel-development-workflow-41dc91e645fe)
- [Claude Code Best Practices Playbook](https://claudecn.com/en/blog/claude-code-best-practices-playbook/)
- [Create custom subagents](https://code.claude.com/docs/en/sub-agents)
- [Claude Code 2.1.0 | VentureBeat](https://venturebeat.com/orchestration/claude-code-2-1-0-arrives-with-smoother-workflows-and-smarter-agents)
