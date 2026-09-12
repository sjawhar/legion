import type { IssueDetails, IssueKey, IssueSummary } from "@legion/contracts";
import type {
  IssueStatus,
  LegionState,
  PendingStatusWrite,
  SpecArtifactResolver,
} from "./legion-state";

/** A non-2xx response from the Dispatch HTTP API: `status` is the HTTP status code, `message` is
 * the server's `error` field (or its raw body when the response is not the expected JSON shape). */
export class DispatchHttpError extends Error {
  override readonly name = "DispatchHttpError";

  constructor(
    readonly status: number,
    message: string
  ) {
    super(message);
  }
}

/** The daemon's thin client for Dispatch's native-tool API: the lifecycle statuses it owns
 * (see events.ts/processes.ts), the issue reads resync needs to detect drift, and the close of a
 * design-gate ask the daemon satisfied itself. Every write
 * carries the daemon's own actor identity (D4): `{kind:"session", id:"legion-daemon:<project>",
 * origin:{session_title}}` — `origin` nests inside the session actor per the contract
 * (`packages/contracts/src/dispatch-api.ts`'s `Actor` union), never a sibling of `actor`: the
 * PATCH decoder rejects unknown top-level fields
 * (`packages/envoy/internal/dispatch/api/issues.go`'s `patchIssue`,
 * `server.go`'s `decodeJSON`'s `DisallowUnknownFields`), so a top-level `origin` would 400 every
 * write. Session-authored writes carry `notify=false`. A non-2xx response throws
 * `DispatchHttpError`; the client never retries — resync is the retry mechanism for a failed
 * lifecycle-status write (see processes.ts). */
export interface DispatchClient {
  /** `GET /api/v1/issues?project=<project>`. */
  listIssues(project: string): Promise<IssueSummary[]>;
  /** `GET /api/v1/issues/<key>`. */
  getIssue(key: string): Promise<IssueDetails>;
  /** `PATCH /api/v1/issues/<key>` with `{status, actor: {kind, id, origin}}`. */
  setStatus(key: string, status: IssueStatus): Promise<void>;
  /** `POST /api/v1/asks/<id>/resolve` with `{kind:"resolved", reason, actor}`: closes an open ask
   * without answering it. Any session actor may resolve any open ask (`resolveAsk` in
   * `packages/envoy/internal/dispatch/api/asks.go` requires an actor, not the author or a human);
   * an already-answered or already-resolved ask is a 409. */
  resolveAsk(id: string, reason: string): Promise<void>;
}

export interface DispatchClientOptions {
  /** The Dispatch service base URL (no `/mcp` suffix, no trailing slash required). */
  baseUrl: string;
  token: string;
  /** The Legion Dispatch project key; embedded in the daemon's actor id and session title. */
  project: string;
  fetch?: typeof fetch;
}

function errorMessage(payload: unknown, response: Response): string {
  if (typeof payload === "object" && payload !== null && "error" in payload) {
    const error = payload.error;
    if (typeof error === "string" && error.length > 0) return error;
  }
  if (typeof payload === "string" && payload.length > 0) return payload;
  return response.statusText;
}

export function createDispatchClient(options: DispatchClientOptions): DispatchClient {
  const baseUrl = options.baseUrl.replace(/\/+$/, "");
  const fetchImpl = options.fetch ?? fetch;
  const actor = {
    kind: "session" as const,
    id: `legion-daemon:${options.project}`,
    origin: { session_title: `Legion daemon · ${options.project}` },
  };

  async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = {
      Accept: "application/json",
      Authorization: `Bearer ${options.token}`,
    };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const response = await fetchImpl(`${baseUrl}${path}`, {
      method,
      headers,
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    const text = await response.text();
    let payload: unknown = text;
    if (text.length > 0) {
      try {
        payload = JSON.parse(text);
      } catch {
        payload = text;
      }
    }
    if (!response.ok) {
      throw new DispatchHttpError(response.status, errorMessage(payload, response));
    }
    return payload as T;
  }

  return {
    listIssues: (project) =>
      request("GET", `/api/v1/issues?project=${encodeURIComponent(project)}`),
    getIssue: (key) => request("GET", `/api/v1/issues/${encodeURIComponent(key)}`),
    async setStatus(key, status) {
      await request("PATCH", `/api/v1/issues/${encodeURIComponent(key)}`, {
        status,
        actor,
      });
    },
    async resolveAsk(id, reason) {
      await request("POST", `/api/v1/asks/${encodeURIComponent(id)}/resolve`, {
        kind: "resolved",
        reason,
        actor,
      });
    },
  };
}

/** The one Dispatch read outside resync: `migrateV23State`'s resolver for a v23 design gate. Reads
 * the issue once and returns its primary artifact's id (the spec document) with the highest
 * version number that artifact carries. Throws naming the issue when the primary artifact is
 * missing from the issue's `artifacts` or has no versions — the caller refuses to start on it. */
export function specArtifactResolver(client: DispatchClient): SpecArtifactResolver {
  return async (issue) => {
    const details = await client.getIssue(issue);
    const spec = details.artifacts.find((artifact) => artifact.id === details.primary_artifact_id);
    if (!spec) {
      throw new Error(`${issue} has no primary artifact ${details.primary_artifact_id}`);
    }
    const latestVersion = Math.max(...spec.versions.map((version) => version.number));
    if (!Number.isSafeInteger(latestVersion) || latestVersion <= 0) {
      throw new Error(`${issue}'s spec document ${spec.id} has no versions`);
    }
    return { artifactId: spec.id, latestVersion };
  };
}

const issueWriteLanes = new Map<IssueKey, Promise<void>>();

/** Serializes `fn` against every other daemon-owned Dispatch write for the same issue: enqueued
 * after every earlier write for this issue settles, and before any later one starts. Both
 * `writeStatus` and `retryPendingWrite` go through this so a direct status write (an API route,
 * a phase-complete write, a spawn/close write) can never interleave with a concurrent resync
 * retry for the same issue. */
function serializeIssueWrite<T>(issue: IssueKey, fn: () => Promise<T>): Promise<T> {
  const previous = issueWriteLanes.get(issue) ?? Promise.resolve();
  const result = previous.then(fn, fn);
  issueWriteLanes.set(
    issue,
    result.then(
      () => undefined,
      () => undefined
    )
  );
  return result;
}

async function writeStatusLocked(
  state: LegionState,
  client: DispatchClient,
  issue: IssueKey,
  status: IssueStatus
): Promise<boolean> {
  const previous = state.pendingStatusWrites[issue];
  try {
    await client.setStatus(issue, status);
    delete state.pendingStatusWrites[issue];
    return previous !== undefined;
  } catch (error) {
    const pending: PendingStatusWrite = { status, statusAtRecord: state.issues[issue]?.status };
    state.pendingStatusWrites[issue] = pending;
    console.error(
      `[legion] failed to PATCH Dispatch status=${status} for ${issue} (recorded for resync retry): ${error instanceof Error ? error.message : String(error)}`
    );
    return (
      previous?.status !== pending.status || previous?.statusAtRecord !== pending.statusAtRecord
    );
  }
}

/** Writes a daemon-owned lifecycle status to Dispatch, serialized against every other write for
 * the same issue (see `serializeIssueWrite`). A failed write records both the requested status
 * and the issue's own local status at the moment of failure (`statusAtRecord`) so resync can
 * discard the intent once that fence no longer holds. Returns whether this call changed
 * pending-write state. */
export function writeStatus(
  state: LegionState,
  client: DispatchClient,
  issue: IssueKey,
  status: IssueStatus
): Promise<boolean> {
  return serializeIssueWrite(issue, () => writeStatusLocked(state, client, issue, status));
}

/** Resolves one pending status write against a single remote read (`remote`), then performs the
 * delete-or-PATCH under `issue`'s write lane, re-validating the pending entry is still exactly
 * `expected` (a concurrent direct `writeStatus` may already have replaced or cleared it while the
 * remote read was in flight) — a decision computed before that read can never clobber a fresher
 * intent. `remote.status === expected.status` means the intent landed; `remote.status !==
 * expected.statusAtRecord` means a real status change superseded it; otherwise the retry PATCHes
 * again. Returns whether state changed (for the caller's own save). */
export async function retryPendingWrite(
  state: LegionState,
  client: DispatchClient,
  issue: IssueKey,
  remote: Pick<IssueDetails, "status">,
  expected: PendingStatusWrite
): Promise<boolean> {
  return serializeIssueWrite(issue, async () => {
    const current = state.pendingStatusWrites[issue];
    if (
      !current ||
      current.status !== expected.status ||
      current.statusAtRecord !== expected.statusAtRecord
    ) {
      return false;
    }
    if (remote.status === current.status || remote.status !== current.statusAtRecord) {
      delete state.pendingStatusWrites[issue];
      return true;
    }
    return writeStatusLocked(state, client, issue, current.status);
  });
}
