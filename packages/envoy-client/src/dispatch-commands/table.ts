import type { dispatchToolSpecs } from "@legion/contracts";
import {
  askCommand,
  editAskCommand,
  followCommand,
  openAsksCommand,
  resolveAskCommand,
} from "./asks";
import type { CommandContext, DispatchToolResult, SessionContext } from "./context";
import {
  commentCommand,
  messageCommand,
  resolveCommentCommand,
  suggestCommand,
} from "./conversation";
import {
  artifactCommand,
  docEditCommand,
  docReadCommand,
  planApproveCommand,
  requestApprovalCommand,
} from "./documents";
import { handoffReadCommand, handoffWriteCommand } from "./handoffs";
import {
  claimCommand,
  issueCommand,
  issuesCommand,
  issueUpdateCommand,
  relatedCommand,
  resultCommand,
  resultsCommand,
  searchCommand,
} from "./issues";
import { architectureSyncCommand, projectCommand, projectsCommand } from "./projects";
import { readCommand } from "./read";
import { answersCommand, whoamiCommand } from "./session";

/** Every command the contract names, so a command added there without a handler here fails to
 *  type-check (`__tests__/dispatch-command-table.test.ts` holds the proof). */
export type DispatchToolName = (typeof dispatchToolSpecs)[number]["name"];

/**
 * One command, and whether running it resolves this call's origin. `origin: "none"` is a command
 * that answers on the session's context alone, so no subprocess runs for it; every other command
 * is handed the context `commandContext` builds.
 */
export type DispatchCommand =
  | {
      readonly origin: "resolved";
      readonly run: (context: CommandContext) => Promise<DispatchToolResult>;
    }
  | {
      readonly origin: "none";
      readonly run: (context: SessionContext) => Promise<DispatchToolResult>;
    };

/** A command that acts for the session: it is handed the context whose origin was resolved. */
function acting(run: (context: CommandContext) => Promise<DispatchToolResult>): DispatchCommand {
  return { origin: "resolved", run };
}

/** A command that answers from the session alone, resolving no origin. */
function onSession(run: (context: SessionContext) => Promise<DispatchToolResult>): DispatchCommand {
  return { origin: "none", run };
}

/** What `dispatch <command>` does, one handler per command the contract names. */
export const dispatchCommands: Record<DispatchToolName, DispatchCommand> = {
  dispatch_issue: acting(issueCommand),
  dispatch_issue_update: acting(issueUpdateCommand),
  dispatch_claim: acting(claimCommand),
  dispatch_issues: acting(issuesCommand),
  dispatch_related: acting(relatedCommand),
  dispatch_search: acting(searchCommand),
  dispatch_project: acting(projectCommand),
  dispatch_projects: acting(projectsCommand),
  dispatch_architecture_sync: acting(architectureSyncCommand),
  dispatch_ask: acting(askCommand),
  dispatch_edit_ask: acting(editAskCommand),
  dispatch_resolve_ask: acting(resolveAskCommand),
  dispatch_follow: acting(followCommand),
  dispatch_open_asks: onSession(openAsksCommand),
  dispatch_comment: acting(commentCommand),
  dispatch_suggest: acting(suggestCommand),
  dispatch_resolve_comment: acting(resolveCommentCommand),
  dispatch_message: acting(messageCommand),
  dispatch_doc_edit: acting(docEditCommand),
  dispatch_doc_read: acting(docReadCommand),
  dispatch_request_approval: acting(requestApprovalCommand),
  dispatch_plan_approve: acting(planApproveCommand),
  dispatch_result: acting(resultCommand),
  dispatch_results: acting(resultsCommand),
  dispatch_artifact: acting(artifactCommand),
  dispatch_handoff_write: acting(handoffWriteCommand),
  dispatch_handoff_read: acting(handoffReadCommand),
  dispatch_read: acting(readCommand),
  dispatch_answers: onSession(answersCommand),
  dispatch_whoami: onSession(whoamiCommand),
};
