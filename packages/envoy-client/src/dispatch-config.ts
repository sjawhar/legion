import {
  checkoutCommandRefusal,
  type EnvoyConfig,
  type EnvoyHarness,
  envoyConfigPath,
  fileValue,
  readEnvoyConfig,
  sectionSwitch,
} from "./envoy-config";
import { messageFor } from "./errors";
import { readSecretFile } from "./secret-file";

/** Default dispatch server base URL, matching the Go server's listen address. */
const DEFAULT_SERVER_URL = "http://localhost:8766";

function normalizeDispatchUrl(url: string): string {
  return url.replace(/\/+$/, "");
}

function parsedDispatchUrl(
  value: string,
  source: string
): { url: string | null; error: string | null } {
  const url = normalizeDispatchUrl(value);
  try {
    new URL(url);
    return { url, error: null };
  } catch {
    return { url: null, error: `${source} must be a valid URL` };
  }
}

type DispatchEnvironment = {
  readonly DISPATCH_URL?: string;
  readonly DISPATCH_TOKEN?: string;
  readonly DISPATCH_TOKEN_FILE?: string;
  readonly HOME?: string;
} & Record<string, string | undefined>;

export interface DispatchConfigResolution {
  readonly enabled: boolean;
  readonly url: string | null;
  readonly token: string | null;
  /** Set when a file, URL, or token prevents Dispatch from being enabled. */
  readonly error: string | null;
}

export interface DispatchConfigOptions {
  /** Where the caller runs, which picks its envoy.json (`envoyConfigPath`). */
  readonly harness: EnvoyHarness;
  /** The working directory whose `.envoy/envoy.json` merges over that file; `process.cwd()` by
   * default. */
  readonly cwd?: string;
}

/**
 * Dispatch's URL and bearer token from `config` (`readEnvoyConfig`) and the environment.
 *
 * A file that turns Dispatch off (`sectionSwitch`) wins. Otherwise `DISPATCH_URL` overrides the
 * file's URL, and the token resolves from `DISPATCH_TOKEN_FILE` (trimmed file contents — how the
 * Legion daemon delivers it to a pane), then `DISPATCH_TOKEN`, then `dispatch.token`, the
 * checkout's ahead of the chosen file's; a set `DISPATCH_TOKEN_FILE` that cannot be read or is
 * blank disables Dispatch with that failure as `error` and never falls back. A file's token goes
 * with `DISPATCH_URL` only when it names the origin of the server written in the file the token
 * comes from; any other server needs `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`. A checkout's
 * `dispatch.serverUrl` is used only with the `dispatch.token` written beside it, so it cannot
 * redirect a credential from the user's file or the environment. The chosen file's `!` token runs
 * here, only once Dispatch is about to use it; a checkout's is refused. Dispatch tools are
 * available only when both URL and bearer token resolve.
 */
export function dispatchSettings(
  env: DispatchEnvironment,
  config: EnvoyConfig
): DispatchConfigResolution {
  if (config.error !== null) {
    return { enabled: false, url: null, token: null, error: config.error };
  }
  const state = sectionSwitch(config, "dispatch", env);
  if (state.offBy !== null) return { enabled: false, url: null, token: null, error: null };
  const own = config.dispatch;
  const checkout = config.checkout;
  const explicitUrl = env.DISPATCH_URL;

  // Repository configuration is controlled by the repository author. Its server goes only with
  // the token written beside it; a server DISPATCH_URL replaces is not used, so not refused.
  if (explicitUrl === undefined && state.enabled && checkout?.dispatch.serverUrl !== undefined) {
    const source = `dispatch.serverUrl in ${checkout.path}`;
    const url = parsedDispatchUrl(checkout.dispatch.serverUrl, source);
    if (url.error !== null) return { enabled: false, url: null, token: null, error: url.error };
    const token = checkout.dispatch.token;
    if (!token) {
      const error = `${checkout.path}: dispatch.serverUrl requires dispatch.token in the same file`;
      return { enabled: false, url: null, token: null, error };
    }
    if (token.startsWith("!")) {
      const error = checkoutCommandRefusal(checkout.path, "dispatch.token");
      return { enabled: false, url: null, token: null, error };
    }
    return { enabled: true, url: url.url, token, error: null };
  }

  const rawUrl =
    explicitUrl !== undefined
      ? { value: explicitUrl, source: "DISPATCH_URL" }
      : state.enabled
        ? { value: own.serverUrl ?? DEFAULT_SERVER_URL, source: "dispatch.serverUrl" }
        : null;
  const url = rawUrl ? parsedDispatchUrl(rawUrl.value, rawUrl.source) : { url: null, error: null };
  // The token, and when a file supplies it, that file and the server it is written for.
  let token: string | null;
  let tokenSource: string;
  let tokenFile: { readonly path: string; readonly serverUrl: string } | null = null;
  if (env.DISPATCH_TOKEN_FILE !== undefined) {
    tokenSource = "DISPATCH_TOKEN_FILE";
    try {
      token = readSecretFile(tokenSource, env.DISPATCH_TOKEN_FILE);
    } catch (error) {
      return { enabled: false, url: url.url, token: null, error: messageFor(error) };
    }
  } else if (env.DISPATCH_TOKEN !== undefined) {
    tokenSource = "DISPATCH_TOKEN";
    token = env.DISPATCH_TOKEN;
  } else if (checkout?.dispatch.token !== undefined) {
    tokenSource = "dispatch.token";
    token = checkout.dispatch.token;
    const serverUrl = checkout.dispatch.serverUrl ?? own.serverUrl ?? DEFAULT_SERVER_URL;
    tokenFile = { path: checkout.path, serverUrl };
  } else {
    tokenSource = "dispatch.token";
    token = own.token ?? null;
    if (token !== null) {
      tokenFile = { path: config.path, serverUrl: own.serverUrl ?? DEFAULT_SERVER_URL };
    }
  }
  // A file's `!` token is a command that has not run: it is never handed out as a token.
  const shownToken = tokenFile !== null && token?.startsWith("!") ? null : token;
  if (url.error !== null) return { enabled: false, url: null, token: shownToken, error: url.error };
  if (url.url === null) return { enabled: false, url: null, token: shownToken, error: null };
  if (explicitUrl !== undefined && tokenFile !== null) {
    const named = new URL(url.url).origin;
    const paired = new URL(tokenFile.serverUrl).origin;
    if (named !== paired) {
      return {
        enabled: false,
        url: null,
        token: null,
        error: `DISPATCH_URL names ${named}, but ${tokenFile.path}'s dispatch.token is for ${paired}; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
      };
    }
  }
  if (tokenFile !== null && token?.startsWith("!")) {
    if (tokenFile.path !== config.path) {
      const error = checkoutCommandRefusal(tokenFile.path, "dispatch.token");
      return { enabled: false, url: null, token: null, error };
    }
    try {
      token = fileValue(token, "dispatch.token", config.path, env);
    } catch (error) {
      return { enabled: false, url: url.url, token: null, error: messageFor(error) };
    }
  }
  if (!token) {
    return {
      enabled: false,
      url: url.url,
      token,
      error: `${tokenSource} must be a non-empty bearer token`,
    };
  }
  return { enabled: true, url: url.url, token, error: null };
}

/** Dispatch's URL and bearer token for a caller running in `options.harness`: `dispatchSettings`
 * over the envoy.json that harness reads. A file location that cannot be worked out (a relative
 * `XDG_CONFIG_HOME`) is an `error`. */
export function resolveDispatchConfig(
  env: DispatchEnvironment,
  options: DispatchConfigOptions
): DispatchConfigResolution {
  let filePath: string;
  try {
    filePath = envoyConfigPath(options.harness, env);
  } catch (error) {
    return { enabled: false, url: null, token: null, error: messageFor(error) };
  }
  const config = readEnvoyConfig(filePath, env, { cwd: options.cwd ?? process.cwd() });
  return dispatchSettings(env, config);
}

/** A resolution with Dispatch enabled: its URL and bearer token both resolved. */
export type ActiveDispatchConfig = DispatchConfigResolution & {
  readonly url: string;
  readonly token: string;
};

/**
 * `resolution` when it has Dispatch enabled, or null when none is configured. A file, URL, or
 * token that prevents Dispatch throws `dispatch config: <reason>` rather than reading as
 * unconfigured.
 */
export function activeDispatchSettings(
  resolution: DispatchConfigResolution
): ActiveDispatchConfig | null {
  if (resolution.error !== null) throw new Error(`dispatch config: ${resolution.error}`);
  const { url, token } = resolution;
  if (!resolution.enabled || url === null || token === null) return null;
  return { ...resolution, url, token };
}

/** The active Dispatch configuration (`resolveDispatchConfig`, then `activeDispatchSettings`). */
export function activeDispatchConfig(
  env: DispatchEnvironment,
  options: DispatchConfigOptions
): ActiveDispatchConfig | null {
  return activeDispatchSettings(resolveDispatchConfig(env, options));
}
