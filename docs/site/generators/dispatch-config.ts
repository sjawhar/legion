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
    "The libraries Dispatch is built on read a few more variables for themselves, outside this table: `HOME` (where the data directory, `~/.local/share/dispatch`, is, and the user `envoy.json` when `XDG_CONFIG_HOME` is unset or empty), libpq's `PG*` variables (connection parameters `DATABASE_URL` leaves out), `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` (outbound HTTP), `SSL_CERT_FILE` and `SSL_CERT_DIR` (the certificates Dispatch trusts), and the Go runtime's `GO*` variables and `TZ`.",
    "",
    "A setting with a file form can name a file that holds its value instead. When Dispatch reads that setting, the file's trimmed contents win over the variable, and a file that cannot be read, or holds nothing, refuses startup.",
    "",
    "`envoy.json` is read from two files: the user file, `legion/envoy.json` under `XDG_CONFIG_HOME` (`~/.config` when that is unset or empty; a path that is not absolute refuses startup), and the checkout's file, `.envoy/envoy.json` in the directory Dispatch starts in, which may hold only a `dispatch` section: an `envoy` section there refuses startup, naming it and the user file it belongs in (the agents' clients ignore it with a notice instead), and a `.envoy` that is a plain file counts as no checkout file. Where both set a `dispatch` key, the checkout's file wins, and `DISPATCH_SERVER_URL` and `NATS_URLS` win over both. Dispatch reads `envoy.natsUrls` whatever `envoy.enabled` says, since `enabled` switches a session's Envoy, not the server's. The OpenCode-era files are not read. A user file still at `~/.config/opencode/envoy.json`, with none at `~/.config/legion/envoy.json` and `XDG_CONFIG_HOME` unset, empty or `~/.config`, refuses startup, naming both paths and the move, whatever `DISPATCH_SERVER_URL` and `NATS_URLS` say; a checkout's `.opencode/envoy.json`, while the checkout has no `.envoy/envoy.json`, is one WARN at start naming the rename. A file that carries a top-level `natsUrls` is refused, naming `envoy.natsUrls`; a JSON `null` at the top level, as a section or as any key's value is refused, naming the key; and JSON that does not parse is refused naming the file, the line and the column, never the text it stopped at. These refusals stop every start, `DISPATCH_NATS_DISABLED` set or not.",
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
  sources: ["packages/envoy/cmd/dispatch/settings.go"],
  body: () => render(readSettings()),
});
