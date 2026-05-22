import { appendFile, mkdir } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

/**
 * File-based logger for the envoy-plugin.
 *
 * Plugins load in-process with OpenCode, which means any byte a plugin writes
 * to process.stderr / process.stdout goes straight to the terminal that the
 * TUI is rendering into — corrupting the screen. console.warn / console.error
 * therefore become a UX bug, not a diagnostic tool.
 *
 * This logger writes to a file instead. Default location colocates with
 * OpenCode's own logs (~/.local/share/opencode/log/) so plugin diagnostics
 * are discoverable next to the host's logs without leaking into the render.
 *
 * The API is intentionally narrow (warn / error / info) so callers can't
 * accidentally substitute it for console (e.g. console.log is also unsafe in
 * a TUI host).
 */

export interface LoggerOptions {
  /** Directory the log file is written into. Created on demand. */
  logDir?: string;
  /** File name within logDir. Defaults to "envoy-plugin.log". */
  fileName?: string;
}

export interface Logger {
  warn(message: string): void;
  error(message: string): void;
  info(message: string): void;
  /** Resolve after every queued write has flushed to disk. Useful in tests. */
  flush(): Promise<void>;
}

function defaultLogDir(): string {
  // Match OpenCode's log directory so plugin diagnostics live next to the
  // host's structured logs. Falls back to tmpdir if HOME is somehow unset.
  const home = os.homedir();
  if (!home) return path.join(os.tmpdir(), "opencode-log");
  return path.join(home, ".local", "share", "opencode", "log");
}

function format(level: "WARN" | "ERROR" | "INFO", message: string): string {
  return `${new Date().toISOString()} ${level} ${message}\n`;
}

export function createLogger(options: LoggerOptions = {}): Logger {
  const dir = options.logDir ?? defaultLogDir();
  const file = path.join(dir, options.fileName ?? "envoy-plugin.log");

  // Serialize writes through a promise chain so concurrent log calls don't
  // interleave bytes mid-line and so flush() can await everything.
  let chain: Promise<void> = Promise.resolve();

  const append = (line: string) => {
    chain = chain
      .then(() => mkdir(dir, { recursive: true }))
      .then(() => appendFile(file, line))
      // Diagnostics must never crash the host. Swallow the error — there's
      // nowhere safe to surface it (console is the very thing we're avoiding).
      .catch(() => {});
  };

  return {
    warn(message) {
      append(format("WARN", message));
    },
    error(message) {
      append(format("ERROR", message));
    },
    info(message) {
      append(format("INFO", message));
    },
    async flush() {
      await chain;
    },
  };
}

/**
 * Default singleton used by the plugin internals. Tests that need an
 * isolated log path call `createLogger({ logDir })` directly.
 */
export const logger: Logger = createLogger();
