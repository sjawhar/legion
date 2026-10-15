import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { connect, type NatsConnection } from "nats";

const root = join(import.meta.dir, "..", "..", "..");
const container = `legion-nats-stream-${randomUUID()}`;
const stream = `NATS_STREAM_${randomUUID().replaceAll("-", "_")}`;
const configDirectory = mkdtempSync(join(tmpdir(), "nats-stream-test."));
const config = join(configDirectory, "envoy.json");
const subjectCount = 10_000;
const budgetSeconds = 2;
let serverUrl = "";
let expectedSequence = 0;
let nc: NatsConnection | undefined;

function docker(...args: string[]): string {
  const result = Bun.spawnSync(["docker", ...args], { cwd: root });
  const stdout = result.stdout.toString();
  const stderr = result.stderr.toString();
  if (result.exitCode !== 0) throw new Error(`docker ${args.join(" ")}: ${stdout}${stderr}`);
  return stdout;
}

async function startNats(): Promise<void> {
  docker(
    "run",
    "--detach",
    "--name",
    container,
    "--cpus",
    "0.5",
    "-p",
    "127.0.0.1::4222",
    "mirror.gcr.io/library/nats:2.10",
    "-js"
  );
  const endpoint = docker("port", container, "4222/tcp").trim();
  serverUrl = `nats://${endpoint}`;

  nc = await connect({
    servers: serverUrl,
    timeout: 10_000,
    waitOnFirstConnect: true,
    reconnectTimeWait: 50,
    maxReconnectAttempts: -1,
  });
}

function runLast(subject: string) {
  const env: Record<string, string | undefined> = {
    ...process.env,
    ENVOY_CONFIG_FILE: config,
  };
  delete env.NATS_NKEY_SEED;
  delete env.NATS_NKEY_SEED_FILE;
  const started = performance.now();
  const result = Bun.spawnSync(
    [
      "bun",
      join(import.meta.dir, "nats-stream.ts"),
      "last",
      serverUrl,
      stream,
      subject,
      String(budgetSeconds),
    ],
    { cwd: root, env }
  );
  return {
    elapsedMilliseconds: performance.now() - started,
    exitCode: result.exitCode,
    stderr: result.stderr.toString(),
    stdout: result.stdout.toString(),
  };
}

beforeAll(async () => {
  await startNats();
  mkdirSync(configDirectory, { recursive: true });
  writeFileSync(config, JSON.stringify({ envoy: { enabled: true, natsUrls: [serverUrl] } }));

  if (!nc) throw new Error("NATS connection was not established");
  const manager = await nc.jetstreamManager();
  await manager.streams.add({ name: stream, subjects: ["events.>"] });
  const js = nc.jetstream();
  for (let start = 0; start < subjectCount; start += 250) {
    const end = Math.min(start + 250, subjectCount);
    await Promise.all(
      Array.from({ length: end - start }, (_, index) =>
        js.publish(`events.subject.${start + index}`, new Uint8Array())
      )
    );
  }
  expectedSequence = (await js.publish("events.newest", new Uint8Array())).seq;
});

afterAll(async () => {
  await nc?.close();
  Bun.spawnSync(["docker", "rm", "-f", "-v", container], { cwd: root });
  rmSync(configDirectory, { recursive: true, force: true });
});

describe("nats-stream last", () => {
  test("reads the newest matching message within its budget without draining every subject", () => {
    const result = runLast("events.>");
    if (result.exitCode !== 0) {
      throw new Error(
        `last exited ${result.exitCode} after ${Math.round(result.elapsedMilliseconds)}ms: ${result.stderr}`
      );
    }
    expect(result.stderr).toBe("");
    expect(result.elapsedMilliseconds).toBeLessThanOrEqual(budgetSeconds * 1_000);
    expect(JSON.parse(result.stdout)).toEqual({
      subject: "events.newest",
      time: expect.any(String),
      seq: expectedSequence,
    });
    console.info(
      `nats-stream last ${subjectCount + 1} subjects: ${Math.round(result.elapsedMilliseconds)}ms`
    );
  });

  test("reports the existing no-message error for an unmatched filter", () => {
    const result = runLast("events.missing.>");
    expect(result.exitCode).toBe(1);
    expect(result.stdout).toBe("");
    expect(result.stderr).toBe(
      `nats-stream: stream ${stream} holds no message on events.missing.>\n`
    );
  });
});
