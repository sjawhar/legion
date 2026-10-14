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

/** Why an Envoy with no NATS server named, from the file or `ENVOY_NATS_URL`, takes no inbound
 * messages. */
const NO_NATS_SERVER = "Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)";
/** Why a caller that opens no NATS connection (`nats: false`) takes none. */
const NO_NATS_CONNECTION = "this session opens no NATS connection";

/** How a session takes inbound messages: over `natsUrls` (never empty) as the nkey user `natsSeed`
 * names, none for a server that asks for no credential; or not at all, `off` saying why. */
export type EnvoyInbound =
  | { readonly natsUrls: readonly string[]; readonly natsSeed: NatsSeed | undefined }
  | { readonly off: string };

/** An Envoy a session uses: every value resolved, credentials included. */
export interface EnabledEnvoySettings {
  readonly enabled: true;
  readonly url: string;
  /** The listener's bearer; none for a loopback listener that asks for none. */
  readonly token: string | undefined;
  /** Inbound messages, which arrive over NATS. Off with `NO_NATS_SERVER` when nothing names a
   * NATS server, with the refusal when the seed for the named ones could not be read or used (the
   * listener and its bearer stand), and with `NO_NATS_CONNECTION` for a caller that opens none. */
  readonly inbound: EnvoyInbound;
  readonly heartbeatMs: number;
}

/** No Envoy: a `notice` says why, or an `error` names what could not be read or used. */
export type DisabledEnvoySettings = SectionOff;

export type EnvoySettings = EnabledEnvoySettings | DisabledEnvoySettings;

/** A NATS server URL as two spellings of one server compare: scheme and host lower-cased, the
 * default port 4222 written when none is, a trailing `/` dropped. Anything that is not a
 * `scheme://host` URL compares as written. */
function natsServerKey(url: string): string {
  const match = /^([a-z][a-z0-9+.-]*):\/\/([^@/]*@)?(\[[^\]]*\]|[^:/]*)(?::(\d+))?(\/.*)?$/i.exec(
    url
  );
  if (match === null) return url;
  const [, scheme = "", userinfo = "", host = "", port = "4222", rest = ""] = match;
  return `${scheme.toLowerCase()}://${userinfo}${host.toLowerCase()}:${port}${rest.replace(/\/+$/, "")}`;
}

/** The NATS user's seed: `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED`, else the section's
 * `natsSeed`, which goes only with the section's own servers: `ENVOY_NATS_URL`, the only way to
 * name others, must name exactly them (as a set, each compared by `natsServerKey`). Throws naming
 * what could not be read or used. */
function natsCredential(
  env: EnvoyEnvironment,
  section: EnvoyFileSettings,
  namedNatsUrls: readonly string[],
  filePath: string
): NatsSeed | undefined {
  const fromEnvironment = natsSeedFromEnvironment(env);
  if (fromEnvironment !== undefined || section.natsSeed === undefined) return fromEnvironment;
  if (namedNatsUrls.length > 0) {
    const servers = new Set((section.natsUrls ?? []).map(natsServerKey));
    const named = new Set(namedNatsUrls.map(natsServerKey));
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
 * the Envoy the session's environment configured. The `enabled` rule is the Dispatch section's
 * (`dispatchSettings`): `envoy.enabled: false` turns Envoy off whatever the variables say;
 * otherwise the section's fields apply, and Envoy is on when its address resolves: `envoy.url` or
 * `ENVOY_URL`, `envoy.natsUrls` or `ENVOY_NATS_URL`, or the default listener `envoy.enabled: true`
 * names. With none of them, Envoy is off with a notice. Each variable overrides its own field:
 * `ENVOY_URL` the listener (`url`), `ENVOY_NATS_URL` (comma-separated) the NATS servers
 * (`natsUrls`), `ENVOY_TOKEN_FILE` (trimmed file contents, how the Legion daemon delivers it) then
 * `ENVOY_TOKEN` the listener's bearer (`token`), and `NATS_NKEY_SEED_FILE` then `NATS_NKEY_SEED`
 * the NATS user's seed (`natsSeed`, resolved only when there are NATS servers to use it on). An
 * empty `ENVOY_*` variable counts as unset; an empty `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED` is
 * refused, naming it (`natsSeedFromEnvironment`). A set pointer that cannot be read is refused,
 * never a fallback: `ENVOY_TOKEN_FILE`'s turns Envoy off with that `error`, and a seed's turns off
 * inbound messages alone (`inbound.off` names the refusal), since the seed is for NATS only.
 *
 * Same server only: the file's token goes with `ENVOY_URL` only when it names the origin of the
 * listener written in the file (or the default listener when it names none); any other listener
 * needs `ENVOY_TOKEN` or `ENVOY_TOKEN_FILE`. The file's seed goes only with the servers written in
 * the file: nkey sign-in sends no seed, but a server that is not the real one can relay the real
 * server's nonce and replay the signature. So `ENVOY_NATS_URL` gets the file's seed only when it
 * names the file's servers (as a set, compared normalised: `natsServerKey`), and is otherwise
 * refused, turning off inbound messages alone; a seed from the variables goes with whatever servers
 * resolve. The file's `!` token and seed run here, only when Envoy uses them, and a seed's failing
 * command, too, turns off inbound messages alone.
 *
 * A caller that opens no NATS connection (`nats: false`, OpenCode's plugin) takes no inbound
 * messages and resolves no seed, so a seed it would never send can neither run a command nor turn
 * it off.
 */
export function envoySettings(
  env: EnvoyEnvironment,
  config: EnvoyConfig,
  options: { readonly nats?: boolean | undefined } = {}
): EnvoySettings {
  if (config.kind === "failed") return offWithError(config.error);
  const section = config.envoy;
  if (section.enabled === false) {
    return offWithNotice(`${config.path} sets envoy.enabled to false: Envoy is off`);
  }
  const namedUrl = nonEmpty(env.ENVOY_URL);
  const namedNatsUrls = (env.ENVOY_NATS_URL ?? "")
    .split(",")
    .map((url) => url.trim())
    .filter((url) => url.length > 0);
  const addressed =
    namedUrl !== undefined ||
    namedNatsUrls.length > 0 ||
    section.url !== undefined ||
    (section.natsUrls ?? []).length > 0 ||
    section.enabled === true;
  if (!addressed) {
    return offWithNotice(
      config.missing === null
        ? `${config.path} names no Envoy listener or NATS server, and neither ENVOY_URL nor ENVOY_NATS_URL is set: Envoy is off`
        : missingConfigNotice(config.path, config.missing, "Envoy is off")
    );
  }
  if (namedUrl !== undefined && httpUrl(namedUrl) === undefined) {
    return offWithError("ENVOY_URL must be an http or https URL");
  }
  const listener = section.url ?? DEFAULT_ENVOY_URL;
  const url = namedUrl ?? listener;
  let token: string | undefined;
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
    token =
      source.kind === "none"
        ? undefined
        : source.kind === "variable"
          ? source.token
          : fileBearer(source, url, "ENVOY", env);
  } catch (error) {
    return offWithError(messageFor(error));
  }
  const natsUrls = namedNatsUrls.length > 0 ? namedNatsUrls : (section.natsUrls ?? []);
  let inbound: EnvoyInbound;
  if (options.nats === false) inbound = { off: NO_NATS_CONNECTION };
  else if (natsUrls.length === 0) inbound = { off: NO_NATS_SERVER };
  else {
    try {
      inbound = { natsUrls, natsSeed: natsCredential(env, section, namedNatsUrls, config.path) };
    } catch (error) {
      inbound = { off: messageFor(error) };
    }
  }
  const rawHeartbeatMs = Number(env.ENVOY_HEARTBEAT_MS);
  return {
    enabled: true,
    url: normalizeEnvoyUrl(url),
    token,
    inbound,
    heartbeatMs:
      Number.isFinite(rawHeartbeatMs) && rawHeartbeatMs > 0
        ? Math.max(rawHeartbeatMs, MIN_HEARTBEAT_MS)
        : DEFAULT_HEARTBEAT_MS,
  };
}

export function normalizeEnvoyUrl(value: string): string {
  return value.replace(/\/+$/, "");
}
