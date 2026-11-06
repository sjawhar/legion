import { actorActText } from "@legion/contracts";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { DisplayTree, Event } from "../../api/types";
import { linkText, textMutedOnSurface, textPrimaryOnSurface } from "../../theme/classes";
import { AskOptionList } from "../inbox/AskOptionList";
import { eventDescription } from "../issue/event-description";
import { AgentParts, AskResolutionLine } from "../refs/AgentName";
import { askResolutionReason } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath, buildReferencePath, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";

type AskEvent = Extract<Event, { type: "ask.opened" | "ask.answered" | "ask.resolved" }>;

export function isAskEvent(event: Event): event is AskEvent {
  return (
    event.type === "ask.opened" || event.type === "ask.answered" || event.type === "ask.resolved"
  );
}

/** The tree of one stored text an event's payload carries, by that text's path in the envelope
 *  (`payload.body`, `payload.resolution.reason`). A payload keeps the shape every reader of the
 *  message bus knows, so its trees ride beside it in `trees` rather than on it
 *  (`api/event_trees.go`); a frame from a Dispatch older than the map carries none, and the
 *  body then renders as its own literal text. */
function payloadTree(event: Event, path: string): DisplayTree | undefined {
  return event.trees?.[path];
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
  // An option's two texts are keyed by its place in the payload's own list, so the trees pair
  // with the options by index, never by label.
  const optionsWithTrees = options.map((option, index) => ({
    ...option,
    description_tree: payloadTree(event, `payload.options.${index}.description`),
    label_tree: payloadTree(event, `payload.options.${index}.label`),
  }));
  return (
    <>
      <p className={`text-sm font-medium ${textPrimaryOnSurface}`}>
        {event.type === "ask.opened" ? "Ask opened:" : "Ask:"}
      </p>
      <MarkdownBody
        markdown={question}
        onRendered={onRendered}
        tree={payloadTree(event, "payload.question")}
      />
      <AskOptionList options={optionsWithTrees} selected={answer?.selected ?? []} />
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
              <MarkdownBody
                markdown={answer.text}
                onRendered={onRendered}
                tree={payloadTree(event, "payload.answer.text")}
              />
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
                tree={payloadTree(event, "payload.resolution.reason")}
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

/** The event of a comment or message whose body the reader may have only part of. */
type BodyEvent = Extract<
  Event,
  { type: "comment.answered" | "comment.created" | "message.answered" | "message.created" }
>;

/** The page that reads the whole body of the item an event carried the first part of: the item's
 *  own page under its issue, and for a comment on a project document, which belongs to no issue,
 *  that document with the comment in view (`itemRoute`), the address every other link to one
 *  uses. Undefined where no page names it: an agent's direct message, which belongs to no issue
 *  either, and a document comment whose event was written before the payload named its
 *  document. */
function wholeItemPath(event: BodyEvent): string | undefined {
  if (event.issue_key !== null) {
    return buildIssuePath({
      id: event.payload.id,
      key: event.issue_key,
      kind: event.type.startsWith("message.") ? "message" : "comment",
    });
  }
  if (event.type !== "comment.created" && event.type !== "comment.answered") return undefined;
  const { artifact_slug: slug, project_key: project } = event.payload;
  if (project === undefined || slug === undefined) return undefined;
  const route = itemRoute("comment", { id: event.payload.id, issue_key: null }, { project, slug });
  return route === undefined ? undefined : buildReferencePath(route);
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
    // (`body_truncated`). The reader is given the text the event holds and the page that reads
    // the stored body whole.
    const whole = event.payload.body_truncated === true ? wholeItemPath(event) : undefined;
    return (
      <>
        <MarkdownBody
          markdown={event.payload.body}
          onRendered={onRendered}
          tree={payloadTree(event, "payload.body")}
        />
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
