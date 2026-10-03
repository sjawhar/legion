#!/usr/bin/env bun
/**
 * Runs `daemon-pane.go`, the rigs' one reading of the Legion daemon, with `go run -overlay` as a
 * main inside a checkout's daemon module, which is how it reaches the daemon's internal packages.
 * Each rig asks it rather than restating the daemon: a change to the daemon reaches the rigs by
 * construction.
 *
 * Command line (setup.sh): `bun daemon-pane.ts <daemon module> <daemon-pane argument>...`, which
 * prints what the program prints.
 */
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";

/** This checkout's daemon module, the one a rig reads unless it names another checkout's. */
export const DAEMON_MODULE = path.resolve(import.meta.dir, "../../../daemon");

const PROGRAM = path.join(import.meta.dir, "daemon-pane.go");

/** Runs daemon-pane.go in `module` with `args`, `input` on its stdin (the caller's own stdin when
 * there is none), and returns its stdout. */
export function runDaemonPane(module: string, args: readonly string[], input?: string): string {
  const scratch = mkdtempSync(path.join(os.tmpdir(), "daemon-pane-"));
  try {
    const overlay = path.join(scratch, "overlay.json");
    const main = path.join(module, "cmd", "daemon-pane", "main.go");
    writeFileSync(overlay, JSON.stringify({ Replace: { [main]: PROGRAM } }));
    const result = Bun.spawnSync(["go", "run", "-overlay", overlay, "./cmd/daemon-pane", ...args], {
      cwd: module,
      stdin: input === undefined ? "inherit" : new TextEncoder().encode(input),
      stdout: "pipe",
      stderr: "pipe",
    });
    if (result.exitCode !== 0) {
      throw new Error(
        `daemon-pane ${args.join(" ")} in ${module} exited ${result.exitCode}: ${result.stderr.toString().trim()}`
      );
    }
    return result.stdout.toString();
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

/** The claim a pane is launched for and the rig's stand-ins for what the daemon knows at boot
 * (daemon-pane.go's paneRequest). */
export interface PaneRequest {
  readonly project: string;
  readonly issue: string;
  readonly role: string;
  readonly stateDir: string;
  readonly workspace: string;
  readonly daemonUrl: string;
  readonly envoyUrl: string;
  readonly natsUrls: readonly string[];
  readonly bootTokenFile: string;
  readonly path: string;
  readonly rolesDir?: string;
}

/** What the daemon tells the pane: every variable, PATH included, and, for a request that names a
 * role bundle, the one `--append-system-prompt` argument as shell text. */
export interface DaemonPane {
  readonly env: Record<string, string>;
  readonly systemPromptArgument?: string;
}

export function daemonPane(module: string, request: PaneRequest): DaemonPane {
  return JSON.parse(runDaemonPane(module, ["pane"], JSON.stringify(request))) as DaemonPane;
}

if (import.meta.main) {
  const [module, ...args] = Bun.argv.slice(2);
  if (module === undefined || args.length === 0) {
    console.error("usage: daemon-pane.ts <daemon module> <daemon-pane argument>...");
    process.exit(2);
  }
  process.stdout.write(runDaemonPane(module, args));
}
