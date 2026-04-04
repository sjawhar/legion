import * as pulumi from "@pulumi/pulumi";
import { pullImages } from "./images";
import type { MachineConfig } from "./machines";
import { createProvider } from "./machines";
import { createNatsPeer } from "./nats";
import { createGithubReceiver, createListener, createSlackReceiver } from "./services";

const cfg = new pulumi.Config("envoy");

// Stack configuration
const registry = cfg.require("registry");
const imageTag = cfg.require("imageTag");
const natsImage = cfg.get("natsImage") ?? "nats:2.11-alpine";
const machines = cfg.requireObject<MachineConfig[]>("machines");

// Secrets
const githubWebhookSecret = cfg.requireSecret("githubWebhookSecret");
const slackSigningSecret = cfg.requireSecret("slackSigningSecret");

const secrets = { githubWebhookSecret, slackSigningSecret };

// Deploy to each machine
for (const machine of machines) {
  const provider = createProvider(machine);
  const images = pullImages(provider, machine, registry, imageTag, natsImage);

  // NATS peer — only on machines with nats config
  const natsDependency: pulumi.Resource[] = [];
  if (machine.nats && images.nats) {
    const nats = createNatsPeer(provider, machine, machines, images.nats);
    natsDependency.push(nats);
  }

  // Listener — on ALL machines
  createListener(provider, machine, machines, images.envoy, natsDependency);

  // Receivers — only where configured
  if (machine.receivers?.github) {
    createGithubReceiver(provider, machine, machines, images.envoy, secrets, natsDependency);
  }

  if (machine.receivers?.slack) {
    createSlackReceiver(provider, machine, machines, images.envoy, secrets, natsDependency);
  }
}
