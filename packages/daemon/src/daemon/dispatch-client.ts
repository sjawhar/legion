import type { IssueDetails, IssueSummary } from "@legion/contracts";
import type { IssueStatus } from "./legion-state";

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
 * (see events.ts/processes.ts) and the issue reads resync needs to detect drift. Every write
 * carries the daemon's own actor identity (D4): `{kind:"session", id:"legion-daemon:<project>"}`
 * with `origin.session_title`, so session-authored status writes are attributed to the daemon and
 * carry `notify=false`. A non-2xx response throws `DispatchHttpError`; the client never retries —
 * resync is the retry mechanism for a failed lifecycle-status write (see processes.ts). */
export interface DispatchClient {
  /** `GET /api/v1/issues?project=<project>`. */
  listIssues(project: string): Promise<IssueSummary[]>;
  /** `GET /api/v1/issues/<key>`. */
  getIssue(key: string): Promise<IssueDetails>;
  /** `PATCH /api/v1/issues/<key>` with `{status, actor, origin}`. */
  setStatus(key: string, status: IssueStatus): Promise<void>;
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
  const actor = { kind: "session" as const, id: `legion-daemon:${options.project}` };
  const origin = { session_title: `Legion daemon · ${options.project}` };

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
        origin,
      });
    },
  };
}
