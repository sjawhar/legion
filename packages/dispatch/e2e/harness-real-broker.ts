/** Whether this run starts the real secrets broker and its dev OIDC issuer (`e2e/run-broker.sh`),
 * the pair `e2e/broker-approval.e2e.ts` drives an approval on. They are the only rows that cost a
 * second `go build` and a database of their own, so a run that cannot reach that spec starts
 * neither of them:
 *
 * - the spec seeds the row the Inbox lists on the fake broker, and only that row's `approve_url`
 *   points at a record on the real broker, so a run whose switch names another broker or none, and
 *   a deployed run, start neither (`e2e/harness-broker.ts`);
 * - the spec is not a `*.multiprocess.e2e.ts`, so only a one-process run selects it, and
 *   two-process mode starts neither (`e2e/harness-processes.ts`). `DISPATCH_E2E_SIGNIN=oidc` is
 *   two-process mode, so its own dev OIDC issuer and this one never run together.
 *
 * `bun run dispatch:local` (`e2e/local-instance.ts`) runs one Dispatch process and leaves the
 * broker switch as the shell set it, so it starts both rows whenever it starts the fake broker, as
 * it starts every other row the table offers, and prints their ports with the rest.
 *
 * Like `e2e/harness-broker.ts`, this module only reads the environment, so any process may import
 * it.
 */

import { usesFakeBroker } from "./harness-broker";
import { harnessProcessCount } from "./harness-processes";

export const usesRealBroker = usesFakeBroker && harnessProcessCount === 1;
