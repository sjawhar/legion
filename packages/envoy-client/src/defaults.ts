const DEFAULT_ENVOY_URL = "http://127.0.0.1:9020";
// There is deliberately no NATS default: the broker's location is deployment
// configuration (this fleet sets ENVOY_NATS_URL in the shared shell env), and
// any baked-in value silently misroutes every deployment it doesn't match.
// An empty natsUrls means "inbound messaging not configured" - consumers must
// degrade loudly instead of dialing an invented address.
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
  const natsUrls =
    environment["ENVOY_NATS_URL"]
      ?.split(",")
      .map((url) => url.trim())
      .filter((url) => url.length > 0) ?? [];

  return {
    // biome-ignore lint/complexity/useLiteralKeys: index signatures require bracket access.
    envoyUrl: normalizeEnvoyUrl(environment["ENVOY_URL"] ?? DEFAULT_ENVOY_URL),
    natsUrls,
    heartbeatMs,
  };
}

export function normalizeEnvoyUrl(value: string): string {
  return value.replace(/\/+$/, "");
}
