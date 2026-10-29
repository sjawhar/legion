import type { ReactNode } from "react";

import type { SearchAsk } from "../../api/types";
import {
  badgeLow,
  textMutedOnSelectedCard,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AskResolutionLine } from "../refs/AgentName";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { markdownHead } from "../refs/markdown-fragment";
import { Timestamp } from "../refs/Timestamp";

function askStateLabel(ask: SearchAsk): string {
  if (ask.state === "open") return "Open";
  if (ask.state === "answered") return "Answered";
  return ask.answer === undefined ? "Closed, no answer" : "Closed after an answer";
}

/** How much of an ask's answer an ask hit carries: about three lines of the row it sits in,
 *  on the scale of the excerpt's 24-word window. The three-line clamp is CSS only, so an answer
 *  left whole (up to 1 MiB) would sit inside the row, which a screen reader then reads as the
 *  row's name from start to end. */
export const ANSWER_PREVIEW_CHARS = 280;

/** Where an ask hit stands, under its excerpt: a badge for its state, who closed it and when,
 *  and its latest answer - who gave it, when, the options chosen and the start of its text
 *  (`ANSWER_PREVIEW_CHARS`), clamped to three lines. Every part wraps inside the row, so a long
 *  label or name never widens the row it sits in. */
export function AskHitStatus({ ask, muted }: { ask: SearchAsk; muted: boolean }): ReactNode {
  const answer = ask.state === "open" ? undefined : ask.answer;
  return (
    <div
      className={`mt-1 min-w-0 text-xs ${muted ? textMutedOnSelectedCard : textSecondaryOnSurface}`}
    >
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
        <span className={`rounded-full px-2 py-0.5 font-medium ${badgeLow.bg} ${badgeLow.text}`}>
          {askStateLabel(ask)}
        </span>
        {ask.state === "resolved" ? (
          <span className="min-w-0 [overflow-wrap:anywhere]">
            <AskResolutionLine agent="static" resolution={ask.resolution} /> ·{" "}
            <Timestamp at={ask.resolution.at} />
          </span>
        ) : null}
        {answer === undefined ? null : (
          <span
            className={`min-w-0 [overflow-wrap:anywhere] ${ask.state === "resolved" ? "basis-full" : ""}`}
          >
            {answer.user} · <Timestamp at={answer.at} /> ·{" "}
            {answer.selected.length === 0
              ? "No option chosen"
              : `Chose ${answer.selected.join(", ")}`}
          </span>
        )}
      </div>
      {answer === undefined || answer.text === null || answer.text === "" ? null : (
        <MarkdownPreview
          className={`mt-1 ${muted ? textMutedOnSelectedCard : textPrimaryOnSurface}`}
          lines={3}
          markdown={markdownHead(answer.text, ANSWER_PREVIEW_CHARS)}
        />
      )}
    </div>
  );
}
