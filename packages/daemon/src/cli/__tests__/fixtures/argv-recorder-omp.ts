#!/usr/bin/env bun
/**
 * Stands in for OMP in the deployment-instructions E2E
 * (`real-deployment-instructions-e2e.test.ts`): records the argv it was launched with (everything
 * after the script path, as JSON) to `argv.json` in its cwd — the `cd <dir>` target of the pane's
 * command. It then stays alive until that test creates `argv-recorder.release`, so the test's
 * bare controller pane remains alive while the daemon marks its window and records its process
 * identity. The record is written to `argv.json.tmp` and renamed into place, so `argv.json` never
 * exists without its full content.
 */
import { existsSync, renameSync, writeFileSync } from "node:fs";

const RELEASE_FILE = "argv-recorder.release";
const WAITING_FILE = "argv-recorder.waiting";

writeFileSync("argv.json.tmp", JSON.stringify(process.argv.slice(2)));
renameSync("argv.json.tmp", "argv.json");
writeFileSync(WAITING_FILE, "");
while (!existsSync(RELEASE_FILE)) await Bun.sleep(20);
