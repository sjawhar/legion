import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { connect, type NatsConnection } from "nats";
import { readLast } from "./nats-stream";

const root = join(import.meta.dir, "..", "..", "..");
const container = `legion-nats-stream-${randomUUID()}`;
const stream = `NATS_STREAM_${randomUUID().replaceAll("-", "_")}`;
const configDirectory = mkdtempSync(join(tmpdir(), "nats-stream-test."));
const config = join(configDirectory, "envoy.json");
const subjectCount = 10_000;
const publishBatchSize = 25;
const publishTimeoutMilliseconds = 30_000;
const budgetSeconds = 5;
const subsecondBudgetSeconds = 0.1;
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

function runLast(subject: string, seconds = budgetSeconds) {
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
      String(seconds),
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

async function runReadLast(subject: string, seconds: number) {
  const client = await connect({ servers: serverUrl, timeout: 10_000, maxReconnectAttempts: 0 });
  const started = performance.now();
  try {
    const result = await readLast(client, stream, subject, seconds);
    return { elapsedMilliseconds: performance.now() - started, result };
  } finally {
    void client.close();
  }
}

function expectWithinBudget(elapsedMilliseconds: number, seconds: number) {
  expect(elapsedMilliseconds).toBeLessThanOrEqual(seconds * 1_000 + 500);
}

async function cleanup(): Promise<void> {
  try {
    await nc?.close();
  } finally {
    Bun.spawnSync(["docker", "rm", "-f", "-v", container], { cwd: root });
    rmSync(configDirectory, { recursive: true, force: true });
  }
}

beforeAll(async () => {
  try {
    await startNats();
    mkdirSync(configDirectory, { recursive: true });
    writeFileSync(config, JSON.stringify({ envoy: { enabled: true, natsUrls: [serverUrl] } }));

    if (!nc) throw new Error("NATS connection was not established");
    const manager = await nc.jetstreamManager();
    await manager.streams.add({ name: stream, subjects: ["events.>"] });
    const js = nc.jetstream();
    for (let start = 0; start < subjectCount; start += publishBatchSize) {
      const end = Math.min(start + publishBatchSize, subjectCount);
      await Promise.all(
        Array.from({ length: end - start }, (_, index) =>
          js.publish(`events.subject.${start + index}`, new Uint8Array(), {
            timeout: publishTimeoutMilliseconds,
          })
        )
      );
    }
    expectedSequence = (
      await js.publish("events.newest", new Uint8Array(), { timeout: publishTimeoutMilliseconds })
    ).seq;
  } catch (error) {
    await cleanup();
    throw error;
  }
}, 120_000);

afterAll(cleanup, 120_000);

describe("nats-stream last", () => {
  test("reads the newest matching message without draining every subject", () => {
    const result = runLast("events.>");
    if (result.exitCode !== 0) {
      throw new Error(
        `last exited ${result.exitCode} after ${Math.round(result.elapsedMilliseconds)}ms: ${result.stderr}`
      );
    }
    expect(result.stderr).toBe("");
    expect(JSON.parse(result.stdout)).toEqual({
      subject: "events.newest",
      time: expect.any(String),
      seq: expectedSequence,
    });
    console.info(
      `nats-stream last ${subjectCount + 1} subjects: ${Math.round(result.elapsedMilliseconds)}ms`
    );
  }, 120_000);

  test("reports the existing no-message error for an unmatched filter", () => {
    const result = runLast("events.missing.>", subsecondBudgetSeconds);
    expect(result.exitCode).toBe(1);
    expect(result.stdout).toBe("");
    expect(result.stderr).toBe(
      `nats-stream: stream ${stream} holds no message on events.missing.>\n`
    );
  }, 120_000);

  test("returns the newest matching message within its budget", async () => {
    const read = await runReadLast("events.>", budgetSeconds);
    expect(read.result).toEqual({
      message: {
        subject: "events.newest",
        time: expect.any(String),
        seq: expectedSequence,
      },
      timedOut: false,
    });
    expectWithinBudget(read.elapsedMilliseconds, budgetSeconds);
  }, 120_000);

  test("finishes an unmatched sub-second read at its deadline", async () => {
    const read = await runReadLast("events.missing.>", subsecondBudgetSeconds);
    expect(read.result).toEqual({ message: undefined, timedOut: true });
    expectWithinBudget(read.elapsedMilliseconds, subsecondBudgetSeconds);
  }, 120_000);
});
