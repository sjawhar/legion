#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/configuration.md from Dispatch's settings table
// (packages/envoy/cmd/dispatch/settings.go), read through `envoy-dispatch settings`, which prints
// every environment variable the server and its subcommands read. Contract: scripts/generate.ts;
// lib/envoy-dispatch.ts runs the binary.
import { readEnvoyDispatch } from "./lib/envoy-dispatch.ts";
import { inline, writePage } from "./lib/markdown.ts";

interface Setting {
  name: string;
  file: string | null;
  default: string | null;
  required: string;
  description: string;
}

function readSettings(): Setting[] {
  return readEnvoyDispatch("settings", "settings", ["name", "required", "description"]).map(
    (setting) => {
      for (const field of ["file", "default"]) {
        if (setting[field] !== null && typeof setting[field] !== "string") {
          throw new Error(`setting ${setting.name} has a ${field} that is neither text nor null`);
        }
      }
      if (!/^(yes|no|when .+)$/.test(setting.required as string)) {
        throw new Error(`setting ${setting.name} has required ${JSON.stringify(setting.required)}`);
      }
      return setting as unknown as Setting;
    }
  );
}

function render(settings: Setting[]): string {
  const capitalized = (text: string) => inline(text.replace(/^./, (first) => first.toUpperCase()));
  const lines = [
    `Dispatch's server, \`envoy-dispatch\`, takes the ${settings.length} settings below from its environment. \`envoy-dispatch settings\` prints the same table from the binary you run.`,
    "",
    "A setting with a file form can name a file that holds its value instead. When Dispatch reads that setting, the file's trimmed contents win over the variable, and a file that cannot be read, or holds nothing, refuses startup.",
    "",
  ];
  for (const setting of settings) {
    lines.push(`## \`${setting.name}\``, "", inline(setting.description), "");
    lines.push(`- **Required:** ${capitalized(setting.required)}`);
    if (setting.default !== null) {
      lines.push(`- **When unset:** ${capitalized(setting.default)}`);
    }
    if (setting.file !== null) {
      lines.push(`- **File form:** \`${setting.file}\``);
    }
    lines.push("");
  }
  return lines.join("\n");
}

writePage({
  path: "dispatch/reference/configuration.md",
  title: "Configuration",
  description:
    "Every environment variable Dispatch's server reads: what it does, whether it is required, and what it means when unset.",
  source: "packages/envoy/cmd/dispatch/settings.go",
  body: () => render(readSettings()),
});
