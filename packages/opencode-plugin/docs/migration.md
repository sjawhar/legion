# Migrating from oh-my-opencode to opencode-legion

This guide covers migrating from the `oh-my-opencode` plugin to the new `opencode-legion` plugin.

## Overview of Changes

| Aspect | oh-my-opencode | opencode-legion |
|--------|---------------|-----------------|
| Agent prompts | Model-specific prompts baked in | Model-neutral base + overlay at dispatch |
| Hooks | 35+ hooks | 10 Tier-1 hooks |
| Categories | Hardcoded routing | Configurable category routing |
| Model support | Anthropic-centric | Anthropic, OpenAI, Google |

## Step 1: Install opencode-legion

```bash
# Install globally
npm install -g opencode-legion

# Or use the CLI
npx opencode-legion init
```

## Step 2: Update Plugin Config

Edit your `opencode.json` or `opencode.jsonc`:

```jsonc
{
  "plugins": [
    // Remove this:
    // "oh-my-opencode",
    
    // Add this:
    "opencode-legion"
  ]
}
```

## Step 3: Migrate Config File

**oh-my-opencode** config: `~/.config/opencode/oh-my-opencode.json`
**opencode-legion** config: `~/.config/opencode/opencode-legion.json`

Key mappings:

| oh-my-opencode | opencode-legion | Notes |
|----------------|-----------------|-------|
| `agents.orchestrator.model` | `agents.orchestrator.model` | Same path |
| `agents.<name>.model` | `agents.<name>.model` | Same path for all agents |
| N/A | `categories.ultrabrain.model` | New: per-category model routing |
| N/A | `categories.<cat>.temperature` | New: per-category temperature |

### Example config

```json
{
  "agents": {
    "orchestrator": { "model": "anthropic/claude-opus-4-6" },
    "executor": { "model": "anthropic/claude-sonnet-4-20250514" }
  }
}
```

## Step 4: Verify

```bash
# Check plugin loads correctly
opencode-legion status
```

## Behavioral Differences

### Model overlays applied at dispatch time (not init time)

In `oh-my-opencode`, model-specific instructions were embedded in agent prompts at initialization. In `opencode-legion`, agent prompts are model-neutral and a small model-specific overlay is injected at dispatch time via the `experimental.chat.system.transform` hook. This means:

- Switching models mid-session works correctly
- Prompts are cleaner and easier to maintain
- Each provider (Anthropic, OpenAI, Google) gets tailored behavioral hints

### Simplified hook set

`opencode-legion` uses 10 focused hooks instead of 35+:

- `chat.params` — Anthropic effort level
- `chat.message` — Auto slash commands, stop continuation
- `tool.execute.before` — Subagent question blocking, label truncation
- `tool.execute.after` — Preemptive compaction
- `shell.env` — Non-interactive environment
- `event` — Background notifications, session recovery, stop continuation
- `experimental.chat.messages.transform` — Thinking block validation
- `experimental.chat.system.transform` — Model overlay injection

### Category-based routing

Tasks can be routed to different models based on category:

| Category | Default Model | Use Case |
|----------|--------------|----------|
| `visual-engineering` | google/gemini-3-pro | Frontend, UI/UX |
| `ultrabrain` | anthropic/claude-opus-4-6 | Hard logic tasks |
| `deep` | openai/gpt-5.2-codex | Autonomous problem-solving |
| `artistry` | anthropic/claude-opus-4-6 | Creative approaches |
| `quick` | anthropic/claude-sonnet-4-20250514 | Trivial tasks |
| `unspecified-low` | anthropic/claude-sonnet-4-20250514 | Low effort |
| `unspecified-high` | anthropic/claude-opus-4-6 | High effort |
| `writing` | anthropic/claude-sonnet-4-20250514 | Documentation |

## Rollback

To rollback:

1. Remove `"opencode-legion"` from `plugins` in your opencode config
2. Re-add `"oh-my-opencode"` to `plugins`
3. Config files don't conflict (different filenames)
4. No data migration needed — both plugins are stateless
