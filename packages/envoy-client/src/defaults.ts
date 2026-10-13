import {
  type EnvoyConfig,
  type EnvoyConfigEnvironment,
  envoyTokenVariableSet,
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

/** No Envoy: `notice` says why, or `error` names what could not be read or used. */
export interface DisabledEnvoySettings {
  readonly enabled: false;
  readonly notice: string | null;
  readonly error: string | null;
}

export type EnvoySettings = EnabledEnvoySettings | DisabledEnvoySettings;

function disabled(error: string): DisabledEnvoySettings {
  return { enabled: false, notice: null, error };
}

/**
 * The Envoy a session uses, from `config` (`readEnvoyConfig`) and the environment.
 *
 * An `envoy.enabled: false` turns Envoy off whatever the variables say. Otherwise each variable
 * overrides its own field of an enabled `envoy` section: `ENVOY_URL` the listener (`url`),
 * `ENVOY_NATS_URL` (comma-separated) the NATS servers (`natsUrls`), `ENVOY_TOKEN_FILE` (trimmed
 * file contents, how the Legion daemon delivers it) then `ENVOY_TOKEN` the listener's bearer
 * (`token`), and `NATS_NKEY_SEED_FILE` then `NATS_NKEY_SEED` the NATS user's seed (`natsSeed`). An
 * empty variable counts as unset. With no enabled section, `ENVOY_URL` or `ENVOY_NATS_URL` alone
 * names an Envoy; with neither, Envoy is off. A set pointer that cannot be read is an `error`,
 * never a fallback.
 *
 * The file's token goes with `ENVOY_URL` only when both name the same origin; any other listener
 * needs `ENVOY_TOKEN` or `ENVOY_TOKEN_FILE`. A checkout's `envoy.url` is used only with the
 * `envoy.token` written beside it.
 */
export function envoySettings(env: EnvoyEnvironment, config: EnvoyConfig): EnvoySettings {
  if (config.error !== null) return disabled(config.error);
  const section = config.envoy;
  const checkout = config.checkout;
  if (section.enabled === false) {
    const file = checkout?.envoy.enabled === false ? checkout.path : config.path;
    return {
      enabled: false,
      notice: `${file} sets envoy.enabled to false: Envoy is off`,
      error: null,
    };
  }
  const file = section.enabled === true ? section : {};
  const namedUrl = env.ENVOY_URL === "" ? undefined : env.ENVOY_URL;
  const namedNatsUrls = (env.ENVOY_NATS_URL ?? "")
    .split(",")
    .map((url) => url.trim())
    .filter((url) => url.length > 0);
  if (section.enabled !== true && namedUrl === undefined && namedNatsUrls.length === 0) {
    return {
      enabled: false,
      notice:
        config.notice ??
        `${config.path} has no enabled envoy section, and neither ENVOY_URL nor ENVOY_NATS_URL is set: Envoy is off`,
      error: null,
    };
  }

  let url: string;
  let token: string | undefined;
  if (namedUrl === undefined && section.enabled === true && checkout?.envoy.url !== undefined) {
    // A repository may name its own listener only with its own bearer, never the user's.
    url = checkout.envoy.url;
    token = checkout.envoy.token;
  } else {
    url = namedUrl ?? file.url ?? DEFAULT_ENVOY_URL;
    const tokenFile = env.ENVOY_TOKEN_FILE === "" ? undefined : env.ENVOY_TOKEN_FILE;
    if (tokenFile !== undefined) {
      try {
        token = readSecretFile("ENVOY_TOKEN_FILE", tokenFile);
      } catch (error) {
        return disabled(messageFor(error));
      }
    } else {
      token = env.ENVOY_TOKEN || file.token;
    }
    if (namedUrl !== undefined && file.token !== undefined && !envoyTokenVariableSet(env)) {
      let named: string;
      try {
        named = new URL(namedUrl).origin;
      } catch {
        return disabled("ENVOY_URL must be a valid URL");
      }
      const listener = new URL(file.url ?? DEFAULT_ENVOY_URL).origin;
      if (named !== listener) {
        const tokenPath = checkout?.envoy.token !== undefined ? checkout.path : config.path;
        return disabled(
          `ENVOY_URL names ${named}, but ${tokenPath}'s envoy.token is for ${listener}; set ENVOY_TOKEN or ENVOY_TOKEN_FILE for it`
        );
      }
    }
  }

  let natsSeed: NatsSeed | undefined;
  try {
    natsSeed = natsSeedFromEnvironment(env);
  } catch (error) {
    return disabled(messageFor(error));
  }
  if (natsSeed === undefined && file.natsSeed !== undefined) {
    const seedPath = checkout?.envoy.natsSeed !== undefined ? checkout.path : config.path;
    natsSeed = { value: file.natsSeed.trim(), source: `envoy.natsSeed in ${seedPath}` };
  }

  const rawHeartbeatMs = Number(env.ENVOY_HEARTBEAT_MS);
  return {
    enabled: true,
    url: normalizeEnvoyUrl(url),
    natsUrls: namedNatsUrls.length > 0 ? namedNatsUrls : (file.natsUrls ?? []),
    token,
    natsSeed,
    heartbeatMs:
      Number.isFinite(rawHeartbeatMs) && rawHeartbeatMs > 0
        ? Math.max(rawHeartbeatMs, MIN_HEARTBEAT_MS)
        : DEFAULT_HEARTBEAT_MS,
  };
}

export function normalizeEnvoyUrl(value: string): string {
  return value.replace(/\/+$/, "");
}
