import { randomBytes } from "node:crypto";
import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import { z } from "zod";
import {
  type BootProbeOptions,
  ProbeAbortedError,
  type ProbeOutcome,
  retryBootProbe,
  SESSION_STORAGE_PROBE_MARK,
} from "./boot-probes";
import type { KubernetesScheduling, RoleResources, SessionStoreName } from "./config";
import type { ImageDigestRef } from "./image-ref";
import { K8sApiError, type K8sClient, type K8sPod } from "./k8s-client";
import {
  k8sResources,
  k8sSlug,
  LABEL_PROJECT,
  LEGION_BINARY,
  labelValue,
  PROVIDERS_DIR,
  providersSecretName,
} from "./k8s-manifests";

/**
 * The in-cluster daemon's boot probe (spec acceptance 2). A daemon in a pod is not the image its
 * workers run at the configured digest, and it has no local OMP to probe, so it runs `legion
 * probe-image --daemon-api-version <N>` inside a one-shot pod of that image: the image's own CLI
 * runs the two OMP probes `verifyOmpAgentsCapability`/`verifyLegionPluginLoaded` run on a tmux
 * host and compares the image plugin's `legion.daemonApiVersion` to this daemon's `<N>`. Only a
 * pod on this cluster proves that this cluster can pull the image and mount the providers Secret
 * with this service account. The pod carries no tree volume and no shim; it is `restartPolicy:
 * Never`, awaited under `slow_command_timeout_seconds`, its log read, and always deleted.
 *
 * The result is remembered per (digest, contract, and — under `session_store: postgres` — the
 * confirmed session-storage marker) in `<state_dir>/image-probes/<hex>.json`, so a crash-restart
 * loop never launches a second probe pod for a digest that already passed at this daemon's
 * contract; only a pass is cached (a definitive failure exits the process, and the next boot must
 * prove the fix). The retry policy and launch hold are the tmux probes' own (`retryBootProbe`,
 * `DAEMON_PROBE_RETRY`): a pod still Pending at the budget is transient, a `Failed` pod is
 * definitive.
 *
 * Under `session_store: postgres` the pods' Oh My Pi must carry the `session.storage` setting, or
 * it would ignore the two variables and silently keep sessions on files. That is proved in exactly
 * one place — inside the image, by `legion probe-image`'s third probe — and confirmed here by the
 * `SESSION_STORAGE_PROBE_MARK` token on its OK line (never by probing a host build): an image whose
 * output lacks it is refused under postgres, exactly as one lacking `daemon-api-version=<N>` is.
 * Under `pvc` the marker is not required, but its presence is still recorded
 * (`sessionStorageProbed`), so a pass cached under `pvc` on a carrying image is reusable under
 * postgres, while one cached before the field existed (absent = not confirmed) is not.
 */

export const LABEL_PROBE = "legion.dev/probe";
export const PROBE_CONTAINER = "probe";
/** How often the pod is polled while it runs; `timeoutMs / POLL_INTERVAL_MS` bounds the polls, so
 * a no-op injected sleep still terminates. */
const POLL_INTERVAL_MS = 2_000;
const LOG_TAIL_LINES = 50;
/** Container waiting reasons no retry changes: the image reference itself is unusable. */
const DEFINITIVE_WAITING_REASONS: Record<string, true> = {
  InvalidImageName: true,
  ErrImageNeverPull: true,
};

export const ImageProbeCacheSchema = z.strictObject({
  digest: z.string().regex(/^sha256:[0-9a-f]{64}$/),
  daemonApiVersion: z.number().int().positive(),
  probedAt: z.string().datetime(),
  /** `true` only when the probe pod's log carried `SESSION_STORAGE_PROBE_MARK`; absent (every
   * entry written before the field, and every pass on an older image) means not confirmed. */
  sessionStorageProbed: z.boolean().optional(),
});
export type ImageProbeCache = z.infer<typeof ImageProbeCacheSchema>;

/** `<stateDir>/image-probes/<64 hex>.json` — the digest without its `sha256:` prefix, since a
 * colon in a file name is awkward; the file's own `digest` field carries the full reference. */
export function imageProbeCachePath(stateDir: string, digest: ImageDigestRef["digest"]): string {
  return path.join(stateDir, "image-probes", `${digest.slice("sha256:".length)}.json`);
}

/** `legion-probe-<project>-<first 12 hex of the digest>`: one name per project and image, so a
 * leftover from this daemon's crashed boot is found by name and replaced rather than
 * accumulating, and two projects' daemons in one namespace probing the same digest never contend
 * for one pod. Bounded to 63 characters (a label value's ceiling, kept for the name too): the
 * project part is slugged to fit. */
export function probePodName(project: string, digest: ImageDigestRef["digest"]): string {
  const hex = digest.slice("sha256:".length, "sha256:".length + 12);
  return `legion-probe-${k8sSlug(project, 63 - "legion-probe-".length - 1 - hex.length)}-${hex}`;
}

export interface ProbePodInput {
  project: string;
  namespace: string;
  image: ImageDigestRef;
  daemonApiVersion: number;
  /** The `small` profile: the probe runs OMP twice and exits. */
  resources: RoleResources;
  scheduling: KubernetesScheduling;
}

/** The worker pod's shape (`buildPodManifest`) minus everything a probe has no use for — tree PVC,
 * init container, boot/provision Secrets, shim, grant dir. The providers Secret is mounted
 * read-only exactly as a worker mounts it: a missing Secret fails the pod's mount, which is one of
 * the things the probe exists to prove. */
export function buildProbePodManifest(input: ProbePodInput): K8sPod {
  return {
    apiVersion: "v1",
    kind: "Pod",
    metadata: {
      name: probePodName(input.project, input.image.digest),
      namespace: input.namespace,
      labels: { [LABEL_PROJECT]: labelValue(input.project), [LABEL_PROBE]: "image" },
      annotations: { "karpenter.sh/do-not-disrupt": "true" },
    },
    spec: {
      restartPolicy: "Never",
      terminationGracePeriodSeconds: 5,
      automountServiceAccountToken: false,
      enableServiceLinks: false,
      securityContext: { runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000 },
      ...(Object.keys(input.scheduling.nodeSelector).length > 0
        ? { nodeSelector: input.scheduling.nodeSelector }
        : {}),
      ...(input.scheduling.tolerations.length > 0
        ? { tolerations: input.scheduling.tolerations }
        : {}),
      ...(input.scheduling.priorityClassName
        ? { priorityClassName: input.scheduling.priorityClassName }
        : {}),
      volumes: [
        {
          name: "providers",
          secret: { secretName: providersSecretName(input.project), defaultMode: 0o440 },
        },
      ],
      containers: [
        {
          name: PROBE_CONTAINER,
          image: input.image.reference,
          command: [
            LEGION_BINARY,
            "probe-image",
            "--daemon-api-version",
            String(input.daemonApiVersion),
          ],
          volumeMounts: [{ name: "providers", mountPath: PROVIDERS_DIR, readOnly: true }],
          resources: k8sResources(input.resources),
        },
      ],
    },
  };
}

/** The cache entry at `file` for `digest`, or `undefined` when there is none. A file that exists
 * but cannot be read, does not validate, or names another image (the path is derived from the
 * digest, so that is a corrupt or misplaced file) is logged (naming the file) and treated as
 * absent: the pod runs again and rewrites it. */
export async function readImageProbeCache(
  file: string,
  digest: ImageDigestRef["digest"],
  log: (line: string) => void
): Promise<ImageProbeCache | undefined> {
  let text: string;
  try {
    text = await readFile(file, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    log(`[legion] ignoring worker image probe cache ${file}: ${describe(error)}`);
    return undefined;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch (error) {
    log(`[legion] ignoring worker image probe cache ${file}: ${describe(error)}`);
    return undefined;
  }
  const result = ImageProbeCacheSchema.safeParse(parsed);
  if (!result.success) {
    log(`[legion] ignoring worker image probe cache ${file}: ${result.error.message}`);
    return undefined;
  }
  if (result.data.digest !== digest) {
    log(
      `[legion] ignoring worker image probe cache ${file}: it records image ${result.data.digest}, this daemon runs ${digest}`
    );
    return undefined;
  }
  return result.data;
}

/** tmp + rename in the cache directory (created as needed): a crash mid-write never leaves a
 * half-written file the next boot would have to log and ignore. */
async function writeImageProbeCache(file: string, entry: ImageProbeCache): Promise<void> {
  await mkdir(path.dirname(file), { recursive: true });
  const temporary = `${file}.${randomBytes(6).toString("hex")}.tmp`;
  await writeFile(temporary, `${JSON.stringify(entry, null, 2)}\n`, "utf8");
  await rename(temporary, file);
}

export interface VerifyWorkerImageDeps {
  client: K8sClient;
  project: string;
  namespace: string;
  image: ImageDigestRef;
  resources: RoleResources;
  scheduling: KubernetesScheduling;
  stateDir: string;
  /** `LEGION_DAEMON_API_VERSION`: the contract this daemon speaks, which the image's plugin must too. */
  daemonApiVersion: number;
  /** `runtime.kubernetes.session_store`: under `postgres` the probe pod's log must carry
   * `SESSION_STORAGE_PROBE_MARK`. */
  sessionStore: SessionStoreName;
  now(): number;
  log(line: string): void;
}

function describe(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** The container's waiting reason, when the pod reports one (`ImagePullBackOff`, …). */
function waitingReason(pod: K8sPod): string | undefined {
  return pod.status?.containerStatuses?.find((status) => status.name === PROBE_CONTAINER)?.state
    ?.waiting?.reason;
}

/**
 * Runs the probe pod for `deps.image` until it passes, fails definitively, or (under a bounded
 * policy) exhausts its attempts — the same driver and outcome vocabulary as the tmux daemon's two
 * OMP probes, so `startDaemon`'s launch hold treats both alike. See the module comment.
 */
export async function verifyWorkerImage(
  deps: VerifyWorkerImageDeps,
  options: BootProbeOptions
): Promise<void> {
  const { image, daemonApiVersion } = deps;
  const cacheFile = imageProbeCachePath(deps.stateDir, image.digest);
  // The cache is a verdict already reached for this digest at this contract (and, under postgres,
  // with the marker confirmed), read once: a hit skips the retry loop entirely, so the attempt
  // below is exactly "run the pod".
  const cached = await readImageProbeCache(cacheFile, image.digest, deps.log);
  if (cached !== undefined) {
    const stale =
      cached.daemonApiVersion !== daemonApiVersion
        ? `it records daemon API contract ${cached.daemonApiVersion}, this daemon speaks ${daemonApiVersion}`
        : deps.sessionStore === "postgres" && cached.sessionStorageProbed !== true
          ? "it records no session-storage probe, and this daemon runs session_store: postgres"
          : undefined;
    if (stale === undefined) {
      deps.log(
        `[legion] worker image ${image.digest} passed its probe at ${cached.probedAt} (daemon API contract ${daemonApiVersion}); reusing ${cacheFile}`
      );
      return;
    }
    deps.log(`[legion] ignoring worker image probe cache ${cacheFile}: ${stale}`);
  }
  await retryBootProbe(
    "worker image",
    async (): Promise<ProbeOutcome> => {
      const outcome = await runProbePod(deps, options);
      if (outcome.passed) {
        await writeImageProbeCache(cacheFile, {
          digest: image.digest,
          daemonApiVersion,
          probedAt: new Date(deps.now()).toISOString(),
          ...(outcome.sessionStorageProbed ? { sessionStorageProbed: true } : {}),
        });
      }
      return outcome;
    },
    async (detail, reason) =>
      new Error(
        reason === "exhausted"
          ? `[legion] worker image probe never completed within its retry budget (${options.retry.maxAttempts} attempts) for ${image.digest}${detail ? `: ${detail}` : ""}`
          : `[legion] worker image ${image.digest} failed its probe: ${detail}`
      ),
    options.retry,
    options.sleep,
    options.signal
  );
}

/** Whether an API failure settles the probe: a request the API server refused for what we sent
 * (400/401/403/422 — a malformed request, RBAC, credentials, a manifest it rejects) will refuse it
 * again, and so will a create that 404s (the namespace does not exist), so the boot stops naming
 * it; anything else (a 5xx, a network failure) is the cluster's moment, retried with the probe's
 * backoff. A pod that vanished mid-poll is judged by the poll itself, not here. */
function apiFailureOutcome(
  error: unknown,
  doing: "creating" | "reading",
  name: string
): ProbeOutcome {
  const definitive =
    error instanceof K8sApiError &&
    (error.status === 400 ||
      error.status === 401 ||
      error.status === 403 ||
      error.status === 422 ||
      (doing === "creating" && error.status === 404));
  return { passed: false, definitive, detail: `${doing} probe pod ${name}: ${describe(error)}` };
}

/** What one poll of the probe pod settled: a terminal pod to judge by its log, or a verdict the
 * pod's state already gave (a definitive waiting reason; the pod vanished). */
type ProbePollVerdict = { pod: K8sPod } | { detail: string; definitive: boolean };

/** One pod run's outcome; a pass also says whether the log carried the session-storage marker. */
type ProbePodOutcome = ProbeOutcome & { readonly sessionStorageProbed?: boolean };

/** One pod run: create (replacing this project's leftover of the same name), poll to a terminal
 * phase within the budget, read the log tail, delete. Returns the attempt's outcome; an API
 * failure is an outcome too (`apiFailureOutcome`), so the retry policy sees every answer. */
async function runProbePod(
  deps: VerifyWorkerImageDeps,
  options: BootProbeOptions
): Promise<ProbePodOutcome> {
  const manifest = buildProbePodManifest(deps);
  const name = manifest.metadata.name;
  const refused = await createProbePod(deps, options, manifest);
  if (refused !== undefined) return refused;
  try {
    return await awaitProbeVerdict(deps, options, name);
  } finally {
    try {
      await deps.client.pods.delete(name, { gracePeriodSeconds: 0 });
    } catch (error) {
      if (!(error instanceof K8sApiError && error.status === 404)) {
        deps.log(`[legion] failed to delete probe pod ${name}: ${describe(error)}`);
      }
    }
  }
}

/** Creates the probe pod; `undefined` once it exists. A 409 is this daemon's own leftover only when
 * the existing pod carries this project's label — the name is project-scoped, so anything else is
 * another deployment's pod that happens to share the name (a label the operator set by hand), not
 * ours to delete — in which case it is deleted, awaited gone, and the create repeated. Any other
 * API failure is the attempt's outcome. */
async function createProbePod(
  deps: VerifyWorkerImageDeps,
  options: BootProbeOptions,
  manifest: K8sPod
): Promise<ProbeOutcome | undefined> {
  const { client, image } = deps;
  const name = manifest.metadata.name;
  try {
    try {
      await client.pods.create(manifest);
      return undefined;
    } catch (error) {
      if (!(error instanceof K8sApiError) || error.status !== 409) throw error;
    }
    const existing = await client.pods.get(name);
    const owner = existing.metadata.labels?.[LABEL_PROJECT];
    if (owner !== labelValue(deps.project)) {
      return {
        passed: false,
        definitive: true,
        detail: `probe pod ${name} already exists and belongs to ${owner === undefined ? "no Legion project" : `project ${owner}`}, not ${deps.project}; not deleting it`,
      };
    }
    deps.log(
      `[legion] deleting leftover probe pod ${name} (from an earlier boot) before probing worker image ${image.digest}`
    );
    await client.pods.delete(name, { gracePeriodSeconds: 0 });
    await awaitPodGone(client, name, options);
    await client.pods.create(manifest);
    return undefined;
  } catch (error) {
    if (error instanceof ProbeAbortedError) throw error;
    return apiFailureOutcome(error, "creating", name);
  }
}

/** Polls the created pod to a verdict within the budget, then judges a terminal pod by its log. */
async function awaitProbeVerdict(
  deps: VerifyWorkerImageDeps,
  options: BootProbeOptions,
  name: string
): Promise<ProbePodOutcome> {
  const { client } = deps;
  // Each poll ends the wait with a terminal pod, or with a verdict the pod's state already gave
  // (`ImagePullBackOff` keeps waiting; `InvalidImageName` does not; a 404 is the pod gone).
  let verdict: ProbePollVerdict | undefined;
  try {
    verdict = await pollUntil(options, async (): Promise<ProbePollVerdict | undefined> => {
      let pod: K8sPod;
      try {
        pod = await client.pods.get(name);
      } catch (error) {
        // Gone between our create and this read: another actor deleted it (a sweep, an
        // operator). Nothing to judge — the next attempt creates it again.
        if (error instanceof K8sApiError && error.status === 404) {
          return { detail: `probe pod ${name} vanished before it finished`, definitive: false };
        }
        throw error;
      }
      const phase = pod.status?.phase;
      if (phase === "Succeeded" || phase === "Failed") return { pod };
      const reason = waitingReason(pod);
      if (reason !== undefined && DEFINITIVE_WAITING_REASONS[reason]) {
        return {
          detail: `pod ${name} ${phase ?? "Unknown"}, container ${PROBE_CONTAINER} waiting: ${reason}`,
          definitive: true,
        };
      }
      return undefined;
    });
  } catch (error) {
    return apiFailureOutcome(error, "reading", name);
  }
  if (options.signal?.aborted) {
    return { passed: false, definitive: false, aborted: true, detail: "" };
  }
  if (verdict === undefined) {
    // Still not terminal at the budget: re-read once for the message, never for a verdict.
    const last = await client.pods.get(name).catch(() => undefined);
    const reason = last === undefined ? undefined : waitingReason(last);
    return {
      passed: false,
      definitive: false,
      detail: `probe pod ${name} still ${last?.status?.phase ?? "Unknown"} after ${options.timeoutMs / 1000} s${reason === undefined ? "" : ` (container ${PROBE_CONTAINER} waiting: ${reason})`}`,
    };
  }
  if ("detail" in verdict) {
    return { passed: false, definitive: verdict.definitive, detail: verdict.detail };
  }
  const logTail = await client.pods
    .log(name, PROBE_CONTAINER, LOG_TAIL_LINES)
    .then((text) => text.trim())
    .catch((error: unknown) => `(log unavailable: ${describe(error)})`);
  return judgeProbeLog(deps, name, verdict.pod.status?.phase, logTail);
}

/** The verdict a terminal probe pod's log gives. The OK line must carry this daemon's contract:
 * citty ignores an unknown flag, so an image whose `legion` CLI predates `--daemon-api-version`
 * runs the two OMP probes, prints a bare `probe-image: OK (…)`, and exits 0 having checked no
 * contract at all. Confirm-before-serve means that image is refused, not waved through; one that
 * confirmed a different contract (the CLI's own check disagreeing with ours) is refused naming
 * both. Under `session_store: postgres` the line must also carry `SESSION_STORAGE_PROBE_MARK`
 * (matched by `includes`, since the OK line's suffix order is the CLI's): an image whose CLI or
 * Oh My Pi predates the session-storage probe would run the pods on files. */
function judgeProbeLog(
  deps: VerifyWorkerImageDeps,
  name: string,
  phase: string | undefined,
  logTail: string
): ProbePodOutcome {
  const refuse = (detail: string): ProbeOutcome => ({ passed: false, definitive: true, detail });
  if (phase === "Failed") return refuse(`pod ${name} Failed — log tail: ${logTail}`);
  if (!logTail.includes("probe-image: OK")) {
    return refuse(`pod ${name} Succeeded without printing probe-image: OK — log tail: ${logTail}`);
  }
  const confirmed = /^probe-image: OK .*daemon-api-version=(\d+)$/m.exec(logTail)?.[1];
  if (confirmed === undefined) {
    return refuse(
      `pod ${name} Succeeded without confirming daemon API contract ${deps.daemonApiVersion} (its legion CLI predates the check) — log tail: ${logTail}`
    );
  }
  if (Number(confirmed) !== deps.daemonApiVersion) {
    return refuse(
      `pod ${name} Succeeded but confirmed daemon API contract ${confirmed}, this daemon requires ${deps.daemonApiVersion} — log tail: ${logTail}`
    );
  }
  const sessionStorageProbed = logTail.includes(SESSION_STORAGE_PROBE_MARK);
  if (deps.sessionStore === "postgres" && !sessionStorageProbed) {
    return refuse(
      `pod ${name} Succeeded without printing ${SESSION_STORAGE_PROBE_MARK}, which session_store: postgres requires (its Oh My Pi or legion CLI predates the session-storage setting) — log tail: ${logTail}`
    );
  }
  deps.log(`[legion] worker image ${deps.image.digest}: probe pod ${name} passed: ${logTail}`);
  return { passed: true, definitive: false, detail: "", sessionStorageProbed };
}

/** Calls `read` every `POLL_INTERVAL_MS` until it answers, for at most the probe budget
 * (`options.timeoutMs`, at least one call); `undefined` when the budget ran out or `options.signal`
 * fired first — the caller tells the two apart by the signal. `read`'s own errors propagate. */
async function pollUntil<T>(
  options: BootProbeOptions,
  read: () => Promise<T | undefined>
): Promise<T | undefined> {
  const maxPolls = Math.max(1, Math.floor(options.timeoutMs / POLL_INTERVAL_MS));
  for (let poll = 0; poll < maxPolls; poll++) {
    if (options.signal?.aborted) return undefined;
    const value = await read();
    if (value !== undefined) return value;
    await options.sleep(POLL_INTERVAL_MS);
  }
  return undefined;
}

/** Polls until the named pod 404s, bounded by the budget like the main wait. */
async function awaitPodGone(
  client: K8sClient,
  name: string,
  options: BootProbeOptions
): Promise<void> {
  const gone = await pollUntil(options, async () => {
    try {
      await client.pods.get(name);
      return undefined;
    } catch (error) {
      if (error instanceof K8sApiError && error.status === 404) return true;
      throw error;
    }
  });
  if (gone) return;
  if (options.signal?.aborted) throw new ProbeAbortedError("worker image");
  throw new Error(
    `[legion] leftover probe pod ${name} did not disappear within ${options.timeoutMs / 1000} s of its deletion`
  );
}
