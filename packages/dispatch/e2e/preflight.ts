import { resetFakeEnvoy, setLiveSessions } from "./agents";
import { baseUrl, devSignInPath, listAgents, sessionCookieName } from "./api";
import { harnessPorts } from "./harness-ports";
import {
  type DispatchProcess,
  dispatchProcesses,
  harnessProcessCount,
  pinnedTo,
} from "./harness-processes";

/** Playwright's global setup, which runs once the web servers are up and before any row
 *  (e2e/playwright.config.ts). Every fixture a row seeds goes to this run's fake Envoy, while the
 *  server under test reads whatever its own `ENVOY_URL` names: run-server.sh builds it from
 *  `FAKE_ENVOY_PORT`, and the acceptance Compose file from the `FAKE_ENVOY_PORT` its own `up` was
 *  given. A server reading any other listener would answer rows from sessions none of them seeded,
 *  so a session put in this run's fake must come back from the server before anything runs.
 *
 *  In two-process mode (e2e/harness-processes.ts) both processes must, each asked pinned rather
 *  than whichever the round-robin proxy picks, and a person signed in on A must be signed in on B:
 *  without one cookie key (run-server.sh's DISPATCH_DEV_SIGNIN_SHARED_KEY) every page would sign
 *  its person out at the first request B answers. */
export default async function preflight(): Promise<void> {
  const sentinel = { session_id: `e2e-preflight-${process.pid}`, title: "e2e preflight" };
  const { port, variable } = harnessPorts.fakeEnvoy;
  const fake = `the fake Envoy this run started on 127.0.0.1:${port} (${variable})`;
  const targets: readonly (DispatchProcess | undefined)[] =
    harnessProcessCount === 2 ? dispatchProcesses : [undefined];
  await setLiveSessions([sentinel]);
  try {
    for (const target of targets) {
      const pinned = target === undefined ? "" : ` pinned to Dispatch ${target.toUpperCase()}`;
      const refusal = `The Dispatch e2e harness cannot start: ${baseUrl}${pinned}`;
      const agents = await listAgents({ as: "agent", process: target }).catch((error: unknown) => {
        throw new Error(
          `${refusal} did not list its agents, so this run cannot tell whether it reads ${fake}: ` +
            (error instanceof Error ? error.message : String(error))
        );
      });
      if (!agents.some((agent) => agent.session_id === sentinel.session_id)) {
        throw new Error(
          `${refusal} does not list the session this run put in ${fake}, so it reads another ` +
            "Envoy listener. Give the server and this run one FAKE_ENVOY_PORT; the acceptance " +
            "Compose file derives the server's ENVOY_URL from the one its `up` is given " +
            "(packages/dispatch/README.md)."
        );
      }
    }
  } finally {
    await resetFakeEnvoy();
  }
  if (harnessProcessCount === 2) await requireOneCookieKey();
}

/** Signs a person in on A and asks B who they are. The person row it writes is emptied by the
 *  first row's `resetDatabase`, as every row's are. */
async function requireOneCookieKey(): Promise<void> {
  const refusal = "The Dispatch e2e harness cannot start: Dispatch B";
  const signIn = await fetch(new URL(devSignInPath("alice"), baseUrl), {
    headers: pinnedTo("a"),
    redirect: "manual",
  });
  const cookie = signIn.headers
    .getSetCookie()
    .find((header) => header.startsWith(`${sessionCookieName}=`))
    ?.split(";")[0];
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
