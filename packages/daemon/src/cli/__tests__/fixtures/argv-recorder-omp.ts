#!/usr/bin/env bun
/**
 * Stands in for OMP in the deployment-instructions E2E
 * (`real-deployment-instructions-e2e.test.ts`): records the argv it was launched with (everything
 * after the script path, as JSON) to `argv.json` in its cwd — the `cd <dir>` target of the pane's
 * command — then exits 0. Proves what the pane's shell actually handed the launched process after
 * `"$(cat …)"` expansion, which no mocked `run` callback can. The record is written to
 * `argv.json.tmp` and renamed into place, so `argv.json` never exists without its full content.
 *
 * With `ARGV_RECORDER_LINGER_MS` set (inherited from the tmux server's environment), the process
 * stays alive that long after recording. A pane running the stand-in bare — the controller's
 * interactive pane, no `legion worker-shim` around it — would otherwise close the instant the
 * record is written, before the daemon has marked the window's owner and read the pane's process
 * identity, which real OMP never does: it lives for the pane's whole life.
 */
import { renameSync, writeFileSync } from "node:fs";

writeFileSync("argv.json.tmp", JSON.stringify(process.argv.slice(2)));
renameSync("argv.json.tmp", "argv.json");
const linger = Number(process.env.ARGV_RECORDER_LINGER_MS ?? "0");
if (Number.isFinite(linger) && linger > 0) {
  await Bun.sleep(linger);
}
