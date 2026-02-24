# Skills Layer

OpenCode skills that orchestrate the autonomous development loop. These are markdown files loaded by OpenCode agents — they contain instructions, not code.

## How Skills Invoke TypeScript

| Interface | Direction | Example |
|-----------|-----------|---------|
| HTTP API | Controller → Daemon | `curl -X POST http://127.0.0.1:$LEGION_DAEMON_PORT/state/collect` |
| Piped CLI (legacy) | Controller → State | `echo $JSON \| bun run packages/daemon/src/state/cli.ts --team-id X` — being replaced by `POST /state/collect` |
| Env vars | Daemon → Controller | `LEGION_TEAM_ID`, `LEGION_DAEMON_PORT`, etc. |
| Prompt context | Controller → Worker | Issue ID, mode, backend passed in dispatch/resume prompt text |
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

Process-level env vars inherited by the shared serve process. These configure the **controller** —
workers receive all context (issue ID, mode, backend) via the dispatch prompt, not env vars.

| Variable | Set By | Used By | Purpose |
|----------|--------|---------|---------|
| `LEGION_TEAM_ID` | CLI/daemon | Controller | Team/project identifier (Linear UUID or GitHub `owner/project-number`) |
| `LEGION_DIR` | CLI/daemon | Controller | Default workspace path |
| `LEGION_SHORT_ID` | CLI/daemon | Controller | Instance ID for heartbeat |
| `LEGION_DAEMON_PORT` | Daemon | Controller | HTTP API port (default 13370) |
| `LEGION_ISSUE_BACKEND` | CLI/daemon | Controller | Issue backend (`linear` or `github`) |
| `LEGION_VCS` | CLI/daemon | Controller | Version control system (`jj` or `git`, auto-detected from `.jj` dir, default `git`) |

All sessions on the shared serve share the same process environment. The controller includes
backend and issue identity in every dispatch/resume prompt so workers are self-contained.

## Worker Lifecycle (SKILL.md)

1. **Start**: Sync with main (jj: `jj git fetch && jj rebase -d main`, git: `git fetch origin && git rebase origin/main`)
2. **Work**: Execute workflow for the assigned mode
3. **Block**: If stuck, try `/legion-oracle` first. If still stuck: push, post issue comment, add `user-input-needed`, remove `worker-active`, exit
4. **Done**: Push (jj: `jj git push`, git: `git push`), add `worker-done` (most modes), remove `worker-active`

## Dispatch vs Resume

- **Dispatch** = `POST /workers` → new session on shared serve (idempotent, deterministic session ID)
- **Resume** = `POST /session/{id}/prompt_async` on shared serve

Resume is used for: user feedback relay, PR changes requested, retro via `/legion-retro` after review.
