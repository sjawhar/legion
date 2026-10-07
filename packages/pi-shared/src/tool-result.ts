import { messageFor } from "@legion/envoy-client/errors";
import type { ToolResult } from "./pi-types";

export function toolSuccess(
  text: string,
  details: Readonly<Record<string, unknown>> = {}
): ToolResult {
  return { content: [{ type: "text", text }], details };
}

export function toolFailure(error: unknown): ToolResult {
  return { content: [{ type: "text", text: messageFor(error) }], details: {}, isError: true };
}
