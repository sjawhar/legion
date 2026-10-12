import {
  ComposerPrimitive,
  MessagePrimitive,
  type ReasoningMessagePartComponent,
  type TextMessagePartComponent,
  ThreadPrimitive,
  type ToolCallMessagePartComponent,
  type Unstable_DirectiveFormatter,
  type Unstable_TriggerItem,
  type Unstable_TriggerMatcher,
  unstable_useTriggerPopoverScopeContext,
  useAuiState,
} from "@assistant-ui/react";
import type { AgentStreamCommand } from "@legion/contracts";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";

import { ChevronIcon } from "../../components/DisclosureToggle";
import { TruncatedText } from "../../components/TruncatedText";
import {
  borderDefault,
  calloutDangerBorder,
  card,
  cardHoverBorder,
  dangerText,
  disclosureButtonText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  selectedCardBg,
  selectedCardBorder,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textOptionDescription,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";
import { ErrorBoundary } from "../shell/ErrorBoundary";
import { readDispatchMarks } from "./dispatch-marks";
import { useFrameCoalesced } from "./useFrameCoalesced";

/**
 * The session's conversation, rendered with assistant-ui's primitives (MIT) over Dispatch's own
 * theme classes rather than the library's stock styles, so it reads as part of the dashboard.
 * The runtime is supplied by the page above; this file is presentation only.
 */

const preClasses = `mt-2 max-h-64 overflow-auto rounded-md p-2 text-xs whitespace-pre-wrap ${surfaceMutedBg} ${textMutedOnSurfaceMuted}`;

/** A tool call and, once it lands, its result. Folded by default: tool traffic is the bulk of a
 *  session's output and a viewer is usually reading the prose around it. */
const ToolCall: ToolCallMessagePartComponent = ({ toolName, argsText, result, isError }) => {
  const [expanded, setExpanded] = useState(false);
  const failed = isError === true;
  const output = typeof result === "string" ? result : result === undefined ? "" : String(result);
  return (
    <div
      className={`my-1 rounded-lg border px-3 py-1 ${card} ${failed ? calloutDangerBorder : borderDefault}`}
      data-testid="agent-tool-call"
    >
      <button
        aria-expanded={expanded}
        className={`flex min-h-8 w-full items-center gap-2 text-left text-xs ${textSecondaryOnCanvas}`}
        onClick={() => setExpanded((open) => !open)}
        type="button"
      >
        <span className="font-semibold">{toolName}</span>
        {failed ? <span className={dangerText}>failed</span> : null}
        {result === undefined ? (
          <span className={textMutedOnCanvas}>running…</span>
        ) : (
          <span className={textMutedOnCanvas}>done</span>
        )}
        <span className={`ml-auto ${disclosureButtonText}`}>
          <ChevronIcon expanded={expanded} />
        </span>
      </button>
      {expanded ? (
        <div className="pb-2">
          <pre className={preClasses}>{argsText === "" ? "(no arguments)" : argsText}</pre>
          {output === "" ? null : (
            <pre className={preClasses} data-testid="agent-tool-result">
              {output}
            </pre>
          )}
        </div>
      ) : null}
    </div>
  );
};

/** A text part of a turn the session wrote, rendered as Markdown - re-parsed as the streamed
 *  text grows, so a heading or a list formats the moment its syntax closes; until then the
 *  partial syntax reads as the literal characters, at the surrounding text's size. A single
 *  newline is a line break here (`softBreaks="line"`): a model's prose puts its lines on lines
 *  rather than hard-wrapping a paragraph, and the document reading (one paragraph, joined by
 *  spaces) made `First, check the config.\nThen, verify the credentials.` one run-on line. Each
 *  re-parse reads the whole turn so far (about 3.6 ms per KB), so the text is read once per
 *  animation frame (`useFrameCoalesced`) rather than once per delta: the publisher sends at most
 *  ten snapshots a second, and a frame is the most anyone can see anyway. A module constant, as
 *  `Reasoning` is: a component minted per render would remount every part. */
const MarkdownText: TextMessagePartComponent = ({ text }) => (
  <MarkdownBody markdown={useFrameCoalesced(text)} softBreaks="line" />
);

const Reasoning: ReasoningMessagePartComponent = ({ text }) => (
  <div className={`my-1 text-xs italic ${textMutedOnCanvas}`} data-testid="agent-reasoning">
    <MarkdownBody markdown={useFrameCoalesced(text)} softBreaks="line" />
  </div>
);

function UserMessage(): ReactNode {
  // A message someone other than the viewer sent the session, stored or taken as its own turn
  // (AgentRuntimeThread names them): it sits on the session's side of the thread with its author,
  // never styled as the viewer's own. Both are text a person or an agent wrote (a broadcast, a
  // targeted message, a reply), so both render as Markdown, as the session's own turns do.
  const author = useAuiState((state) => readDispatchMarks(state.message.metadata.custom).author);
  if (author !== undefined) {
    return (
      <div
        className={`mt-4 max-w-[80%] rounded-xl border px-3 py-2 text-sm ${borderDefault} ${textPrimaryOnCanvas}`}
        data-testid="agent-message-other"
      >
        <p className={`mb-1 text-xs font-semibold ${textSecondaryOnCanvas}`}>{author}</p>
        <MessagePrimitive.Parts components={{ Text: MarkdownText }} />
      </div>
    );
  }
  return (
    <div className="mt-4 flex justify-end" data-testid="agent-message-user">
      <div
        className={`max-w-[80%] rounded-xl px-3 py-2 text-sm ${surfaceMutedBg} ${textPrimaryOnCanvas}`}
      >
        <MessagePrimitive.Parts components={{ Text: MarkdownText }} />
      </div>
    </div>
  );
}

function AssistantMessage(): ReactNode {
  // Two selectors, not one combined object read: `useAuiState` compares a selector's return by
  // `Object.is` and documents that returning a new object literal (as reading both marks into one
  // object here would) re-renders on every store update rather than only when the selected slice
  // changes (@assistant-ui/store's useAuiState.d.ts).
  // A reply the session sent through Dispatch (AgentRuntimeThread marks it), not a streamed turn.
  const fromDispatch = useAuiState(
    (state) => readDispatchMarks(state.message.metadata.custom).dispatch === true
  );
  // The provider/model that produced this streamed turn (LEGION-548): absent for a reply sent
  // through Dispatch, and for a turn whose publishing host reported none.
  const model = useAuiState((state) => readDispatchMarks(state.message.metadata.custom).model);
  if (fromDispatch) {
    return (
      <div
        className={`mt-4 rounded-xl border px-3 py-2 text-sm ${card} ${borderDefault} ${textPrimaryOnCanvas}`}
        data-testid="agent-dispatch-reply"
      >
        <p className={`mb-1 text-xs font-semibold ${textSecondaryOnCanvas}`}>Reply via Dispatch</p>
        <MessagePrimitive.Parts components={{ Text: MarkdownText }} />
      </div>
    );
  }
  return (
    <div className={`mt-4 text-sm ${textPrimaryOnCanvas}`} data-testid="agent-message-assistant">
      {model === undefined ? null : (
        <p
          className={`mb-1 truncate text-xs ${textMutedOnCanvas}`}
          data-testid="agent-message-model"
          title={model}
        >
          <TruncatedText>{model}</TruncatedText>
        </p>
      )}
      <MessagePrimitive.Parts
        components={{ Reasoning, Text: MarkdownText, tools: { Fallback: ToolCall } }}
      />
    </div>
  );
}

/** The most commands the list shows at once: a session with every skill installed lists
 *  hundreds, and the person narrows them by typing. */
const SHOWN_COMMANDS = 50;

/**
 * Where a slash command is being typed: only a `/` that starts the message, since the session runs
 * the text as a command only from the start of its input, as its terminal does, and only while the
 * caret is still in the command's name. The replaced span is the whole name, so a pick made with
 * the caret inside a name replaces all of it.
 */
const atMessageStart: Unstable_TriggerMatcher = (text, trigger, cursor) => {
  if (!text.startsWith(trigger)) return null;
  const query = text.slice(trigger.length, cursor);
  if (/\s/u.test(query)) return null;
  const nameEnd = text.slice(trigger.length).search(/\s/u);
  return {
    endOffset: nameEnd === -1 ? text.length : trigger.length + nameEnd,
    offset: 0,
    query,
  };
};

/**
 * A picked command goes into the message as the person would type it at the session's terminal:
 * `/name`, and the popover adds the space after it. assistant-ui's own formatter writes a
 * `:type[label]` directive, which the session would receive as that markup rather than as its
 * command. `parse` serves only a rich-text input, which this composer is not.
 */
const slashFormatter: Unstable_DirectiveFormatter = {
  parse: (text) => [{ kind: "text", text }],
  serialize: (item) => `/${item.id}`,
};

/**
 * The commands that match what the person typed after the slash, at most `SHOWN_COMMANDS`: every
 * command the session can run before the ones only its terminal runs (listed so the person learns
 * why `/new` does nothing here), and within each, names that start with what was typed before
 * names that merely hold it, otherwise in the session's own order.
 */
function matchingCommands(
  commands: readonly AgentStreamCommand[],
  query: string
): Unstable_TriggerItem[] {
  const typed = query.toLowerCase();
  const seen = new Set<string>();
  const ranked: { command: AgentStreamCommand; rank: number }[] = [];
  for (const command of commands) {
    // A name listed twice would be two rows with one id, which the popover cannot tell apart.
    if (seen.has(command.name)) continue;
    seen.add(command.name);
    const name = command.name.toLowerCase();
    if (!name.includes(typed)) continue;
    const rank = (command.terminalOnly === true ? 2 : 0) + (name.startsWith(typed) ? 0 : 1);
    ranked.push({ command, rank });
  }
  // `sort` is stable, so equal ranks keep the session's order.
  return ranked
    .sort((left, right) => left.rank - right.rank)
    .slice(0, SHOWN_COMMANDS)
    .map(({ command }) => ({
      description: command.description,
      id: command.name,
      label: `/${command.name}`,
      metadata: { terminalOnly: command.terminalOnly === true },
      type: command.source,
    }));
}

/** One command in the list: its name, a muted `terminal only` where the session cannot run it,
 *  and its description. The highlighted row stays in view as the arrows move through a list
 *  taller than the popover. */
function CommandItem({ index, item }: { index: number; item: Unstable_TriggerItem }): ReactNode {
  const highlighted = unstable_useTriggerPopoverScopeContext().highlightedIndex === index;
  const row = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (highlighted) row.current?.scrollIntoView({ block: "nearest" });
  }, [highlighted]);
  return (
    // Below `xl` every button is `inline-flex` with centred items (styles.css's touch rule), so
    // the two lines stack explicitly and each stretches to the row's width.
    <ComposerPrimitive.Unstable_TriggerPopoverItem
      className={`flex min-h-11 w-full flex-col rounded-lg border px-3 py-2 text-left text-sm ${cardHoverBorder} ${
        highlighted ? `${selectedCardBg} ${selectedCardBorder}` : card
      }`}
      index={index}
      item={item}
      // Keeps the caret in the message, so the person types on after picking.
      onMouseDown={(event) => event.preventDefault()}
      ref={row}
    >
      <span className="flex items-baseline gap-2 self-stretch">
        <span className={`font-medium ${textPrimaryOnSurface}`}>{item.label}</span>
        {item.metadata?.terminalOnly === true ? (
          <span className={`text-xs ${textOptionDescription}`}>terminal only</span>
        ) : null}
      </span>
      {item.description === undefined ? null : (
        <TruncatedText
          className={`self-stretch text-xs ${textOptionDescription}`}
          title={item.description}
        >
          {item.description}
        </TruncatedText>
      )}
    </ComposerPrimitive.Unstable_TriggerPopoverItem>
  );
}

/** The session's slash commands, offered while the message starts with `/`. A pick writes
 *  `/name ` into the message; sending it is the composer's ordinary Send. */
function SlashCommands({ commands }: { commands: readonly AgentStreamCommand[] }): ReactNode {
  const adapter = useMemo(
    () => ({
      categories: () => [],
      categoryItems: () => [],
      search: (query: string) => matchingCommands(commands, query),
    }),
    [commands]
  );
  return (
    <ComposerPrimitive.Unstable_TriggerPopover
      adapter={adapter}
      aria-label="Slash commands"
      char="/"
      className={`absolute right-0 bottom-full left-0 z-10 mb-2 flex max-h-72 flex-col gap-1 overflow-y-auto rounded-lg border p-1 shadow-lg ${card}`}
      matcher={atMessageStart}
    >
      <ComposerPrimitive.Unstable_TriggerPopover.Directive formatter={slashFormatter} />
      <ComposerPrimitive.Unstable_TriggerPopoverItems className="contents">
        {(items) =>
          items.map((item, index) => <CommandItem index={index} item={item} key={item.id} />)
        }
      </ComposerPrimitive.Unstable_TriggerPopoverItems>
    </ComposerPrimitive.Unstable_TriggerPopover>
  );
}

/**
 * `resetKey` clears a caught error when the viewer moves to another session.
 *
 * The boundary is around the transcript alone, and deliberately not around the composer. A frame
 * this build renders wrongly throws inside assistant-ui's own conversion, and a boundary that
 * enclosed both would take the composer with it — leaving a viewer looking at an error with no
 * way to talk to the session, which is worse than the bad frame. The session's ring re-serves
 * that frame on every visit, so "worse" here means permanently.
 */
export function AgentThread({
  commands,
  empty,
  placeholder,
  resetKey,
}: {
  /** The slash commands the composer completes; none is offered while this is undefined or
   *  empty. */
  commands: readonly AgentStreamCommand[] | undefined;
  empty: string;
  placeholder: string;
  resetKey: string;
}): ReactNode {
  return (
    <ThreadPrimitive.Root className="flex min-h-0 flex-1 flex-col">
      <ErrorBoundary region="this conversation" resetKey={resetKey}>
        {/* `mt-3`: the scroller clips a turn at its top edge, and without a gap that clipped
            line sat against the page's note above it and read as overlapping text. `pl-1`
            matches `pr-1` for the same reason on the left: a turn's numbered list hangs its
            marker outside the list's content box, and the scroller clipped its first digit. */}
        <ThreadPrimitive.Viewport
          autoScroll
          className="mt-3 min-h-0 flex-1 overflow-y-auto pr-1 pl-1"
          data-testid="agent-thread"
        >
          <ThreadPrimitive.Empty>
            {/* Why there is nothing here is the page's to say: a session that cannot stream never
                fills this space, and "the next turn appears here" would be a promise. */}
            <p className={`mt-6 text-sm ${textMutedOnCanvas}`} data-testid="agent-thread-empty">
              {empty}
            </p>
          </ThreadPrimitive.Empty>
          <ThreadPrimitive.Messages components={{ AssistantMessage, UserMessage }} />
        </ThreadPrimitive.Viewport>
      </ErrorBoundary>
      {/* The trigger root always wraps the composer, so commands arriving or going never
          remounts the input under the person's caret. */}
      <ComposerPrimitive.Unstable_TriggerPopoverRoot>
        <ComposerPrimitive.Root
          className={`relative mt-3 flex items-end gap-2 border-t pt-3 ${borderDefault}`}
          data-testid="agent-composer"
        >
          {commands === undefined || commands.length === 0 ? null : (
            <SlashCommands commands={commands} />
          )}
          <ComposerPrimitive.Input
            className={`min-h-11 flex-1 rounded-lg px-3 py-2 text-sm ${inputClasses(false)}`}
            placeholder={placeholder}
            rows={1}
          />
          <ComposerPrimitive.Send
            className={`min-h-11 rounded-lg px-4 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
          >
            Send
          </ComposerPrimitive.Send>
        </ComposerPrimitive.Root>
      </ComposerPrimitive.Unstable_TriggerPopoverRoot>
    </ThreadPrimitive.Root>
  );
}
