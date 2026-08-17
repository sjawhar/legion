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
  api.registerCommand("envoy_whoami", {
    description: "Show this session's Envoy identity and copy its session ID to the clipboard.",
    handler: async (_args, context) => {
      const identity = currentIdentity();
      const copied = await copySessionID(copyToClipboard, identity.sessionID);
      context.ui.notify(formatIdentity(identity, copied), copied ? "info" : "warning");
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

function formatIdentity(identity: EnvoyIdentity, copied: boolean): string {
  const clipboard = copied
    ? "Copied session ID to clipboard."
    : "Could not copy session ID to clipboard.";
  return `${JSON.stringify(
    { session_id: identity.sessionID, machine_id: identity.machineID, dir: identity.directory },
    null,
    2
  )}\n\n${clipboard}`;
}
