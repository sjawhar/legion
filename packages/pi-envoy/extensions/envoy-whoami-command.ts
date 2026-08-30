import { copyToClipboard } from "@oh-my-pi/pi-coding-agent";
import type { CommandContext, PiApi } from "../src/pi-types";

export function registerEnvoyWhoamiCommand(
  api: Pick<PiApi, "registerCommand">,
  cachedSessionID: () => string
): void {
  api.registerCommand("whoami", {
    description: "Copy session ID",
    handler: async (_args, context: CommandContext) => {
      // The live session manager is the source of truth for the session ID;
      // the envoy closure's cached ID is stale for sessions created lazily
      // after session_start. Fall back to the cached ID when a host invokes
      // the command without a session manager in context.
      const sessionID = context.sessionManager?.getSessionId() || cachedSessionID();
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
