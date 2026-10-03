import type { EditOp } from "@legion/contracts";
import { asObject } from "@legion/envoy-client/dispatch-execute";
import type { ToolResultEvent } from "./pi-types";

/**
 * An `ask` block's opener: `:::ask{` at the start of a line, after nothing but spaces or tabs, as the
 * server reads an opener only on a line of its own. It is matched one line at a time, so a long
 * blank run is read once.
 */
const ASK_BLOCK_OPENER = /^[ \t]*:::ask\{/u;

/** A code fence line: up to three spaces, three or more backticks or tildes, then the rest. */
const CODE_FENCE = /^ {0,3}(`{3,}|~{3,})(.*)$/u;

/**
 * Whether markdown holds an `ask` block's opener outside fenced code, which the server stores as
 * text. A fence opens on a line of three or more backticks (with no backtick after them) or tildes,
 * and closes on a line of nothing but the same character at least as many times; an unclosed fence
 * runs to the end.
 */
function opensAskBlock(markdown: string): boolean {
  let fence: string | undefined;
  for (const line of markdown.split(/\r\n|\r|\n/u)) {
    const [, marker, rest = ""] = CODE_FENCE.exec(line) ?? [];
    if (fence === undefined) {
      if (marker !== undefined && !(marker.startsWith("`") && rest.includes("`"))) fence = marker;
      else if (ASK_BLOCK_OPENER.test(line)) return true;
    } else if (
      marker !== undefined &&
      marker[0] === fence[0] &&
      marker.length >= fence.length &&
      rest.trim() === ""
    ) {
      fence = undefined;
    }
  }
  return false;
}

/**
 * Whether a successful call opened the ask itself, so the run-end nudge has nothing to say:
 * `dispatch_ask`, `dispatch_request_approval`, a `dispatch_issue` or `dispatch_artifact` whose
 * stored document holds a decision block, or a `dispatch_doc_edit` that writes one, by inserting an
 * `ask` block outside fenced code or by retyping a block into one. The server counts every block in
 * a stored document (`advice.decision_blocks`), answered ones included, so re-uploading a document
 * whose blocks are all answered reads as opening one and that stop goes without a reminder: no
 * result tells the two apart.
 */
export function opensAsk({ toolName, input, details }: ToolResultEvent): boolean {
  if (toolName === "dispatch_ask" || toolName === "dispatch_request_approval") return true;
  if (toolName === "dispatch_issue" || toolName === "dispatch_artifact") {
    const blocks = asObject(asObject(details)?.advice)?.decision_blocks;
    return typeof blocks === "number" && blocks > 0;
  }
  if (toolName !== "dispatch_doc_edit" || !Array.isArray(input.ops)) return false;
  // A successful edit's operations passed the tool's schema.
  return (input.ops as readonly EditOp[]).some(
    ({ op, markdown, type }) =>
      (op === "insert" && markdown !== undefined && opensAskBlock(markdown)) ||
      (op === "retype" && type === "ask")
  );
}

/**
 * The tool an Oh My Pi tool-device `write` (to `xd://<tool>`) ran, or documented for content that
 * asks for help, from the `details.xdev` the host reports on that write's result whatever spelling
 * of the path it accepted; undefined for any other call.
 */
export function deviceTool({ toolName, details }: ToolResultEvent): string | undefined {
  if (toolName !== "write") return undefined;
  const tool = asObject(asObject(details)?.xdev)?.tool;
  return typeof tool === "string" ? tool : undefined;
}
