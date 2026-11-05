import type { ReactNode } from "react";

import {
  borderDefault,
  surfaceMutedBg,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../../theme/classes";
import type { Conversation } from "../conversation/conversation-model";
import { pageChrome } from "./page-structure";
import {
  CONTROL_LABELS,
  CONVERSATION_STATE_NAMES,
  CONVERSATION_STATE_ORDER,
  type ConversationState,
} from "./page-words";
import { isPinnedConversation } from "./pins";

/**
 * The conversation's four state controls, inside the one right-hand panel
 * (dispatch://INFRA-1196 R12): everything the issue recorded, what the reader pinned, the
 * questions alone, and what people and agents said.
 *
 * They are options of one listbox rather than tabs, because the panel is not a set of panes:
 * one state is picked, and the conversation the page draws is in that state. `/conversation`
 * and `/activity` are the addresses of two of them, so picking a state is navigation
 * (`IssuePage`'s `pickState`), and the address a reader arrives at says which one is selected.
 */
export function ConversationStates({
  onPick,
  state,
}: {
  onPick: (state: ConversationState) => void;
  state: ConversationState;
}): ReactNode {
  return (
    <section className={`border-b pb-2 ${borderDefault}`}>
      <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`} {...pageChrome}>
        {CONTROL_LABELS.conversation}
      </h2>
      <div
        aria-label={CONTROL_LABELS.conversation}
        className={`mt-1 flex flex-wrap rounded-md border ${borderDefault}`}
        role="listbox"
      >
        {CONVERSATION_STATE_ORDER.map((candidate) => (
          <button
            aria-selected={candidate === state}
            className={`min-h-11 px-2 text-xs font-medium md:min-h-9 ${
              candidate === state
                ? `${surfaceMutedBg} ${textPrimaryOnSurfaceMuted}`
                : textSecondaryOnSurface
            }`}
            data-conversation-state={candidate}
            key={candidate}
            onClick={() => onPick(candidate)}
            role="option"
            type="button"
            {...pageChrome}
          >
            {CONVERSATION_STATE_NAMES[candidate]}
          </button>
        ))}
      </div>
    </section>
  );
}

/**
 * Which conversations `state` keeps, or undefined for a state that keeps them all. `everything`
 * keeps them all too: that state draws the issue's whole event history beside them, which is
 * the Activity list, and the conversation under it is not narrowed.
 */
export function narrowConversation(
  state: ConversationState,
  dismissed: readonly string[]
): ((conversation: Conversation) => boolean) | undefined {
  if (state === "questions") return (conversation) => conversation.kind === "ask";
  if (state === "pinned") {
    return (conversation) => isPinnedConversation([...dismissed], conversation.id);
  }
  return undefined;
}
