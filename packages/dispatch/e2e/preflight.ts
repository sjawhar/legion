import { resetFakeEnvoy, setLiveSessions } from "./agents";
import { baseUrl, baseUrlOf, devSignInPath, e2eAgentToken, sessionCookieFrom } from "./api";
import { asLane, harnessPorts, laneCount } from "./harness-ports";
import {
  type DispatchProcess,
  dispatchProcesses,
  harnessProcessCount,
  pinnedTo,
} from "./harness-processes";
import { harnessSignIn } from "./harness-signin";

/** Playwright's global setup, which runs before any row, once e2e/run.ts has started the harness
 *  (e2e/playwright.config.ts). Every fixture a row seeds goes to its lane's fake Envoy, while the
 *  server under test reads whatever its own `ENVOY_URL` names: run-server.sh builds it from
 *  `FAKE_ENVOY_PORT`, and the acceptance Compose file from the `FAKE_ENVOY_PORT` its own `up` was
 *  given. A server reading any other listener would answer rows from sessions none of them seeded,
 *  so a session put in a lane's fake must come back from that lane's server before anything runs.
 *
 *  Every lane is checked, not just the one this process resolves: a run has one lane per worker
 *  (e2e/run.ts), each with servers of its own, and a row that lands in lane 2 is no better off
 *  for lane 0 having answered. This process resolves each lane's ports and URL the way a worker
 *  of that lane does (`asLane`) and hands them to the helpers it shares with the rows, since
 *  those read their own lane otherwise: e2e/api.ts's `baseUrl` is the lane of the process that
 *  imported it, and e2e/agents.ts's fixture helpers address their own lane's fake Envoy port.
 *
 *  In two-process mode (e2e/harness-processes.ts) both processes must answer, each asked pinned
 *  rather than whichever the round-robin proxy picks, and a person signed in on A must be signed
 *  in on B: without one cookie key (run-server.sh's DISPATCH_DEV_SIGNIN_SHARED_KEY) every page
 *  would sign its person out at the first request B answers. Under DISPATCH_E2E_SIGNIN=oidc
 *  (e2e/harness-signin.ts) the servers have no dev sign-in to check that with; there the one spec
 *  it runs signs a person in through the issuer and asks both processes who they are. That mode
 *  runs one lane: a run of more than one refuses `DISPATCH_E2E_PROCESSES=2`
 *  (e2e/harness-ports.ts). */
export default async function preflight(): Promise<void> {
  const lanes = laneCount();
  for (let lane = 0; lane < lanes; lane++) await checkLane(lane, lanes);
  if (harnessProcessCount === 2 && harnessSignIn === "dev") await requireOneCookieKey();
}

/** The lane's Agents rows, as `listAgents` reads its own lane's (e2e/api.ts). */
async function agentsOf(url: string, target: DispatchProcess | undefined): Promise<string[]> {
  const response = await fetch(new URL("/api/v1/agents", url), {
    headers: {
      Authorization: `Bearer ${e2eAgentToken}`,
      "Content-Type": "application/json",
      ...(target === undefined ? {} : pinnedTo(target)),
    },
  });
  if (!response.ok) {
    throw new Error(`GET /api/v1/agents failed: ${response.status} ${await response.text()}`);
  }
  const agents = (await response.json()) as { session_id: string }[];
  return agents.map((agent) => agent.session_id);
}

/** Puts a session in the lane's fake Envoy, asks the lane's server for it, and empties the fake
 *  again, whatever the answer was. */
async function checkLane(lane: number, lanes: number): Promise<void> {
  const ports = harnessPorts(asLane(lane));
  const url = baseUrlOf(asLane(lane));
  const named = lanes === 1 ? "" : ` (lane ${lane})`;
  const sentinel = { session_id: `e2e-preflight-${process.pid}-${lane}`, title: "e2e preflight" };
  const fake = `the fake Envoy this run started on 127.0.0.1:${ports.fakeEnvoy.port} (${ports.fakeEnvoy.variable}${named})`;
  const targets: readonly (DispatchProcess | undefined)[] =
    harnessProcessCount === 2 ? dispatchProcesses : [undefined];
  await setLiveSessions([sentinel], ports.fakeEnvoy.port);
  try {
    for (const target of targets) {
      const pinned = target === undefined ? "" : ` pinned to Dispatch ${target.toUpperCase()}`;
      const refusal = `The Dispatch e2e harness cannot start: ${url}${named}${pinned}`;
      const agents = await agentsOf(url, target).catch((error: unknown) => {
        throw new Error(
          `${refusal} did not list its agents, so this run cannot tell whether it reads ${fake}: ` +
            (error instanceof Error ? error.message : String(error))
        );
      });
      if (!agents.includes(sentinel.session_id)) {
        throw new Error(
          `${refusal} does not list the session this run put in ${fake}, so it reads another ` +
            "Envoy listener. Give the server and this run one FAKE_ENVOY_PORT; the acceptance " +
            "Compose file derives the server's ENVOY_URL from the one its `up` is given " +
            "(packages/dispatch/README.md)."
        );
      }
    }
  } finally {
    await resetFakeEnvoy(ports.fakeEnvoy.port);
  }
}

/** Signs a person in on A and asks B who they are. The person row it writes is emptied by the
 *  first row's `resetDatabase`, as every row's are. */
async function requireOneCookieKey(): Promise<void> {
  const refusal = "The Dispatch e2e harness cannot start: Dispatch B";
  const signIn = await fetch(new URL(devSignInPath("alice"), baseUrl), {
    headers: pinnedTo("a"),
    redirect: "manual",
  });
  const cookie = sessionCookieFrom(signIn);
  if (signIn.status !== 302 || cookie === undefined) {
    throw new Error(
      `The Dispatch e2e harness cannot start: dev sign-in on Dispatch A answered ` +
        `${signIn.status} ${await signIn.text()}`
    );
  }
  const whoami = await fetch(new URL("/auth/whoami", baseUrl), {
    headers: { ...pinnedTo("b"), Cookie: cookie },
  });
  if (whoami.status !== 200) {
    throw new Error(
      `${refusal} answered ${whoami.status} ${await whoami.text()} to a session Dispatch A ` +
        "minted, so a person signed in on one process is signed out on the other. Both must sign " +
        "cookies with the key derived from the one DISPATCH_DEV_SIGNIN_SHARED_KEY run-server.sh " +
        "hands them from the harness's DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY (e2e/harness-listeners.ts)."
    );
  }
}
