/** The harness ports, resolved and validated for every reader in `e2e/`.
 *
 * An unset or empty variable is port `0`: the listener binds port 0, so the OS chooses its port,
 * and the process reports the port it got on its listening line. `e2e/harness-start.ts` reads
 * those reports and returns the ports by variable, and `e2e/run.ts` exports them before it starts
 * the command, so every reader below that runs under it sees the real ports. A set value is used
 * as given: its listener binds exactly that port, and an occupied one fails with "address already
 * in use" rather than being replaced. `PLAIN_HTTP_PORT` is the plain-HTTP project's browser-facing
 * proxy, which only the TypeScript harness reads. `DISPATCH_E2E_PORT_A` and `DISPATCH_E2E_PORT_B`
 * are Dispatch A's and B's in two-process mode (`e2e/harness-processes.ts`), where
 * `DISPATCH_E2E_PORT` is the round-robin proxy's; unset, each is held before the proxy starts, so
 * the proxy can name it (`e2e/harness-start.ts`). `DISPATCH_E2E_SIGNIN_ISSUER_PORT` is the dev
 * OIDC issuer's under `DISPATCH_E2E_SIGNIN=oidc` (`e2e/harness-signin.ts`), which always serves
 * on a port the OS chooses: `e2e/run-signin-issuer.sh` refuses it set. `REAL_BROKER_PORT` is the
 * real secrets broker's and `DEV_OIDC_ISSUER_PORT` its dev OIDC issuer's (`e2e/run-broker.sh`),
 * the pair `e2e/broker-approval.e2e.ts` drives an approval on. A run starts that pair only when a
 * spec of its can reach it (`e2e/harness-real-broker.ts`), so neither port takes part in the
 * collision check otherwise. The issuer serves on a port the OS chooses, as the sign-in issuer
 * does, and `e2e/run-broker.sh` refuses `DEV_OIDC_ISSUER_PORT` set for it. The broker half reads
 * that variable instead, which `e2e/harness-start.ts` exports from the issuer's report, since the
 * broker takes the issuer's URL as its token issuer and its sign-in provider.
 *
 * A set value that is not a port in canonical decimal is refused naming its variable, because
 * every later consumer turns it into something that names nothing: `net.connect` raises
 * `ERR_SOCKET_BAD_PORT` on `NaN`, and a URL built from a value like `1e4` or `0x2249` points nowhere
 * anything is listening. `0` is refused too: unset is how a caller asks for a port the OS chooses,
 * and a written `0` would read as a port a URL can name. A leading zero is not canonical either:
 * `08777` and the `8777` it parses to are one port written two ways, and the point of validating
 * here is that only one representation leaves this module.
 *
 * `harnessPorts` reads the environment when it is called, not when this module is imported: the
 * harness's starters (`e2e/harness-start.ts` through `e2e/local-instance.ts` and the docs media
 * harness) import it before the listeners have reported, and the modules they import afterwards
 * must see the ports those set. It opens no socket, so importing it is safe from any process.
 */

import { harnessProcessCount } from "./harness-processes";
import { usesRealBroker } from "./harness-real-broker";
import { harnessSignIn } from "./harness-signin";

/** Each harness listener's port variable. */
export const harnessVariables = {
  devIssuer: "DEV_OIDC_ISSUER_PORT",
  dispatch: "DISPATCH_E2E_PORT",
  dispatchA: "DISPATCH_E2E_PORT_A",
  dispatchB: "DISPATCH_E2E_PORT_B",
  fakeBroker: "FAKE_BROKER_PORT",
  fakeEnvoy: "FAKE_ENVOY_PORT",
  fakeGithub: "FAKE_GITHUB_PORT",
  plainHttp: "PLAIN_HTTP_PORT",
  realBroker: "REAL_BROKER_PORT",
  signinIssuer: "DISPATCH_E2E_SIGNIN_ISSUER_PORT",
} as const;

export type HarnessListenerKey = keyof typeof harnessVariables;

/** One harness listener's port, beside the variable a refusal has to name. `port` is 0 when the
 *  variable is unset: the listener binds port 0, and the OS chooses. */
export interface HarnessPort {
  readonly port: number;
  readonly variable: string;
}

function harnessPort(env: NodeJS.ProcessEnv, variable: string): HarnessPort {
  const given = env[variable] ?? "";
  if (given === "") return { port: 0, variable };
  const port = Number(given);
  if (!/^[1-9][0-9]{0,4}$/.test(given) || port > 65535) {
    throw new Error(
      `${variable} must be a port number, not ${JSON.stringify(given)}. Leave it unset to have ` +
        "the OS choose one."
    );
  }
  return { port, variable };
}

/** The ports by listener, the one place a port is paired with its variable, as `env` names them
 *  now. Two set variables naming one port are refused: each would pass a per-port check, and
 *  every consumer would then fail in its own words — the second listener dying on `EADDRINUSE`
 *  without naming a variable. Refused here, where the ports resolve, so every caller refuses it
 *  the same way, including the Bun listeners. A's and B's ports take part only in two-process
 *  mode, the one mode that listens on them, the sign-in issuer's only under
 *  `DISPATCH_E2E_SIGNIN=oidc`, and the real broker's and its issuer's only when this run starts
 *  that pair (`e2e/harness-real-broker.ts`). */
export function harnessPorts(
  env: NodeJS.ProcessEnv = process.env
): Record<HarnessListenerKey, HarnessPort> {
  const ports = {
    devIssuer: harnessPort(env, harnessVariables.devIssuer),
    dispatch: harnessPort(env, harnessVariables.dispatch),
    dispatchA: harnessPort(env, harnessVariables.dispatchA),
    dispatchB: harnessPort(env, harnessVariables.dispatchB),
    fakeBroker: harnessPort(env, harnessVariables.fakeBroker),
    fakeEnvoy: harnessPort(env, harnessVariables.fakeEnvoy),
    fakeGithub: harnessPort(env, harnessVariables.fakeGithub),
    plainHttp: harnessPort(env, harnessVariables.plainHttp),
    realBroker: harnessPort(env, harnessVariables.realBroker),
    signinIssuer: harnessPort(env, harnessVariables.signinIssuer),
  };
  const given = Object.values(ports).filter(
    (entry) =>
      entry.port !== 0 &&
      (harnessProcessCount === 2 || (entry !== ports.dispatchA && entry !== ports.dispatchB)) &&
      (harnessSignIn === "oidc" || entry !== ports.signinIssuer) &&
      (usesRealBroker || (entry !== ports.devIssuer && entry !== ports.realBroker))
  );
  const collisions = [...new Set(given.map((entry) => entry.port))]
    .map((port) => ({
      port,
      variables: given.filter((entry) => entry.port === port).map((entry) => entry.variable),
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
      `The Dispatch e2e harness cannot start: ${named}. Give each harness server its own port, ` +
        "or leave the variables unset to have the OS choose."
    );
  }
  return ports;
}
