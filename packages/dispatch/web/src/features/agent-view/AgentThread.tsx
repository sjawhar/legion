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
import { type ReactNode, type RefObject, useEffect, useId, useMemo, useRef, useState } from "react";

import { ChevronIcon } from "../../components/DisclosureToggle";
import { RefusableButton } from "../../components/RefusableButton";
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
  primaryButtonHoverBg,
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
import { PendingSendTurn } from "../conversation/PendingSendTurn";
import type { SessionSends } from "../conversation/useSessionSends";
import { AgentName } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { ErrorBoundary } from "../shell/ErrorBoundary";
import { readDispatchMarks, turnKey } from "./dispatch-marks";
import type { LivePlace } from "./live-views";
import { useLiveFollow } from "./use-live-follow";
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

/**
 * The key this turn keeps across a rebuild of the transcript, which every rendered turn carries in
 * `data-message-id` so `useLiveFollow` can find the turn a reader was on. One selector returning
 * the string, for the `Object.is` reason `AssistantMessage` documents below.
 */
function useTurnKey(): string {
  return useAuiState((state) => turnKey(state.message.metadata.custom, state.message.id));
}

function UserMessage(): ReactNode {
  // A message someone other than the viewer sent the session, stored or taken as its own turn
  // (AgentRuntimeThread names them): it sits on the session's side of the thread with its author,
  // never styled as the viewer's own. Both are text a person or an agent wrote (a broadcast, a
  // targeted message, a reply), so both render as Markdown, as the session's own turns do.
  // Two selectors, for the reason `AssistantMessage` gives: the `author` mark is a new object on
  // every rebuild of the transcript (each streamed frame), but its `actor` is the stored message's
  // own author, which keeps its identity across frames, and its `issue` is a string.
  const actor = useAuiState(
    (state) => readDispatchMarks(state.message.metadata.custom).author?.actor
  );
  const issue = useAuiState(
    (state) => readDispatchMarks(state.message.metadata.custom).author?.issue
  );
  const turn = useTurnKey();
  if (actor !== undefined) {
    return (
      <div
        className={`mt-4 max-w-[80%] rounded-xl border px-3 py-2 text-sm ${borderDefault} ${textPrimaryOnCanvas}`}
        data-message-id={turn}
        data-testid="agent-message-other"
      >
        <p className={`mb-1 text-xs font-semibold ${textSecondaryOnCanvas}`}>
          <AgentName actor={actor} inline />
          {issue === undefined ? null : ` · ${issue}`}
        </p>
        <MessagePrimitive.Parts components={{ Text: MarkdownText }} />
      </div>
    );
  }
  return (
    <div className="mt-4 flex justify-end" data-message-id={turn} data-testid="agent-message-user">
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
  const turn = useTurnKey();
  if (fromDispatch) {
    return (
      <div
        className={`mt-4 rounded-xl border px-3 py-2 text-sm ${card} ${borderDefault} ${textPrimaryOnCanvas}`}
        data-message-id={turn}
        data-testid="agent-dispatch-reply"
      >
        <p className={`mb-1 text-xs font-semibold ${textSecondaryOnCanvas}`}>Reply via Dispatch</p>
        <MessagePrimitive.Parts components={{ Text: MarkdownText }} />
      </div>
    );
  }
  return (
    <div
      className={`mt-4 text-sm ${textPrimaryOnCanvas}`}
      data-message-id={turn}
      data-testid="agent-message-assistant"
    >
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

/** How the session's publisher names a skill: `skill:<name>` (`AgentStreamCommand` in
 *  `packages/contracts/src/agent-stream.ts`; the plugin passes its host's picker names through).
 *  Anchored, so only the prefix goes and a name holding `skill:` later keeps it. */
const SKILL_PREFIX = /^skill:/u;

/**
 * Where a slash command is being typed: the `/` token the caret sits in, whose `/` starts the
 * message or follows a space or a line break. A `/` inside a word - a path, a date - belongs to
 * that word and opens nothing. The replaced span is the whole token, so a pick made with the
 * caret inside a name replaces all of it.
 *
 * It also records where that `/` is, which is what the formatter below needs and the library
 * never hands it: `serialize(item)` takes the item alone. The matcher runs on every keystroke
 * and on every move of the caret, so the offset it leaves is the open popover's own.
 */
function slashAtCaret(triggerOffset: RefObject<number>): Unstable_TriggerMatcher {
  return (text, trigger, cursor) => {
    // The token the caret sits in: back to the space or line break before it, or to the start.
    const offset = text.slice(0, cursor).search(/\S*$/u);
    if (!text.startsWith(trigger, offset)) return null;
    const tokenEnd = text.slice(cursor).search(/\s/u);
    triggerOffset.current = offset;
    return {
      endOffset: tokenEnd === -1 ? text.length : cursor + tokenEnd,
      offset,
      query: text.slice(offset + trigger.length, cursor),
    };
  };
}

/**
 * What a picked command puts in the message. A pick that starts the message is a command line:
 * the session runs it as the person would have typed it at its terminal, `/skill:asd-ste100`,
 * and the popover adds the space after it. A pick inside a sentence is the person naming the
 * skill to the session - nothing there is run - so it writes the bare name, `asd-ste100`,
 * without the slash and without the publisher's `skill:` prefix.
 *
 * assistant-ui's own formatter writes a `:type[label]` directive, which the session would
 * receive as that markup rather than as its command. `parse` serves only a rich-text input,
 * which this composer is not.
 */
function slashFormatter(triggerOffset: RefObject<number>): Unstable_DirectiveFormatter {
  return {
    parse: (text) => [{ kind: "text", text }],
    serialize: (item) =>
      triggerOffset.current === 0 ? `/${item.id}` : item.id.replace(SKILL_PREFIX, ""),
  };
}

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
        // Whole, on as many lines as it takes (INFRA-812, R5): a row of this list is read to
        // learn what the command does, and an ellipsis there hides the half that says it.
        <span className={`self-stretch text-xs ${textOptionDescription}`}>{item.description}</span>
      )}
    </ComposerPrimitive.Unstable_TriggerPopoverItem>
  );
}

/** The session's slash commands, offered for the `/` token the caret sits in. A pick writes the
 *  command at the start of a message and the bare name inside a sentence (`slashFormatter`);
 *  sending it is the composer's ordinary Send. */
function SlashCommands({ commands }: { commands: readonly AgentStreamCommand[] }): ReactNode {
  // Where the open popover's `/` sits, written by the matcher and read by the formatter, which
  // the library gives nothing else to tell a command line from a mention by.
  const triggerOffset = useRef(0);
  const matcher = useMemo(() => slashAtCaret(triggerOffset), []);
  const formatter = useMemo(() => slashFormatter(triggerOffset), []);
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
      matcher={matcher}
    >
      <ComposerPrimitive.Unstable_TriggerPopover.Directive formatter={formatter} />
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
  contentIds,
  empty,
  held,
  initialPlace,
  onPlace,
  ownSendCount,
  placeholder,
  replayCount,
  resetKey,
  sends,
  sessionId,
}: {
  /** The slash commands the composer completes; none is offered while this is undefined or
   *  empty. */
  commands: readonly AgentStreamCommand[] | undefined;
  /** Every turn of the transcript, in the order it is rendered, by the key each one carries in
   *  `data-message-id`: what the follow hook counts as new and finds the reader's place in. */
  contentIds: readonly string[];
  empty: string;
  /** Whether a model or thinking switch is pending on this session: Send is disabled, still
   *  reading "Send", until it ends, and `placeholder` is what says why. */
  held: boolean;
  /** Where this session's reader was when its view last reported, which the view's entry in
   *  `live-views.ts` holds while the view sleeps, or `null` for one that was following. */
  initialPlace: LivePlace | null;
  /** Reports the place the transcript holds, for that entry to keep. */
  onPlace: (place: LivePlace | null) => void;
  /** Bumped once per message this viewer sent that the session accepted. */
  ownSendCount: number;
  placeholder: string;
  /** Bumped once per rebuild of the whole transcript from the session's replay. */
  replayCount: number;
  resetKey: string;
  /** Every message to the session Dispatch has not yet made, in press order, each a turn of its
   *  own at the transcript's end, and what acts on them. */
  sends: SessionSends;
  /** The session being read: moving to another one lands where that session was left, or at its
   *  live end when nothing was kept for it. */
  sessionId: string;
}): ReactNode {
  // The element itself, not a ref held once: this viewport is inside the boundary below, which
  // renders a fresh node once a reset clears the error it was holding, and the hook binds its
  // listener and its observers to whichever node it is given.
  const [viewport, setViewport] = useState<HTMLDivElement | null>(null);
  const { pending } = sends;
  const follow = useLiveFollow({
    contentIds: useMemo(
      () => [...contentIds, ...pending.map((send) => `pending:${send.id}`)],
      [contentIds, pending]
    ),
    initialPlace,
    onPlace,
    ownSendCount,
    replayCount,
    sessionId,
    viewport,
  });
  // An empty box is the one thing that refuses Send (INFRA-442): a message still out, late or
  // refused is a turn of its own, and the next message goes out behind it. A pending switch
  // disables Send and names why instead. A turn still streaming does not refuse: the session
  // decides what a message does mid-turn by its delivery mode, as at its terminal.
  const boxEmpty = useAuiState((state) => state.composer.isEmpty);
  const refusal = boxEmpty && !held ? "Type a message first." : null;
  // The line under the box reads nothing until a person presses Send and the press is refused
  // (INFRA-812, R6): an empty box is where this view rests, and a reason printed there before
  // anybody tries is rule text on the page, which the question mark under the box now holds.
  // The reason reaches a screen reader all the same, on the button's own hidden line and its
  // title, and the next thing typed clears the press.
  const [refusedPress, setRefusedPress] = useState(false);
  const shownRefusal = refusedPress ? refusal : null;
  if (refusal === null && refusedPress) setRefusedPress(false);
  const hintId = useId();
  return (
    <ThreadPrimitive.Root className="flex min-h-0 flex-1 flex-col">
      <ErrorBoundary region="this conversation" resetKey={resetKey}>
        {/* `relative` for the control below, which is the viewport's sibling and out of flow, so
            it changes neither `scrollHeight` nor `scrollTop`, the composer does not move, and it
            can never cover the composer's controls. */}
        <div className="relative flex min-h-0 flex-1 flex-col">
          {/* `mt-3`: the scroller clips a turn at its top edge, and without a gap that clipped
              line sat against the page's note above it and read as overlapping text. `pl-1`
              matches `pr-1` for the same reason on the left: a turn's numbered list hangs its
              marker outside the list's content box, and the scroller clipped its first digit.

              All four of assistant-ui's auto-scroll options are off and `useLiveFollow` does
              every scroll this view makes: with them off the library plants no scroll intent and
              performs none. Its own `thread.runStart` handler scrolls to the bottom without
              asking where the reader is, which is this view's whole complaint; its scroll-up
              detector requires `scrollHeight` to be unchanged, so it never notices a reader who
              scrolls back while a turn streams; and the flag it would then keep is internal, so
              nothing outside the library can clear it.

              `[overflow-anchor:none]` (the sidebar's precedent in `app.tsx`) leaves the hook's
              own compensation the only one there is: the browser's anchoring would adjust a
              rebuild a frame after the hook already had, hiding that compensation and arriving
              as a scroll event nothing can tell from the reader's own. */}
          <ThreadPrimitive.Viewport
            autoScroll={false}
            className="mt-3 min-h-0 flex-1 overflow-y-auto pr-1 pl-1 [overflow-anchor:none]"
            data-testid="agent-thread"
            ref={setViewport}
            scrollToBottomOnInitialize={false}
            scrollToBottomOnRunStart={false}
            scrollToBottomOnThreadSwitch={false}
          >
            {/* One wrapper around everything the viewport scrolls: the hook's `ResizeObserver`
                measures the viewport's first element child as the transcript's height. */}
            <div>
              <ThreadPrimitive.Empty>
                {/* Why there is nothing here is the page's to say: a session that cannot stream
                    never fills this space, and "the next turn appears here" would be a
                    promise. */}
                <p className={`mt-6 text-sm ${textMutedOnCanvas}`} data-testid="agent-thread-empty">
                  {empty}
                </p>
              </ThreadPrimitive.Empty>
              <ThreadPrimitive.Messages components={{ AssistantMessage, UserMessage }} />
              {pending.map((send) => (
                <PendingSendTurn held={send} key={send.id} sends={sends} />
              ))}
            </div>
          </ThreadPrimitive.Viewport>
          {follow.atEnd ? null : (
            <button
              className={`absolute bottom-3 left-1/2 z-10 min-h-11 -translate-x-1/2 rounded-full px-4 text-sm font-semibold whitespace-nowrap shadow-lg ${primaryButtonBg} ${primaryButtonHoverBg}`}
              data-testid="jump-to-latest"
              onClick={follow.jumpToEnd}
              type="button"
            >
              Jump to latest{follow.newCount === 0 ? "" : ` · ${follow.newCount} new`}
            </button>
          )}
        </div>
      </ErrorBoundary>
      {/* The trigger root always wraps the composer, so commands arriving or going never
          remounts the input under the person's caret. The form stays the composer's one box,
          `relative`, so the command list is anchored to it and the whole composer, hint line
          included, is what a layout measures. */}
      <ComposerPrimitive.Unstable_TriggerPopoverRoot>
        <ComposerPrimitive.Root
          className={`relative mt-3 flex flex-col gap-1 border-t pt-3 ${borderDefault}`}
          data-testid="agent-composer"
        >
          {commands === undefined || commands.length === 0 ? null : (
            <SlashCommands commands={commands} />
          )}
          <div className="flex items-end gap-2">
            <ComposerPrimitive.Input
              className={`min-h-11 flex-1 rounded-lg px-3 py-2 text-sm ${inputClasses(false)}`}
              placeholder={placeholder}
              rows={1}
              submitMode="enter"
            />
            {/* A submit button, so a click and Enter both reach the form's submit, which the
                composer root turns into assistant-ui's own send: one way out for a message. */}
            <RefusableButton
              busy={held ? "Send" : undefined}
              className="min-h-11"
              describedBy={held ? hintId : undefined}
              onRefusedPress={() => setRefusedPress(true)}
              refusal={refusal}
              refusalShownBy={shownRefusal === null ? undefined : hintId}
              type="submit"
            >
              Send
            </RefusableButton>
          </div>
          {/* The Send control's own words, and nothing else: while a switch is pending this line
              says what the box's placeholder says, which the text typed may hide, and once a
              person has pressed Send with nothing typed it says why that press was refused. At
              rest it reads nothing - how to send is a rule of the page, which the question mark
              under the box now holds (INFRA-812, R6), not a word about this press. */}
          <p
            className={`text-xs ${textMutedOnCanvas}`}
            data-testid="agent-composer-hint"
            id={hintId}
          >
            {held ? placeholder : shownRefusal}
          </p>
        </ComposerPrimitive.Root>
      </ComposerPrimitive.Unstable_TriggerPopoverRoot>
    </ThreadPrimitive.Root>
  );
}
