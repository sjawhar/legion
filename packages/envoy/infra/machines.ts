import * as docker from "@pulumi/docker";

export interface NatsConfig {
  serverName: string;
}

export interface ListenerConfig {
  registryDir: string;
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
