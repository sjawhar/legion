# Legion

Autonomous development swarm using Claude Code agents. Workers implement Linear issues in isolated jj workspaces, coordinated by a controller daemon.

## Project Structure

```
legion/
├── src/legion/           # Core Python package
│   ├── cli.py           # Click CLI entrypoint
│   ├── daemon.py        # Controller daemon loop
│   ├── state/           # Issue state machine
│   ├── tmux.py          # Session management
│   └── short_id.py      # Legion instance IDs
├── skills/              # Claude Code skills
│   ├── legion-controller/  # Polls Linear, dispatches workers
│   └── legion-worker/      # Implements issues (plan/implement/review/retro/finish)
├── hooks/               # Claude Code hooks
├── tests/               # pytest test suite
└── docs/
    ├── plans/           # Implementation plans
    ├── brainstorms/     # Design explorations
    ├── solutions/       # Documented learnings
    └── research/        # Tool evaluations
```

## Tech Stack

- **Python 3.13+** with async (anyio, aiofiles)
- **Click** for CLI
- **uv** for package management
- **pytest** for testing
- **jj (Jujutsu)** for version control with workspaces
- **Linear MCP** for issue tracking
- **tmux** for worker session management

## Development

### Setup

```bash
uv sync
```

### Testing

```bash
uv run pytest
```

### Version Control

This project uses jj (Jujutsu), not git:

| Task | Command |
|------|---------|
| Status | `jj status` |
| Log | `jj log` |
| Diff | `jj diff` |
| Push | `jj git push` |
| Fetch | `jj git fetch` |

Changes auto-accumulate in the working copy. Push directly to feature branches.

## Architecture

### Flow

```
Linear Issue → Controller → Worker (in jj workspace) → PR → Review → Merge
```

### Issue Lifecycle

```
Todo → In Progress → Needs Review → Retro → Done
         ↑              │
         └──────────────┘
         (changes requested)
```

### Worker Modes

| Mode | Phase | Output |
|------|-------|--------|
| `plan` | Todo | Plan posted to Linear |
| `implement` | In Progress | PR opened |
| `review` | Needs Review | PR labeled |
| `retro` | Retro | Learnings documented |
| `finish` | Done | PR merged, workspace cleaned |

### Labels

**Linear:**
- `worker-done` - Worker signals completion
- `user-input-needed` - Waiting for human
- `user-feedback-given` - Human answered

**GitHub PR:**
- `worker-approved` - Review passed
- `worker-changes-requested` - Review found issues

## Conventions

### Python Style

Follows [Google Python Style Guide](https://google.github.io/styleguide/pyguide.html) with these specifics:

**Imports:**
- No relative imports - always use absolute: `from legion import daemon`
- Import modules, not functions/classes: `from legion import tmux` then `tmux.run()`
- Exceptions: `typing` module, `legion.state.types` (for type annotations)

**Code:**
- Async-first for I/O operations
- Type hints on public interfaces
- Docstrings for non-obvious functions

### Skills

Skills live in `skills/<name>/SKILL.md`. Workflows are in `skills/<name>/workflows/`.

### Documentation

- Plans go in `docs/plans/YYYY-MM-DD-<slug>.md`
- Learnings go in `docs/solutions/<category>/<slug>.md`

## Environment Variables

| Variable | Description |
|----------|-------------|
| `LINEAR_TEAM_ID` | Linear team UUID |
| `LEGION_DIR` | Path to default jj workspace |
| `LEGION_SHORT_ID` | Short ID for tmux sessions |
| `LINEAR_ISSUE_ID` | Current issue (workers only) |
| `WORKSPACE_DIR` | Worker's jj workspace path |
