/** @jsxImportSource @opentui/solid */
import type {
  TuiPlugin,
  TuiPluginApi,
  TuiPluginModule,
  TuiSlotPlugin,
} from "@opencode-ai/plugin/tui";
import { createSignal } from "solid-js";
import { copyOsc52 } from "./clipboard";
import { resolveTuiPort } from "./tui-port";

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

type ClientWithConfig = TuiPluginApi["client"] & {
  client: { getConfig(): { baseUrl?: string } };
};

function baseUrl(api: TuiPluginApi): string | undefined {
  const url = (api.client as ClientWithConfig).client.getConfig().baseUrl;
  return url;
}

function ClickableRow(props: {
  text: string;
  onCopy: () => void;
  mutedColor: TuiPluginApi["theme"]["current"]["textMuted"];
  textColor: TuiPluginApi["theme"]["current"]["text"];
}) {
  const [hover, setHover] = createSignal(false);
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: OpenTUI box supports mouse events.
    // biome-ignore lint/a11y/useKeyWithMouseEvents: This row is mouse-only in the TUI sidebar.
    <box
      flexDirection="row"
      onMouseOver={() => setHover(true)}
      onMouseOut={() => setHover(false)}
      onMouseDown={() => props.onCopy()}
    >
      <text fg={hover() ? props.textColor : props.mutedColor} wrapMode="none">
        {props.text}
      </text>
    </box>
  );
}

function EnvoySidebar(props: { api: TuiPluginApi; sessionID: string }) {
  const theme = () => props.api.theme.current;
  const port = resolveTuiPort(baseUrl(props.api), props.sessionID);

  return (
    <box>
      <ClickableRow
        text={props.sessionID}
        mutedColor={theme().textMuted}
        textColor={theme().text}
        onCopy={() => copyWithToast(props.api, props.sessionID, "Session ID copied")}
      />
      {port !== null ? (
        <ClickableRow
          text={`port ${port}`}
          mutedColor={theme().textMuted}
          textColor={theme().text}
          onCopy={() => copyWithToast(props.api, String(port), "Port copied")}
        />
      ) : null}
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
        return <EnvoySidebar api={api} sessionID={value.session_id} />;
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
