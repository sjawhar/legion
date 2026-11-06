// The session file an Oh My Pi of the dispatch-session-settings driver writes, read as the checks'
// oracle for turns, switches and the cards pi-envoy delivers.
import { readFileSync } from "node:fs";

export interface Entry {
  readonly type?: string;
  readonly timestamp?: string;
  readonly customType?: string;
  readonly model?: string;
  readonly role?: string;
  readonly data?: { readonly message_id?: string };
  readonly content?: unknown;
  readonly message?: {
    readonly role?: string;
    readonly provider?: string;
    readonly model?: string;
    readonly stopReason?: string;
    readonly errorMessage?: string;
    readonly tag?: string;
    /** When the host started the message, in milliseconds. */
    readonly timestamp?: number;
    readonly content?: unknown;
  };
}

/** The session file's entries; none while the host has not written it yet. */
export function entries(file: string): Entry[] {
  let raw: string;
  try {
    raw = readFileSync(file, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return [];
    throw error;
  }
  return raw.split("\n").flatMap((line) => {
    if (line.trim() === "") return [];
    try {
      return [JSON.parse(line) as Entry];
    } catch {
      return [];
    }
  });
}

export function text(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return JSON.stringify(content ?? "");
  return content
    .map((part: { type?: string; text?: string }) =>
      part.type === "text" ? (part.text ?? "") : ""
    )
    .join("\n");
}

export const userMessages = (file: string) =>
  entries(file).filter((entry) => entry.type === "message" && entry.message?.role === "user");
export const assistantMessages = (file: string) =>
  entries(file).filter((entry) => entry.type === "message" && entry.message?.role === "assistant");
export const turnModel = (entry: Entry) => `${entry.message?.provider}/${entry.message?.model}`;
/** Envoy cards holding `needle`: what pi-envoy delivers for every frame that is not a turn. */
export const cards = (all: readonly Entry[], needle: string) =>
  all.filter(
    (entry) =>
      entry.type === "custom_message" &&
      entry.customType === "envoy-message" &&
      text(entry.content).includes(needle)
  );
/** The attempts pi-envoy recorded as handled, by Dispatch message id. */
export const handledAttempts = (all: readonly Entry[]) =>
  all.flatMap((entry) =>
    entry.type === "custom" && entry.customType === "envoy-dispatch-handled-attempt"
      ? [entry.data?.message_id ?? ""]
      : []
  );
