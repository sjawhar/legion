import {
  ComposerPrimitive,
  MessagePrimitive,
  type ReasoningMessagePartComponent,
  ThreadPrimitive,
  type ToolCallMessagePartComponent,
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
  return (
    <div
      className={`mt-4 text-sm whitespace-pre-wrap ${textPrimaryOnCanvas}`}
      data-testid="agent-message-assistant"
    >
      <MessagePrimitive.Parts components={{ Reasoning, tools: { Fallback: ToolCall } }} />
    </div>
  );
}

export function AgentThread({ placeholder }: { placeholder: string }): ReactNode {
  return (
    <ThreadPrimitive.Root className="flex min-h-0 flex-1 flex-col">
      <ThreadPrimitive.Viewport
        autoScroll
        className="min-h-0 flex-1 overflow-y-auto pr-1"
        data-testid="agent-thread"
      >
        <ThreadPrimitive.Empty>
          <p className={`mt-6 text-sm ${textMutedOnCanvas}`}>
            Nothing yet. This session's next turn appears here as it happens.
          </p>
        </ThreadPrimitive.Empty>
        <ThreadPrimitive.Messages components={{ AssistantMessage, UserMessage }} />
      </ThreadPrimitive.Viewport>
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
