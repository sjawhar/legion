#!/usr/bin/env bun
// The tester-proof pane, as the Legion daemon's tmux runtime launches a phase worker, pointed at
// rig.sh's daemon stand-in. Writes two files into the run directory:
//   worker.env   the pane's environment, one KEY=value line each: the grant rig's
//                `workerEnvironment` over <base env file>, the variables rig.sh gives every agent
//   system-args  the pane's one `--append-system-prompt` argument, as shell text: the role prompt
//                files from <roles dir> then the daemon's own from <daemon prompts dir>, in the
//                order prompts.Compose gives a phase worker (packages/daemon-go/internal/prompts),
//                then its addressing sentence (addressingFragment, internal/daemon/specs.go), as
//                omplaunch.SystemPromptArgument renders them
//
//   worker-pane.ts <run dir> <base env file> <daemon port> <profile> <project> <issue> <role>
//                  <roles dir> <daemon prompts dir>
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { controllerToken, isLegionRole, roleToken, roleTopic } from "@legion/contracts";
import { workerEnvironment } from "../grant-rig/run";

const [runDir, baseEnvFile, port, profile, project, issue, role, rolesDir, daemonPromptsDir] =
  Bun.argv.slice(2);
if (
  runDir === undefined ||
  baseEnvFile === undefined ||
  port === undefined ||
  profile === undefined ||
  project === undefined ||
  issue === undefined ||
  role === undefined ||
  rolesDir === undefined ||
  daemonPromptsDir === undefined
) {
  console.error(
    "usage: worker-pane.ts <run dir> <base env file> <daemon port> <profile> <project> <issue> <role> <roles dir> <daemon prompts dir>"
  );
  process.exit(2);
}
if (!isLegionRole(role) || role === "architect" || role === "merger") {
  console.error(`worker-pane.ts: ${role} is not a phase worker the shared role bundle composes`);
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

// prompts.Compose for a planner, implementer, tester or reviewer: the shared parts, then the
// daemon's part for the role and the part every phase worker shares.
const promptPaths = [
  ...["core/common.md", `core/${role}.md`, "mechanics/headless.md", `${role}.md`].map((part) =>
    path.join(rolesDir, part)
  ),
  ...[`${role}.md`, "worker-common.md"].map((part) => path.join(daemonPromptsDir, part)),
];
for (const file of promptPaths) {
  if (!existsSync(file)) {
    console.error(`worker-pane.ts: the role prompt ${file} is missing`);
    process.exit(2);
  }
}
// The pane's tree is its issue (workerEnvironment's LEGION_TREE), so that issue's architect owns it.
const addressing =
  `Legion addressing: your role topic is \`${roleTopic(roleToken(project, issue, role))}\`; ` +
  `the architect that owns your issue is \`${roleTopic(roleToken(project, issue, "architect"))}\`; ` +
  `the project's controller is \`${roleTopic(controllerToken(project))}\`; a sibling role on your ` +
  "issue is your topic with the trailing `-<role>` replaced.";
// shellprefix.Word for each path, and the escaping of the four characters a shell interprets inside
// double quotes for the addressing text.
const word = (value: string): string =>
  /[^A-Za-z0-9_./:-]/.test(value) ? `'${value.replaceAll("'", "'\\''")}'` : value;
const doubleQuoted = (value: string): string => value.replaceAll(/[\\"$`]/g, (c) => `\\${c}`);
writeFileSync(
  path.join(runDir, "system-args"),
  `--append-system-prompt "$(cat ${promptPaths.map(word).join(" ")})\n\n${doubleQuoted(addressing)}"`
);
