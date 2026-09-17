import { describe, expect, it } from "bun:test";
import type { IssueKey, LegionRole } from "@legion/contracts";
import { DEFAULT_KUBERNETES_RESOURCES, DEFAULT_ROLE_PROFILES, type SessionStore } from "../config";
import { parseImageDigestRef } from "../image-ref";
import { createK8sClient, K8sApiError, type K8sPod } from "../k8s-client";
import {
  BOOT_DIR,
  BOOT_TOKEN_KEY,
  buildPodManifest,
  GRANT_DIR,
  IMAGE_PATH,
  INIT_CONTAINER,
  LABEL_ISSUE,
  LABEL_PROJECT,
  LABEL_ROLE,
  LABEL_TREE,
  MAIN_CONTAINER,
  OMP_SESSIONS_DIR,
  POD_CREDENTIAL_HELPER,
  PROVIDERS_DIR,
  podLabels,
  podSelector,
  SESSION_SQL_DSN_FILE_VARIABLE,
  SESSION_STORAGE_VARIABLE,
  TREE_MOUNT,
  UNREFERENCED_SINCE_ANNOTATION,
} from "../k8s-manifests";
import {
  type ExternalControllerLocator,
  type Locator,
  ProcessStopFailed,
  type SpawnSpec,
} from "../runtime";
import {
  CONTROLLER_HEARTBEAT_MS,
  controllerLivenessMs,
  KubernetesRuntime,
  PVC_RETENTION_MS,
  type RoleLookupFetch,
} from "../runtime-kubernetes";
import type { WorkerRpcClient } from "../worker-rpc";
import { createFakeK8sApi, type FakeK8sEvent } from "./fake-k8s-api";
import { fakeWorkerRpcClient } from "./fake-runtime";

const issue: IssueKey = "LEGION-42";
const IMAGE = `ghcr.io/sjawhar/legion-worker@sha256:${"a".repeat(64)}`;
const START = Date.parse("2026-09-13T00:00:00.000Z");
/** Distinct from every flag and key name (`--boot-token-file`, `LEGION_BOOT_TOKEN`), so a leak
 * check on the request log finds only a real value. */
const BOOT_TOKEN = "bt-a3f9c1d2e";
const PROVISION_TOKEN = "ghs-inst-7f2b";
const DAY_MS = 24 * 60 * 60 * 1000;

interface HarnessOptions {
  forbidden?: string[];
  readFile?: (file: string) => Promise<string>;
  deploymentInstructionsFile?: string;
  sleep?: (ms: number) => Promise<void>;
  provisioningToken?: () => Promise<string>;
  workerBootTimeoutMs?: number;
  workerBootRegistrationDeadlineIntervals?: number;
  sessionStore?: SessionStore;
  envoyToken?: string;
  /** The Envoy listener the operator-launched controller probe reads; every call is recorded. */
  envoyFetch?: RoleLookupFetch;
}

function harness(options: HarnessOptions = {}) {
  let clock = START;
  const api = createFakeK8sApi({
    namespace: "legion",
    now: () => clock,
    forbidden: options.forbidden,
  });
  const registrations = new Map<string, WorkerRpcClient>();
  const awaits: Array<{ token: string; timeoutMs: number }> = [];
  const logs: string[] = [];
  const sleeps: number[] = [];
  const runtime = new KubernetesRuntime({
    project: "omp",
    config: {
      namespace: "legion",
      image: parseImageDigestRef(IMAGE),
      treeVolume: "20Gi",
      sessionStore: options.sessionStore ?? { kind: "pvc" },
      resources: DEFAULT_KUBERNETES_RESOURCES,
      roleProfiles: DEFAULT_ROLE_PROFILES,
    },
    client: createK8sClient({ server: "https://fake", namespace: "legion", fetch: api.fetch }),
    listener: () => ({
      registrations,
      awaitRegistration: async (token, timeoutMs) => {
        awaits.push({ token, timeoutMs });
        const client = registrations.get(token);
        if (!client) throw new Error(`no stream for ${token}`);
        return client;
      },
    }),
    repoForIssue: (candidate) =>
      candidate.startsWith("AGENTC-") ? "trajectory-labs-pbc/agent-c" : "acme/widgets",
    provisioningToken: options.provisioningToken ?? (async () => PROVISION_TOKEN),
    daemonUrl: "http://172.18.0.1:19370",
    workerStreamPort: 19371,
    workerBootTimeoutMs: options.workerBootTimeoutMs ?? 120_000,
    workerBootRegistrationDeadlineIntervals: options.workerBootRegistrationDeadlineIntervals ?? 3,
    workerStopTimeoutMs: 10_000,
    workerRpcTimeoutMs: () => 5_000,
    deploymentInstructionsFile: options.deploymentInstructionsFile,
    readFile: options.readFile ?? (async (file) => `text of ${file}`),
    envoyUrl: "http://envoy.test:9020",
    envoyToken: options.envoyToken,
    fetch: options.envoyFetch,
    now: () => clock,
    sleep:
      options.sleep ??
      (async (ms) => {
        sleeps.push(ms);
      }),
    log: (line) => logs.push(line),
  });
  return {
    api,
    runtime,
    registrations,
    awaits,
    logs,
    sleeps,
    advance: (ms: number) => {
      clock += ms;
    },
    now: () => clock,
  };
}

/** The env a worker spec carries out of `ProcessManager.launchWorker` today: the daemon-path
 * values the runtime must re-point, and the pass-throughs it must leave alone. */
const DAEMON_ENV = {
  LEGION_TREE: issue,
  LEGION_ISSUE: issue,
  LEGION_ROLE: "tester",
  LEGION_GENERATION: "1",
  LEGION_DAEMON_URL: "http://172.18.0.1:19370",
  LEGION_PROJECT: "omp",
  LEGION_STATE_DIR: "/state",
  LEGION_CREDENTIAL_HELPER: "!/opt/legion/bun /cli credential",
  ENVOY_NATS_URL: "nats://172.18.0.1:14222",
  ENVOY_URL: "http://172.18.0.1:19020",
  GIT_CONFIG_COUNT: "0",
  GIT_TERMINAL_PROMPT: "0",
  LEGION_GRANT_FILE: "/state/secrets/legion-omp-legion-42-tester-grant",
  GH_CONFIG_DIR: "/state/gh",
  PATH: "/state/worker-bin:/full/bin",
  GH_TOKEN: "",
  GITHUB_TOKEN: "",
  GH_HOST: "",
  DISPATCH_URL: "http://172.18.0.1:8766",
  DISPATCH_TOKEN_FILE: "/state/secrets/dispatch-token",
  UNSET: undefined,
};

/** Every re-pointed key, with the consumer inside the pod each value serves. */
const POD_ENV = {
  LEGION_TREE: issue,
  LEGION_ISSUE: issue,
  LEGION_ROLE: "tester",
  LEGION_GENERATION: "1",
  LEGION_DAEMON_URL: "http://172.18.0.1:19370",
  LEGION_PROJECT: "omp",
  LEGION_STATE_DIR: TREE_MOUNT,
  LEGION_CREDENTIAL_HELPER: POD_CREDENTIAL_HELPER,
  ENVOY_NATS_URL: "nats://172.18.0.1:14222",
  ENVOY_URL: "http://172.18.0.1:19020",
  GIT_CONFIG_COUNT: "0",
  GIT_TERMINAL_PROMPT: "0",
  LEGION_GRANT_FILE: `${GRANT_DIR}/legion-omp-legion-42-tester-grant`,
  GH_CONFIG_DIR: `${TREE_MOUNT}/gh`,
  PATH: `${TREE_MOUNT}/worker-bin:${IMAGE_PATH}`,
  GH_TOKEN: "",
  GITHUB_TOKEN: "",
  GH_HOST: "",
  DISPATCH_URL: "http://172.18.0.1:8766",
  DISPATCH_TOKEN_FILE: `${PROVIDERS_DIR}/DISPATCH_TOKEN`,
  LEGION_WORKSPACE: "/legion/workspaces/acme/widgets/legion-42",
  LEGION_BOOT_TOKEN_FILE: `${BOOT_DIR}/${BOOT_TOKEN_KEY}`,
};

function workerSpec(overrides: Partial<SpawnSpec> = {}): SpawnSpec {
  return {
    issue,
    tree: issue,
    generation: 1,
    role: "tester",
    env: DAEMON_ENV,
    launch: { promptPath: "/roles/tester.md", addressingPrompt: "address tester" },
    secrets: { LEGION_BOOT_TOKEN: BOOT_TOKEN },
    ...overrides,
  };
}

function seedPod(
  api: ReturnType<typeof createFakeK8sApi>,
  name: string,
  input: {
    role?: LegionRole;
    generation?: number;
    createdAt: number;
    phase?: NonNullable<K8sPod["status"]>["phase"];
    status?: Partial<NonNullable<K8sPod["status"]>>;
    labels?: Record<string, string>;
    finalizers?: string[];
    uid?: string;
  }
): K8sPod {
  const pod: K8sPod = {
    metadata: {
      name,
      namespace: "legion",
      uid: input.uid ?? `uid-${name}`,
      labels:
        input.labels ??
        podLabels({
          project: "omp",
          tree: issue,
          issue,
          role: input.role ?? "tester",
          generation: input.generation ?? 1,
        }),
      creationTimestamp: new Date(input.createdAt).toISOString(),
      ...(input.finalizers ? { finalizers: input.finalizers } : {}),
    },
    spec: {},
    status: { phase: input.phase ?? "Running", ...input.status },
  };
  api.pods.set(name, pod);
  return pod;
}

function locatorFor(pod: K8sPod, roleToken = "legion-omp-legion-42-tester"): Locator {
  return {
    runtime: "kubernetes",
    namespace: "legion",
    podName: pod.metadata.name,
    podUid: pod.metadata.uid ?? "",
    pvcName: "legion-legion-42",
    roleToken,
  };
}

interface ContainerView {
  command: string[];
  image: string;
  env: Array<{ name: string; value: string }>;
  resources: unknown;
  volumeMounts: Array<{ name: string; mountPath: string; subPath?: string; readOnly?: boolean }>;
}

/** The pod's single init container (`workspace-init`). */
function initContainer(pod: K8sPod | undefined): ContainerView & { name: string } {
  const initContainers = pod?.spec.initContainers;
  if (!Array.isArray(initContainers) || initContainers.length !== 1) {
    throw new Error("pod has no single init container");
  }
  const init: ContainerView & { name: string } = initContainers[0];
  return init;
}

const envOf = (container: ContainerView) =>
  Object.fromEntries(container.env.map((entry) => [entry.name, entry.value]));

const POSTGRES: SessionStore = { kind: "postgres", dsnSecretKey: "SESSION_DSN" };
const SESSION_VARIABLES = [
  { name: SESSION_STORAGE_VARIABLE, value: "sql" },
  { name: SESSION_SQL_DSN_FILE_VARIABLE, value: `${PROVIDERS_DIR}/SESSION_DSN` },
];

/** The main container of a pod the fake stored -- `buildPodManifest`'s output verbatim, whose
 * first `containers[]` entry is the worker. */
function mainContainer(pod: K8sPod | undefined): ContainerView {
  const containers = pod?.spec.containers;
  if (!Array.isArray(containers) || containers.length === 0) {
    throw new Error("pod has no containers");
  }
  const container: ContainerView = containers[0];
  return container;
}

/** The `gracePeriodSeconds` a recorded DELETE request carried. */
function graceOf(body: unknown): number | undefined {
  if (body && typeof body === "object" && "gracePeriodSeconds" in body) {
    return typeof body.gracePeriodSeconds === "number" ? body.gracePeriodSeconds : undefined;
  }
  return undefined;
}

const requestPaths = (api: ReturnType<typeof createFakeK8sApi>) =>
  api.requests.map((request) => [request.method, request.path]);

const previousPodSelector = (role: LegionRole) =>
  encodeURIComponent(
    podSelector({ [LABEL_PROJECT]: "omp", [LABEL_ISSUE]: issue, [LABEL_ROLE]: role })
  );

describe("KubernetesRuntime.spawn", () => {
  it("creates the PVC, the Secret, and the Pod in that order with the exact manifests, re-pointing every daemon-path env value and leaking no secret outside the Secret create", async () => {
    const { api, runtime } = harness();
    const locator = await runtime.spawn("worker", workerSpec());

    expect(requestPaths(api)).toEqual([
      ["GET", "/persistentvolumeclaims/legion-legion-42"],
      ["POST", "/persistentvolumeclaims"],
      ["GET", `/pods?labelSelector=${previousPodSelector("tester")}`],
      // A same-name Secret left by a crashed attempt is reclaimed before the create (404 here).
      ["DELETE", "/secrets/legion-legion-42-tester-g1"],
      ["POST", "/secrets"],
      ["POST", "/pods"],
    ]);
    const podCreate = api.requests.find((r) => r.method === "POST" && r.path === "/pods");
    expect(podCreate?.body).toEqual(
      buildPodManifest({
        project: "omp",
        tree: issue,
        issue,
        role: "tester",
        generation: 1,
        namespace: "legion",
        image: IMAGE,
        pvcName: "legion-legion-42",
        podName: "legion-legion-42-tester-g1",
        secretName: "legion-legion-42-tester-g1",
        secretKeys: [BOOT_TOKEN_KEY],
        resources: DEFAULT_KUBERNETES_RESOURCES.large,
        env: POD_ENV,
        workspaceDir: "/legion/workspaces/acme/widgets/legion-42",
        repo: "acme/widgets",
        shimEndpoint: "tcp://172.18.0.1:19371",
        ompArgv: [
          "omp",
          "--mode",
          "rpc",
          "--append-system-prompt",
          "text of /roles/tester.md\n\naddress tester",
        ],
        terminationGracePeriodSeconds: 10,
        // (120 s x 3 intervals) + one interval: the daemon's boot deadline plus its margin.
        workspaceInitLockWaitSeconds: 480,
      })
    );
    const secretCreate = api.requests.find((r) => r.method === "POST" && r.path === "/secrets");
    expect(secretCreate?.body).toEqual({
      apiVersion: "v1",
      kind: "Secret",
      metadata: {
        name: "legion-legion-42-tester-g1",
        labels: podLabels({ project: "omp", tree: issue, issue, role: "tester", generation: 1 }),
      },
      type: "Opaque",
      stringData: { LEGION_BOOT_TOKEN: BOOT_TOKEN, LEGION_PROVISION_TOKEN: PROVISION_TOKEN },
    });
    const everythingElse = JSON.stringify(api.requests.filter((r) => r !== secretCreate));
    expect(everythingElse).not.toContain(BOOT_TOKEN);
    expect(everythingElse).not.toContain(PROVISION_TOKEN);

    const createdUid = api.pods.get("legion-legion-42-tester-g1")?.metadata.uid;
    expect(createdUid).toBeDefined();
    expect(locator).toEqual({
      runtime: "kubernetes",
      namespace: "legion",
      podName: "legion-legion-42-tester-g1",
      podUid: createdUid ?? "",
      pvcName: "legion-legion-42",
      roleToken: "legion-omp-legion-42-tester",
    });
    expect(api.pvcs.get("legion-legion-42")?.spec).toEqual({
      accessModes: ["ReadWriteOnce"],
      resources: { requests: { storage: "20Gi" } },
    });
  });

  it("provisions an AGENTC issue from its project's repository", async () => {
    const agentc = "AGENTC-9" as IssueKey;
    const { api, runtime } = harness();
    await runtime.spawn(
      "worker",
      workerSpec({
        issue: agentc,
        tree: agentc,
        env: { ...DAEMON_ENV, LEGION_TREE: agentc, LEGION_ISSUE: agentc },
      })
    );

    expect(initContainer(api.pods.get("legion-agentc-9-tester-g1")).command).toContain(
      "trajectory-labs-pbc/agent-c"
    );
  });

  it("spawns a tree root as its architect pod with LEGION_ROOT_WORKSPACE and the architect claim token", async () => {
    const { api, runtime } = harness();
    const locator = await runtime.spawn(
      "root",
      workerSpec({
        role: "architect",
        env: { ...DAEMON_ENV, LEGION_ROLE: "architect" },
        launch: { promptPath: "/roles/architect-root.md", addressingPrompt: "address architect" },
      })
    );
    expect(locator).toMatchObject({
      podName: "legion-legion-42-architect-g1",
      roleToken: "legion-omp-legion-42-architect",
    });
    const pod = api.pods.get("legion-legion-42-architect-g1");
    const container = mainContainer(pod);
    const env = Object.fromEntries(container.env.map((entry) => [entry.name, entry.value]));
    expect(env.LEGION_ROOT_WORKSPACE).toBe("/legion/workspaces/acme/widgets/legion-42");
    expect(env.LEGION_WORKSPACE).toBeUndefined();
    expect(container.resources).toEqual({
      requests: { cpu: "500m", memory: "1Gi", "ephemeral-storage": "2Gi" },
      limits: { cpu: "2", memory: "3Gi", "ephemeral-storage": "8Gi" },
    });
  });

  it("passes --resume as the second OMP argument, tells the init container where the session file is on the volume, and logs the resume when a session file is recorded", async () => {
    const { api, runtime, logs } = harness();
    const file = "/home/legion/.omp/profiles/legion/agent/sessions/s.jsonl";
    await runtime.spawn(
      "worker",
      workerSpec({ launch: { promptPath: "/roles/tester.md", resumeSessionFile: file } })
    );
    const pod = api.pods.get("legion-legion-42-tester-g1");
    const command = mainContainer(pod).command;
    const omp = command.slice(command.indexOf("--") + 1);
    expect(omp.slice(0, 2)).toEqual(["omp", `--resume=${file}`]);
    // The init container mounts the whole volume at /legion; the main container's sessions
    // directory is its `sessions` subPath -- so it checks the same file there, and fails the pod
    // (a launch failure, as tmux's stat is) rather than let OMP run as a fresh agent.
    expect(initContainer(pod).env).toContainEqual({
      name: "LEGION_RESUME_SESSION_FILE",
      value: "/legion/sessions/s.jsonl",
    });
    expect(logs).toContain(`[legion] respawning ${issue} by resuming OMP session ${file}`);
  });

  it("refuses, before any API call, to resume a recorded session file that is not under the pod's sessions directory", async () => {
    const { api, runtime } = harness();
    await expect(
      runtime.spawn(
        "worker",
        workerSpec({
          launch: { promptPath: "/roles/tester.md", resumeSessionFile: "/state/sessions/s.jsonl" },
        })
      )
    ).rejects.toThrow(
      "recorded OMP session file /state/sessions/s.jsonl is not under /home/legion/.omp/profiles/legion/agent/sessions, the only directory a pod persists sessions to; it cannot be resumed on this runtime"
    );
    expect(api.requests).toEqual([]);
  });

  describe("session_store: postgres", () => {
    it("adds OMP_SESSION_STORAGE=sql and OMP_SESSION_SQL_DSN_FILE under the providers mount to every pod of a tree, and to neither init container", async () => {
      const pvc = harness();
      await pvc.runtime.spawn("worker", workerSpec());
      const { api, runtime } = harness({ sessionStore: POSTGRES });
      await runtime.spawn("worker", workerSpec());
      await runtime.spawn(
        "root",
        workerSpec({
          role: "architect",
          env: { ...DAEMON_ENV, LEGION_ROLE: "architect" },
          launch: { promptPath: "/roles/architect-root.md", addressingPrompt: "address architect" },
        })
      );
      const worker = api.pods.get("legion-legion-42-tester-g1");
      const root = api.pods.get("legion-legion-42-architect-g1");
      // The worker's whole env is the pvc pod's plus exactly the two variables: the store changes
      // nothing else about a pod.
      expect(envOf(mainContainer(worker))).toEqual({
        ...envOf(mainContainer(pvc.api.pods.get("legion-legion-42-tester-g1"))),
        [SESSION_STORAGE_VARIABLE]: "sql",
        [SESSION_SQL_DSN_FILE_VARIABLE]: `${PROVIDERS_DIR}/SESSION_DSN`,
      });
      expect(mainContainer(root).env).toEqual(expect.arrayContaining(SESSION_VARIABLES));
      for (const pod of [worker, root]) {
        // `workspace-init` never runs Oh My Pi and does not mount the providers Secret.
        expect(initContainer(pod).env.map((entry) => entry.name)).not.toContainEqual(
          expect.stringMatching(/^OMP_SESSION_/)
        );
      }
    });

    it("keeps the tree affinity term and the sessions subPath mount: only the transcript moves to the database", async () => {
      const { api, runtime } = harness({ sessionStore: POSTGRES });
      await runtime.spawn("worker", workerSpec());
      const pod = api.pods.get("legion-legion-42-tester-g1");
      expect(pod?.spec.affinity).toEqual({
        podAffinity: {
          requiredDuringSchedulingIgnoredDuringExecution: [
            {
              labelSelector: { matchLabels: { [LABEL_TREE]: issue } },
              topologyKey: "kubernetes.io/hostname",
            },
          ],
        },
      });
      expect(mainContainer(pod).volumeMounts).toContainEqual({
        name: "tree",
        mountPath: OMP_SESSIONS_DIR,
        subPath: "sessions",
      });
    });

    it("resumes a pod from ompSessionFile with the same image, the same storage variables, and no HOME/OMP_PROFILE override, without asking the init container to stat a file the database holds", async () => {
      const { api, runtime, logs } = harness({ sessionStore: POSTGRES });
      const file = `${OMP_SESSIONS_DIR}/-legion-workspaces-acme-widgets-legion-42/2026-09-13T15-00-00-000Z_01a09bb2.jsonl`;
      await runtime.spawn("worker", workerSpec());
      const first = mainContainer(api.pods.get("legion-legion-42-tester-g1"));
      await runtime.spawn(
        "worker",
        workerSpec({
          generation: 2,
          launch: {
            promptPath: "/roles/tester.md",
            addressingPrompt: "address tester",
            resumeSessionFile: file,
          },
        })
      );
      const pod = api.pods.get("legion-legion-42-tester-g2");
      const second = mainContainer(pod);
      const omp = second.command.slice(second.command.indexOf("--") + 1);
      expect(omp.slice(0, 2)).toEqual(["omp", `--resume=${file}`]);
      // The row key embeds the home-relative sessions root, so the replacement resolves the same
      // row only with the same HOME, OMP_PROFILE, and storage variables: the image's own
      // environment (never overridden by the daemon) and the one `podEnvironment` seam.
      expect(second.image).toBe(first.image);
      for (const container of [first, second]) {
        expect(container.env.map((entry) => entry.name)).not.toContain("HOME");
        expect(container.env.map((entry) => entry.name)).not.toContain("OMP_PROFILE");
        expect(container.env).toEqual(expect.arrayContaining(SESSION_VARIABLES));
      }
      // Under postgres the transcript is a database row, not a file on the volume: the init
      // container's existence check (`LEGION_RESUME_SESSION_FILE`) would fail every resume.
      expect(initContainer(pod).env.map((entry) => entry.name)).not.toContain(
        "LEGION_RESUME_SESSION_FILE"
      );
      expect(logs).toContain(`[legion] respawning ${issue} by resuming OMP session ${file}`);
    });
  });

  it("sizes the init container's lock wait from the daemon's boot deadline: worker_boot_timeout_seconds x worker_boot_registration_deadline_intervals, plus one interval, for a non-default config", async () => {
    // The probe keeps an initialising pod alive for exactly the watchdog's registration deadline,
    // so `workspace-init` must be willing to wait at least that long behind another pod's lock --
    // for whatever the deployment configures, not a constant the CLI happens to hard-code.
    const { api, runtime } = harness({
      workerBootTimeoutMs: 600_000,
      workerBootRegistrationDeadlineIntervals: 3,
    });
    await runtime.spawn("worker", workerSpec());
    const init = initContainer(api.pods.get("legion-legion-42-tester-g1"));
    expect(init.name).toBe(INIT_CONTAINER);
    expect(init.env).toContainEqual({
      name: "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS",
      value: String(600 * 3 + 600),
    });
  });

  it("joins the role prompt, the addressing text, and the deployment instructions into one --append-system-prompt argument, in that order", async () => {
    const { api, runtime } = harness({ deploymentInstructionsFile: "/state/instructions.md" });
    await runtime.spawn("worker", workerSpec());
    const pod = api.pods.get("legion-legion-42-tester-g1");
    const command = mainContainer(pod).command;
    // OMP's flag is last-wins: three flags would hand the model only the instructions.
    expect(command.filter((word) => word === "--append-system-prompt")).toHaveLength(1);
    const prompts = command.filter((_, index) => command[index - 1] === "--append-system-prompt");
    expect(prompts).toEqual([
      "text of /roles/tester.md\n\naddress tester\n\ntext of /state/instructions.md",
    ]);
  });

  it("clears the unreferenced mark from an existing PVC instead of creating one", async () => {
    const { api, runtime } = harness();
    api.pvcs.set("legion-legion-42", {
      metadata: {
        name: "legion-legion-42",
        annotations: { [UNREFERENCED_SINCE_ANNOTATION]: "2026-09-01T00:00:00.000Z" },
      },
      spec: {},
    });
    await runtime.spawn("worker", workerSpec());
    const paths = requestPaths(api);
    expect(paths).not.toContainEqual(["POST", "/persistentvolumeclaims"]);
    const patch = api.requests.find((r) => r.method === "PATCH");
    expect(patch).toEqual({
      method: "PATCH",
      path: "/persistentvolumeclaims/legion-legion-42",
      body: { metadata: { annotations: { [UNREFERENCED_SINCE_ANNOTATION]: null } } },
    });
    expect(api.pvcs.get("legion-legion-42")?.metadata.annotations).toEqual({});
  });

  it("tolerates a concurrent first spawn of a new tree: both read no PVC, both create, the second create's 409 is the volume existing -- both spawns succeed, exactly one PVC", async () => {
    // Observed on kind: two issues of one brand-new tree spawned in the same instant. The fake
    // API server, like the real one, refuses the second create of a name with 409 AlreadyExists.
    const { api, runtime } = harness();
    const tree: IssueKey = "LEGION-50";
    const [a, b] = await Promise.all([
      runtime.spawn(
        "worker",
        workerSpec({ issue: "LEGION-51", tree, env: { ...DAEMON_ENV, LEGION_ISSUE: "LEGION-51" } })
      ),
      runtime.spawn(
        "worker",
        workerSpec({ issue: "LEGION-52", tree, env: { ...DAEMON_ENV, LEGION_ISSUE: "LEGION-52" } })
      ),
    ]);
    expect(a.runtime === "kubernetes" && a.pvcName).toBe("legion-legion-50");
    expect(b.runtime === "kubernetes" && b.pvcName).toBe("legion-legion-50");
    expect(
      requestPaths(api).filter(
        ([method, path]) => method === "POST" && path === "/persistentvolumeclaims"
      )
    ).toHaveLength(2);
    expect([...api.pvcs.keys()]).toEqual(["legion-legion-50"]);
    expect([...api.pods.keys()].sort()).toEqual([
      "legion-legion-51-tester-g1",
      "legion-legion-52-tester-g1",
    ]);
  });

  it("serializes concurrent generations of one role: the replacement is admitted only once the earlier generation's pod exists, so it retires that pod instead of racing it", async () => {
    // Two spawns of one (issue, role) in flight together -- a respawn racing the generation it
    // replaces. `retirePreviousPods` can only retire a pod that exists: unserialized, generation
    // 2's retire step would run while generation 1 is still between its own retire step and its
    // pod create, find nothing, and both pods would land on one workspace. The first spawn is held
    // after its retire step (at the token read its Secret needs, the last step before its pod
    // create) while the second is started. A spawn's first act is its prompt read (`readFile`),
    // before any API call, so the prompt reads witness admission: none for the second while the
    // first is held; once released, the second is admitted with generation 1's pod already there.
    // Nothing here waits on the clock.
    const firstReached = Promise.withResolvers<void>();
    const releaseFirst = Promise.withResolvers<void>();
    let tokenCalls = 0;
    /** The pods that exist at each spawn's admission. */
    const admittedWith: string[][] = [];
    let podsNow: () => string[] = () => [];
    let finalizeDeletions = () => {};
    const context = harness({
      readFile: async (file) => {
        admittedWith.push(podsNow());
        return `text of ${file}`;
      },
      provisioningToken: async () => {
        tokenCalls += 1;
        if (tokenCalls === 1) {
          firstReached.resolve();
          await releaseFirst.promise;
        }
        return PROVISION_TOKEN;
      },
      sleep: async () => finalizeDeletions(),
    });
    podsNow = () => [...context.api.pods.keys()];
    finalizeDeletions = () => context.api.finalizeDeletions();
    const first = context.runtime.spawn("worker", workerSpec());
    await firstReached.promise;
    const second = context.runtime.spawn("worker", workerSpec({ generation: 2 }));
    // Let everything already runnable run (the second spawn's own promise reactions; no timed
    // wait): with the first still held, only the first has been admitted.
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(admittedWith).toEqual([[]]);
    releaseFirst.resolve();
    await Promise.all([first, second]);
    expect(admittedWith).toEqual([[], ["legion-legion-42-tester-g1"]]);
    expect([...context.api.pods.keys()]).toEqual(["legion-legion-42-tester-g2"]);
    expect(requestPaths(context.api)).toContainEqual([
      "DELETE",
      "/pods/legion-legion-42-tester-g1",
    ]);
  });

  it("deletes the previous generation's pod gracefully, waits until it is gone, removes its Secret, then creates the new generation", async () => {
    let polls = 0;
    const context = harness({
      sleep: async () => {
        polls += 1;
        if (polls === 3) context.api.finalizeDeletions();
      },
    });
    const { api, runtime } = context;
    seedPod(api, "legion-legion-42-tester-g1", { createdAt: START - DAY_MS });
    api.secrets.set("legion-legion-42-tester-g1", {
      metadata: { name: "legion-legion-42-tester-g1" },
    });
    await runtime.spawn("worker", workerSpec({ generation: 2 }));
    const paths = requestPaths(api);
    const firstDelete = api.requests.find((r) => r.method === "DELETE");
    expect(firstDelete).toEqual({
      method: "DELETE",
      path: "/pods/legion-legion-42-tester-g1",
      body: { apiVersion: "v1", kind: "DeleteOptions", gracePeriodSeconds: 10 },
    });
    const deleteAt = paths.findIndex((p) => p[0] === "DELETE");
    const secretDeleteAt = paths.findIndex(
      (p) => p[0] === "DELETE" && p[1] === "/secrets/legion-legion-42-tester-g1"
    );
    const secretCreateAt = paths.findIndex((p) => p[0] === "POST" && p[1] === "/secrets");
    const podCreateAt = paths.findIndex((p) => p[0] === "POST" && p[1] === "/pods");
    expect(deleteAt).toBeGreaterThan(-1);
    // Three reads found it present (each followed by a poll sleep; the third sleep released the
    // finalizer), the fourth found it gone.
    expect(
      paths.filter((p) => p[0] === "GET" && p[1] === "/pods/legion-legion-42-tester-g1")
    ).toHaveLength(4);
    expect(deleteAt).toBeLessThan(secretDeleteAt);
    expect(secretDeleteAt).toBeLessThan(secretCreateAt);
    expect(secretCreateAt).toBeLessThan(podCreateAt);
    expect(api.pods.has("legion-legion-42-tester-g2")).toBe(true);
    expect(api.secrets.has("legion-legion-42-tester-g2")).toBe(true);
    expect(api.secrets.has("legion-legion-42-tester-g1")).toBe(false);
  });

  it("force-deletes a previous pod that outlives the stop timeout and rejects when even that leaves it present", async () => {
    const { api, runtime } = harness();
    // A finalizer keeps the object after a grace-0 delete, as it does on a real cluster.
    seedPod(api, "legion-legion-42-tester-g1", {
      createdAt: START - DAY_MS,
      finalizers: ["example.com/keep"],
    });
    await expect(runtime.spawn("worker", workerSpec({ generation: 2 }))).rejects.toThrow(
      "previous pod legion-legion-42-tester-g1 is still present 10s after a grace-0 delete"
    );
    const deletes = api.requests.filter((r) => r.method === "DELETE");
    expect(deletes.map((r) => graceOf(r.body))).toEqual([10, 0]);
    expect(api.pods.has("legion-legion-42-tester-g2")).toBe(false);
    expect(api.secrets.has("legion-legion-42-tester-g2")).toBe(false);
  });

  it("fails before any pod exists when the Secret create is forbidden, naming the verb and resource", async () => {
    const { api, runtime } = harness({ forbidden: ["create secrets"] });
    const rejection = runtime.spawn("worker", workerSpec());
    await expect(rejection).rejects.toBeInstanceOf(K8sApiError);
    await expect(rejection).rejects.toThrow(/create secrets\/legion-legion-42-tester-g1/);
    expect(requestPaths(api)).not.toContainEqual(["POST", "/pods"]);
  });

  it("deletes the Secret it created when the pod create fails", async () => {
    const { api, runtime } = harness();
    api.failNext("POST", /\/pods$/, 500);
    await expect(runtime.spawn("worker", workerSpec())).rejects.toBeInstanceOf(K8sApiError);
    expect(requestPaths(api).at(-1)).toEqual(["DELETE", "/secrets/legion-legion-42-tester-g1"]);
    expect(api.secrets.has("legion-legion-42-tester-g1")).toBe(false);
  });

  it("reclaims a per-pod Secret left behind without its pod (a crashed earlier attempt) instead of 409ing on every retry of the same generation", async () => {
    // A daemon that died between the Secret and the Pod create, or a Pod create whose cleanup
    // delete also failed, leaves the Secret alone; the orphan sweep never sees it (no pod, no
    // list), and the retry reuses the generation and so the name. No Secret get or list: the
    // spawn deletes by name once retirePreviousPods has proven no pod of the role exists.
    const { api, runtime } = harness();
    api.secrets.set("legion-legion-42-tester-g1", {
      metadata: {
        name: "legion-legion-42-tester-g1",
        creationTimestamp: new Date(START).toISOString(),
      },
    });
    const locator = await runtime.spawn("worker", workerSpec());
    expect(locator.runtime === "kubernetes" && locator.podName).toBe("legion-legion-42-tester-g1");
    expect([...api.secrets.keys()]).toEqual(["legion-legion-42-tester-g1"]);
    expect(api.requests.filter((r) => r.path.startsWith("/secrets")).map((r) => r.method)).toEqual([
      "DELETE",
      "POST",
    ]);
    expect(api.pods.has("legion-legion-42-tester-g1")).toBe(true);
  });

  it("refuses to launch the controller, saying this runtime does not launch it", async () => {
    const { api, runtime } = harness();
    await expect(
      runtime.spawn("controller", {
        role: "controller",
        env: {},
        launch: { promptPath: "/roles/controller-root.md" },
        secrets: { LEGION_CONTROLLER_SECRET: "s" },
      })
    ).rejects.toThrow("the controller is not launched by this runtime (LEGION-25)");
    expect(api.requests).toEqual([]);
  });

  it("refuses an oversized prompt fragment before any API call", async () => {
    const { api, runtime } = harness({
      deploymentInstructionsFile: "/state/instructions.md",
      readFile: async (file) => (file === "/state/instructions.md" ? "x".repeat(200_000) : "ROLE"),
    });
    await expect(runtime.spawn("worker", workerSpec())).rejects.toThrow(/MAX_ARG_STRLEN/);
    expect(api.requests).toEqual([]);
  });

  it("refuses a spec that does not name its tree and generation, or that carries no boot token", async () => {
    const { api, runtime } = harness();
    const { tree: _tree, ...withoutTree } = workerSpec();
    await expect(runtime.spawn("worker", withoutTree)).rejects.toThrow(
      "spawn worker requires spec.issue, spec.tree, and spec.generation"
    );
    await expect(
      runtime.spawn("worker", workerSpec({ secrets: { ENVOY_TOKEN: "a" } }))
    ).rejects.toThrow(`kubernetes runtime requires the ${BOOT_TOKEN_KEY} secret`);
    expect(api.requests).toEqual([]);
  });

  it("projects every secret in the spec but ENVOY_TOKEN from the per-pod Secret as its own <NAME>_FILE; ENVOY_TOKEN_FILE is the providers mount's file and the token is never copied per pod", async () => {
    const { api, runtime } = harness();
    await runtime.spawn(
      "worker",
      workerSpec({
        secrets: { LEGION_BOOT_TOKEN: BOOT_TOKEN, ENVOY_TOKEN: "envoy-listener-token" },
      })
    );
    const secretCreate = api.requests.find((r) => r.method === "POST" && r.path === "/secrets");
    expect(secretCreate?.body).toMatchObject({
      stringData: { LEGION_BOOT_TOKEN: BOOT_TOKEN, LEGION_PROVISION_TOKEN: PROVISION_TOKEN },
    });
    expect(JSON.stringify(api.requests)).not.toContain("envoy-listener-token");
    const podCreate = api.requests.find((r) => r.method === "POST" && r.path === "/pods");
    expect(podCreate?.body).toEqual(
      buildPodManifest({
        project: "omp",
        tree: issue,
        issue,
        role: "tester",
        generation: 1,
        namespace: "legion",
        image: IMAGE,
        pvcName: "legion-legion-42",
        podName: "legion-legion-42-tester-g1",
        secretName: "legion-legion-42-tester-g1",
        secretKeys: [BOOT_TOKEN_KEY],
        resources: DEFAULT_KUBERNETES_RESOURCES.large,
        env: { ...POD_ENV, ENVOY_TOKEN_FILE: `${PROVIDERS_DIR}/ENVOY_TOKEN` },
        workspaceDir: "/legion/workspaces/acme/widgets/legion-42",
        repo: "acme/widgets",
        shimEndpoint: "tcp://172.18.0.1:19371",
        ompArgv: [
          "omp",
          "--mode",
          "rpc",
          "--append-system-prompt",
          "text of /roles/tester.md\n\naddress tester",
        ],
        terminationGracePeriodSeconds: 10,
        workspaceInitLockWaitSeconds: 480,
      })
    );
  });
});

describe("KubernetesRuntime.probe", () => {
  it("reports a missing pod gone", async () => {
    const { api, runtime } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    const locator = locatorFor(pod);
    api.pods.delete(pod.metadata.name);
    expect(await runtime.probe(locator)).toEqual({ status: "dead", reason: "gone" });
  });

  it("reports a pod with another uid as not the recorded process, naming both uids", async () => {
    const { api, runtime } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    const locator = locatorFor(pod);
    api.reissueUid(pod.metadata.name);
    const observed = api.pods.get(pod.metadata.name)?.metadata.uid;
    expect(await runtime.probe(locator)).toEqual({
      status: "dead",
      reason: "not-recorded-process",
      detail: `pod legion-legion-42-tester-g1 is uid ${observed} (recorded ${locator.runtime === "kubernetes" ? locator.podUid : ""})`,
    });
  });

  it.each([
    ["a pod being deleted", { deletionTimestamp: true }],
    ["a Succeeded pod", { phase: "Succeeded" as const }],
  ])("reports %s gone", async (_name, input) => {
    const { api, runtime } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", {
      createdAt: START,
      phase: "phase" in input ? input.phase : "Running",
    });
    if ("deletionTimestamp" in input)
      pod.metadata.deletionTimestamp = new Date(START).toISOString();
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "dead", reason: "gone" });
  });

  it("reports a Failed pod gone and quotes the failed init container's log tail", async () => {
    const { api, runtime, logs } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", {
      createdAt: START,
      phase: "Failed",
      status: {
        initContainerStatuses: [
          { name: INIT_CONTAINER, state: { terminated: { exitCode: 1, reason: "Error" } } },
        ],
      },
    });
    api.logs.set(`legion-legion-42-tester-g1/${INIT_CONTAINER}`, "clone failed: 401");
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "dead", reason: "gone" });
    expect(logs).toEqual([
      `[legion] pod legion-legion-42-tester-g1 Failed: last lines of ${INIT_CONTAINER}:\nclone failed: 401`,
    ]);
  });

  it("quotes the main container's log tail when a Failed pod's init container succeeded", async () => {
    const { api, runtime, logs } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", {
      createdAt: START,
      phase: "Failed",
      status: {
        initContainerStatuses: [{ name: INIT_CONTAINER, state: { terminated: { exitCode: 0 } } }],
      },
    });
    api.logs.set(`legion-legion-42-tester-g1/${MAIN_CONTAINER}`, "omp: session file missing");
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "dead", reason: "gone" });
    expect(logs).toEqual([
      `[legion] pod legion-legion-42-tester-g1 Failed: last lines of ${MAIN_CONTAINER}:\nomp: session file missing`,
    ]);
  });

  it("keeps a Pending pod alive until the boot timeout, then reports it gone with its events", async () => {
    const { api, runtime, logs, advance } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START, phase: "Pending" });
    const event: FakeK8sEvent = {
      metadata: { name: "legion-legion-42-tester-g1.1" },
      type: "Warning",
      reason: "FailedScheduling",
      message: "0/3 nodes are available",
      involvedObject: { name: "legion-legion-42-tester-g1" },
    };
    api.events.push(event);
    advance(119_999);
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "alive" });
    expect(logs).toEqual([]);
    advance(1);
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "dead", reason: "gone" });
    expect(logs).toEqual([
      "[legion] pod legion-legion-42-tester-g1 pending for 120s (worker_boot_timeout_seconds 120); events: Warning FailedScheduling: 0/3 nodes are available",
    ]);
  });

  it("keeps a Pending pod whose init container is running alive past the boot timeout: an initialising pod is a live process, however long its provisioning or lock wait takes", async () => {
    // The init container is `workspace-init`: a clone/fetch of up to slow_command_timeout each,
    // or a wait behind another pod's lock on the shared clone. The boot watchdog probes at every
    // worker_boot_timeout interval and re-arms on alive; a dead verdict here would retire a pod
    // that is simply still provisioning. The watchdog's own registration deadline (intervals x
    // timeout) still bounds it -- the runtime's verdict is only about whether the process lives.
    const { api, runtime, logs, advance } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", {
      createdAt: START,
      phase: "Pending",
      status: {
        initContainerStatuses: [
          {
            name: INIT_CONTAINER,
            state: { running: { startedAt: new Date(START).toISOString() } },
          },
        ],
      },
    });
    advance(120_000);
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "alive" });
    advance(240_000);
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "alive" });
    expect(logs).toEqual([]);
    expect(api.requests.filter(({ method }) => method === "DELETE")).toEqual([]);
  });

  it("ages a Pending pod whose init container finished (exit 0) from that finish, not from the pod's creation: a long provisioning does not eat the main container's boot timeout", async () => {
    // Init took 5 minutes (a large clone); the main container started at 300 s. At pod age 6 min
    // the old rule (age from creation) said dead; the boot is 60 s old. At 300 + 120 s it is gone.
    const { api, runtime, logs, advance } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", {
      createdAt: START,
      phase: "Pending",
      status: {
        initContainerStatuses: [
          {
            name: INIT_CONTAINER,
            state: {
              terminated: {
                exitCode: 0,
                startedAt: new Date(START).toISOString(),
                finishedAt: new Date(START + 300_000).toISOString(),
              },
            },
          },
        ],
      },
    });
    advance(360_000);
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "alive" });
    expect(logs).toEqual([]);
    advance(59_999);
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "alive" });
    advance(1);
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "dead", reason: "gone" });
    expect(logs).toEqual([
      "[legion] pod legion-legion-42-tester-g1 main container not started 120s after its init containers finished (worker_boot_timeout_seconds 120); events: ",
    ]);
  });

  it("reports a Pending pod whose init container terminated non-zero gone, quoting its log tail, whatever its age", async () => {
    const { api, runtime, logs } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", {
      createdAt: START,
      phase: "Pending",
      status: {
        initContainerStatuses: [
          { name: INIT_CONTAINER, state: { terminated: { exitCode: 1, reason: "Error" } } },
        ],
      },
    });
    api.logs.set(
      `legion-legion-42-tester-g1/${INIT_CONTAINER}`,
      "Timed out after 900 s waiting for workspace-init lock /legion/repos/github.com/acme/widgets.lock"
    );
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "dead", reason: "gone" });
    expect(logs).toEqual([
      `[legion] pod legion-legion-42-tester-g1 Failed: last lines of ${INIT_CONTAINER}:\nTimed out after 900 s waiting for workspace-init lock /legion/repos/github.com/acme/widgets.lock`,
    ]);
  });

  it("reports a Running pod alive whether or not its stream is registered", async () => {
    const { api, runtime } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "alive" });
  });

  it("answers alive on an API failure only when the pod's stream is registered, else unknown", async () => {
    const { api, runtime, registrations } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    api.failNext("GET", /\/pods\/legion-legion-42-tester-g1$/, 500);
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "unknown" });
    registrations.set("legion-omp-legion-42-tester", fakeWorkerRpcClient());
    api.failNext("GET", /\/pods\/legion-legion-42-tester-g1$/, 500);
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "alive" });
  });

  it("reports an Unknown phase as unknown", async () => {
    const { api, runtime } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START, phase: "Unknown" });
    expect(await runtime.probe(locatorFor(pod))).toEqual({ status: "unknown" });
  });

  it("refuses to operate a tmux locator", async () => {
    const { runtime } = harness();
    const foreign: Locator = { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@1" };
    for (const attempt of [
      () => runtime.probe(foreign),
      () => runtime.connect(foreign),
      () => runtime.stop(foreign, 50),
    ]) {
      await expect(attempt()).rejects.toThrow("kubernetes runtime cannot operate a tmux locator");
    }
  });
});

describe("KubernetesRuntime.connect", () => {
  it("awaits the listener registration for the locator's claim token with the RPC timeout by default", async () => {
    const { api, runtime, registrations, awaits } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    const client = fakeWorkerRpcClient();
    registrations.set("legion-omp-legion-42-tester", client);
    expect(await runtime.connect(locatorFor(pod))).toBe(client);
    expect(await runtime.connect(locatorFor(pod), 1_234)).toBe(client);
    expect(awaits).toEqual([
      { token: "legion-omp-legion-42-tester", timeoutMs: 5_000 },
      { token: "legion-omp-legion-42-tester", timeoutMs: 1_234 },
    ]);
  });
});

describe("operator-launched controller (LEGION-25 Part B)", () => {
  const externalRecord = (sessionId: string): ExternalControllerLocator => ({
    runtime: "kubernetes",
    external: true,
    sessionId,
    registeredAt: START,
  });

  /** A recording listener answering `GET /v1/roles/<token>` with the given status and body. */
  function envoy(status: number, body: unknown) {
    const calls: Array<{ url: string; method: string | undefined; authorization: string | null }> =
      [];
    const fetch: RoleLookupFetch = async (input, init) => {
      const headers = new Headers(init?.headers);
      calls.push({
        url: String(input),
        method: init?.method,
        authorization: headers.get("authorization"),
      });
      return new Response(typeof body === "string" ? body : JSON.stringify(body), {
        status,
        headers: { "content-type": "application/json" },
      });
    };
    return { calls, fetch };
  }

  it("controllerLaunch is operator and controllerReadyLocator records the session at now", () => {
    const { runtime, advance } = harness();
    expect(runtime.controllerLaunch).toBe("operator");
    advance(5_000);
    expect(runtime.controllerReadyLocator("ses_op")).toEqual({
      runtime: "kubernetes",
      external: true,
      sessionId: "ses_op",
      registeredAt: START + 5_000,
    });
  });

  it("probe of an external record asks the listener for the controller role with the bearer", async () => {
    const listener = envoy(200, { holder: "ses_op", last_seen: START });
    const { runtime } = harness({ envoyFetch: listener.fetch, envoyToken: "envoy-bearer" });
    expect(await runtime.probe(externalRecord("ses_op"))).toEqual({ status: "alive" });
    expect(listener.calls).toEqual([
      {
        url: "http://envoy.test:9020/v1/roles/legion-omp-controller",
        method: "GET",
        authorization: "Bearer envoy-bearer",
      },
    ]);
  });

  it("sends no Authorization header when the daemon has no Envoy token", async () => {
    const listener = envoy(200, { holder: "ses_op", last_seen: START });
    const { runtime } = harness({ envoyFetch: listener.fetch });
    await runtime.probe(externalRecord("ses_op"));
    expect(listener.calls[0]?.authorization).toBeNull();
  });

  it("is alive while the holder is the recorded session and last_seen is within two heartbeats", async () => {
    const listener = envoy(200, { holder: "ses_op", last_seen: START - 239_999 });
    const { runtime, logs } = harness({ envoyFetch: listener.fetch });
    expect(await runtime.probe(externalRecord("ses_op"))).toEqual({ status: "alive" });
    expect(logs).toEqual([]);
  });

  it("is gone once last_seen is two heartbeats old, logging the age", async () => {
    const listener = envoy(200, { holder: "ses_op", last_seen: START - 240_000 });
    const { runtime, logs } = harness({ envoyFetch: listener.fetch });
    expect(await runtime.probe(externalRecord("ses_op"))).toEqual({
      status: "dead",
      reason: "gone",
    });
    expect(logs).toEqual([
      "[legion] the controller session ses_op was last seen 240s ago; treating it as gone",
    ]);
  });

  it("never lets the liveness window drop below the boot timeout", async () => {
    expect(controllerLivenessMs(120_000)).toBe(2 * CONTROLLER_HEARTBEAT_MS);
    expect(controllerLivenessMs(600_000)).toBe(600_000);
    const listener = envoy(200, { holder: "ses_op", last_seen: START - 599_999 });
    const { runtime } = harness({ envoyFetch: listener.fetch, workerBootTimeoutMs: 600_000 });
    expect(await runtime.probe(externalRecord("ses_op"))).toEqual({ status: "alive" });
  });

  it("is gone on 404: nobody holds the controller role", async () => {
    const listener = envoy(404, { error: "no holder for role legion-omp-controller" });
    const { runtime } = harness({ envoyFetch: listener.fetch });
    expect(await runtime.probe(externalRecord("ses_op"))).toEqual({
      status: "dead",
      reason: "gone",
    });
  });

  it("is gone when another session holds the role, logging both ids", async () => {
    const listener = envoy(200, { holder: "ses_other", last_seen: START });
    const { runtime, logs } = harness({ envoyFetch: listener.fetch });
    expect(await runtime.probe(externalRecord("ses_op"))).toEqual({
      status: "dead",
      reason: "gone",
    });
    expect(logs).toEqual([
      "[legion] the controller role is held by session ses_other, not the recorded ses_op; treating the recorded controller as gone",
    ]);
  });

  it("is unknown, never dead, when the listener is unreachable", async () => {
    const { runtime, logs } = harness({
      envoyFetch: async () => {
        throw new Error("ECONNREFUSED");
      },
    });
    expect(await runtime.probe(externalRecord("ses_op"))).toEqual({ status: "unknown" });
    expect(logs).toEqual([
      "[legion] Envoy listener http://envoy.test:9020 unreachable while probing the controller: ECONNREFUSED",
    ]);
  });

  it("is unknown on a 500 (naming whether a bearer was sent) and on an unparseable body", async () => {
    const failing = envoy(500, "boom");
    const { runtime: withoutToken, logs } = harness({ envoyFetch: failing.fetch });
    expect(await withoutToken.probe(externalRecord("ses_op"))).toEqual({ status: "unknown" });
    expect(logs).toEqual([
      "[legion] Envoy listener http://envoy.test:9020 answered 500 to the controller role lookup with no bearer token sent",
    ]);

    const garbled = envoy(200, "not json");
    const { runtime, logs: garbledLogs } = harness({ envoyFetch: garbled.fetch });
    expect(await runtime.probe(externalRecord("ses_op"))).toEqual({ status: "unknown" });
    expect(garbledLogs).toEqual([
      "[legion] Envoy listener http://envoy.test:9020 answered the controller role lookup with an unreadable body",
    ]);
  });

  it("stop of an external record makes no API call and resolves", async () => {
    const { api, runtime } = harness();
    await runtime.stop(externalRecord("ses_op"), 50);
    await runtime.stop(externalRecord("ses_op"), 50, { skipGraceful: true });
    expect(api.requests).toEqual([]);
  });
});

describe("KubernetesRuntime.stop", () => {
  function shutdownCounting(neverCloses = false) {
    const client = fakeWorkerRpcClient();
    let shutdowns = 0;
    const realShutdown = client.shutdown.bind(client);
    client.shutdown = () => {
      shutdowns += 1;
      if (!neverCloses) realShutdown();
    };
    return { client, shutdowns: () => shutdowns };
  }

  it("sends one shutdown frame, deletes the pod with the timeout's grace and its uid precondition, then deletes the Secret", async () => {
    const { api, runtime, registrations } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    api.secrets.set(pod.metadata.name, { metadata: { name: pod.metadata.name } });
    const { client, shutdowns } = shutdownCounting();
    registrations.set("legion-omp-legion-42-tester", client);
    await runtime.stop(locatorFor(pod), 50);
    expect(shutdowns()).toBe(1);
    expect(api.requests).toEqual([
      {
        method: "DELETE",
        path: "/pods/legion-legion-42-tester-g1",
        body: {
          apiVersion: "v1",
          kind: "DeleteOptions",
          gracePeriodSeconds: 1,
          preconditions: { uid: pod.metadata.uid },
        },
      },
      { method: "DELETE", path: "/secrets/legion-legion-42-tester-g1", body: undefined },
    ]);
    expect(pod.metadata.deletionTimestamp).toBeDefined();
    expect(api.secrets.has(pod.metadata.name)).toBe(false);
  });

  it("goes straight to the delete when no stream is registered", async () => {
    const { api, runtime } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    await runtime.stop(locatorFor(pod), 50);
    expect(requestPaths(api)).toEqual([
      ["DELETE", "/pods/legion-legion-42-tester-g1"],
      ["DELETE", "/secrets/legion-legion-42-tester-g1"],
    ]);
  });

  it("skipGraceful deletes with grace 0 and sends no shutdown", async () => {
    const { api, runtime, registrations } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    const { client, shutdowns } = shutdownCounting();
    registrations.set("legion-omp-legion-42-tester", client);
    await runtime.stop(locatorFor(pod), 50, { skipGraceful: true });
    expect(shutdowns()).toBe(0);
    expect(graceOf(api.requests[0]?.body)).toBe(0);
    expect(api.pods.has(pod.metadata.name)).toBe(false);
  });

  it("refuseKill sends the shutdown and touches neither the pod nor the Secret", async () => {
    const { api, runtime, registrations } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    api.secrets.set(pod.metadata.name, { metadata: { name: pod.metadata.name } });
    const { client, shutdowns } = shutdownCounting(true);
    registrations.set("legion-omp-legion-42-tester", client);
    await runtime.stop(locatorFor(pod), 50, { refuseKill: true });
    expect(shutdowns()).toBe(1);
    expect(api.requests).toEqual([]);
    expect(api.secrets.has(pod.metadata.name)).toBe(true);
  });

  it("logs and leaves the Secret alone when the uid precondition refuses the delete", async () => {
    const { api, runtime, logs } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    const locator = locatorFor(pod);
    const recorded = pod.metadata.uid;
    api.secrets.set(pod.metadata.name, { metadata: { name: pod.metadata.name } });
    api.reissueUid(pod.metadata.name);
    await runtime.stop(locator, 50, { skipGraceful: true });
    expect(logs).toEqual([
      `[legion] not deleting pod legion-legion-42-tester-g1, treating it as already gone: its uid is not the recorded ${recorded}`,
    ]);
    expect(requestPaths(api)).toEqual([["DELETE", "/pods/legion-legion-42-tester-g1"]]);
    expect(api.pods.has(pod.metadata.name)).toBe(true);
    expect(api.secrets.has(pod.metadata.name)).toBe(true);
  });

  it("still deletes the Secret when the pod is already gone", async () => {
    const { api, runtime } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    api.secrets.set(pod.metadata.name, { metadata: { name: pod.metadata.name } });
    api.pods.delete(pod.metadata.name);
    await runtime.stop(locatorFor(pod), 50, { skipGraceful: true });
    expect(api.secrets.has(pod.metadata.name)).toBe(false);
  });

  it.each([
    [
      "the pod delete",
      /\/pods\/legion-legion-42-tester-g1$/,
      "delete pods/legion-legion-42-tester-g1",
    ],
    [
      "the Secret delete",
      /\/secrets\/legion-legion-42-tester-g1$/,
      "delete secrets/legion-legion-42-tester-g1",
    ],
  ])("raises ProcessStopFailed when %s fails for a reason other than 404 or 409", async (_name, pattern, verb) => {
    const { api, runtime } = harness();
    const pod = seedPod(api, "legion-legion-42-tester-g1", { createdAt: START });
    api.failNext("DELETE", pattern, 500);
    const rejection = runtime.stop(locatorFor(pod), 50, { skipGraceful: true });
    await expect(rejection).rejects.toBeInstanceOf(ProcessStopFailed);
    await expect(rejection).rejects.toThrow(verb);
  });
});

describe("KubernetesRuntime.reconcileOrphans", () => {
  const projectLabels = { [LABEL_PROJECT]: "omp" };

  it("deletes unknown pods past the grace with the stop grace and each one's per-pod Secret by name, keeps young and known ones, and never lists Secrets", async () => {
    // The daemon's service account has no `secrets: list` -- a list returns every Secret's data,
    // the providers Secret's keys included -- so the sweep derives Secret names from the orphan
    // pods (same names) and deletes by name, tolerating one that is already gone.
    const { api, runtime } = harness();
    const created = new Date(START - 200_000).toISOString();
    seedPod(api, "old-orphan", { createdAt: START - 200_000, labels: projectLabels });
    api.secrets.set("old-orphan", {
      metadata: { name: "old-orphan", labels: projectLabels, creationTimestamp: created },
    });
    seedPod(api, "old-orphan-secretless", { createdAt: START - 200_000, labels: projectLabels });
    seedPod(api, "young-orphan", { createdAt: START - 1_000, labels: projectLabels });
    seedPod(api, "known", { createdAt: START - 200_000, labels: projectLabels });
    api.secrets.set("legion-omp-providers", {
      metadata: { name: "legion-omp-providers", labels: projectLabels, creationTimestamp: created },
    });
    await runtime.reconcileOrphans(new Set(["known"]), 120_000);
    expect(api.requests.filter((r) => r.method === "DELETE")).toEqual([
      {
        method: "DELETE",
        path: "/pods/old-orphan",
        body: { apiVersion: "v1", kind: "DeleteOptions", gracePeriodSeconds: 10 },
      },
      { method: "DELETE", path: "/secrets/old-orphan", body: undefined },
      {
        method: "DELETE",
        path: "/pods/old-orphan-secretless",
        body: { apiVersion: "v1", kind: "DeleteOptions", gracePeriodSeconds: 10 },
      },
      { method: "DELETE", path: "/secrets/old-orphan-secretless", body: undefined },
    ]);
    expect(api.requests.filter((r) => r.method === "GET" && r.path.startsWith("/secrets"))).toEqual(
      []
    );
    expect(api.secrets.has("old-orphan")).toBe(false);
    expect(api.secrets.has("legion-omp-providers")).toBe(true);
  });

  it("marks an unknown PVC unreferenced, leaves it for seven days, then deletes it", async () => {
    const { api, runtime, advance, now } = harness();
    api.pvcs.set("legion-legion-7", {
      metadata: {
        name: "legion-legion-7",
        labels: { ...projectLabels, [LABEL_TREE]: "LEGION-7" },
        creationTimestamp: new Date(START - 200_000).toISOString(),
      },
      spec: {},
    });
    await runtime.reconcileOrphans(new Set(), 120_000);
    const marked = new Date(now()).toISOString();
    expect(api.pvcs.get("legion-legion-7")?.metadata.annotations).toEqual({
      [UNREFERENCED_SINCE_ANNOTATION]: marked,
    });
    advance(DAY_MS);
    await runtime.reconcileOrphans(new Set(), 120_000);
    expect(api.pvcs.get("legion-legion-7")?.metadata.annotations).toEqual({
      [UNREFERENCED_SINCE_ANNOTATION]: marked,
    });
    expect(api.requests.filter((r) => r.method === "PATCH")).toHaveLength(1);
    advance(PVC_RETENTION_MS - DAY_MS);
    await runtime.reconcileOrphans(new Set(), 120_000);
    expect(api.pvcs.has("legion-legion-7")).toBe(false);
  });

  it("clears the unreferenced mark from a PVC that is known again", async () => {
    const { api, runtime } = harness();
    api.pvcs.set("legion-legion-42", {
      metadata: {
        name: "legion-legion-42",
        labels: { ...projectLabels, [LABEL_TREE]: issue },
        annotations: { [UNREFERENCED_SINCE_ANNOTATION]: "2026-09-01T00:00:00.000Z" },
        creationTimestamp: new Date(START - 200_000).toISOString(),
      },
      spec: {},
    });
    await runtime.reconcileOrphans(new Set(["legion-legion-42"]), 120_000);
    expect(api.pvcs.get("legion-legion-42")?.metadata.annotations).toEqual({});
  });

  it("logs a failed delete and keeps sweeping the remaining orphans", async () => {
    const { api, runtime, logs } = harness();
    seedPod(api, "orphan-a", { createdAt: START - 200_000, labels: projectLabels });
    seedPod(api, "orphan-b", { createdAt: START - 200_000, labels: projectLabels });
    api.failNext("DELETE", /\/pods\/orphan-a$/, 403);
    await runtime.reconcileOrphans(new Set(), 120_000);
    expect(logs).toHaveLength(1);
    expect(logs[0]).toContain("orphan-a");
    expect(logs[0]).toContain("403");
    expect(api.pods.get("orphan-b")?.metadata.deletionTimestamp).toBeDefined();
    expect(api.pods.get("orphan-a")?.metadata.deletionTimestamp).toBeUndefined();
  });
});
