import type { ReactNode } from "react";

import type { Event } from "../../api/types";
import { textMutedOnSurface, textPrimaryOnSurface } from "../../theme/classes";
import { AskOptionList } from "../inbox/AskOptionList";
import { actorLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import { eventDescription } from "./event-description";

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
  const { answer, question, options, resolution, created_at } = event.payload;
  return (
    <>
      <p className={`text-sm font-medium ${textPrimaryOnSurface}`}>
        {event.type === "ask.opened" ? "Ask opened:" : "Ask:"}
      </p>
      <MarkdownBody markdown={question} onRendered={onRendered} />
      <AskOptionList options={options} selected={answer?.selected ?? []} />
      <p className={`mt-2 text-xs ${textMutedOnSurface}`}>
        Opened <Timestamp at={created_at} />
      </p>
      {answer === null ? null : (
        <>
          <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
            Answered by {answer.user} <Timestamp at={answer.at} />
          </p>
          {answer.text === null || answer.text === "" ? null : (
            <div className="mt-2">
              <MarkdownBody markdown={answer.text} onRendered={onRendered} />
            </div>
          )}
        </>
      )}
      {resolution === undefined ? null : (
        <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
          Resolved by {actorLabel(resolution.actor)} (
          <MarkdownBody markdown={resolution.reason} onRendered={onRendered} variant="inline" />){" "}
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
    event.type === "comment.created"
  ) {
    return <MarkdownBody markdown={event.payload.body} onRendered={onRendered} />;
  }
  return <p className={`font-medium ${textPrimaryOnSurface}`}>{eventDescription(event)}</p>;
}
