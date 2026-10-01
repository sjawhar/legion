#!/usr/bin/env bun
// The tester-proof pane, as the TypeScript daemon launches a phase worker, pointed at rig.sh's
// daemon stand-in. Writes two files into the run directory:
//   worker.env   the pane's environment, one KEY=value line each: the grant rig's
//                `workerEnvironment` over <base env file>, the variables rig.sh gives every agent
//   system-args  the pane's one `--append-system-prompt` argument, as shell text: the daemon's
//                prompt parts for the role from <roles dir>, then its addressing fragment
//
//   worker-pane.ts <run dir> <base env file> <daemon port> <profile> <project> <issue> <role> <roles dir>
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { isLegionRole } from "@legion/contracts";
import { addressingFragment, workerPromptPaths } from "../../../daemon/src/daemon/processes";
import { systemPromptArguments } from "../../../daemon/src/daemon/runtime-tmux";
import { workerEnvironment } from "../grant-rig/run";

const [runDir, baseEnvFile, port, profile, project, issue, role, rolesDir] = Bun.argv.slice(2);
if (
  runDir === undefined ||
  baseEnvFile === undefined ||
  port === undefined ||
  profile === undefined ||
  project === undefined ||
  issue === undefined ||
  role === undefined ||
  rolesDir === undefined
) {
  console.error(
    "usage: worker-pane.ts <run dir> <base env file> <daemon port> <profile> <project> <issue> <role> <roles dir>"
  );
  process.exit(2);
}
if (!isLegionRole(role)) {
  console.error(`worker-pane.ts: ${role} is not a Legion role`);
  process.exit(2);
}
const base: Record<string, string> = {};
for (const line of readFileSync(baseEnvFile, "utf8").split("\n")) {
  if (line === "") continue;
  const equals = line.indexOf("=");
  if (equals <= 0) {
    console.error(`worker-pane.ts: ${baseEnvFile} has a line that is not KEY=value: ${line}`);
    process.exit(2);
  }
  base[line.slice(0, equals)] = line.slice(equals + 1);
}
const env = workerEnvironment({ rig: runDir, port: Number(port), profile }, base, {
  project,
  issue,
  role,
});
for (const [key, value] of Object.entries(env)) {
  if (value.includes("\n")) {
    console.error(`worker-pane.ts: ${key} holds a newline, which a KEY=value line cannot carry`);
    process.exit(2);
  }
}
writeFileSync(
  path.join(runDir, "worker.env"),
  Object.entries(env)
    .map(([key, value]) => `${key}=${value}\n`)
    .join("")
);
writeFileSync(
  path.join(runDir, "system-args"),
  systemPromptArguments(
    workerPromptPaths(rolesDir, role),
    addressingFragment(project, issue, issue, role),
    undefined
  )
);
