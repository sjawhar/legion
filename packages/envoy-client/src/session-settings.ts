import { type EnvoyEnvironment, type EnvoySettings, envoySettings } from "./defaults";
import { type DispatchConfigResolution, dispatchSettings } from "./dispatch-config";
import {
  type EnvoyConfig,
  type EnvoyHarness,
  envoyConfigPath,
  readEnvoyConfig,
} from "./envoy-config";
import { messageFor } from "./errors";

/** The envoy.json a plugin's session reads (`envoyConfigPath`), or why its location cannot be
 * worked out (a relative `XDG_CONFIG_HOME`). */
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
 * The start notice. A file that cannot be read turns both sections off with its one error. A
 * missing file turns both off with its own notice; when a variable turned a section on, each
 * section still off names the file alone. A section's own error (a token that does not resolve, a
 * command that failed) is said for that section. A file that turns a section off, or holds no
 * section for it, says nothing.
 */
function startNotice(
  config: EnvoyConfig,
  dispatch: DispatchConfigResolution,
  envoy: EnvoySettings
): string | null {
  if (config.error !== null) return `${config.error}: Dispatch and Envoy are off`;
  const missing = config.notice !== null;
  const dispatchOff = !dispatch.enabled && dispatch.error === null;
  const envoyOff = !envoy.enabled && envoy.error === null;
  if (missing && dispatchOff && envoyOff) return config.notice;
  const parts: string[] = [];
  if (dispatch.error !== null) parts.push(`Dispatch is off: ${dispatch.error}`);
  else if (dispatchOff && missing) parts.push(`no envoy.json at ${config.path}: Dispatch is off`);
  if (!envoy.enabled && envoy.error !== null) parts.push(`Envoy is off: ${envoy.error}`);
  else if (envoyOff && missing) parts.push(`no envoy.json at ${config.path}: Envoy is off`);
  return parts.length === 0 ? null : parts.join("; ");
}

/** Both sections from `file` (`dispatchSettings`, `envoySettings`), each resolved on its own, and
 * the session's start notice. A file whose location could not be worked out turns both off with
 * that error. */
export function sessionSettings(
  file: SessionConfigFile,
  env: EnvoyEnvironment,
  options: { readonly cwd: string }
): SessionSettings {
  if (file.path === null) {
    return {
      dispatch: { enabled: false, url: null, token: null, error: file.error },
      envoy: { enabled: false, notice: null, error: file.error },
      notice: `${file.error}: Dispatch and Envoy are off`,
    };
  }
  const config = readEnvoyConfig(file.path, env, options);
  const dispatch = dispatchSettings(env, config);
  const envoy = envoySettings(env, config);
  return { dispatch, envoy, notice: startNotice(config, dispatch, envoy) };
}

/** Dispatch from `file` as it reads now, for a plugin that resolves Dispatch on every use rather
 * than once at its start. */
export function sessionDispatchSettings(
  file: SessionConfigFile,
  env: EnvoyEnvironment,
  options: { readonly cwd: string }
): DispatchConfigResolution {
  if (file.path === null) return { enabled: false, url: null, token: null, error: file.error };
  return dispatchSettings(env, readEnvoyConfig(file.path, env, options));
}
