import type { SessionContext, SideTurn } from "@legion/pi-shared/pi-types";

/**
 * Oh My Pi's /btw prompt (`packages/coding-agent/src/prompts/system/btw-user.md`, the same file
 * from 18.2.9 to 18.6.0) around `question`, which goes in as given.
 */
function btwPrompt(question: string): string {
  return [
    "<btw>",
    "Ephemeral side question for current interactive session.",
    "Answer briefly, directly; use conversation context already provided.",
    "NEVER use tools.",
    "NEVER ask follow-up questions.",
    "Question:",
    question,
    "</btw>",
  ].join("\n");
}

/**
 * The host's side turn, which a targeted Dispatch BTW and the stop-time self-check both use: one
 * hidden model call over a snapshot of the conversation that adds nothing to the transcript. Oh My
 * Pi serves it on the extension context as `runEphemeralTurn` (18.3 on), which sends the prompt as
 * given, so the question goes out in the /btw prompt here. Undefined when the host has none.
 */
export function sideTurn(context: SessionContext | undefined): SideTurn | undefined {
  const runEphemeralTurn = context?.runEphemeralTurn;
  if (runEphemeralTurn === undefined) return undefined;
  return ({ prompt, signal }) => runEphemeralTurn({ promptText: btwPrompt(prompt), signal });
}
