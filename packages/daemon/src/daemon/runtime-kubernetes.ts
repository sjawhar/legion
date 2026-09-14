import { readFile as readFileFs } from "node:fs/promises";
import path from "node:path";
import { type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
import type { JjIdentity } from "@legion/workspace";
import type { KubernetesRuntimeConfig, SessionStore } from "./config";
import { K8sApiError, type K8sClient, type K8sPod } from "./k8s-client";
import {
  BOOT_DIR,
  BOOT_TOKEN_KEY,
  buildPodManifest,
  buildPvcManifest,
  buildSecretManifest,
  GRANT_DIR,
  IMAGE_PATH,
  INIT_CONTAINER,
  LABEL_ISSUE,
  LABEL_PROJECT,
  LABEL_ROLE,
  labelValue,
  MAIN_CONTAINER,
  POD_CREDENTIAL_HELPER,
  PROVIDERS_DIR,
  PROVISION_TOKEN_KEY,
  podLabels,
  podName,
  podSelector,
  podWorkspaceDir,
  pvcName,
  SESSION_SQL_DSN_FILE_VARIABLE,
  SESSION_STORAGE_VARIABLE,
  TREE_MOUNT,
  UNREFERENCED_SINCE_ANNOTATION,
} from "./k8s-manifests";
import {
  awaitShutdown,
  type K8sLocator,
  type Locator,
  type ProbeResult,
  ProcessStopFailed,
  type Runtime,
  type SpawnSpec,
  serialize,
} from "./runtime";
import { grantSecretName } from "./secrets";
import { workerBinDir } from "./worker-bin";
import type { WorkerRpcClient } from "./worker-rpc";
import type { WorkerStreamListener } from "./worker-stream-listener";

/** How long an unreferenced tree PVC is kept after the sweep first found it unreferenced: seven
 * days (root spec, LEGION-19 architect's decision), then the sweep deletes it. */
export const PVC_RETENTION_MS = 7 * 24 * 60 * 60 * 1000;

/** How often `spawn` re-reads a previous generation's pod while waiting for it to disappear. */
const PREVIOUS_POD_POLL_MS = 500;
/** How many log lines a Failed pod's container contributes to the daemon log. */
const LOG_TAIL_LINES = 20;

export interface KubernetesRuntimeDeps {
  project: string;
  config: KubernetesRuntimeConfig;
  client: K8sClient;
  /** Read lazily on first use: `index.ts` creates the listener after the runtime and the API. */
  listener(): Pick<WorkerStreamListener, "registrations" | "awaitRegistration">;
  repo: `${string}/${string}`;
  /** The GitHub App installation token the pod's init container clones with. */
  provisioningToken(owner: string): Promise<string>;
  /** `config.daemonUrl`; its host is the shim's dial target. */
  daemonUrl: string;
  workerStreamPort: number;
  workerBootTimeoutMs: number;
  /** `config.workerBootRegistrationDeadlineIntervals`: with `workerBootTimeoutMs`, the boot
   * watchdog's registration deadline -- how long `ProcessManager` tolerates a pod that is alive
   * but not yet ready, an initialising one included. The init container's lock wait is sized
   * from it (`workspaceInitLockWaitSeconds`). */
  workerBootRegistrationDeadlineIntervals: number;
  workerStopTimeoutMs: number;
  workerRpcTimeoutMs(): number;
  deploymentInstructionsFile?: string;
  /** Overridable for tests; defaults to `node:fs/promises` `readFile(path, "utf8")`. */
  readFile?(path: string): Promise<string>;
  now(): number;
  /** Bounds the previous-pod wait and a graceful stop's wait; overridable for tests. */
  sleep?(ms: number): Promise<void>;
  /** One line per notable event; defaults to `console.error`. */
  log?(line: string): void;
}

function kubernetesLocator(locator: Locator): K8sLocator & { runtime: "kubernetes" } {
  if (locator.runtime !== "kubernetes") {
    throw new Error("kubernetes runtime cannot operate a tmux locator");
  }
  return locator;
}

function isApiStatus(error: unknown, status: number): boolean {
  return error instanceof K8sApiError && error.status === status;
}

function describeError(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function ageMs(now: number, creationTimestamp: string | undefined): number {
  const created = creationTimestamp === undefined ? Number.NaN : Date.parse(creationTimestamp);
  return Number.isNaN(created) ? 0 : now - created;
}

/** Whether the pod's `workspace-init` init container has exited non-zero: the one container state
 * that means the pod's provisioning failed, whatever phase the pod currently reports. */
function initContainerFailed(pod: K8sPod): boolean {
  return (pod.status?.initContainerStatuses ?? []).some(
    (status) => (status.state?.terminated?.exitCode ?? 0) !== 0
  );
}

/** When the pod's init containers had all finished -- the latest `finishedAt` -- and so when its
 * main container began to start; `undefined` while any init container has not terminated (or
 * the pod has none reported yet). */
function initContainersFinishedAt(pod: K8sPod): string | undefined {
  const statuses = pod.status?.initContainerStatuses ?? [];
  if (statuses.length === 0) return undefined;
  let latest: string | undefined;
  for (const status of statuses) {
    const finishedAt = status.state?.terminated?.finishedAt;
    if (finishedAt === undefined) return undefined;
    if (latest === undefined || Date.parse(finishedAt) > Date.parse(latest)) latest = finishedAt;
  }
  return latest;
}

/**
 * The Kubernetes implementation of `Runtime` (root spec section 3): every Legion process is one
 * bare Pod (`restartPolicy: Never`) per process generation, scheduled beside its tree's other pods
 * on the node that holds the tree's one RWO volume (`legion-<tree-slug>`). The pod's init
 * container (`legion workspace-init`) prepares the issue's working copy on that volume with the
 * repository token; the main container runs `legion worker-shim --connect` around OMP, dialing the
 * daemon's worker stream with the boot token and exporting the mounted provider keys into the OMP
 * child alone. The daemon never dials a pod: `connect` waits for the shim's registration under
 * the locator's claim token, and `probe`/`stop` read that same live registration map.
 *
 * Secrets travel only as files: the boot token and the repository token in a per-pod Secret
 * projected into the main and init containers respectively, provider keys in the deployment's
 * `legion-<project>-providers` Secret. Nothing secret appears in a pod's env, command, or args.
 *
 * Retries, generations, `--resume`, and the running-worker cap stay `ProcessManager`'s: this
 * runtime never uses a Job, a StatefulSet, or `activeDeadlineSeconds`.
 */
export class KubernetesRuntime implements Runtime {
  readonly launchesController = false;
  readonly removesWorkspacesOnTreeClose = false;
  private readonly log: (line: string) => void;
  private readonly readFile: (file: string) => Promise<string>;
  private readonly sleep: (ms: number) => Promise<void>;
  /** A root and a worker may be launched concurrently; a per-role lane ensures their pod/PVC
   * mutation sequence cannot leave two generations sharing one workspace. */
  private readonly spawnQueues = new Map<string, Promise<unknown>>();

  constructor(private readonly deps: KubernetesRuntimeDeps) {
    this.log = deps.log ?? ((line) => console.error(line));
    this.readFile = deps.readFile ?? ((file) => readFileFs(file, "utf8"));
    this.sleep = deps.sleep ?? ((ms) => Bun.sleep(ms));
  }

  /**
   * Order, and why: the prompt texts are read and the whole manifest built before any API call
   * (a missing prompt or an oversized fragment fails as the tmux `stat` does, with nothing
   * created); the tree PVC is ensured; any previous pod of this (issue, role) is deleted and
   * awaited so a replacement generation never runs beside its predecessor on the same working
   * copy; the per-pod Secret is created; the Pod last, so a failure anywhere before it leaves no
   * pod, and a pod-create failure deletes the Secret it would have mounted.
   */
  async spawn(kind: "root" | "worker" | "controller", spec: SpawnSpec): Promise<Locator> {
    if (kind === "controller" || spec.role === "controller") {
      throw new Error(
        "KubernetesRuntime does not launch the controller: the controller is not launched by this runtime (LEGION-25)"
      );
    }
    const { issue, tree, generation, role } = spec;
    if (issue === undefined || tree === undefined || generation === undefined) {
      throw new Error(`spawn ${kind} requires spec.issue, spec.tree, and spec.generation`);
    }
    // Every secret in the spec but the shared `ENVOY_TOKEN` goes into the per-pod Secret and is
    // projected into the main container as `<NAME>_FILE` (`podSecrets`); the boot token is the one
    // the shim itself reads (`--boot-token-file`), so it must be among them. `ENVOY_TOKEN` is the
    // providers Secret's contract, like `DISPATCH_TOKEN`: every pod mounts that Secret, so its
    // `ENVOY_TOKEN_FILE` is the mount's own file and the token is never copied per pod — the
    // deployment (an in-cluster daemon reading `envoy_token_file` from that mount, or an
    // out-of-cluster kubeconfig daemon whose operator put the same token in the providers Secret)
    // owns the one copy.
    const secretNames = Object.keys(spec.secrets);
    if (!secretNames.includes(BOOT_TOKEN_KEY)) {
      throw new Error(`kubernetes runtime requires the ${BOOT_TOKEN_KEY} secret`);
    }
    const validated = { kind, spec, issue, tree, generation, role };
    return serialize(this.spawnQueues, `${issue}:${role}`, () => this.spawnSerialized(validated));
  }

  private async spawnSerialized({
    kind,
    spec,
    issue,
    tree,
    generation,
    role,
  }: {
    kind: "root" | "worker";
    spec: SpawnSpec;
    issue: IssueKey;
    tree: IssueKey;
    generation: number;
    role: LegionRole;
  }): Promise<Locator> {
    const { projected, pointers } = podSecrets(spec.secrets);
    const promptText = await this.readFile(spec.launch.promptPath);
    const instructionsText =
      this.deps.deploymentInstructionsFile === undefined
        ? undefined
        : await this.readFile(this.deps.deploymentInstructionsFile);

    const { config, project, repo } = this.deps;
    const token = roleToken(project, issue, role);
    const name = podName(issue, role, generation);
    const pvc = pvcName(tree);
    const labels = podLabels({ project, tree, issue, role, generation });
    const workspaceDir = podWorkspaceDir(repo, issue);
    const { resumeSessionFile, addressingPrompt } = spec.launch;
    // The same three texts `systemPromptArguments` (runtime-tmux.ts) joins, in its order -- role
    // prompt, addressing, instructions -- as ONE argument separated by blank lines: OMP's flag is
    // last-wins, so three flags would hand the model only the deployment instructions. Here the
    // texts are inline (no shell expands anything inside a pod command), so no quoting applies.
    const systemPrompt = [promptText, addressingPrompt, instructionsText]
      .filter((text): text is string => text !== undefined)
      .join("\n\n");
    const ompArgv = [
      "omp",
      ...(resumeSessionFile === undefined ? [] : [`--resume=${resumeSessionFile}`]),
      "--mode",
      "rpc",
      "--append-system-prompt",
      systemPrompt,
    ];
    const graceSeconds = Math.ceil(this.deps.workerStopTimeoutMs / 1000);
    const manifest = buildPodManifest({
      project,
      tree,
      issue,
      role,
      generation,
      namespace: config.namespace,
      image: config.image.reference,
      pvcName: pvc,
      podName: name,
      secretName: name,
      secretKeys: Object.keys(projected),
      resources: config.resources[config.roleProfiles[role]],
      env: podEnvironment(kind, spec.env, token, workspaceDir, pointers, config.sessionStore),
      workspaceDir,
      repo,
      shimEndpoint: `tcp://${new URL(this.deps.daemonUrl).hostname}:${this.deps.workerStreamPort}`,
      ompArgv,
      terminationGracePeriodSeconds: graceSeconds,
      workspaceInitLockWaitSeconds: this.workspaceInitLockWaitSeconds(),
      // The init container stats the recorded session on the tree volume before OMP runs
      // (`LEGION_RESUME_SESSION_FILE`). Under postgres the transcript is a database row it cannot
      // see, so it is not asked to; `--resume` reaches OMP under both stores.
      resumeSessionFile: config.sessionStore.kind === "pvc" ? resumeSessionFile : undefined,
    });

    await this.ensurePvc(pvc, tree);
    await this.retirePreviousPods(issue, role, graceSeconds);
    // No pod of this (issue, role) exists now, so a Secret still wearing this pod's name is the
    // leftover of a crashed attempt (a daemon that died between the Secret and the Pod create, or
    // a Pod create whose cleanup delete also failed) -- exactly as `retirePreviousPods` treats a
    // same-name pod. Reclaimed here, by name, so the retry (same generation) does not 409 forever.
    await this.deleteSecretTolerating404(name);

    const [owner] = repo.split("/") as [string, string];
    await this.deps.client.secrets.create(
      buildSecretManifest({
        name,
        labels,
        stringData: {
          ...projected,
          [PROVISION_TOKEN_KEY]: await this.deps.provisioningToken(owner),
        },
      })
    );
    if (resumeSessionFile !== undefined) {
      this.log(
        `[legion] ${kind === "root" ? "resurrecting" : "respawning"} ${issue} by resuming OMP session ${resumeSessionFile}`
      );
    }
    let created: K8sPod;
    try {
      created = await this.deps.client.pods.create(manifest);
    } catch (error) {
      try {
        await this.deps.client.secrets.delete(name);
      } catch (cleanup) {
        this.log(
          `[legion] failed to delete Secret ${name} after pod ${name} could not be created: ${describeError(cleanup)}`
        );
      }
      throw error;
    }
    if (created.metadata.uid === undefined) {
      throw new Error(`pod ${name} was created without a uid`);
    }
    // No `ompSessionFile` here: `launchWorker` and `/process|worker/started` set it, as for tmux.
    return {
      runtime: "kubernetes",
      namespace: config.namespace,
      podName: name,
      podUid: created.metadata.uid,
      pvcName: pvc,
      roleToken: token,
    };
  }

  /** How long the pod's `workspace-init` may wait for another pod's lock on the shared clone:
   * the boot watchdog's registration deadline (`worker_boot_timeout_seconds x
   * worker_boot_registration_deadline_intervals`, the whole time `ProcessManager` tolerates an
   * alive-but-unready pod -- `probe` reports an initialising pod alive) plus one more interval,
   * since the watchdog acts only at an interval boundary and its retirement is itself a graceful
   * stop. The init container therefore never gives up on a wait the daemon would still tolerate,
   * whatever the deployment configures: the daemon's deadline is the operative bound, the pod's
   * own wait the backstop for a pod nobody is watching. */
  private workspaceInitLockWaitSeconds(): number {
    const interval = Math.ceil(this.deps.workerBootTimeoutMs / 1000);
    return interval * (this.deps.workerBootRegistrationDeadlineIntervals + 1);
  }

  /** The pod's workspace exists only on its mounted tree volume, so the adoption runs inside the
   * pod: every assignment -- the first, and a live idle worker re-prompted over its stream,
   * whatever role's work the previous phase left undescribed -- asks the pod's shim to run the
   * shared command (`runAdoptWorkingCopy`) under the assigned role's identity, as a shim frame
   * over the registered stream
   * (`WorkerRpcClient.adoptWorkingCopy`). The shim's failure is this method's failure, exactly
   * as a failed `jj metaedit` is the tmux runtime's, so `ProcessManager`'s refused-prompt path
   * applies. A role whose stream is not registered cannot be prompted either; that is a
   * failure here, not a silent skip. */
  async adoptWorkingCopy(
    issue: IssueKey,
    role: LegionRole,
    identity: JjIdentity,
    timeoutMs: number
  ): Promise<void> {
    const token = roleToken(this.deps.project, issue, role);
    const client = this.deps.listener().registrations.get(token);
    if (!client) {
      throw new Error(
        `Could not adopt ${issue}'s working copy for ${role}: no worker stream is registered for ${token}`
      );
    }
    try {
      await client.adoptWorkingCopy(identity, timeoutMs);
    } catch (error) {
      throw new Error(
        `Could not adopt ${issue}'s working copy for ${role}: ${describeError(error)}`
      );
    }
  }

  /** Create-if-missing on every spawn (spec acceptance 5); a volume mounted again after the sweep
   * marked it unreferenced has the mark removed, so its retention clock restarts only when it is
   * unreferenced again. Two first spawns of a brand-new tree at once both read 404 and both
   * create; the API server refuses the second with 409 AlreadyExists, which means exactly what
   * the read path's success means -- the volume exists, freshly made, with no mark to clear -- so
   * it is tolerated here, where the read-then-create lives. Any other failure propagates:
   * admission fails loudly and `ProcessManager` counts the launch failure. */
  private async ensurePvc(name: string, tree: IssueKey): Promise<void> {
    const { client, config, project } = this.deps;
    try {
      const existing = await client.persistentVolumeClaims.get(name);
      if (existing.metadata.annotations?.[UNREFERENCED_SINCE_ANNOTATION] !== undefined) {
        await client.persistentVolumeClaims.patchAnnotations(name, {
          [UNREFERENCED_SINCE_ANNOTATION]: null,
        });
      }
      return;
    } catch (error) {
      if (!isApiStatus(error, 404)) throw error;
    }
    try {
      await client.persistentVolumeClaims.create(
        buildPvcManifest({
          name,
          project,
          tree,
          storageClass: config.storageClass,
          size: config.treeVolume,
        })
      );
    } catch (error) {
      if (!isApiStatus(error, 409)) throw error;
    }
  }

  /** Every pod still carrying this (issue, role) -- an older generation, or a same-name leftover
   * of a crashed attempt -- is asked to go (a graceful delete unless it is already terminating),
   * awaited until the API stops returning it, force-deleted at grace 0 if it outlives the stop
   * timeout, and its Secret removed once it is gone. A pod that survives even the forced delete
   * fails the spawn: two generations must never share the working copy. */
  private async retirePreviousPods(
    issue: IssueKey,
    role: LegionRole,
    graceSeconds: number
  ): Promise<void> {
    const { client, project, workerStopTimeoutMs } = this.deps;
    const previous = await client.pods.list(
      podSelector({
        [LABEL_PROJECT]: project,
        [LABEL_ISSUE]: labelValue(issue),
        [LABEL_ROLE]: role,
      })
    );
    for (const pod of previous) {
      const name = pod.metadata.name;
      if (pod.metadata.deletionTimestamp === undefined) {
        await this.deletePodTolerating404(name, { gracePeriodSeconds: graceSeconds });
      }
      if (!(await this.awaitPodGone(name, workerStopTimeoutMs))) {
        await this.deletePodTolerating404(name, { gracePeriodSeconds: 0 });
        if (!(await this.awaitPodGone(name, workerStopTimeoutMs))) {
          throw new Error(
            `previous pod ${name} is still present ${workerStopTimeoutMs / 1000}s after a grace-0 delete`
          );
        }
      }
      await this.deleteSecretTolerating404(name);
    }
  }

  private async deleteSecretTolerating404(name: string): Promise<void> {
    try {
      await this.deps.client.secrets.delete(name);
    } catch (error) {
      if (!isApiStatus(error, 404)) throw error;
    }
  }

  private async deletePodTolerating404(
    name: string,
    options: { gracePeriodSeconds: number; uid?: string }
  ): Promise<void> {
    try {
      await this.deps.client.pods.delete(name, options);
    } catch (error) {
      if (!isApiStatus(error, 404)) throw error;
    }
  }

  /** Polls `get` every `PREVIOUS_POD_POLL_MS` until a 404, for at most `timeoutMs` of polling --
   * counted in polls, not wall clock, so an injected no-op `sleep` still bounds the wait. */
  private async awaitPodGone(name: string, timeoutMs: number): Promise<boolean> {
    for (let waited = 0; waited < timeoutMs; waited += PREVIOUS_POD_POLL_MS) {
      try {
        await this.deps.client.pods.get(name);
      } catch (error) {
        if (isApiStatus(error, 404)) return true;
        throw error;
      }
      await this.sleep(PREVIOUS_POD_POLL_MS);
    }
    return false;
  }

  /**
   * Pod phase, container state, and the live stream registration map, folded into one verdict:
   * 404 -> gone; a uid other than the recorded one -> not-recorded-process (something wears the
   * name, but not this process); a deletionTimestamp, Succeeded, or Failed -> gone (a Failed
   * pod's failing container has its log tail quoted first). Pending is where the pod's own
   * `workspace-init` init container runs, and a live one is a live process whatever its age --
   * the tmux runtime's `spawn` does that same provisioning in-process before its watchdog is
   * even armed, and its probe re-arms on a live pane -- so a Pending pod with a Running init
   * container is alive, one whose init container terminated non-zero is gone (log tail quoted;
   * `restartPolicy: Never` turns it Failed moments later), and only a Pending pod that is not
   * initialising (unscheduled, image pull, volume mount) is gone past the boot timeout, its
   * events quoted (the boot watchdog's existing path retires it and `stop` deletes it). Running
   * -- registered stream or not, since a booting or redialing shim is not death -> alive;
   * `Unknown` -> unknown. An API failure is never a death verdict: alive when the pod's stream
   * is registered (live proof), else unknown.
   *
   * `ProcessManager`'s boot watchdog still bounds an initialising pod: it re-arms on every alive
   * verdict for at most `worker_boot_registration_deadline_intervals` intervals of
   * `worker_boot_timeout_seconds` (default 3 x 120 s) before retiring the boot -- which deletes
   * the pod, releases any repository lock its init container was holding or waiting on, and
   * spawns the next generation. The pod's own `workspace-init` lock wait is sized from that same
   * deadline by `spawn` (`LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS`, see
   * `workspaceInitLockWaitSeconds`), so the init container never gives up on a wait the daemon
   * would still tolerate.
   */
  async probe(locator: Locator): Promise<ProbeResult> {
    const target = kubernetesLocator(locator);
    let pod: K8sPod;
    try {
      pod = await this.deps.client.pods.get(target.podName);
    } catch (error) {
      if (isApiStatus(error, 404)) return { status: "dead", reason: "gone" };
      return this.deps.listener().registrations.has(target.roleToken)
        ? { status: "alive" }
        : { status: "unknown" };
    }
    if (pod.metadata.uid !== target.podUid) {
      return {
        status: "dead",
        reason: "not-recorded-process",
        detail: `pod ${target.podName} is uid ${pod.metadata.uid} (recorded ${target.podUid})`,
      };
    }
    if (pod.metadata.deletionTimestamp !== undefined) return { status: "dead", reason: "gone" };
    const phase = pod.status?.phase;
    if (phase === "Succeeded" || phase === "Failed") {
      if (phase === "Failed") await this.logFailedContainerTail(pod);
      return { status: "dead", reason: "gone" };
    }
    if (phase === "Pending") {
      const initContainers = pod.status?.initContainerStatuses ?? [];
      if (initContainers.some((status) => status.state?.running !== undefined)) {
        return { status: "alive" };
      }
      if (initContainerFailed(pod)) {
        await this.logFailedContainerTail(pod);
        return { status: "dead", reason: "gone" };
      }
      // A pod whose init containers have all finished (exit 0) is a pod whose main container is
      // starting: its boot began when the last init container finished, not when the pod was
      // created -- a clone that took most of the boot timeout must not count against the main
      // container's own start. Only a pod that has not started initialising (unscheduled, image
      // pull, volume mount) is aged from its creation.
      const initFinishedAt = initContainersFinishedAt(pod);
      const age = ageMs(this.deps.now(), initFinishedAt ?? pod.metadata.creationTimestamp);
      if (age >= this.deps.workerBootTimeoutMs) {
        await this.logPendingEvents(
          pod.metadata.name,
          initFinishedAt === undefined
            ? `pending for ${Math.floor(age / 1000)}s`
            : `main container not started ${Math.floor(age / 1000)}s after its init containers finished`
        );
        return { status: "dead", reason: "gone" };
      }
      return { status: "alive" };
    }
    if (phase === "Running") return { status: "alive" };
    return { status: "unknown" };
  }

  /** Best-effort: a log read that fails is itself logged, never thrown -- the verdict stands. */
  private async logFailedContainerTail(pod: K8sPod): Promise<void> {
    const name = pod.metadata.name;
    const container = initContainerFailed(pod) ? INIT_CONTAINER : MAIN_CONTAINER;
    try {
      const text = await this.deps.client.pods.log(name, container, LOG_TAIL_LINES);
      this.log(`[legion] pod ${name} Failed: last lines of ${container}:\n${text}`);
    } catch (error) {
      this.log(
        `[legion] pod ${name} Failed; its ${container} log could not be read: ${describeError(error)}`
      );
    }
  }

  private async logPendingEvents(name: string, measured: string): Promise<void> {
    const timeout = Math.floor(this.deps.workerBootTimeoutMs / 1000);
    let events = "";
    try {
      const list = await this.deps.client.events.list(name);
      events = list
        .map((event) => `${event.type ?? ""} ${event.reason ?? ""}: ${event.message ?? ""}`)
        .join(" | ");
    } catch (error) {
      events = `(events could not be read: ${describeError(error)})`;
    }
    this.log(
      `[legion] pod ${name} ${measured} (worker_boot_timeout_seconds ${timeout}); events: ${events}`
    );
  }

  /** Waits for the pod's shim to register under the locator's claim token. Never dials. */
  async connect(locator: Locator, timeoutMs?: number): Promise<WorkerRpcClient> {
    const target = kubernetesLocator(locator);
    return await this.deps
      .listener()
      .awaitRegistration(target.roleToken, timeoutMs ?? this.deps.workerRpcTimeoutMs());
  }

  /**
   * One shutdown frame over the registered stream (unless `skipGraceful`), bounded by
   * `timeoutMs`; a confirmed close does not skip the delete, since a finished pod object stays
   * until deleted. `refuseKill` ends here. The delete carries `preconditions.uid`: Kubernetes
   * itself refuses (409) to remove a pod that is not the recorded one, which is logged and
   * treated as already gone -- the Secret is left alone, since the stranger may be a newer
   * generation's. A 404 is already gone; then the per-pod Secret goes. Any other failure of
   * either delete is `ProcessStopFailed`: the stop is unconfirmed, and the caller retries later.
   */
  async stop(
    locator: Locator,
    timeoutMs: number,
    options?: { skipGraceful?: boolean; refuseKill?: boolean }
  ): Promise<void> {
    const target = kubernetesLocator(locator);
    if (!options?.skipGraceful) {
      const client = this.deps.listener().registrations.get(target.roleToken);
      // An absent dep hands `awaitShutdown` its cancellable real timer, exactly as tmux does.
      if (client) await awaitShutdown(client, timeoutMs, this.deps.sleep);
    }
    if (options?.refuseKill) return;
    try {
      await this.deps.client.pods.delete(target.podName, {
        gracePeriodSeconds: options?.skipGraceful ? 0 : Math.ceil(timeoutMs / 1000),
        uid: target.podUid,
      });
    } catch (error) {
      if (isApiStatus(error, 409)) {
        this.log(
          `[legion] not deleting pod ${target.podName}, treating it as already gone: its uid is not the recorded ${target.podUid}`
        );
        return;
      }
      if (!isApiStatus(error, 404)) {
        throw new ProcessStopFailed(
          locator,
          `delete pods/${target.podName} failed: ${describeError(error)}`
        );
      }
    }
    try {
      await this.deps.client.secrets.delete(target.podName);
    } catch (error) {
      if (!isApiStatus(error, 404)) {
        throw new ProcessStopFailed(
          locator,
          `delete secrets/${target.podName} failed: ${describeError(error)}`
        );
      }
    }
  }

  /**
   * Best-effort passes over everything labelled `legion.dev/project=<project>` (one object's
   * failure is logged and the pass continues): a pod not in `known` and older than `graceMs` is
   * deleted together with its per-pod Secret -- the Secret is named as the pod is, so the sweep
   * never lists Secrets (the daemon's service account has no `secrets: list`: a list would return
   * every Secret's data in the namespace, the providers Secret's provider keys included, and the
   * daemon must never hold those); a PVC not in `known` is marked
   * `legion.dev/unreferenced-since` once it is older than the grace and deleted once that mark is
   * `PVC_RETENTION_MS` old, while a PVC that is in `known` again has a stale mark removed (a sweep
   * between a spawn's PVC create and its locator persisting would otherwise leave one behind).
   */
  async reconcileOrphans(known: ReadonlySet<string>, graceMs: number): Promise<void> {
    const { client, project, workerStopTimeoutMs } = this.deps;
    const now = this.deps.now();
    const selector = podSelector({ [LABEL_PROJECT]: project });
    const graceSeconds = Math.ceil(workerStopTimeoutMs / 1000);

    for (const pod of await this.listOrNone(() => client.pods.list(selector), "pods")) {
      const name = pod.metadata.name;
      if (
        known.has(name) ||
        pod.metadata.deletionTimestamp !== undefined ||
        ageMs(now, pod.metadata.creationTimestamp) < graceMs
      ) {
        continue;
      }
      try {
        await client.pods.delete(name, { gracePeriodSeconds: graceSeconds });
      } catch (error) {
        this.log(`[legion] orphan sweep: failed to delete pod ${name}: ${describeError(error)}`);
      }
      try {
        await this.deleteSecretTolerating404(name);
      } catch (error) {
        this.log(`[legion] orphan sweep: failed to delete Secret ${name}: ${describeError(error)}`);
      }
    }

    for (const pvc of await this.listOrNone(
      () => client.persistentVolumeClaims.list(selector),
      "persistentvolumeclaims"
    )) {
      const name = pvc.metadata.name;
      const since = pvc.metadata.annotations?.[UNREFERENCED_SINCE_ANNOTATION];
      try {
        if (known.has(name)) {
          if (since !== undefined) {
            await client.persistentVolumeClaims.patchAnnotations(name, {
              [UNREFERENCED_SINCE_ANNOTATION]: null,
            });
          }
        } else if (since === undefined) {
          if (ageMs(now, pvc.metadata.creationTimestamp) >= graceMs) {
            await client.persistentVolumeClaims.patchAnnotations(name, {
              [UNREFERENCED_SINCE_ANNOTATION]: new Date(now).toISOString(),
            });
          }
        } else if (ageMs(now, since) >= PVC_RETENTION_MS) {
          await client.persistentVolumeClaims.delete(name);
        }
      } catch (error) {
        this.log(`[legion] orphan sweep: failed on PVC ${name}: ${describeError(error)}`);
      }
    }
  }

  private async listOrNone<T>(list: () => Promise<T[]>, resource: string): Promise<T[]> {
    try {
      return await list();
    } catch (error) {
      this.log(`[legion] orphan sweep: failed to list ${resource}: ${describeError(error)}`);
      return [];
    }
  }
}

/**
 * The main container's environment: every defined entry of the spec's env, then each key whose
 * value is a daemon-machine path re-pointed at its location inside the pod -- with the consumer
 * each one serves:
 * - `PATH`: the volume's `worker-bin` (the `gh` shim `legion workspace-init` installs) first,
 *   then the image's PATH; `legion gh` strips it again (`pathWithoutWorkerBin`).
 * - `GH_CONFIG_DIR`: `<volume>/gh`, written by `gh` itself.
 * - `LEGION_GRANT_FILE`: under the memory-backed `grant` emptyDir; the pi-envoy extension writes
 *   it before every bash command, `legion credential`/`gh`/`handoff complete` read it.
 * - `LEGION_STATE_DIR`: the volume (the extension's jj attribution overlay lives under it).
 * - `LEGION_CREDENTIAL_HELPER`: the value `workspace-init` wrote into the clone's git config.
 * - `DISPATCH_TOKEN_FILE` (only when the spec carried one): the providers Secret's
 *   `DISPATCH_TOKEN` file, whose trimmed contents `resolveDispatchConfig` reads.
 * - `OMP_SESSION_STORAGE` / `OMP_SESSION_SQL_DSN_FILE` (only under `session_store: postgres`):
 *   Oh My Pi's own session-store variables; the file is the providers Secret's `session_dsn_secret`
 *   key — the same Secret `DISPATCH_TOKEN_FILE` points into. Every pod of a tree passes through
 *   here, so this is the one place the store shapes a pod.
 * - `LEGION_ROOT_WORKSPACE` / `LEGION_WORKSPACE`: the issue's working copy on the volume, the
 *   main container's `workingDir`.
 * - `<NAME>_FILE` for every secret in the spec (`secretPointers`, from `podSecrets`):
 *   `LEGION_BOOT_TOKEN_FILE` always, the per-pod Secret's projection into `BOOT_DIR`;
 *   `ENVOY_TOKEN_FILE` when the daemon has an Envoy bearer — the providers mount's own file.
 * Everything else passes through unchanged.
 */
/** The per-pod Secret's keys (`projected`) and each secret's `<NAME>_FILE` value (`pointers`):
 * every spec secret is projected under `BOOT_DIR`, except `ENVOY_TOKEN`, which is the providers
 * mount's own file (see `spawn`). */
export function podSecrets(secrets: SpawnSpec["secrets"]): {
  projected: Record<string, string>;
  pointers: Record<string, string>;
} {
  const projected: Record<string, string> = {};
  const pointers: Record<string, string> = {};
  for (const [name, value] of Object.entries(secrets)) {
    if (name === "ENVOY_TOKEN") {
      pointers.ENVOY_TOKEN_FILE = `${PROVIDERS_DIR}/ENVOY_TOKEN`;
      continue;
    }
    projected[name] = value;
    pointers[`${name}_FILE`] = `${BOOT_DIR}/${name}`;
  }
  return { projected, pointers };
}

function podEnvironment(
  kind: "root" | "worker",
  env: Record<string, string | undefined>,
  token: string,
  workspaceDir: string,
  secretPointers: Record<string, string>,
  sessionStore: SessionStore
): Record<string, string> {
  const result: Record<string, string> = {};
  for (const [name, value] of Object.entries(env)) {
    if (value !== undefined) result[name] = value;
  }
  result.PATH = `${workerBinDir(TREE_MOUNT)}${path.delimiter}${IMAGE_PATH}`;
  result.GH_CONFIG_DIR = `${TREE_MOUNT}/gh`;
  result.LEGION_GRANT_FILE = `${GRANT_DIR}/${grantSecretName(token)}`;
  result.LEGION_STATE_DIR = TREE_MOUNT;
  result.LEGION_CREDENTIAL_HELPER = POD_CREDENTIAL_HELPER;
  if (env.DISPATCH_TOKEN_FILE !== undefined) {
    result.DISPATCH_TOKEN_FILE = `${PROVIDERS_DIR}/DISPATCH_TOKEN`;
  }
  if (sessionStore.kind === "postgres") {
    result[SESSION_STORAGE_VARIABLE] = "sql";
    result[SESSION_SQL_DSN_FILE_VARIABLE] = `${PROVIDERS_DIR}/${sessionStore.dsnSecretKey}`;
  }
  result[kind === "root" ? "LEGION_ROOT_WORKSPACE" : "LEGION_WORKSPACE"] = workspaceDir;
  Object.assign(result, secretPointers);
  return result;
}
