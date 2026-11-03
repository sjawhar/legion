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
  BROKER_URL_VAR,
  type BrokerEnvironment,
  brokerTokenClient,
  IDENTITY_POOL,
  IDENTITY_VAR,
  machineIdentity,
  tradeSession,
} from "@legion/broker-client/environment";
import type { BrokerTokenClient } from "@legion/broker-client/token-client";
import type { RequestCredential } from "./bearer";

/**
 * The session a trade writes under, which the broker records the minted token against and the
 * listener checks a subscribe, an unsubscribe, a role claim and a send's own `source_session`
 * against. A caller that holds one name passes it; a caller whose session changes over the life
 * of its process — Claude Code mints a new id on `/clear`, and one Oh My Pi process switches
 * between sessions — passes a function, which is read at every trade rather than fixed when the
 * configuration resolved. A caller that acts as no session at all passes nothing, and the trade
 * names whatever `AGENT_SECRETS_SESSION` does, which is how a daemon names its own
 * (`tradeSession`, `@legion/broker-client/environment`, the one rule both clients read).
 */
export type BrokerSessionName = string | (() => string);

/** `naming` as one name, trimmed; "" when it names none. */
function sessionName(naming: BrokerSessionName | undefined): string {
  if (naming === undefined) return "";
  return (typeof naming === "string" ? naming : naming()).trim();
}

/**
 * Whether this machine can trade for one of the broker's access tokens — a broker to trade at
 * and an identity this client can prove — and, where it cannot, the phrase that says which of
 * the three is missing, as a refusal ends with. One pass answers both, so no caller decides the
 * question a second time to say why. An identity named with its own variables missing throws
 * here, naming them, rather than reading as a machine that has none.
 *
 * `AGENT_SECRETS_MACHINE_IDENTITY=pool` is a machine with no TypeScript route rather than a
 * mistake: only the Go client federates at a workload identity pool (`agent-secrets`), and the
 * Go side of such a machine works. Reading it as an error here would turn Dispatch and Envoy off
 * on a machine whose listener asks for no credential at all.
 */
export type BrokerRoute =
  | { readonly traded: true }
  | { readonly traded: false; readonly gap: string };

export function brokerRouteConfigured(env: BrokerEnvironment): BrokerRoute {
  if ((env[BROKER_URL_VAR] ?? "").trim() === "") {
    return { traded: false, gap: `${BROKER_URL_VAR} names no secrets broker to trade it at` };
  }
  if ((env[IDENTITY_VAR] ?? "").trim() === IDENTITY_POOL) {
    return {
      traded: false,
      gap: `${IDENTITY_VAR}=${IDENTITY_POOL} is traded by the Go client alone (agent-secrets), never by this process`,
    };
  }
  if (machineIdentity(env) === undefined) {
    return { traded: false, gap: `${IDENTITY_VAR} names no identity this machine proves` };
  }
  return { traded: true };
}

/**
 * The clients this process has made, by the session they trade under and, under it, by the
 * variables that describe them. One process makes one key per broker, identity and session, and
 * keeps one token live under it, which both Dispatch and the listener of that session present:
 * two keys would be two credentials for one session, and the broker records the key the token is
 * bound to. The session is part of the identity, not of the process: a token names one session,
 * and the listener answers 403 to a session presenting another's, so two sessions of one process
 * hold two clients. An entry is one key pair and one token, made only for a session this process
 * actually traded for, and dropped when the host leaves that session
 * (`forgetBrokerSession`).
 */
const CLIENTS = new Map<string, Map<string, Promise<BrokerTokenClient | undefined>>>();

/** Drops what this process made, so a test drives a broker of its own from nothing. */
export function forgetBrokerClients(): void {
  CLIENTS.clear();
}

/**
 * Drops what this process traded for `session`: its key pair, and the token renewing under it.
 *
 * For the host that has left that session, once it has made its last call for it — Oh My Pi's
 * pass once the role of the session it left is handed back, Claude Code's handoff once the old
 * id is deregistered. One long-lived process binds many session ids over its life (`/clear`, a
 * session switch), and without this each one it ever traded under keeps a key pair and a token
 * that trades again at every renewal, for the life of the process. A session that comes back
 * trades afresh, as one this process never traded for does.
 */
export function forgetBrokerSession(session: string): void {
  CLIENTS.delete(session.trim());
}

/**
 * The client `env` describes for a caller acting as the session `own`, made once per process.
 * The entry is held under the name the trade writes under rather than under `own`, since that is
 * the name the token carries: a caller that acts as no session and one that acts as the session
 * `AGENT_SECRETS_SESSION` names are one client, and two environments naming two sessions are two.
 */
function clientFor(env: BrokerEnvironment, own: string): Promise<BrokerTokenClient | undefined> {
  const session = tradeSession(env, own);
  const key = JSON.stringify([
    env.AGENT_SECRETS_URL,
    env.AGENT_SECRETS_MACHINE_IDENTITY,
    env.AGENT_SECRETS_MACHINE_TOKEN_FILE,
    env.AGENT_SECRETS_MACHINE_AUDIENCE,
  ]);
  const held = CLIENTS.get(session) ?? new Map<string, Promise<BrokerTokenClient | undefined>>();
  CLIENTS.set(session, held);
  const made = held.get(key) ?? brokerTokenClient(env, session);
  held.set(key, made);
  return made;
}

/**
 * This machine's broker credential for `session`: `Authorization: DPoP <access token>` and a
 * proof of this process's key naming the request's method, its target URI and the token it is
 * presented beside (`ath`), so a copy of the token taken off the wire buys nothing.
 *
 * The token names `session` (`BrokerSessionName`), which is what a fleet entry that fixes no
 * session requires and what the listener checks each call against; a caller that names none
 * trades under `AGENT_SECRETS_SESSION` instead. The name is read at every trade, so a process
 * that moves from one session to another trades afresh under the new name rather than
 * presenting the old one.
 *
 * A call that names a session of its own (`headers`' third argument) is answered that session's
 * credential rather than this one: the listener holds a write that acts as a session to the
 * session its token names, and a process that has moved on still has calls to make for the
 * session it left — the hand-back of a role that session holds, its deregistration — which under
 * the credential of the session running now are answered 403. Each such session is one client of
 * its own and one trade of its own (`CLIENTS`), so the session this process left answers out of
 * the token it already holds.
 *
 * The trade happens at the first request that needs it and at every renewal after, both inside
 * `headers`, so nothing of the caller's restarts when a token lapses. Dispatch and the listener
 * mint no DPoP nonce (`packages/envoy/internal/brokertoken/brokertoken.go`, `withoutNonces`), so
 * a request to either carries the proof it was signed with and no nonce round trip; the broker's
 * own token endpoint, which may ask for one, is where that retry lives
 * (`@legion/broker-client/token-client`).
 */
export function brokerCredential(
  env: BrokerEnvironment,
  session?: BrokerSessionName | undefined
): RequestCredential {
  let held = "";
  return {
    scheme: "DPoP",
    async headers(
      method: string,
      url: string,
      acting?: string
    ): Promise<Readonly<Record<string, string>>> {
      const client = await clientFor(env, (acting ?? "").trim() || sessionName(session));
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
