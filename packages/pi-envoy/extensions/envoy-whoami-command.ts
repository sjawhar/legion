import { copyToClipboard } from "@oh-my-pi/pi-coding-agent";

type CommandContext = {
  readonly ui: { readonly notify: (message: string, level: "info" | "warning") => void };
  // OMP command handlers receive the full extension context; the live session
  // manager is the source of truth for the session ID. The envoy closure's
  // cached ID is stale for sessions created lazily after session_start.
  readonly sessionManager?: { readonly getSessionId: () => string };
};

type EnvoyWhoamiCommandApi = {
  readonly registerCommand: (
    name: string,
    command: {
      readonly description: string;
      readonly handler: (args: string, context: CommandContext) => Promise<void>;
    }
  ) => void;
};

export function registerEnvoyWhoamiCommand(
  api: EnvoyWhoamiCommandApi,
  cachedSessionID: () => string
): void {
  api.registerCommand("whoami", {
    description: "Copy session ID",
    handler: async (_args, context) => {
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

