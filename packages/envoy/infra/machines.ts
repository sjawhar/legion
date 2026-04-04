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
}

export interface MachineConfig {
  name: string;
  sshHost: string;
  machineId: string;
  tailscaleIp: string;
  nats?: NatsConfig;
  listener: ListenerConfig;
  receivers?: ReceiverConfig;
}

export function createProvider(machine: MachineConfig): docker.Provider {
  return new docker.Provider(`docker-${machine.name}`, {
    host: machine.sshHost,
  });
}
