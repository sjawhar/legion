import { describe, expect, test } from "bun:test";
import { hostname } from "node:os";
import { machineID } from "../machine";

describe("machineID", () => {
  test("uses the OS hostname instead of the HOSTNAME environment variable", () => {
    const originalHostname = process.env.HOSTNAME;
    const originalMachineId = process.env.ENVOY_MACHINE_ID;
    process.env.HOSTNAME = "envoy-test-hostname-override";
    delete process.env.ENVOY_MACHINE_ID;

    try {
      expect(machineID()).toBe(hostname());
      expect(machineID()).not.toBe(process.env.HOSTNAME);
    } finally {
      if (originalHostname === undefined) delete process.env.HOSTNAME;
      else process.env.HOSTNAME = originalHostname;
      if (originalMachineId === undefined) delete process.env.ENVOY_MACHINE_ID;
      else process.env.ENVOY_MACHINE_ID = originalMachineId;
    }
  });

  test("honors the listener's logical machine name when ENVOY_MACHINE_ID is set", () => {
    const originalMachineId = process.env.ENVOY_MACHINE_ID;
    process.env.ENVOY_MACHINE_ID = "logical-machine-name";

    try {
      expect(machineID()).toBe("logical-machine-name");
    } finally {
      if (originalMachineId === undefined) delete process.env.ENVOY_MACHINE_ID;
      else process.env.ENVOY_MACHINE_ID = originalMachineId;
    }
  });
});
