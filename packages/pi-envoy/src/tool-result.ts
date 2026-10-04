import type { ToolImage } from "@legion/envoy-client/dispatch-pictures";
import { messageFor } from "@legion/envoy-client/errors";
import type { ToolResult } from "./pi-types";

/** A result the model reads: the text, then each picture as an image block. */
export function toolSuccess(
  text: string,
  details: Readonly<Record<string, unknown>> = {},
  images: readonly ToolImage[] = []
): ToolResult {
  return {
    content: [
      { type: "text", text },
      ...images.map(({ data, mimeType }) => ({ type: "image" as const, data, mimeType })),
    ],
    details,
  };
}

export function toolFailure(error: unknown): ToolResult {
  return { content: [{ type: "text", text: messageFor(error) }], details: {}, isError: true };
}
