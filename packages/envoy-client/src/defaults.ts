import {
  checkoutCommandRefusal,
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
 * A file that turns Envoy off (`sectionSwitch`) wins. Otherwise each variable overrides its own
 * field of an enabled `envoy` section, the checkout's merged over the chosen file's: `ENVOY_URL`
 * the listener (`url`), `ENVOY_NATS_URL` (comma-separated) the NATS servers (`natsUrls`),
 * `ENVOY_TOKEN_FILE` (trimmed file contents, how the Legion daemon delivers it) then `ENVOY_TOKEN`
 * the listener's bearer (`token`), and `NATS_NKEY_SEED_FILE` then `NATS_NKEY_SEED` the NATS user's
 * seed (`natsSeed`, resolved only when there are NATS servers to use it on). An empty variable
 * counts as unset. With no enabled section, `ENVOY_URL` or `ENVOY_NATS_URL` alone names an Envoy;
 * with neither, Envoy is off. A set pointer that cannot be read is an `error`, never a fallback.
 *
 * A file's token goes with `ENVOY_URL` only when it names the origin of the listener written in the
 * file the token comes from; any other listener needs `ENVOY_TOKEN` or `ENVOY_TOKEN_FILE`. A
 * checkout's `envoy.url` is used only with the `envoy.token` written beside it. The chosen file's
 * `!` token and seed run here, only when Envoy uses them; a checkout's are refused.
 */
export function envoySettings(env: EnvoyEnvironment, config: EnvoyConfig): EnvoySettings {
  if (config.error !== null) return disabled(config.error);
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
  // The files' values count only for an enabled section.
  const own = state.enabled ? config.envoy : {};
  const checkout = state.enabled ? config.checkout : null;

  let url: string;
  let token: string | undefined;
  if (namedUrl === undefined && checkout?.envoy.url !== undefined) {
    // A repository may name its own listener only with its own bearer, never the user's.
    if (!checkout.envoy.token) {
      return disabled(`${checkout.path}: envoy.url requires envoy.token in the same file`);
    }
    if (checkout.envoy.token.startsWith("!")) {
      return disabled(checkoutCommandRefusal(checkout.path, "envoy.token"));
    }
    url = checkout.envoy.url;
    token = checkout.envoy.token;
  } else {
    url = namedUrl ?? own.url ?? DEFAULT_ENVOY_URL;
    // When a file supplies the token: that file and the listener it is written for.
    let tokenFile: { readonly path: string; readonly url: string } | null = null;
    const tokenPointer = env.ENVOY_TOKEN_FILE === "" ? undefined : env.ENVOY_TOKEN_FILE;
    if (tokenPointer !== undefined) {
      try {
        token = readSecretFile("ENVOY_TOKEN_FILE", tokenPointer);
      } catch (error) {
        return disabled(messageFor(error));
      }
    } else if (env.ENVOY_TOKEN) {
      token = env.ENVOY_TOKEN;
    } else if (checkout?.envoy.token !== undefined) {
      token = checkout.envoy.token;
      const listener = checkout.envoy.url ?? own.url ?? DEFAULT_ENVOY_URL;
      tokenFile = { path: checkout.path, url: listener };
    } else if (own.token !== undefined) {
      token = own.token;
      tokenFile = { path: config.path, url: own.url ?? DEFAULT_ENVOY_URL };
    }
    if (namedUrl !== undefined && tokenFile !== null) {
      let named: string;
      try {
        named = new URL(namedUrl).origin;
      } catch {
        return disabled("ENVOY_URL must be a valid URL");
      }
      const listener = new URL(tokenFile.url).origin;
      if (named !== listener) {
        return disabled(
          `ENVOY_URL names ${named}, but ${tokenFile.path}'s envoy.token is for ${listener}; set ENVOY_TOKEN or ENVOY_TOKEN_FILE for it`
        );
      }
    }
    if (tokenFile !== null && token?.startsWith("!")) {
      if (tokenFile.path !== config.path) {
        return disabled(checkoutCommandRefusal(tokenFile.path, "envoy.token"));
      }
      try {
        token = fileValue(token, "envoy.token", config.path, env);
      } catch (error) {
        return disabled(messageFor(error));
      }
    }
  }

  const natsUrls =
    namedNatsUrls.length > 0 ? namedNatsUrls : (checkout?.envoy.natsUrls ?? own.natsUrls ?? []);
  let natsSeed: NatsSeed | undefined;
  if (natsUrls.length > 0) {
    try {
      natsSeed = natsSeedFromEnvironment(env);
      if (natsSeed === undefined && checkout?.envoy.natsSeed !== undefined) {
        if (checkout.envoy.natsSeed.startsWith("!")) {
          return disabled(checkoutCommandRefusal(checkout.path, "envoy.natsSeed"));
        }
        const source = `envoy.natsSeed in ${checkout.path}`;
        natsSeed = { value: checkout.envoy.natsSeed.trim(), source };
      } else if (natsSeed === undefined && own.natsSeed !== undefined) {
        const value = fileValue(own.natsSeed, "envoy.natsSeed", config.path, env);
        natsSeed = { value: value.trim(), source: `envoy.natsSeed in ${config.path}` };
      }
    } catch (error) {
      return disabled(messageFor(error));
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
