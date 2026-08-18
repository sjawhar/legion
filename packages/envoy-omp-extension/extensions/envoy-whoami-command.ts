import { copyToClipboard } from "@oh-my-pi/pi-coding-agent";

type EnvoyIdentity = {
  readonly sessionID: string;
  readonly machineID: string;
  readonly directory: string;
};

type CommandContext = {
  readonly ui: { readonly notify: (message: string, level: "info" | "warning") => void };
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
  currentIdentity: () => EnvoyIdentity
): void {
  api.registerCommand("whoami", {
    description: "Copy session ID",
    handler: async (_args, context) => {
      const identity = currentIdentity();
      if (identity.sessionID === "") {
        context.ui.notify("No active session", "warning");
        return;
      }
      const copied = await copySessionID(copyToClipboard, identity.sessionID);
      context.ui.notify(
        copied ? `Session ID copied: ${identity.sessionID}` : `Could not copy session ID: ${identity.sessionID}`,
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

