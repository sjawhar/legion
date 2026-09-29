#!/usr/bin/env bun
/**
 * Runs the LEGION-357 batch — the rows `pane-guard-bash.test.ts` runs, from
 * `src/legion/pane-guard-write-rows.ts` — against a guard build, and prints what each row does.
 *
 * The test asserts each row against the guard beside it. This measures the same rows against ANY
 * build, so a claim about what a change closed or cost is derived from the rows that ship rather
 * than counted by hand. Point it at another revision by writing that revision's file out first:
 *
 *     jj file show -r main@origin packages/pi-envoy/src/legion/pane-guard.ts \
 *       > packages/pi-envoy/src/legion/pane-guard.base.ts
 *     bun packages/pi-envoy/scripts/measure-pane-guard-writes.ts \
 *       packages/pi-envoy/src/legion/pane-guard.base.ts base
 *     bun packages/pi-envoy/scripts/measure-pane-guard-writes.ts \
 *       packages/pi-envoy/src/legion/pane-guard.ts head
 *
 * The copy goes inside the package: a guard build imports the package's own modules, so one
 * written to a temporary directory fails to resolve them.
 *
 * A row is a LEAK when the guard allowed it and real bash changed something under the canary
 * HOME, and a COST when the guard refused a row the pane is meant to be able to run. Both counts
 * come from the same run, because a change that closes leaks by refusing everything is not a fix.
 */
import { mkdirSync, mkdtempSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  buildWriteFixture,
  fixtureProperties,
  type GuardFactory,
  measureWriteRow,
  WRITE_ROWS,
} from "../src/legion/pane-guard-write-rows";

const [, , modulePath, label = "guard"] = process.argv;
if (modulePath === undefined) {
  console.error("usage: measure-pane-guard-writes.ts <path-to-pane-guard.ts> [label]");
  process.exit(2);
}
// Dynamic by design: the build to measure is this script's argument, which is the whole point.
const { createPaneGuard } = (await import(path.resolve(modulePath))) as {
  createPaneGuard: GuardFactory;
};

const base = mkdtempSync(path.join(os.tmpdir(), "pane-guard-writes-"));
try {
  const properties = fixtureProperties(base);
  console.log(
    Object.entries(properties)
      .map(([name, value]) => `fixture.${name}=${value}`)
      .join(" ")
  );

  const results = WRITE_ROWS.map((row) => measureWriteRow(base, row, createPaneGuard));
  for (const { row, refusal, error, live, exit } of results) {
    console.log(
      `${(error !== undefined ? "ERROR" : refusal === undefined ? "ALLOW" : "REFUSE").padEnd(7)}${(live ? "CHANGED" : "intact").padEnd(8)}` +
        `exit=${String(exit).padEnd(4)}${row.family.padEnd(9)}${row.name}`
    );
  }

  const leaks = results.filter(
    (result) => result.live && result.refusal === undefined && result.error === undefined
  );
  const costs = results.filter(
    (result) =>
      result.row.role === "must-allow" &&
      (result.refusal !== undefined || result.error !== undefined)
  );
  console.log(
    `\n${label}: rows=${results.length} changed=${results.filter((r) => r.live).length} ` +
      `LEAKS=${leaks.length} COSTS=${costs.length} ` +
      `refused_changed=${results.filter((r) => r.live && r.refusal !== undefined).length} ` +
      `allowed_intact=${results.filter((r) => !r.live && r.refusal === undefined && r.error === undefined).length} ` +
      `ERRORS=${results.filter((r) => r.error !== undefined).length}`
  );
  for (const leak of leaks) console.log(`  leak: ${leak.row.name}`);
  for (const cost of costs)
    console.log(`  cost: ${cost.row.name} -> ${cost.error ?? cost.refusal}`);

  // Optional cost probe: source population is measured outside the timed guard calls. A fresh
  // destination must not require a recursive source scan before an ordinary copy can start.
  if (process.argv.includes("--copy-cost")) {
    const fixture = buildWriteFixture(mkdtempSync(path.join(base, "cost-")));
    const source = path.join(fixture.workspace, "large-source");
    mkdirSync(source);
    for (let directory = 0; directory < 48; directory += 1) {
      const dir = path.join(source, String(directory));
      mkdirSync(dir);
      for (let file = 0; file < 1000; file += 1) writeFileSync(path.join(dir, String(file)), "");
    }
    const entries = readdirSync(source, { recursive: true }).length;
    const guard = createPaneGuard({
      workspace: fixture.workspace,
      ompPid: process.pid,
      scratch: fixture.scratch,
    });
    const elapsed: number[] = [];
    for (let run = 0; run < 3; run += 1) {
      const started = performance.now();
      const refusal = guard.bash("cp -r large-source dir/", fixture.workspace, {
        HOME: fixture.home,
        LEGION_WORKSPACE: fixture.workspace,
        TMPDIR: fixture.scratch,
        PATH: process.env.PATH,
      });
      elapsed.push(performance.now() - started);
      if (refusal !== undefined) throw new Error(`fresh copy refused: ${refusal}`);
    }
    // Leave a small, measured amount of the walk budget, then copy into an existing destination
    // subtree larger than it: each entry the copy inspects is charged to the same budget, so the
    // copy is refused at the walk limit (`destination_walk_budget_refused=true`).
    const env = {
      HOME: fixture.home,
      LEGION_WORKSPACE: fixture.workspace,
      TMPDIR: fixture.scratch,
      PATH: process.env.PATH,
    };
    const nonRecursive: number[] = [];
    for (let run = 0; run < 3; run += 1) {
      const started = performance.now();
      const refusal = guard.bash("cp large-source dir/", fixture.workspace, env);
      nonRecursive.push(performance.now() - started);
      if (refusal !== undefined) throw new Error(`non-recursive copy refused: ${refusal}`);
    }
    let allowed = 0;
    let refused = 100_000;
    while (allowed + 1 < refused) {
      const middle = Math.floor((allowed + refused) / 2);
      if (guard.bash("true;".repeat(middle), fixture.workspace, env)?.includes("walk limit")) {
        refused = middle;
      } else {
        allowed = middle;
      }
    }
    const existing = path.join(fixture.workspace, "dir", "large-source", "0");
    mkdirSync(existing, { recursive: true });
    for (let file = 0; file < 200; file += 1) writeFileSync(path.join(existing, String(file)), "");
    const command = `${"true;".repeat(Math.max(0, allowed - 16))}cp -r large-source dir/`;
    console.log(
      `${label}: destination_walk_budget_refused=${guard.bash(command, fixture.workspace, env)?.includes("walk limit") === true}`
    );
    console.log(
      `${label}: source_entries=${entries} fresh_copy_ms=${elapsed.map((ms) => ms.toFixed(3)).join(",")} ` +
        `nonrecursive_ms=${nonRecursive.map((ms) => ms.toFixed(3)).join(",")}`
    );
  }
} finally {
  rmSync(base, { recursive: true, force: true });
}
