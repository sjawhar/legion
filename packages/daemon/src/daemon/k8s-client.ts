import { readFile as readFileFs } from "node:fs/promises";
import path from "node:path";
import { parse } from "yaml";

// The subset of core/v1 the daemon reads or writes; every other field is dropped.

export interface K8sObjectMeta {
  name: string;
  namespace?: string;
  uid?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  creationTimestamp?: string;
  deletionTimestamp?: string;
  /** Set on an object some controller must release before it is removed: a delete, even at
   * grace 0, only marks it (`deletionTimestamp`) until the list is empty. */
  finalizers?: string[];
}

export interface K8sContainerStateTerminated {
  exitCode: number;
  reason?: string;
  message?: string;
  startedAt?: string;
  finishedAt?: string;
}

export interface K8sContainerStatus {
  name: string;
  state?: {
    waiting?: { reason?: string; message?: string };
    running?: { startedAt?: string };
    terminated?: K8sContainerStateTerminated;
  };
}

export interface K8sPod {
  apiVersion?: "v1";
  kind?: "Pod";
  metadata: K8sObjectMeta;
  spec: Record<string, unknown>;
  status?: {
    phase?: "Pending" | "Running" | "Succeeded" | "Failed" | "Unknown";
    initContainerStatuses?: K8sContainerStatus[];
    containerStatuses?: K8sContainerStatus[];
    reason?: string;
    message?: string;
  };
}

export interface K8sPersistentVolumeClaim {
  apiVersion?: "v1";
  kind?: "PersistentVolumeClaim";
  metadata: K8sObjectMeta;
  spec: Record<string, unknown>;
  status?: { phase?: string };
}

export interface K8sSecret {
  apiVersion?: "v1";
  kind?: "Secret";
  metadata: K8sObjectMeta;
  type?: string;
  stringData?: Record<string, string>;
}

export interface K8sEvent {
  metadata: K8sObjectMeta;
  reason?: string;
  message?: string;
  type?: string;
  count?: number;
  lastTimestamp?: string;
}

/** A non-2xx answer from the API server. `status` is the HTTP status; `verb`/`resource` name the
 * call (`"delete"`, `"pods/legion-x-planner-g1"`) so a 403 reads as an RBAC gap for that verb. */
export class K8sApiError extends Error {
  constructor(
    readonly status: number,
    readonly verb: string,
    readonly resource: string,
    readonly reason: string | undefined,
    message: string
  ) {
    super(message);
    this.name = "K8sApiError";
  }
}

export interface K8sDeleteOptions {
  gracePeriodSeconds: number;
  /** `preconditions.uid`: the delete is refused (409) unless the object's uid matches. */
  uid?: string;
}

export interface K8sClient {
  readonly namespace: string;
  pods: {
    get(name: string): Promise<K8sPod>; // 404 -> K8sApiError(404)
    list(labelSelector: string): Promise<K8sPod[]>;
    create(pod: K8sPod): Promise<K8sPod>; // the response carries metadata.uid
    delete(name: string, options: K8sDeleteOptions): Promise<void>;
    log(name: string, container: string, tailLines: number): Promise<string>;
  };
  persistentVolumeClaims: {
    get(name: string): Promise<K8sPersistentVolumeClaim>;
    list(labelSelector: string): Promise<K8sPersistentVolumeClaim[]>;
    create(pvc: K8sPersistentVolumeClaim): Promise<K8sPersistentVolumeClaim>;
    delete(name: string): Promise<void>;
    /** JSON merge patch of `metadata.annotations`; a `null` value removes the annotation. */
    patchAnnotations(name: string, annotations: Record<string, string | null>): Promise<void>;
  };
  /** No `list`: a Secret list returns every Secret's data in the namespace, the deployment's
   * provider keys included, and the daemon must never hold those. The orphan sweep names each
   * per-pod Secret from its pod and deletes by name. */
  secrets: {
    create(secret: K8sSecret): Promise<void>;
    delete(name: string): Promise<void>;
  };
  events: {
    /** Core/v1 events for one object: `fieldSelector=involvedObject.name=<name>`. */
    list(involvedObjectName: string): Promise<K8sEvent[]>;
  };
}

export interface K8sCredentials {
  server: string; // https://host:port, no trailing slash
  namespace?: string; // in-cluster: the service account's namespace file
  token?: string; // bearer
  tls?: { ca?: string; cert?: string; key?: string; rejectUnauthorized?: boolean };
}

export interface K8sClientOptions extends K8sCredentials {
  namespace: string;
  fetch?: typeof fetch;
}

export const IN_CLUSTER_SERVICE_ACCOUNT_DIR = "/var/run/secrets/kubernetes.io/serviceaccount";

interface K8sListResponse<T> {
  items: T[];
}

interface K8sStatusBody {
  reason?: string;
  message?: string;
}

async function readErrorBody(
  response: Response
): Promise<{ reason: string | undefined; bodyMessage: string | undefined }> {
  const text = await response.text().catch(() => "");
  if (!text) return { reason: undefined, bodyMessage: undefined };
  try {
    const parsed = JSON.parse(text) as unknown;
    if (typeof parsed === "object" && parsed !== null) {
      const body = parsed as K8sStatusBody;
      return { reason: body.reason, bodyMessage: body.message };
    }
    return { reason: undefined, bodyMessage: text };
  } catch {
    return { reason: undefined, bodyMessage: text };
  }
}

export function createK8sClient(options: K8sClientOptions): K8sClient {
  const { namespace, server, token, tls } = options;
  const fetchImpl = options.fetch ?? fetch;
  const base = `${server}/api/v1/namespaces/${namespace}`;

  async function send(
    method: string,
    urlPath: string,
    verb: string,
    resource: string,
    init?: { body?: unknown; contentType?: string; accept?: string }
  ): Promise<Response> {
    const headers: Record<string, string> = { Accept: init?.accept ?? "application/json" };
    if (init?.contentType) headers["Content-Type"] = init.contentType;
    if (token) headers.Authorization = `Bearer ${token}`;
    const requestInit: Record<string, unknown> = {
      method,
      headers,
      body: init?.body === undefined ? undefined : JSON.stringify(init.body),
    };
    if (tls) requestInit.tls = tls;
    const response = await fetchImpl(`${base}${urlPath}`, requestInit as RequestInit);
    if (response.status < 200 || response.status >= 300) {
      const { reason, bodyMessage } = await readErrorBody(response);
      const message = `${verb} ${resource} failed with ${response.status}${
        reason ? ` (${reason})` : ""
      }${bodyMessage ? `: ${bodyMessage}` : ""}`;
      throw new K8sApiError(response.status, verb, resource, reason, message);
    }
    return response;
  }

  async function sendJson<T>(
    method: string,
    urlPath: string,
    verb: string,
    resource: string,
    init?: { body?: unknown; contentType?: string }
  ): Promise<T> {
    const response = await send(method, urlPath, verb, resource, init);
    return (await response.json()) as T;
  }

  async function listOf<T>(resource: string, labelSelector: string): Promise<T[]> {
    const body = await sendJson<K8sListResponse<T>>(
      "GET",
      `/${resource}?labelSelector=${encodeURIComponent(labelSelector)}`,
      "list",
      resource
    );
    return body.items;
  }

  return {
    namespace,
    pods: {
      get(name) {
        return sendJson<K8sPod>("GET", `/pods/${encodeURIComponent(name)}`, "get", `pods/${name}`);
      },
      list(labelSelector) {
        return listOf<K8sPod>("pods", labelSelector);
      },
      create(pod) {
        return sendJson<K8sPod>("POST", "/pods", "create", `pods/${pod.metadata.name}`, {
          body: pod,
        });
      },
      async delete(name, deleteOptions) {
        const body: Record<string, unknown> = {
          apiVersion: "v1",
          kind: "DeleteOptions",
          gracePeriodSeconds: deleteOptions.gracePeriodSeconds,
        };
        if (deleteOptions.uid !== undefined) body.preconditions = { uid: deleteOptions.uid };
        await send("DELETE", `/pods/${encodeURIComponent(name)}`, "delete", `pods/${name}`, {
          body,
        });
      },
      async log(name, container, tailLines) {
        // The log subresource is negotiated against the apiserver's serializers (json, yaml,
        // protobuf) before its text/plain streamer runs, so a bare `text/plain` Accept is 406
        // (`only the following media types are accepted: …`, kind v1.31.0). kubectl sends
        // `application/json, */*`; the wildcard is what admits the request, and the body still
        // arrives as text/plain.
        const response = await send(
          "GET",
          `/pods/${encodeURIComponent(name)}/log?container=${encodeURIComponent(
            container
          )}&tailLines=${tailLines}`,
          "log",
          `pods/${name}/log`,
          { accept: "*/*" }
        );
        return await response.text();
      },
    },
    persistentVolumeClaims: {
      get(name) {
        return sendJson<K8sPersistentVolumeClaim>(
          "GET",
          `/persistentvolumeclaims/${encodeURIComponent(name)}`,
          "get",
          `persistentvolumeclaims/${name}`
        );
      },
      list(labelSelector) {
        return listOf<K8sPersistentVolumeClaim>("persistentvolumeclaims", labelSelector);
      },
      create(pvc) {
        return sendJson<K8sPersistentVolumeClaim>(
          "POST",
          "/persistentvolumeclaims",
          "create",
          `persistentvolumeclaims/${pvc.metadata.name}`,
          { body: pvc }
        );
      },
      async delete(name) {
        await send(
          "DELETE",
          `/persistentvolumeclaims/${encodeURIComponent(name)}`,
          "delete",
          `persistentvolumeclaims/${name}`
        );
      },
      async patchAnnotations(name, annotations) {
        await send(
          "PATCH",
          `/persistentvolumeclaims/${encodeURIComponent(name)}`,
          "patch",
          `persistentvolumeclaims/${name}`,
          { body: { metadata: { annotations } }, contentType: "application/merge-patch+json" }
        );
      },
    },
    secrets: {
      async create(secret) {
        await send("POST", "/secrets", "create", `secrets/${secret.metadata.name}`, {
          body: secret,
        });
      },
      async delete(name) {
        await send("DELETE", `/secrets/${encodeURIComponent(name)}`, "delete", `secrets/${name}`);
      },
    },
    events: {
      list(involvedObjectName) {
        return sendJson<K8sListResponse<K8sEvent>>(
          "GET",
          `/events?fieldSelector=${encodeURIComponent(`involvedObject.name=${involvedObjectName}`)}`,
          "list",
          "events"
        ).then((body) => body.items);
      },
    },
  };
}

/** Reads a PEM-ish credential field from a kubeconfig `cluster`/`user` block: the `-data` key is
 * base64, the plain key is a path resolved relative to the kubeconfig's own directory. */
async function resolveKubeconfigPemField(
  source: Record<string, unknown>,
  dataKey: string,
  fileKey: string,
  kubeconfigDir: string,
  readFile: (filePath: string) => Promise<string>
): Promise<string | undefined> {
  const dataValue = source[dataKey];
  if (typeof dataValue === "string") return Buffer.from(dataValue, "base64").toString("utf8");
  const fileValue = source[fileKey];
  if (typeof fileValue === "string") {
    return (await readFile(path.resolve(kubeconfigDir, fileValue))).trim();
  }
  return undefined;
}

interface KubeconfigDoc {
  "current-context"?: string;
  contexts?: Array<{ name: string; context: { cluster: string; user: string } }>;
  clusters?: Array<{ name: string; cluster: Record<string, unknown> }>;
  users?: Array<{ name: string; user: Record<string, unknown> }>;
}

async function resolveFromKubeconfig(
  kubeconfigPath: string,
  readFile: (filePath: string) => Promise<string>
): Promise<K8sCredentials> {
  const doc = parse(await readFile(kubeconfigPath)) as KubeconfigDoc;
  const currentContext = doc["current-context"];
  const contextEntry = currentContext
    ? doc.contexts?.find((entry) => entry.name === currentContext)
    : undefined;
  if (!currentContext || !contextEntry) {
    throw new Error(`${kubeconfigPath} has no usable current-context`);
  }
  const clusterEntry = doc.clusters?.find((entry) => entry.name === contextEntry.context.cluster);
  const userEntry = doc.users?.find((entry) => entry.name === contextEntry.context.user);
  const cluster = clusterEntry?.cluster ?? {};
  const user = userEntry?.user ?? {};
  const dir = path.dirname(kubeconfigPath);

  const ca = await resolveKubeconfigPemField(
    cluster,
    "certificate-authority-data",
    "certificate-authority",
    dir,
    readFile
  );
  const cert = await resolveKubeconfigPemField(
    user,
    "client-certificate-data",
    "client-certificate",
    dir,
    readFile
  );
  const key = await resolveKubeconfigPemField(user, "client-key-data", "client-key", dir, readFile);
  const rejectUnauthorized = cluster["insecure-skip-tls-verify"] === true ? false : undefined;

  const tls: K8sCredentials["tls"] =
    ca !== undefined || cert !== undefined || key !== undefined || rejectUnauthorized !== undefined
      ? {
          ...(ca !== undefined ? { ca } : {}),
          ...(cert !== undefined ? { cert } : {}),
          ...(key !== undefined ? { key } : {}),
          ...(rejectUnauthorized !== undefined ? { rejectUnauthorized } : {}),
        }
      : undefined;

  const token = typeof user.token === "string" ? user.token : undefined;

  return {
    server: cluster.server as string,
    ...(token !== undefined ? { token } : {}),
    ...(tls !== undefined ? { tls } : {}),
  };
}

/** kubeconfig when given (its `current-context`); else the in-cluster files; else throws naming both
 * (`runtime.kubernetes.kubeconfig is not set and ${dir}/token does not exist: the daemon runs neither
 * in a pod nor with a kubeconfig`). */
export async function resolveK8sCredentials(input: {
  kubeconfig?: string;
  serviceAccountDir?: string;
  env?: Record<string, string | undefined>;
  readFile?: (filePath: string) => Promise<string>;
}): Promise<K8sCredentials> {
  const readFile = input.readFile ?? ((filePath: string) => readFileFs(filePath, "utf8"));
  if (input.kubeconfig !== undefined) {
    return await resolveFromKubeconfig(input.kubeconfig, readFile);
  }
  const dir = input.serviceAccountDir ?? IN_CLUSTER_SERVICE_ACCOUNT_DIR;
  const env = input.env ?? process.env;
  const tokenPath = `${dir}/token`;
  let token: string;
  try {
    token = (await readFile(tokenPath)).trim();
  } catch {
    throw new Error(
      `runtime.kubernetes.kubeconfig is not set and ${tokenPath} does not exist: the daemon runs neither in a pod nor with a kubeconfig`
    );
  }
  const ca = (await readFile(`${dir}/ca.crt`)).trim();
  const namespace = (await readFile(`${dir}/namespace`)).trim();
  return {
    server: `https://${env.KUBERNETES_SERVICE_HOST}:${env.KUBERNETES_SERVICE_PORT}`,
    namespace,
    token,
    tls: { ca },
  };
}
