import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Event } from "../../api/types";
import { linkText, textMutedOnSurface, textPrimaryOnSurface } from "../../theme/classes";
import { AskOptionList } from "../inbox/AskOptionList";
import { eventDescription } from "../issue/event-description";
import { AgentParts, AskResolutionLine } from "../refs/AgentName";
import { askResolutionReason } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath } from "../refs/routes";
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
  const resolutionReason = resolution === undefined ? undefined : askResolutionReason(resolution);
  // A changed answer names the one it replaced; the first answer's event carries none. An event
  // written before answers stored an empty list holds null for either's selected: no option.
  const previous = event.type === "ask.answered" ? event.payload.previous_answer : undefined;
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
              <MarkdownBody markdown={resolutionReason} onRendered={onRendered} variant="inline" />
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
    const message = event.type.startsWith("message.");
    // A body is bounded block by block, so one event carries its first part alone and says so
    // (`body_truncated`). The reader is given the text the event holds and the item's own page,
    // which reads the stored body whole.
    const whole =
      event.payload.body_truncated === true && event.issue_key !== null
        ? buildIssuePath({
            id: event.payload.id,
            key: event.issue_key,
            kind: message ? "message" : "comment",
          })
        : undefined;
    return (
      <>
        <MarkdownBody markdown={event.payload.body} onRendered={onRendered} />
        {whole === undefined ? null : (
          <p className="mt-1 text-xs">
            <Link className={`dispatch-inline-link underline ${linkText}`} to={whole}>
              {message ? "Read the whole message" : "Read the whole comment"}
            </Link>
          </p>
        )}
      </>
    );
  }
  return (
    <p className={`font-medium ${textPrimaryOnSurface}`}>
      <AgentParts parts={eventDescription(event)} />
    </p>
  );
}
