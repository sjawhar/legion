import type { PiApi, SessionContext, SideTurn } from "./pi-types";

/**
 * Oh My Pi's /btw prompt (`packages/coding-agent/src/prompts/system/btw-user.md`, the same file
 * from 18.2.9 to 18.3.2) around `question`, which goes in as given.
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
 * hidden model call over a snapshot of the conversation that adds nothing to the transcript.
 * Oh My Pi 18.3 serves it on the extension context as `runEphemeralTurn`, which sends the prompt
 * as given, so the question goes out in the /btw prompt here; it wins when a host has both.
 * Earlier fork releases have only `pi.askEphemeral`, which adds that prompt itself; Legion pins
 * one (18.2.9, in packages/daemon/src/daemon/omp-pin.ts), so it stays the fallback until that
 * pin reaches 18.3. Undefined when the host has neither.
 */
export function sideTurn(pi: PiApi, context: SessionContext | undefined): SideTurn | undefined {
  const runEphemeralTurn = context?.runEphemeralTurn;
  if (runEphemeralTurn === undefined) return pi.askEphemeral;
  return ({ prompt, signal }) => runEphemeralTurn({ promptText: btwPrompt(prompt), signal });
}
