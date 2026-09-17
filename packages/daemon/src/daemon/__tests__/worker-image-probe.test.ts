import { afterEach, beforeEach, describe, expect, it, spyOn } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type ProbeRetryPolicy, SESSION_STORAGE_PROBE_MARK } from "../boot-probes";
import { DEFAULT_KUBERNETES_RESOURCES } from "../config";
import { parseImageDigestRef } from "../image-ref";
import { createK8sClient } from "../k8s-client";
import { LABEL_PROJECT, LEGION_BINARY, PROVIDERS_DIR } from "../k8s-manifests";
import {
  buildProbePodManifest,
  imageProbeCachePath,
  LABEL_PROBE,
  PROBE_CONTAINER,
  probePodName,
  type VerifyWorkerImageDeps,
  verifyWorkerImage,
} from "../worker-image-probe";
import { createFakeK8sApi, type FakeK8sApi } from "./fake-k8s-api";

const HEX = "0123456789abcdef".repeat(4);
const IMAGE = parseImageDigestRef(`ghcr.io/sjawhar/legion-worker:sha-1a2b3c@sha256:${HEX}`);
const POD = `legion-probe-demo-${HEX.slice(0, 12)}`;
const START = Date.parse("2026-09-13T12:00:00.000Z");
const CONTRACT = 7;
/** An image whose `legion` CLI predates the session-storage probe: a bare contract-confirming OK. */
const OK_LOG = `probe-image: OK (/opt/omp/bin/omp) daemon-api-version=${CONTRACT}\n`;
/** The OK line an image built at or after LEGION-80 prints (`cmdProbeImage`). */
const OK_LOG_PROBED = `probe-image: OK (/opt/omp/bin/omp) ${SESSION_STORAGE_PROBE_MARK} daemon-api-version=${CONTRACT}\n`;
/** One attempt, no backoff: the outcome of a single pod run is what most cases assert. */
const ONCE: ProbeRetryPolicy = { initialDelayMs: 10, maxDelayMs: 10, maxAttempts: 1 };

let stateDir: string;
beforeEach(async () => {
  stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-image-probe-"));
});
afterEach(async () => {
  await rm(stateDir, { recursive: true, force: true });
});

interface Harness {
  api: FakeK8sApi;
  logs: string[];
  sleeps: number[];
  /** Runs `verifyWorkerImage` with `onSleep` invoked on every poll sleep (the place a test
   * advances the pod's phase, as a real cluster would between two `GET`s). */
  run(
    onSleep?: (attemptSleeps: number) => void,
    overrides?: Partial<VerifyWorkerImageDeps>,
    retry?: ProbeRetryPolicy,
    signal?: AbortSignal
  ): Promise<void>;
}

function harness(): Harness {
  const api = createFakeK8sApi({ namespace: "legion", now: () => START });
  const logs: string[] = [];
  const sleeps: number[] = [];
  return {
    api,
    logs,
    sleeps,
    run: (onSleep, overrides = {}, retry = ONCE, signal) =>
      verifyWorkerImage(
        {
          client: createK8sClient({
            server: "https://fake",
            namespace: "legion",
            fetch: api.fetch,
          }),
          project: "demo",
          namespace: "legion",
          image: IMAGE,
          resources: DEFAULT_KUBERNETES_RESOURCES.small,
          scheduling: { nodeSelector: {}, tolerations: [] },
          stateDir,
          daemonApiVersion: CONTRACT,
          sessionStore: "pvc",
          now: () => START,
          log: (line) => {
            logs.push(line);
          },
          ...overrides,
        },
        {
          sleep: async (ms) => {
            sleeps.push(ms);
            onSleep?.(sleeps.length);
          },
          timeoutMs: 6_000,
          retry,
          signal,
        }
      ),
  };
}

function succeed(api: FakeK8sApi, log = OK_LOG): void {
  api.setPhase(POD, "Succeeded");
  api.logs.set(`${POD}/${PROBE_CONTAINER}`, log);
}

function requests(api: FakeK8sApi): string[][] {
  return api.requests.map((r) => [r.method, r.path]);
}

describe("imageProbeCachePath / probePodName", () => {
  it("names the cache file by the bare digest hex and the pod by the project and the digest's first 12 characters, within 63 characters", () => {
    expect(imageProbeCachePath(stateDir, IMAGE.digest)).toBe(
      path.join(stateDir, "image-probes", `${HEX}.json`)
    );
    expect(probePodName("demo", IMAGE.digest)).toBe(POD);
    expect(probePodName("Acme Widgets", IMAGE.digest)).toBe(
      `legion-probe-acme-widgets-${HEX.slice(0, 12)}`
    );
    const long = probePodName("p".repeat(80), IMAGE.digest);
    expect(long).toHaveLength(63);
    expect(long).toMatch(new RegExp(`^legion-probe-p+-[0-9a-f]{8}-${HEX.slice(0, 12)}$`));
    expect(probePodName("p".repeat(80), IMAGE.digest)).not.toBe(
      probePodName("q".repeat(80), IMAGE.digest)
    );
  });
});

describe("buildProbePodManifest", () => {
  it("builds a one-shot pod of the worker image running probe-image with the daemon's contract, mounting the providers Secret", () => {
    expect(
      buildProbePodManifest({
        project: "demo",
        namespace: "legion",
        image: IMAGE,
        daemonApiVersion: CONTRACT,
        resources: DEFAULT_KUBERNETES_RESOURCES.small,
        scheduling: { nodeSelector: {}, tolerations: [] },
      })
    ).toEqual({
      apiVersion: "v1",
      kind: "Pod",
      metadata: {
        name: POD,
        namespace: "legion",
        labels: { [LABEL_PROJECT]: "demo", [LABEL_PROBE]: "image" },
        annotations: { "karpenter.sh/do-not-disrupt": "true" },
      },
      spec: {
        restartPolicy: "Never",
        terminationGracePeriodSeconds: 5,
        automountServiceAccountToken: false,
        enableServiceLinks: false,
        securityContext: { runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000 },
        volumes: [
          {
            name: "providers",
            secret: { secretName: "legion-demo-providers", defaultMode: 0o440 },
          },
        ],
        containers: [
          {
            name: PROBE_CONTAINER,
            image: IMAGE.reference,
            command: [LEGION_BINARY, "probe-image", "--daemon-api-version", String(CONTRACT)],
            volumeMounts: [{ name: "providers", mountPath: PROVIDERS_DIR, readOnly: true }],
            resources: {
              requests: { cpu: "500m", memory: "1Gi", "ephemeral-storage": "2Gi" },
              limits: { cpu: "2", memory: "3Gi", "ephemeral-storage": "8Gi" },
            },
          },
        ],
      },
    });
  });

  it("places the probe on the configured worker pool", () => {
    expect(
      buildProbePodManifest({
        project: "demo",
        namespace: "legion",
        image: IMAGE,
        daemonApiVersion: CONTRACT,
        resources: DEFAULT_KUBERNETES_RESOURCES.small,
        scheduling: {
          nodeSelector: { "legion.dev/pool": "legion" },
          tolerations: [
            {
              key: "legion.dev/pool",
              operator: "Equal",
              value: "legion",
              effect: "NoSchedule",
            },
          ],
          priorityClassName: "legion",
        },
      })
    ).toMatchObject({
      metadata: { annotations: { "karpenter.sh/do-not-disrupt": "true" } },
      spec: {
        nodeSelector: { "legion.dev/pool": "legion" },
        tolerations: [
          {
            key: "legion.dev/pool",
            operator: "Equal",
            value: "legion",
            effect: "NoSchedule",
          },
        ],
        priorityClassName: "legion",
      },
    });
  });

  it("omits empty scheduling settings from the probe", () => {
    const manifest = buildProbePodManifest({
      project: "demo",
      namespace: "legion",
      image: IMAGE,
      daemonApiVersion: CONTRACT,
      resources: DEFAULT_KUBERNETES_RESOURCES.small,
      scheduling: { nodeSelector: {}, tolerations: [] },
    });
    expect(manifest.metadata.annotations).toEqual({ "karpenter.sh/do-not-disrupt": "true" });
    expect(manifest.spec).not.toHaveProperty("nodeSelector");
    expect(manifest.spec).not.toHaveProperty("tolerations");
    expect(manifest.spec).not.toHaveProperty("priorityClassName");
  });
});

describe("verifyWorkerImage", () => {
  it("creates the probe pod once, waits for it to succeed, quotes its OK line, writes the cache, and deletes the pod", async () => {
    const h = harness();
    await h.run(() => succeed(h.api));

    expect(requests(h.api)).toEqual([
      ["POST", "/pods"],
      ["GET", `/pods/${POD}`],
      ["GET", `/pods/${POD}`],
      ["GET", `/pods/${POD}/log?container=${PROBE_CONTAINER}&tailLines=50`],
      ["DELETE", `/pods/${POD}`],
    ]);
    const created = h.api.requests.find((r) => r.method === "POST");
    expect(created?.body).toEqual(
      buildProbePodManifest({
        project: "demo",
        namespace: "legion",
        image: IMAGE,
        daemonApiVersion: CONTRACT,
        resources: DEFAULT_KUBERNETES_RESOURCES.small,
        scheduling: { nodeSelector: {}, tolerations: [] },
      })
    );
    expect(h.api.pods.has(POD)).toBe(false);
    expect(h.sleeps).toEqual([2_000]);

    const cache = JSON.parse(await readFile(imageProbeCachePath(stateDir, IMAGE.digest), "utf8"));
    expect(cache).toEqual({
      digest: IMAGE.digest,
      daemonApiVersion: CONTRACT,
      probedAt: new Date(START).toISOString(),
    });
    expect(h.logs).toEqual([
      `[legion] worker image ${IMAGE.digest}: probe pod ${POD} passed: ${OK_LOG.trim()}`,
    ]);
  });

  it("refuses definitively, quoting the pod's log with both contract versions and the digest, when the pod fails; writes no cache; deletes the pod", async () => {
    const h = harness();
    const mismatch = `pi-legion-envoy at /home/legion/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json (package 0.9.1) speaks daemon API contract 6; this daemon requires ${CONTRACT}. Install the @sjawhar/pi-legion-envoy release built from this daemon's commit into the active profile.\n`;
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      await expect(
        h.run(() => {
          h.api.setPhase(POD, "Failed");
          h.api.logs.set(`${POD}/${PROBE_CONTAINER}`, mismatch);
        })
      ).rejects.toThrow(
        `[legion] worker image ${IMAGE.digest} failed its probe: pod ${POD} Failed — log tail: ${mismatch.trim()}`
      );
      expect(errorSpy).not.toHaveBeenCalled();
    } finally {
      errorSpy.mockRestore();
    }
    expect(h.api.pods.has(POD)).toBe(false);
    await expect(stat(imageProbeCachePath(stateDir, IMAGE.digest))).rejects.toThrow();
  });

  it("treats a pod that exits 0 without printing the OK line as a definitive failure", async () => {
    const h = harness();
    await expect(h.run(() => succeed(h.api, "something else\n"))).rejects.toThrow(
      `[legion] worker image ${IMAGE.digest} failed its probe: pod ${POD} Succeeded without printing probe-image: OK — log tail: something else`
    );
    await expect(stat(imageProbeCachePath(stateDir, IMAGE.digest))).rejects.toThrow();
  });

  it("treats an OK line that does not confirm this daemon's contract as definitive: an image whose CLI predates --daemon-api-version ignores the flag and prints a bare OK", async () => {
    const h = harness();
    await expect(
      h.run(() => succeed(h.api, "probe-image: OK (/opt/omp/bin/omp)\n"))
    ).rejects.toThrow(
      `[legion] worker image ${IMAGE.digest} failed its probe: pod ${POD} Succeeded without confirming daemon API contract ${CONTRACT} (its legion CLI predates the check) — log tail: probe-image: OK (/opt/omp/bin/omp)`
    );
    await expect(stat(imageProbeCachePath(stateDir, IMAGE.digest))).rejects.toThrow();
  });

  it("treats an OK line confirming another contract as definitive, naming both contracts", async () => {
    const h = harness();
    await expect(
      h.run(() =>
        succeed(h.api, `probe-image: OK (/opt/omp/bin/omp) daemon-api-version=${CONTRACT + 1}\n`)
      )
    ).rejects.toThrow(
      `[legion] worker image ${IMAGE.digest} failed its probe: pod ${POD} Succeeded but confirmed daemon API contract ${CONTRACT + 1}, this daemon requires ${CONTRACT} — log tail: probe-image: OK (/opt/omp/bin/omp) daemon-api-version=${CONTRACT + 1}`
    );
  });

  it("is transient while the pod is still Pending at the budget, naming the phase and the container's waiting reason, deleting the pod, and retrying with a fresh pod", async () => {
    const h = harness();
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      let attempt = 0;
      await h.run(
        (count) => {
          // Attempt 1: the image never pulls within the budget (timeoutMs 6 s / 2 s polls = 3
          // polls). The backoff sleep between attempts is the 4th sleep; attempt 2 succeeds.
          if (count === 1) {
            h.api.setPhase(POD, "Pending", {
              containerStatuses: [
                { name: PROBE_CONTAINER, state: { waiting: { reason: "ImagePullBackOff" } } },
              ],
            });
          }
          if (count === 4) attempt = 2;
          if (attempt === 2 && count >= 5) succeed(h.api);
        },
        {},
        { initialDelayMs: 10, maxDelayMs: 10, maxAttempts: 3 }
      );
      expect(errorSpy).toHaveBeenCalledTimes(1);
      expect(String(errorSpy.mock.calls[0]?.[0])).toBe(
        `[legion] worker image probe failed transiently (attempt 1/3); retrying in 0.01s: probe pod ${POD} still Pending after 6 s (container ${PROBE_CONTAINER} waiting: ImagePullBackOff)`
      );
    } finally {
      errorSpy.mockRestore();
    }
    // 3 polls, the backoff, then attempt 2's poll(s).
    expect(h.sleeps.slice(0, 4)).toEqual([2_000, 2_000, 2_000, 10]);
    expect(requests(h.api).filter(([method]) => method === "POST")).toHaveLength(2);
    expect(requests(h.api).filter(([method]) => method === "DELETE")).toHaveLength(2);
    expect(h.api.pods.has(POD)).toBe(false);
    await expect(readFile(imageProbeCachePath(stateDir, IMAGE.digest), "utf8")).resolves.toContain(
      IMAGE.digest
    );
  });

  it.each([
    "InvalidImageName",
    "ErrImageNeverPull",
  ])("refuses definitively as soon as the container waits with %s", async (reason) => {
    const h = harness();
    await expect(
      h.run(() => {
        h.api.setPhase(POD, "Pending", {
          containerStatuses: [{ name: PROBE_CONTAINER, state: { waiting: { reason } } }],
        });
      })
    ).rejects.toThrow(
      `[legion] worker image ${IMAGE.digest} failed its probe: pod ${POD} Pending, container ${PROBE_CONTAINER} waiting: ${reason}`
    );
    // Refused at the first poll that showed the reason: one sleep, not the whole budget.
    expect(h.sleeps).toEqual([2_000]);
    expect(h.api.pods.has(POD)).toBe(false);
  });

  it("passes from the cache without any API call when the digest was probed at the current contract", async () => {
    const h = harness();
    const file = imageProbeCachePath(stateDir, IMAGE.digest);
    await mkdir(path.dirname(file), { recursive: true });
    const probedAt = "2026-09-12T08:00:00.000Z";
    await writeFile(
      file,
      JSON.stringify({ digest: IMAGE.digest, daemonApiVersion: CONTRACT, probedAt })
    );
    await h.run();
    expect(h.api.requests).toEqual([]);
    expect(h.logs).toEqual([
      `[legion] worker image ${IMAGE.digest} passed its probe at ${probedAt} (daemon API contract ${CONTRACT}); reusing ${file}`,
    ]);
  });

  it("runs the pod and rewrites the cache when the cached contract is not the daemon's", async () => {
    const h = harness();
    const file = imageProbeCachePath(stateDir, IMAGE.digest);
    await mkdir(path.dirname(file), { recursive: true });
    await writeFile(
      file,
      JSON.stringify({
        digest: IMAGE.digest,
        daemonApiVersion: CONTRACT - 1,
        probedAt: "2026-09-12T08:00:00.000Z",
      })
    );
    await h.run(() => succeed(h.api));
    expect(requests(h.api)[0]).toEqual(["POST", "/pods"]);
    expect(JSON.parse(await readFile(file, "utf8"))).toEqual({
      digest: IMAGE.digest,
      daemonApiVersion: CONTRACT,
      probedAt: new Date(START).toISOString(),
    });
  });

  describe("session_store", () => {
    const cacheFile = () => imageProbeCachePath(stateDir, IMAGE.digest);
    const seedCache = async (entry: Record<string, unknown>) => {
      await mkdir(path.dirname(cacheFile()), { recursive: true });
      await writeFile(
        cacheFile(),
        JSON.stringify({
          digest: IMAGE.digest,
          daemonApiVersion: CONTRACT,
          probedAt: "2026-09-12T08:00:00.000Z",
          ...entry,
        })
      );
    };

    it("under postgres, passes on a probe pod whose log carries the session-storage marker and records sessionStorageProbed in the cache", async () => {
      const h = harness();
      await h.run(() => succeed(h.api, OK_LOG_PROBED), { sessionStore: "postgres" });
      expect(JSON.parse(await readFile(cacheFile(), "utf8"))).toEqual({
        digest: IMAGE.digest,
        daemonApiVersion: CONTRACT,
        probedAt: new Date(START).toISOString(),
        sessionStorageProbed: true,
      });
    });

    it("under postgres, refuses definitively a Succeeded probe pod whose log lacks the marker, naming session_store, writing no cache, and deleting the pod", async () => {
      const h = harness();
      await expect(
        h.run(() => succeed(h.api, OK_LOG), { sessionStore: "postgres" })
      ).rejects.toThrow(
        `[legion] worker image ${IMAGE.digest} failed its probe: pod ${POD} Succeeded without printing ${SESSION_STORAGE_PROBE_MARK}, which session_store: postgres requires (its Oh My Pi or legion CLI predates the session-storage setting) — log tail: ${OK_LOG.trim()}`
      );
      await expect(stat(cacheFile())).rejects.toThrow();
      expect(requests(h.api).at(-1)).toEqual(["DELETE", `/pods/${POD}`]);
    });

    it("under pvc, still records a marker the image printed, so the same pass is reusable under postgres later", async () => {
      const h = harness();
      await h.run(() => succeed(h.api, OK_LOG_PROBED));
      expect(JSON.parse(await readFile(cacheFile(), "utf8"))).toMatchObject({
        sessionStorageProbed: true,
      });
      const again = harness();
      await again.run(undefined, { sessionStore: "postgres" });
      expect(again.api.requests).toEqual([]);
      expect(again.logs.at(-1)).toContain(`reusing ${cacheFile()}`);
    });

    it("under postgres, ignores a cached pass that records no session-storage probe, runs the pod, and rewrites the cache", async () => {
      await seedCache({});
      const h = harness();
      await h.run(() => succeed(h.api, OK_LOG_PROBED), { sessionStore: "postgres" });
      expect(h.logs[0]).toBe(
        `[legion] ignoring worker image probe cache ${cacheFile()}: it records no session-storage probe, and this daemon runs session_store: postgres`
      );
      expect(requests(h.api)[0]).toEqual(["POST", "/pods"]);
      expect(JSON.parse(await readFile(cacheFile(), "utf8"))).toMatchObject({
        sessionStorageProbed: true,
      });
    });
  });

  it.each([
    ["not JSON", "{nope"],
    ["missing a field", JSON.stringify({ digest: IMAGE.digest, daemonApiVersion: CONTRACT })],
    [
      "carrying an unknown field",
      JSON.stringify({
        digest: IMAGE.digest,
        daemonApiVersion: CONTRACT,
        probedAt: "2026-09-12T08:00:00.000Z",
        extra: 1,
      }),
    ],
    [
      "naming another digest",
      JSON.stringify({
        digest: `sha256:${"f".repeat(64)}`,
        daemonApiVersion: CONTRACT,
        probedAt: "2026-09-12T08:00:00.000Z",
      }),
    ],
  ])("logs a cache file %s, naming it, then runs the pod and rewrites it", async (_case, text) => {
    const h = harness();
    const file = imageProbeCachePath(stateDir, IMAGE.digest);
    await mkdir(path.dirname(file), { recursive: true });
    await writeFile(file, text);
    await h.run(() => succeed(h.api));
    expect(h.logs[0]).toStartWith(`[legion] ignoring worker image probe cache ${file}: `);
    expect(requests(h.api)[0]).toEqual(["POST", "/pods"]);
    expect(JSON.parse(await readFile(file, "utf8"))).toMatchObject({
      digest: IMAGE.digest,
      daemonApiVersion: CONTRACT,
    });
  });

  it("deletes a leftover probe pod from a crashed boot before creating its own", async () => {
    const h = harness();
    h.api.pods.set(POD, {
      metadata: {
        name: POD,
        namespace: "legion",
        uid: "old",
        labels: { "legion.dev/project": "demo" },
      },
      spec: {},
      status: { phase: "Succeeded" },
    });
    await h.run(() => succeed(h.api));
    expect(requests(h.api).slice(0, 5)).toEqual([
      ["POST", "/pods"],
      ["GET", `/pods/${POD}`],
      ["DELETE", `/pods/${POD}`],
      ["GET", `/pods/${POD}`],
      ["POST", "/pods"],
    ]);
    expect(h.logs[0]).toBe(
      `[legion] deleting leftover probe pod ${POD} (from an earlier boot) before probing worker image ${IMAGE.digest}`
    );
    expect(h.api.pods.has(POD)).toBe(false);
  });

  it("still deletes the pod, and surfaces the failure, when its log cannot be read", async () => {
    const h = harness();
    h.api.failNext("GET", new RegExp(`/pods/${POD}/log`), 500);
    await expect(
      h.run(() => {
        h.api.setPhase(POD, "Failed");
      })
    ).rejects.toThrow(
      `[legion] worker image ${IMAGE.digest} failed its probe: pod ${POD} Failed — log tail: (log unavailable: `
    );
    expect(h.api.pods.has(POD)).toBe(false);
  });

  it("refuses, without deleting anything, when a pod of its name belongs to another project or to no Legion project", async () => {
    for (const labels of [{ "legion.dev/project": "other" }, undefined]) {
      const h = harness();
      h.api.pods.set(POD, {
        metadata: { name: POD, namespace: "legion", uid: "theirs", labels },
        spec: {},
        status: { phase: "Running" },
      });
      await expect(h.run()).rejects.toThrow(
        `[legion] worker image ${IMAGE.digest} failed its probe: probe pod ${POD} already exists and belongs to ${labels ? "project other" : "no Legion project"}, not demo; not deleting it`
      );
      expect(requests(h.api)).toEqual([
        ["POST", "/pods"],
        ["GET", `/pods/${POD}`],
      ]);
      expect(h.api.pods.get(POD)?.metadata.uid).toBe("theirs");
      expect(h.logs).toEqual([]);
    }
  });

  it("treats a pod that vanishes mid-poll as transient — the next attempt creates it again — never as a boot failure", async () => {
    const h = harness();
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      let attempt = 1;
      await h.run(
        (count) => {
          if (attempt === 1 && count === 1) {
            h.api.pods.delete(POD);
            attempt = 2;
          } else if (attempt === 2 && count >= 3) {
            succeed(h.api);
          }
        },
        {},
        { initialDelayMs: 10, maxDelayMs: 10, maxAttempts: 2 }
      );
      expect(errorSpy.mock.calls.map((call) => call[0])).toEqual([
        `[legion] worker image probe failed transiently (attempt 1/2); retrying in 0.01s: probe pod ${POD} vanished before it finished`,
      ]);
      expect(requests(h.api).filter((r) => r[0] === "POST")).toHaveLength(2);
    } finally {
      errorSpy.mockRestore();
    }
  });

  it.each([
    [500, false],
    [400, true],
    [401, true],
    [403, true],
    [404, true],
    [422, true],
  ])("classifies an API failure on create (HTTP %i) as an outcome: definitive=%s — a 404 here is a namespace that does not exist", async (status, definitive) => {
    const h = harness();
    h.api.failNext("POST", /\/pods$/, status);
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      const run = h.run(undefined, {}, { initialDelayMs: 10, maxDelayMs: 10, maxAttempts: 1 });
      if (definitive) {
        await expect(run).rejects.toThrow(
          new RegExp(
            `^\\[legion\\] worker image ${IMAGE.digest} failed its probe: creating probe pod ${POD}: .*${status}`
          )
        );
      } else {
        await expect(run).rejects.toThrow(
          new RegExp(
            `^\\[legion\\] worker image probe never completed within its retry budget \\(1 attempts\\) for ${IMAGE.digest}: creating probe pod ${POD}: .*${status}`
          )
        );
      }
    } finally {
      errorSpy.mockRestore();
    }
  });

  it("gives the pod up and deletes it when the daemon aborts mid-wait, with no transient log", async () => {
    const h = harness();
    const controller = new AbortController();
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      await expect(
        h.run(
          () => controller.abort(),
          {},
          { initialDelayMs: 10, maxDelayMs: 10 },
          controller.signal
        )
      ).rejects.toThrow("worker image probe abandoned");
      expect(errorSpy).not.toHaveBeenCalled();
    } finally {
      errorSpy.mockRestore();
    }
    expect(h.api.pods.has(POD)).toBe(false);
    expect(h.sleeps).toEqual([2_000]);
  });
});
