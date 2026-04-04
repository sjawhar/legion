import * as docker from "@pulumi/docker";
import type { MachineConfig } from "./machines";

export interface MachineImages {
  nats?: docker.RemoteImage;
  listener: docker.RemoteImage;
  github?: docker.RemoteImage;
  slack?: docker.RemoteImage;
}

/**
 * Pull only the images a machine actually needs.
 * - All machines: listener
 * - NATS machines only: nats
 * - Receiver machines only: github, slack (as configured)
 *
 * Uses keepLocally: true to avoid deleting images on `pulumi destroy`.
 */
export function pullImages(
  provider: docker.Provider,
  machine: MachineConfig,
  registry: string,
  imageTag: string,
  natsImage: string
): MachineImages {
  const result: MachineImages = {
    listener: new docker.RemoteImage(
      `listener-image-${machine.name}`,
      {
        name: `${registry}/envoy-listener:${imageTag}`,
        keepLocally: true,
      },
      { provider }
    ),
  };

  if (machine.nats) {
    result.nats = new docker.RemoteImage(
      `nats-image-${machine.name}`,
      {
        name: natsImage,
        keepLocally: true,
      },
      { provider }
    );
  }

  if (machine.receivers?.github) {
    result.github = new docker.RemoteImage(
      `github-image-${machine.name}`,
      {
        name: `${registry}/envoy-github:${imageTag}`,
        keepLocally: true,
      },
      { provider }
    );
  }

  if (machine.receivers?.slack) {
    result.slack = new docker.RemoteImage(
      `slack-image-${machine.name}`,
      {
        name: `${registry}/envoy-slack:${imageTag}`,
        keepLocally: true,
      },
      { provider }
    );
  }

  return result;
}
