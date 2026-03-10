# opencode-legion Plugin Trial — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Set up an isolated opencode config that uses the opencode-legion plugin instead of oh-my-opencode, start the daemon with a Sisyphus external controller, and validate workers load the new plugin.

**Architecture:** Isolated config directory via `XDG_CONFIG_HOME` redirect. External controller communicates with daemon HTTP API. Workers on shared serve inherit the isolated config. See design doc: `docs/plans/2026-03-10-opencode-legion-plugin-trial-design.md`

**Tech Stack:** Bun (build), opencode serve, legion daemon, jj

---

### Task 1: Build the opencode-legion plugin

The plugin at `packages/opencode-plugin/` has no `dist/` directory. It needs to be built since `package.json` declares `"main": "dist/index.js"`.

**Files:**
- Build output: `packages/opencode-plugin/dist/`

**Step 1: Run the build**

```bash
cd ~/legion/default/packages/opencode-plugin && bun run build
```

Expected: `dist/index.js` and `dist/cli/index.js` created.

**Step 2: Verify build output**

```bash
ls -la ~/legion/default/packages/opencode-plugin/dist/
```

Expected: `index.js` exists.

---

### Task 2: Create isolated config directory structure

**Files:**
- Create: `~/.legion/trial-config/opencode/` (directory tree)

**Step 1: Create directories**

```bash
mkdir -p ~/.legion/trial-config/opencode/skills
mkdir -p ~/.legion/trial-config/opencode/plugins
```

**Step 2: Create symlinks — config infrastructure**

```bash
cd ~/.legion/trial-config/opencode
ln -s ~/.dotfiles/.claude/CLAUDE.md AGENTS.md
ln -s ~/.config/opencode/mcp-oauth.json mcp-oauth.json
ln -s ~/.config/opencode/package.json package.json
ln -s ~/.config/opencode/node_modules node_modules
ln -s ~/.config/opencode/bun.lock bun.lock
```

**Step 3: Create symlinks — essential skills**

```bash
cd ~/.legion/trial-config/opencode/skills
ln -s ~/.dotfiles/vendor/superpowers/skills superpowers
ln -s ~/.dotfiles/vendor/legion/.opencode/skills legion
ln -s ~/.dotfiles/plugins/sjawhar/skills/using-jj using-jj
ln -s ~/legion/default/.opencode/skills/github github
ln -s ~/legion/default/.opencode/skills/linear linear
```

**Step 4: Create symlink — superpowers plugin**

```bash
cd ~/.legion/trial-config/opencode/plugins
ln -s ~/.dotfiles/vendor/superpowers/.opencode/plugins/superpowers.js superpowers.js
```

**Step 5: Verify symlinks resolve**

```bash
ls -la ~/.legion/trial-config/opencode/
ls -la ~/.legion/trial-config/opencode/skills/
ls -la ~/.legion/trial-config/opencode/plugins/
```

Expected: All symlinks resolve (no broken links).

---

### Task 3: Create modified opencode.json

**Files:**
- Create: `~/.legion/trial-config/opencode/opencode.json`

**Step 1: Write the config**

Copy current `~/.config/opencode/opencode.json` verbatim, replacing the oh-my-opencode plugin reference:

```diff
- "file://{env:HOME}/oh-my-opencode/original",
+ "file:///home/ubuntu/legion/default/packages/opencode-plugin",
```

All other sections (keybinds, provider, instructions, mcp, autoupdate) remain identical.

**Step 2: Verify JSON is valid**

```bash
bun -e "console.log(JSON.parse(require('fs').readFileSync('$HOME/.legion/trial-config/opencode/opencode.json','utf8')).plugin)"
```

Expected: Plugin array contains `file:///home/ubuntu/legion/default/packages/opencode-plugin` and NOT `oh-my-opencode`.

---

### Task 4: Create opencode-legion.json with ported model assignments

**Files:**
- Create: `~/.legion/trial-config/opencode/opencode-legion.json`

**Step 1: Write the plugin config**

Port model assignments from current `~/.config/opencode/oh-my-opencode.json`:

```json
{
  "agents": {
    "orchestrator": { "model": "anthropic/claude-opus-4-6" },
    "executor": { "model": "anthropic/claude-sonnet-4-6" },
    "explorer": { "model": "anthropic/claude-haiku-4-5" },
    "librarian": { "model": "anthropic/claude-sonnet-4-6" },
    "oracle": { "model": "openai/gpt-5.4" },
    "metis": { "model": "openai/gpt-5.4" },
    "momus": { "model": "openai/gpt-5.4" },
    "multimodal": { "model": "openai/gpt-5.4" }
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

---

### Task 5: Validate the isolated config loads correctly

**Step 1: Check opencode sees the right config path**

```bash
XDG_CONFIG_HOME=$HOME/.legion/trial-config opencode debug paths
```

Expected: `config` line shows `~/.legion/trial-config/opencode`.

**Step 2: Check opencode resolves the plugin**

```bash
XDG_CONFIG_HOME=$HOME/.legion/trial-config opencode debug config 2>&1 | head -40
```

Expected: Config loads without errors. Plugin list includes `opencode-legion`. No reference to `oh-my-opencode`.

**Step 3: If validation fails, diagnose and fix before proceeding**

Common issues:
- Plugin build failed → rebuild
- Broken symlinks → check target paths
- JSON parse error → fix syntax in opencode.json
- Missing dependency → check node_modules symlink

---

### Task 6: Start the daemon with external controller

**Step 1: Determine team ID**

The user needs to provide the GitHub project team key. Ask if not known.

**Step 2: Start daemon in background**

```bash
XDG_CONFIG_HOME=$HOME/.legion/trial-config \
LEGION_CONTROLLER_SESSION_ID=ses_external_trial \
nohup legion start <team> -b github -w ~/legion/default > ~/.legion/trial-config/daemon.log 2>&1 &
```

**Step 3: Verify daemon is running**

```bash
curl -s http://127.0.0.1:13370/health | jq .
```

Expected: `{"status":"ok", "workerCount":0, ...}`

**Step 4: Check daemon logs for plugin loading**

```bash
tail -20 ~/.legion/trial-config/daemon.log
```

Expected: Shared serve started, no plugin errors.

---

### Task 7: Start the controller loop

**Step 1: Set controller env vars in the Sisyphus session**

The controller skill reads `LEGION_DAEMON_PORT`, `LEGION_TEAM_ID`, `LEGION_ISSUE_BACKEND`, etc. These need to be available. Export them in the session's shell:

```bash
export LEGION_DAEMON_PORT=13370
export LEGION_TEAM_ID=<team>
export LEGION_ISSUE_BACKEND=github
export LEGION_DIR=~/legion/default
```

**Step 2: Invoke the controller skill**

In the Sisyphus session, invoke `/legion-controller` to start the hybrid loop.

**Step 3: Verify controller can reach daemon**

The controller's first action is `POST /state/collect`. Watch for successful response.

---

### Task 8: Smoke test — dispatch a worker

**Step 1: Pick a test issue**

Choose a small, well-defined issue from the GitHub project.

**Step 2: Let the controller dispatch (or manually dispatch)**

Either let the controller loop pick it up, or manually:

```bash
curl -s -X POST http://127.0.0.1:13370/workers \
  -H "Content-Type: application/json" \
  -d '{"issueId":"<issue-id>","mode":"implement","workspace":"/home/ubuntu/legion/default"}' | jq .
```

**Step 3: Verify worker is running**

```bash
curl -s http://127.0.0.1:13370/workers | jq .
```

Expected: One worker with status `running` or `starting`.

**Step 4: Check worker uses opencode-legion plugin**

The worker should be running on the shared serve which loaded the isolated config. Verify by checking serve logs or worker behavior (delegation tools, model routing should match opencode-legion config).

---

### Teardown (when done with trial)

```bash
# Stop daemon
curl -s -X POST http://127.0.0.1:13370/shutdown

# Or if daemon is in foreground:
# Ctrl+C or: legion stop <team>

# Delete isolated config
rm -rf ~/.legion/trial-config
```
