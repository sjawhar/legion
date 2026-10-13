import {
  dispatchTokenVariableSet,
  type EnvoyConfig,
  type EnvoyHarness,
  envoyConfigPath,
  readEnvoyConfig,
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
 * A `dispatch.enabled: false` turns Dispatch off whatever the variables say. Otherwise
 * `DISPATCH_URL` overrides the file's URL, and the token resolves from `DISPATCH_TOKEN_FILE`
 * (trimmed file contents — how the Legion daemon delivers it to a pane), then `DISPATCH_TOKEN`,
 * then `dispatch.token`; a set `DISPATCH_TOKEN_FILE` that cannot be read or is blank disables
 * Dispatch with that failure as `error` and never falls back. The file's token goes with
 * `DISPATCH_URL` only when both name the same origin; any other server needs `DISPATCH_TOKEN` or
 * `DISPATCH_TOKEN_FILE`. A checkout's `dispatch.serverUrl` is used only with the `dispatch.token`
 * written beside it, so it cannot redirect a credential from the user's file or the environment.
 * Dispatch tools are available only when both URL and bearer token resolve.
 */
export function dispatchSettings(
  env: DispatchEnvironment,
  config: EnvoyConfig
): DispatchConfigResolution {
  if (config.error !== null) {
    return { enabled: false, url: null, token: null, error: config.error };
  }
  const settings = config.dispatch;
  if (settings.enabled === false) return { enabled: false, url: null, token: null, error: null };
  const explicitUrl = env.DISPATCH_URL;
  const checkout = config.checkout;

  // Repository configuration is controlled by the repository author. It may provide its own
  // endpoint and credential, but must never redirect a credential inherited from the user's
  // file or the process environment.
  if (
    explicitUrl === undefined &&
    settings.enabled === true &&
    checkout?.dispatch.serverUrl !== undefined
  ) {
    const token = checkout.dispatch.token ?? null;
    const source = `dispatch.serverUrl in ${checkout.path}`;
    const url = parsedDispatchUrl(checkout.dispatch.serverUrl, source);
    if (url.error !== null) return { enabled: false, url: null, token: null, error: url.error };
    if (!token) {
      return {
        enabled: false,
        url: url.url,
        token: null,
        error: `${checkout.path}: dispatch.serverUrl requires dispatch.token in the same file`,
      };
    }
    return { enabled: true, url: url.url, token, error: null };
  }

  const rawUrl =
    explicitUrl !== undefined
      ? { value: explicitUrl, source: "DISPATCH_URL" }
      : settings.enabled === true
        ? { value: settings.serverUrl ?? DEFAULT_SERVER_URL, source: "dispatch.serverUrl" }
        : null;
  const url = rawUrl ? parsedDispatchUrl(rawUrl.value, rawUrl.source) : { url: null, error: null };
  let token: string | null;
  let tokenSource: string;
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
  } else {
    tokenSource = "dispatch.token";
    token = settings.token ?? null;
  }
  if (url.error !== null) return { enabled: false, url: null, token, error: url.error };
  if (url.url === null) return { enabled: false, url: null, token, error: null };
  if (explicitUrl !== undefined && token !== null && !dispatchTokenVariableSet(env)) {
    const named = new URL(url.url).origin;
    const fileServer = new URL(settings.serverUrl ?? DEFAULT_SERVER_URL).origin;
    if (named !== fileServer) {
      const tokenFile = checkout?.dispatch.token !== undefined ? checkout.path : config.path;
      return {
        enabled: false,
        url: null,
        token: null,
        error: `DISPATCH_URL names ${named}, but ${tokenFile}'s dispatch.token is for ${fileServer}; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
      };
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
