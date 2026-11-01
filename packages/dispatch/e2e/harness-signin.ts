/** How the harness's Dispatch servers sign a person in.
 *
 * `DISPATCH_E2E_SIGNIN` unset (or empty) is dev sign-in: each server mounts `/auth/_dev/signin`
 * (`DISPATCH_DEV_SIGNIN=1`), where `e2e/users.ts` and `e2e/api.ts` sign `alice` and `bob` in.
 * `oidc` is the production sign-in, through `packages/envoy/cmd/dev-oidc-issuer` as the issuer:
 * the harness starts that issuer (`e2e/run-signin-issuer.sh`), and both servers of two-process
 * mode take it as `DISPATCH_SIGNIN_ISSUER` under one `DISPATCH_SIGNING_KEY` with dev sign-in off,
 * so a browser signs in at `/auth/start` and `/auth/callback` as it does in a deployment. Only
 * `e2e/signin.multiprocess.e2e.ts` runs then, since every other spec signs in through dev sign-in.
 * `oidc` needs `DISPATCH_E2E_PROCESSES=2`: what it proves is that a sign-in started on one process
 * completes on the other. Any other value is refused naming the allowed ones, as
 * `e2e/harness-processes.ts` refuses its own, and `e2e/run-server.sh` refuses it the same way.
 *
 * Like `e2e/harness-processes.ts`, this module reads only the environment and opens nothing, so
 * every process in `e2e/` may import it.
 */

import { harnessProcessCount } from "./harness-processes";

export type HarnessSignIn = "dev" | "oidc";

/** Dispatch's confidential client at the dev OIDC issuer, the sign-in pool group the servers
 *  admit (`DISPATCH_SIGNIN_GROUP`), and the person the issuer signs in with none queued, a member
 *  of that group (`--sign-in-as`, `--sign-in-group`). */
export const oidcSignIn = {
  clientId: "dispatch-e2e",
  group: "dispatch-e2e-people",
  person: "alice@example.com",
} as const;

function resolveSignIn(): HarnessSignIn {
  const requested = process.env.DISPATCH_E2E_SIGNIN;
  if (requested === undefined || requested === "") return "dev";
  if (requested !== "oidc") {
    throw new Error(
      `DISPATCH_E2E_SIGNIN must be "oidc" or unset, not ${JSON.stringify(requested)}. Leave it ` +
        "unset to sign people in through dev sign-in, or set it to oidc, with " +
        "DISPATCH_E2E_PROCESSES=2, to sign in through the dev OIDC issuer."
    );
  }
  if (harnessProcessCount !== 2) {
    throw new Error(
      "DISPATCH_E2E_SIGNIN=oidc needs DISPATCH_E2E_PROCESSES=2: its one spec starts a sign-in on " +
        "Dispatch A and completes it on B. Run `DISPATCH_E2E_SIGNIN=oidc bun run e2e:multiprocess`."
    );
  }
  return "oidc";
}

export const harnessSignIn = resolveSignIn();
