import { type BearerSource, bearerSource, type FileBearer, fileBearer } from "./bearer";
import {
  type CheckoutEnvoyConfig,
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
  readonly DISPATCH_PUBLIC_URL?: string | undefined;
};

/** Dispatch on: its URL and bearer token, both resolved, and `publicUrl`, Dispatch's dashboard as
 * people open it (DISPATCH_PUBLIC_URL), set only when the variable is: a Legion pod reaches Dispatch
 * at an in-cluster `url` nobody outside the cluster can open, so every link the `dispatch` command
 * prints is resolved against `publicUrl` when it is set, and against `url` otherwise. */
export interface ActiveDispatchConfig {
  readonly enabled: true;
  readonly url: string;
  readonly token: string;
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
 * token is refused; the chosen file's runs here, only once Dispatch is about to use it. Dispatch is
 * on only when both URL and bearer token resolve; off, it says why: a `notice` when nothing names
 * it or a file turns it off, an `error` otherwise.
 */
export function dispatchSettings(
  env: DispatchEnvironment,
  config: EnvoyConfig
): DispatchConfigResolution {
  if (config.kind === "failed") return offWithError(config.error);
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
    return offWithNotice(
      config.missing === null
        ? `${config.path} names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off`
        : missingConfigNotice(config.path, config.missing, "Dispatch is off")
    );
  }
  if (explicitUrl !== undefined && httpUrl(explicitUrl) === undefined) {
    return offWithError("DISPATCH_URL must be an http or https URL");
  }

  try {
    // A checkout's server goes only with the token written beside it.
    const source =
      checkout !== null && checkoutServer !== undefined
        ? checkoutPair(checkout, checkoutServer)
        : bearerSource(env, "DISPATCH", () => writtenToken(config, checkout));
    if (source.kind === "none") {
      return offWithError(
        explicitUrl === undefined
          ? `${config.path}: dispatch is enabled and no token is: set dispatch.token, or DISPATCH_TOKEN or DISPATCH_TOKEN_FILE`
          : `DISPATCH_URL is set and no token is: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or dispatch.token in ${config.path}`
      );
    }
    const url = (
      explicitUrl ??
      (source.kind === "file" ? source.server : (own.serverUrl ?? DEFAULT_SERVER_URL))
    ).replace(/\/+$/, "");
    const token =
      source.kind === "variable" ? source.token : fileBearer(source, url, "DISPATCH", env);
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
      token,
      ...(publicUrl === undefined ? {} : { publicUrl: publicUrl.replace(/\/+$/, "") }),
    };
  } catch (error) {
    return offWithError(messageFor(error));
  }
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
