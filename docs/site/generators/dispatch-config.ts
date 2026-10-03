#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/configuration.md from Dispatch's settings table
// (packages/envoy/cmd/dispatch/settings.go), read through `envoy-dispatch settings`, which prints
// every environment variable the server and its subcommands read. It runs the `envoy-dispatch` on
// PATH, which scripts/build-binaries.sh builds from this checkout before any generator runs, and
// refuses to run without one.
import { inline, writePage } from "./lib/markdown.ts";

const SOURCE = "packages/envoy/cmd/dispatch/settings.go";
const GENERATOR = "docs/site/generators/dispatch-config.ts";
const COMMAND = ["envoy-dispatch", "settings"];

interface Setting {
  name: string;
  file: string | null;
  default: string | null;
  required: string;
  description: string;
}

function readSettings(): Setting[] {
  if (Bun.which(COMMAND[0]) === null) {
    throw new Error(
      `${COMMAND[0]} is not on PATH: run this generator through docs/site/scripts/generate.ts, which builds it (scripts/build-binaries.sh) and puts it there`
    );
  }
  const run = Bun.spawnSync(COMMAND, { stdout: "pipe", stderr: "pipe" });
  if (run.exitCode !== 0) {
    throw new Error(`${COMMAND.join(" ")} exited ${run.exitCode}:\n${run.stderr.toString()}`);
  }
  const body = JSON.parse(run.stdout.toString()) as { settings?: unknown };
  if (!Array.isArray(body.settings) || body.settings.length === 0) {
    throw new Error(`${COMMAND.join(" ")} printed no settings`);
  }
  return body.settings.map((setting: Record<string, unknown>) => {
    for (const field of ["name", "required", "description"]) {
      if (typeof setting[field] !== "string" || setting[field] === "") {
        throw new Error(`setting ${JSON.stringify(setting)} has no ${field}`);
      }
    }
    for (const field of ["file", "default"]) {
      if (setting[field] !== null && typeof setting[field] !== "string") {
        throw new Error(`setting ${setting.name} has a ${field} that is neither text nor null`);
      }
    }
    if (!/^(yes|no|when .+)$/.test(setting.required as string)) {
      throw new Error(`setting ${setting.name} has required ${JSON.stringify(setting.required)}`);
    }
    return setting as unknown as Setting;
  });
}

function render(settings: Setting[]): string {
  const lines = [
    "---",
    "title: Configuration",
    `description: ${JSON.stringify("Every environment variable Dispatch's server reads: what it does, whether it is required, and what it means when unset.")}`,
    "editUrl: false",
    "---",
    "",
    `> Generated from \`${SOURCE}\` by \`${GENERATOR}\`. Edit the settings table, not this page.`,
    "",
    `Dispatch's server, \`envoy-dispatch\`, takes the ${settings.length} settings below from its environment. \`envoy-dispatch settings\` prints the same table from the binary you run.`,
    "",
    "A setting with a file form can name a file that holds its value instead. When Dispatch reads that setting, the file's trimmed contents win over the variable, and a file that cannot be read, or holds nothing, refuses startup.",
    "",
  ];
  for (const setting of settings) {
    lines.push(`## \`${setting.name}\``, "", inline(setting.description), "");
    lines.push(
      `- **Required:** ${inline(setting.required.replace(/^./, (first) => first.toUpperCase()))}`
    );
    if (setting.default !== null) {
      lines.push(
        `- **When unset:** ${inline(setting.default.replace(/^./, (first) => first.toUpperCase()))}`
      );
    }
    if (setting.file !== null) {
      lines.push(`- **File form:** \`${setting.file}\``);
    }
    lines.push("");
  }
  return lines.join("\n");
}

writePage(GENERATOR, "dispatch/reference/configuration.md", () => render(readSettings()));
