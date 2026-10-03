#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/configuration.md from Dispatch's settings table
// (packages/envoy/cmd/dispatch/settings.go), read through `envoy-dispatch settings`, which prints
// every Dispatch setting. Contract: scripts/generate.ts; lib/envoy-dispatch.ts runs the binary.
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
    `Dispatch's server, \`envoy-dispatch\`, and its subcommands take the ${settings.length} Dispatch settings below from the environment. \`envoy-dispatch settings\` prints the same table from the binary you run.`,
    "",
    "The libraries Dispatch is built on read a few more variables for themselves, outside this table: `HOME` (where the data directory, `~/.local/share/dispatch`, and the user `envoy.json` are), libpq's `PG*` variables (connection parameters `DATABASE_URL` leaves out), `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` (outbound HTTP), `SSL_CERT_FILE` and `SSL_CERT_DIR` (the certificates Dispatch trusts), and the Go runtime's `GO*` variables and `TZ`.",
    "",
    "A setting with a file form can name a file that holds its value instead. When Dispatch reads that setting, the file's trimmed contents win over the variable, and a file that cannot be read, or holds nothing, refuses startup.",
    "",
    "`envoy.json` is read from two files: the user file, `~/.config/opencode/envoy.json`, and the repository file, `.opencode/envoy.json` in the directory Dispatch starts in. Where both set a key, the repository file wins, and `DISPATCH_SERVER_URL` and `NATS_URLS` win over both.",
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
    "Every Dispatch setting: what it does, whether it is required, and what it means when unset.",
  source: "packages/envoy/cmd/dispatch/settings.go",
  body: () => render(readSettings()),
});
