import { BROKER_URL_VAR, IDENTITY_VAR } from "@legion/broker-client/environment";

import {
  type BearerSource,
  bearerCredential,
  bearerSource,
  type FileBearer,
  fileBearer,
  type RequestCredential,
} from "./bearer";
import {
  type BrokerSessionName,
  brokerCredential,
  brokerRouteConfigured,
} from "./broker-credential";
import {
  type CheckoutEnvoyConfig,
  type CommandMode,
  type EnvoyConfig,
  type EnvoyConfigEnvironment,
  httpUrl,
  missingConfigNotice,
  nonEmpty,
  offWithError,
  offWithNotice,
  type ReadEnvoyConfig,
  type SectionOff,
} from "./envoy-config";
import { messageFor } from "./errors";

/** Default dispatch server base URL, matching the Go server's listen address. */
const DEFAULT_SERVER_URL = "http://localhost:8766";

/** The variables `dispatchSettings` consults, by name, over whatever else the environment holds. */
export type DispatchEnvironment = EnvoyConfigEnvironment & {
  readonly DISPATCH_TOKEN?: string | undefined;
  readonly DISPATCH_TOKEN_FILE?: string | undefined;
  readonly DISPATCH_TOKEN_BROKER?: string | undefined;
  readonly DISPATCH_PUBLIC_URL?: string | undefined;
};

/** Dispatch on: its URL and the credential every request presents, and `publicUrl`, Dispatch's
 * dashboard as people open it (DISPATCH_PUBLIC_URL), set only when the variable is: a Legion pod
 * reaches Dispatch at an in-cluster `url` nobody outside the cluster can open, so every link the
 * `dispatch` command prints is resolved against `publicUrl` when it is set, and against `url`
 * otherwise. */
export interface ActiveDispatchConfig {
  readonly enabled: true;
  readonly url: string;
  /**
   * What every request presents: the bearer a variable or a file gave (`bearerCredential`), or
   * the access token this machine traded its own identity for at the secrets broker, presented
   * under the `DPoP` scheme with a fresh proof of this process's key on each request
   * (`brokerCredential`). One field, so no caller can present half of it.
   */
  readonly credential: RequestCredential;
  readonly publicUrl?: string;
}

/** Dispatch on, or off with the one notice or error that says why (`SectionOff`). */
export type DispatchConfigResolution = ActiveDispatchConfig | SectionOff;

type ValidCheckout = Extract<CheckoutEnvoyConfig, { readonly kind: "valid" }>;

/** A checkout's own server and the token written beside it, which only that server gets. Throws
 * when the checkout names no token. */
function checkoutPair(checkout: ValidCheckout, serverUrl: string): BearerSource {
  const { token } = checkout.dispatch;
  if (token === undefined) {
    throw new Error(
      `${checkout.path}: dispatch.serverUrl requires dispatch.token in the same file`
    );
  }
  return {
    kind: "file",
    token,
    field: "dispatch.token",
    path: checkout.path,
    server: serverUrl,
    checkout: true,
  };
}

/** The token a file gives when no variable does: the chosen file's `dispatch.token`. A checkout's
 * `dispatch.token` here has no `dispatch.serverUrl` beside it (`checkoutPair` takes one that does),
 * and is refused. */
function writtenToken(
  config: ReadEnvoyConfig,
  checkout: ValidCheckout | null
): FileBearer | undefined {
  if (checkout?.dispatch.token !== undefined) {
    throw new Error(
      `${checkout.path}: dispatch.token requires dispatch.serverUrl in the same file`
    );
  }
  const own = config.dispatch;
  if (own.token === undefined) return undefined;
  const server = own.serverUrl ?? DEFAULT_SERVER_URL;
  return {
    token: own.token,
    field: "dispatch.token",
    path: config.path,
    server,
    checkout: false,
  };
}

/**
 * The notice for `DISPATCH_TOKEN_FILE` set while nothing names Dispatch's server, or undefined when
 * it is not set. Legion sets `DISPATCH_TOKEN_FILE` only beside `DISPATCH_URL`, in a tmux pane and in
 * a pod alike, and a pod has no envoy.json; its session keeps one shell, where an `unset
 * DISPATCH_URL` lasts. So this notice names the variable to export again, not the missing file.
 * `DISPATCH_TOKEN` alone says nothing of the kind: it also goes with a server an envoy.json names.
 * What may name the server on this machine is still said after it: the missing file's dangling
 * link, then the unread OpenCode-era user file, last, as `missingConfigNotice` says it
 * (claude-envoy's session hook prints a notice only when it ends with that file's notice).
 */
export function lostDispatchUrlNotice(
  env: DispatchEnvironment,
  config: ReadEnvoyConfig
): string | undefined {
  if (nonEmpty(env.DISPATCH_TOKEN_FILE) === undefined) return undefined;
  const link = config.missing?.link ?? null;
  const old = config.missing?.oldUserFileNotice;
  return [
    "DISPATCH_TOKEN_FILE is set and DISPATCH_URL is not, so Dispatch is off: this shell lost the DISPATCH_URL it started with. Export DISPATCH_URL again, set to the Dispatch server that token is for",
    ...(link === null ? [] : [`${config.path} is a link to ${link}, which does not exist`]),
    ...(old === undefined ? [] : [old]),
  ].join("; ");
}

/**
 * Dispatch's URL and bearer token from `config` (`readEnvoyConfig`) and the environment. An empty
 * variable counts as unset.
 *
 * The `enabled` rule is the Envoy section's (`envoySettings`): the chosen file's
 * `dispatch.enabled: false` turns Dispatch off whatever the variables say; otherwise the section's
 * fields apply, and Dispatch is on when its server resolves: `DISPATCH_URL`, a checkout's own
 * server, the chosen file's `dispatch.serverUrl`, or the default that `dispatch.enabled: true`
 * names. A checkout's file is read only while `DISPATCH_URL` is unset: `DISPATCH_URL` replaces its
 * server, its token and its `enabled` together, so a repository can neither turn off the Dispatch
 * the session's environment configured nor send its own token to the server the environment names.
 * A checkout contributes only its `dispatch.enabled: false`, or its own server with its own token;
 * its `dispatch.enabled: true` alone turns nothing on. A checkout's file that cannot be read or is
 * not valid turns Dispatch off with its error, likewise only while `DISPATCH_URL` is unset.
 *
 * The token's source is chosen first: a checkout that names its own `dispatch.serverUrl` gives that
 * server and the `dispatch.token` written beside it, and nothing else; otherwise the token resolves
 * from `DISPATCH_TOKEN_FILE` (trimmed file contents, how the Legion daemon delivers it to a pane),
 * then `DISPATCH_TOKEN`, then the chosen file's `dispatch.token` (a checkout's token without its own
 * server is refused). A set `DISPATCH_TOKEN_FILE` that cannot be read or is blank disables Dispatch
 * with that failure as `error` and never falls back. The URL follows: `DISPATCH_URL`, else the
 * server written beside a file's token, else the chosen file's `dispatch.serverUrl`, else the
 * default.
 *
 * Same server only: the chosen file's token goes with `DISPATCH_URL` only when it names the origin
 * of the server written beside it (the default when it names none); any other server needs
 * `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`. A checkout's server with its own token points the
 * session at that server as that token's holder, whichever server it is, the user's own included;
 * what a checkout cannot do is take the user's token, or a variable's, anywhere. A checkout's `!`
 * token is refused; the chosen file's runs here, only once Dispatch is about to use it, unless
 * `options.commands` is `kept`: then only an outcome a run already kept in this process answers,
 * and with none Dispatch is off with an error saying the command has not run (`fileValue`).
 * Dispatch is on when its URL resolves and a credential does: one of those bearers, or — when no
 * bearer is configured at all — this machine's own identity traded at the secrets broker
 * (`AGENT_SECRETS_URL` with `AGENT_SECRETS_MACHINE_IDENTITY`, `brokerCredential`), which is a
 * DPoP-bound access token rather than a bearer, traded under `options.session`. The bearers are
 * resolved first and unchanged, so a session that has one keeps behaving exactly as it did. Off,
 * it says why: a `notice` when nothing names it or a file turns it off, an `error` otherwise.
 * Nothing naming it while a token variable is set is said as the `DISPATCH_URL` the shell lost
 * (`lostDispatchUrlNotice`).
 *
 * `DISPATCH_TOKEN_BROKER=1` turns that last route from a fallback into the only one: no bearer is
 * read at all — not `DISPATCH_TOKEN`, not `DISPATCH_TOKEN_FILE`, not the chosen file's
 * `dispatch.token` — and a machine that can trade nothing turns Dispatch off with an error saying
 * so, rather than presenting whatever bearer the box happens to hold. It is a deployment's word
 * about the session it launches, which `legion controller start` sets under `dispatch_token_broker`
 * so that the operator's own envoy.json cannot write Dispatch as the operator. Any other value is
 * a mistake in the deployment, refused rather than read as on or as off.
 */
export function dispatchSettings(
  env: DispatchEnvironment,
  config: EnvoyConfig,
  options: {
    readonly commands?: CommandMode | undefined;
    /** The session a broker trade writes under (`BrokerSessionName`), for a caller that acts as
     * one. Nothing else reads it: a bearer route resolves the same whatever it says. */
    readonly session?: BrokerSessionName | undefined;
  } = {}
): DispatchConfigResolution {
  const target = dispatchTarget(env, config);
  if (!target.enabled) return target;
  const { url, source } = target;
  let credential: RequestCredential;
  try {
    credential =
      source.kind === "broker"
        ? brokerCredential(env, options.session)
        : bearerCredential(
            source.kind === "variable"
              ? source.token
              : fileBearer(source, url, "DISPATCH", env, options.commands)
          );
  } catch (error) {
    return offWithError(messageFor(error));
  }
  // An http(s) URL the URL parser takes whose port, when it names one, is from 1 to 65535. The
  // daemon accepts at load only values taken here, under a narrower rule (its CheckPod and this
  // read hold to packages/contracts/fixtures/dispatch-public-url.json).
  const publicUrl = nonEmpty(env.DISPATCH_PUBLIC_URL);
  const publicParsed = publicUrl === undefined ? undefined : httpUrl(publicUrl);
  if (publicUrl !== undefined && (publicParsed === undefined || publicParsed.port === "0")) {
    return offWithError("DISPATCH_PUBLIC_URL must be an http or https URL");
  }
  return {
    enabled: true,
    url,
    credential,
    ...(publicUrl === undefined ? {} : { publicUrl: publicUrl.replace(/\/+$/, "") }),
  };
}

/**
 * The URL `dispatchSettings` resolves from `config` and the environment, without reading the token:
 * no `!` command runs, and no outcome of one is needed. Undefined when Dispatch is off before its
 * token is read: nothing names it, a file turns it off or cannot be used, `DISPATCH_URL` is no
 * http(s) URL, or no token source is configured. For a caller on a deadline that holds a list read
 * from one Dispatch URL and must know whether it is the configured one while the token's command
 * has not run in its process (`readSessionAnswers`' `dispatchUrl`).
 */
export function dispatchUrl(env: DispatchEnvironment, config: EnvoyConfig): string | undefined {
  const target = dispatchTarget(env, config);
  return target.enabled ? target.url : undefined;
}

/** Where Dispatch's credential comes from: a variable, a file with the server written beside it,
 * or the secrets broker, which this machine's own identity is traded at. */
type TokenSource = Exclude<BearerSource, { readonly kind: "none" }> | { readonly kind: "broker" };

/** `DISPATCH_TOKEN_BROKER`'s value as a question dispatchTarget can ask: true where the deployment
 * has said this session's Dispatch credential is this machine's own identity and nothing else,
 * false where it says nothing, and a refusal for any other value, which is a deployment that meant
 * one of the two and wrote neither. */
function brokerRouteForced(env: DispatchEnvironment): boolean | SectionOff {
  const written = (env.DISPATCH_TOKEN_BROKER ?? "").trim();
  if (written === "") return false;
  if (written === "1") return true;
  return offWithError(
    `DISPATCH_TOKEN_BROKER is "${written}" and takes 1: set it to 1 where this session's Dispatch credential is this machine's own identity traded at the secrets broker, and leave it unset where a bearer is read as usual`
  );
}

/** Why this machine trades nothing at the broker, as the phrase a forced refusal ends with: the
 * three ways `brokerRouteConfigured` answers false. A named identity missing its own variables
 * throws there instead, and that throw says which variable. */
function brokerRouteGap(env: DispatchEnvironment): string {
  if ((env[BROKER_URL_VAR] ?? "").trim() === "") {
    return `${BROKER_URL_VAR} names no secrets broker to trade it at`;
  }
  const identity = (env[IDENTITY_VAR] ?? "").trim();
  if (identity === "") return `${IDENTITY_VAR} names no identity this machine proves`;
  return `${IDENTITY_VAR}=${identity} is traded by the Go client alone (agent-secrets), never by this process`;
}

/** Dispatch as `dispatchSettings` resolves it up to its token: its URL and the token's source, or
 * off saying why. Reads a `DISPATCH_TOKEN_FILE`, never runs a `!` command. */
function dispatchTarget(
  env: DispatchEnvironment,
  config: EnvoyConfig
): SectionOff | { readonly enabled: true; readonly url: string; readonly source: TokenSource } {
  if (config.kind === "failed") return offWithError(config.error);
  const forced = brokerRouteForced(env);
  if (typeof forced !== "boolean") return forced;
  const explicitUrl = nonEmpty(env.DISPATCH_URL);
  const checkoutFile = explicitUrl === undefined ? config.checkout : null;
  if (checkoutFile?.kind === "invalid") return offWithError(checkoutFile.error);
  const checkout = checkoutFile?.kind === "valid" ? checkoutFile : null;
  const own = config.dispatch;
  if (own.enabled === false) {
    return offWithNotice(`${config.path} sets dispatch.enabled to false: Dispatch is off`);
  }
  if (checkout?.dispatch.enabled === false) {
    return offWithNotice(`${checkout.path} sets dispatch.enabled to false: Dispatch is off`);
  }
  // On when its address resolves: DISPATCH_URL, a checkout's own server, the chosen file's server,
  // or the default its enabled: true names. A checkout's enabled: true alone would name the default
  // server and draw a variable's token to it, so it names nothing.
  const checkoutServer = checkout?.dispatch.serverUrl;
  const addressed =
    explicitUrl !== undefined ||
    checkoutServer !== undefined ||
    own.serverUrl !== undefined ||
    own.enabled === true;
  if (!addressed) {
    const lost = lostDispatchUrlNotice(env, config);
    if (lost !== undefined) return offWithNotice(lost);
    return offWithNotice(
      config.missing === null
        ? `${config.path} names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off`
        : missingConfigNotice(config.path, config.missing, "Dispatch is off")
    );
  }
  if (explicitUrl !== undefined && httpUrl(explicitUrl) === undefined) {
    return offWithError("DISPATCH_URL must be an http or https URL");
  }

  // Forced, no bearer is resolved at all: not a variable, not the chosen file's `dispatch.token`,
  // not a checkout's pair. The broker route follows below, and nothing else does.
  let source: BearerSource = { kind: "none" };
  if (!forced) {
    try {
      // A checkout's server goes only with the token written beside it.
      source =
        checkout !== null && checkoutServer !== undefined
          ? checkoutPair(checkout, checkoutServer)
          : bearerSource(env, "DISPATCH", () => writtenToken(config, checkout));
    } catch (error) {
      return offWithError(messageFor(error));
    }
  }
  if (source.kind === "none") {
    // No bearer at all: this machine may still have an identity of its own to trade.
    let traded: boolean;
    try {
      traded = brokerRouteConfigured(env);
    } catch (error) {
      return offWithError(messageFor(error));
    }
    if (traded) {
      const brokered = (explicitUrl ?? own.serverUrl ?? DEFAULT_SERVER_URL).replace(/\/+$/, "");
      return { enabled: true, url: brokered, source: { kind: "broker" } };
    }
    if (forced) {
      return offWithError(
        `DISPATCH_TOKEN_BROKER is set, so this session's Dispatch credential is this machine's own identity traded at the secrets broker, and ${brokerRouteGap(env)}: Dispatch is off rather than written with whatever bearer this box holds`
      );
    }
    return offWithError(
      explicitUrl === undefined
        ? `${config.path}: dispatch is enabled and no token is: set dispatch.token, or DISPATCH_TOKEN or DISPATCH_TOKEN_FILE`
        : `DISPATCH_URL is set and no token is: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or dispatch.token in ${config.path}`
    );
  }
  const url = (
    explicitUrl ?? (source.kind === "file" ? source.server : (own.serverUrl ?? DEFAULT_SERVER_URL))
  ).replace(/\/+$/, "");
  return { enabled: true, url, source };
}

/**
 * `resolution` when it has Dispatch enabled, or null when none is configured. A file, URL, or
 * token that prevents Dispatch throws `dispatch config: <reason>` rather than reading as
 * unconfigured.
 */
export function activeDispatchSettings(
  resolution: DispatchConfigResolution
): ActiveDispatchConfig | null {
  if (resolution.enabled) return resolution;
  if (resolution.error !== null) throw new Error(`dispatch config: ${resolution.error}`);
  return null;
}
