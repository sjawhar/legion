import * as docker from "@pulumi/docker";
import * as pulumi from "@pulumi/pulumi";
import type { MachineConfig } from "./machines";

// --- Pure helper (exported for testing) ---

interface PeerInfo {
  name: string;
  tailscaleIp: string;
  nats: boolean;
}

/**
 * Compute the NATS_URLS connection string for a machine.
 * Machines with a local NATS peer get 127.0.0.1 first, then all other peers.
 * Machines without a local peer get all peer URLs.
 */
export function computeNatsUrls(
  machineName: string,
  hasLocalNats: boolean,
  machines: PeerInfo[]
): string {
  const remotePeers = machines
    .filter((m) => m.nats && m.name !== machineName)
    .map((m) => `nats://${m.tailscaleIp}:4222`);

  if (hasLocalNats) {
    return ["nats://127.0.0.1:4222", ...remotePeers].join(",");
  }
  return remotePeers.join(",");
}

// --- Pulumi resources ---

interface ServiceSecrets {
  githubWebhookSecret: pulumi.Output<string>;
  slackSigningSecret: pulumi.Output<string>;
}

function getNatsUrls(machine: MachineConfig, allMachines: MachineConfig[]): string {
  const peerInfo = allMachines.map((m) => ({
    name: m.name,
    tailscaleIp: m.tailscaleIp,
    nats: !!m.nats,
  }));
  return computeNatsUrls(machine.name, !!machine.nats, peerInfo);
}

/**
 * Count the number of NATS peers in the fleet.
 * Used to set ENVOY_KV_REPLICAS for proper JetStream replication.
 */
function countNatsPeers(allMachines: MachineConfig[]): number {
  return allMachines.filter((m) => !!m.nats).length;
}

/**
 * Create the listener container on a machine.
 * Runs on ALL machines (host networking).
 */
export function createListener(
  provider: docker.Provider,
  machine: MachineConfig,
  allMachines: MachineConfig[],
  image: docker.RemoteImage,
  dependsOn: pulumi.Resource[]
): docker.Container {
  const natsUrls = getNatsUrls(machine, allMachines);
  const kvReplicas = countNatsPeers(allMachines);

  return new docker.Container(
    `listener-${machine.name}`,
    {
      name: "envoy-listener",
      image: image.imageId,
      command: ["/usr/local/bin/envoy-listener"],
      restart: "unless-stopped",
      networkMode: "host",
      envs: [
        "PORT=9020",
        `ENVOY_MACHINE_ID=${machine.machineId}`,
        `NATS_URLS=${natsUrls}`,
        `ENVOY_REGISTRY_DIR=${machine.listener.registryDir}`,
        "ENVOY_HOST_BRIDGE=127.0.0.1",
        `ENVOY_KV_REPLICAS=${kvReplicas}`,
      ],
      volumes: [
        {
          hostPath: machine.listener.registryDir,
          containerPath: machine.listener.registryDir,
          readOnly: true,
        },
      ],
      healthcheck: {
        tests: ["CMD", "curl", "-f", "http://127.0.0.1:9020/healthz"],
        interval: "10s",
        timeout: "3s",
        retries: 3,
        startPeriod: "30s",
      },
      wait: true,
      waitTimeout: 90,
    },
    { provider, dependsOn, deleteBeforeReplace: true }
  );
}

/**
 * Create the GitHub webhook receiver container.
 * Only on machines with receivers.github = true.
 */
export function createGithubReceiver(
  provider: docker.Provider,
  machine: MachineConfig,
  allMachines: MachineConfig[],
  image: docker.RemoteImage,
  secrets: ServiceSecrets,
  dependsOn: pulumi.Resource[]
): docker.Container {
  const natsUrls = getNatsUrls(machine, allMachines);

  return new docker.Container(
    `github-${machine.name}`,
    {
      name: "envoy-github",
      image: image.imageId,
      command: ["/usr/local/bin/envoy-github"],
      restart: "unless-stopped",
      networkMode: "host",
      envs: [
        "PORT=9010",
        `ENVOY_MACHINE_ID=${machine.machineId}`,
        `NATS_URLS=${natsUrls}`,
        pulumi.interpolate`ENVOY_GITHUB_WEBHOOK_SECRET=${secrets.githubWebhookSecret}`,
        "ENVOY_GITHUB_MENTION_TRIGGER=@legion",
      ],
      healthcheck: {
        tests: ["CMD", "curl", "-f", "http://127.0.0.1:9010/healthz"],
        interval: "10s",
        timeout: "3s",
        retries: 3,
        startPeriod: "5s",
      },
      wait: true,
      waitTimeout: 30,
    },
    { provider, dependsOn, deleteBeforeReplace: true }
  );
}

/**
 * Create the Slack webhook receiver container.
 * Only on machines with receivers.slack = true.
 */
export function createSlackReceiver(
  provider: docker.Provider,
  machine: MachineConfig,
  allMachines: MachineConfig[],
  image: docker.RemoteImage,
  secrets: ServiceSecrets,
  dependsOn: pulumi.Resource[]
): docker.Container {
  const natsUrls = getNatsUrls(machine, allMachines);

  return new docker.Container(
    `slack-${machine.name}`,
    {
      name: "envoy-slack",
      image: image.imageId,
      command: ["/usr/local/bin/envoy-slack"],
      restart: "unless-stopped",
      networkMode: "host",
      envs: [
        "PORT=9011",
        `ENVOY_MACHINE_ID=${machine.machineId}`,
        `NATS_URLS=${natsUrls}`,
        pulumi.interpolate`ENVOY_SLACK_SIGNING_SECRET=${secrets.slackSigningSecret}`,
      ],
      healthcheck: {
        tests: ["CMD", "curl", "-f", "http://127.0.0.1:9011/healthz"],
        interval: "10s",
        timeout: "3s",
        retries: 3,
        startPeriod: "5s",
      },
      wait: true,
      waitTimeout: 30,
    },
    { provider, dependsOn, deleteBeforeReplace: true }
  );
}
