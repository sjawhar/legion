import { asObject } from "@legion/envoy-client/dispatch-execute";
import type { ToolResultEvent } from "./pi-types";

/** The count a write's advice reports under key, 0 when it reports none. */
function adviceCount(details: unknown, key: "decision_blocks" | "decision_blocks_added"): number {
  const count = asObject(asObject(details)?.advice)?.[key];
  return typeof count === "number" ? count : 0;
}

/**
 * Whether a successful call opened the ask itself, so the run-end nudge has nothing to say:
 * `dispatch_ask`, `dispatch_request_approval`, a `dispatch_issue` or `dispatch_artifact` whose
 * stored document holds a decision block (`advice.decision_blocks`), or a `dispatch_doc_edit` that
 * added one (`advice.decision_blocks_added`). Both counts are the server's own reading of the
 * document, so an opener quoted in code counts nothing and one in a blockquote or a list item
 * counts. Both count answered blocks too - every block in a stored document, and a block an edit
 * writes back under an answered ask's id - so re-uploading a document whose blocks are all answered
 * reads as opening one and that stop goes without a reminder: no result tells the two apart.
 */
export function opensAsk({ toolName, details }: ToolResultEvent): boolean {
  switch (toolName) {
    case "dispatch_ask":
    case "dispatch_request_approval":
      return true;
    case "dispatch_issue":
    case "dispatch_artifact":
      return adviceCount(details, "decision_blocks") > 0;
    case "dispatch_doc_edit":
      return adviceCount(details, "decision_blocks_added") > 0;
    default:
      return false;
  }
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
