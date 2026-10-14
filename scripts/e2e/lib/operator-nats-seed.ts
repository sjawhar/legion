// scripts/e2e/lib/operator-nats-seed.ts: the nkey seed a command run from the operator's shell
// sends to a NATS it names (envoy-bridge.ts's upstream, nats-stream.ts's server). It is what
// `sessionEnvoySettings` resolves over the envoy.json `envoyConfigPath("command")` chooses with
// ENVOY_NATS_URL set to those servers (packages/envoy-client/src/session-settings.ts), so the
// operator's `envoy.natsSeed` serves these scripts as it serves the operator's sessions, and only
// against the servers written beside it.
import type { EnvoyEnvironment, NatsSeed } from "../../../packages/envoy-client/src/envoy-settings";
import {
  sessionConfigFile,
  sessionEnvoySettings,
} from "../../../packages/envoy-client/src/session-settings";

/**
 * The seed for `servers` (comma-separated, as ENVOY_NATS_URL takes them): NATS_NKEY_SEED_FILE or
 * NATS_NKEY_SEED, else the chosen file's `envoy.natsSeed` when its `envoy.natsUrls` are exactly
 * these servers; undefined when neither names one, for a server that asks for no credential.
 * Throws naming `label` when Envoy resolves none: a file that turns Envoy off or cannot be read, a
 * seed variable that cannot be read, or a file's seed for other servers (which leave Envoy on with
 * inbound messages off, `inbound.off` naming the refusal, and are a refusal here all the same).
 */
export function operatorNatsSeed(
  servers: string,
  label: string,
  env: EnvoyEnvironment,
  cwd: string
): NatsSeed | undefined {
  const named = { ...env, ENVOY_NATS_URL: servers };
  const settings = sessionEnvoySettings(sessionConfigFile("command", named), named, { cwd });
  const refused = `the NATS seed for ${label} (resolved as ENVOY_NATS_URL)`;
  if (!settings.enabled) throw new Error(`${refused}: ${settings.error ?? settings.notice}`);
  const { inbound } = settings;
  if ("off" in inbound) throw new Error(`${refused}: ${inbound.off}`);
  return inbound.natsSeed;
}
