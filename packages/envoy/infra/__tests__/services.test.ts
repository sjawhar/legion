import { describe, expect, test } from "bun:test";
import { computeNatsUrls, computeTsnetEnvs } from "../services";
import type { MachineConfig } from "../machines";

describe("computeNatsUrls", () => {
  const machines = [
    { name: "sami-agents-mx", nats: true },
    { name: "sami", nats: true },
    { name: "sami-claude", nats: true },
    { name: "ghost-wispr", nats: false },
  ];

  test("machine with local NATS peer gets 127.0.0.1 first, then remote peers", () => {
    const urls = computeNatsUrls("sami-agents-mx", true, machines);
    expect(urls).toBe("nats://127.0.0.1:4222,nats://sami:4222,nats://sami-claude:4222");
  });

  test("machine without local NATS peer gets all remote peers", () => {
    const urls = computeNatsUrls("ghost-wispr", false, machines);
    expect(urls).toBe("nats://sami-agents-mx:4222,nats://sami:4222,nats://sami-claude:4222");
  });
});

describe("computeTsnetEnvs", () => {
  const baseMachine: MachineConfig = {
    name: "test-machine",
    machineId: "test-machine",
    listener: {},
  };

  const tsnetMachine: MachineConfig = {
    ...baseMachine,
    listener: {
      tsnet: {
        hostname: "envoy-listener-test",
        stateDir: "/var/lib/envoy-tsnet/listener-test",
      },
    },
  };

  const emptySecrets = {
    githubWebhookSecret: {} as any,
    slackSigningSecret: {} as any,
  };

  test("returns disabled when tsnet not configured", () => {
    const envs = computeTsnetEnvs(baseMachine, emptySecrets);
    expect(envs).toEqual(["ENVOY_TSNET_ENABLED=false"]);
  });

  test("returns enabled env vars when tsnet configured", () => {
    const envs = computeTsnetEnvs(tsnetMachine, emptySecrets);
    expect(envs).toContain("ENVOY_TSNET_ENABLED=true");
    expect(envs).toContain("ENVOY_TSNET_HOSTNAME=envoy-listener-test");
    expect(envs).toContain(
      "ENVOY_TSNET_STATE_DIR=/var/lib/envoy-tsnet/listener-test",
    );
  });

  test("includes auth key when secret provided", () => {
    const secrets = {
      ...emptySecrets,
      tsnetAuthKey: {} as any,
    };
    const envs = computeTsnetEnvs(tsnetMachine, secrets);
    // Auth key is a pulumi.interpolate output — verify it's in the array
    // (exact value is a Pulumi Output, not a plain string)
    expect(envs.length).toBe(4); // enabled + hostname + stateDir + authKey
  });

  test("omits auth key when not provided", () => {
    const envs = computeTsnetEnvs(tsnetMachine, emptySecrets);
    expect(envs.length).toBe(3); // enabled + hostname + stateDir
  });
});
