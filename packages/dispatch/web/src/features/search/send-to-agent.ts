import { api } from "../../api/client";
import { type Attempt, plainWrite, type Writes } from "../writes/write-store";

/**
 * The words one send carries. The route the send goes through
 * (`POST /api/v1/agents/{session_id}/messages`) belongs to no issue, so the issue the person was
 * looking at is named in the text or the agent cannot tell which work this is about. The
 * person's own words follow it, as they typed them: nothing here rewrites them, and nothing
 * here adds a word of its own.
 */
export function handoffBody(issue: string, words: string): string {
  return `${issue}: ${words}`;
}

/** What one approved offer sends. */
export interface SendToAgentWrite {
  /** The issue the bar was opened on, which the body names. */
  readonly issue: string;
  /** The session holding that issue's claim, which the message goes to. */
  readonly session: string;
  /** The words the person typed, as they typed them. */
  readonly words: string;
}

/** How the offer sends; the tests pass their own. */
export type SendToAgent = (writes: Writes, write: SendToAgentWrite) => Promise<void>;

/**
 * Sends the person's words to the session on the issue, through the route every other client
 * sends a person's message to one session through. One press is one request, under a key of its
 * own, tried until Dispatch decides it and named in the shell's indicator while it is out;
 * `inline` because the bar says a refusal itself.
 *
 * `aside` is the mode: the words reach the session while its turn runs, which is what a person
 * handing over work at this moment means, and it is the mode the bar's other write (the first
 * message on a new issue) already uses.
 */
export async function sendToAgent(
  writes: Writes,
  { issue, session, words }: SendToAgentWrite
): Promise<void> {
  const write: Attempt<unknown> = (bounds) =>
    api.createAgentMessage(session, { body: handoffBody(issue, words), delivery: "aside" }, bounds);
  await writes.run({
    detail: null,
    inline: true,
    kind: plainWrite,
    label: `“${words}” to the agent on ${issue}`,
    presentation: "indicator",
    write,
  });
}
