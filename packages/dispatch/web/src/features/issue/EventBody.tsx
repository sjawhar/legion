import type { ReactNode } from "react";
import Markdown from "react-markdown";
import rehypeSanitize from "rehype-sanitize";
import remarkGfm from "remark-gfm";

import type { Event } from "../../api/types";
import { textMutedOnSurface, textPrimaryOnSurface } from "../../theme/classes";
import { sanitizeSchema } from "../doc/sanitize";
import { AskOptionList } from "../inbox/AskOptionList";
import { actorLabel } from "../refs/actor";
import { Timestamp } from "../refs/Timestamp";
import { eventDescription } from "./log-model";

type AskEvent = Extract<Event, { type: "ask.opened" | "ask.answered" | "ask.resolved" }>;

export function isAskEvent(event: Event): event is AskEvent {
  return (
    event.type === "ask.opened" || event.type === "ask.answered" || event.type === "ask.resolved"
  );
}

function MarkdownBody({ markdown }: { markdown: string }): ReactNode {
  return (
    <div className="prose prose-sm prose-slate max-w-none break-words dark:prose-invert">
      <Markdown rehypePlugins={[[rehypeSanitize, sanitizeSchema]]} remarkPlugins={[remarkGfm]}>
        {markdown}
      </Markdown>
    </div>
  );
}

function AskEventBody({ event }: { event: AskEvent }): ReactNode {
  const { answer, question, options, resolution, created_at } = event.payload;
  return (
    <>
      <p className={`text-sm font-medium ${textPrimaryOnSurface}`}>
        {event.type === "ask.opened" ? "Ask opened:" : "Ask:"}
      </p>
      <MarkdownBody markdown={question} />
      <AskOptionList
        descriptionClass={textMutedOnSurface}
        labelClass={textPrimaryOnSurface}
        options={options}
        selected={answer?.selected ?? []}
      />
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
              <MarkdownBody markdown={answer.text} />
            </div>
          )}
        </>
      )}
      {resolution === undefined ? null : (
        <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
          Resolved by {actorLabel(resolution.actor)} ({resolution.reason}){" "}
          <Timestamp at={resolution.at} />
        </p>
      )}
    </>
  );
}

export function EventBody({ event }: { event: Event }): ReactNode {
  if (isAskEvent(event)) {
    return <AskEventBody event={event} />;
  }
  if (event.type === "message.created" || event.type === "comment.created") {
    return <MarkdownBody markdown={event.payload.body} />;
  }
  return <p className={`font-medium ${textPrimaryOnSurface}`}>{eventDescription(event)}</p>;
}
