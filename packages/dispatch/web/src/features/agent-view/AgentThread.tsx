import {
  ComposerPrimitive,
  MessagePrimitive,
  type ReasoningMessagePartComponent,
  ThreadPrimitive,
  type ToolCallMessagePartComponent,
  useAuiState,
} from "@assistant-ui/react";
import { type ReactNode, useState } from "react";

import { ChevronIcon } from "../../components/DisclosureToggle";
import {
  borderDefault,
  calloutDangerBorder,
  card,
  dangerText,
  disclosureButtonText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ErrorBoundary } from "../shell/ErrorBoundary";

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

const Reasoning: ReasoningMessagePartComponent = ({ text }) => (
  <p className={`my-1 text-xs italic ${textMutedOnCanvas}`} data-testid="agent-reasoning">
    {text}
  </p>
);

function UserMessage(): ReactNode {
  // A stored message someone other than the viewer sent the session (AgentRuntimeThread names
  // them): it sits on the session's side of the thread with its author, never styled as the
  // viewer's own.
  const author = useAuiState((state) => state.message.metadata.custom.author);
  if (typeof author === "string") {
    return (
      <div
        className={`mt-4 max-w-[80%] rounded-xl border px-3 py-2 text-sm whitespace-pre-wrap ${borderDefault} ${textPrimaryOnCanvas}`}
        data-testid="agent-message-other"
      >
        <p className={`mb-1 text-xs font-semibold ${textSecondaryOnCanvas}`}>{author}</p>
        <MessagePrimitive.Parts />
      </div>
    );
  }
  return (
    <div className="mt-4 flex justify-end" data-testid="agent-message-user">
      <div
        className={`max-w-[80%] rounded-xl px-3 py-2 text-sm whitespace-pre-wrap ${surfaceMutedBg} ${textPrimaryOnCanvas}`}
      >
        <MessagePrimitive.Parts />
      </div>
    </div>
  );
}

function AssistantMessage(): ReactNode {
  // A reply the session sent through Dispatch (AgentRuntimeThread marks it), not a streamed turn.
  const fromDispatch = useAuiState((state) => state.message.metadata.custom.dispatch === true);
  if (fromDispatch) {
    return (
      <div
        className={`mt-4 rounded-xl border px-3 py-2 text-sm whitespace-pre-wrap ${card} ${borderDefault} ${textPrimaryOnCanvas}`}
        data-testid="agent-dispatch-reply"
      >
        <p className={`mb-1 text-xs font-semibold ${textSecondaryOnCanvas}`}>Reply via Dispatch</p>
        <MessagePrimitive.Parts />
      </div>
    );
  }
  return (
    <div
      className={`mt-4 text-sm whitespace-pre-wrap ${textPrimaryOnCanvas}`}
      data-testid="agent-message-assistant"
    >
      <MessagePrimitive.Parts components={{ Reasoning, tools: { Fallback: ToolCall } }} />
    </div>
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
  empty,
  placeholder,
  resetKey,
}: {
  empty: string;
  placeholder: string;
  resetKey: string;
}): ReactNode {
  return (
    <ThreadPrimitive.Root className="flex min-h-0 flex-1 flex-col">
      <ErrorBoundary region="this conversation" resetKey={resetKey}>
        <ThreadPrimitive.Viewport
          autoScroll
          className="min-h-0 flex-1 overflow-y-auto pr-1"
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
      <ComposerPrimitive.Root
        className={`mt-3 flex items-end gap-2 border-t pt-3 ${borderDefault}`}
        data-testid="agent-composer"
      >
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
    </ThreadPrimitive.Root>
  );
}
