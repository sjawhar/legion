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
 * on a port the OS chooses: `e2e/run-signin-issuer.sh` refuses it set.
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
 *
 * A run of more than one Playwright worker runs one lane per worker (`e2e/run.ts`): a whole
 * harness, with a Postgres, ports and a file-store bucket of its own. Lane `i` carries every one
 * of its variables under `DISPATCH_E2E_LANE_<i>_<V>`, and `laneValue` below is the one place a
 * reader picks its own: the lane's name when it is set, the plain name otherwise, so a one-lane
 * run reads exactly what it read before lanes existed. The lane is `TEST_PARALLEL_INDEX`, which
 * Playwright sets in each worker process and a restarted worker keeps, and which is 0 in every
 * process that is not a worker: the config's own, `e2e/preflight.ts`, and each harness listener
 * `e2e/run.ts` starts before Playwright, which is handed its lane's values under the plain names.
 */

import { harnessProcessCount } from "./harness-processes";
import { harnessSignIn } from "./harness-signin";

/** How many Playwright workers, and so how many lanes, a run has. */
export const workerCountVariable = "DISPATCH_E2E_WORKERS";
/** How many lanes `e2e/run.ts` started, which it exports for the command it runs. */
export const laneCountVariable = "DISPATCH_E2E_LANES";

function count(env: NodeJS.ProcessEnv, variable: string): number | undefined {
  const given = env[variable] ?? "";
  if (given === "") return undefined;
  if (!/^[1-9][0-9]*$/.test(given)) {
    throw new Error(
      `${variable} must be how many lanes the run has, a number from 1 up, not ` +
        `${JSON.stringify(given)}. Leave it unset for one.`
    );
  }
  return Number(given);
}

/** The lanes a run asks for: `DISPATCH_E2E_WORKERS`, where unset or empty is one. */
export function workerCount(env: NodeJS.ProcessEnv = process.env): number {
  return count(env, workerCountVariable) ?? 1;
}

/** The lanes a run has: the `DISPATCH_E2E_LANES` `e2e/run.ts` exported, else what this run asks
 *  for, so a run that addresses servers a person started (`DISPATCH_E2E_REUSE_SERVERS=1`) reads
 *  its own worker count. */
export function laneCount(env: NodeJS.ProcessEnv = process.env): number {
  return count(env, laneCountVariable) ?? workerCount(env);
}

/** The name lane `lane` carries `variable` under. */
export function laneVariable(lane: number, variable: string): string {
  return `DISPATCH_E2E_LANE_${lane}_${variable}`;
}

/** The lane the process `env` belongs to. */
export function laneIndex(env: NodeJS.ProcessEnv = process.env): number {
  const given = env.TEST_PARALLEL_INDEX ?? "";
  if (given === "") return 0;
  if (!/^(0|[1-9][0-9]*)$/.test(given)) {
    throw new Error(
      `TEST_PARALLEL_INDEX must be the lane's index, a number from 0 up, not ` +
        `${JSON.stringify(given)}. Playwright sets it in each worker process; leave it unset.`
    );
  }
  return Number(given);
}

/** `variable` as this process's lane names it, falling back to the plain name, which a one-lane
 *  run and every process `e2e/run.ts` starts itself read. */
export function laneValue(env: NodeJS.ProcessEnv, variable: string): string | undefined {
  return env[laneVariable(laneIndex(env), variable)] ?? env[variable];
}

/** `env` as a process of lane `lane` reads it, so one process can resolve another lane's ports
 *  and database through the same lookup its workers use (`e2e/preflight.ts`). */
export function asLane(lane: number, env: NodeJS.ProcessEnv = process.env): NodeJS.ProcessEnv {
  return { ...env, TEST_PARALLEL_INDEX: String(lane) };
}

/** Each harness listener's port variable. */
export const harnessVariables = {
  dispatch: "DISPATCH_E2E_PORT",
  dispatchA: "DISPATCH_E2E_PORT_A",
  dispatchB: "DISPATCH_E2E_PORT_B",
  fakeBroker: "FAKE_BROKER_PORT",
  fakeEnvoy: "FAKE_ENVOY_PORT",
  fakeGithub: "FAKE_GITHUB_PORT",
  plainHttp: "PLAIN_HTTP_PORT",
  signinIssuer: "DISPATCH_E2E_SIGNIN_ISSUER_PORT",
} as const;

export type HarnessListenerKey = keyof typeof harnessVariables;

/** One harness listener's port, beside the variable a refusal has to name. `port` is 0 when the
 *  variable is unset: the listener binds port 0, and the OS chooses. */
export interface HarnessPort {
  readonly port: number;
  readonly variable: string;
}

/** `env` with `listener`'s port unset under every name a child this process starts would read it
 *  under: the plain variable and the one this process's lane carries it under. A spec that starts
 *  a second listener of its own wants the OS to choose its port (`e2e/fake-envoy.e2e.ts`,
 *  `e2e/plain-http-proxy.e2e.ts`), and in a lane's run both names are set, so dropping the plain
 *  one alone would still hand the child the port the harness listener holds, and it would die on
 *  `EADDRINUSE` before it printed its listening line. The child keeps `TEST_PARALLEL_INDEX`, so
 *  the lane it resolves is the one cleared here. */
export function withoutHarnessPort(
  listener: HarnessListenerKey,
  env: NodeJS.ProcessEnv = process.env
): NodeJS.ProcessEnv {
  const variable = harnessVariables[listener];
  const rest = { ...env };
  delete rest[variable];
  delete rest[laneVariable(laneIndex(env), variable)];
  return rest;
}

function harnessPort(env: NodeJS.ProcessEnv, variable: string): HarnessPort {
  const given = laneValue(env, variable) ?? "";
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
 *  mode, the one mode that listens on them, and the issuer's only under `DISPATCH_E2E_SIGNIN=oidc`. */
export function harnessPorts(
  env: NodeJS.ProcessEnv = process.env
): Record<HarnessListenerKey, HarnessPort> {
  const ports = {
    dispatch: harnessPort(env, harnessVariables.dispatch),
    dispatchA: harnessPort(env, harnessVariables.dispatchA),
    dispatchB: harnessPort(env, harnessVariables.dispatchB),
    fakeBroker: harnessPort(env, harnessVariables.fakeBroker),
    fakeEnvoy: harnessPort(env, harnessVariables.fakeEnvoy),
    fakeGithub: harnessPort(env, harnessVariables.fakeGithub),
    plainHttp: harnessPort(env, harnessVariables.plainHttp),
    signinIssuer: harnessPort(env, harnessVariables.signinIssuer),
  };
  const given = Object.values(ports).filter(
    (entry) =>
      entry.port !== 0 &&
      (harnessProcessCount === 2 || (entry !== ports.dispatchA && entry !== ports.dispatchB)) &&
      (harnessSignIn === "oidc" || entry !== ports.signinIssuer)
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

/** Why a run of more than one lane refuses a variable the caller set. Each names a thing one
 *  lane owns: a run that kept the caller's value would hand every lane the one database, the one
 *  deployed server, or the one port, and the rows would overwrite each other's state. The port
 *  variables are refused for the last reason, so they share it below. */
const laneConflictReasons: Record<string, string> = {
  DATABASE_URL:
    "each lane runs its own Postgres, whose URL the harness hands its servers and its psql",
  DISPATCH_E2E_PROCESSES:
    "two-process mode is two servers on one lane's database, which one lane runs",
  DISPATCH_E2E_REUSE_SERVERS:
    "the servers a person started are one set of servers, which every lane would share",
  PLAYWRIGHT_BASE_URL: "a deployed run has one server, which every lane would share",
};

/** The variables `env` sets that a run of more than one lane cannot honour, in the order a
 *  message names them. `DISPATCH_E2E_PROCESSES` counts only at `2`: `1` is what one lane runs. */
export function laneConflicts(env: NodeJS.ProcessEnv): readonly string[] {
  const named = Object.keys(laneConflictReasons).filter((variable) =>
    variable === "DISPATCH_E2E_PROCESSES"
      ? env[variable] === "2"
      : (env[variable] ?? "").trim() !== ""
  );
  const ports = Object.values(harnessVariables).filter(
    (variable) => (env[variable] ?? "").trim() !== ""
  );
  return [...named, ...ports].sort();
}

/** Refuses a run of `lanes` lanes whose caller set a variable one lane owns, before anything
 *  starts. `e2e/run.ts` calls it before it adds its own values of those variables. */
export function refuseLaneConflicts(lanes: number, env: NodeJS.ProcessEnv = process.env): void {
  for (const variable of laneConflicts(env)) {
    const reason =
      laneConflictReasons[variable] ??
      "a port is one listener's, and each lane needs listeners of its own";
    throw new Error(
      `${workerCountVariable}=${lanes} runs ${lanes} lanes, each a harness of its own, so ` +
        `${variable} must not be set: ${reason}. Unset ${variable}, or leave ` +
        `${workerCountVariable} unset to run one lane as before.`
    );
  }
}
