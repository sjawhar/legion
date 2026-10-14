import { afterEach, beforeEach } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";

// Every variable that picks the envoy.json a plugin or a `dispatch` command reads, HOME, under
// which the neutral file lives, and the credential pointers a Legion pane carries beside them. A
// shell an Oh My Pi or Claude Code session runs carries ENVOY_CONFIG_FILE naming the developer's
// own file, which the plugin would read (and whose `!` commands it would run) ahead of HOME; a
// pane's pointer to a file that has gone away would fail tests that never asked for it.
const configVariables = [
  "ENVOY_CONFIG_FILE",
  "OPENCODE_CONFIG_DIR",
  "CLAUDE_CONFIG_DIR",
  "OMP_PROFILE",
  "PI_CODING_AGENT_DIR",
  "XDG_CONFIG_HOME",
  "HOME",
  "ENVOY_TOKEN",
  "ENVOY_TOKEN_FILE",
  "NATS_NKEY_SEED",
  "NATS_NKEY_SEED_FILE",
  "DISPATCH_TOKEN_FILE",
] as const;

/**
 * Starts each test in the calling file with HOME a scratch directory, ENVOY_CONFIG_FILE naming a
 * file in it that holds no section, and every other variable that picks an envoy.json unset, so
 * Dispatch and Envoy come only from the variables the test sets; restores the process's own values
 * and removes the directory after it. Returns the running test's scratch directory.
 */
export function isolateEnvoyConfig(): () => string {
  const original = configVariables.map((name) => [name, process.env[name]] as const);
  let directory = "";
  beforeEach(() => {
    for (const name of configVariables) delete process.env[name];
    directory = mkdtempSync(path.join(os.tmpdir(), "envoy-plugin-config-"));
    process.env.HOME = directory;
    process.env.ENVOY_CONFIG_FILE = path.join(directory, "envoy.json");
    writeFileSync(process.env.ENVOY_CONFIG_FILE, "{}\n");
  });
  afterEach(() => {
    for (const [name, value] of original) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
    rmSync(directory, { recursive: true, force: true });
  });
  return () => directory;
}
