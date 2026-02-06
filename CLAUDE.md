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
│   └── legion-worker/      # Implements issues (plan/implement/review/retro/merge)
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
- **ruff** for linting and formatting
- **basedpyright** for type checking
- **jj (Jujutsu)** for version control with workspaces
- **Linear MCP** for issue tracking
- **tmux** for worker session management

## Development

### Setup

```bash
uv sync
```

### Required Claude Code Plugins

Legion workers use skills and agents from these plugins:

| Plugin | Purpose |
|--------|---------|
| `superpowers@claude-plugins-official` | TDD, debugging, workflows |
| `compound-engineering@every-marketplace` | Research agents, review agents |

Install with:
```bash
legion install
```

**Note:** Context7 (for framework documentation lookup) is bundled in Legion's plugin config.

### Quality Checks

```bash
uv run ruff check .        # Lint
uv run ruff format --check # Format check
uv run basedpyright .      # Type check
uv run pytest              # Test
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
Triage ──┬──► Icebox ──► Backlog ──► Todo ──► In Progress ──► Needs Review ──► Retro ──► Done
         │                  ^           ^            ^               │
         │                  │           │            │               │
         ├──────────────────┘           │            └───────────────┘
         │   (already spec-ready)       │            (changes requested)
         │                              │
         └──────────────────────────────┘
                    (urgent + clear)
```

### Worker Modes

| Mode | Phase | Output |
|------|-------|--------|
| `architect` | Backlog | Spec-ready issue or sub-issues |
| `plan` | Todo | Plan posted to Linear |
| `implement` | In Progress | PR opened |
| `review` | Needs Review | PR labeled |
| `retro` | Retro | Learnings documented |
| `merge` | Done | PR merged, workspace cleaned |

### Labels (Linear)

- `worker-done` - Worker signals completion
- `worker-active` - Worker dispatched and running
- `user-input-needed` - Waiting for human
- `user-feedback-given` - Human answered

Review outcomes use **PR draft status** (not labels): draft = changes requested, ready = approved.

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

## Usage

```bash
# Start the swarm for a Linear team (key or UUID)
legion start <team> [--workspace /path/to/repo]

# Check status
legion status <team>

# Stop the swarm
legion stop <team>

# Cache team key → UUID mappings
legion teams
```

The daemon automatically passes context to spawned processes via environment variables (internal implementation detail - users don't set these manually).
