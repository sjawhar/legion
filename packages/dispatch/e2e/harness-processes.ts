/** How many Dispatch server processes the harness runs, and how a request names one of them.
 *
 * `DISPATCH_E2E_PROCESSES` unset (or `1`) is the single server on `DISPATCH_E2E_PORT`. `2` runs
 * Dispatch A and B on `DISPATCH_E2E_PORT_A` and `DISPATCH_E2E_PORT_B` against the one
 * `DATABASE_URL`, behind `e2e/round-robin-proxy.ts` on `DISPATCH_E2E_PORT`, so `baseUrl` and every
 * helper still address one origin. Any other value is refused naming the allowed ones, rather
 * than read as one of them: a run that meant two processes and got one passes every spec that
 * cannot tell them apart. `e2e/run-server.sh` refuses it the same way.
 *
 * Like `e2e/harness-ports.ts`, which imports it, this module reads only the environment and opens
 * nothing, so every process in `e2e/` may import it, and each refuses a bad value alike.
 */

/** One of the two processes of two-process mode. */
export type DispatchProcess = "a" | "b";

export const dispatchProcesses: readonly DispatchProcess[] = ["a", "b"];

/** The request header that sends one request to one process, whatever the cookie says. */
export const processPinHeader = "X-Dispatch-E2E-Process";

/** The cookie that sends every request of a browser context to one process, websocket upgrades
 *  included, which carry no header a page can set. */
export const processPinCookie = "dispatch-e2e-process";

/** The cookies that split a browser context between the two processes: `pagePinCookie` sends its
 *  page requests (a navigation, `index.html`, `/assets/*`) to one process and `apiPinCookie` every
 *  other request (the API, its event stream and websockets, `/auth`) to the other, so one build's
 *  page runs against the other build's server. Each beats `processPinCookie` for its kind. */
export const pagePinCookie = "dispatch-e2e-page";
export const apiPinCookie = "dispatch-e2e-api";

/** The envoy-dispatch Dispatch B runs in place of the one e2e/run-server.sh builds from this tree,
 *  when `DISPATCH_E2E_BINARY_B` names one (a path from packages/envoy): a previous release's build
 *  (packages/envoy/scripts/build-dispatch-build.sh), so a run drives two builds of the server. Such
 *  a build must be legion-envoy-v20.1.0 or later: it serves on the socket the harness holds for it
 *  and signs cookies with the run's shared key, as a build from this tree does. */
export const binaryB = process.env.DISPATCH_E2E_BINARY_B || undefined;

/** Headers that send a fetch to `target`. */
export function pinnedTo(target: DispatchProcess): Record<string, string> {
  return { [processPinHeader]: target };
}

function resolveProcessCount(): 1 | 2 {
  const requested = process.env.DISPATCH_E2E_PROCESSES;
  if (requested === undefined || requested === "" || requested === "1") return 1;
  if (requested === "2") return 2;
  throw new Error(
    `DISPATCH_E2E_PROCESSES must be "1", "2" or unset, not ${JSON.stringify(requested)}. ` +
      "Leave it unset (or 1) to run one Dispatch server, or set it to 2 to run two behind the " +
      "round-robin proxy, as `bun run e2e:multiprocess` does."
  );
}

export const harnessProcessCount = resolveProcessCount();
