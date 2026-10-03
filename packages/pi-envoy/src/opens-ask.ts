import type { EditOp } from "@legion/contracts";
import { asObject } from "@legion/envoy-client/dispatch-execute";
import type { ToolResultEvent } from "./pi-types";

/**
 * An `ask` block's opener: `:::ask{` at the start of a line, after nothing but spaces or tabs, as the
 * server reads an opener only on a line of its own. `[ \t]*` keeps each attempt on one line, where a
 * `\s*` would cross line breaks and make every line start of a long blank run rescan the run.
 */
const ASK_BLOCK_OPENER = /^[ \t]*:::ask\{/mu;

/**
 * Whether a successful call opened the ask itself, so the run-end nudge has nothing to say:
 * `dispatch_ask`, `dispatch_request_approval`, a `dispatch_issue` or `dispatch_artifact` whose
 * stored document holds a decision block, or a `dispatch_doc_edit` that writes one, by inserting an
 * `ask` block or by retyping a block into one. The server counts every block in a stored document
 * (`advice.decision_blocks`), answered ones included, so re-uploading a document whose blocks are
 * all answered reads as opening one and that stop goes without a reminder: no result tells the two
 * apart.
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
      (op === "insert" && markdown !== undefined && ASK_BLOCK_OPENER.test(markdown)) ||
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
