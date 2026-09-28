#!/usr/bin/env bun
/**
 * Runs the LEGION-354 batch — the rows `pane-guard-bash.test.ts` runs, from
 * `src/legion/pane-guard-model-rows.ts` — against a guard build, and prints what each row does.
 *
 * The test asserts each row against the guard beside it. This measures the same rows against ANY
 * build, so a claim about what a change closed or cost is derived from the rows that ship rather
 * than counted by hand. Point it at another revision by writing that revision's file out first:
 *
 *     jj file show -r main@origin packages/pi-envoy/src/legion/pane-guard.ts \
 *       > packages/pi-envoy/src/legion/pane-guard.base.ts
 *     bun packages/pi-envoy/scripts/measure-pane-guard-model.ts \
 *       packages/pi-envoy/src/legion/pane-guard.base.ts base
 *     bun packages/pi-envoy/scripts/measure-pane-guard-model.ts \
 *       packages/pi-envoy/src/legion/pane-guard.ts head
 *
 * The copy goes inside the package: a guard build imports the package's own modules, so one
 * written to a temporary directory fails to resolve them.
 *
 * A row is a leak when the guard ALLOWED it and real bash destroyed the canary HOME — for a row
 * the shell performs in order. The rows marked `racy` are the ones the shell does not wait for,
 * where the write happens beside the read: one trial decides nothing about those, so they are
 * counted apart and their live rate comes from repeating the run, not from here. Folding them
 * into one leak total would make it swing by however many races a run happened to catch.
 */
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { DESTRUCTIVE, KILL_PID, MODEL_ROWS, PID_ROWS } from "../src/legion/pane-guard-model-rows";

/** The one thing this script needs of a guard build, so any revision's file satisfies it. */
interface GuardModule {
  createPaneGuard: (options: {
    workspace: string | undefined;
    ompPid: number;
    scratch?: string;
  }) => { bash: (command: string, cwd: string, env: NodeJS.ProcessEnv) => string | undefined };
}

const [, , modulePath, label = "guard"] = process.argv;
if (modulePath === undefined) {
  console.error("usage: measure-pane-guard-model.ts <path-to-pane-guard.ts> [label]");
  process.exit(2);
}
// Dynamic by design: the build to measure is this script's argument, which is the whole point.
const { createPaneGuard } = (await import(path.resolve(modulePath))) as GuardModule;

const base = mkdtempSync(path.join(os.tmpdir(), "pane-guard-model-"));
try {
  const results: { name: string; allowed: boolean; destroyed: boolean; racy: boolean }[] = [];
  const run = (
    name: string,
    payload: string,
    script: string | undefined,
    pid: boolean,
    racy = false
  ): void => {
    const dir = path.join(base, String(results.length));
    const ws = path.join(dir, "ws");
    const home = path.join(dir, "home");
    const scratch = path.join(dir, "scratch");
    for (const made of [ws, home, scratch, path.join(ws, "deep")]) {
      mkdirSync(made, { recursive: true });
    }
    // The canary. A row that runs nothing leaves it in place and so proves nothing.
    writeFileSync(path.join(home, "victim"), "do-not-delete\n");
    writeFileSync(path.join(ws, "unread.sh"), DESTRUCTIVE);
    writeFileSync(path.join(ws, "safe.sh"), "PRESENT\n");
    if (script !== undefined) writeFileSync(path.join(ws, "gen.sh"), script);
    const env: NodeJS.ProcessEnv = {
      HOME: home,
      LEGION_WORKSPACE: ws,
      TMPDIR: scratch,
      PATH: process.env.PATH,
    };
    const allowed =
      createPaneGuard({ workspace: ws, ompPid: process.pid, scratch }).bash(payload, ws, env) ===
      undefined;
    // A pid row's `kill` is replaced: the measurement is the verdict, and nothing is signalled.
    spawnSync("bash", ["-c", pid ? payload.replace(KILL_PID, ":") : payload], {
      cwd: ws,
      env,
      timeout: 20_000,
    });
    if (pid) spawnSync("bash", ["-c", "pkill -P $$ sleep 2>/dev/null; true"], { cwd: ws, env });
    results.push({ name, allowed, destroyed: !existsSync(path.join(home, "victim")), racy });
  };

  for (const row of MODEL_ROWS) run(row.name, row.payload, row.script, false, row.live === "racy");
  for (const row of PID_ROWS) run(row.name, row.payload, undefined, true);

  const ordered = results.filter((row) => !row.racy);
  const unwaited = results.filter((row) => row.racy);
  const leaks = ordered.filter((row) => row.allowed && row.destroyed);
  for (const row of results) {
    const live = row.racy ? "(race)" : row.destroyed ? "DESTROYED" : "intact";
    console.log(`${(row.allowed ? "ALLOW" : "REFUSE").padEnd(7)}${live.padEnd(10)}${row.name}`);
  }
  console.log(
    `\n${label}: ${results.length} rows (${MODEL_ROWS.length} + ${PID_ROWS.length}), of which ` +
      `${unwaited.length} the shell does not wait for and are counted apart.\n` +
      `  in order: ${leaks.length} leak, ` +
      `${ordered.filter((row) => row.allowed && !row.destroyed).length} allowed and safe\n` +
      `  unwaited: ${unwaited.filter((row) => row.allowed).length} of ${unwaited.length} allowed ` +
      `(each a live leak when allowed; one trial cannot show it, so this run does not try)`
  );
  for (const row of leaks) console.log(`  leak: ${row.name}`);
  for (const row of unwaited) {
    console.log(`  unwaited, ${row.allowed ? "ALLOWED" : "refused"}: ${row.name}`);
  }
} finally {
  rmSync(base, { recursive: true, force: true });
}
