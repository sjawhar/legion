import { bearerSource, fileBearer } from "./bearer";
import {
  type EnvoyConfig,
  type EnvoyConfigEnvironment,
  type EnvoyFileSettings,
  fileValue,
  httpUrl,
  missingConfigNotice,
  nonEmpty,
  offWithError,
  offWithNotice,
  type SectionOff,
} from "./envoy-config";
import { messageFor } from "./errors";
import { readSecretFile } from "./secret-file";

/** The listener's address when neither `envoy.url` nor `ENVOY_URL` names one. */
export const DEFAULT_ENVOY_URL = "http://127.0.0.1:9020";
// There is deliberately no NATS default: the broker's location is deployment configuration
// (`envoy.natsUrls` or `ENVOY_NATS_URL`), and any baked-in value silently misroutes every
// deployment it doesn't match. An empty natsUrls means "inbound messaging not configured" -
// consumers must degrade loudly instead of dialing an invented address.
export const DEFAULT_HEARTBEAT_MS = 120_000;
const MIN_HEARTBEAT_MS = 25;

/** The variables `envoySettings` consults, by name, over whatever else the environment holds. */
export type EnvoyEnvironment = EnvoyConfigEnvironment & {
  readonly ENVOY_URL?: string | undefined;
  readonly ENVOY_NATS_URL?: string | undefined;
  readonly ENVOY_TOKEN?: string | undefined;
  readonly ENVOY_TOKEN_FILE?: string | undefined;
  readonly NATS_NKEY_SEED?: string | undefined;
  readonly NATS_NKEY_SEED_FILE?: string | undefined;
  readonly ENVOY_HEARTBEAT_MS?: string | undefined;
};

/** A NATS user's nkey seed, and where it came from, which an error about it names. */
export interface NatsSeed {
  readonly value: string;
  readonly source: string;
}

/** The seed the environment names: the trimmed contents of the file `NATS_NKEY_SEED_FILE` names,
 * else `NATS_NKEY_SEED`. A set variable is authoritative, and the file pointer wins over the seed:
 * an empty pointer, or a missing, unreadable or blank file, throws naming the variable and the path
 * (`readSecretFile`), never a fallback to `NATS_NKEY_SEED` or to no credential; so does a blank
 * `NATS_NKEY_SEED`. Neither set is undefined. No error carries the seed. The Go clients read the
 * same two variables (`packages/envoy/internal/bus/nkey.go`, `packages/daemon/internal/natsauth`). */
export function natsSeedFromEnvironment(env: EnvoyEnvironment): NatsSeed | undefined {
  const { NATS_NKEY_SEED_FILE: file, NATS_NKEY_SEED: plain } = env;
  if (file !== undefined) {
    if (file === "") throw new Error("NATS_NKEY_SEED_FILE is set but empty");
    return {
      value: readSecretFile("NATS_NKEY_SEED_FILE", file),
      source: `NATS_NKEY_SEED_FILE (${file})`,
    };
  }
  if (plain === undefined) return undefined;
  const value = plain.trim();
  if (value.length === 0) throw new Error("NATS_NKEY_SEED is set but empty");
  return { value, source: "NATS_NKEY_SEED" };
}

/** An Envoy a session uses: every value resolved, credentials included. */
export interface EnabledEnvoySettings {
  readonly enabled: true;
  readonly url: string;
  readonly natsUrls: readonly string[];
  /** The listener's bearer; none for a loopback listener that asks for none. */
  readonly token: string | undefined;
  /** The NATS user's nkey seed; none for a server that asks for no credential. */
  readonly natsSeed: NatsSeed | undefined;
  readonly heartbeatMs: number;
}

/** No Envoy: a `notice` says why, or an `error` names what could not be read or used. */
export type DisabledEnvoySettings = SectionOff;

export type EnvoySettings = EnabledEnvoySettings | DisabledEnvoySettings;

/** The NATS user's seed: `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED`, else the section's
 * `natsSeed`, which goes only with the section's own servers: `ENVOY_NATS_URL`, the only way to
 * name others, must name exactly them (as a set). Throws naming what could not be read or used. */
function natsCredential(
  env: EnvoyEnvironment,
  section: EnvoyFileSettings,
  namedNatsUrls: readonly string[],
  filePath: string
): NatsSeed | undefined {
  const fromEnvironment = natsSeedFromEnvironment(env);
  if (fromEnvironment !== undefined || section.natsSeed === undefined) return fromEnvironment;
  if (namedNatsUrls.length > 0) {
    const servers = new Set(section.natsUrls ?? []);
    const named = new Set(namedNatsUrls);
    if (named.size !== servers.size || ![...named].every((server) => servers.has(server))) {
      throw new Error(
        `ENVOY_NATS_URL names other servers than ${filePath}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`
      );
    }
  }
  return {
    value: fileValue(section.natsSeed, "envoy.natsSeed", filePath, env),
    source: `envoy.natsSeed in ${filePath}`,
  };
}

/**
 * The Envoy a session uses, from `config` (`readEnvoyConfig`) and the environment.
 *
 * Envoy is configured only by the chosen file and the variables: a checkout's file has no say in
 * it, so a cloned repository never chooses where a session's deliveries come from, nor turns off
 * the Envoy the session's environment configured. The chosen file's `envoy.enabled: false` wins.
 * Otherwise each variable overrides its own field of the chosen file's enabled `envoy` section:
 * `ENVOY_URL` the listener (`url`), `ENVOY_NATS_URL` (comma-separated) the NATS servers
 * (`natsUrls`), `ENVOY_TOKEN_FILE` (trimmed file contents, how the Legion daemon delivers it) then
 * `ENVOY_TOKEN` the listener's bearer (`token`), and `NATS_NKEY_SEED_FILE` then `NATS_NKEY_SEED`
 * the NATS user's seed (`natsSeed`, resolved only when there are NATS servers to use it on). An
 * empty `ENVOY_*` variable counts as unset; an empty `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED` is
 * refused, naming it (`natsSeedFromEnvironment`). With no enabled section, `ENVOY_URL` or
 * `ENVOY_NATS_URL` alone names an Envoy; with neither, Envoy is off with a notice. A set pointer
 * that cannot be read is an `error`, never a fallback.
 *
 * Same server only: the file's token goes with `ENVOY_URL` only when it names the origin of the
 * listener written in the file (or the default listener when it names none); any other listener
 * needs `ENVOY_TOKEN` or `ENVOY_TOKEN_FILE`. The file's seed goes only with the servers written in
 * the file: nkey sign-in sends no seed, but a server that is not the real one can relay the real
 * server's nonce and replay the signature. So `ENVOY_NATS_URL` gets the file's seed only when it
 * names the file's servers (as a set); a seed from the variables goes with whatever servers
 * resolve. The file's `!` token and seed run here, only when Envoy uses them.
 *
 * A caller that opens no NATS connection (`nats: false`, OpenCode's plugin) gets no NATS servers
 * and resolves no seed, so a seed it would never send can neither run a command nor turn it off.
 */
export function envoySettings(
  env: EnvoyEnvironment,
  config: EnvoyConfig,
  options: { readonly nats?: boolean | undefined } = {}
): EnvoySettings {
  if (config.kind === "failed") return offWithError(config.error);
  const own = config.envoy;
  if (own.enabled === false) {
    return offWithNotice(`${config.path} sets envoy.enabled to false: Envoy is off`);
  }
  const namedUrl = nonEmpty(env.ENVOY_URL);
  const namedNatsUrls = (env.ENVOY_NATS_URL ?? "")
    .split(",")
    .map((url) => url.trim())
    .filter((url) => url.length > 0);
  if (own.enabled !== true && namedUrl === undefined && namedNatsUrls.length === 0) {
    return offWithNotice(
      config.missing === null
        ? `${config.path} has no enabled envoy section, and neither ENVOY_URL nor ENVOY_NATS_URL is set: Envoy is off`
        : missingConfigNotice(config.path, config.missing, "Envoy is off")
    );
  }
  if (namedUrl !== undefined && httpUrl(namedUrl) === undefined) {
    return offWithError("ENVOY_URL must be an http or https URL");
  }
  // The file's values count only for an enabled section.
  const section = own.enabled === true ? own : {};
  const natsUrls =
    options.nats === false
      ? []
      : namedNatsUrls.length > 0
        ? namedNatsUrls
        : (section.natsUrls ?? []);
  const listener = section.url ?? DEFAULT_ENVOY_URL;
  const url = namedUrl ?? listener;
  try {
    const source = bearerSource(env, "ENVOY", () =>
      section.token === undefined
        ? undefined
        : {
            token: section.token,
            field: "envoy.token",
            path: config.path,
            server: listener,
            checkout: false,
          }
    );
    const token =
      source.kind === "none"
        ? undefined
        : source.kind === "variable"
          ? source.token
          : fileBearer(source, url, "ENVOY", env);
    const natsSeed =
      natsUrls.length > 0 ? natsCredential(env, section, namedNatsUrls, config.path) : undefined;
    const rawHeartbeatMs = Number(env.ENVOY_HEARTBEAT_MS);
    return {
      enabled: true,
      url: normalizeEnvoyUrl(url),
      natsUrls,
      token,
      natsSeed,
      heartbeatMs:
        Number.isFinite(rawHeartbeatMs) && rawHeartbeatMs > 0
          ? Math.max(rawHeartbeatMs, MIN_HEARTBEAT_MS)
          : DEFAULT_HEARTBEAT_MS,
    };
  } catch (error) {
    return offWithError(messageFor(error));
  }
}

export function normalizeEnvoyUrl(value: string): string {
  return value.replace(/\/+$/, "");
}
