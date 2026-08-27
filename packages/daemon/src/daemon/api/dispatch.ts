import { randomUUID } from "node:crypto";

export type DispatchFetch = (
  input: string | URL | Request,
  init?: RequestInit
) => Promise<Response>;

export interface DispatchDeps {
  url: string;
  bearer: string;
  fetch?: DispatchFetch;
}

export type DispatchThreadResult = { thread: number; url: string };

export async function dispatchThread(
  deps: DispatchDeps,
  parent: string,
  subject: string,
  body: string,
  ask: unknown,
  urgency: unknown
): Promise<DispatchThreadResult> {
  const response = await (deps.fetch ?? fetch)(`${deps.url.replace(/\/+$/, "")}/mcp`, {
    method: "POST",
    headers: {
      Accept: "application/json, text/event-stream",
      Authorization: `Bearer ${deps.bearer}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      jsonrpc: "2.0",
      id: randomUUID(),
      method: "tools/call",
      params: {
        name: "dispatch",
        arguments: {
          parent,
          subject,
          body,
          ...(ask === undefined ? {} : { ask }),
          ...(urgency === undefined ? {} : { urgency }),
        },
      },
    }),
  });
  const responseBody = await response.text();
  if (!response.ok) {
    throw new Error(`Dispatch MCP request failed with ${response.status}: ${responseBody}`);
  }
  const payload: unknown = JSON.parse(responseBody);
  if (typeof payload !== "object" || payload === null || !("result" in payload)) {
    throw new Error("Dispatch MCP response is missing a result");
  }
  const result = payload.result;
  if (
    typeof result !== "object" ||
    result === null ||
    !("content" in result) ||
    !Array.isArray(result.content)
  ) {
    throw new Error("Dispatch MCP response has invalid content");
  }
  const text = result.content[0];
  if (
    typeof text !== "object" ||
    text === null ||
    !("text" in text) ||
    typeof text.text !== "string"
  ) {
    throw new Error("Dispatch MCP response is missing result text");
  }
  const dispatched: unknown = JSON.parse(text.text);
  if (
    typeof dispatched !== "object" ||
    dispatched === null ||
    !("thread" in dispatched) ||
    typeof dispatched.thread !== "number" ||
    !Number.isInteger(dispatched.thread) ||
    !("url" in dispatched) ||
    typeof dispatched.url !== "string"
  ) {
    throw new Error("Dispatch MCP result must contain an integer thread and URL");
  }
  return { thread: dispatched.thread, url: dispatched.url };
}
