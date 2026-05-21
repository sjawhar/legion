/** @jsxImportSource @opentui/solid */
import type { TuiPlugin, TuiPluginApi, TuiPluginModule } from "@opencode-ai/plugin/tui";
import { copyOsc52 } from "./clipboard";

function currentSessionID(api: TuiPluginApi): string | undefined {
  const route = api.route.current;
  if (route.name !== "session") return undefined;
  return (route.params as { sessionID?: string } | undefined)?.sessionID;
}

function copyWithToast(api: TuiPluginApi, text: string, successMessage: string) {
  if (copyOsc52(text)) {
    api.ui.toast({ message: successMessage, variant: "success" });
  } else {
    api.ui.toast({ message: `Failed: ${successMessage}`, variant: "error" });
  }
}

const tui: TuiPlugin = async (api) => {
  api.keymap.registerLayer({
    commands: [
      {
        name: "envoy.whoami.copy",
        title: "Copy session ID",
        category: "Envoy",
        namespace: "palette",
        slashName: "whoami",
        run() {
          const sessionID = currentSessionID(api);
          if (!sessionID) {
            api.ui.toast({ message: "No active session", variant: "warning" });
            return;
          }
          copyWithToast(api, sessionID, "Session ID copied");
        },
      },
    ],
  });
};

const plugin: TuiPluginModule = {
  id: "envoy-tui",
  tui,
};

export default plugin;
