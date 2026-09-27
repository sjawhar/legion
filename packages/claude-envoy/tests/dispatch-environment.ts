import { afterEach, beforeEach } from "bun:test"

// Every variable resolveDispatchConfig reads. A Legion pane carries DISPATCH_URL and
// DISPATCH_TOKEN_FILE, and the resolver reads the file pointer ahead of DISPATCH_TOKEN, so a
// fixture that sets only DISPATCH_URL and DISPATCH_TOKEN would still send the pane's real token.
const dispatchVariables = ["DISPATCH_URL", "DISPATCH_TOKEN", "DISPATCH_TOKEN_FILE"] as const

// Starts each test in the calling file with the whole family unset and restores the process's
// own values after it.
export function isolateDispatchEnvironment(): void {
  const original = dispatchVariables.map((name) => [name, process.env[name]] as const)
  beforeEach(() => {
    for (const name of dispatchVariables) delete process.env[name]
  })
  afterEach(() => {
    for (const [name, value] of original) {
      if (value === undefined) delete process.env[name]
      else process.env[name] = value
    }
  })
}
