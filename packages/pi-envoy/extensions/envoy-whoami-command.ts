import { copyToClipboard } from "@oh-my-pi/pi-coding-agent";
import type { CommandContext, PiApi } from "../src/pi-types";

export function registerEnvoyWhoamiCommand(
  api: Pick<PiApi, "registerCommand">,
  replyAddress: (context: CommandContext, liveSessionID: string) => Promise<string>
): void {
  api.registerCommand("whoami", {
    description: "Copy session ID",
    handler: async (_args, context: CommandContext) => {
      // The extension resolves the address a reply reaches: the live session manager for an
      // ordinary session — it is the source of truth, the envoy closure's cached ID being
      // stale for sessions created lazily after session_start — and the session that spawned
      // it for a `task` subagent, whose own live id is registered nowhere. A session this
      // process cannot place, or a process with no session at all, reports none.
      const sessionID = await replyAddress(context, context.sessionManager.getSessionId());
      if (sessionID === "") {
        context.ui.notify("No active session", "warning");
        return;
      }
      const copied = await copySessionID(copyToClipboard, sessionID);
      context.ui.notify(
        copied ? `Session ID copied: ${sessionID}` : `Could not copy session ID: ${sessionID}`,
        copied ? "info" : "warning"
      );
    },
  });
}

async function copySessionID(
  copyToClipboard: (text: string) => Promise<void>,
  sessionID: string
): Promise<boolean> {
  try {
    await copyToClipboard(sessionID);
    return true;
  } catch {
    return false;
  }
}
