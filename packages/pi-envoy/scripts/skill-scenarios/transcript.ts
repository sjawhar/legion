// The Oh My Pi session transcript shapes score.ts and seed.ts both read, so a run's own JSONL has
// one parser and one notion of what a tool result says, never two that can drift.
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

/** An Oh My Pi session transcript line: a message, whose assistant content holds each tool call
 * whole (`toolCall`: id, tool name, arguments) and whose tool results name the call they answer.
 * Its `tool_execution_start` entries keep only the first 200 characters of the arguments, so
 * nothing here reads them. */
export const SessionEntry = z.looseObject({
  type: z.string().optional(),
  timestamp: z.string().optional(),
  message: z
    .looseObject({
      role: z.string().optional(),
      toolName: z.string().optional(),
      toolCallId: z.string().optional(),
      content: z
        .array(
          z.looseObject({
            type: z.string(),
            text: z.string().optional(),
            id: z.string().optional(),
            name: z.string().optional(),
            arguments: z.unknown().optional(),
          })
        )
        .optional(),
    })
    .optional(),
});
export type SessionEntry = z.infer<typeof SessionEntry>;

/** The run's own session transcript, parsed whole, or empty when it never got a model turn. A line
 * that does not parse is the rig's fault. */
export function session(runDir: string): SessionEntry[] {
  const dir = path.join(runDir, "sessions");
  const file = existsSync(dir)
    ? readdirSync(dir).find((name) => name.endsWith(".jsonl"))
    : undefined;
  if (file === undefined) return [];
  const full = path.join(dir, file);
  return readFileSync(full, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line, index) => {
      const result = SessionEntry.safeParse(JSON.parse(line));
      if (!result.success)
        throw new Error(`${full}:${index + 1} is not a record: ${result.error.message}`);
      return result.data;
    });
}

/** Every tool result's text, in order: a bare `env`/`printenv` dump reaches the model only here, in
 * the `toolResult` message a tool call's own arguments never carry. */
export function toolResults(entries: SessionEntry[]): string[] {
  return entries.flatMap((entry) => {
    const message = entry.message;
    if (message?.role !== "toolResult") return [];
    return [(message.content ?? []).map((part) => part.text ?? "").join("")];
  });
}

/** Dispatch's answer when `dispatch_issue` creates an issue, whatever reached it: a top-level
 * `write` to the `xd://dispatch_issue` device, the same inside an `eval` cell, or a host that calls
 * the tool by name. A refusal reads `Not created:`, which the capital and the key after it keep
 * from matching. */
export const CREATED = /\bCreated ([A-Z][A-Z0-9]*-\d+):/;
