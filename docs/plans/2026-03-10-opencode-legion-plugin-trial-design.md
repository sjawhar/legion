# opencode-legion Plugin Trial via Isolated Config

**Date:** 2026-03-10
**Status:** Approved
**Goal:** Run legion workers under the opencode-legion plugin (instead of oh-my-opencode) using an isolated config directory, with a live Sisyphus session as the external controller.

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│ SISYPHUS SESSION (External Controller)                    │
│ Config: ~/.config/opencode/ (UNCHANGED, oh-my-opencode)   │
│ Role: Hybrid controller — runs loop, user interrupts      │
│ Communicates with daemon via HTTP on port 13370            │
└────────────────────────┬─────────────────────────────────┘
                         │ HTTP calls (curl / skill)
                         ▼
┌──────────────────────────────────────────────────────────┐
│ DAEMON (legion start, background process)                 │
│ Env: LEGION_CONTROLLER_SESSION_ID=ses_external_trial      │
│ Thin substrate: state machine, worker tracking, HTTP API  │
│ Flags: -b github                                          │
└────────────────────────┬─────────────────────────────────┘
                         │ manages
                         ▼
┌──────────────────────────────────────────────────────────┐
│ SHARED SERVE (opencode serve, port 13381)                 │
│ Env: XDG_CONFIG_HOME=~/.legion/trial-config               │
│ Reads config from: ~/.legion/trial-config/opencode/       │
│   → opencode-legion plugin (NOT oh-my-opencode)           │
│   → essential skills only                                 │
│                                                           │
│ ┌────────────┐ ┌────────────┐ ┌────────────┐            │
│ │  Worker 1  │ │  Worker 2  │ │  Worker N  │            │
│ └────────────┘ └────────────┘ └────────────┘            │
└──────────────────────────────────────────────────────────┘
```

### Key Insight

The external controller communicates with workers ONLY through the daemon HTTP API — never directly through the shared serve. Controller config and worker config are completely independent.

## Isolated Config Directory

Location: `~/.legion/trial-config/opencode/`

### Real files (created)

**`opencode.json`** — Verbatim copy of `~/.config/opencode/opencode.json` with one change:

```diff
- "file://{env:HOME}/oh-my-opencode/original",
+ "file:///home/ubuntu/legion/default/packages/opencode-plugin",
```

All other sections (keybinds, provider, instructions, mcp, autoupdate) copied verbatim.

**`opencode-legion.json`** — Plugin config ported from current oh-my-opencode.json:

```json
{
  "agents": {
    "orchestrator": { "model": "anthropic/claude-opus-4-6" },
    "executor": { "model": "anthropic/claude-sonnet-4-6" },
    "explorer": { "model": "anthropic/claude-haiku-4-5" },
    "librarian": { "model": "anthropic/claude-sonnet-4-6" },
    "oracle": { "model": "openai/gpt-5.4" },
    "metis": { "model": "openai/gpt-5.4" },
    "momus": { "model": "openai/gpt-5.4" }
  },
  "categories": {
    "ultrabrain": { "defaultModel": "anthropic/claude-opus-4-6" },
    "deep": { "defaultModel": "openai/gpt-5.3-codex" },
    "visual-engineering": { "defaultModel": "google/gemini-3.1-pro-preview" },
    "quick": { "defaultModel": "anthropic/claude-haiku-4-5" },
    "unspecified-low": { "defaultModel": "anthropic/claude-sonnet-4-6" },
    "unspecified-high": { "defaultModel": "anthropic/claude-opus-4-6" },
    "writing": { "defaultModel": "google/gemini-3-flash-preview" },
    "artistry": { "defaultModel": "google/gemini-3.1-pro-preview" }
  }
}
```

### Symlinks (point to existing files)

| Symlink | Target | Purpose |
|---------|--------|---------|
| `AGENTS.md` | `~/.dotfiles/.claude/CLAUDE.md` | Agent instructions |
| `mcp-oauth.json` | `~/.config/opencode/mcp-oauth.json` | MCP auth tokens |
| `package.json` | `~/.config/opencode/package.json` | @opencode-ai/plugin dep |
| `node_modules/` | `~/.config/opencode/node_modules/` | Installed deps |
| `bun.lock` | `~/.config/opencode/bun.lock` | Lockfile |
| `skills/superpowers` | `~/.dotfiles/vendor/superpowers/skills` | Superpowers skills |
| `skills/legion` | `~/.dotfiles/vendor/legion/.opencode/skills` | Legion skills |
| `skills/using-jj` | `~/.dotfiles/plugins/sjawhar/skills/using-jj` | jj VCS skill |
| `skills/github` | `~/legion/default/.opencode/skills/github` | GitHub skill |
| `skills/linear` | `~/legion/default/.opencode/skills/linear` | Linear skill |
| `plugins/superpowers.js` | `~/.dotfiles/vendor/superpowers/.opencode/plugins/superpowers.js` | Superpowers plugin |

### Intentionally absent

- `oh-my-opencode.json` — not needed, plugin removed
- Non-essential skills (ce, sjawhar, sentry-for-ai) — workers don't need them

## Prerequisites

1. **Build opencode-legion plugin** — `dist/` directory doesn't exist yet:
   ```bash
   cd packages/opencode-plugin && bun run build
   ```

## Startup Sequence

```bash
# 1. Build plugin (one-time)
cd ~/legion/default/packages/opencode-plugin && bun run build

# 2. Run setup script (creates isolated config dir)
# (script creates ~/.legion/trial-config/opencode/ with files + symlinks)

# 3. Start daemon
XDG_CONFIG_HOME=$HOME/.legion/trial-config \
LEGION_CONTROLLER_SESSION_ID=ses_external_trial \
legion start <team> -b github -w ~/legion/default
```

Then in the Sisyphus session:
1. Set `LEGION_DAEMON_PORT=13370` (env or hardcoded in curl calls)
2. Invoke `/legion-controller` skill for hybrid loop
3. User can interrupt and issue manual overrides at any time

## Teardown / Rollback

```bash
legion stop <team>              # Stop daemon + shared serve + all workers
rm -rf ~/.legion/trial-config   # Delete isolated config
```

Real `~/.config/opencode/` is **never modified**.

## Risks & Mitigations

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| opencode-legion plugin fails to load | Medium | Pre-validate: run `XDG_CONFIG_HOME=~/.legion/trial-config opencode debug config` before starting daemon |
| Workers miss a skill | Low | Essential skills included; can add more symlinks without restart |
| Auth/API keys not available | Low | Auth plugins in opencode.json plugin list; mcp-oauth.json symlinked |
| Plugin has runtime bugs | Medium | This is the whole point of the trial — catch issues with Sisyphus as controller for live debugging |
| Model assignments don't match | Low | Ported from oh-my-opencode.json; verify with `opencode debug agent` under trial config |

## Success Criteria

1. Daemon starts cleanly with external controller
2. Workers load opencode-legion plugin (visible in serve logs)
3. At least one worker completes a full workflow (dispatch → implement → done)
4. No regressions vs oh-my-opencode behavior for worker tasks
