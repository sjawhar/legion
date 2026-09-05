// The HTTP client to the dispatch service. One stateless Streamable HTTP
// tools/call per dispatch: a token minted for this call, no MCP session, no
// initialize handshake, no cache, no retry. The service reads the bearer from
// each POST and forwards it to GitHub verbatim.

import { execFile } from "node:child_process";
import { promisify } from "node:util";
import type { DispatchServiceArguments } from "./dispatch-call";
import { DISPATCH_TOOL_NAME } from "./dispatch-contract";
import { messageFor } from "./errors";

const execFileAsync = promisify(execFile);

export type TokenGetter = () => Promise<string | null>;

/** Mint a GitHub token the way the session's own shell would: `gh auth token` in the session cwd, so the user's per-repo gh profile applies. */
export function ghTokenGetter(cwd: string): TokenGetter {
  return async () => {
    try {
      const { stdout } = await execFileAsync("gh", ["auth", "token"], { cwd, timeout: 5_000 });
      const value = stdout.trim();
      return value.length > 0 ? value : null;
    } catch {
      return null;
    }
  };
}

export interface DispatchClientOptions {
  /** The service's /mcp endpoint, from resolveDispatchConfig(). */
  readonly serviceUrl: string;
  readonly getToken: TokenGetter;
  readonly fetchImpl?: typeof fetch;
}

export interface DispatchServiceResult {
  readonly thread: number;
  readonly url: string;
  readonly comment?: string;
}

export class DispatchServiceError extends Error {
  override readonly name = "DispatchServiceError";
  constructor(
    readonly kind: "auth" | "transport" | "tool",
    message: string
  ) {
    super(message);
  }
}

interface JsonRpcResponse {
  result?: { isError?: boolean; content?: Array<{ type?: string; text?: string }> };
  error?: { message?: string };
}

/** A Streamable HTTP response is either plain JSON or one SSE `data:` line. */
function parseResponseBody(body: string, contentType: string): unknown {
  if (!contentType.includes("text/event-stream")) return JSON.parse(body);
  for (const line of body.split("\n")) {
    const payload = line.match(/^data:\s*(.+)$/)?.[1];
    if (payload !== undefined) return JSON.parse(payload);
  }
  return null;
}

function isDispatchServiceResult(value: unknown): value is DispatchServiceResult {
  return (
    typeof value === "object" &&
    value !== null &&
    "thread" in value &&
    typeof value.thread === "number" &&
    "url" in value &&
    typeof value.url === "string"
  );
}

export async function callDispatch(
  options: DispatchClientOptions,
  args: DispatchServiceArguments
): Promise<DispatchServiceResult> {
  const token = await options.getToken();
  if (!token) {
    throw new DispatchServiceError(
      "auth",
      `dispatch: gh auth token returned empty in ${args.origin.cwd} — check your gh-app setup`
    );
  }
  const fetchImpl = options.fetchImpl ?? fetch;
  let response: Response;
  try {
    response = await fetchImpl(options.serviceUrl, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "tools/call",
        params: { name: DISPATCH_TOOL_NAME, arguments: args },
      }),
    });
  } catch (error) {
    throw new DispatchServiceError(
      "transport",
      `dispatch service unreachable at ${options.serviceUrl}: ${messageFor(error)}`
    );
  }
  const body = await response.text();
  if (response.status === 401) {
    throw new DispatchServiceError(
      "auth",
      `dispatch service rejected the GitHub token (401): ${body.slice(0, 200)}`
    );
  }
  if (!response.ok) {
    throw new DispatchServiceError(
      "transport",
      `dispatch service returned ${response.status} ${response.statusText}: ${body.slice(0, 200)}`
    );
  }
  let parsed: unknown;
  try {
    parsed = parseResponseBody(body, response.headers.get("content-type") ?? "");
  } catch (error) {
    throw new DispatchServiceError(
      "transport",
      `dispatch service sent an unreadable response: ${messageFor(error)}`
    );
  }
  const rpc = (parsed ?? {}) as JsonRpcResponse;
  if (rpc.error) {
    throw new DispatchServiceError(
      "tool",
      rpc.error.message ?? "dispatch service returned an error"
    );
  }
  const text = (rpc.result?.content ?? [])
    .map((item) => item.text ?? "")
    .filter(Boolean)
    .join("\n");
  if (rpc.result?.isError) throw new DispatchServiceError("tool", text || "dispatch failed");
  let result: unknown;
  try {
    result = JSON.parse(text);
  } catch {
    throw new DispatchServiceError(
      "transport",
      `dispatch service returned a non-JSON result: ${text.slice(0, 200)}`
    );
  }
  if (!isDispatchServiceResult(result)) {
    throw new DispatchServiceError(
      "transport",
      `dispatch service result lacks thread/url: ${text.slice(0, 200)}`
    );
  }
  return result;
}
