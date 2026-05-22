// Core forwarding logic for the local MCP shim that proxies opencode's
// stdio MCP traffic to the remote dispatch server's Streamable HTTP /mcp,
// minting a fresh GitHub bearer per request via the user's `gh` shim.
//
// Exposed as a library so the bridge can be unit-tested without spawning
// a real subprocess. The CLI wrapper in bin/dispatch-mcp-shim.ts wires
// this to stdin/stdout.

import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

/** Refresh well before the 1h gh-app installation token expiry. */
const DEFAULT_TOKEN_CACHE_TTL_MS = 50 * 60 * 1000;

export interface JsonRpcRequest {
  jsonrpc: "2.0";
  id?: string | number;
  method: string;
  params?: unknown;
}

export interface JsonRpcResponse {
  jsonrpc: "2.0";
  id: string | number | null;
  result?: unknown;
  error?: { code: number; message: string; data?: unknown };
}

export type TokenGetter = () => Promise<string | null>;
export type FetchImpl = typeof fetch;

export interface BridgeOptions {
  remoteUrl: string;
  getToken: TokenGetter;
  fetchImpl?: FetchImpl;
  tokenCacheTtlMs?: number;
  /** Optional logger for stderr-side diagnostics. */
  logError?: (msg: string) => void;
  /** Optional clock injection for tests. */
  now?: () => number;
}

export interface Bridge {
  handle(request: JsonRpcRequest): Promise<JsonRpcResponse | null>;
}

export const defaultGhTokenGetter: TokenGetter = async () => {
  try {
    const { stdout } = await execFileAsync("gh", ["auth", "token"], {
      timeout: 5_000,
    });
    const value = stdout.trim();
    return value.length > 0 ? value : null;
  } catch {
    return null;
  }
};

/**
 * Parse a Streamable-HTTP SSE response and return the first `event: message`
 * payload as parsed JSON. Returns null if no message line was found.
 */
function parseSseBody(body: string): unknown {
  for (const line of body.split("\n")) {
    const match = line.match(/^data:\s*(.+)$/);
    if (match) {
      return JSON.parse(match[1] as string);
    }
  }
  return null;
}

/**
 * Detect whether a parsed JSON-RPC response indicates that the *upstream*
 * GitHub call failed with 401 (e.g. expired App-installation token).
 *
 * The Go MCP server forwards GitHub errors verbatim in the tool result
 * (`result.isError: true`, content text contains "401 Bad credentials")
 * or as a JSON-RPC error message. Either signal triggers a one-shot retry
 * with a freshly-minted token.
 */
function hasUpstreamUnauthorized(parsed: unknown): boolean {
  if (!parsed || typeof parsed !== "object") return false;
  const obj = parsed as { error?: { message?: unknown }; result?: unknown };
  if (
    obj.error &&
    typeof obj.error.message === "string" &&
    containsUnauthorized(obj.error.message)
  ) {
    return true;
  }
  const result = obj.result as { isError?: unknown; content?: unknown } | undefined;
  if (!result || result.isError !== true || !Array.isArray(result.content)) return false;
  for (const item of result.content) {
    if (
      item &&
      typeof item === "object" &&
      "text" in item &&
      typeof (item as { text: unknown }).text === "string"
    ) {
      if (containsUnauthorized((item as { text: string }).text)) return true;
    }
  }
  return false;
}

function containsUnauthorized(msg: string): boolean {
  return /\b401\b/.test(msg) && /bad credentials|unauthorized/i.test(msg);
}

/**
 * Collapse JSON-Schema union `type` arrays (e.g. { type: ["null", "array"] }) into a
 * single-type schema. Google Gemini's function-declaration validator rejects union-type
 * arrays: the array branch loses its `items` and the top-level `items` is left orphaned,
 * producing `any_of[0].items: missing field`. Remote dispatch tools express nullable fields
 * this way, so we normalize them in transit. Dropping "null" is safe — the model omits or
 * passes a real value, and `required` already governs presence.
 */
function normalizeSchemaUnionTypes(node: unknown): unknown {
  if (Array.isArray(node)) return node.map(normalizeSchemaUnionTypes);
  if (!node || typeof node !== "object") return node;
  const result: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(node)) {
    result[key] = normalizeSchemaUnionTypes(value);
  }
  if (Array.isArray(result.type)) {
    const nonNull = result.type.filter((t) => t !== "null");
    if (nonNull.length === 1) {
      result.type = nonNull[0];
    } else if (nonNull.length === 0) {
      result.type = "null";
    } else {
      // Multiple non-null types: express as anyOf, carrying items into the array branch
      // so no branch is left itemless.
      const items = result.items;
      delete result.items;
      result.anyOf = nonNull.map((t) =>
        t === "array" && items != null ? { type: t, items } : { type: t }
      );
      delete result.type;
    }
  }
  return result;
}

/**
 * Normalize tool input schemas in a `tools/list` response so downstream providers
 * (notably Gemini) accept them. No-op for any other response shape.
 */
function normalizeToolsListResponse(response: JsonRpcResponse | null): JsonRpcResponse | null {
  if (!response || typeof response.result !== "object" || response.result === null) return response;
  const result = response.result as { tools?: unknown };
  if (!Array.isArray(result.tools)) return response;
  const tools = result.tools.map((entry) => {
    if (!entry || typeof entry !== "object") return entry;
    const tool = entry as Record<string, unknown>;
    if (tool.inputSchema == null || typeof tool.inputSchema !== "object") return tool;
    return { ...tool, inputSchema: normalizeSchemaUnionTypes(tool.inputSchema) };
  });
  return { ...response, result: { ...result, tools } };
}

/** Apply response normalization that depends on the request method. */
function finalizeResponse(
  request: JsonRpcRequest,
  response: JsonRpcResponse | null
): JsonRpcResponse | null {
  return request.method === "tools/list" ? normalizeToolsListResponse(response) : response;
}

export function createBridge(opts: BridgeOptions): Bridge {
  const remoteUrl = opts.remoteUrl;
  const getToken = opts.getToken;
  const fetchImpl = opts.fetchImpl ?? fetch;
  const ttl = opts.tokenCacheTtlMs ?? DEFAULT_TOKEN_CACHE_TTL_MS;
  const now = opts.now ?? Date.now;
  const log = opts.logError ?? ((m) => process.stderr.write(`${m}\n`));

  let cachedToken: { value: string; fetchedAt: number } | null = null;
  let sessionId: string | null = null;

  async function token(force: boolean): Promise<string | null> {
    if (!force && cachedToken && now() - cachedToken.fetchedAt < ttl) {
      return cachedToken.value;
    }
    const value = await getToken();
    if (value) {
      cachedToken = { value, fetchedAt: now() };
    } else {
      cachedToken = null;
    }
    return value;
  }

  function errorResponse(
    id: string | number | null | undefined,
    code: number,
    message: string
  ): JsonRpcResponse | null {
    if (id === undefined || id === null) return null;
    return { jsonrpc: "2.0", id, error: { code, message } };
  }

  async function attempt(
    request: JsonRpcRequest,
    forceRefresh: boolean
  ): Promise<
    | { kind: "ok"; response: JsonRpcResponse | null }
    | { kind: "retry" }
    | { kind: "err"; response: JsonRpcResponse | null }
  > {
    const bearer = await token(forceRefresh);
    if (!bearer) {
      return {
        kind: "err",
        response: errorResponse(
          request.id,
          -32000,
          "envoy-dispatch shim: gh auth token returned empty — check your gh-app setup"
        ),
      };
    }

    const headers: Record<string, string> = {
      Authorization: `Bearer ${bearer}`,
      "Content-Type": "application/json",
      Accept: "application/json, text/event-stream",
    };
    if (sessionId) headers["Mcp-Session-Id"] = sessionId;

    let response: Response;
    try {
      response = await fetchImpl(remoteUrl, {
        method: "POST",
        headers,
        body: JSON.stringify(request),
      });
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      return {
        kind: "err",
        response: errorResponse(request.id, -32603, `envoy-dispatch shim network error: ${msg}`),
      };
    }

    if (response.status === 401 && !forceRefresh) {
      return { kind: "retry" };
    }

    const respSession = response.headers.get("mcp-session-id");
    if (respSession) sessionId = respSession;

    if (!response.ok) {
      const body = await response.text().catch(() => "");
      return {
        kind: "err",
        response: errorResponse(
          request.id,
          -32603,
          `envoy-dispatch shim: remote ${response.status} ${response.statusText} ${body.slice(0, 200)}`
        ),
      };
    }

    if (request.id === undefined || request.id === null) {
      // Notification — no response expected by JSON-RPC contract.
      return { kind: "ok", response: null };
    }

    const body = await response.text();
    const ct = response.headers.get("content-type") ?? "";
    try {
      const parsed = ct.includes("text/event-stream") ? parseSseBody(body) : JSON.parse(body);
      if (parsed && typeof parsed === "object") {
        if (!forceRefresh && hasUpstreamUnauthorized(parsed)) {
          return { kind: "retry" };
        }
        return { kind: "ok", response: parsed as JsonRpcResponse };
      }
      return {
        kind: "err",
        response: errorResponse(
          request.id,
          -32603,
          "envoy-dispatch shim: empty/invalid response body"
        ),
      };
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      return {
        kind: "err",
        response: errorResponse(request.id, -32603, `envoy-dispatch shim: parse error: ${msg}`),
      };
    }
  }

  return {
    async handle(request: JsonRpcRequest): Promise<JsonRpcResponse | null> {
      const first = await attempt(request, false);
      if (first.kind === "ok" || first.kind === "err")
        return finalizeResponse(request, first.response);
      // retry once with forced refresh on 401
      log("envoy-dispatch shim: 401 from remote, re-minting token and retrying once");
      const second = await attempt(request, true);
      return finalizeResponse(request, second.kind === "retry" ? null : second.response);
    },
  };
}
