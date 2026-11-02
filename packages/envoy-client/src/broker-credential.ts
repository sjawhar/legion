// packages/envoy-client/src/broker-credential.ts
//
// The credential a machine makes out of the identity its platform already gives it: the secrets
// broker's RFC 8693 exchange turns a pod's projected service-account token, or a Compute Engine
// machine's Google identity token, into an access token bound to a key this process made, and
// Dispatch and the Envoy listener read that token back at the broker's introspection route.
//
// It is the last source a section consults. Every bearer a deployment writes today —
// `DISPATCH_TOKEN`, `DISPATCH_TOKEN_FILE`, `ENVOY_TOKEN`, `ENVOY_TOKEN_FILE`, an envoy.json's own
// token — is resolved first and unchanged, so a session that has a token by any of those routes
// behaves exactly as it did before this module existed.

import {
  type BrokerEnvironment,
  brokerTokenClient,
  machineIdentity,
} from "@legion/broker-client/environment";
import type { BrokerTokenClient } from "@legion/broker-client/token-client";
import type { RequestCredential } from "./bearer";

/**
 * Whether this machine can trade for one of the broker's access tokens: a broker to trade at and
 * an identity to trade. An identity named with its own variables missing throws here, naming
 * them, rather than reading as a machine that has none.
 */
export function brokerRouteConfigured(env: BrokerEnvironment): boolean {
  return (env.AGENT_SECRETS_URL ?? "").trim() !== "" && machineIdentity(env) !== undefined;
}

/**
 * The clients this process has made, by the variables that describe them. One process makes one
 * key per broker and identity and keeps one token live under it, which both Dispatch and the
 * listener present: two keys would be two credentials for one session, and the broker records the
 * key the token is bound to.
 */
const CLIENTS = new Map<string, Promise<BrokerTokenClient | undefined>>();

/** Drops what this process made, so a test drives a broker of its own from nothing. */
export function forgetBrokerClients(): void {
  CLIENTS.clear();
}

/** The client `env` describes, made once per process. */
function clientFor(env: BrokerEnvironment): Promise<BrokerTokenClient | undefined> {
  const key = JSON.stringify([
    env.AGENT_SECRETS_URL,
    env.AGENT_SECRETS_MACHINE_IDENTITY,
    env.AGENT_SECRETS_MACHINE_TOKEN_FILE,
    env.AGENT_SECRETS_MACHINE_AUDIENCE,
    env.AGENT_SECRETS_SESSION,
  ]);
  const made = CLIENTS.get(key) ?? brokerTokenClient(env);
  CLIENTS.set(key, made);
  return made;
}

/**
 * This machine's broker credential: `Authorization: DPoP <access token>` and a proof of this
 * process's key naming the request's method, its target URI and the token it is presented beside
 * (`ath`), so a copy of the token taken off the wire buys nothing.
 *
 * The trade happens at the first request that needs it and at every renewal after, both inside
 * `headers`, so nothing of the caller's restarts when a token lapses. Dispatch and the listener
 * mint no DPoP nonce (`packages/envoy/internal/brokertoken/brokertoken.go`, `withoutNonces`), so
 * a request to either carries the proof it was signed with and no nonce round trip; the broker's
 * own token endpoint, which may ask for one, is where that retry lives
 * (`@legion/broker-client/token-client`).
 */
export function brokerCredential(env: BrokerEnvironment): RequestCredential {
  let held = "";
  return {
    scheme: "DPoP",
    async headers(method: string, url: string): Promise<Readonly<Record<string, string>>> {
      const client = await clientFor(env);
      if (client === undefined) {
        throw new Error(
          "AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_IDENTITY no longer name a broker and an identity to trade"
        );
      }
      const token = await client.token();
      held = token.accessToken;
      return {
        Authorization: `DPoP ${token.accessToken}`,
        DPoP: await client.key.proof({ method, url, accessToken: token.accessToken }),
      };
    },
    secret: () => held,
  };
}
