import { resetFakeEnvoy, setLiveSessions } from "./agents";
import { baseUrl, listAgents } from "./api";
import { fakeEnvoyPort } from "./harness-ports";

/** Playwright's global setup, which runs once the web servers are up and before any row
 *  (e2e/playwright.config.ts). Every fixture a row seeds goes to this run's fake Envoy, while the
 *  server under test reads whatever its own `ENVOY_URL` names: run-server.sh builds it from
 *  `FAKE_ENVOY_PORT`, and the acceptance Compose file from the `FAKE_ENVOY_PORT` its own `up` was
 *  given. A server reading any other listener would answer rows from sessions none of them seeded,
 *  so a session put in this run's fake must come back from the server before anything runs. */
export default async function preflight(): Promise<void> {
  const sentinel = { session_id: `e2e-preflight-${process.pid}`, title: "e2e preflight" };
  const refusal = `The Dispatch e2e harness cannot start: ${baseUrl}`;
  const fake = `the fake Envoy this run started on 127.0.0.1:${fakeEnvoyPort} (FAKE_ENVOY_PORT)`;
  await setLiveSessions([sentinel]);
  try {
    const agents = await listAgents({ as: "agent" }).catch((error: unknown) => {
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
  } finally {
    await resetFakeEnvoy();
  }
}
