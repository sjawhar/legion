#!/usr/bin/env bun
/**
 * Measures every row of `PATH_ROWS` twice against whatever guard this checkout holds: what
 * `guard.bash` returns, and what real bash does to a canary HOME. Run it at two revisions —
 * swapping `src/legion/pane-guard.ts` is enough — and diff the output.
 *
 *   bun scripts/measure-pane-guard-paths.ts            # one line per row, then the counts
 *   bun scripts/measure-pane-guard-paths.ts --summary  # the counts only
 *
 * `src/legion/pane-guard-bash.test.ts` asserts the same measurement, through the same functions.
 */
import { spawnSync } from "node:child_process";
import { lstatSync, mkdtempSync, realpathSync, rmSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  buildPathFixture,
  canaryDigest,
  DOTDOT_COMPONENT,
  measureAllPathRows,
  PATH_ROWS,
} from "../src/legion/pane-guard-path-rows";

const summary = process.argv.includes("--summary");
// A scratch root of its own: never the real /tmp, and the canary HOME is not under it.
const root = mkdtempSync(path.join(os.tmpdir(), "pane-guard-paths-"));
try {
  const results = measureAllPathRows(root);

  // The properties the batch turns on, as booleans, once — not as prose.
  const fixture = buildPathFixture(mkdtempSync(path.join(root, "props-")));
  console.log(
    [
      `fixture.e_is_a_symlink=${lstatSync(`${fixture.workspace}/e`).isSymbolicLink()}`,
      `fixture.dotdot_after_e_is_the_home=${path.dirname(realpathSync(`${fixture.workspace}/e`)) === realpathSync(fixture.home)}`,
      `fixture.home_outside_workspace=${!fixture.home.startsWith(`${fixture.workspace}/`)}`,
      `fixture.home_outside_scratch=${!fixture.home.startsWith(`${fixture.scratch}/`)}`,
      `fixture.scratch_is_not_real_tmp=${fixture.scratch !== "/tmp"}`,
      `rows.dotdot_rows_carry_a_literal_dotdot=${PATH_ROWS.filter((row) => row.dotdot).every((row) => DOTDOT_COMPONENT.test(row.command))}`,
      `canary.digest_counts_entries=${Number(canaryDigest(fixture.home).split(":")[0]) > 5}`,
    ].join(" ")
  );

  if (!summary) {
    for (const { row, refusal, live, exit } of results) {
      const verdict = refusal === undefined ? "ALLOW" : "refused";
      console.log(
        `${row.family}\t${row.role}\t${row.name}\t${verdict}\tlive=${live}\texit=${exit}\t${row.command}`
      );
    }
  }
  const leaks = results.filter((result) => result.live && result.refusal === undefined);
  console.log(
    `rows=${results.length} live=${results.filter((r) => r.live).length} ` +
      `LEAKS=${leaks.length} ` +
      `refused_live=${results.filter((r) => r.live && r.refusal !== undefined).length} ` +
      `allowed_intact=${results.filter((r) => !r.live && r.refusal === undefined).length} ` +
      `refused_intact=${results.filter((r) => !r.live && r.refusal !== undefined).length}`
  );
  for (const leak of leaks) console.log(`LEAK ${leak.row.name}`);
} finally {
  // The fixture's unsearchable directory is there on purpose; nothing removes it until it opens.
  spawnSync("chmod", ["-R", "u+rwX", root]);
  rmSync(root, { recursive: true, force: true });
}
