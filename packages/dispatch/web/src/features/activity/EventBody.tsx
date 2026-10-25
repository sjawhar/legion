import type { ReactNode } from "react";

import type { Event } from "../../api/types";
import { textMutedOnSurface, textPrimaryOnSurface } from "../../theme/classes";
import { AskOptionList } from "../inbox/AskOptionList";
import { eventDescription } from "../issue/event-description";
import { actorLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";

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
  // The ask the event is about, whose question, options, answer and reason a `#id` names into.
  const source = { id: event.payload.id, kind: "ask" } as const;
  // A changed answer names the one it replaced; the first answer's event carries none.
  const previous = event.type === "ask.answered" ? event.payload.previous_answer : undefined;
  return (
    <>
      <p className={`text-sm font-medium ${textPrimaryOnSurface}`}>
        {event.type === "ask.opened" ? "Ask opened:" : "Ask:"}
      </p>
      <MarkdownBody markdown={question} onRendered={onRendered} source={source} />
      <AskOptionList options={options} selected={answer?.selected ?? []} source={source} />
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
              <MarkdownBody markdown={answer.text} onRendered={onRendered} source={source} />
            </div>
          )}
          {previous === undefined ? null : (
            <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
              <s>
                was:{" "}
                {[previous.selected.join(", "), previous.text ?? ""]
                  .filter((part) => part !== "")
                  .join(" - ")}
              </s>
            </p>
          )}
        </>
      )}
      {resolution === undefined ? null : (
        <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
          Resolved by {actorLabel(resolution.actor)} (
          <MarkdownBody
            markdown={resolution.reason}
            onRendered={onRendered}
            source={source}
            variant="inline"
          />
          ) <Timestamp at={resolution.at} />
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
  if (event.type === "message.created" || event.type === "message.answered") {
    // A message on no issue has no document for a `#id` to name.
    const { body, id, issue_key } = event.payload;
    return (
      <MarkdownBody
        markdown={body}
        onRendered={onRendered}
        source={issue_key === null ? undefined : { id, kind: "message" }}
      />
    );
  }
  if (event.type === "comment.created" || event.type === "comment.answered") {
    return (
      <MarkdownBody
        markdown={event.payload.body}
        onRendered={onRendered}
        source={{ id: event.payload.id, kind: "comment" }}
      />
    );
  }
  return <p className={`font-medium ${textPrimaryOnSurface}`}>{eventDescription(event)}</p>;
}
