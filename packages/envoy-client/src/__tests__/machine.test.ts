import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { hostname } from "node:os";
import { machineID } from "../machine";

const KEYS = ["HOSTNAME", "ENVOY_MACHINE_ID", "AGENTBOX_HOST"] as const;

describe("machineID", () => {
  let saved: Partial<Record<(typeof KEYS)[number], string>>;
  beforeEach(() => {
    saved = {};
    for (const key of KEYS) {
      if (process.env[key] !== undefined) saved[key] = process.env[key];
      delete process.env[key];
    }
  });
  afterEach(() => {
    for (const key of KEYS) {
      const value = saved[key];
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  });

  test("uses the OS hostname instead of the HOSTNAME environment variable", () => {
    process.env.HOSTNAME = "envoy-test-hostname-override";
    expect(machineID()).toBe(hostname());
    expect(machineID()).not.toBe(process.env.HOSTNAME);
  });

  test("honors the listener's logical machine name when ENVOY_MACHINE_ID is set", () => {
    process.env.ENVOY_MACHINE_ID = "logical-machine-name";
    process.env.AGENTBOX_HOST = "example-host-devbox";
    expect(machineID()).toBe("logical-machine-name");
  });

  test("inside an agent box, names the machine the box runs on, not the container's hostname", () => {
    process.env.AGENTBOX_HOST = "example-host-devbox";
    expect(machineID()).toBe("example-host-devbox");
  });

  test("an empty AGENTBOX_HOST falls back to the kernel hostname", () => {
    process.env.AGENTBOX_HOST = "";
    expect(machineID()).toBe(hostname());
  });
});
