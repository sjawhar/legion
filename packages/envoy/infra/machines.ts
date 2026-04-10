import * as docker from "@pulumi/docker";

export interface NatsConfig {
  serverName: string;
}

export interface TsnetConfig {
  /** Tailscale hostname — must be unique per service per machine (e.g., "envoy-listener-sami-agents-mx"). */
  hostname: string;
  /** Persistent state directory — must be unique per service to avoid identity collisions. */
  stateDir: string;
}

export interface ListenerConfig {
	/** When set, the listener serves /v1/* exclusively on the tsnet TLS interface. */
	tsnet?: TsnetConfig;
}

export interface ReceiverConfig {
  github?: boolean;
  slack?: boolean;
  ghostwispr?: boolean;
}

export interface MachineConfig {
  name: string;
  sshHost?: string;
  machineId: string;
  nats?: NatsConfig;
  listener: ListenerConfig;
  receivers?: ReceiverConfig;
}

export function createProvider(
  machine: MachineConfig,
  registryAuth?: docker.types.input.ProviderRegistryAuth[]
): docker.Provider {
  return new docker.Provider(`docker-${machine.name}`, {
    host: machine.sshHost,
    registryAuth,
  });
}
