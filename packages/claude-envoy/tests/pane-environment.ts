import { afterEach, beforeEach } from "bun:test"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

// Every variable an in-process resolver reads that a Legion pane or the caller's own session
// carries: sessionDispatchSettings (DISPATCH_URL; DISPATCH_TOKEN_FILE ahead of DISPATCH_TOKEN), the
// Envoy credentials (ENVOY_TOKEN_FILE ahead of ENVOY_TOKEN), and what picks the envoy.json both read
// (CLAUDE_CONFIG_DIR, then ENVOY_CONFIG_FILE, then XDG_CONFIG_HOME). A fixture that sets only the
// plain token would still send the pane's real one, and a file pointer that does not resolve fails
// the client outright.
const paneVariables = [
  "DISPATCH_URL",
  "DISPATCH_TOKEN",
  "DISPATCH_TOKEN_FILE",
  "ENVOY_TOKEN",
  "ENVOY_TOKEN_FILE",
  "CLAUDE_CONFIG_DIR",
  "ENVOY_CONFIG_FILE",
  "XDG_CONFIG_HOME",
] as const

// Starts each test in the calling file with those variables unset, and ENVOY_CONFIG_FILE naming a
// file that holds no section, so nothing comes from the caller's own envoy.json; restores the
// process's own values after it. A test that spawns a subprocess with a literal env needs none of
// this.
export function isolatePaneEnvironment(): void {
  const original = paneVariables.map((name) => [name, process.env[name]] as const)
  let configDirectory = ""
  beforeEach(() => {
    for (const name of paneVariables) delete process.env[name]
    configDirectory = mkdtempSync(join(tmpdir(), "claude-envoy-config-"))
    process.env["ENVOY_CONFIG_FILE"] = join(configDirectory, "envoy.json")
    writeFileSync(process.env["ENVOY_CONFIG_FILE"], "{}\n")
  })
  afterEach(() => {
    for (const [name, value] of original) {
      if (value === undefined) delete process.env[name]
      else process.env[name] = value
    }
    rmSync(configDirectory, { recursive: true, force: true })
  })
}
