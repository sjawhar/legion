import type { ToolCallEvent } from "./pi-types";

const TOOL_DEVICE_SCHEME = "xd://";

/**
 * The tool an Oh My Pi tool-device invocation calls, or undefined for any other call. A host offers
 * an extension tool as a device too: a `write` whose `path` is `xd://<tool>` and whose `content`
 * carries the call's input, JSON arguments for the extension tools. Such a `write` is that tool's
 * call, never a file mutation.
 */
export function toolDeviceName(
  call: Pick<ToolCallEvent, "toolName" | "input">
): string | undefined {
  const { path } = call.input;
  if (call.toolName !== "write" || typeof path !== "string") return undefined;
  if (!path.startsWith(TOOL_DEVICE_SCHEME)) return undefined;
  return path.slice(TOOL_DEVICE_SCHEME.length);
}
