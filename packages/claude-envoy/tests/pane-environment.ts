import { afterEach, beforeEach } from "bun:test"

// Every variable a Legion pane carries that an in-process resolver reads: resolveDispatchConfig
// (DISPATCH_URL; DISPATCH_TOKEN_FILE ahead of DISPATCH_TOKEN) and createEnvoyClient
// (ENVOY_TOKEN_FILE ahead of ENVOY_TOKEN). A fixture that sets only the plain token would still
// send the pane's real one, and a file pointer that does not resolve fails the client outright.
const paneVariables = [
  "DISPATCH_URL",
  "DISPATCH_TOKEN",
  "DISPATCH_TOKEN_FILE",
  "ENVOY_TOKEN",
  "ENVOY_TOKEN_FILE",
] as const

// Starts each test in the calling file with those variables unset and restores the process's own
// values after it. A test that spawns a subprocess with a literal env needs none of this.
export function isolatePaneEnvironment(): void {
  const original = paneVariables.map((name) => [name, process.env[name]] as const)
  beforeEach(() => {
    for (const name of paneVariables) delete process.env[name]
  })
  afterEach(() => {
    for (const [name, value] of original) {
      if (value === undefined) delete process.env[name]
      else process.env[name] = value
    }
  })
}
