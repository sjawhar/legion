// The Oh My Pi session transcript shapes score.ts and seed.ts both read, so a run's own JSONL has
// one parser and one notion of what a tool result says, never two that can drift.
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

/** An Oh My Pi session transcript line: a message, whose assistant content holds each tool call
 * whole (`toolCall`: id, tool name, arguments) and whose tool results name the call they answer,
 * whether the tool refused it (`isError`) and the tool's own record (`details`; a `bash` result's
 * holds its `exitCode`). Its `tool_execution_start` entries keep only the first 200 characters of
 * the arguments, so nothing here reads them. The first line is the header, `{"type":"session",
 * "id":"<session id>"}`. A background task's report arrives as a line of its own
 * (`"type":"custom_message"`, `"customType":"async-result"`) whose `content` is its text. */
export const SessionEntry = z.looseObject({
  type: z.string().optional(),
  id: z.unknown().optional(),
  timestamp: z.string().optional(),
  customType: z.string().optional(),
  content: z.unknown().optional(),
  message: z
    .looseObject({
      role: z.string().optional(),
      toolName: z.string().optional(),
      toolCallId: z.string().optional(),
      isError: z.boolean().optional(),
      details: z.unknown().optional(),
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

/** The session id the transcript's header names, or undefined when it has no header. */
export function sessionId(entries: SessionEntry[]): string | undefined {
  const id = entries.find((entry) => entry.type === "session")?.id;
  return typeof id === "string" ? id : undefined;
}

/** The file of the run's own session transcript, or undefined when it never got a model turn. */
function sessionFile(runDir: string): string | undefined {
  const dir = path.join(runDir, "sessions");
  const file = existsSync(dir)
    ? readdirSync(dir).find((name) => name.endsWith(".jsonl"))
    : undefined;
  return file === undefined ? undefined : path.join(dir, file);
}
/** A transcript file, parsed whole. A line that does not parse is the rig's fault. */
function parsedSession(full: string): SessionEntry[] {
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

/** The run's own session transcript, parsed whole, or empty when it never got a model turn. A line
 * that does not parse is the rig's fault. */
export function session(runDir: string): SessionEntry[] {
  const full = sessionFile(runDir);
  return full === undefined ? [] : parsedSession(full);
}

/** Each subagent the run's session started, by its name, with its transcript parsed whole. Oh My Pi
 * writes every subagent's transcript to `<session file less .jsonl>/<name>.jsonl` beside the
 * session's own, whatever started it: a `task` tool call, or `tool.task(…)` or the `agent(…)`
 * helper inside an eval cell. A subagent's own subagents, one directory deeper, are left out. */
export function subagentSessions(runDir: string): { name: string; entries: SessionEntry[] }[] {
  const full = sessionFile(runDir);
  if (full === undefined) return [];
  const dir = full.replace(/\.jsonl$/, "");
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(".jsonl"))
    .map((entry) => entry.name)
    .sort()
    .map((name) => ({
      name: name.replace(/\.jsonl$/, ""),
      entries: parsedSession(path.join(dir, name)),
    }));
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

/** Dispatch's answer when `dispatch issue` creates an issue, whatever reached it: the command in a
 * `bash` call, the same run from inside an `eval` cell, or a host that calls the tool by name. A
 * refusal reads `Not created:`, which the capital and the key after it keep from matching. */
export const CREATED = /\bCreated ([A-Z][A-Z0-9]*-\d+):/;
