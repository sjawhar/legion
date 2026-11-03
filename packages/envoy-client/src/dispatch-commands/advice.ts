import type { WriteAdvice } from "@legion/contracts";
import { messageFor } from "../errors";

const triageAdviceShown = new Set<string>();

export function resetAdviceMemory(): void {
  triageAdviceShown.clear();
}

/** The triage keys this process has shown, so a short-lived host can carry them across calls. */
export function adviceMemory(): Set<string> {
  return triageAdviceShown;
}

export function renderAdvice(
  tool: string,
  key: string,
  advice: WriteAdvice | undefined,
  opts: {
    isAskReply?: boolean;
    isPrimarySpec?: boolean;
    setsStatus?: boolean;
    replyToOwnAsk?: boolean;
  }
): string[] {
  if (advice === undefined) return [];

  const hasIssueAdvice = advice.issue_status !== undefined;
  const openAsks = advice.your_open_asks;
  const writesSinceHuman = advice.session_writes_since_human;
  const lines: string[] = [];
  const unparsed = advice.unparsed_openers;
  if (
    unparsed !== undefined &&
    unparsed.count > 0 &&
    (tool === "dispatch_issue" || tool === "dispatch_artifact")
  ) {
    const quoted = unparsed.examples.map((example) => JSON.stringify(example)).join(", ");
    const subject =
      unparsed.count === 1
        ? "1 typed-block opening in this document is text, not a block"
        : `${unparsed.count} typed-block openings in this document are text, not blocks`;
    lines.push(
      `${subject}: ${quoted}. An opening like \`:::ask{…}\` makes a block only as a line of its own, so as text it asks nobody. Mentioning the syntax on purpose? Put it in code. See the \`dispatch-writing-specs\` skill, "Questions".`
    );
  }
  if (
    advice.decision_blocks === 0 &&
    opts.isPrimarySpec === true &&
    (tool === "dispatch_issue" || tool === "dispatch_artifact")
  ) {
    lines.push(
      "This spec holds no ask blocks. A person does not watch your chat. They see that you need them only through an open ask or a spec that waits for their approval. If the design needs a person's choice, add an ask block. If it does not, request approval with `dispatch request-approval`."
    );
  }

  // The advice is dropped, not reworded, on a Legion-managed issue: the Legion daemon moves such
  // an issue out of triage itself when it releases it, so asking the session to move the status
  // asks it to do the one thing the daemon undoes. The memo is marked inside the block, so a key
  // whose line was never printed can still be told later if it leaves Legion's hands.
  if (
    advice.issue_status === "triage" &&
    advice.legion_managed_status !== true &&
    tool !== "dispatch_issue" &&
    !(tool === "dispatch_issue_update" && opts.setsStatus === true) &&
    !triageAdviceShown.has(key)
  ) {
    triageAdviceShown.add(key);
    lines.push(
      `${key} is still in triage — nobody can see its development status. See the \`dispatch\` skill, "Issue status is yours to move".`
    );
  }

  if (
    hasIssueAdvice &&
    openAsks !== undefined &&
    openAsks.length > 0 &&
    (tool === "dispatch_message" ||
      tool === "dispatch_doc_edit" ||
      tool === "dispatch_issue_update" ||
      (tool === "dispatch_comment" && opts.replyToOwnAsk !== true))
  ) {
    const askCount = Math.min(openAsks.length, 2);
    for (let index = 0; index < askCount; index += 1) {
      const ask = openAsks[index];
      if (ask === undefined) break;
      lines.push(
        `You still have an open ask on ${key}: "${ask.question.slice(0, 80)}" (${ask.id}). Still needed? See the \`dispatch\` skill, "Close what you opened".`
      );
    }
  }

  // The gate line is last for the document tools, for the reason the cadence line is last for
  // the others: behind `| tail -1` it is the only line the agent sees, and it is the one line of
  // a document write that someone else has to act on. It describes this one write - the version
  // it stored and the approval that version replaced - rather than a rule about editing a spec,
  // because the server sends it only on the write that superseded the approval.
  const superseded = advice.superseded_approval;
  if (superseded !== undefined && (tool === "dispatch_doc_edit" || tool === "dispatch_artifact")) {
    lines.push(
      `Stored version ${superseded.version}, superseding version ${superseded.approved_version}, which ${superseded.approved_by} approved. That approval is stale until someone approves version ${superseded.version}, and no undo restores it: a Legion design gate on this document is closed meanwhile, so every READY in its tree is refused. Meant to change the approved design? Request approval with \`dispatch request-approval\`. Meant a correction beside it? Tell the tree's architect, and keep the next one in a comment or in the plan.`
    );
  }

  // The cadence line comes last and opens by saying the write went through: behind `| tail -1`
  // it is the only line an agent sees, and "Stop posting" alone reads as a refusal.
  if (
    hasIssueAdvice &&
    writesSinceHuman !== undefined &&
    writesSinceHuman >= 3 &&
    (tool === "dispatch_message" ||
      tool === "dispatch_ask" ||
      (tool === "dispatch_comment" && opts.isAskReply !== true))
  ) {
    const middle =
      writesSinceHuman >= 6
        ? "Stop posting here until a human replies. Their reply ends this: post then what you held back."
        : "Progress ledger or scratchpad? If so, stop.";
    lines.push(
      `Posted. You've sent ${writesSinceHuman} messages on ${key} with no human response. ${middle} See the \`dispatch\` skill, "Structure over stream".`
    );
  }

  return lines;
}

/** The advice lines a write prints after its related-item check, or one line saying why they
 *  could not be shown: the write already succeeded, and an exception here must not cost the
 *  reader the line naming what was written or what the check found. */
export async function otherAdviceLines(
  render: () => string[] | Promise<string[]>
): Promise<string[]> {
  try {
    return await render();
  } catch (error) {
    return [`Other advice could not be shown: ${messageFor(error)}`];
  }
}
