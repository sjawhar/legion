import { ISSUE_TITLE_MAX } from "@legion/contracts";
import type { KeyboardEvent, ReactNode } from "react";

import {
  bgTransparent,
  borderStrongHover,
  borderTransparent,
  focusBorder,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { authorText } from "./page-structure";
import type { IssueDrafts } from "./useIssueDrafts";

/**
 * Row two of the identity block: the issue's title, which is the issue as a person names it, and
 * the editor it becomes when the reader puts the caret in it.
 *
 * The heading and the editor are the same box at the same size, so the blur a mousedown causes
 * never moves what the reader was about to click: Chromium would otherwise deliver the mouseup,
 * and so the click, to a different element and swallow the first press after a title edit.
 * `condensed` only takes the type down a step, which costs the sticky block height and the title
 * nothing a reader needs.
 */
export function IssueTitleRow({
  condensed,
  drafts,
  editing,
  isClosed,
  onEditingChange,
}: {
  condensed: boolean;
  drafts: IssueDrafts;
  editing: boolean;
  isClosed: boolean;
  onEditingChange: (editing: boolean) => void;
}): ReactNode {
  // Below `md` the title takes the body's own size: a 37-character title costs three lines at
  // 22 px on a 390 px screen and two at 14 px, and the lines it saves are lines of the head
  // the first open question needs (dispatch://INFRA-1196 R6).
  const size = condensed ? "text-base leading-6" : "text-sm leading-5 md:text-[22px] md:leading-7";
  const saveOnEnter = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Enter") {
      event.currentTarget.blur();
    }
  };
  if (editing) {
    return (
      <input
        aria-describedby={drafts.titleError === null ? undefined : "issue-title-help"}
        aria-label="Issue title"
        className={`min-w-0 w-full rounded-lg border px-2 py-0.5 font-semibold tracking-tight outline-none md:py-0 ${size} ${borderTransparent} ${bgTransparent} ${textPrimaryOnSurface} ${borderStrongHover} ${focusBorder}`}
        disabled={isClosed}
        maxLength={ISSUE_TITLE_MAX}
        onBlur={() => {
          drafts.requestTitleSubmit();
          onEditingChange(false);
        }}
        onChange={(event) => drafts.writeTitle(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            drafts.discardTitle();
            onEditingChange(false);
            return;
          }
          saveOnEnter(event);
        }}
        ref={(node) => node?.focus()}
        value={drafts.title}
      />
    );
  }
  return (
    <h1
      className={`min-w-0 break-words rounded-lg border px-2 py-0.5 font-semibold tracking-tight md:py-0 ${size} ${borderTransparent} ${textPrimaryOnSurface} ${
        isClosed ? "" : `cursor-text ${borderStrongHover}`
      }`}
      onClick={() => {
        if (!isClosed) {
          onEditingChange(true);
        }
      }}
      onFocus={() => {
        if (!isClosed) {
          onEditingChange(true);
        }
      }}
      onKeyDown={(event) => {
        if (!isClosed && (event.key === "Enter" || event.key === " ")) {
          event.preventDefault();
          onEditingChange(true);
        }
      }}
      tabIndex={isClosed ? -1 : 0}
      {...authorText}
    >
      {drafts.title}
    </h1>
  );
}
