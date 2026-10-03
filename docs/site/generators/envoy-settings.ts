#!/usr/bin/env bun
// Writes <content dir>/envoy/reference/settings.md from the listener's settings table
// (packages/envoy/cmd/listener/settings.go), read through `envoy-listener settings`, which prints
// every listener setting. Contract: scripts/generate.ts; lib/binary-table.ts runs the binary.
import { readBinaryTable } from "./lib/binary-table.ts";
import { inline, writePage } from "./lib/markdown.ts";

interface Setting {
  name: string;
  file: string | null;
  default: string | null;
  required: string;
  description: string;
}

function readSettings(): Setting[] {
  return readBinaryTable("envoy-listener", "settings", "settings", [
    "name",
    "required",
    "description",
  ]).map((setting) => {
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
  const capitalized = (text: string) => inline(text.replace(/^./, (first) => first.toUpperCase()));
  const lines = [
    `The Envoy listener, \`envoy-listener\`, takes the ${settings.length} settings below from the environment. \`envoy-listener settings\` prints the same table from the binary you run.`,
    "",
    "The libraries the listener is built on read a few more variables for themselves, outside this table: `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` (outbound HTTP), `SSL_CERT_FILE` and `SSL_CERT_DIR` (the certificates it trusts), and the Go runtime's `GO*` variables and `TZ`.",
    "",
    "A setting with a file form can name a file that holds its value instead. The file's trimmed contents win over the variable, and a file form that is set but empty, or names a file that cannot be read or holds nothing, refuses startup.",
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
  path: "envoy/reference/settings.md",
  title: "Listener settings",
  description:
    "Every Envoy listener setting: what it does, whether it is required, and what it means when unset.",
  source: "packages/envoy/cmd/listener/settings.go",
  body: () => render(readSettings()),
});
