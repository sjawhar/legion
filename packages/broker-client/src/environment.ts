// packages/broker-client/src/environment.ts
//
// The variables every program holding one of these identities reads, so a deployment writes one
// set of values and the Go client (`packages/envoy/internal/brokerclient/environment.go`) and this
// one are told the same things the same way.

import { newDpopKey } from "./dpop";
import { metadataIdentity } from "./metadata-identity";
import { projectedToken } from "./projected-token";
import type { TokenSource } from "./source";
import { BrokerTokenClient } from "./token-client";

/** Names the secrets broker, as it does for every other form of `agent-secrets`. */
export const BROKER_URL_VAR = "AGENT_SECRETS_URL";
/** Says which identity this machine proves. Unset, this machine has none and nothing trades,
 *  which is every deployment that has not turned the exchange on. */
export const IDENTITY_VAR = "AGENT_SECRETS_MACHINE_IDENTITY";
/** The projected Kubernetes service-account token file, which {@link IDENTITY_KUBERNETES} reads. */
export const MACHINE_TOKEN_FILE_VAR = "AGENT_SECRETS_MACHINE_TOKEN_FILE";
/** The audience of this machine's Google identity token: the metadata server mints it for that
 *  audience. */
export const MACHINE_AUDIENCE_VAR = "AGENT_SECRETS_MACHINE_AUDIENCE";
/** The session a trade writes under for a caller that acts as no session of its own, which a
 *  daemon, a command and a helper are ({@link tradeSession}). */
export const SESSION_VAR = "AGENT_SECRETS_SESSION";

/** A pod's projected service-account token. */
export const IDENTITY_KUBERNETES = "kubernetes";
/** A Compute Engine machine's own Google identity token. */
export const IDENTITY_METADATA = "metadata";
/** A Google identity token minted through a workload identity pool. Only the Go client federates
 *  one, so a TypeScript caller told to is refused here rather than trading something else. */
export const IDENTITY_POOL = "pool";

/** The variables this module reads, by name. */
interface BrokerVariables {
  readonly AGENT_SECRETS_URL?: string | undefined;
  readonly AGENT_SECRETS_MACHINE_IDENTITY?: string | undefined;
  readonly AGENT_SECRETS_MACHINE_TOKEN_FILE?: string | undefined;
  readonly AGENT_SECRETS_MACHINE_AUDIENCE?: string | undefined;
  readonly AGENT_SECRETS_SESSION?: string | undefined;
}

/** An environment this module reads those variables out of, over whatever else it holds: a
 *  process's own, or a caller's record of one. */
export type BrokerEnvironment = BrokerVariables & Readonly<Record<string, string | undefined>>;

/** `name`'s value with its whitespace trimmed, or "" when it is unset or blank. */
function value(env: BrokerEnvironment, name: keyof BrokerVariables): string {
  return (env[name] ?? "").trim();
}

/** A variable a named identity cannot do without. */
function required(env: BrokerEnvironment, name: keyof BrokerVariables, kind: string): string {
  const named = value(env, name);
  if (named === "") throw new Error(`${name} is required by ${IDENTITY_VAR}=${kind}`);
  return named;
}

/**
 * The identity {@link IDENTITY_VAR} names, or undefined when it names none, which is a machine
 * that trades nothing and is no error. A named identity whose own variables are missing throws
 * naming them, since a deployment that asked for one and gave it nothing has a mistake in it.
 */
export function machineIdentity(env: BrokerEnvironment): TokenSource | undefined {
  const kind = value(env, IDENTITY_VAR);
  switch (kind) {
    case "":
      return undefined;
    case IDENTITY_KUBERNETES:
      return projectedToken(required(env, MACHINE_TOKEN_FILE_VAR, kind));
    case IDENTITY_METADATA:
      return metadataIdentity(required(env, MACHINE_AUDIENCE_VAR, kind));
    case IDENTITY_POOL:
      throw new Error(
        `${IDENTITY_VAR}=${IDENTITY_POOL} is traded by the Go client alone (agent-secrets), which federates at the workload identity pool; this process can prove ${IDENTITY_KUBERNETES} or ${IDENTITY_METADATA}`
      );
    default:
      throw new Error(
        `${IDENTITY_VAR}="${kind}" names no identity this machine can prove; it takes ${IDENTITY_KUBERNETES} or ${IDENTITY_METADATA}`
      );
  }
}

/**
 * The session a trade writes under, which the broker records the minted token against: `own`,
 * the session the caller is acting as, where it acts as one, and otherwise `AGENT_SECRETS_SESSION`.
 * The Go client reads the same rule out of the same two places
 * (`packages/envoy/internal/brokerclient/environment.go`, `TradeSession`).
 *
 * The caller's own session comes first because a token names one session and the Envoy listener
 * holds every write that acts as a session to the session its token names
 * (`packages/envoy/cmd/listener/brokerauth.go`, `sessionNotBound`): a token minted under some
 * other name is one its own holder cannot spend, so a deployment's variable winning here would
 * turn a refused trade, which says which name the entry fixes, into a 403 on every subscribe,
 * unsubscribe, role claim and send. `AGENT_SECRETS_SESSION` is therefore the name for a caller
 * that acts as no session of its own — a daemon, a command, the helper — and the only name such
 * a caller has.
 */
export function tradeSession(env: BrokerEnvironment, own = ""): string {
  const acting = own.trim();
  return acting !== "" ? acting : value(env, SESSION_VAR);
}

/**
 * The client this environment describes for a caller acting as the session `own`, or undefined
 * where it names no broker or no identity — a machine that trades nothing, which every
 * deployment that has not turned the exchange on is. The key is this process's own and is made
 * here, once per client, so only its public half ever leaves the process.
 *
 * It declines the launcher credential its identity holds (`launcherCredential`): every caller of
 * this factory wants a Dispatch access token and nothing else, and none of them reads
 * `credentialId`. A key is this process's and this session's, so asking would write a
 * `launcher_credentials` row and an audit row for every `dispatch` call, every Claude Code hook
 * run, every `envoy-send` and every session of a long-lived Oh My Pi or Claude Code process,
 * each of them on the operator's machine-logins list for its hour and removed by nothing. The
 * machine's own launcher credential is `agent-secrets`' and the helper's, which ask for it.
 */
export async function brokerTokenClient(
  env: BrokerEnvironment,
  own = ""
): Promise<BrokerTokenClient | undefined> {
  const brokerUrl = value(env, BROKER_URL_VAR);
  const subject = machineIdentity(env);
  if (brokerUrl === "" || subject === undefined) return undefined;
  return new BrokerTokenClient({
    brokerUrl,
    key: await newDpopKey(),
    subject,
    session: tradeSession(env, own),
    launcherCredential: false,
  });
}
