import * as docker from "@pulumi/docker";
import type { MachineConfig } from "./machines";

export interface MachineImages {
  nats?: docker.RemoteImage;
  envoy: docker.RemoteImage;
}

/**
 * Pull only the images a machine actually needs.
 * - All machines: envoy (single multi-binary image)
 * - NATS machines only: nats
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
    envoy: new docker.RemoteImage(
      `envoy-image-${machine.name}`,
      {
        name: `${registry}/envoy:${imageTag}`,
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

  return result;
}
