import { ClaudeCodeAdapter } from "./claude-code";
import { OpenCodeAdapter } from "./opencode";
import type { RuntimeAdapter } from "./types";

export function createAdapter(
  runtime: "opencode" | "claude-code",
  opts: { port: number; shortId: string }
): RuntimeAdapter {
  if (runtime === "claude-code") {
    return new ClaudeCodeAdapter(opts.shortId);
  }
  return new OpenCodeAdapter(opts.port);
}

export type { RuntimeAdapter } from "./types";
