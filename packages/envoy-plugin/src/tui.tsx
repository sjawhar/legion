/** @jsxImportSource @opentui/solid */
import type {
  TuiPlugin,
  TuiPluginApi,
  TuiPluginModule,
  TuiSlotPlugin,
} from "@opencode-ai/plugin/tui";
import { createSignal } from "solid-js";
import { copyOsc52 } from "./clipboard";
import { parsePort } from "./tui-port";

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

function ClickableRow(props: { text: string; onCopy: () => void }) {
  const [hover, setHover] = createSignal(false);
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: OpenTUI box supports mouse events.
    // biome-ignore lint/a11y/useKeyWithMouseEvents: This row is mouse-only in the TUI sidebar.
    <box
      onMouseOver={() => setHover(true)}
      onMouseOut={() => setHover(false)}
      onMouseUp={() => props.onCopy()}
    >
      <text fg={hover() ? undefined : "gray"}>{props.text}</text>
    </box>
  );
}

const tui: TuiPlugin = async (api) => {
  // Slash command
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

  // Sidebar: clickable session ID row
  const slot: TuiSlotPlugin = {
    order: 10,
    slots: {
      sidebar_content(_ctx, value) {
        if (!value.session_id) return null;
        const port = parsePort(
          (
            api.client as TuiPluginApi["client"] & {
              getConfig(): { baseUrl?: string };
            }
          ).getConfig().baseUrl
        );
        return (
          <box flexDirection="column" paddingTop={1}>
            <ClickableRow
              text={value.session_id}
              onCopy={() => copyWithToast(api, value.session_id, "Session ID copied")}
            />
            {port !== null ? (
              <ClickableRow
                text={`port ${port}`}
                onCopy={() => copyWithToast(api, String(port), "Port copied")}
              />
            ) : null}
          </box>
        );
      },
    },
  };
  api.slots.register(slot);
};

const plugin: TuiPluginModule = {
  id: "envoy-tui",
  tui,
};

export default plugin;
