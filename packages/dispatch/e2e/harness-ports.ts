/** The three harness ports, resolved and validated once for every reader in `e2e/`.
 *
 * `e2e/run-server.sh:21-23` resolves the same variables with `${VAR:-default}`, so an empty value
 * means the default here too — the same rule for all three, matching the process that binds them.
 * A value that is not a port in canonical decimal is refused naming its variable, because every
 * later consumer turns it into something that names nothing: `net.connect` raises
 * `ERR_SOCKET_BAD_PORT` on `NaN`, `Bun.serve` binds a random port for `0`, and a URL built from a
 * value like `1e4` or `0x2249` points nowhere anything is listening. A leading zero is not
 * canonical either: `08777` and the `8777` it parses to are one port written two ways, and the
 * point of validating here is that only one representation leaves this module.
 *
 * This module refuses a bad or duplicated value and otherwise only computes: it opens no socket
 * and reads nothing but the environment, so importing it is safe from any process. The port
 * probe and the reuse decision stay in `e2e/playwright.config.ts` for that reason —
 * `e2e/fake-envoy.ts` and `e2e/fake-github.ts` import this module as plain `bun` processes with
 * no `process.send` and no `PLAYWRIGHT_BASE_URL`, so a probe reached through an import would fire
 * inside them and refuse the second fake as soon as the first is listening.
 */
function harnessPort(variable: string, fallback: string): number {
  const resolved = process.env[variable] || fallback;
  const port = Number(resolved);
  if (!/^[1-9][0-9]{0,4}$/.test(resolved) || port > 65535) {
    throw new Error(`${variable} must be a port number, not ${JSON.stringify(resolved)}.`);
  }
  return port;
}

export const dispatchPort = harnessPort("DISPATCH_E2E_PORT", "8777");
export const fakeEnvoyPort = harnessPort("FAKE_ENVOY_PORT", "9021");
export const fakeGithubPort = harnessPort("FAKE_GITHUB_PORT", "9022");

/** The same three, paired with the variable a message has to name. */
export const harnessPorts = [
  { variable: "DISPATCH_E2E_PORT", port: dispatchPort },
  { variable: "FAKE_ENVOY_PORT", port: fakeEnvoyPort },
  { variable: "FAKE_GITHUB_PORT", port: fakeGithubPort },
];

// Two variables naming one port would each pass a per-port check, and every consumer would then
// fail in its own words: Playwright refuses the second `webServer` without naming a variable, and
// the second fake listener dies on `EADDRINUSE`. Refused here, where the ports resolve, so every
// importer refuses it the same way — including the two fakes, which run as plain `bun` processes.
const collisions = [...new Set(harnessPorts.map((entry) => entry.port))]
  .map((port) => ({
    port,
    variables: harnessPorts.filter((entry) => entry.port === port).map((entry) => entry.variable),
  }))
  .filter((collision) => collision.variables.length > 1);
if (collisions.length > 0) {
  const named = collisions
    .map(({ port, variables }) => {
      const names =
        variables.length > 2
          ? `${variables.slice(0, -1).join(", ")} and ${variables[variables.length - 1]}`
          : variables.join(" and ");
      return `${names} name port ${port}`;
    })
    .join("; ");
  throw new Error(
    `The Dispatch e2e harness cannot start: ${named}. Give each harness server its own port.`
  );
}
