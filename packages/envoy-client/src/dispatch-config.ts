import {
  checkoutCommandRefusal,
  type EnvoyConfig,
  type EnvoyConfigEnvironment,
  fileValue,
  httpUrl,
  missingConfigNotice,
  nonEmpty,
  offWithError,
  offWithNotice,
  type ReadEnvoyConfig,
  type SectionOff,
  unsendableRefusal,
} from "./envoy-config";
import { messageFor } from "./errors";
import { readSecretFile } from "./secret-file";

/** Default dispatch server base URL, matching the Go server's listen address. */
const DEFAULT_SERVER_URL = "http://localhost:8766";

function normalizeDispatchUrl(url: string): string {
  return url.replace(/\/+$/, "");
}

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

/** The bearer a session sends, and, when a file supplies it, that file and the server it is
 * written for: the only server it may go to. */
type TokenSource =
  | { readonly kind: "variable"; readonly token: string }
  | {
      readonly kind: "file";
      readonly token: string;
      readonly path: string;
      readonly serverUrl: string;
      readonly checkout: boolean;
    }
  | { readonly kind: "none" };

/** Where the token comes from: `DISPATCH_TOKEN_FILE`, then `DISPATCH_TOKEN`, then the checkout's
 * `dispatch.token` (only beside its own `dispatch.serverUrl`), then the chosen file's. */
function tokenSource(
  env: DispatchEnvironment,
  config: ReadEnvoyConfig,
  checkout: {
    readonly path: string;
    readonly serverUrl?: string | undefined;
    readonly token?: string | undefined;
  } | null
): TokenSource | { readonly error: string } {
  const pointer = nonEmpty(env.DISPATCH_TOKEN_FILE);
  if (pointer !== undefined) {
    try {
      return { kind: "variable", token: readSecretFile("DISPATCH_TOKEN_FILE", pointer) };
    } catch (error) {
      return { error: messageFor(error) };
    }
  }
  const plain = nonEmpty(env.DISPATCH_TOKEN);
  if (plain !== undefined) {
    const refusal = unsendableRefusal("DISPATCH_TOKEN", plain);
    return refusal === null ? { kind: "variable", token: plain } : { error: refusal };
  }
  if (checkout?.token !== undefined) {
    if (checkout.serverUrl === undefined) {
      return {
        error: `${checkout.path}: dispatch.token requires dispatch.serverUrl in the same file`,
      };
    }
    const { path, serverUrl, token } = checkout;
    return { kind: "file", token, path, serverUrl, checkout: true };
  }
  const own = config.dispatch;
  if (own.token === undefined) return { kind: "none" };
  return {
    kind: "file",
    token: own.token,
    path: config.path,
    serverUrl: own.serverUrl ?? DEFAULT_SERVER_URL,
    checkout: false,
  };
}

/**
 * Dispatch's URL and bearer token from `config` (`readEnvoyConfig`) and the environment. An empty
 * variable counts as unset.
 *
 * The chosen file's `dispatch.enabled: false` turns Dispatch off whatever the variables say; a
 * checkout's does only while `DISPATCH_URL` is unset, so a repository cannot turn off the Dispatch
 * the session's environment configured. A checkout's file that cannot be read or is not valid
 * turns Dispatch off with its error, likewise only while `DISPATCH_URL` is unset. Otherwise
 * `DISPATCH_URL` overrides the file's URL, and the token resolves from `DISPATCH_TOKEN_FILE`
 * (trimmed file contents — how the Legion daemon delivers it to a pane), then `DISPATCH_TOKEN`,
 * then `dispatch.token`, the checkout's ahead of the chosen file's; a set `DISPATCH_TOKEN_FILE` that
 * cannot be read or is blank disables Dispatch with that failure as `error` and never falls back.
 *
 * Same server only: a file's token goes with `DISPATCH_URL` only when it names the origin of the
 * server written in the file the token comes from (the chosen file's `dispatch.serverUrl`, or the
 * default when it names none); any other server needs `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`. A
 * checkout's `dispatch.serverUrl` is used only with the `dispatch.token` written beside it, and its
 * `dispatch.token` only beside its own `dispatch.serverUrl`, so a cloned repository can neither
 * redirect the user's credential nor make the session act as the repository's author on the
 * user's server. The chosen file's `!` token runs here, only once Dispatch is about to use it; a
 * checkout's is refused. Dispatch is on only when both URL and bearer token resolve; off, it says
 * why: a `notice` when nothing names it or a file turns it off, an `error` otherwise.
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
    return offWithNotice(
      config.missing === null
        ? `${config.path} has no enabled dispatch section, and DISPATCH_URL is not set: Dispatch is off`
        : missingConfigNotice(config.path, config.missing, "Dispatch is off")
    );
  }

  // A checkout's server goes only with the token written beside it. One that DISPATCH_URL
  // replaces is not used, so not refused.
  if (explicitUrl === undefined && checkout?.dispatch.serverUrl !== undefined) {
    const token = checkout.dispatch.token;
    if (!token) {
      return offWithError(
        `${checkout.path}: dispatch.serverUrl requires dispatch.token in the same file`
      );
    }
    if (token.startsWith("!")) {
      return offWithError(checkoutCommandRefusal(checkout.path, "dispatch.token"));
    }
    const refusal = unsendableRefusal(`dispatch.token in ${checkout.path}`, token);
    if (refusal !== null) return offWithError(refusal);
    return { enabled: true, url: normalizeDispatchUrl(checkout.dispatch.serverUrl), token };
  }

  if (explicitUrl !== undefined && httpUrl(explicitUrl) === undefined) {
    return offWithError("DISPATCH_URL must be an http or https URL");
  }
  const url = normalizeDispatchUrl(explicitUrl ?? own.serverUrl ?? DEFAULT_SERVER_URL);
  const source = tokenSource(
    env,
    config,
    checkout === null ? null : { path: checkout.path, ...checkout.dispatch }
  );
  if ("error" in source) return offWithError(source.error);
  if (source.kind === "none") {
    return offWithError(
      explicitUrl === undefined
        ? `${config.path}: dispatch is enabled and no token is: set dispatch.token, or DISPATCH_TOKEN or DISPATCH_TOKEN_FILE`
        : `DISPATCH_URL is set and no token is: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or dispatch.token in ${config.path}`
    );
  }
  if (source.kind === "variable") return { enabled: true, url, token: source.token };

  const named = httpUrl(url)?.origin;
  const paired = httpUrl(source.serverUrl)?.origin;
  if (explicitUrl !== undefined && named !== paired) {
    return offWithError(
      `DISPATCH_URL names ${named}, but ${source.path}'s dispatch.token is for ${paired}; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`
    );
  }
  if (source.checkout && source.token.startsWith("!")) {
    return offWithError(checkoutCommandRefusal(source.path, "dispatch.token"));
  }
  if (source.token === "") {
    return offWithError(`dispatch.token in ${source.path} must be a non-empty bearer token`);
  }
  try {
    return {
      enabled: true,
      url,
      token: fileValue(source.token, "dispatch.token", source.path, env),
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
