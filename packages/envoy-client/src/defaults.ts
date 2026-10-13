import {
  type EnvoyConfig,
  type EnvoyConfigEnvironment,
  fileValue,
  sectionSwitch,
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
 * Envoy is configured only by the chosen file and the variables: a checkout's `envoy` section
 * (`checkout.envoyError`) turns Envoy off with that error, so a cloned repository never chooses
 * where a session's deliveries come from. A file that turns Envoy off (`sectionSwitch`) wins.
 * Otherwise each variable overrides its own field of the chosen file's enabled `envoy` section:
 * `ENVOY_URL` the listener (`url`), `ENVOY_NATS_URL` (comma-separated) the NATS servers
 * (`natsUrls`), `ENVOY_TOKEN_FILE` (trimmed file contents, how the Legion daemon delivers it) then
 * `ENVOY_TOKEN` the listener's bearer (`token`), and `NATS_NKEY_SEED_FILE` then `NATS_NKEY_SEED`
 * the NATS user's seed (`natsSeed`, resolved only when there are NATS servers to use it on). An
 * empty variable counts as unset. With no enabled section, `ENVOY_URL` or `ENVOY_NATS_URL` alone
 * names an Envoy; with neither, Envoy is off. A set pointer that cannot be read is an `error`,
 * never a fallback.
 *
 * The file's token goes with `ENVOY_URL` only when it names the origin of the listener written in
 * the file; any other listener needs `ENVOY_TOKEN` or `ENVOY_TOKEN_FILE`. The file's seed goes
 * only with the servers written in the file: nkey sign-in sends no seed, but a server that is not
 * the real one can relay the real server's nonce and replay the signature. So `ENVOY_NATS_URL`
 * gets the file's seed only when it names the file's servers (as a set); a seed from the variables
 * goes with whatever servers resolve. The file's `!` token and seed run here, only when Envoy uses
 * them.
 */
export function envoySettings(env: EnvoyEnvironment, config: EnvoyConfig): EnvoySettings {
  if (config.error !== null) return disabled(config.error);
  if (config.checkout !== null && config.checkout.envoyError !== null) {
    return disabled(config.checkout.envoyError);
  }
  const state = sectionSwitch(config, "envoy", env);
  if (state.offBy !== null) {
    return {
      enabled: false,
      notice: `${state.offBy} sets envoy.enabled to false: Envoy is off`,
      error: null,
    };
  }
  const namedUrl = env.ENVOY_URL === "" ? undefined : env.ENVOY_URL;
  const namedNatsUrls = (env.ENVOY_NATS_URL ?? "")
    .split(",")
    .map((url) => url.trim())
    .filter((url) => url.length > 0);
  if (!state.enabled && namedUrl === undefined && namedNatsUrls.length === 0) {
    return {
      enabled: false,
      notice:
        config.notice ??
        `${config.path} has no enabled envoy section, and neither ENVOY_URL nor ENVOY_NATS_URL is set: Envoy is off`,
      error: null,
    };
  }
  // The file's values count only for an enabled section.
  const own = state.enabled ? config.envoy : {};

  const url = namedUrl ?? own.url ?? DEFAULT_ENVOY_URL;
  let token: string | undefined;
  const tokenPointer = env.ENVOY_TOKEN_FILE === "" ? undefined : env.ENVOY_TOKEN_FILE;
  if (tokenPointer !== undefined) {
    try {
      token = readSecretFile("ENVOY_TOKEN_FILE", tokenPointer);
    } catch (error) {
      return disabled(messageFor(error));
    }
  } else if (env.ENVOY_TOKEN) {
    token = env.ENVOY_TOKEN;
  } else if (own.token !== undefined) {
    if (namedUrl !== undefined) {
      let named: string;
      try {
        named = new URL(namedUrl).origin;
      } catch {
        return disabled("ENVOY_URL must be a valid URL");
      }
      const listener = new URL(own.url ?? DEFAULT_ENVOY_URL).origin;
      if (named !== listener) {
        return disabled(
          `ENVOY_URL names ${named}, but ${config.path}'s envoy.token is for ${listener}; set ENVOY_TOKEN or ENVOY_TOKEN_FILE for it`
        );
      }
    }
    try {
      token = fileValue(own.token, "envoy.token", config.path, env);
    } catch (error) {
      return disabled(messageFor(error));
    }
  }

  const natsUrls = namedNatsUrls.length > 0 ? namedNatsUrls : (own.natsUrls ?? []);
  let natsSeed: NatsSeed | undefined;
  if (natsUrls.length > 0) {
    try {
      natsSeed = natsSeedFromEnvironment(env);
    } catch (error) {
      return disabled(messageFor(error));
    }
    if (natsSeed === undefined && own.natsSeed !== undefined) {
      // Without ENVOY_NATS_URL the servers are the file's own, so only it can name others.
      const servers = new Set((own.natsUrls ?? []).map((server) => server.trim()));
      const used = new Set(natsUrls.map((server) => server.trim()));
      if (used.size !== servers.size || ![...used].every((server) => servers.has(server))) {
        return disabled(
          `ENVOY_NATS_URL names other servers than ${config.path}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`
        );
      }
      try {
        const value = fileValue(own.natsSeed, "envoy.natsSeed", config.path, env);
        natsSeed = { value: value.trim(), source: `envoy.natsSeed in ${config.path}` };
      } catch (error) {
        return disabled(messageFor(error));
      }
    }
  }

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
}

export function normalizeEnvoyUrl(value: string): string {
  return value.replace(/\/+$/, "");
}
