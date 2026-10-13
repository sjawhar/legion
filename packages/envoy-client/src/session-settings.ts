import { type EnvoyEnvironment, type EnvoySettings, envoySettings } from "./defaults";
import {
  type ActiveDispatchConfig,
  activeDispatchSettings,
  type DispatchConfigResolution,
  type DispatchEnvironment,
  dispatchSettings,
} from "./dispatch-config";
import {
  type EnvoyConfig,
  type EnvoyHarness,
  envoyConfigPath,
  missingConfigNotice,
  offWithError,
  readEnvoyConfig,
} from "./envoy-config";
import { messageFor } from "./errors";

/** The envoy.json a plugin's session reads (`envoyConfigPath`), or why its location cannot be
 * worked out (a relative `XDG_CONFIG_HOME`, `ENVOY_CONFIG_FILE` or harness directory). */
export type SessionConfigFile =
  | { readonly path: string; readonly error: null }
  | { readonly path: null; readonly error: string };

/** The file `harness`'s rule chooses, with the rule's refusal as `error` rather than a throw, so a
 * plugin that cannot work out its file still loads with both sections off. */
export function sessionConfigFile(harness: EnvoyHarness, env: EnvoyEnvironment): SessionConfigFile {
  try {
    return { path: envoyConfigPath(harness, env), error: null };
  } catch (error) {
    return { path: null, error: messageFor(error) };
  }
}

/** Dispatch and Envoy as a session starts with them, and what the session says about it once. */
export interface SessionSettings {
  readonly dispatch: DispatchConfigResolution;
  readonly envoy: EnvoySettings;
  /** Why a section is off, or null when every section that is off was turned off by a file. */
  readonly notice: string | null;
}

/**
 * The start notice. A chosen file that cannot be read turns both sections off with its one error.
 * A missing file turns both off with its own notice (`missingConfigNotice`, which names a dangling
 * link's target and the old OpenCode file while it is still there); when a variable turned a
 * section on, each section still off is said with the same missing-file text. A section's own
 * error (a token that does not resolve, a command that failed) is said for that section. A file
 * that turns a section off, or holds no section for it, says nothing. What a checkout's file holds
 * that the session does not read is said too: an `envoy` section, and a file that cannot be used
 * while `DISPATCH_URL` replaces it.
 */
function startNotice(
  config: EnvoyConfig,
  dispatch: DispatchConfigResolution,
  envoy: EnvoySettings
): string | null {
  if (config.kind === "failed") return `${config.error}: Dispatch and Envoy are off`;
  const { missing, checkout } = config;
  const dispatchQuiet = !dispatch.enabled && dispatch.error === null;
  const envoyQuiet = !envoy.enabled && envoy.error === null;
  const parts: string[] = [];
  if (missing !== null && dispatchQuiet && envoyQuiet) {
    parts.push(missingConfigNotice(config.path, missing, "Dispatch and Envoy are off"));
  } else {
    if (!dispatch.enabled && dispatch.error !== null)
      parts.push(`Dispatch is off: ${dispatch.error}`);
    else if (dispatchQuiet && missing !== null) {
      parts.push(missingConfigNotice(config.path, missing, "Dispatch is off"));
    }
    if (!envoy.enabled && envoy.error !== null) parts.push(`Envoy is off: ${envoy.error}`);
    else if (envoyQuiet && missing !== null) {
      parts.push(missingConfigNotice(config.path, missing, "Envoy is off"));
    }
  }
  if (checkout?.kind === "valid" && checkout.envoyError !== null) parts.push(checkout.envoyError);
  if (checkout?.kind === "invalid" && (dispatch.enabled || dispatch.error !== checkout.error)) {
    parts.push(`${checkout.error}; not read while DISPATCH_URL is set`);
  }
  return parts.length === 0 ? null : parts.join("; ");
}

/** Both sections from `file` (`dispatchSettings`, `envoySettings`), each resolved on its own, and
 * the session's start notice. A file whose location could not be worked out turns both off with
 * that error. `nats: false` is for a session that opens no NATS connection (`envoySettings`). */
export function sessionSettings(
  file: SessionConfigFile,
  env: EnvoyEnvironment,
  options: { readonly cwd: string; readonly nats?: boolean | undefined }
): SessionSettings {
  if (file.path === null) {
    const off = offWithError(file.error);
    return { dispatch: off, envoy: off, notice: `${file.error}: Dispatch and Envoy are off` };
  }
  const config = readEnvoyConfig(file.path, env, options);
  const dispatch = dispatchSettings(env, config);
  const envoy = envoySettings(env, config, { nats: options.nats });
  return { dispatch, envoy, notice: startNotice(config, dispatch, envoy) };
}

/** Dispatch from `file` as it reads now, for a caller that resolves Dispatch on every use rather
 * than once at its start. */
export function sessionDispatchSettings(
  file: SessionConfigFile,
  env: DispatchEnvironment,
  options: { readonly cwd: string }
): DispatchConfigResolution {
  if (file.path === null) return offWithError(file.error);
  return dispatchSettings(env, readEnvoyConfig(file.path, env, options));
}

export interface DispatchConfigOptions {
  /** Where the caller runs, which picks its envoy.json (`envoyConfigPath`). */
  readonly harness: EnvoyHarness;
  /** The working directory whose `.envoy/envoy.json` merges over that file; `process.cwd()` by
   * default. */
  readonly cwd?: string;
}

/** Dispatch for a caller running in `options.harness`: `sessionDispatchSettings` over the file
 * that harness's rule chooses (`sessionConfigFile`). */
export function resolveDispatchConfig(
  env: DispatchEnvironment,
  options: DispatchConfigOptions
): DispatchConfigResolution {
  return sessionDispatchSettings(sessionConfigFile(options.harness, env), env, {
    cwd: options.cwd ?? process.cwd(),
  });
}

/** The active Dispatch configuration (`resolveDispatchConfig`, then `activeDispatchSettings`). */
export function activeDispatchConfig(
  env: DispatchEnvironment,
  options: DispatchConfigOptions
): ActiveDispatchConfig | null {
  return activeDispatchSettings(resolveDispatchConfig(env, options));
}
