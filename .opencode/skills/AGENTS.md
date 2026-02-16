# Skills Layer

OpenCode skills that orchestrate the autonomous development loop. These are markdown files loaded by OpenCode agents — they contain instructions, not code.

## How Skills Invoke TypeScript

| Interface | Direction | Example |
|-----------|-----------|---------|
| HTTP API | Controller → Daemon | `curl -X POST http://127.0.0.1:$LEGION_DAEMON_PORT/state/collect` |
| Piped CLI | Controller → State | `echo $JSON \| bun run packages/daemon/src/state/cli.ts --team-id X` |
| Env vars | Daemon → Worker | `LEGION_ISSUE_ID`, `LEGION_ISSUE_BACKEND`, `LEGION_DIR`, etc. |
| Issue backend | Worker → Linear/GitHub | `linear_linear(action="get"\|"update"\|"comment"\|"create"\|"search")` or `gh issue view/edit/comment` |

## Structure

```
skills/
├── github/
│   └── SKILL.md          # GitHub CLI skill (embedded) — issue + PR operations
├── linear/
│   └── SKILL.md          # Linear MCP (embedded) — single tool, action dispatch
├── legion-controller/
│   └── SKILL.md          # Persistent loop: fetch → decide → dispatch → sleep 30s
├── legion-retro/
│   └── SKILL.md          # Dual-perspective retrospective → docs/solutions/
├── legion-oracle/
│   └── SKILL.md          # Research institutional knowledge before escalating to human
└── legion-worker/
    ├── SKILL.md           # Router: reads mode, delegates to workflow
    ├── workflows/
    │   ├── architect.md   # Break down vague issues into spec-ready sub-issues
    │   ├── plan.md        # Create executable implementation plans (with review iterations)
    │   ├── implement.md   # TDD-driven coding, PR creation
    │   ├── review.md      # Deep PR review with line-level comments
    │   └── merge.md       # Merge PR, handle CI, cleanup workspace
    └── references/
        ├── github-labels.md  # GitHub label conventions (add/remove)
        └── linear-labels.md  # Linear label conventions and MCP update patterns
```

## Environment Variables

Set by daemon when spawning workers, consumed by skills:

| Variable | Set By | Used By | Purpose |
|----------|--------|---------|---------|
| `LEGION_TEAM_ID` | CLI/daemon | Controller + workers | Team/project identifier (Linear UUID or GitHub `owner/project-number`) |
| `LEGION_DIR` | CLI/daemon | Controller + workers | Default jj workspace path |
| `LEGION_SHORT_ID` | Daemon | Controller | Instance ID for heartbeat |
| `LEGION_DAEMON_PORT` | Daemon | Controller | HTTP API port (default 13370) |
| `LEGION_ISSUE_ID` | Daemon | Workers | Issue identifier (e.g., `LEG-18`) |
| `LEGION_ISSUE_BACKEND` | Daemon | Workers | Issue backend (`linear` or `github`) |

## Worker Lifecycle (SKILL.md)

1. **Start**: `jj git fetch && jj rebase -d main && jj new`
2. **Work**: Execute workflow for the assigned mode
3. **Block**: If stuck, try `/legion-oracle` first. If still stuck: push, post issue comment, add `user-input-needed`, remove `worker-active`, exit
4. **Done**: `jj git push`, add `worker-done` (most modes), remove `worker-active`

## Dispatch vs Resume

- **Dispatch** = `POST /workers` → new session on shared serve (idempotent, deterministic session ID)
- **Resume** = `POST /session/{id}/prompt_async` on shared serve

Resume is used for: user feedback relay, PR changes requested, retro via `/legion-retro` after review.
