#!/usr/bin/env bun
/**
 * Stands in for `omp --mode rpc` in the deployment-instructions E2E
 * (`real-deployment-instructions-e2e.test.ts`): records the argv it was launched with (everything
 * after the script path, as JSON) to `argv.json` in its cwd — the `cd <dir>` target of the pane's
 * shim command — then exits 0. Proves what the pane's shell actually handed the wrapped process
 * after `"$(cat …)"` expansion, which no mocked `run` callback can. The record is written to
 * `argv.json.tmp` and renamed into place, so `argv.json` never exists without its full content.
 */
import { renameSync, writeFileSync } from "node:fs";

writeFileSync("argv.json.tmp", JSON.stringify(process.argv.slice(2)));
renameSync("argv.json.tmp", "argv.json");
