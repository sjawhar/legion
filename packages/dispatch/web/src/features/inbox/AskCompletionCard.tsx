import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Ask, AskEdit, AskResolution } from "../../api/types";
import {
  badgeLow,
  calloutSuccessBg,
  calloutSuccessBorder,
  calloutSuccessText,
  calloutSuccessTimestampText,
  card,
  inlineWarningText,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textPrimaryOnSuccessCallout,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { actorLabel, describeAskResolutionActor } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { AskAnchorHeader } from "./AskAnchorLink";
import { AskOptionList } from "./AskOptionList";

export function AskEditHistory({ ask, edits }: { ask: Ask; edits: AskEdit[] }): ReactNode {
  if (ask.edited_at === null) {
    return null;
  }
  return (
    <div className={`mt-2 text-xs ${textMutedOnSurface}`}>
      <p>
        Edited <Timestamp at={ask.edited_at} />
      </p>
      {edits.length === 0 ? null : (
        <details className="mt-1">
          <summary className={`cursor-pointer font-medium ${linkText} ${linkHoverText}`}>
            {edits.length === 1
              ? "Show 1 previous version"
              : `Show ${edits.length} previous versions`}
          </summary>
          <div className="mt-2 space-y-3">
            {edits.map((edit) => (
              <div key={edit.at}>
                <div className={`font-medium ${textPrimaryOnSurface}`}>
                  <MarkdownBody markdown={edit.previous.question} />
                </div>
                <AskOptionList options={edit.previous.options} selected={[]} />
                <p className="mt-1">
                  Reworded by {actorLabel(edit.edited_by)} · <Timestamp at={edit.at} />
                </p>
              </div>
            ))}
          </div>
        </details>
      )}
    </div>
  );
}

export function OrphanedAnchorNotice({
  artifactSlug,
  ask,
}: {
  artifactSlug: string | undefined;
  ask: Ask;
}): ReactNode {
  const anchor = ask.anchor;
  if (anchor === null || !anchor.orphaned || artifactSlug === undefined) {
    return null;
  }
  return (
    <p className={`mb-2 text-xs font-medium ${inlineWarningText}`}>
      Text changed.{" "}
      {ask.issue_key === null ? null : (
        <Link
          className="underline"
          to={`${buildIssuePath({
            key: ask.issue_key,
            kind: "artifact",
            slug: artifactSlug,
            version: anchor.version,
          })}&ask=${ask.id}`}
        >
          View original text
        </Link>
      )}
    </p>
  );
}

function AnsweredAsk({
  artifactSlug,
  ask,
  edits,
  frame,
}: {
  artifactSlug: string | undefined;
  ask: Ask;
  edits: AskEdit[];
  frame: AskFrame;
}): ReactNode {
  const { answer } = ask;
  // A chosen "Other" answer carries no real option (answer.selected is empty) but still has
  // free text - render it under an Other label so the record reads as a chosen option, not an
  // unlabeled addendum. An ask with no options at all has no Other row to have chosen, so its
  // free text is always the plain answer instead.
  const otherText =
    ask.options.length > 0 && answer !== null && answer.selected.length === 0 ? answer.text : null;
  return (
    <article
      className={`rounded-xl p-4 text-sm ${frame === "block" ? "mt-3" : ""} ${calloutSuccessBorder} ${calloutSuccessBg} ${calloutSuccessText}`}
      data-testid={`ask-${ask.id}`}
    >
      <AskAnchorHeader ask={ask} inBlock={frame === "block"} tone="success" />
      <OrphanedAnchorNotice artifactSlug={artifactSlug} ask={ask} />
      {frame === "block" ? null : (
        <div className={`font-medium ${textPrimaryOnSuccessCallout}`}>
          <MarkdownBody markdown={ask.question} />
        </div>
      )}
      <p className={`mt-1 text-xs ${calloutSuccessTimestampText}`}>{actorLabel(ask.author)}</p>
      <AskEditHistory ask={ask} edits={edits} />
      <AskOptionList options={ask.options} selected={answer?.selected ?? []} />
      {otherText !== null && otherText !== "" ? (
        <div className="mt-2">
          <p className={`text-sm font-medium ${textPrimaryOnSuccessCallout}`}>Other</p>
          <div className="mt-1">
            <MarkdownBody markdown={otherText} />
          </div>
        </div>
      ) : answer === null || answer.text === null || answer.text === "" ? null : (
        <div className="mt-1">
          <MarkdownBody markdown={answer.text} />
        </div>
      )}
      {answer === null ? (
        <p className={`mt-2 text-xs ${calloutSuccessTimestampText}`}>
          Asked <Timestamp at={ask.created_at} />
        </p>
      ) : (
        <p className={`mt-2 text-xs ${calloutSuccessTimestampText}`}>
          Asked <Timestamp at={ask.created_at} /> · Answered by{" "}
          <span className="font-semibold">{answer.user}</span> <Timestamp at={answer.at} />
        </p>
      )}
    </article>
  );
}

function ResolvedAsk({
  ask,
  edits,
  frame,
}: {
  ask: Ask & { resolution: AskResolution };
  edits: AskEdit[];
  frame: AskFrame;
}): ReactNode {
  const { resolution } = ask;
  return (
    <article
      className={`rounded-xl p-4 text-sm ${frame === "block" ? "mt-3" : ""} ${card}`}
      data-testid={`ask-${ask.id}`}
    >
      <AskAnchorHeader ask={ask} inBlock={frame === "block"} tone="surface" />
      {frame === "block" ? null : (
        <div className={`font-medium ${textPrimaryOnSurface}`}>
          <MarkdownBody markdown={ask.question} />
        </div>
      )}
      <span
        className={`mt-2 inline-block rounded-full px-2.5 py-1 text-xs font-medium ${badgeLow.bg} ${badgeLow.text}`}
        data-testid="ask-resolution-badge"
      >
        {resolution.kind === "retracted" ? "Retracted" : "Resolved"}
      </span>
      <p className={`mt-1 text-xs ${textMutedOnSurface}`}>{actorLabel(ask.author)}</p>
      <AskOptionList options={ask.options} selected={[]} />
      <AskEditHistory ask={ask} edits={edits} />
      <p className={`mt-2 text-xs ${textMutedOnSurface}`}>
        Asked <Timestamp at={ask.created_at} /> · {describeAskResolutionActor(resolution)} -{" "}
        <MarkdownBody markdown={resolution.reason} variant="inline" />
      </p>
    </article>
  );
}

/** `block`: the record sits inside its decision block, whose editor content is the question. */
export type AskFrame = "card" | "block";

interface AskCompletionCardProps {
  artifactSlug: string | undefined;
  ask: Ask;
  edits: AskEdit[];
  frame?: AskFrame;
}

export function AskCompletionCard({
  artifactSlug,
  ask,
  edits,
  frame = "card",
}: AskCompletionCardProps): ReactNode {
  if (ask.state === "resolved") {
    if (ask.resolution === undefined) {
      throw new Error("resolved ask is missing its resolution");
    }
    return <ResolvedAsk ask={{ ...ask, resolution: ask.resolution }} edits={edits} frame={frame} />;
  }
  return (
    <AnsweredAsk
      artifactSlug={artifactSlug}
      ask={{ ...ask, answer: ask.answer }}
      edits={edits}
      frame={frame}
    />
  );
}
