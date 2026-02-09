# Skills Layer

OpenCode skills that orchestrate the autonomous development loop. These are markdown files loaded by OpenCode agents — they contain instructions, not code.

## How Skills Invoke TypeScript

| Interface | Direction | Example |
|-----------|-----------|---------|
| HTTP API | Controller → Daemon | `curl POST http://127.0.0.1:$LEGION_DAEMON_PORT/workers` |
| Piped CLI | Controller → State | `echo $JSON \| bun run packages/daemon/src/state/cli.ts --team-id X` |
| Env vars | Daemon → Worker | `LINEAR_ISSUE_ID`, `LEGION_DIR`, etc. |
| Linear skill | Worker → Linear | `linear_linear(action="get"\|"update"\|"comment"\|"create"\|"search")` |

## Structure

```
skills/
├── linear/
│   └── SKILL.md          # Stream Linear MCP (embedded) — single tool, action dispatch
├── legion-controller/
│   └── SKILL.md          # Persistent loop: fetch → decide → dispatch → sleep 30s
└── legion-worker/
    ├── SKILL.md           # Router: reads mode, delegates to workflow
    ├── workflows/
    │   ├── architect.md   # Break down vague issues into spec-ready sub-issues
    │   ├── plan.md        # Create executable implementation plans (with review iterations)
    │   ├── implement.md   # TDD-driven coding, PR creation
    │   ├── review.md      # Deep PR review with line-level comments
    │   ├── retro.md       # Dual-perspective retrospective → docs/solutions/
    │   ├── merge.md       # Merge PR, handle CI, cleanup workspace
    │   └── oracle.md      # Research institutional knowledge before escalating to human
    └── references/
        └── linear-labels.md  # Label conventions and MCP update patterns
```

## Environment Variables

Set by daemon when spawning workers, consumed by skills:

| Variable | Set By | Used By | Purpose |
|----------|--------|---------|---------|
| `LINEAR_TEAM_ID` | CLI/daemon | Controller + workers | Linear team UUID |
| `LEGION_DIR` | CLI/daemon | Controller + workers | Default jj workspace path |
| `LEGION_SHORT_ID` | Daemon | Controller | Instance ID for heartbeat |
| `LEGION_DAEMON_PORT` | Daemon | Controller | HTTP API port (default 13370) |
| `LINEAR_ISSUE_ID` | Daemon | Workers | Issue identifier (e.g., `LEG-18`) |

## Worker Lifecycle (SKILL.md)

1. **Start**: `jj git fetch && jj rebase -d main && jj new`
2. **Work**: Execute workflow for the assigned mode
3. **Block**: If stuck, try oracle first. If still stuck: push, post Linear comment, add `user-input-needed`, remove `worker-active`, exit
4. **Done**: `jj git push`, add `worker-done` (most modes), remove `worker-active`

## Dispatch vs Resume

- **Dispatch** = `POST /workers` → new OpenCode serve process + `prompt_async`
- **Resume** = `POST /session/{id}/prompt_async` on existing worker port

Resume is used for: user feedback relay, PR changes requested, retro after review.
