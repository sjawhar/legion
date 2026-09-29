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
import { mkdtempSync, rmSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
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
  for (const { row, refusal, live, exit } of results) {
    console.log(
      `${(refusal === undefined ? "ALLOW" : "REFUSE").padEnd(7)}${(live ? "CHANGED" : "intact").padEnd(8)}` +
        `exit=${String(exit).padEnd(4)}${row.family.padEnd(9)}${row.name}`
    );
  }

  const leaks = results.filter((result) => result.live && result.refusal === undefined);
  const costs = results.filter(
    (result) => result.row.role === "must-allow" && result.refusal !== undefined
  );
  console.log(
    `\n${label}: rows=${results.length} changed=${results.filter((r) => r.live).length} ` +
      `LEAKS=${leaks.length} COSTS=${costs.length} ` +
      `refused_changed=${results.filter((r) => r.live && r.refusal !== undefined).length} ` +
      `allowed_intact=${results.filter((r) => !r.live && r.refusal === undefined).length}`
  );
  for (const leak of leaks) console.log(`  leak: ${leak.row.name}`);
  for (const cost of costs) console.log(`  cost: ${cost.row.name} -> ${cost.refusal}`);
} finally {
  rmSync(base, { recursive: true, force: true });
}
