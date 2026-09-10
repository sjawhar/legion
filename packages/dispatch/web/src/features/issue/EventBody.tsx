import type { HeadlessProofEditor } from "@sjawhar/proof-editor/headless";
import { DOMSerializer } from "prosemirror-model";
import { type ReactNode, useEffect, useRef } from "react";

import type { Event } from "../../api/types";
import { textMutedOnSurface, textPrimaryOnSurface } from "../../theme/classes";
import { AskOptionList } from "../inbox/AskOptionList";
import { actorLabel } from "../refs/actor";
import { Timestamp } from "../refs/Timestamp";
import { eventDescription } from "./log-model";

let headlessProof: Promise<HeadlessProofEditor> | undefined;

function loadHeadlessProof(): Promise<HeadlessProofEditor> {
  headlessProof ??= import("@sjawhar/proof-editor/headless").then(({ createHeadlessProof }) =>
    createHeadlessProof()
  );
  return headlessProof;
}

function MarkdownBody({
  markdown,
  onRendered,
}: {
  markdown: string;
  onRendered?: () => void;
}): ReactNode {
  const onRenderedRef = useRef(onRendered);
  onRenderedRef.current = onRendered;
  const root = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let mounted = true;
    void loadHeadlessProof().then((proof) => {
      if (!mounted) {
        return;
      }
      if (root.current === null) {
        throw new Error("EventBody's Markdown root is unavailable.");
      }
      const document = proof.parseMarkdown(markdown);
      root.current.replaceChildren(
        DOMSerializer.fromSchema(proof.schema).serializeFragment(document.content)
      );
      for (const item of root.current.querySelectorAll("li[data-spread='false']")) {
        const paragraph = item.firstElementChild;
        if (item.childElementCount === 1 && paragraph?.tagName === "P") {
          paragraph.replaceWith(...paragraph.childNodes);
        }
      }
      onRenderedRef.current?.();
    });
    return () => {
      mounted = false;
    };
  }, [markdown]);

  return (
    <div
      className="prose prose-sm prose-slate max-w-none break-words dark:prose-invert"
      ref={root}
    />
  );
}

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
              <MarkdownBody markdown={answer.text} onRendered={onRendered} />
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
  if (event.type === "message.created" || event.type === "comment.created") {
    return <MarkdownBody markdown={event.payload.body} onRendered={onRendered} />;
  }
  return <p className={`font-medium ${textPrimaryOnSurface}`}>{eventDescription(event)}</p>;
}
