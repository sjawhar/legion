import * as docker from "@pulumi/docker";
import type { MachineConfig } from "./machines";

// --- Pure helpers (exported for testing) ---

interface PeerInfo {
  name: string;
  nats: boolean;
}

/**
 * Compute NATS cluster routes for a given machine.
 * Returns routes to all OTHER machines that run NATS peers.
 */
export function computeNatsRoutes(machineName: string, machines: PeerInfo[]): string[] {
  return machines
    .filter((m) => m.nats && m.name !== machineName)
    .map((m) => `nats://${m.name}:6222`);
}

/**
 * Render nats.conf content matching the format produced by
 * deploy/scripts/render-nats-peer.sh.
 */
export function renderNatsConf(serverName: string, routes: string[]): string {
  const routeLines = routes.map((r) => `    ${r}`).join("\n");
  return `server_name=${serverName}
listen=0.0.0.0:4222

jetstream {
  store_dir=/data
}

cluster {
  name: envoy
  listen: 0.0.0.0:6222
  routes: [
${routeLines}
  ]
}
`;
}

// --- Pulumi resources ---

/**
 * Create NATS peer resources on a machine: named volume + container.
 * Only called for machines with nats config.
 * Returns the container resource for dependency wiring.
 *
 * Volume name matches the existing Docker Compose-generated volume
 * (project "nats" + volume "nats_data" = "nats_nats_data").
 * Verify actual volume name per-machine during preflight.
 */
export function createNatsPeer(
  provider: docker.Provider,
  machine: MachineConfig,
  allMachines: MachineConfig[],
  natsImage: docker.RemoteImage
): docker.Container {
  if (!machine.nats) {
    throw new Error(`createNatsPeer called for ${machine.name} which has no nats config`);
  }

  const peerInfo = allMachines.map((m) => ({
    name: m.name,
    nats: !!m.nats,
  }));

  const routes = computeNatsRoutes(machine.name, peerInfo);
  const conf = renderNatsConf(machine.nats.serverName, routes);

  // Named volume — matches existing compose-generated "nats_nats_data"
  // for zero-copy migration. docker compose down (without -v) preserves it.
  const volume = new docker.Volume(
    `nats-data-${machine.name}`,
    {
      name: "nats_nats_data",
    },
    { provider }
  );

  const container = new docker.Container(
    `nats-${machine.name}`,
    {
      name: "envoy-nats",
      image: natsImage.imageId,
      restart: "unless-stopped",
      networkMode: "host",
      uploads: [
        {
          content: conf,
          file: "/etc/nats/nats.conf",
        },
      ],
      volumes: [
        {
          volumeName: volume.name,
          containerPath: "/data",
        },
      ],
      command: ["-c", "/etc/nats/nats.conf", "-m", "8222"],
      healthcheck: {
        tests: ["CMD", "wget", "-q", "--spider", "http://127.0.0.1:8222/healthz"],
        interval: "10s",
        timeout: "3s",
        retries: 3,
        startPeriod: "5s",
      },
      wait: true,
      waitTimeout: 30,
    },
    { provider, deleteBeforeReplace: true }
  );

  return container;
}
