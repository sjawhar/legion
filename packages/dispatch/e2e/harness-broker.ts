/** The secrets broker the harness server relays credential requests to, from one switch,
 * `DISPATCH_E2E_AGENT_SECRETS_URL`, which `e2e/run-server.sh` reads the same way
 * (`${VAR-default}`, no colon, so an empty value is not an unset one):
 *
 * - unset: `e2e/fake-broker.ts` on `FAKE_BROKER_PORT`, which the harness starts for a local run and
 *   `resetDatabase()` clears, and where a row seeds the requests it needs;
 * - empty: no broker, the credential feature off (the pending list answers `null`, every other
 *   credential route `404 FEATURE_OFF`), as in a deployment that configures none;
 * - a URL: a broker the caller runs, with its UI bearer in `DISPATCH_E2E_AGENT_SECRETS_TOKEN_FILE`;
 *   the harness neither starts nor resets it, and a row that seeds the fake is skipped.
 *
 * A deployed run (`PLAYWRIGHT_BASE_URL`) starts no server and so no fake broker, whatever the
 * switch says. This module only reads the environment, so any process may import it.
 */
export const usesFakeBroker =
  process.env.DISPATCH_E2E_AGENT_SECRETS_URL === undefined && !process.env.PLAYWRIGHT_BASE_URL;
