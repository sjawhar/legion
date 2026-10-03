#!/usr/bin/env bun
// The tester-proof pane, as the Legion daemon's tmux runtime launches a phase worker, pointed at
// rig.sh's daemon stand-in: the grant rig's `workerPane` over <base env file> (the variables rig.sh
// gives every agent), read from <checkout>'s own daemon module, whose role prompts it composes from
// <checkout>'s bundle. Writes two files into the run directory:
//   worker.env   the pane's environment, one KEY=value line each
//   system-args  the pane's one `--append-system-prompt` argument, as shell text
//
//   worker-pane.ts <run dir> <base env file> <daemon port> <profile> <project> <issue> <role>
//                  <checkout>
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { isLegionRole } from "@legion/contracts";
import { workerPane } from "../grant-rig/run";

const [runDir, baseEnvFile, port, profile, project, issue, role, checkout] = Bun.argv.slice(2);
if (
  runDir === undefined ||
  baseEnvFile === undefined ||
  port === undefined ||
  profile === undefined ||
  project === undefined ||
  issue === undefined ||
  role === undefined ||
  checkout === undefined
) {
  console.error(
    "usage: worker-pane.ts <run dir> <base env file> <daemon port> <profile> <project> <issue> <role> <checkout>"
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
const pane = workerPane(
  { rig: runDir, port: Number(port), profile },
  base,
  { project, issue, role },
  {
    daemonModule: path.join(checkout, "packages", "daemon"),
    rolesDir: path.join(checkout, "packages", "pi-envoy", "roles"),
  }
);
for (const [key, value] of Object.entries(pane.env)) {
  if (value.includes("\n")) {
    console.error(`worker-pane.ts: ${key} holds a newline, which a KEY=value line cannot carry`);
    process.exit(2);
  }
}
writeFileSync(
  path.join(runDir, "worker.env"),
  Object.entries(pane.env)
    .map(([key, value]) => `${key}=${value}\n`)
    .join("")
);
if (pane.systemPromptArgument === undefined) {
  console.error("worker-pane.ts: the daemon composed no system prompt argument");
  process.exit(2);
}
writeFileSync(path.join(runDir, "system-args"), pane.systemPromptArgument);
