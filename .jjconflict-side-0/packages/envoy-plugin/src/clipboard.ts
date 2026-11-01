import { spawnSync } from "node:child_process";
import { appendFileSync } from "node:fs";
import { platform } from "node:os";

/**
 * Build marker. Bump on every change so the debug log proves whether the
 * running OpenCode process loaded the new module — the TUI plugin is loaded via
 * `await import()` (cached by URL), so a behavior fix here only takes effect
 * after a full OpenCode process restart, not just a new TUI session.
 */
export const CLIPBOARD_BUILD_ID = "renderer-osc52-2026-06-06-2";

/**
 * Clipboard copy for the OpenCode TUI plugin.
 *
 * Copies MUST go through the renderer's own OSC 52 writer
 * (api.renderer.copyToClipboardOSC52). opentui's renderer owns the terminal and
 * serializes all output via its native writer; it also applies tmux/screen DCS
 * passthrough wrapping and gates on terminal OSC 52 support (see opentui zig
 * terminal.zig writeClipboard). Writing a raw OSC 52 to process.stdout bypasses
 * that serialization and races with frame output, so copies landed only
 * intermittently ("works once right after a native copy, then stops") — that was
 * the flakiness, now removed.
 *
 * A native OS clipboard command is kept as a fallback for terminals without
 * OSC 52 support; it is a subprocess and never writes to the terminal stream.
 */

type Runner = (cmd: string, args: string[], text: string) => boolean;

export interface ClipboardRenderer {
  copyToClipboardOSC52(text: string): boolean;
}

function debugClipboard(event: Record<string, unknown>): void {
  const path = process.env.OPENCODE_TUI_CLIPBOARD_DEBUG;
  if (!path) return;
  try {
    appendFileSync(
      path,
      `${JSON.stringify({
        ts: new Date().toISOString(),
        buildId: CLIPBOARD_BUILD_ID,
        pid: process.pid,
        entry: import.meta.url,
        isTTY: process.stdout.isTTY ?? null,
        term: process.env.TERM ?? null,
        tmux: Boolean(process.env.TMUX),
        sty: Boolean(process.env.STY),
        display: Boolean(process.env.DISPLAY),
        wayland: Boolean(process.env.WAYLAND_DISPLAY),
        ...event,
      })}\n`
    );
  } catch {
    // diagnostics must never break copy
  }
}

function copyViaRenderer(renderer: ClipboardRenderer | undefined, text: string): boolean {
  if (!renderer) return false;
  try {
    const ok = renderer.copyToClipboardOSC52(text);
    debugClipboard({ phase: "renderer-osc52", returned: ok });
    return ok;
  } catch (error) {
    debugClipboard({ phase: "renderer-osc52", error: String(error) });
    return false;
  }
}

function defaultWhich(cmd: string): boolean {
  try {
    return spawnSync("which", [cmd], { stdio: "ignore" }).status === 0;
  } catch {
    return false;
  }
}

function defaultRun(cmd: string, args: string[], text: string): boolean {
  try {
    const result = spawnSync(cmd, args, {
      input: text,
      stdio: ["pipe", "ignore", "ignore"],
      timeout: 1000,
    });
    return result.status === 0;
  } catch {
    return false;
  }
}

export interface NativeCopyDeps {
  which?: (cmd: string) => boolean;
  run?: Runner;
  os?: NodeJS.Platform;
  env?: NodeJS.ProcessEnv;
}

export function copyNative(text: string, deps: NativeCopyDeps = {}): boolean {
  const which = deps.which ?? defaultWhich;
  const run = deps.run ?? defaultRun;
  const os = deps.os ?? platform();
  const env = deps.env ?? process.env;

  if (os === "darwin" && which("osascript")) {
    const escaped = text.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
    return run("osascript", ["-e", `set the clipboard to "${escaped}"`], "");
  }

  if (os === "linux") {
    if (env.WAYLAND_DISPLAY && which("wl-copy")) return run("wl-copy", [], text);
    if (which("xclip")) return run("xclip", ["-selection", "clipboard"], text);
    if (which("xsel")) return run("xsel", ["--clipboard", "--input"], text);
  }

  if (os === "win32") {
    return run(
      "powershell.exe",
      [
        "-NonInteractive",
        "-NoProfile",
        "-Command",
        "[Console]::InputEncoding = [System.Text.Encoding]::UTF8; Set-Clipboard -Value ([Console]::In.ReadToEnd())",
      ],
      text
    );
  }

  return false;
}

export function copyToClipboard(text: string, renderer?: ClipboardRenderer): boolean {
  debugClipboard({ phase: "copy-start", textLength: text.length });
  const viaRenderer = copyViaRenderer(renderer, text);
  const native = viaRenderer ? false : copyNative(text);
  const ok = viaRenderer || native;
  debugClipboard({ phase: "copy-result", viaRenderer, native, ok });
  return ok;
}
