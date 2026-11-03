import type { Handoff, HandoffSummary } from "@legion/contracts";
import { actorText } from "../actor-text";
import { type CommandContext, type DispatchToolResult, optionalNumber, stringArg } from "./context";
import { refusalWithCode } from "./failures";

/** `dispatch handoff-write`: record this phase's handoff on the issue. */
export async function handoffWriteCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, issue } = context;
  // The Legion daemon accepts a handoff only from the session that holds the phase's claim,
  // so a write names this session and no stand-in.
  if (!input.sessionId?.trim()) {
    throw new Error("host session id is required for dispatch handoff-write");
  }
  let recorded: HandoffSummary;
  try {
    // The body goes as the schema passed it, the text the caller wrote, never re-encoded.
    recorded = await client.writeHandoff(issue(), stringArg(args, "phase"), {
      actor,
      body: stringArg(args, "body"),
    });
  } catch (error) {
    // The description names ISSUE_CLOSED and CAP_EXCEEDED, so the code reaches the caller.
    throw refusalWithCode(error);
  }
  return {
    text: `Recorded the ${recorded.phase} handoff of ${recorded.issue_key}, version ${recorded.version} (id ${recorded.id})`,
    details: {
      issue: recorded.issue_key,
      phase: recorded.phase,
      version: recorded.version,
      handoff: recorded.id,
    },
  };
}

/** `dispatch handoff-read`: the newest handoff of one phase, or the version named. */
export async function handoffReadCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, issue } = context;
  const issueKey = issue();
  const phase = stringArg(args, "phase");
  const version = optionalNumber(args, "version");
  let handoff: Handoff;
  try {
    if (version === undefined) {
      handoff = await client.latestHandoff(issueKey, phase);
    } else {
      // Dispatch reads a handoff by id or the phase's newest, so a named version is found in
      // the issue's list (newest first, without bodies) and then read by its id.
      const listed = await client.listHandoffs(issueKey);
      const ofPhase = listed.filter((summary) => summary.phase === phase);
      const match = ofPhase.find((summary) => summary.version === version);
      if (match === undefined) {
        const newest = Math.max(0, ...ofPhase.map((summary) => summary.version));
        throw new Error(
          `HANDOFF_NOT_FOUND: issue ${issueKey} has no ${phase} handoff version ${version}; ${
            newest === 0
              ? `it has no ${phase} handoff at all`
              : `its newest ${phase} handoff is version ${newest}`
          }`
        );
      }
      handoff = await client.handoff(match.id);
    }
  } catch (error) {
    // HANDOFF_NOT_FOUND is named in the description, so the code reaches the caller.
    throw refusalWithCode(error);
  }
  // One line about the record, then the body exactly as stored.
  return {
    text: `${handoff.issue_key} ${handoff.phase} handoff, version ${handoff.version} (id ${handoff.id}), written by ${actorText(handoff.author)} at ${handoff.created_at}\n${handoff.body}`,
    details: {
      issue: handoff.issue_key,
      phase: handoff.phase,
      version: handoff.version,
      handoff: handoff.id,
    },
  };
}
