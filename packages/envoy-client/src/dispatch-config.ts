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
};

/** Dispatch on: its URL and bearer token, both resolved. */
export interface ActiveDispatchConfig {
  readonly enabled: true;
  readonly url: string;
  readonly token: string;
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

/** The token a file gives when no variable does: the checkout's `dispatch.token`, only beside its
 * own `dispatch.serverUrl` (refused without it), else the chosen file's. */
function writtenToken(
  config: ReadEnvoyConfig,
  checkout: ValidCheckout | null
): FileBearer | undefined {
  const field = "dispatch.token";
  if (checkout?.dispatch.token !== undefined) {
    const { serverUrl, token } = checkout.dispatch;
    if (serverUrl === undefined) {
      throw new Error(
        `${checkout.path}: dispatch.token requires dispatch.serverUrl in the same file`
      );
    }
    return { token, field, path: checkout.path, server: serverUrl, checkout: true };
  }
  const own = config.dispatch;
  if (own.token === undefined) return undefined;
  const server = own.serverUrl ?? DEFAULT_SERVER_URL;
  return { token: own.token, field, path: config.path, server, checkout: false };
}

/**
 * Dispatch's URL and bearer token from `config` (`readEnvoyConfig`) and the environment. An empty
 * variable counts as unset.
 *
 * The chosen file's `dispatch.enabled: false` turns Dispatch off whatever the variables say; a
 * checkout's does only while `DISPATCH_URL` is unset, so a repository cannot turn off the Dispatch
 * the session's environment configured. A checkout's file that cannot be read or is not valid
 * turns Dispatch off with its error, likewise only while `DISPATCH_URL` is unset.
 *
 * The token's source is chosen first: while `DISPATCH_URL` is unset, a checkout that names its own
 * `dispatch.serverUrl` gives that server and the `dispatch.token` written beside it, and nothing
 * else; otherwise the token resolves from `DISPATCH_TOKEN_FILE` (trimmed file contents, how the
 * Legion daemon delivers it to a pane), then `DISPATCH_TOKEN`, then `dispatch.token`, the
 * checkout's (only beside its own `dispatch.serverUrl`) ahead of the chosen file's. A set
 * `DISPATCH_TOKEN_FILE` that cannot be read or is blank disables Dispatch with that failure as
 * `error` and never falls back. The URL follows: `DISPATCH_URL`, else the server written beside a
 * file's token, else the chosen file's `dispatch.serverUrl`, else the default.
 *
 * Same server only: a file's token goes with `DISPATCH_URL` only when it names the origin of the
 * server written in the file the token comes from (the chosen file's `dispatch.serverUrl`, or the
 * default when it names none); any other server needs `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`.
 * A checkout's `!` token is refused; the chosen file's runs here, only once Dispatch is about to
 * use it. Dispatch is on only when both URL and bearer token resolve; off, it says why: a `notice`
 * when nothing names it or a file turns it off, an `error` otherwise.
 */
export function dispatchSettings(
  env: DispatchEnvironment,
  config: EnvoyConfig
): DispatchConfigResolution {
  if (config.kind === "failed") return offWithError(config.error);
  const explicitUrl = nonEmpty(env.DISPATCH_URL);
  if (config.checkout?.kind === "invalid" && explicitUrl === undefined) {
    return offWithError(config.checkout.error);
  }
  const checkout = config.checkout?.kind === "valid" ? config.checkout : null;
  const own = config.dispatch;
  if (own.enabled === false) {
    return offWithNotice(`${config.path} sets dispatch.enabled to false: Dispatch is off`);
  }
  if (checkout?.dispatch.enabled === false && explicitUrl === undefined) {
    return offWithNotice(`${checkout.path} sets dispatch.enabled to false: Dispatch is off`);
  }
  const enabled = checkout?.dispatch.enabled === true || own.enabled === true;
  if (explicitUrl === undefined && !enabled) {
    const missing = config.missing;
    return offWithNotice(
      missing === null
        ? `${config.path} has no enabled dispatch section, and DISPATCH_URL is not set: Dispatch is off`
        : missingConfigNotice(config.path, missing, "Dispatch is off"),
      missing?.oldUserFileNotice !== undefined
    );
  }
  if (explicitUrl !== undefined && httpUrl(explicitUrl) === undefined) {
    return offWithError("DISPATCH_URL must be an http or https URL");
  }

  try {
    // A checkout's server goes only with the token written beside it. One that DISPATCH_URL
    // replaces is not used, so not refused.
    const checkoutServer = explicitUrl === undefined ? checkout?.dispatch.serverUrl : undefined;
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
    return { enabled: true, url, token };
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
