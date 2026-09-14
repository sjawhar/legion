import { createHash } from "node:crypto";
import type { IssueKey, LegionRole } from "@legion/contracts";
import type { RoleResources } from "./config";
import type { K8sPersistentVolumeClaim, K8sPod, K8sSecret } from "./k8s-client";

export const LABEL_PROJECT = "legion.dev/project";
export const LABEL_TREE = "legion.dev/tree";
export const LABEL_ISSUE = "legion.dev/issue";
export const LABEL_ROLE = "legion.dev/role";
export const LABEL_GENERATION = "legion.dev/generation";
export const UNREFERENCED_SINCE_ANNOTATION = "legion.dev/unreferenced-since";

/** Container-side paths. The image fixes HOME=/home/legion and OMP_PROFILE=legion
 * (worker.Dockerfile:111-114); OMP's DirResolver puts the profile's sessions at
 * <HOME>/.omp/profiles/<profile>/agent/sessions (oh-my-pi packages/utils/src/dirs.ts:110-117,
 * 315-321, 872-874). */
export const TREE_MOUNT = "/legion";
export const OMP_SESSIONS_DIR = "/home/legion/.omp/profiles/legion/agent/sessions";
/** The tree volume's directory that is mounted at `OMP_SESSIONS_DIR` in the main container. */
export const SESSIONS_SUBPATH = "sessions";
export const PROVIDERS_DIR = "/var/run/legion/providers";
/** Oh My Pi's own session-store overrides (`session.storage` / `session.sql.dsnFile`, LEGION-80),
 * the two variables `podEnvironment` sets under `session_store: postgres`: the first selects the
 * store (`sql`), the second names a file whose trimmed contents are the Postgres URL — one key of
 * the providers Secret, under `PROVIDERS_DIR`. Defined here, the runtime leaf both `config.ts`
 * (its reserved-key check) and `boot-probes.ts` (its host probe) can import without a cycle. */
export const SESSION_STORAGE_VARIABLE = "OMP_SESSION_STORAGE";
export const SESSION_SQL_DSN_FILE_VARIABLE = "OMP_SESSION_SQL_DSN_FILE";
export const BOOT_DIR = "/var/run/legion/boot";
export const PROVISION_DIR = "/var/run/legion/provision";
export const GRANT_DIR = "/var/run/legion/grant";
export const IMAGE_PATH = "/opt/legion/bin:/opt/omp/bin:/usr/local/bin:/usr/bin:/bin"; // worker.Dockerfile:114
export const LEGION_BINARY = "/opt/legion/bin/legion"; // worker.Dockerfile:105
export const POD_CREDENTIAL_HELPER = `!${LEGION_BINARY} credential`;
export const BOOT_TOKEN_KEY = "LEGION_BOOT_TOKEN";
export const PROVISION_TOKEN_KEY = "LEGION_PROVISION_TOKEN";
export const INIT_CONTAINER = "workspace-init";
export const MAIN_CONTAINER = "worker";
/** Linux MAX_ARG_STRLEN: the largest single argv string an exec accepts. */
export const MAX_ARGV_STRING_BYTES = 131072;

/** lowercase, `[^a-z0-9-]` -> "-", collapse, trim "-"; over `maxLength`: head + "-" + 8 hex of
 * `sha256(value)`. */
export function k8sSlug(value: string, maxLength: number): string {
  const slug = value
    .toLowerCase()
    .replace(/[^a-z0-9-]/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-+|-+$/g, "");
  if (slug.length <= maxLength) return slug;
  const hash = createHash("sha256").update(value).digest("hex").slice(0, 8);
  return `${slug.slice(0, maxLength - 1 - hash.length)}-${hash}`;
}

export function podName(issue: IssueKey, role: LegionRole, generation: number): string {
  return `legion-${k8sSlug(issue, 38)}-${role}-g${generation}`;
}

export function pvcName(tree: IssueKey): string {
  return `legion-${k8sSlug(tree, 56)}`;
}

export function providersSecretName(project: string): string {
  return `legion-${project}-providers`;
}

/** `key.length <= 63 ? key : k8sSlug(key, 63)` — a label value's own 63-character ceiling, distinct
 * from a resource name's. */
export function labelValue(key: string): string {
  return key.length <= 63 ? key : k8sSlug(key, 63);
}

export function podLabels(input: {
  project: string;
  tree: IssueKey;
  issue: IssueKey;
  role: LegionRole;
  generation: number;
}): Record<string, string> {
  return {
    [LABEL_PROJECT]: input.project,
    [LABEL_TREE]: labelValue(input.tree),
    [LABEL_ISSUE]: labelValue(input.issue),
    [LABEL_ROLE]: input.role,
    [LABEL_GENERATION]: String(input.generation),
  };
}

export function podSelector(labels: Record<string, string>): string {
  return Object.entries(labels)
    .map(([key, value]) => `${key}=${value}`)
    .join(",");
}

/** `<TREE_MOUNT>/workspaces/<owner>/<repo>/<issue-lower>` — the path `provisionIssueWorkspace`
 * yields for `stateDir=/legion`. */
export function podWorkspaceDir(repo: `${string}/${string}`, issue: IssueKey): string {
  return `${TREE_MOUNT}/workspaces/${repo}/${issue.toLowerCase()}`;
}

/** A role profile's requests/limits in the API's shape (`ephemeralStorage` → `ephemeral-storage`);
 * shared with the probe pod (`worker-image-probe.ts`). */
export function k8sResources(resources: RoleResources): Record<string, Record<string, string>> {
  return {
    requests: {
      cpu: resources.requests.cpu,
      memory: resources.requests.memory,
      "ephemeral-storage": resources.requests.ephemeralStorage,
    },
    limits: {
      cpu: resources.limits.cpu,
      memory: resources.limits.memory,
      "ephemeral-storage": resources.limits.ephemeralStorage,
    },
  };
}

export interface PodManifestInput {
  project: string;
  tree: IssueKey;
  issue: IssueKey;
  role: LegionRole;
  generation: number;
  namespace: string;
  image: string;
  pvcName: string;
  podName: string;
  secretName: string;
  /** The per-pod Secret's keys projected into the main container's `BOOT_DIR`, one file each
   * (`LEGION_BOOT_TOKEN` always; every further secret the daemon delivers, e.g. `ENVOY_TOKEN`).
   * The provision token is projected for the init container separately. */
  secretKeys: readonly string[];
  resources: RoleResources;
  /** The main container's final environment (already re-pointed by the runtime). */
  env: Record<string, string>;
  workspaceDir: string;
  repo: `${string}/${string}`;
  shimEndpoint: string; // tcp://host:port
  /** The wrapped OMP argv: ["omp", ("--resume=<file>")?, "--mode", "rpc", "--append-system-prompt",
   * <text>, …]. */
  ompArgv: string[];
  terminationGracePeriodSeconds: number;
  /** `LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS` for the init container: how long its
   * `workspace-init` waits for another pod's lock on the shared clone, sized by the runtime from
   * the daemon's boot deadline (`KubernetesRuntime.workspaceInitLockWaitSeconds`). */
  workspaceInitLockWaitSeconds: number;
  /** The recorded OMP session file the main container resumes (`--resume=<file>`, a path under
   * `OMP_SESSIONS_DIR`), when this generation resumes one from the tree volume. The init container
   * is told where that file is on the volume (`LEGION_RESUME_SESSION_FILE`) and fails the pod if it
   * is missing: the pinned OMP given a missing `--resume` path exits 0 and runs as a fresh agent,
   * which would silently break the same-agent invariant the tmux runtime enforces with a `stat`.
   * Under `session_store: postgres` the transcript is a database row the init container cannot
   * stat, so the runtime passes none here while `ompArgv` still carries `--resume`. */
  resumeSessionFile?: string;
}

/** The tree-volume path of a main-container session file: `OMP_SESSIONS_DIR` is the volume's
 * `sessions` directory mounted at that path (`subPath: sessions`), so the init container, which
 * mounts the whole volume at `TREE_MOUNT`, sees the same file under `TREE_MOUNT/sessions`. A
 * recorded session path anywhere else cannot be resumed on this runtime (the pod's HOME is not
 * on the volume) and is refused here, before any API call. */
export function initContainerSessionPath(resumeSessionFile: string): string {
  const prefix = `${OMP_SESSIONS_DIR}/`;
  if (!resumeSessionFile.startsWith(prefix)) {
    throw new Error(
      `recorded OMP session file ${resumeSessionFile} is not under ${OMP_SESSIONS_DIR}, the only directory a pod persists sessions to; it cannot be resumed on this runtime`
    );
  }
  return `${TREE_MOUNT}/${SESSIONS_SUBPATH}/${resumeSessionFile.slice(prefix.length)}`;
}

export function buildPodManifest(input: PodManifestInput): K8sPod {
  input.ompArgv.forEach((value, index) => {
    const bytes = Buffer.byteLength(value, "utf8");
    if (bytes > MAX_ARGV_STRING_BYTES) {
      // Name the element: the value of a flag (`--append-system-prompt <text>` is the realistic
      // case) by the flag before it, anything else by its own head.
      const flag = index > 0 ? input.ompArgv[index - 1] : undefined;
      const element =
        flag?.startsWith("--") && !value.startsWith("--")
          ? `the value of ${flag}`
          : `argv element #${index} (${JSON.stringify(value.slice(0, 40))}…)`;
      throw new Error(
        `omp argv: ${element} is ${bytes} bytes; a single argv string may not exceed ` +
          `${MAX_ARGV_STRING_BYTES} (Linux MAX_ARG_STRLEN)`
      );
    }
  });

  const initEnv = [
    {
      name: "LEGION_PROVISION_TOKEN_FILE",
      value: `${PROVISION_DIR}/${PROVISION_TOKEN_KEY}`,
    },
    {
      name: "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS",
      value: String(input.workspaceInitLockWaitSeconds),
    },
    ...(input.resumeSessionFile === undefined
      ? []
      : [
          {
            name: "LEGION_RESUME_SESSION_FILE",
            value: initContainerSessionPath(input.resumeSessionFile),
          },
        ]),
  ];

  return {
    apiVersion: "v1",
    kind: "Pod",
    metadata: { name: input.podName, namespace: input.namespace, labels: podLabels(input) },
    spec: {
      restartPolicy: "Never",
      terminationGracePeriodSeconds: input.terminationGracePeriodSeconds,
      automountServiceAccountToken: false,
      enableServiceLinks: false,
      securityContext: { runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000 },
      affinity: {
        podAffinity: {
          requiredDuringSchedulingIgnoredDuringExecution: [
            {
              labelSelector: { matchLabels: { [LABEL_TREE]: labelValue(input.tree) } },
              topologyKey: "kubernetes.io/hostname",
            },
          ],
        },
      },
      volumes: [
        { name: "tree", persistentVolumeClaim: { claimName: input.pvcName } },
        {
          name: "providers",
          secret: { secretName: providersSecretName(input.project), defaultMode: 0o440 },
        },
        {
          name: "boot",
          secret: {
            secretName: input.secretName,
            defaultMode: 0o440,
            items: input.secretKeys.map((key) => ({ key, path: key })),
          },
        },
        {
          name: "provision",
          secret: {
            secretName: input.secretName,
            defaultMode: 0o440,
            items: [{ key: PROVISION_TOKEN_KEY, path: PROVISION_TOKEN_KEY }],
          },
        },
        { name: "grant", emptyDir: { medium: "Memory", sizeLimit: "1Mi" } },
      ],
      initContainers: [
        {
          name: INIT_CONTAINER,
          image: input.image,
          command: [
            LEGION_BINARY,
            "workspace-init",
            "--issue",
            input.issue,
            "--repo",
            input.repo,
            "--root",
            TREE_MOUNT,
            "--credential-helper",
            POD_CREDENTIAL_HELPER,
          ],
          env: initEnv,
          workingDir: TREE_MOUNT,
          volumeMounts: [
            { name: "tree", mountPath: TREE_MOUNT },
            { name: "provision", mountPath: PROVISION_DIR, readOnly: true },
          ],
          resources: k8sResources(input.resources),
        },
      ],
      containers: [
        {
          name: MAIN_CONTAINER,
          image: input.image,
          command: [
            LEGION_BINARY,
            "worker-shim",
            "--connect",
            input.shimEndpoint,
            "--boot-token-file",
            `${BOOT_DIR}/${BOOT_TOKEN_KEY}`,
            "--provider-env-dir",
            PROVIDERS_DIR,
            "--",
            ...input.ompArgv,
          ],
          env: [
            ...Object.entries(input.env).map(([name, value]) => ({ name, value })),
            // The shim (PID 1) sizes its SIGTERM handling from the pod's own grace: half of it
            // for the wrapped process to exit on its closed stdin before the fallback SIGTERM.
            {
              name: "LEGION_TERMINATION_GRACE_SECONDS",
              value: String(input.terminationGracePeriodSeconds),
            },
          ],
          workingDir: input.workspaceDir,
          volumeMounts: [
            { name: "tree", mountPath: TREE_MOUNT },
            { name: "tree", mountPath: OMP_SESSIONS_DIR, subPath: SESSIONS_SUBPATH },
            { name: "providers", mountPath: PROVIDERS_DIR, readOnly: true },
            { name: "boot", mountPath: BOOT_DIR, readOnly: true },
            { name: "grant", mountPath: GRANT_DIR },
          ],
          resources: k8sResources(input.resources),
        },
      ],
    },
  };
}

export function buildPvcManifest(input: {
  name: string;
  project: string;
  tree: IssueKey;
  storageClass?: string;
  size: string;
}): K8sPersistentVolumeClaim {
  return {
    apiVersion: "v1",
    kind: "PersistentVolumeClaim",
    metadata: {
      name: input.name,
      labels: { [LABEL_PROJECT]: input.project, [LABEL_TREE]: labelValue(input.tree) },
    },
    spec: {
      accessModes: ["ReadWriteOnce"],
      resources: { requests: { storage: input.size } },
      ...(input.storageClass !== undefined ? { storageClassName: input.storageClass } : {}),
    },
  };
}

export function buildSecretManifest(input: {
  name: string;
  labels: Record<string, string>;
  stringData: Record<string, string>;
}): K8sSecret {
  return {
    apiVersion: "v1",
    kind: "Secret",
    metadata: { name: input.name, labels: input.labels },
    type: "Opaque",
    stringData: input.stringData,
  };
}
