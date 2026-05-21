# Envoy Plugin TUI `/whoami` & Sidebar Session ID Display — Design

**Date:** 2026-05-21
**Status:** Approved, ready for implementation plan
**Owner:** sjawhar

## Goal

Make it trivially easy for a **human user** in the OpenCode TUI to grab the current session's session ID (and port) — without asking an agent to invoke `envoy_whoami` and reading the result out of the conversation.

The session ID is the primary handle used to coordinate agents (via `envoy_send target_session=<id>`). Today, a human in the TUI has no first-class way to obtain their own session's ID:

- The TUI sidebar conditionally renders the session ID, but only when `InstallationChannel !== "latest"` (see `~/opencode/default/packages/opencode/src/cli/cmd/tui/routes/session/sidebar.tsx:59-61`). On a stable build the ID is hidden.
- The `envoy_whoami` tool exposes the ID, but only when an agent invokes it — that requires a prompt round-trip and the result is buried in conversation messages.

We want a slash command and an always-visible sidebar surface, both bundled into the existing `@sjawhar/opencode-legion-envoy` plugin so we ship without waiting on an upstream PR.

## Out of scope

- Upstream opencode contribution that removes the `InstallationChannel !== "latest"` guard. (Revisit later — for now we add our own row in a plugin slot.)
- Exposing additional fields (machine_id, dir, full JSON copy) in the sidebar or slash command. `envoy_whoami` already covers that for agents; humans rarely need them.
- Dir display — opencode already renders dir/workspace info elsewhere in the sidebar.
- Keybind shortcut (e.g. `<leader> y i`). Slash command + click-to-copy already gives single-keystroke ergonomics; no need to claim a keybind slot.
- CLI command (`opencode session current` etc). Different surface, different story; not needed for this pain point.
- Persistent dialog with full whoami JSON.

## Design

### Package structure

The envoy-plugin currently exposes only a `server` plugin via `"main": "dist/index.js"`. We add a TUI plugin alongside it.

**`packages/envoy-plugin/package.json` changes:**
- Update `"main"` from `"dist/index.js"` to `"dist/server.js"` (renamed entry; preserves Node's legacy fallback for tools that don't honor `exports`).
- Add an `exports` map that names both entries explicitly:
  ```jsonc
  "exports": {
    "./server": {
      "import": "./dist/server.js",
      "types": "./dist/server.d.ts"
    },
    "./tui": {
      "import": "./dist/tui.js",
      "types": "./dist/tui.d.ts"
    }
  }
  ```
- Update `build` script to emit both `dist/server.js` and `dist/tui.js`.

**Why this works:** opencode's plugin loader looks for `pkg.json.exports['./server']` and `pkg.json.exports['./tui']` separately (see `~/opencode/default/packages/opencode/src/plugin/shared.ts:103-114`). A single package can expose both.

**Source layout:**
- `src/server.ts` — rename of current `src/index.ts` (the existing Envoy tools). Keep all current behavior; only the filename changes.
- `src/tui.tsx` — new TUI plugin entry: slash command + sidebar slot.
- `src/clipboard.ts` — small OSC 52 helper, shared between slash command and sidebar click handlers.
- `src/port.ts` — unchanged (server-side port resolution; TUI gets port from `api.client.getConfig().baseUrl`, not from this module).

**Build:**
- Replace the existing single-entry build with two `bun build` invocations (e.g. `bun build src/server.ts --outdir dist --target bun --format esm` and the analogous `src/tui.tsx` build).
- The TUI entry is `.tsx` and uses Solid.js JSX. Bun supports JSX out of the box; the TSX file declares `/** @jsxImportSource solid-js */` (or equivalent tsconfig setting) so JSX compiles against Solid.
- **Externals (Solid + opentui + plugin API) need to be marked external so they're resolved against opencode's TUI runtime at load time, not bundled.** Precise list (e.g. `solid-js`, `solid-js/web`, `@opentui/core`, `@opentui/solid`, `@opentui/keymap`, `@opencode-ai/plugin/tui`) is an implementation concern — validate by checking what opencode's TUI plugin loader resolves and confirming the built file doesn't bundle Solid/opentui.

### Slash command: `/whoami`

Registered via the modern keymap API in the TUI plugin entry:

```ts
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
        if (copyOsc52(sessionID)) {
          api.ui.toast({ message: "Session ID copied", variant: "success" });
        } else {
          api.ui.toast({ message: "Failed to copy session ID", variant: "error" });
        }
      },
    },
  ],
});
```

Where `currentSessionID(api)` reads from `api.route.current` when `name === "session"`:

```ts
function currentSessionID(api: TuiPluginApi): string | undefined {
  const route = api.route.current;
  if (route.name !== "session") return undefined;
  return route.params?.sessionID as string | undefined;
}
```

Behavior:
- In a session route → copy ID to clipboard via OSC 52, success toast.
- Outside a session (home route) → "No active session" warning toast. Command remains discoverable in the palette but the action is a no-op.

### Sidebar slot

Render a clickable block in the `sidebar_content` slot, which appears below the built-in title group (which already includes workspace and share URL).

```tsx
// `api` is captured from the outer TuiPlugin closure.
// `copy(text, toast)` is a small helper colocated in tui.tsx:
//
//   function copy(text: string, toastMessage: string) {
//     if (copyOsc52(text)) {
//       api.ui.toast({ message: toastMessage, variant: "success" });
//     } else {
//       api.ui.toast({ message: `Failed: ${toastMessage}`, variant: "error" });
//     }
//   }

const block: TuiSlotPlugin = {
  order: 10,
  slots: {
    sidebar_content(_ctx, value) {
      const port = parsePort(api.client.getConfig().baseUrl);
      return (
        <box flexDirection="column">
          <ClickableRow
            text={value.session_id}
            onCopy={() => copy(value.session_id, "Session ID copied")}
          />
          {port ? (
            <ClickableRow
              text={`port ${port}`}
              onCopy={() => copy(String(port), "Port copied")}
            />
          ) : null}
        </box>
      );
    },
  },
};
```

**`ClickableRow` component:**
- Solid.js component wrapping a `<text>` in a `<box>`.
- Holds a `hover` signal toggled by `onMouseOver` / `onMouseOut`.
- `onMouseUp` invokes the `onCopy` callback.
- Visual: `theme.textMuted` by default, `theme.text` on hover. No border or background change (keeps the sidebar uncluttered).

**Fallback behavior:**
- If `api.client.getConfig().baseUrl` is missing, malformed, or has no parseable port (e.g. unix socket), the port row is omitted. Session ID row still renders.
- If the slot is somehow invoked outside a session context (shouldn't happen — opencode only mounts sidebar in session route), the slot returns `null`.

**`parsePort` semantics:**
- Input: string baseUrl like `http://localhost:4096`, `http://127.0.0.1:13381`, or potentially something exotic.
- Try `new URL(baseUrl).port` first. If non-empty and parses as positive integer → return it.
- Else return `null`. No fallback to `ss(8)` here — that's a server-side concern; the TUI doesn't have the right PID anyway.

### Clipboard via OSC 52

`src/clipboard.ts`:

```ts
/**
 * Copy text to the user's clipboard via OSC 52 escape sequence.
 *
 * Works across local terminals and SSH sessions when the terminal emulator
 * supports OSC 52 (iTerm2, Kitty, Alacritty, modern xterm, tmux with
 * `set -g set-clipboard on`, etc).
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

**Why OSC 52 (not `clipboardy`):**
- Sami's primary use is over SSH. OSC 52 is the canonical SSH-clipboard mechanism.
- No runtime dependency to add.
- The opencode TUI also writes OSC 52 (see `~/opencode/default/packages/opencode/src/cli/cmd/tui/util/clipboard.ts:21-22`), so behavior is consistent.
- The opencode TUI's `Clipboard.copy` also tries `clipboardy` and shell utilities as a more aggressive fallback. We deliberately stay minimal — OSC 52 alone is enough for the SSH/terminal-first audience; if a user's terminal doesn't support it, they can fall back to mouse-select on the sidebar row (which is plain text and works regardless).

### Visual reference (sidebar after change)

Before (on `latest` channel):
```
┌──────────────────────┐
│ Session Title        │
│ workspace-label      │
│ https://share.url    │
└──────────────────────┘
```

After:
```
┌──────────────────────────────────┐
│ Session Title                    │
│ workspace-label                  │
│ https://share.url                │
│                                  │   ← gap from sidebar_content slot
│ ses_2e6ca3034ffejVikSZ8mDwk0mR   │   ← muted; click → copy + toast
│ port 13381                        │   ← muted; click → copy + toast
└──────────────────────────────────┘
```

## Trade-offs

**Why a plugin and not an upstream PR to opencode:**
- Faster to ship; user has the pain today.
- We already maintain the envoy-plugin, which conceptually owns session-identity surfaces.
- Strictly additive — anyone running envoy-plugin gets it; nobody else is affected.
- An upstream PR can come later if other opencode users care.

**Why OSC 52 only (no `clipboardy` fallback):**
- Reduces deps and surface area.
- Most likely failure mode is "terminal doesn't support OSC 52", in which case the user can still mouse-select the visible session ID text in the sidebar.
- If this turns out to be insufficient in practice, we can add `clipboardy` later without touching the design.

**Why `sidebar_content` and not `sidebar_title` slot:**
- `sidebar_title` is `single_winner` mode — using it would replace the built-in title/workspace/share rendering, forcing us to re-implement that.
- `sidebar_content` is append-only, so we add a row without fighting the host.

**Why click-to-copy on each row separately (not one "copy whoami JSON" action):**
- Most flows want exactly one field at a time (e.g. "paste session ID into the other agent's prompt").
- Per-field clicks match the per-row visual layout.
- The `envoy_whoami` tool still exists for agent-driven full payload access.

## Test coverage

**Unit tests (`bun test`):**

1. `src/__tests__/clipboard.test.ts`
   - `copyOsc52("ses_abc")` writes `\x1b]52;c;c2VzX2FiYw==\x07` to stdout. Stub `process.stdout.write` to capture.
   - `copyOsc52("")` writes the empty payload escape (`\x1b]52;c;\x07`) and returns true.
   - UTF-8: `copyOsc52("café")` base64-encodes the UTF-8 bytes (not Latin-1 / not the JS string code units).
   - Returns false when `process.stdout.write` throws.

2. `src/__tests__/tui-port.test.ts` (new file — the server-side `port.test.ts` already covers the existing `resolvePort` helper; this is a distinct, sync URL-only parser for the TUI surface)
   - `parsePort("http://localhost:4096")` → `4096`
   - `parsePort("http://127.0.0.1:13381")` → `13381`
   - `parsePort("http://localhost")` → `null` (no explicit port → opencode TUI URL would always have one, but be defensive)
   - `parsePort("")` → `null`
   - `parsePort("not a url")` → `null`
   - `parsePort("http://localhost:abc")` → `null`

3. `src/__tests__/tui.test.tsx` (best-effort; depends on what's mockable from opencode plugin TUI surface)
   - Slot returns `null` when `value.session_id` is missing.
   - Slot renders one row (session ID only) when port can't be parsed.
   - Slot renders two rows when port parses.
   - Mouse-up handler invokes the clipboard helper with the expected text.
   - If JSX runtime is hard to stand up in tests (Solid.js + opentui), fall back to factoring out a pure function `whoamiRows(sessionID, port)` returning `Array<{text, copyValue, toastMessage}>` and test that.

**Manual verification checklist (do not skip):**
- Launch TUI in a stable-channel install. Session ID row appears in sidebar.
- Click session ID row → terminal clipboard contains the ID. Paste into another terminal/agent prompt and confirm it matches.
- Click port row → terminal clipboard contains the port number.
- Type `/whoami`, press Enter → toast "Session ID copied", clipboard contains the ID.
- Type `/whoami` from home route (no session) → "No active session" toast.
- SSH session test: same steps over `ssh sami@host` with iTerm2 + OSC 52 enabled. Clipboard on local mac receives the value.

## Future possibilities (explicitly deferred)

- Upstream opencode contribution removing the `InstallationChannel !== "latest"` guard on the built-in sidebar session ID display.
- Additional slash commands (`/sid`, `/host`, `/cwd`) — only if usage shows demand.
- Keybind shortcut (e.g. `<leader> y i`) — only if slash + click-to-copy proves insufficient.
- Customizable display: hide port row, custom format string, etc.

## Acceptance

This design is approved when:
- Slash command `/whoami` copies session ID via OSC 52 with appropriate toast feedback.
- Sidebar shows session ID (always) and port (when parseable) as clickable rows.
- Both slash command and click handlers work over SSH (assuming OSC-52-capable terminal).
- Package exposes both `./server` and `./tui` entrypoints; existing server tools continue to work unchanged.
- Unit tests cover clipboard encoding and port parsing.
