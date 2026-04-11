import * as docker from "@pulumi/docker";
import * as pulumi from "@pulumi/pulumi";
import type { MachineConfig } from "./machines";

// --- Pure helper (exported for testing) ---

interface PeerInfo {
  name: string;
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
    .map((m) => `nats://${m.name}:4222`);

  if (hasLocalNats) {
    return ["nats://127.0.0.1:4222", ...remotePeers].join(",");
  }
  return remotePeers.join(",");
}

// --- Pulumi resources ---

interface ServiceSecrets {
  githubWebhookSecret: pulumi.Output<string>;
  slackSigningSecret: pulumi.Output<string>;
  ghostWisprSigningSecret?: pulumi.Output<string>;
  tsnetAuthKey?: pulumi.Output<string>;
}

function getNatsUrls(machine: MachineConfig, allMachines: MachineConfig[]): string {
  const peerInfo = allMachines.map((m) => ({
    name: m.name,
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
 * Compute tsnet environment variables for a listener container.
 * Returns empty array when tsnet is not configured.
 */
export function computeTsnetEnvs(
  machine: MachineConfig,
  secrets: ServiceSecrets,
): pulumi.Input<string>[] {
  const tsnet = machine.listener.tsnet;
  if (!tsnet) {
    return ["ENVOY_TSNET_ENABLED=false"];
  }
  return [
    "ENVOY_TSNET_ENABLED=true",
    `ENVOY_TSNET_HOSTNAME=${tsnet.hostname}`,
    `ENVOY_TSNET_STATE_DIR=${tsnet.stateDir}`,
    ...(secrets.tsnetAuthKey
      ? [pulumi.interpolate`ENVOY_TSNET_AUTH_KEY=${secrets.tsnetAuthKey}`]
      : []),
  ];
}

/**
 * Compute tsnet volumes for a listener container.
 * Returns a named Docker volume for persistent tsnet state when enabled.
 */
export function computeTsnetVolumes(
  provider: docker.Provider,
  machine: MachineConfig,
): { volumes: docker.types.input.ContainerVolume[]; dependsOn: pulumi.Resource[] } {
  const tsnet = machine.listener.tsnet;
  if (!tsnet) {
    return { volumes: [], dependsOn: [] };
  }
  const volumeName = `envoy-listener-tsnet-state-${machine.name}`;
  const volume = new docker.Volume(
    `listener-tsnet-state-${machine.name}`,
    { name: volumeName },
    { provider },
  );
  return {
    volumes: [
      {
        volumeName: volume.name,
        containerPath: tsnet.stateDir,
      },
    ],
    dependsOn: [volume],
  };
}

/**
 * Create the listener container on a machine.
 * Runs on ALL machines (host networking).
 * When tsnet is configured, /v1/* routes are served exclusively on the
 * tsnet TLS interface and the legacy HTTP port only serves /healthz.
 */
export function createListener(
  provider: docker.Provider,
  machine: MachineConfig,
  allMachines: MachineConfig[],
  image: docker.RemoteImage,
  secrets: ServiceSecrets,
  dependsOn: pulumi.Resource[],
): docker.Container {
  const natsUrls = getNatsUrls(machine, allMachines);
  const kvReplicas = countNatsPeers(allMachines);
  const tsnetEnvs = computeTsnetEnvs(machine, secrets);
  const tsnetVols = computeTsnetVolumes(provider, machine);

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
        "ENVOY_HOST_BRIDGE=127.0.0.1",
        `ENVOY_KV_REPLICAS=${kvReplicas}`,
        ...tsnetEnvs,
      ],
      volumes: [...tsnetVols.volumes],
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
    {
      provider,
      dependsOn: [...dependsOn, ...tsnetVols.dependsOn],
      deleteBeforeReplace: true,
    },
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

/**
 * Create the Ghost Wispr webhook receiver container.
 * Only on machines with receivers.ghostwispr = true.
 */
export function createGhostWisprReceiver(
  provider: docker.Provider,
  machine: MachineConfig,
  allMachines: MachineConfig[],
  image: docker.RemoteImage,
  secrets: ServiceSecrets,
  dependsOn: pulumi.Resource[]
): docker.Container {
  const port = 9012;
  const natsUrls = getNatsUrls(machine, allMachines);
  const envs: pulumi.Input<string>[] = [
    `PORT=${port}`,
    `ENVOY_MACHINE_ID=${machine.machineId}`,
    `NATS_URLS=${natsUrls}`,
    ...(secrets.ghostWisprSigningSecret
      ? [pulumi.interpolate`ENVOY_GHOSTWISPR_SIGNING_SECRET=${secrets.ghostWisprSigningSecret}`]
      : []),
  ];

  return new docker.Container(
    `ghostwispr-${machine.name}`,
    {
      name: "envoy-ghostwispr",
      image: image.imageId,
      command: ["/usr/local/bin/envoy-ghostwispr"],
      restart: "unless-stopped",
      networkMode: "host",
      envs,
      healthcheck: {
        tests: ["CMD", "curl", "-f", `http://127.0.0.1:${port}/healthz`],
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
