# Envoy Plugin `/whoami` TUI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Tasks within a phase that are marked "parallel-eligible" MAY be dispatched concurrently via `team_*` tools when team mode is enabled.

**Goal:** Add a `/whoami` slash command and a clickable session ID + port block to the OpenCode TUI sidebar via the existing `@sjawhar/opencode-legion-envoy` plugin, so a human user can grab the active session's ID with one keystroke or mouse click.

**Architecture:** Convert `@sjawhar/opencode-legion-envoy` from a single-entry server plugin into a dual-entry package exposing both `./server` (existing tools) and `./tui` (new). The TUI entry registers a slash command via `api.keymap.registerLayer` and a `sidebar_content` slot. Clipboard writes use OSC 52 directly (no new runtime deps).

**Tech Stack:** TypeScript, Bun runtime, Bun build, Solid.js JSX via `@opentui/solid`, `@opencode-ai/plugin/tui` typings (version `~1.14.46` to match runtime), jj for version control. Tests use `bun:test`.

**Spec:** `docs/superpowers/specs/2026-05-21-envoy-plugin-whoami-tui-design.md`

---

## Conventions & invariants

- **Version control: jj, not git.** Every "commit" step uses `jj describe -m "..."` + `jj new`. Never `git add` / `git commit`.
- **TDD:** every new pure helper starts with a failing test. UI code (Solid+opentui rendering) is verified by E2E gates against a running opencode session, not by unit tests.
- **Phases gated by running-opencode E2E checks.** No phase advances until its E2E gate passes. Build, lint, and typecheck are *necessary but not sufficient* — every phase produces some externally observable behavior that the agent **must** verify in a real opencode session before advancing.
- **`[USER]` markers** identify steps the human must perform (mouse clicks, hover styling, cross-machine paste). Everything else the agent can run unattended via tmux.
- **Working directory:** `/home/ubuntu/legion/default` for all paths and commands unless otherwise stated.
- **Build invocation:** `bun run build` from `packages/envoy-plugin/`.
- **Typecheck noise:** `bunx tsc --noEmit` in this package emits hundreds of pre-existing errors from a `@types/node` vs `@types/bun` conflict in the workspace `node_modules`. CI does not run tsc for this package (see `.github/workflows/pr-and-main.yaml`). Wherever this plan says "verify typecheck", use this filter:
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && (bunx tsc --noEmit 2>&1 || true) | grep -E '^src/' | head -20
  ```
  Expected: no output (no errors in any file under `src/`). If output appears, fix it before continuing.
- **Tool calls within phases.** Each task block lists its files and parallelization eligibility. Within a phase, parallel-eligible tasks can be dispatched concurrently; their sole contract is the type signature of any helper they expose. Sequential tasks must complete in order.
- **Clipboard verification:** OSC 52 sequences emitted by the plugin can be captured into tmux's paste buffer by enabling `set-clipboard on` on the tmux session, then read with `tmux show-buffer`. This is how the agent verifies clipboard correctness end-to-end without a graphical paste.

---

## File layout (final state)

```
packages/envoy-plugin/
├── package.json                      ← MODIFIED: main, exports, build, dep bumps
├── tsconfig.json                     ← MODIFIED: include .tsx + jsx: preserve
├── scripts/sync-host.sh              ← MODIFIED: PLUGIN_REF points to package dir
├── bun.lock                          ← AUTO-UPDATED by bun install
├── src/
│   ├── server.ts                     ← RENAMED from src/index.ts (no code change)
│   ├── clipboard.ts                  ← NEW: OSC 52 helper
│   ├── tui-port.ts                   ← NEW: synchronous URL port parser
│   ├── tui.tsx                       ← NEW: TUI plugin entry (slash + slot)
│   ├── port.ts                       ← UNCHANGED
│   └── __tests__/
│       ├── index.test.ts             ← MODIFIED: import "../index" → "../server"
│       ├── port.test.ts              ← UNCHANGED
│       ├── clipboard.test.ts         ← NEW
│       └── tui-port.test.ts          ← NEW
└── dist/                             ← BUILD OUTPUT
    ├── server.js
    └── tui.js
```

---

## QA harness — one-time setup before Phase 0

Backs up the user's global opencode config and swaps the npm plugin reference for a local `file://` path so the build artifacts under test are what opencode actually loads.

- [ ] **Setup 1**: Snapshot the global config.
  ```bash
  cp ~/.config/opencode/opencode.json ~/.config/opencode/opencode.json.bak-whoami-plan
  diff ~/.config/opencode/opencode.json ~/.config/opencode/opencode.json.bak-whoami-plan
  ```
  Expected: no diff output.

- [ ] **Setup 2**: Replace the npm plugin ref with a `file://` path pointing at the package directory (the directory — opencode resolves `package.json` exports from there).
  ```bash
  jq '(.plugin // []) |= [.[] | if test("@sjawhar/opencode-legion-envoy") then "file:///home/ubuntu/legion/default/packages/envoy-plugin" else . end]' \
    ~/.config/opencode/opencode.json > /tmp/opencode.json.tmp \
    && mv /tmp/opencode.json.tmp ~/.config/opencode/opencode.json
  jq '.plugin' ~/.config/opencode/opencode.json
  ```
  Expected: plugin list contains `"file:///home/ubuntu/legion/default/packages/envoy-plugin"` and no longer contains the npm ref.

- [ ] **Setup 3**: Run a baseline build with current code so the package has a valid `dist/index.js` to load on the first opencode launch.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun install && bun run build
  ls dist/
  ```
  Expected: `dist/index.js` exists, install completes.

- [ ] **Setup 4**: Smoke-check that opencode boots cleanly with the local plugin (sanity baseline before changes).
  ```bash
  tmux kill-session -t whoami-baseline 2>/dev/null || true
  tmux new-session -d -s whoami-baseline -x 220 -y 50 'cd /home/ubuntu/legion/default && opencode'
  sleep 8
  tmux capture-pane -t whoami-baseline -p | grep -iE "fail|error|cannot find" | head -10
  tmux kill-session -t whoami-baseline
  ```
  Expected: no error lines (or only known-harmless ones — investigate if any look related to the plugin).

**Restore step (run after Phase 4 sign-off, not now):** see Phase 4.

---

## Phase 0: Foundation — package restructuring (no new features; no regressions)

**Externally testable behavior:** all 8 existing `envoy_*` tools continue to work in a real opencode session after the package is renamed, dependencies are bumped to the runtime-matching version, and sync-host rollout is corrected.

**Tasks:**
- **Task 0.1** (sequential): Rename `src/index.ts` → `src/server.ts` and update `package.json` `main` + `build` script.
- **Task 0.2** (sequential, after 0.1): Bump `@opencode-ai/plugin` to `~1.14.46`, add `@opentui/{core,keymap,solid}` and `solid-js` as devDependencies, update `tsconfig.json` for `.tsx` + `jsx: preserve`, run `bun install`.
- **Task 0.3** (parallel-eligible with 0.1 or 0.2): Update `scripts/sync-host.sh` `PLUGIN_REF`.

**Why the dep bump is in Phase 0 even though it enables Phase 1 work:** the installed `@opencode-ai/plugin@^1.1.19` resolves to `1.4.3`, which exposes only the deprecated `api.command` and lacks `api.keymap` entirely. The runtime opencode is `1.14.46-sami.20260511`, which uses the modern `api.keymap.registerLayer` API. Bumping to `~1.14.46` aligns the type surface with the runtime. We exercise the dep bump's compatibility with the server plugin in this phase's E2E gate.

---

### Task 0.1: Rename `src/index.ts` → `src/server.ts`

**Files:**
- Rename: `packages/envoy-plugin/src/index.ts` → `packages/envoy-plugin/src/server.ts`
- Modify: `packages/envoy-plugin/src/__tests__/index.test.ts`
- Modify: `packages/envoy-plugin/package.json` (`main` + `build` script)

- [ ] **Step 0.1.1**: Move the file.
  ```bash
  cd /home/ubuntu/legion/default
  mv packages/envoy-plugin/src/index.ts packages/envoy-plugin/src/server.ts
  ```
  jj auto-snapshots; no `jj mv` needed.

- [ ] **Step 0.1.2**: In `packages/envoy-plugin/src/__tests__/index.test.ts`, replace every `from "../index"` and `import("../index")` with `"../server"` (use Grep first to find them, then Edit each occurrence). Expected count: at least 1 import-path string.

- [ ] **Step 0.1.3**: Edit `packages/envoy-plugin/package.json`:
  - Change `"main": "dist/index.js"` → `"main": "dist/server.js"`.
  - Change `"build": "bun build src/index.ts ..."` → `"build": "bun build src/server.ts --outdir dist --target bun --format esm"`.
  - Leave everything else unchanged for now (exports map, deps, etc. — that's Task 0.2 and Phase 1 territory).

- [ ] **Step 0.1.4**: Verify the existing test suite still passes.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun test
  ```
  Expected: all 22 tests pass. A "Cannot find module '../index'" error means a missed import update.

- [ ] **Step 0.1.5**: Verify build emits the new artifact.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && rm -rf dist && bun run build && ls dist/
  ```
  Expected: `dist/server.js` exists; `dist/index.js` does **not** exist.

- [ ] **Step 0.1.6**: Source-file typecheck + lint.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bunx biome check src/
  (bunx tsc --noEmit 2>&1 || true) | grep -E '^src/' | head -20
  ```
  Expected: biome clean, filtered tsc empty.

- [ ] **Step 0.1.7**: Commit.
  ```bash
  cd /home/ubuntu/legion/default
  jj describe -m "refactor(envoy-plugin): rename src/index.ts to src/server.ts

  Prep for adding a TUI entry alongside the server entry. No behavior change.

  Refs: docs/superpowers/specs/2026-05-21-envoy-plugin-whoami-tui-design.md"
  jj new
  ```

---

### Task 0.2: Bump `@opencode-ai/plugin` and add TUI-typing devDependencies

**Files:**
- Modify: `packages/envoy-plugin/package.json` (dependencies + devDependencies)
- Modify: `packages/envoy-plugin/tsconfig.json`
- Auto-modified: `bun.lock`

- [ ] **Step 0.2.1**: Edit `packages/envoy-plugin/package.json` — bump `@opencode-ai/plugin` and add devDependencies. Replace the `dependencies` and `devDependencies` blocks:

  From:
  ```json
    "dependencies": {
      "@opencode-ai/plugin": "^1.1.19"
    },
    "devDependencies": {
      "@biomejs/biome": "^2.3.14",
      "@types/bun": "latest",
      "typescript": "^5.3.0"
    }
  ```

  To:
  ```json
    "dependencies": {
      "@opencode-ai/plugin": "~1.14.46"
    },
    "devDependencies": {
      "@biomejs/biome": "^2.3.14",
      "@opentui/core": "^0.2.6",
      "@opentui/keymap": "^0.2.6",
      "@opentui/solid": "^0.2.6",
      "@types/bun": "latest",
      "solid-js": "^1.9.0",
      "typescript": "^5.3.0"
    }
  ```

  Notes:
  - `~1.14.46` permits patch updates within 1.14.x only (not 1.15.x — there was a minor bump on npm that may have breaking changes).
  - opentui + solid-js are devDeps **only**. Runtime opencode provides them; the build will mark them external.

- [ ] **Step 0.2.2**: Update `packages/envoy-plugin/tsconfig.json` to include `.tsx` files and enable JSX. Replace the file contents:
  ```json
  {
    "compilerOptions": {
      "target": "ES2022",
      "module": "ESNext",
      "moduleResolution": "Bundler",
      "strict": true,
      "noEmit": true,
      "jsx": "preserve",
      "types": ["bun"]
    },
    "include": ["src/**/*.ts", "src/**/*.tsx"]
  }
  ```
  `"jsx": "preserve"` keeps TS hands-off; Bun's build handles JSX via the per-file `/** @jsxImportSource @opentui/solid */` pragma added in Phase 1.

- [ ] **Step 0.2.3**: Run `bun install` from the workspace root.
  ```bash
  cd /home/ubuntu/legion/default && bun install
  ```
  Expected: install completes (exit 0). Peer-dependency warnings are acceptable.

- [ ] **Step 0.2.4**: Verify the new versions are resolvable.
  ```bash
  find /home/ubuntu/legion/default/node_modules -name package.json \( -path '*@opencode-ai/plugin/package.json' -o -path '*@opentui/core/package.json' -o -path '*@opentui/keymap/package.json' -o -path '*@opentui/solid/package.json' \) -exec sh -c 'echo "$(jq -r .name < \"$1\") $(jq -r .version < \"$1\")"' _ {} \; | sort -u
  ```
  Expected: `@opencode-ai/plugin` shows `1.14.46` or later patch; opentui packages show `0.2.x` (≥0.2.6).

- [ ] **Step 0.2.5**: Source-file typecheck + lint + tests.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun test && bunx biome check src/
  (bunx tsc --noEmit 2>&1 || true) | grep -E '^src/' | head -20
  ```
  Expected: all green; filtered tsc empty. If the server-side type surface broke (e.g. `tool.schema` shape changed between 1.4.x and 1.14.x), fix here before continuing.

- [ ] **Step 0.2.6**: Rebuild and confirm `dist/server.js` is regenerated cleanly.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && rm -rf dist && bun run build && ls dist/
  ```
  Expected: `dist/server.js` exists.

- [ ] **Step 0.2.7**: Commit.
  ```bash
  cd /home/ubuntu/legion/default
  jj describe -m "chore(envoy-plugin): bump @opencode-ai/plugin to ~1.14.46

  Aligns plugin types with the runtime opencode (1.14.46), which exposes
  the modern api.keymap API. Adds opentui + solid-js devDeps so the TSX
  entry added in Phase 1 typechecks; they're external in the build and
  resolved by opencode's TUI runtime at load time.

  Updates tsconfig to include .tsx with jsx: preserve so Bun's build
  handles JSX via per-file pragmas.

  Refs: docs/superpowers/specs/2026-05-21-envoy-plugin-whoami-tui-design.md"
  jj new
  ```

---

### Task 0.3: Update `sync-host.sh` PLUGIN_REF

**Files:**
- Modify: `packages/envoy-plugin/scripts/sync-host.sh`

This task is **parallel-eligible** with 0.1 or 0.2 (touches a disjoint file).

The current script writes `PLUGIN_REF="file://{env:HOME}/${PLUGIN_DIR}/dist/index.js"`. After the rename, `dist/index.js` doesn't exist. Pointing the file:// at the package directory (instead of any specific dist file) lets opencode read `package.json` exports and resolve both `./server` and `./tui` entries.

- [ ] **Step 0.3.1**: Edit `packages/envoy-plugin/scripts/sync-host.sh` — change:
  ```bash
  PLUGIN_REF="file://{env:HOME}/${PLUGIN_DIR}/dist/index.js"
  ```
  to:
  ```bash
  PLUGIN_REF="file://{env:HOME}/${PLUGIN_DIR}"
  ```
  (Just remove the `/dist/index.js` suffix.)

- [ ] **Step 0.3.2**: Verify the script parses as valid bash.
  ```bash
  bash -n /home/ubuntu/legion/default/packages/envoy-plugin/scripts/sync-host.sh
  ```
  Expected: no output, exit 0.

- [ ] **Step 0.3.3**: Verify the script's jq rewrite logic produces the expected ref shape.
  ```bash
  echo '{"plugin":["@sjawhar/opencode-legion-envoy@latest","other-thing"]}' \
    | jq --arg ref 'file://{env:HOME}/legion/default/packages/envoy-plugin' \
        '(.plugin // []) |= [.[] | if (test("opencode-legion-envoy") and (test("^file://") | not)) then $ref else . end]'
  ```
  Expected: output contains `"file://{env:HOME}/legion/default/packages/envoy-plugin"` (no `/dist/...` suffix), and `"other-thing"` is preserved.

- [ ] **Step 0.3.4**: Commit.
  ```bash
  cd /home/ubuntu/legion/default
  jj describe -m "fix(envoy-plugin): sync-host points to package dir, not dist file

  After splitting into server+tui entries, plugin discovery needs to
  read package.json exports — requires the file:// ref to point at the
  package directory, not at a single dist file.

  Refs: docs/superpowers/specs/2026-05-21-envoy-plugin-whoami-tui-design.md"
  jj new
  ```

---

### Phase 0 E2E gate (running opencode session)

All three tasks must be committed before this gate. Every existing `envoy_*` tool gets exercised; if any tool behavior changed due to the dep bump, this gate catches it.

- [ ] **Gate 0.1**: Boot opencode TUI in a fresh tmux session with clipboard capture enabled.
  ```bash
  tmux kill-session -t whoami-p0 2>/dev/null || true
  tmux new-session -d -s whoami-p0 -x 220 -y 50 'cd /home/ubuntu/legion/default && opencode'
  tmux set-option -t whoami-p0 set-clipboard on
  sleep 8
  tmux capture-pane -t whoami-p0 -p > /tmp/whoami-p0-boot.txt
  grep -iE "failed to load|cannot find module|plugin .* error" /tmp/whoami-p0-boot.txt | head -5 || echo "OK: clean boot"
  ```
  Expected: prints "OK: clean boot" (no plugin load errors).

- [ ] **Gate 0.2**: Start a session and exercise `envoy_whoami`. The agent will be asked to call the tool and report its output verbatim.
  ```bash
  tmux send-keys -t whoami-p0 "Please call envoy_whoami and paste the exact JSON it returned." Enter
  sleep 25
  tmux capture-pane -t whoami-p0 -p > /tmp/whoami-p0-whoami.txt
  grep -E 'session_id|machine_id|"port"|"dir"' /tmp/whoami-p0-whoami.txt | head -10
  ```
  Expected: response contains keys `session_id`, `machine_id`, `port`, `dir`.

- [ ] **Gate 0.3**: Exercise `envoy_sessions` (lists registered sessions). The current session should appear.
  ```bash
  tmux send-keys -t whoami-p0 "Now call envoy_sessions with no arguments and tell me whether the current session appears in the result." Enter
  sleep 25
  tmux capture-pane -t whoami-p0 -p > /tmp/whoami-p0-sessions.txt
  grep -E 'ses_|session_id|appears|present|found' /tmp/whoami-p0-sessions.txt | tail -10
  ```
  Expected: response confirms the current session is in the list (look for `ses_...` matching the value from Gate 0.2).

- [ ] **Gate 0.4**: Exercise `envoy_list` (lists subscriptions for this session). At baseline it should at least contain the auto-subscribed `notifications.agent.<session_id>` topic.
  ```bash
  tmux send-keys -t whoami-p0 "Call envoy_list and paste the result." Enter
  sleep 25
  tmux capture-pane -t whoami-p0 -p > /tmp/whoami-p0-list.txt
  grep -E 'notifications\.agent\.|topic' /tmp/whoami-p0-list.txt | tail -5
  ```
  Expected: at least the agent-self topic is present.

- [ ] **Gate 0.5**: Exercise `envoy_subscribe` + `envoy_unsubscribe` round-trip on a test topic.
  ```bash
  tmux send-keys -t whoami-p0 "Use envoy_subscribe to subscribe this session to topics ['notifications.test.whoami.qa'], then call envoy_list and confirm the topic appears, then call envoy_unsubscribe with the same topics, then envoy_list again and confirm it's gone. Paste each result." Enter
  sleep 50
  tmux capture-pane -t whoami-p0 -p > /tmp/whoami-p0-subs.txt
  grep -E "notifications\.test\.whoami\.qa" /tmp/whoami-p0-subs.txt | head -10
  ```
  Expected: topic appears after subscribe, absent after unsubscribe.

- [ ] **Gate 0.6**: Exercise `envoy_send` to self (the simplest round-trip — agent sends a message to its own session ID).
  ```bash
  tmux send-keys -t whoami-p0 "Call envoy_whoami to get your session ID, then call envoy_send with target_session set to that ID and a one-line test message of your choosing. Report whether the send succeeded (HTTP-level response, no need to wait for receipt). Don't process any inbound message that may arrive — just report send status." Enter
  sleep 35
  tmux capture-pane -t whoami-p0 -p > /tmp/whoami-p0-send.txt
  grep -iE "succeed|ok|sent|delivered|error|fail" /tmp/whoami-p0-send.txt | tail -10
  ```
  Expected: response indicates send succeeded (or at minimum, no exception).

- [ ] **Gate 0.7**: Exercise `envoy_publish` to a custom topic.
  ```bash
  tmux send-keys -t whoami-p0 "Call envoy_publish with topic 'notifications.test.whoami.qa' and any short message body. Report whether the publish succeeded." Enter
  sleep 25
  tmux capture-pane -t whoami-p0 -p > /tmp/whoami-p0-pub.txt
  grep -iE "publish|succeed|ok|error" /tmp/whoami-p0-pub.txt | tail -5
  ```
  Expected: publish succeeded.

- [ ] **Gate 0.8**: Exercise `envoy_role_set` (idempotent — just claim a test role).
  ```bash
  tmux send-keys -t whoami-p0 "Call envoy_role_set with role 'whoami-plan-qa-test'. Report whether the role was set." Enter
  sleep 25
  tmux capture-pane -t whoami-p0 -p > /tmp/whoami-p0-role.txt
  grep -iE "role|succeed|error" /tmp/whoami-p0-role.txt | tail -5
  ```
  Expected: role set succeeded.

- [ ] **Gate 0.9**: Tear down.
  ```bash
  tmux kill-session -t whoami-p0
  ```

**Pass criteria for Phase 0:** Gates 0.1–0.8 all pass. If any gate fails, the dep bump or rename introduced a regression — investigate and fix before advancing to Phase 1.

---

## Phase 1: `/whoami` slash command copies session ID to clipboard

**Externally testable behavior:** in an opencode session, typing `/whoami` and pressing Enter copies the active session's ID to the user's clipboard via OSC 52 and shows a "Session ID copied" toast. From the home route (no session), it shows a "No active session" toast.

**Tasks:**
- **Task 1.1** (parallel-eligible): `src/clipboard.ts` — OSC 52 helper + unit tests. Exposes `function copyOsc52(text: string): boolean`.
- **Task 1.2** (parallel-eligible, depends on Task 1.1's signature contract): `src/tui.tsx` with the slash command only (no sidebar yet) + update `package.json` `exports` map and `build` script.

When dispatched in parallel via `team_*`, both implementers must agree up front: `copyOsc52(text: string): boolean` is the *only* exported helper from `clipboard.ts` consumed by `tui.tsx`. Task 1.2 may stub the import while 1.1 is in progress, but at integration time both files must coexist in the working copy and the build must succeed.

---

### Task 1.1: OSC 52 clipboard helper

**Files:**
- Create: `packages/envoy-plugin/src/clipboard.ts`
- Create: `packages/envoy-plugin/src/__tests__/clipboard.test.ts`

- [ ] **Step 1.1.1**: Write the failing test. Create `packages/envoy-plugin/src/__tests__/clipboard.test.ts`:
  ```ts
  import { afterEach, beforeEach, describe, expect, it, mock } from "bun:test";
  import { copyOsc52 } from "../clipboard";

  describe("copyOsc52", () => {
    let writes: string[];
    let originalWrite: typeof process.stdout.write;

    beforeEach(() => {
      writes = [];
      originalWrite = process.stdout.write;
      process.stdout.write = mock((chunk: string | Uint8Array) => {
        writes.push(typeof chunk === "string" ? chunk : Buffer.from(chunk).toString("utf-8"));
        return true;
      }) as typeof process.stdout.write;
    });

    afterEach(() => {
      process.stdout.write = originalWrite;
    });

    it("writes the expected OSC 52 escape sequence for ASCII", () => {
      const ok = copyOsc52("ses_abc");
      expect(ok).toBe(true);
      expect(writes).toEqual(["\x1b]52;c;c2VzX2FiYw==\x07"]);
    });

    it("writes the empty payload escape for an empty string", () => {
      const ok = copyOsc52("");
      expect(ok).toBe(true);
      expect(writes).toEqual(["\x1b]52;c;\x07"]);
    });

    it("base64-encodes UTF-8 bytes, not Latin-1 / not JS code units", () => {
      const ok = copyOsc52("café");
      expect(ok).toBe(true);
      // UTF-8 bytes for "café" = 63 61 66 c3 a9 → base64 "Y2Fmw6k="
      expect(writes).toEqual(["\x1b]52;c;Y2Fmw6k=\x07"]);
    });

    it("returns false when process.stdout.write throws", () => {
      process.stdout.write = mock(() => {
        throw new Error("stdout closed");
      }) as typeof process.stdout.write;
      const ok = copyOsc52("anything");
      expect(ok).toBe(false);
    });
  });
  ```

- [ ] **Step 1.1.2**: Confirm the test fails.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun test src/__tests__/clipboard.test.ts
  ```
  Expected: fails with "Cannot find module '../clipboard'".

- [ ] **Step 1.1.3**: Implement `packages/envoy-plugin/src/clipboard.ts`:
  ```ts
  /**
   * Copy text to the user's clipboard via the OSC 52 escape sequence.
   *
   * Works across local terminals and SSH sessions when the terminal
   * emulator supports OSC 52 (iTerm2, Kitty, Alacritty, modern xterm,
   * tmux with `set -g set-clipboard on`, etc).
   *
   * Returns true if the sequence was written, false on error.
   */
  export function copyOsc52(text: string): boolean {
    try {
      const b64 = Buffer.from(text, "utf-8").toString("base64");
      process.stdout.write(`\x1b]52;c;${b64}\x07`);
      return true;
    } catch {
      return false;
    }
  }
  ```

- [ ] **Step 1.1.4**: Confirm the test passes and lint is clean.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun test src/__tests__/clipboard.test.ts && bunx biome check src/clipboard.ts src/__tests__/clipboard.test.ts
  ```
  Expected: 4 tests pass; biome clean.

- [ ] **Step 1.1.5**: Commit.
  ```bash
  cd /home/ubuntu/legion/default
  jj describe -m "feat(envoy-plugin): add OSC 52 clipboard helper

  Pure utility used by the /whoami slash command and sidebar click
  handlers. Works over SSH without external deps.

  Refs: docs/superpowers/specs/2026-05-21-envoy-plugin-whoami-tui-design.md"
  jj new
  ```

---

### Task 1.2: TUI plugin entry with `/whoami` slash command

**Files:**
- Create: `packages/envoy-plugin/src/tui.tsx`
- Modify: `packages/envoy-plugin/package.json` (add `exports` map, extend `build` script)

- [ ] **Step 1.2.1**: Edit `packages/envoy-plugin/package.json` — add `exports` map and extend `build` script. Final state of the relevant parts:
  ```json
  {
    "name": "@sjawhar/opencode-legion-envoy",
    "version": "0.19.0",
    "type": "module",
    "main": "dist/server.js",
    "exports": {
      "./server": {
        "import": "./dist/server.js",
        "types": "./dist/server.d.ts"
      },
      "./tui": {
        "import": "./dist/tui.js",
        "types": "./dist/tui.d.ts"
      }
    },
    "types": "dist/server.d.ts",
    "scripts": {
      "build": "bun build src/server.ts --outdir dist --target bun --format esm && bun build src/tui.tsx --outdir dist --target bun --format esm --external '@opencode-ai/*' --external 'solid-js' --external 'solid-js/*' --external '@opentui/*'",
      "typecheck": "bunx tsc --noEmit",
      "test": "bun test",
      "lint": "bunx biome check src/"
    },
    ...rest unchanged from Task 0.2 state...
  }
  ```
  - `dist/tui.d.ts` won't actually be emitted by bun's bundler — fine; `types` is aspirational and unused by consumers.
  - `--external` flags ensure the TUI bundle doesn't embed Solid/opentui; opencode's TUI runtime provides them.

- [ ] **Step 1.2.2**: Create `packages/envoy-plugin/src/tui.tsx` with the slash command only (no sidebar yet — that's Phase 2):
  ```tsx
  /** @jsxImportSource @opentui/solid */
  import type { TuiPlugin, TuiPluginApi, TuiPluginModule } from "@opencode-ai/plugin/tui";
  import { copyOsc52 } from "./clipboard";

  function currentSessionID(api: TuiPluginApi): string | undefined {
    const route = api.route.current;
    if (route.name !== "session") return undefined;
    return (route.params as { sessionID?: string } | undefined)?.sessionID;
  }

  function copyWithToast(api: TuiPluginApi, text: string, successMessage: string) {
    if (copyOsc52(text)) {
      api.ui.toast({ message: successMessage, variant: "success" });
    } else {
      api.ui.toast({ message: `Failed: ${successMessage}`, variant: "error" });
    }
  }

  const tui: TuiPlugin = async (api) => {
    api.keymap.registerLayer({
      commands: [
        {
          name: "envoy.whoami.copy",
          title: "Copy session ID",
          category: "Envoy",
          namespace: "palette",
          slashName: "whoami",
          run() {
            const sessionID = currentSessionID(api);
            if (!sessionID) {
              api.ui.toast({ message: "No active session", variant: "warning" });
              return;
            }
            copyWithToast(api, sessionID, "Session ID copied");
          },
        },
      ],
    });
  };

  const plugin: TuiPluginModule = {
    id: "envoy-tui",
    tui,
  };

  export default plugin;
  ```

- [ ] **Step 1.2.3**: Build.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && rm -rf dist && bun run build && ls dist/
  ```
  Expected: `dist/server.js` and `dist/tui.js` both present. Build emits no errors.

- [ ] **Step 1.2.4**: Verify the TUI bundle has the slash command symbol and does NOT embed Solid (proves externals are correct).
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin
  grep -c "whoami" dist/tui.js
  grep -c "createComponent\|createMemo" dist/tui.js
  ```
  Expected: first count ≥ 1 (slash command name present); second count = 0 (Solid not bundled).

- [ ] **Step 1.2.5**: Source-file typecheck + lint + tests.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun test && bunx biome check src/
  (bunx tsc --noEmit 2>&1 || true) | grep -E '^src/' | head -20
  ```
  Expected: all green; filtered tsc empty.

- [ ] **Step 1.2.6**: Commit.
  ```bash
  cd /home/ubuntu/legion/default
  jj describe -m "feat(envoy-plugin): add /whoami slash command via TUI plugin entry

  New TUI plugin exposes a /whoami slash command that copies the active
  session's ID to the clipboard via OSC 52 and shows a toast. From the
  home route (no session), shows a 'No active session' warning toast.

  - src/tui.tsx: slash command registered via api.keymap.registerLayer
  - package.json: exports map adds ./server and ./tui, build emits both
    bundles with Solid and opentui marked external

  Sidebar UI comes in the next commits (Phase 2/3 of the plan).

  Refs: docs/superpowers/specs/2026-05-21-envoy-plugin-whoami-tui-design.md"
  jj new
  ```

---

### Phase 1 E2E gate (running opencode session)

Verifies the slash command's full pipeline end-to-end: input → keymap dispatch → session ID lookup → OSC 52 byte sequence → tmux clipboard capture → exact match against `envoy_whoami`'s session_id.

- [ ] **Gate 1.1**: Boot opencode with clipboard capture.
  ```bash
  tmux kill-session -t whoami-p1 2>/dev/null || true
  tmux new-session -d -s whoami-p1 -x 220 -y 50 'cd /home/ubuntu/legion/default && opencode'
  tmux set-option -t whoami-p1 set-clipboard on
  sleep 8
  tmux capture-pane -t whoami-p1 -p | grep -iE "failed|cannot find|plugin .* error" | head -5 || echo "OK: clean boot"
  ```
  Expected: "OK: clean boot".

- [ ] **Gate 1.2**: Start a session, then capture the canonical session ID via the existing tool.
  ```bash
  tmux send-keys -t whoami-p1 "Please call envoy_whoami and paste ONLY the session_id value (no JSON, just the ses_... string)." Enter
  sleep 25
  tmux capture-pane -t whoami-p1 -p > /tmp/whoami-p1-id.txt
  # Extract the most recent ses_... token
  EXPECTED_SID=$(grep -oE 'ses_[a-zA-Z0-9]+' /tmp/whoami-p1-id.txt | tail -1)
  echo "EXPECTED_SID=$EXPECTED_SID"
  test -n "$EXPECTED_SID" || { echo "FAIL: no session ID extracted"; exit 1; }
  ```
  Expected: a `ses_...` ID is extracted.

- [ ] **Gate 1.3**: Clear tmux's paste buffer, then dispatch `/whoami`. Verify the toast and the clipboard contents.
  ```bash
  tmux delete-buffer -t whoami-p1 2>/dev/null || true
  tmux set-buffer -t whoami-p1 "" 2>/dev/null
  tmux send-keys -t whoami-p1 "/whoami" Enter
  sleep 3
  tmux capture-pane -t whoami-p1 -p > /tmp/whoami-p1-after-slash.txt
  # Verify toast — opencode toasts typically include the success text somewhere on screen
  grep -iE "session id copied|copied" /tmp/whoami-p1-after-slash.txt | head -5 || echo "WARN: toast text not visible in capture (may have already faded)"
  # Verify clipboard contents
  ACTUAL_CLIP=$(tmux show-buffer -t whoami-p1 2>/dev/null || echo "")
  echo "ACTUAL_CLIP=$ACTUAL_CLIP"
  test "$ACTUAL_CLIP" = "$EXPECTED_SID" && echo "OK: clipboard matches session ID" || { echo "FAIL: clipboard mismatch (expected=$EXPECTED_SID actual=$ACTUAL_CLIP)"; exit 1; }
  ```
  Expected: "OK: clipboard matches session ID". The toast text check is best-effort (toasts fade quickly); the clipboard equality check is the authoritative one.

- [ ] **Gate 1.4**: Navigate to home route (no session), dispatch `/whoami`, verify the "No active session" path.
  ```bash
  # Navigate to home: opencode binding for "session.new" routes to home (or use whatever the user's binding is for going to home/new session)
  # Easiest portable approach: use the /new slash command which opencode core registers (app.tsx:457)
  tmux send-keys -t whoami-p1 "/new" Enter
  sleep 3
  tmux delete-buffer -t whoami-p1 2>/dev/null || true
  tmux send-keys -t whoami-p1 "/whoami" Enter
  sleep 3
  tmux capture-pane -t whoami-p1 -p > /tmp/whoami-p1-home.txt
  grep -iE "no active session" /tmp/whoami-p1-home.txt | head -3 || echo "WARN: warning toast text not visible (may have faded)"
  # Clipboard should NOT have changed to a session ID (it should be empty or the previous value at best)
  ACTUAL_CLIP_HOME=$(tmux show-buffer -t whoami-p1 2>/dev/null || echo "")
  echo "Clipboard after /whoami from home: '$ACTUAL_CLIP_HOME'"
  # Strictly: clipboard should be empty after our delete-buffer, since the warning path doesn't copy
  test -z "$ACTUAL_CLIP_HOME" && echo "OK: clipboard unchanged from home route" || { echo "FAIL: clipboard wrote something from home route (value: $ACTUAL_CLIP_HOME)"; exit 1; }
  ```
  Expected: "OK: clipboard unchanged from home route" and the warning toast text appears (best-effort).

- [ ] **Gate 1.5** `[USER]`: From the real TUI (not tmux capture), confirm:
  1. Type `/whoami` in a session → "Session ID copied" toast appears.
  2. Paste into another terminal application — pasted text equals the active session ID.
  3. Type `/whoami` from the home/new screen → "No active session" toast appears.

- [ ] **Gate 1.6**: Regression check — every Phase 0 envoy_* tool still works. Quick spot-check:
  ```bash
  tmux send-keys -t whoami-p1 "Please call envoy_whoami one more time. Just to confirm it still works after the TUI plugin was added." Enter
  sleep 25
  tmux capture-pane -t whoami-p1 -p | grep -E 'session_id|machine_id' | tail -5
  ```
  Expected: valid JSON response.

- [ ] **Gate 1.7**: Tear down.
  ```bash
  tmux kill-session -t whoami-p1
  ```

**Pass criteria for Phase 1:** Gates 1.1–1.4 and 1.6 all pass (auto), Gate 1.5 confirmed by user. If clipboard contents in Gate 1.3 don't match the expected session ID exactly, the OSC 52 emission is broken — debug `copyOsc52` or how the TUI process's stdout is being captured.

---

## Phase 2: Clickable session ID row in sidebar

**Externally testable behavior:** in any opencode session, the sidebar always renders the session ID on its own line. Clicking the line copies the ID to the clipboard with a toast. Hovering changes the text color.

**Tasks:**
- **Task 2.1**: Add `ClickableRow` Solid component and register a `sidebar_content` slot rendering the session ID. (Single task — touches only `src/tui.tsx`.)

---

### Task 2.1: ClickableRow + sidebar session ID row

**Files:**
- Modify: `packages/envoy-plugin/src/tui.tsx`

- [ ] **Step 2.1.1**: Replace the entire `packages/envoy-plugin/src/tui.tsx` contents with the version below. It keeps the slash command from Phase 1 and adds the `ClickableRow` component + sidebar slot registration.
  ```tsx
  /** @jsxImportSource @opentui/solid */
  import type {
    TuiPlugin,
    TuiPluginApi,
    TuiPluginModule,
    TuiSlotPlugin,
  } from "@opencode-ai/plugin/tui";
  import { createSignal } from "solid-js";
  import { copyOsc52 } from "./clipboard";

  function currentSessionID(api: TuiPluginApi): string | undefined {
    const route = api.route.current;
    if (route.name !== "session") return undefined;
    return (route.params as { sessionID?: string } | undefined)?.sessionID;
  }

  function copyWithToast(api: TuiPluginApi, text: string, successMessage: string) {
    if (copyOsc52(text)) {
      api.ui.toast({ message: successMessage, variant: "success" });
    } else {
      api.ui.toast({ message: `Failed: ${successMessage}`, variant: "error" });
    }
  }

  function ClickableRow(props: { text: string; onCopy: () => void }) {
    const [hover, setHover] = createSignal(false);
    return (
      <box
        onMouseOver={() => setHover(true)}
        onMouseOut={() => setHover(false)}
        onMouseUp={() => props.onCopy()}
      >
        <text fg={hover() ? undefined : "gray"}>{props.text}</text>
      </box>
    );
  }

  const tui: TuiPlugin = async (api) => {
    // Slash command
    api.keymap.registerLayer({
      commands: [
        {
          name: "envoy.whoami.copy",
          title: "Copy session ID",
          category: "Envoy",
          namespace: "palette",
          slashName: "whoami",
          run() {
            const sessionID = currentSessionID(api);
            if (!sessionID) {
              api.ui.toast({ message: "No active session", variant: "warning" });
              return;
            }
            copyWithToast(api, sessionID, "Session ID copied");
          },
        },
      ],
    });

    // Sidebar: clickable session ID row
    const slot: TuiSlotPlugin = {
      order: 10,
      slots: {
        sidebar_content(_ctx, value) {
          if (!value.session_id) return null;
          return (
            <box flexDirection="column" paddingTop={1}>
              <ClickableRow
                text={value.session_id}
                onCopy={() => copyWithToast(api, value.session_id, "Session ID copied")}
              />
            </box>
          );
        },
      },
    };
    api.slots.register(slot);
  };

  const plugin: TuiPluginModule = {
    id: "envoy-tui",
    tui,
  };

  export default plugin;
  ```
  Notes on `fg={hover() ? undefined : "gray"}`: `undefined` falls back to the parent's default foreground (theme-consistent), and `"gray"` provides a muted default. If the contrast looks wrong in Gate 2.x QA, fix in a follow-up by reading from `ctx.theme` (the slot's first arg).

- [ ] **Step 2.1.2**: Build.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun run build
  ```
  Expected: clean build.

- [ ] **Step 2.1.3**: Typecheck + lint + tests.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun test && bunx biome check src/
  (bunx tsc --noEmit 2>&1 || true) | grep -E '^src/' | head -20
  ```
  Expected: all green; filtered tsc empty.

- [ ] **Step 2.1.4**: Commit.
  ```bash
  cd /home/ubuntu/legion/default
  jj describe -m "feat(envoy-plugin): render clickable session ID in TUI sidebar

  Adds a sidebar_content slot rendering the active session's ID as a
  clickable row. Mouse-up copies the ID via OSC 52 with a success
  toast. Hover changes text color to read as interactive.

  Refs: docs/superpowers/specs/2026-05-21-envoy-plugin-whoami-tui-design.md"
  jj new
  ```

---

### Phase 2 E2E gate (running opencode session)

- [ ] **Gate 2.1**: Boot and start a session.
  ```bash
  tmux kill-session -t whoami-p2 2>/dev/null || true
  tmux new-session -d -s whoami-p2 -x 220 -y 50 'cd /home/ubuntu/legion/default && opencode'
  tmux set-option -t whoami-p2 set-clipboard on
  sleep 8
  tmux send-keys -t whoami-p2 "Please call envoy_whoami and paste ONLY the session_id value." Enter
  sleep 25
  EXPECTED_SID=$(tmux capture-pane -t whoami-p2 -p | grep -oE 'ses_[a-zA-Z0-9]+' | tail -1)
  echo "EXPECTED_SID=$EXPECTED_SID"
  test -n "$EXPECTED_SID" || { echo "FAIL: could not extract session ID"; exit 1; }
  ```

- [ ] **Gate 2.2**: Verify the sidebar contains the **exact** session ID (not just the `ses_` pattern).
  ```bash
  tmux capture-pane -t whoami-p2 -p > /tmp/whoami-p2-sidebar.txt
  grep -F "$EXPECTED_SID" /tmp/whoami-p2-sidebar.txt | head -5
  test "$(grep -cF "$EXPECTED_SID" /tmp/whoami-p2-sidebar.txt)" -ge 1 && echo "OK: sidebar contains exact session ID" || { echo "FAIL: session ID $EXPECTED_SID not in sidebar"; exit 1; }
  ```
  Expected: "OK: sidebar contains exact session ID".

- [ ] **Gate 2.3**: Regression — `/whoami` still copies to clipboard correctly (proves Phase 1 wasn't broken by Phase 2).
  ```bash
  tmux delete-buffer -t whoami-p2 2>/dev/null || true
  tmux send-keys -t whoami-p2 "/whoami" Enter
  sleep 3
  ACTUAL=$(tmux show-buffer -t whoami-p2 2>/dev/null || echo "")
  test "$ACTUAL" = "$EXPECTED_SID" && echo "OK: /whoami still works" || { echo "FAIL: /whoami regression (expected=$EXPECTED_SID actual=$ACTUAL)"; exit 1; }
  ```

- [ ] **Gate 2.4** `[USER]`: From the real TUI:
  1. Confirm the session ID is rendered in the sidebar as a separate row.
  2. Hover the mouse over the row — text color visibly changes (gray → normal).
  3. Click the row — "Session ID copied" toast appears.
  4. Paste into another terminal — pasted text equals the session ID shown in the sidebar.

- [ ] **Gate 2.5**: Tear down.
  ```bash
  tmux kill-session -t whoami-p2
  ```

**Pass criteria for Phase 2:** Gates 2.1–2.3 pass (auto), Gate 2.4 confirmed by user.

---

## Phase 3: Clickable port row in sidebar

**Externally testable behavior:** the sidebar additionally renders the OpenCode serve port (parsed from the TUI client's baseUrl) as a clickable row below the session ID. Clicking copies the port number. When the baseUrl has no parseable port (edge case), the port row is omitted but the session ID row still renders.

**Tasks:**
- **Task 3.1** (parallel-eligible): `src/tui-port.ts` — synchronous URL port parser + unit tests. Exposes `function parsePort(baseUrl: string | undefined): number | null`.
- **Task 3.2** (parallel-eligible, depends on Task 3.1's signature contract): Extend `src/tui.tsx` sidebar slot to render the port row when `parsePort` returns non-null.

Parallel-dispatch contract: `parsePort(baseUrl: string | undefined): number | null`. Task 3.2 may stub the import while 3.1 is being written; at integration time both files must coexist.

---

### Task 3.1: `parsePort` helper

**Files:**
- Create: `packages/envoy-plugin/src/tui-port.ts`
- Create: `packages/envoy-plugin/src/__tests__/tui-port.test.ts`

- [ ] **Step 3.1.1**: Write the failing test. Create `packages/envoy-plugin/src/__tests__/tui-port.test.ts`:
  ```ts
  import { describe, expect, it } from "bun:test";
  import { parsePort } from "../tui-port";

  describe("parsePort", () => {
    it("returns the port from a standard http URL", () => {
      expect(parsePort("http://localhost:4096")).toBe(4096);
    });

    it("returns the port from a 127.0.0.1 URL", () => {
      expect(parsePort("http://127.0.0.1:13381")).toBe(13381);
    });

    it("returns the port from an https URL with explicit port", () => {
      expect(parsePort("https://example.com:8443")).toBe(8443);
    });

    it("returns null when the URL has no explicit port", () => {
      expect(parsePort("http://localhost")).toBe(null);
    });

    it("returns null for the empty string", () => {
      expect(parsePort("")).toBe(null);
    });

    it("returns null for a non-URL string", () => {
      expect(parsePort("not a url")).toBe(null);
    });

    it("returns null for a malformed port", () => {
      expect(parsePort("http://localhost:abc")).toBe(null);
    });

    it("returns null for undefined input", () => {
      expect(parsePort(undefined)).toBe(null);
    });
  });
  ```

- [ ] **Step 3.1.2**: Confirm fail.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun test src/__tests__/tui-port.test.ts
  ```
  Expected: fails with "Cannot find module '../tui-port'".

- [ ] **Step 3.1.3**: Implement `packages/envoy-plugin/src/tui-port.ts`:
  ```ts
  /**
   * Parse the port from an OpenCode serve baseUrl.
   *
   * Synchronous and URL-only: the TUI plugin runs in-process with the
   * OpenCode TUI, which always knows the baseUrl of its serve daemon.
   * This is separate from the server-side `resolvePort` helper, which
   * additionally consults `ss(8)` by PID — irrelevant in the TUI process.
   *
   * Returns null when input is missing, malformed, or has no explicit
   * numeric port.
   */
  export function parsePort(baseUrl: string | undefined): number | null {
    if (!baseUrl) return null;
    let parsed: URL;
    try {
      parsed = new URL(baseUrl);
    } catch {
      return null;
    }
    if (!parsed.port) return null;
    const port = Number.parseInt(parsed.port, 10);
    if (!Number.isFinite(port) || port <= 0) return null;
    return port;
  }
  ```

- [ ] **Step 3.1.4**: Confirm pass + lint.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun test src/__tests__/tui-port.test.ts && bunx biome check src/tui-port.ts src/__tests__/tui-port.test.ts
  ```
  Expected: 8 tests pass; biome clean.

- [ ] **Step 3.1.5**: Commit.
  ```bash
  cd /home/ubuntu/legion/default
  jj describe -m "feat(envoy-plugin): add parsePort helper for TUI

  Synchronous URL-only port parser used by the upcoming sidebar port
  row. Distinct from server-side resolvePort, which also uses ss(8) —
  that fallback is meaningless from the TUI process.

  Refs: docs/superpowers/specs/2026-05-21-envoy-plugin-whoami-tui-design.md"
  jj new
  ```

---

### Task 3.2: Extend sidebar slot with port row

**Files:**
- Modify: `packages/envoy-plugin/src/tui.tsx`

- [ ] **Step 3.2.1**: Edit `packages/envoy-plugin/src/tui.tsx`. Add the `parsePort` import and replace the `slot` definition inside `tui` to also render a port row. The rest of the file is unchanged. The new `slot` definition:
  ```tsx
    // Sidebar: clickable session ID + (optional) port rows
    const slot: TuiSlotPlugin = {
      order: 10,
      slots: {
        sidebar_content(_ctx, value) {
          if (!value.session_id) return null;
          const port = parsePort(api.client.getConfig().baseUrl);
          return (
            <box flexDirection="column" paddingTop={1}>
              <ClickableRow
                text={value.session_id}
                onCopy={() => copyWithToast(api, value.session_id, "Session ID copied")}
              />
              {port !== null ? (
                <ClickableRow
                  text={`port ${port}`}
                  onCopy={() => copyWithToast(api, String(port), "Port copied")}
                />
              ) : null}
            </box>
          );
        },
      },
    };
  ```
  And add this import at the top of the file (after the `solid-js` import):
  ```tsx
  import { parsePort } from "./tui-port";
  ```

- [ ] **Step 3.2.2**: Build.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun run build
  ```
  Expected: clean build.

- [ ] **Step 3.2.3**: Typecheck + lint + tests.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun test && bunx biome check src/
  (bunx tsc --noEmit 2>&1 || true) | grep -E '^src/' | head -20
  ```
  Expected: all green; filtered tsc empty.

- [ ] **Step 3.2.4**: Commit.
  ```bash
  cd /home/ubuntu/legion/default
  jj describe -m "feat(envoy-plugin): render clickable serve port in TUI sidebar

  Extends the sidebar_content slot with a second clickable row showing
  the port the OpenCode serve daemon is listening on (parsed from
  api.client.getConfig().baseUrl). Click copies the port number.

  Port row is omitted when baseUrl has no parseable port (e.g. unix
  socket) — session ID row still renders.

  Refs: docs/superpowers/specs/2026-05-21-envoy-plugin-whoami-tui-design.md"
  jj new
  ```

---

### Phase 3 E2E gate (running opencode session)

- [ ] **Gate 3.1**: Boot.
  ```bash
  tmux kill-session -t whoami-p3 2>/dev/null || true
  tmux new-session -d -s whoami-p3 -x 220 -y 50 'cd /home/ubuntu/legion/default && opencode'
  tmux set-option -t whoami-p3 set-clipboard on
  sleep 8
  ```

- [ ] **Gate 3.2**: Capture both the session ID and the port from `envoy_whoami` — these are the agent's "ground truth" values that the sidebar must match.
  ```bash
  tmux send-keys -t whoami-p3 "Please call envoy_whoami and paste the result as a single line in the form 'session_id=<sid> port=<port>'." Enter
  sleep 25
  RAW=$(tmux capture-pane -t whoami-p3 -p)
  EXPECTED_SID=$(printf '%s' "$RAW" | grep -oE 'ses_[a-zA-Z0-9]+' | tail -1)
  EXPECTED_PORT=$(printf '%s' "$RAW" | grep -oE 'port=[0-9]+' | tail -1 | sed 's/port=//')
  # Fallback if the agent answered with JSON instead of the requested format
  if [ -z "$EXPECTED_PORT" ]; then
    EXPECTED_PORT=$(printf '%s' "$RAW" | grep -oE '"port"[[:space:]]*:[[:space:]]*[0-9]+' | tail -1 | grep -oE '[0-9]+')
  fi
  echo "EXPECTED_SID=$EXPECTED_SID EXPECTED_PORT=$EXPECTED_PORT"
  test -n "$EXPECTED_SID" -a -n "$EXPECTED_PORT" || { echo "FAIL: could not extract both session_id and port"; exit 1; }
  ```
  Expected: both values extracted.

- [ ] **Gate 3.3**: Verify the sidebar contains the **exact** session ID and the **exact** port.
  ```bash
  tmux capture-pane -t whoami-p3 -p > /tmp/whoami-p3-sidebar.txt
  grep -F "$EXPECTED_SID" /tmp/whoami-p3-sidebar.txt > /dev/null && echo "OK: sidebar contains session ID" || { echo "FAIL: session ID not in sidebar"; exit 1; }
  grep -E "port[[:space:]]+$EXPECTED_PORT(\$|[^0-9])" /tmp/whoami-p3-sidebar.txt > /dev/null && echo "OK: sidebar contains port" || { echo "FAIL: port $EXPECTED_PORT not in sidebar"; exit 1; }
  ```
  Expected: both "OK" lines.

- [ ] **Gate 3.4**: Regression — `/whoami` still copies session ID.
  ```bash
  tmux delete-buffer -t whoami-p3 2>/dev/null || true
  tmux send-keys -t whoami-p3 "/whoami" Enter
  sleep 3
  ACTUAL=$(tmux show-buffer -t whoami-p3 2>/dev/null || echo "")
  test "$ACTUAL" = "$EXPECTED_SID" && echo "OK: /whoami still works" || { echo "FAIL: /whoami regression"; exit 1; }
  ```

- [ ] **Gate 3.5** `[USER]`: From the real TUI:
  1. Confirm both rows render in the sidebar: session ID on one line, `port N` on the next.
  2. Hover each row independently — color changes per-row.
  3. Click the session ID row — "Session ID copied" toast; paste in another terminal confirms.
  4. Click the port row — "Port copied" toast; paste in another terminal confirms the port number.

- [ ] **Gate 3.6**: Tear down.
  ```bash
  tmux kill-session -t whoami-p3
  ```

**Pass criteria for Phase 3:** Gates 3.1–3.4 pass (auto), Gate 3.5 confirmed by user.

---

## Phase 4: Final E2E sign-off & cleanup

No new code. One comprehensive final pass + restore the original global config.

- [ ] **Gate 4.1**: Confirm all tests pass.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bun test
  ```
  Expected: green; includes `clipboard.test.ts` (4) + `tui-port.test.ts` (8) plus existing tests.

- [ ] **Gate 4.2**: Confirm lint + filtered typecheck clean.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && bunx biome check src/
  (bunx tsc --noEmit 2>&1 || true) | grep -E '^src/' | head -20
  ```
  Expected: biome clean; tsc filter empty.

- [ ] **Gate 4.3**: Confirm both bundles build cleanly from scratch.
  ```bash
  cd /home/ubuntu/legion/default/packages/envoy-plugin && rm -rf dist && bun run build && ls dist/
  ```
  Expected: `server.js` and `tui.js`.

- [ ] **Gate 4.4**: Confirm commit history is what we expect (8 commits since main + the spec doc).
  ```bash
  cd /home/ubuntu/legion/default && jj log -r 'main..@' --no-graph
  ```
  Expected (in some order):
  - spec doc commit (already on main path? — actually on the current change before this plan started)
  - plan doc commit
  - Task 0.1 rename
  - Task 0.2 dep bump
  - Task 0.3 sync-host
  - Task 1.1 clipboard helper
  - Task 1.2 slash command
  - Task 2.1 sidebar session ID
  - Task 3.1 parsePort helper
  - Task 3.2 sidebar port row

- [ ] **Gate 4.5**: Comprehensive opencode-session smoke test.
  ```bash
  tmux kill-session -t whoami-final 2>/dev/null || true
  tmux new-session -d -s whoami-final -x 220 -y 50 'cd /home/ubuntu/legion/default && opencode'
  tmux set-option -t whoami-final set-clipboard on
  sleep 8

  tmux capture-pane -t whoami-final -p | grep -iE "failed|cannot find|plugin .* error" > /tmp/whoami-final-boot-errs.txt
  test ! -s /tmp/whoami-final-boot-errs.txt && echo "OK: clean boot" || { echo "FAIL: boot errors"; cat /tmp/whoami-final-boot-errs.txt; }

  tmux send-keys -t whoami-final "Please call envoy_whoami and paste the result as 'session_id=<sid> port=<port>'." Enter
  sleep 25
  RAW=$(tmux capture-pane -t whoami-final -p)
  EXPECTED_SID=$(printf '%s' "$RAW" | grep -oE 'ses_[a-zA-Z0-9]+' | tail -1)
  EXPECTED_PORT=$(printf '%s' "$RAW" | grep -oE 'port=[0-9]+' | tail -1 | sed 's/port=//')
  if [ -z "$EXPECTED_PORT" ]; then
    EXPECTED_PORT=$(printf '%s' "$RAW" | grep -oE '"port"[[:space:]]*:[[:space:]]*[0-9]+' | tail -1 | grep -oE '[0-9]+')
  fi

  tmux capture-pane -t whoami-final -p > /tmp/whoami-final-sidebar.txt
  grep -F "$EXPECTED_SID" /tmp/whoami-final-sidebar.txt > /dev/null && echo "OK: session ID in sidebar" || echo "FAIL: session ID missing"
  grep -E "port[[:space:]]+$EXPECTED_PORT(\$|[^0-9])" /tmp/whoami-final-sidebar.txt > /dev/null && echo "OK: port in sidebar" || echo "FAIL: port missing"

  tmux delete-buffer -t whoami-final 2>/dev/null || true
  tmux send-keys -t whoami-final "/whoami" Enter
  sleep 3
  ACTUAL=$(tmux show-buffer -t whoami-final 2>/dev/null || echo "")
  test "$ACTUAL" = "$EXPECTED_SID" && echo "OK: /whoami copies session ID" || echo "FAIL: /whoami clipboard mismatch"

  tmux kill-session -t whoami-final
  ```
  Expected: four "OK:" lines, zero "FAIL:" lines.

- [ ] **Gate 4.6** `[USER]`: Final human verification in real TUI:
  1. Open opencode fresh. Confirm clean boot (no plugin errors visible).
  2. Start any session. Sidebar shows session ID + port rows.
  3. Hover each row — color changes.
  4. Click session ID — toast + clipboard contains the ID — paste in another terminal confirms.
  5. Click port — toast + clipboard contains the port — paste confirms.
  6. `/whoami` from session → "Session ID copied" toast → paste confirms.
  7. `/whoami` from home/new screen → "No active session" warning toast.
  8. Ask an agent to call each of `envoy_whoami`, `envoy_sessions`, `envoy_list`, `envoy_subscribe`, `envoy_unsubscribe`, `envoy_send`, `envoy_publish`, `envoy_role_set`. Each must work without errors (the agent should report success or normal output).
  9. **SSH test (if applicable)**: SSH from your local machine to a remote host where you've synced this plugin via `./packages/envoy-plugin/scripts/sync-host.sh`, repeat steps 4–7 from a session on the remote, confirm the local clipboard receives the values.

- [ ] **Gate 4.7**: Restore the original global config.
  ```bash
  mv ~/.config/opencode/opencode.json.bak-whoami-plan ~/.config/opencode/opencode.json
  jq '.plugin' ~/.config/opencode/opencode.json | head -10
  ```
  Expected: plugin list contains the original `@sjawhar/opencode-legion-envoy@latest` (or whatever was there before) again.

- [ ] **Gate 4.8**: Final implementer report.

  After all the above pass, the implementer reports to the user:
  > **Ready for review.**
  >
  > **All commits on top of main:** [list from `jj log`].
  >
  > **Automated verification passed:**
  > - Unit tests: 12 new + 22 existing = 34 total pass
  > - Build: emits `dist/server.js` and `dist/tui.js` cleanly
  > - tmux smoke (Gate 4.5): clean boot, sidebar renders both rows with the exact session ID and port, `/whoami` clipboard byte-equal to envoy_whoami's session_id
  > - All 8 envoy_* tools exercised in Phase 0 gate; no regressions in Phases 1/2/3
  >
  > **User-side verification (Gate 4.6):** [PASS / FAIL per step]
  > **SSH verification (Gate 4.6 step 9):** [PASS / FAIL / N/A]

  If any step in Gate 4.6 fails, do **not** report ready — fix and re-run from the failed gate.

---

## Self-Review

**Spec coverage (every line of the spec maps to a phase):**
- ✅ Slash command `/whoami` → Phase 1
- ✅ Sidebar always-visible session ID → Phase 2
- ✅ Port display in sidebar → Phase 3
- ✅ Click-to-copy on session ID row → Phase 2 (Gate 2.4 user verification)
- ✅ Click-to-copy on port row → Phase 3 (Gate 3.5 user verification)
- ✅ OSC 52 clipboard → Task 1.1, verified end-to-end via tmux clipboard buffer in every Phase 1/2/3 gate
- ✅ Package structure with `./server` + `./tui` exports → Task 1.2
- ✅ `parsePort` helper → Task 3.1
- ✅ Tests for clipboard + parsePort → Tasks 1.1 + 3.1
- ✅ `sync-host.sh` PLUGIN_REF update → Task 0.3

**Phase E2E coverage:** every phase ends with at least one running-opencode-session test that verifies the phase's externally testable behavior using strict equality (e.g., clipboard contents = `envoy_whoami` session_id, sidebar text contains the exact ID and port). No phase relies on "build succeeded" or "unit tests passed" as its sole verification.

**Parallelization within phases:**
- Phase 0: Task 0.3 parallel-eligible with 0.1 or 0.2 (disjoint file).
- Phase 1: Tasks 1.1 and 1.2 parallel-eligible (disjoint files, contract = `copyOsc52` signature).
- Phase 3: Tasks 3.1 and 3.2 parallel-eligible (disjoint files, contract = `parsePort` signature).
- Phase 2: single task.

**Placeholder scan:** no "TBD", no "TODO", no "fill in". The `[USER]` markers are intentional, clearly defined, and gated explicitly.

**Type / name consistency:**
- `copyOsc52`, `parsePort`, `currentSessionID`, `copyWithToast`, `ClickableRow` consistent across the plan.
- Slash command field is `slashName` (matches modern keymap API, runtime version `1.14.46`).
- Slot is `sidebar_content` (append mode, default — doesn't replace built-in title block).
- Slot's `value` arg typed as `TuiHostSlotMap["sidebar_content"]` = `{ session_id: string }`, referenced as `value.session_id`.

**Tmux mouse-event limitation, honest:** synthetic mouse clicks via tmux `send-keys` were probed and did not trigger opencode's onMouseUp handlers in a quick test. Click verification is therefore handled by `[USER]` steps. The agent verifies the underlying code path indirectly via the slash command (which invokes the same `copyWithToast` helper the click handler invokes), so a passing `/whoami` gate provides high confidence the click handler also works — but the actual UX confirmation (toast + paste + hover styling) is a human-eyes verification.
