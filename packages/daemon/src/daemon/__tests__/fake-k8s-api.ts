import { randomUUID } from "node:crypto";
import type {
  K8sEvent,
  K8sObjectMeta,
  K8sPersistentVolumeClaim,
  K8sPod,
  K8sSecret,
} from "../k8s-client";

export interface FakeK8sApiOptions {
  namespace: string;
  now(): number;
  /** Verbs to answer 403, e.g. ["delete pods"]. */
  forbidden?: string[];
}

/** `K8sEvent` as the client sees it, plus the `involvedObject` reference the real API carries and
 * this client's lean type drops. Tests that need `events.list` filtering to see a given event push
 * one of these onto `FakeK8sApi.events`; a plain `K8sEvent` (no `involvedObject`) never matches any
 * filtered query. */
export interface FakeK8sEvent extends K8sEvent {
  involvedObject?: { name?: string };
}

export interface FakeK8sApi {
  readonly fetch: typeof fetch;
  readonly pods: Map<string, K8sPod>;
  readonly pvcs: Map<string, K8sPersistentVolumeClaim>;
  readonly secrets: Map<string, K8sSecret>;
  readonly events: K8sEvent[];
  readonly logs: Map<string, string>; // `${pod}/${container}` -> text
  readonly requests: Array<{ method: string; path: string; body?: unknown }>;
  /** Moves a pod to `Running`/`Succeeded`/`Failed` (with optional container statuses). */
  setPhase(
    name: string,
    phase: NonNullable<K8sPod["status"]>["phase"],
    status?: Partial<NonNullable<K8sPod["status"]>>
  ): void;
  /** Reissues the pod's uid: the name now belongs to some other process. */
  reissueUid(name: string): void;
  /** Completes every graceful deletion in flight (objects with a deletionTimestamp vanish). */
  finalizeDeletions(): void;
  /** Fails the next request matching `method path` once with `status`. */
  failNext(method: string, pathPattern: RegExp, status: number): void;
}

interface PendingFailure {
  method: string;
  pathPattern: RegExp;
  status: number;
}

function statusBody(status: number, reason: string, message: string) {
  return { kind: "Status", apiVersion: "v1", status: "Failure", reason, message, code: status };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function matchesLabelSelector(
  labels: Record<string, string> | undefined,
  selector: string | null
): boolean {
  if (!selector) return true;
  const pairs = selector
    .split(",")
    .map((pair) => pair.trim())
    .filter((pair) => pair.length > 0);
  return pairs.every((pair) => {
    const [key, value] = pair.split("=");
    return key !== undefined && labels?.[key] === value;
  });
}

const POD_ITEM = /^\/pods\/([^/]+)$/;
const POD_LOG = /^\/pods\/([^/]+)\/log$/;
const PVC_ITEM = /^\/persistentvolumeclaims\/([^/]+)$/;
const SECRET_ITEM = /^\/secrets\/([^/]+)$/;
const INVOLVED_OBJECT_NAME = /^involvedObject\.name=(.*)$/;

/** The apiserver's registered serializer media types; a log request's Accept must name one of
 * them or the full wildcard (a `text/` wildcard does not count) to pass negotiation. */
const LOG_NEGOTIABLE_TYPES = [
  "application/json",
  "application/yaml",
  "application/vnd.kubernetes.protobuf",
];

function acceptsLogStream(accept: string | null): boolean {
  if (accept === null || accept.trim() === "") return true;
  return accept.split(",").some((entry) => {
    const type = entry.split(";")[0]?.trim() ?? "";
    return type === "*/*" || LOG_NEGOTIABLE_TYPES.includes(type);
  });
}

export function createFakeK8sApi(options: FakeK8sApiOptions): FakeK8sApi {
  const { namespace, now, forbidden = [] } = options;
  const pods = new Map<string, K8sPod>();
  const pvcs = new Map<string, K8sPersistentVolumeClaim>();
  const secrets = new Map<string, K8sSecret>();
  const events: FakeK8sEvent[] = [];
  const logs = new Map<string, string>();
  const requests: Array<{ method: string; path: string; body?: unknown }> = [];
  const pendingFailures: PendingFailure[] = [];
  const prefix = `/api/v1/namespaces/${namespace}`;

  function errorResponse(status: number, reason: string, message: string): Response {
    return jsonResponse(statusBody(status, reason, message), status);
  }

  function checkForbidden(verb: string, kind: string): Response | undefined {
    if (!forbidden.includes(`${verb} ${kind}`)) return undefined;
    return errorResponse(403, "Forbidden", `${verb} ${kind} is forbidden`);
  }

  function takeFailure(method: string, fullPath: string): number | undefined {
    const index = pendingFailures.findIndex(
      (failure) => failure.method === method && failure.pathPattern.test(fullPath)
    );
    if (index === -1) return undefined;
    const [failure] = pendingFailures.splice(index, 1);
    return failure?.status;
  }

  function listResponse<T extends { metadata: K8sObjectMeta }>(
    map: Map<string, T>,
    labelSelector: string | null
  ): Response {
    const items = [...map.values()].filter((item) =>
      matchesLabelSelector(item.metadata.labels, labelSelector)
    );
    return jsonResponse({ items });
  }

  function handleCreate<T extends { metadata: K8sObjectMeta }>(
    map: Map<string, T>,
    kind: string,
    obj: T,
    isPod: boolean
  ): Response {
    const name = obj.metadata.name;
    if (map.has(name)) {
      return errorResponse(409, "AlreadyExists", `${kind} "${name}" already exists`);
    }
    const created: T = {
      ...obj,
      metadata: {
        ...obj.metadata,
        uid: randomUUID(),
        creationTimestamp: new Date(now()).toISOString(),
      },
    };
    if (isPod) (created as unknown as K8sPod).status = { phase: "Pending" };
    map.set(name, created);
    return jsonResponse(created, 201);
  }

  function handleDelete<T extends { metadata: K8sObjectMeta }>(
    map: Map<string, T>,
    kind: string,
    name: string,
    body: unknown
  ): Response {
    const obj = map.get(name);
    if (!obj) return errorResponse(404, "NotFound", `${kind} "${name}" not found`);
    const deleteOptions = (body ?? {}) as {
      gracePeriodSeconds?: number;
      preconditions?: { uid?: string };
    };
    const uid = deleteOptions.preconditions?.uid;
    if (uid !== undefined && uid !== obj.metadata.uid) {
      return errorResponse(409, "Conflict", `${kind} "${name}" uid mismatch`);
    }
    const gracePeriodSeconds = deleteOptions.gracePeriodSeconds ?? 0;
    // A finalizer holds the object past any grace, as on a real cluster; `finalizeDeletions()`
    // stands in for the controller that releases it.
    if (gracePeriodSeconds > 0 || (obj.metadata.finalizers?.length ?? 0) > 0) {
      obj.metadata.deletionTimestamp = new Date(now()).toISOString();
      return jsonResponse(obj);
    }
    map.delete(name);
    return jsonResponse(obj);
  }

  async function route(
    method: string,
    url: URL,
    bodyText: string | undefined,
    accept: string | null
  ): Promise<Response> {
    const fullPath = url.pathname.startsWith(prefix)
      ? url.pathname.slice(prefix.length)
      : url.pathname;
    const fullPathWithSearch = fullPath + url.search;
    const body = bodyText ? (JSON.parse(bodyText) as unknown) : undefined;
    requests.push({ method, path: fullPathWithSearch, body });

    const failStatus = takeFailure(method, fullPathWithSearch);
    if (failStatus !== undefined) {
      return errorResponse(failStatus, "Injected", `injected failure for ${method} ${fullPath}`);
    }

    if (fullPath === "/pods" && method === "GET") {
      return (
        checkForbidden("list", "pods") ?? listResponse(pods, url.searchParams.get("labelSelector"))
      );
    }
    if (fullPath === "/pods" && method === "POST") {
      return checkForbidden("create", "pods") ?? handleCreate(pods, "pods", body as K8sPod, true);
    }
    const logMatch = POD_LOG.exec(fullPath);
    if (logMatch && method === "GET") {
      const name = decodeURIComponent(logMatch[1] as string);
      const forbiddenResponse = checkForbidden("log", "pods");
      if (forbiddenResponse) return forbiddenResponse;
      // The real apiserver negotiates the log subresource against its registered serializers
      // before its text/plain streamer ever runs (kind v1.31.0, 2026-09-13): an Accept without
      // one of those types or a wildcard is 406, with exactly this Status.
      if (!acceptsLogStream(accept)) {
        return errorResponse(
          406,
          "NotAcceptable",
          "only the following media types are accepted: application/json, application/yaml, application/vnd.kubernetes.protobuf"
        );
      }
      const container = url.searchParams.get("container") ?? "";
      const text = logs.get(`${name}/${container}`);
      if (text === undefined)
        return errorResponse(404, "NotFound", `no log for ${name}/${container}`);
      return new Response(text, { status: 200, headers: { "Content-Type": "text/plain" } });
    }
    const podMatch = POD_ITEM.exec(fullPath);
    if (podMatch) {
      const name = decodeURIComponent(podMatch[1] as string);
      if (method === "GET") {
        const forbiddenResponse = checkForbidden("get", "pods");
        if (forbiddenResponse) return forbiddenResponse;
        const pod = pods.get(name);
        return pod ? jsonResponse(pod) : errorResponse(404, "NotFound", `pods "${name}" not found`);
      }
      if (method === "DELETE") {
        return checkForbidden("delete", "pods") ?? handleDelete(pods, "pods", name, body);
      }
    }

    if (fullPath === "/persistentvolumeclaims" && method === "GET") {
      return (
        checkForbidden("list", "persistentvolumeclaims") ??
        listResponse(pvcs, url.searchParams.get("labelSelector"))
      );
    }
    if (fullPath === "/persistentvolumeclaims" && method === "POST") {
      return (
        checkForbidden("create", "persistentvolumeclaims") ??
        handleCreate(pvcs, "persistentvolumeclaims", body as K8sPersistentVolumeClaim, false)
      );
    }
    const pvcMatch = PVC_ITEM.exec(fullPath);
    if (pvcMatch) {
      const name = decodeURIComponent(pvcMatch[1] as string);
      if (method === "GET") {
        const forbiddenResponse = checkForbidden("get", "persistentvolumeclaims");
        if (forbiddenResponse) return forbiddenResponse;
        const pvc = pvcs.get(name);
        return pvc
          ? jsonResponse(pvc)
          : errorResponse(404, "NotFound", `persistentvolumeclaims "${name}" not found`);
      }
      if (method === "DELETE") {
        return (
          checkForbidden("delete", "persistentvolumeclaims") ??
          handleDelete(pvcs, "persistentvolumeclaims", name, body)
        );
      }
      if (method === "PATCH") {
        const forbiddenResponse = checkForbidden("patch", "persistentvolumeclaims");
        if (forbiddenResponse) return forbiddenResponse;
        const pvc = pvcs.get(name);
        if (!pvc)
          return errorResponse(404, "NotFound", `persistentvolumeclaims "${name}" not found`);
        const patch =
          (body as { metadata?: { annotations?: Record<string, string | null> } } | undefined)
            ?.metadata?.annotations ?? {};
        const annotations = { ...(pvc.metadata.annotations ?? {}) };
        for (const [key, value] of Object.entries(patch)) {
          if (value === null) delete annotations[key];
          else annotations[key] = value;
        }
        pvc.metadata.annotations = annotations;
        return jsonResponse(pvc);
      }
    }

    if (fullPath === "/secrets" && method === "GET") {
      return (
        checkForbidden("list", "secrets") ??
        listResponse(secrets, url.searchParams.get("labelSelector"))
      );
    }
    if (fullPath === "/secrets" && method === "POST") {
      return (
        checkForbidden("create", "secrets") ??
        handleCreate(secrets, "secrets", body as K8sSecret, false)
      );
    }
    const secretMatch = SECRET_ITEM.exec(fullPath);
    if (secretMatch && method === "DELETE") {
      const name = decodeURIComponent(secretMatch[1] as string);
      return checkForbidden("delete", "secrets") ?? handleDelete(secrets, "secrets", name, body);
    }

    if (fullPath === "/events" && method === "GET") {
      const forbiddenResponse = checkForbidden("list", "events");
      if (forbiddenResponse) return forbiddenResponse;
      const selector = url.searchParams.get("fieldSelector");
      const wantedName = selector ? INVOLVED_OBJECT_NAME.exec(selector)?.[1] : undefined;
      const items = events.filter(
        (event) => wantedName === undefined || event.involvedObject?.name === wantedName
      );
      return jsonResponse({ items });
    }

    return errorResponse(404, "NotFound", `no route for ${method} ${fullPath}`);
  }

  const fakeFetch = (async (
    input: string | URL | Request,
    init?: RequestInit & { tls?: unknown }
  ): Promise<Response> => {
    const url = new URL(typeof input === "string" || input instanceof URL ? input : input.url);
    const method = init?.method ?? "GET";
    const bodyText = typeof init?.body === "string" ? init.body : undefined;
    return await route(method, url, bodyText, new Headers(init?.headers).get("accept"));
  }) as typeof fetch;

  return {
    fetch: fakeFetch,
    pods,
    pvcs,
    secrets,
    events,
    logs,
    requests,
    setPhase(name, phase, status) {
      const pod = pods.get(name);
      if (!pod) throw new Error(`fake k8s api: no pod named ${name}`);
      pod.status = { ...pod.status, ...status, phase };
    },
    reissueUid(name) {
      const pod = pods.get(name);
      if (!pod) throw new Error(`fake k8s api: no pod named ${name}`);
      pod.metadata = { ...pod.metadata, uid: randomUUID() };
    },
    finalizeDeletions() {
      for (const map of [pods, pvcs, secrets]) {
        for (const [name, obj] of map) {
          if (obj.metadata.deletionTimestamp !== undefined) map.delete(name);
        }
      }
    },
    failNext(method, pathPattern, status) {
      pendingFailures.push({ method, pathPattern, status });
    },
  };
}
