const DEFAULT_ENVOY_URL = "http://127.0.0.1:9020";
// NATS lives on the tailnet host `envoy-nats`, not on each machine. The
// pre-consolidation OMP extension defaulted to this URL; a localhost default
// silently breaks inbound delivery for every session that omits ENVOY_NATS_URL.
const DEFAULT_NATS_URL = "nats://envoy-nats:4222";
const DEFAULT_HEARTBEAT_MS = 120_000;
const MIN_HEARTBEAT_MS = 25;

export type EnvoyDefaults = {
  readonly envoyUrl: string;
  readonly natsUrls: readonly string[];
  readonly heartbeatMs: number;
};

export type EnvoyEnvironment = Readonly<Record<string, string | undefined>>;

export function envoyDefaultsFromEnvironment(environment: EnvoyEnvironment): EnvoyDefaults {
  // biome-ignore lint/complexity/useLiteralKeys: index signatures require bracket access.
  const rawHeartbeatMs = Number(environment["ENVOY_HEARTBEAT_MS"]);
  const heartbeatMs =
    Number.isFinite(rawHeartbeatMs) && rawHeartbeatMs > 0
      ? Math.max(rawHeartbeatMs, MIN_HEARTBEAT_MS)
      : DEFAULT_HEARTBEAT_MS;
  // biome-ignore lint/complexity/useLiteralKeys: index signatures require bracket access.
  const natsUrls = environment["ENVOY_NATS_URL"]
    ?.split(",")
    .map((url) => url.trim())
    .filter((url) => url.length > 0) ?? [DEFAULT_NATS_URL];

  return {
    // biome-ignore lint/complexity/useLiteralKeys: index signatures require bracket access.
    envoyUrl: normalizeEnvoyUrl(environment["ENVOY_URL"] ?? DEFAULT_ENVOY_URL),
    natsUrls: natsUrls.length > 0 ? natsUrls : [DEFAULT_NATS_URL],
    heartbeatMs,
  };
}

export function normalizeEnvoyUrl(value: string): string {
  return value.replace(/\/+$/, "");
}
