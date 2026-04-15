import type * as docker from "@pulumi/docker";
import * as pulumi from "@pulumi/pulumi";
import { pullImages } from "./images";
import type { MachineConfig } from "./machines";
import { createProvider } from "./machines";
import { createNatsPeer } from "./nats";
import { computeNatsUrls, createListener } from "./services";
import { deployWatchdog } from "./watchdog";

const cfg = new pulumi.Config("envoy");

// Stack configuration
const registry = cfg.require("registry");
const imageTag = cfg.require("imageTag");
const natsImage = cfg.get("natsImage") ?? "nats:2.11-alpine";
const machines = cfg.requireObject<MachineConfig[]>("machines");

// Secrets — webhook secrets are optional at the Pulumi level.
// Go startup validates required secrets via ENVOY_WEBHOOKS config-gating.
const githubWebhookSecret = cfg.getSecret("githubWebhookSecret");
const slackSigningSecret = cfg.getSecret("slackSigningSecret");
const ghostWisprSigningSecret = cfg.getSecret("ghostWisprSigningSecret");
const tsnetOAuthClientId = cfg.getSecret("tsnetOAuthClientId");
const tsnetOAuthClientSecret = cfg.getSecret("tsnetOAuthClientSecret");

const secrets = {
  githubWebhookSecret,
  slackSigningSecret,
  ghostWisprSigningSecret,
  tsnetOAuthClientId,
  tsnetOAuthClientSecret,
};

// Registry auth for GHCR (private packages)
const registryAuth: docker.types.input.ProviderRegistryAuth[] = [
  { address: registry, username: "sjawhar", password: cfg.requireSecret("ghcrToken") },
];

// Deploy to each machine
for (const machine of machines) {
  const provider = createProvider(machine, registryAuth);
  const images = pullImages(provider, machine, registry, imageTag, natsImage);

  // NATS peer — only on machines with nats config
  const natsDependency: pulumi.Resource[] = [];
  if (machine.nats && images.nats) {
    const nats = createNatsPeer(provider, machine, machines, images.nats);
    natsDependency.push(nats);
  }

  // Listener — on ALL machines
  const listener = createListener(provider, machine, machines, images.envoy, secrets, natsDependency);

  // Watchdog — on-prem machines only (machines with SSH access).
  // Detects container removal and recreates via systemd timer.
  if (machine.sshHost) {
    const envs = [
      "PORT=9020",
      `ENVOY_MACHINE_ID=${machine.machineId}`,
      `NATS_URLS=${computeNatsUrls(machine.name, !!machine.nats, machines.map(m => ({ name: m.name, nats: !!m.nats })))}`,
      "ENVOY_HOST_BRIDGE=127.0.0.1",
      `ENVOY_KV_REPLICAS=${machines.filter(m => !!m.nats).length}`,
    ];
    deployWatchdog(machine, envs, `${registry}/envoy:${imageTag}`, [listener]);
  }
}
