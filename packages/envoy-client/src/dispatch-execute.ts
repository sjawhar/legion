import { checkDispatchCall, dispatchServerUrls } from "./dispatch-commands/check";
import { commandContext } from "./dispatch-commands/command-context";
import {
  type DispatchToolResult,
  type ExecuteDispatchToolInput,
  sessionContext,
} from "./dispatch-commands/context";
import {
  type DispatchCommand,
  type DispatchToolName,
  dispatchCommands,
} from "./dispatch-commands/table";
import { ToolInputError } from "./tool-input-errors";

// What this module has always offered its callers, from the modules the command dispatcher is
// split into: the handlers are reached through the table alone, so none of them imports this one.
export { adviceMemory, resetAdviceMemory } from "./dispatch-commands/advice";
export type {
  CheckedCall,
  DispatchCallCheckInput,
  DispatchToolResult,
  ExecuteDispatchToolInput,
} from "./dispatch-commands/context";
export { asObject } from "./dispatch-commands/context";
export type {
  DocumentBlock,
  DocumentBlocks,
  DocumentTarget,
  DocumentTargetContext,
} from "./dispatch-commands/document-targets";
export { readDocumentBlocks, resolveIssueDocumentId } from "./dispatch-commands/document-targets";
export { dispatchRefFromUrl } from "./dispatch-commands/refs";
export { formatOpenAsksSummary, positionText } from "./dispatch-commands/render-item";
export { checkDispatchCall, dispatchServerUrls };

/** Validate and execute one native Dispatch tool against the JSON HTTP API. */
export async function executeDispatchTool(
  input: ExecuteDispatchToolInput
): Promise<DispatchToolResult> {
  // One validation pass: owner resolution, the strict schema, the cross-field hand checks and the
  // picture files; the call is refused once with every problem listed, or proceeds with arguments
  // the schema has fully validated (unknown keys and wrong types are impossible in a handler, so
  // structured arguments only need the contract's shape named).
  const checked = await checkDispatchCall({
    ...input,
    serverUrls: dispatchServerUrls(input.config),
  });
  if (checked.problems.length > 0) throw new ToolInputError(input.tool, checked.problems);
  // The check reads the tool's own schema, so an unknown tool is already refused by the time this
  // runs; the table is still read as a lookup that can miss, so the name stays in the message.
  const command: DispatchCommand | undefined = dispatchCommands[input.tool as DispatchToolName];
  if (command === undefined) throw new Error(`Unknown Dispatch tool: ${input.tool}`);
  const session = sessionContext(input, checked);
  // `dispatch_open_asks`, `dispatch_answers` and `dispatch_whoami` answer from the session alone;
  // every other command is handed the context that resolves this call's origin.
  return command.origin === "none"
    ? command.run(session)
    : command.run(await commandContext(session));
}
