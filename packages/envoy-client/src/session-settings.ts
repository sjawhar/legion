import {
  type DispatchConfigResolution,
  type DispatchEnvironment,
  dispatchSettings,
} from "./dispatch-config";
import {
  type EnvoyConfig,
  type EnvoyHarness,
  envoyConfigPath,
  handedConfigPath,
  type MissingConfig,
  missingConfigNotice,
  offWithError,
  readEnvoyConfig,
} from "./envoy-config";
import { type EnvoyEnvironment, type EnvoySettings, envoySettings } from "./envoy-settings";
import { messageFor } from "./errors";

/** The envoy.json a session reads (`envoyConfigPath`), or why its location cannot be worked out (a
 * relative `XDG_CONFIG_HOME`, `ENVOY_CONFIG_FILE` or harness directory). */
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

/** The file `ENVOY_CONFIG_FILE` names, the one a plugin hands its session's commands, with a
 * relative path as `error`; null when the variable is unset or empty. */
export function handedConfigFile(env: EnvoyEnvironment): SessionConfigFile | null {
  try {
    const handed = handedConfigPath(env);
    return handed === undefined ? null : { path: handed, error: null };
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
 * A missing file configures neither section: with no variable naming either, its one notice says
 * both are off (`missingConfigNotice`, which names a dangling link's target and the OpenCode-era
 * user file, which is not read, while it is still there); when a variable turned a section on, each
 * section still off is said with the same missing-file text. A section's own error (a token that
 * does not resolve, a command that failed) is said for that section. A file that turns a section
 * off, or holds no section for it, says nothing. What a checkout's files hold that the session does
 * not read is said too: an `envoy` section, a file that cannot be used while `DISPATCH_URL`
 * replaces it, and an OpenCode-era `.opencode/envoy.json` (`warnings`). Last, for a session that
 * opens a NATS connection (`nats` not false), an Envoy that is on and takes no inbound messages is
 * said to, naming why (`inbound.off`: no NATS server, or the seed's refusal).
 */
function startNotice(
  config: EnvoyConfig,
  dispatch: DispatchConfigResolution,
  envoy: EnvoySettings,
  nats: boolean
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
  parts.push(...config.warnings);
  if (envoy.enabled && nats && "off" in envoy.inbound) {
    parts.push(
      `Envoy's inbound messages are off: ${envoy.inbound.off}; the Envoy tools still work`
    );
  }
  return parts.length === 0 ? null : parts.join("; ");
}

/** Both sections from `file` (`dispatchSettings`, `envoySettings`), each resolved on its own, and
 * the session's start notice. A file whose location could not be worked out turns both off with
 * that error. `nats: false` is for a session that opens no NATS connection (`envoySettings`), whose
 * notice says nothing of inbound messages. */
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
  return { dispatch, envoy, notice: startNotice(config, dispatch, envoy, options.nats !== false) };
}

/** Dispatch from a session's envoy.json as it reads now, and what the read says beside it. */
export interface SessionDispatch {
  /** Dispatch on, or off saying why (`dispatchSettings`). */
  readonly dispatch: DispatchConfigResolution;
  /** Each file the read found and did not read, naming its move (`ReadEnvoyConfig.warnings`). */
  readonly warnings: readonly string[];
  /** Set when the chosen file does not exist (`ReadEnvoyConfig.missing`). */
  readonly missing: MissingConfig | null;
}

/** Dispatch from `file`, read now: for a command, which resolves it once, and for a plugin that
 * resolves it on every use rather than once at its start. A file whose location could not be
 * worked out turns Dispatch off with that error. `activeDispatchSettings` turns its `dispatch` into
 * the configuration a caller uses, or null, or a throw. */
export function sessionDispatchSettings(
  file: SessionConfigFile,
  env: DispatchEnvironment,
  options: { readonly cwd: string }
): SessionDispatch {
  if (file.path === null)
    return { dispatch: offWithError(file.error), warnings: [], missing: null };
  const config = readEnvoyConfig(file.path, env, options);
  return {
    dispatch: dispatchSettings(env, config),
    warnings: config.kind === "read" ? config.warnings : [],
    missing: config.kind === "read" ? config.missing : null,
  };
}

/** Envoy from `file` (`envoySettings`), for a command that resolves Envoy alone. A file whose
 * location could not be worked out turns Envoy off with that error. */
export function sessionEnvoySettings(
  file: SessionConfigFile,
  env: EnvoyEnvironment,
  options: { readonly cwd: string; readonly nats?: boolean | undefined }
): EnvoySettings {
  if (file.path === null) return offWithError(file.error);
  return envoySettings(env, readEnvoyConfig(file.path, env, options), { nats: options.nats });
}
