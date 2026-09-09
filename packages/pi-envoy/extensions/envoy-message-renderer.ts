import { Box, Spacer, Text } from "@oh-my-pi/pi-tui";
import type { MessageRenderOptions, MessageRendererTheme, PiApi, RenderableMessage } from "../src/pi-types";

/**
 * Lines of the inbound body shown before the "…" fold when collapsed. Kept
 * small since these cards accumulate in a live transcript.
 */
const ENVOY_MESSAGE_COLLAPSED_LINES = 12;

/**
 * Renders an inbound `envoy-message` as a plain-text card instead of OMP's
 * default Markdown body. `renderInbound` (`@legion/envoy-client/delivery`)
 * nests a structured `message` value under a `message:` TOON key at 4-space
 * indent, which CommonMark reads as an indented code block — Markdown then
 * mangles or mis-styles every delivery. Displaying the content through
 * `Text` (not `Markdown`) keeps it verbatim.
 */
function renderEnvoyMessage(
  message: RenderableMessage,
  options: MessageRenderOptions,
  theme: MessageRendererTheme
): Box {
  const box = new Box(1, 1, undefined, {
    chars: theme.boxRound,
    color: (text) => theme.fg("borderMuted", text),
  });
  box.setIgnoreTight(true);
  box.addChild(new Text(theme.fg("customMessageLabel", theme.bold(message.customType)), 0, 0));
  box.addChild(new Spacer(1));

  let text = message.content;
  if (!options.expanded) {
    const lines = text.split("\n");
    if (lines.length > ENVOY_MESSAGE_COLLAPSED_LINES) {
      text = `${lines.slice(0, ENVOY_MESSAGE_COLLAPSED_LINES).join("\n")}\n…`;
    }
  }
  box.addChild(new Text(theme.fg("customMessageText", text), 0, 0));

  return box;
}

export function registerEnvoyMessageRenderer(pi: Pick<PiApi, "registerMessageRenderer">): void {
  pi.registerMessageRenderer("envoy-message", renderEnvoyMessage);
}
