import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { operatorNatsSeed } from "./operator-nats-seed";

const dir = mkdtempSync(join(tmpdir(), "operator-nats-seed-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const production = "nats://envoy-nats.internal.example:4222";
const file = join(dir, "envoy.json");
mkdirSync(join(dir, "cwd"));
const cwd = join(dir, "cwd");
writeFileSync(
  file,
  JSON.stringify({ envoy: { enabled: true, natsUrls: [production], natsSeed: "SUFILESEED" } })
);
const off = join(dir, "off.json");
writeFileSync(off, JSON.stringify({ envoy: { enabled: false } }));

describe("operatorNatsSeed", () => {
  test("the file's seed goes to the servers written beside it", () => {
    expect(
      operatorNatsSeed(production, "SMOKE_UPSTREAM_NATS", { ENVOY_CONFIG_FILE: file }, cwd)
    ).toEqual({ value: "SUFILESEED", source: `envoy.natsSeed in ${file}` });
  });

  test("a seed variable wins over the file's, for any server", () => {
    const env = { ENVOY_CONFIG_FILE: file, NATS_NKEY_SEED: "SUVARIABLE" };
    expect(operatorNatsSeed("nats://other.internal.example:4222", "x", env, cwd)).toEqual({
      value: "SUVARIABLE",
      source: "NATS_NKEY_SEED",
    });
  });

  test("the file's seed never goes to another server, and the refusal names the caller's input", () => {
    expect(() =>
      operatorNatsSeed(
        "nats://other.internal.example:4222",
        "SMOKE_UPSTREAM_NATS",
        { ENVOY_CONFIG_FILE: file },
        cwd
      )
    ).toThrow(
      `the NATS seed for SMOKE_UPSTREAM_NATS (resolved as ENVOY_NATS_URL): ENVOY_NATS_URL names other servers than ${file}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`
    );
  });

  test("no seed anywhere is no credential", () => {
    const env = { ENVOY_CONFIG_FILE: join(dir, "absent.json") };
    expect(operatorNatsSeed(production, "x", env, cwd)).toBeUndefined();
  });

  test("a file that turns Envoy off is refused, saying so", () => {
    expect(() =>
      operatorNatsSeed(production, "<nats-url>", { ENVOY_CONFIG_FILE: off }, cwd)
    ).toThrow(
      `the NATS seed for <nats-url> (resolved as ENVOY_NATS_URL): ${off} sets envoy.enabled to false: Envoy is off`
    );
  });
});
