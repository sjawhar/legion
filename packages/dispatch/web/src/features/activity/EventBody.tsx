import { actorActText } from "@legion/contracts";
import type { ReactNode } from "react";

import type { Event } from "../../api/types";
import { textMutedOnSurface, textPrimaryOnSurface } from "../../theme/classes";
import { AskOptionList } from "../inbox/AskOptionList";
import { eventDescription } from "../issue/event-description";
import { AgentParts, AskResolutionLine } from "../refs/AgentName";
import { askResolutionReason } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";

type AskEvent = Extract<Event, { type: "ask.opened" | "ask.answered" | "ask.resolved" }>;

export function isAskEvent(event: Event): event is AskEvent {
  return (
    event.type === "ask.opened" || event.type === "ask.answered" || event.type === "ask.resolved"
  );
}

function AskEventBody({
  event,
  onRendered,
}: {
  event: AskEvent;
  onRendered?: () => void;
}): ReactNode {
  const source = textSourceOf("ask", event.payload.id);
  const { answer, question, options, resolution, created_at } = event.payload;
  const resolutionReason = resolution === undefined ? undefined : askResolutionReason(resolution);
  // A changed answer names the one it replaced; the first answer's event carries none. An event
  // written before answers stored an empty list holds null for either's selected: no option.
  const previous = event.type === "ask.answered" ? event.payload.previous_answer : undefined;
  return (
    <>
      <p className={`text-sm font-medium ${textPrimaryOnSurface}`}>
        {event.type === "ask.opened" ? "Ask opened:" : "Ask:"}
      </p>
      <MarkdownBody markdown={question} onRendered={onRendered} source={source} />
      <AskOptionList options={options} selected={answer?.selected ?? []} />
      <p className={`mt-2 text-xs ${textMutedOnSurface}`}>
        Opened <Timestamp at={created_at} />
      </p>
      {answer === null ? null : (
        <>
          <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
            Answered by {actorActText(answer.user, answer.act)} <Timestamp at={answer.at} />
          </p>
          {answer.text === null || answer.text === "" ? null : (
            <div className="mt-2">
              <MarkdownBody markdown={answer.text} onRendered={onRendered} source={source} />
            </div>
          )}
          {previous === undefined ? null : (
            <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
              <s>
                was:{" "}
                {[(previous.selected ?? []).join(", "), previous.text ?? ""]
                  .filter((part) => part !== "")
                  .join(" - ")}
              </s>
            </p>
          )}
        </>
      )}
      {resolution === undefined ? null : (
        <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
          <AskResolutionLine resolution={resolution} />
          {resolutionReason === undefined ? null : (
            <>
              {" "}
              -{" "}
              <MarkdownBody
                markdown={resolutionReason}
                onRendered={onRendered}
                source={source}
                variant="inline"
              />
            </>
          )}{" "}
          <Timestamp at={resolution.at} />
        </p>
      )}
    </>
  );
}

export function EventBody({
  event,
  onRendered,
}: {
  event: Event;
  onRendered?: () => void;
}): ReactNode {
  if (isAskEvent(event)) {
    return <AskEventBody event={event} onRendered={onRendered} />;
  }
  if (
    event.type === "message.created" ||
    event.type === "message.answered" ||
    event.type === "comment.created" ||
    event.type === "comment.answered"
  ) {
    return (
      <MarkdownBody
        markdown={event.payload.body}
        onRendered={onRendered}
        source={textSourceOf(
          event.type === "comment.created" || event.type === "comment.answered"
            ? "comment"
            : "message",
          event.payload.id
        )}
      />
    );
  }
  return (
    <p className={`font-medium ${textPrimaryOnSurface}`}>
      <AgentParts parts={eventDescription(event)} />
    </p>
  );
}
